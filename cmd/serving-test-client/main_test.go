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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestStreamingChatRequiresDataAndDone(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("request = %s Accept=%q", request.Method, request.Header.Get("Accept"))
		}
		var payload struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload.Model != "test-model" || !payload.Stream {
			t.Errorf("payload = %+v", payload)
		}
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	summary, err := requestStreamingChat(context.Background(), server.Client(), server.URL, "test-model")
	if err != nil {
		t.Fatalf("requestStreamingChat() error = %v", err)
	}
	if summary.SSEDataEvents != 1 || !summary.Done {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestRequestStreamingChatRejectsIncompleteSSE(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"done without data": "data: [DONE]\n\n",
		"data without done": "data: {\"choices\":[]}\n\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()

			if _, err := requestStreamingChat(context.Background(), server.Client(), server.URL, "test-model"); err == nil {
				t.Fatal("requestStreamingChat() error = nil, want incomplete SSE failure")
			}
		})
	}
}
