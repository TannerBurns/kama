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

// Package llamaserver provides a deterministic, non-GPU stand-in for the
// llama.cpp HTTP server. It is test infrastructure and is not a model server.
package llamaserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	defaultModel       = "kama-fixture"
	fixtureResponse    = "Kama fixture response"
	maxRequestBodySize = 1 << 20
)

// Config controls deterministic failure and timing behavior for a Server.
type Config struct {
	StartupDelay  time.Duration
	ResponseDelay time.Duration
	Capacity      int
	Overload      bool
	FailureStatus int
	StreamChunks  int
	Now           func() time.Time
}

// DefaultConfig returns a ready-immediately server with one inference slot.
func DefaultConfig() Config {
	return Config{
		Capacity:     1,
		StreamChunks: 2,
		Now:          time.Now,
	}
}

// Server implements the fake llama-server HTTP surface.
type Server struct {
	config  Config
	readyAt time.Time

	mu    sync.Mutex
	slots []bool
}

// New returns a fake llama server with the supplied behavior.
func New(config Config) (*Server, error) {
	if config.StartupDelay < 0 {
		return nil, errors.New("startup delay must not be negative")
	}
	if config.ResponseDelay < 0 {
		return nil, errors.New("response delay must not be negative")
	}
	if config.Capacity < 1 {
		return nil, errors.New("capacity must be at least one")
	}
	if config.StreamChunks < 1 {
		return nil, errors.New("stream chunks must be at least one")
	}
	if config.FailureStatus != 0 && (config.FailureStatus < 400 || config.FailureStatus > 599) {
		return nil, errors.New("failure status must be zero or an HTTP error status")
	}
	if config.Now == nil {
		config.Now = time.Now
	}

	return &Server{
		config:  config,
		readyAt: config.Now().Add(config.StartupDelay),
		slots:   make([]bool, config.Capacity),
	}, nil
}

// Handler returns the fake server's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /slots", s.handleSlots)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	return mux
}

func (s *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	if s.config.Now().Before(s.readyAt) {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

type slotResponse struct {
	ID     int `json:"id"`
	IDTask int `json:"id_task"`
	State  int `json:"state"`
}

func (s *Server) handleSlots(writer http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	response := make([]slotResponse, len(s.slots))
	for index, active := range s.slots {
		response[index] = slotResponse{ID: index, IDTask: -1}
		if active {
			response[index].IDTask = index
			response[index].State = 1
		}
	}
	s.mu.Unlock()

	writeJSON(writer, http.StatusOK, response)
}

type chatRequest struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Stream   bool              `json:"stream"`
}

func (s *Server) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
	var input chatRequest
	if err := decodeJSONBody(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Messages) == 0 {
		writeError(writer, http.StatusBadRequest, "messages must contain at least one item")
		return
	}
	if input.Model == "" {
		input.Model = defaultModel
	}
	if s.config.Overload {
		writeError(writer, http.StatusTooManyRequests, "fixture is configured as overloaded")
		return
	}

	slotID, acquired := s.acquireSlot()
	if !acquired {
		writeError(writer, http.StatusTooManyRequests, "all fixture slots are busy")
		return
	}
	defer s.releaseSlot(slotID)

	if input.Stream {
		s.writeStreamingResponse(request.Context(), writer, input.Model)
		return
	}
	if !waitFor(request.Context(), s.config.ResponseDelay) {
		return
	}
	if s.config.FailureStatus != 0 {
		writeError(writer, s.config.FailureStatus, "fixture is configured to fail")
		return
	}

	writeJSON(writer, http.StatusOK, unaryResponse(input.Model))
}

func (s *Server) writeStreamingResponse(ctx context.Context, writer http.ResponseWriter, model string) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "streaming is not supported by the response writer")
		return
	}
	if s.config.FailureStatus != 0 {
		if !waitFor(ctx, s.config.ResponseDelay) {
			return
		}
		writeError(writer, s.config.FailureStatus, "fixture is configured to fail")
		return
	}

	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("X-Accel-Buffering", "no")
	for index := range s.config.StreamChunks {
		if !waitFor(ctx, s.config.ResponseDelay) {
			return
		}
		if err := writeServerSentEvent(writer, streamResponse(model, index)); err != nil {
			return
		}
		flusher.Flush()
	}
	if ctx.Err() != nil {
		return
	}
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) acquireSlot() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, active := range s.slots {
		if !active {
			s.slots[index] = true
			return index, true
		}
	}
	return -1, false
}

func (s *Server) releaseSlot(slotID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots[slotID] = false
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	if delay == 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func decodeJSONBody(writer http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBodySize)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func unaryResponse(model string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-kama-fixture",
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]string{
				"role":    "assistant",
				"content": fixtureResponse,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     1,
			"completion_tokens": 3,
			"total_tokens":      4,
		},
	}
}

func streamResponse(model string, index int) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-kama-fixture",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]string{
				"role":    "assistant",
				"content": fmt.Sprintf("fixture-%d", index+1),
			},
			"finish_reason": nil,
		}},
	}
}

func writeServerSentEvent(writer io.Writer, value any) error {
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(value); err != nil {
		return fmt.Errorf("encode server-sent event: %w", err)
	}
	_, err := fmt.Fprintf(writer, "data: %s\n", encoded.Bytes())
	return err
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "fixture_error",
		},
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
