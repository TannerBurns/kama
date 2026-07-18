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

package llamaserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const chatRequestBody = `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`

func TestHealthAndSlots(t *testing.T) {
	t.Parallel()

	now := time.Unix(10, 0)
	config := DefaultConfig()
	config.Capacity = 2
	config.StartupDelay = 5 * time.Second
	config.Now = func() time.Time { return now }
	server := mustNewServer(t, config)

	response := performRequest(context.Background(), server, http.MethodGet, "/health", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("health before startup status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}

	now = now.Add(config.StartupDelay)
	response = performRequest(context.Background(), server, http.MethodGet, "/health", "")
	if response.Code != http.StatusOK {
		t.Fatalf("health after startup status = %d, want %d", response.Code, http.StatusOK)
	}

	slotID, acquired := server.acquireSlot()
	if !acquired {
		t.Fatal("acquireSlot() = false, want true")
	}
	defer server.releaseSlot(slotID)
	response = performRequest(context.Background(), server, http.MethodGet, "/slots", "")
	var slots []slotResponse
	if err := json.Unmarshal(response.Body.Bytes(), &slots); err != nil {
		t.Fatalf("decode slots: %v", err)
	}
	if len(slots) != config.Capacity || slots[0].State != 1 || slots[1].State != 0 {
		t.Fatalf("slots = %+v, want one active and one idle slot", slots)
	}
}

func TestUnaryChatCompletionIsDeterministic(t *testing.T) {
	t.Parallel()

	server := mustNewServer(t, DefaultConfig())
	first := performRequest(context.Background(), server, http.MethodPost, "/v1/chat/completions", chatRequestBody)
	second := performRequest(context.Background(), server, http.MethodPost, "/v1/chat/completions", chatRequestBody)
	if first.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want %d; body = %s", first.Code, http.StatusOK, first.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("completion bodies differ:\nfirst: %s\nsecond: %s", first.Body.String(), second.Body.String())
	}
	if !strings.Contains(first.Body.String(), `"id":"chatcmpl-kama-fixture"`) ||
		!strings.Contains(first.Body.String(), `"content":"Kama fixture response"`) {
		t.Fatalf("completion body does not contain deterministic fixture fields: %s", first.Body.String())
	}
}

func TestStreamingChatCompletion(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.StreamChunks = 3
	server := mustNewServer(t, config)
	body := strings.TrimSuffix(chatRequestBody, "}") + `,"stream":true}`
	response := performRequest(context.Background(), server, http.MethodPost, "/v1/chat/completions", body)
	if response.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("stream content type = %q, want text/event-stream", contentType)
	}
	for index := 1; index <= config.StreamChunks; index++ {
		expectedContent := fmt.Sprintf(`"content":"fixture-%d"`, index)
		if !strings.Contains(response.Body.String(), expectedContent) {
			t.Fatalf("stream body is missing chunk %d: %s", index, response.Body.String())
		}
	}
	if !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream body does not end in the done marker: %q", response.Body.String())
	}
}

func TestConfiguredOverloadFailureAndCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configure  func(*Config)
		exhaust    bool
		wantStatus int
	}{
		{
			name: "forced overload",
			configure: func(config *Config) {
				config.Overload = true
			},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name: "forced failure",
			configure: func(config *Config) {
				config.FailureStatus = http.StatusBadGateway
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "capacity exhausted",
			configure:  func(_ *Config) {},
			exhaust:    true,
			wantStatus: http.StatusTooManyRequests,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := DefaultConfig()
			test.configure(&config)
			server := mustNewServer(t, config)
			if test.exhaust {
				_, _ = server.acquireSlot()
			}
			response := performRequest(
				context.Background(), server, http.MethodPost, "/v1/chat/completions", chatRequestBody,
			)
			if response.Code != test.wantStatus {
				t.Fatalf("completion status = %d, want %d; body = %s",
					response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestCanceledRequestReleasesSlot(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.ResponseDelay = time.Hour
	server := mustNewServer(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		_ = performRequest(ctx, server, http.MethodPost, "/v1/chat/completions", chatRequestBody)
	}()

	waitForActiveSlot(t, server)
	cancel()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("canceled completion did not return")
	}

	server.mu.Lock()
	active := server.slots[0]
	server.mu.Unlock()
	if active {
		t.Fatal("slot remained active after request cancellation")
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.Capacity = 0
	if _, err := New(config); err == nil {
		t.Fatal("New() with zero capacity returned nil error")
	}
	config = DefaultConfig()
	config.FailureStatus = http.StatusOK
	if _, err := New(config); err == nil {
		t.Fatal("New() with non-error failure status returned nil error")
	}
}

func mustNewServer(t *testing.T, config Config) *Server {
	t.Helper()
	server, err := New(config)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return server
}

func performRequest(
	ctx context.Context,
	server *Server,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func waitForActiveSlot(t *testing.T, server *Server) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		active := server.slots[0]
		server.mu.Unlock()
		if active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completion did not acquire a slot")
}
