/*
Copyright 2026 Kama Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package huggingface

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetadataAuthAndRangeDownload(t *testing.T) {
	t.Parallel()
	config, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig(): %v", err)
	}
	fixture, err := New(config)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	metadataURL := "http://fixture/api/models/" + PrivateRepository + "/revision/main"
	response := doRequest(t, fixture.Handler(), http.MethodGet, metadataURL, "", "")
	if response.StatusCode != http.StatusUnauthorized {
		closeResponse(t, response)
		t.Fatalf("unauthenticated metadata status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	closeResponse(t, response)

	response = doRequest(t, fixture.Handler(), http.MethodGet, metadataURL, "Bearer "+DefaultToken, "")
	metadata, readErr := io.ReadAll(response.Body)
	closeResponse(t, response)
	if readErr != nil {
		t.Fatalf("read metadata: %v", readErr)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(metadata), DefaultRevision) {
		t.Fatalf("metadata status/body = %d %s", response.StatusCode, metadata)
	}

	downloadURL := "http://fixture/" + PrivateRepository + "/resolve/" + DefaultRevision + "/model.gguf"
	response = doRequest(t, fixture.Handler(), http.MethodGet, downloadURL, "Bearer "+DefaultToken, "bytes=4-")
	body, readErr := io.ReadAll(response.Body)
	closeResponse(t, response)
	if readErr != nil {
		t.Fatalf("read range response: %v", readErr)
	}
	if response.StatusCode != http.StatusPartialContent || response.Header.Get("Content-Range") == "" {
		t.Fatalf("range response = status %d headers %v", response.StatusCode, response.Header)
	}
	if len(body) == 0 {
		t.Fatal("range response body is empty")
	}
}

func closeResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
}

func TestPauseFaultTracksInterruptedAndResumedTransfer(t *testing.T) {
	t.Parallel()
	config, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig(): %v", err)
	}
	fixture, err := New(config)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	target := PublicRepository + "/model.gguf"
	armRequest := httptest.NewRequest(http.MethodPut, "http://fixture/state/fault", strings.NewReader(
		`{"target":"`+target+`","pauseAfterBytes":64}`,
	))
	armRequest.Header.Set("Content-Type", "application/json")
	armRecorder := httptest.NewRecorder()
	fixture.Handler().ServeHTTP(armRecorder, armRequest)
	if armRecorder.Code != http.StatusOK {
		t.Fatalf("arm fault status/body = %d %s", armRecorder.Code, armRecorder.Body.String())
	}

	downloadURL := "http://fixture/" + PublicRepository + "/resolve/" + DefaultRevision + "/model.gguf"
	ctx, cancel := context.WithCancel(context.Background())
	firstRequest := httptest.NewRequest(http.MethodGet, downloadURL, nil).WithContext(ctx)
	firstRecorder := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		fixture.Handler().ServeHTTP(firstRecorder, firstRequest)
		close(firstDone)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		state := readState(t, fixture.Handler())
		if state.Transfers[target].ActivePauses == 1 {
			if state.Transfers[target].Attempts != 1 || state.Transfers[target].Pauses != 1 {
				t.Fatalf("active transfer counters = %+v", state.Transfers[target])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transfer did not pause; state = %+v", state)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("interrupted transfer did not stop")
	}
	if firstRecorder.Body.Len() != 64 {
		t.Fatalf("interrupted body length = %d, want 64", firstRecorder.Body.Len())
	}

	secondRequest := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	secondRequest.Header.Set("Range", "bytes=64-")
	secondRequest.Header.Set("If-Range", config.Repositories[PublicRepository].Files["model.gguf"].ETag)
	secondRecorder := httptest.NewRecorder()
	fixture.Handler().ServeHTTP(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusPartialContent {
		t.Fatalf("resume status/body = %d %s", secondRecorder.Code, secondRecorder.Body.String())
	}

	state := readState(t, fixture.Handler())
	if got := state.Transfers[target]; got != (transferCounters{
		Attempts: 2, Ranges: 1, Pauses: 1, Completions: 1,
	}) {
		t.Fatalf("final transfer counters = %+v", got)
	}
	if state.Downloads[target] != 1 {
		t.Fatalf("completed downloads = %d, want 1", state.Downloads[target])
	}
	if state.Fault == nil || state.Fault.Remaining != 0 {
		t.Fatalf("consumed fault state = %+v", state.Fault)
	}

	resetRecorder := httptest.NewRecorder()
	fixture.Handler().ServeHTTP(resetRecorder, httptest.NewRequest(http.MethodPut, "http://fixture/state/reset", nil))
	if resetRecorder.Code != http.StatusOK {
		t.Fatalf("reset status/body = %d %s", resetRecorder.Code, resetRecorder.Body.String())
	}
	state = readState(t, fixture.Handler())
	if len(state.Downloads) != 0 || len(state.Transfers) != 0 || state.Fault != nil {
		t.Fatalf("reset state = %+v", state)
	}
}

type fixtureState struct {
	Downloads map[string]int              `json:"downloads"`
	Transfers map[string]transferCounters `json:"transfers"`
	Fault     *pauseFaultStatus           `json:"fault"`
}

func readState(t *testing.T, handler http.Handler) fixtureState {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://fixture/state", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("state status/body = %d %s", recorder.Code, recorder.Body.String())
	}
	var state fixtureState
	if err := json.NewDecoder(recorder.Body).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return state
}

func doRequest(
	t *testing.T,
	handler http.Handler,
	method, requestURL, authorization, byteRange string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, requestURL, nil)
	if err != nil {
		t.Fatalf("NewRequest(): %v", err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Range", byteRange)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
