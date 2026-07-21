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

// Package main runs a bounded in-cluster streaming check against a llama-server Service.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultRequestTimeout = 2 * time.Minute
	maximumRequestTimeout = 5 * time.Minute
	maximumResponseBytes  = 4 << 20
	maximumSSELineBytes   = 1 << 20
)

type streamSummary struct {
	SchemaVersion int  `json:"schemaVersion"`
	SSEDataEvents int  `json:"sseDataEvents"`
	Done          bool `json:"done"`
}

func main() {
	var endpoint string
	var model string
	var timeout time.Duration
	flag.StringVar(&endpoint, "endpoint", "", "Full llama-server chat-completions endpoint URL")
	flag.StringVar(&model, "model", "", "Model name sent in the chat request")
	flag.DurationVar(&timeout, "timeout", defaultRequestTimeout, "End-to-end request deadline")
	flag.Parse()

	if endpoint == "" || model == "" {
		slog.Error("Both --endpoint and --model are required")
		os.Exit(2)
	}
	if timeout <= 0 || timeout > maximumRequestTimeout {
		slog.Error("Request timeout is outside the supported bound", "maximum", maximumRequestTimeout)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	summary, err := requestStreamingChat(ctx, http.DefaultClient, endpoint, model)
	if err != nil {
		slog.Error("Streaming serving check failed", "error", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		slog.Error("Encode serving-check summary", "error", err)
		os.Exit(1)
	}
}

func requestStreamingChat(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	model string,
) (streamSummary, error) {
	payload, err := json.Marshal(struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int  `json:"max_tokens"`
		Stream    bool `json:"stream"`
	}{
		Model: model,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: "Reply with one short greeting."}},
		MaxTokens: 16,
		Stream:    true,
	})
	if err != nil {
		return streamSummary{}, fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return streamSummary{}, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return streamSummary{}, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return streamSummary{}, fmt.Errorf("chat endpoint returned HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return streamSummary{}, fmt.Errorf("chat endpoint returned content type %q, want text/event-stream", mediaType)
	}

	summary := streamSummary{SchemaVersion: 1}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, maximumResponseBytes))
	scanner.Buffer(make([]byte, 4096), maximumSSELineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			if summary.SSEDataEvents == 0 {
				return streamSummary{}, errors.New("SSE completion arrived before any data event")
			}
			summary.Done = true
			return summary, nil
		}
		summary.SSEDataEvents++
	}
	if err := scanner.Err(); err != nil {
		return streamSummary{}, fmt.Errorf("read SSE response: %w", err)
	}
	if summary.SSEDataEvents == 0 {
		return streamSummary{}, errors.New("SSE response contained no data event")
	}
	return streamSummary{}, errors.New("SSE response ended without data: [DONE]")
}
