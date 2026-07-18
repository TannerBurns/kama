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

// Package main runs the deterministic fake llama HTTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TannerBurns/kama/internal/testfixtures/llamaserver"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Fake llama-server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	config := llamaserver.DefaultConfig()
	var listenAddress string
	flag.StringVar(&listenAddress, "listen-address", ":8080", "Address for the fixture HTTP server")
	flag.DurationVar(&config.StartupDelay, "startup-delay", 0, "Delay before /health reports ready")
	flag.DurationVar(&config.ResponseDelay, "response-delay", 0,
		"Delay before a unary response or each SSE chunk")
	flag.IntVar(&config.Capacity, "capacity", config.Capacity, "Maximum concurrent chat completion requests")
	flag.BoolVar(&config.Overload, "overload", false, "Return HTTP 429 for every chat completion request")
	flag.IntVar(&config.FailureStatus, "fail-status", 0,
		"Return this HTTP error status for every admitted completion; zero disables failure")
	flag.IntVar(&config.StreamChunks, "stream-chunks", config.StreamChunks,
		"Number of deterministic content events emitted for an SSE response")
	flag.Parse()

	fixture, err := llamaserver.New(config)
	if err != nil {
		return fmt.Errorf("configure fake llama-server: %w", err)
	}
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           fixture.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("Starting fake llama-server", "address", listenAddress)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down fake llama-server: %w", err)
		}
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve fake llama-server: %w", err)
	}
}
