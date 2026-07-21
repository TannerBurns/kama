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

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunVersionAndArgumentValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := run(context.Background(), []string{"--version"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("run(--version) exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatal("run(--version) produced no version")
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := run(context.Background(), []string{"unexpected"}, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("run(positional) exit = %d, want 2", exitCode)
	}
}

func TestRunDrainCallsBlockingLoopbackEndpoint(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/drain" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		called.Store(true)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var stderr bytes.Buffer
	address := strings.TrimPrefix(server.URL, "http://")
	if exitCode := runDrain(context.Background(), []string{"--address=" + address}, &stderr); exitCode != 0 {
		t.Fatalf("runDrain() exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if !called.Load() {
		t.Fatal("drain endpoint was not called")
	}
}
