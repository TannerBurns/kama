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

// Package main runs Kama's deterministic Hugging Face-compatible test server.
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

	"github.com/TannerBurns/kama/internal/testfixtures/huggingface"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Fake Hugging Face server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var listenAddress string
	flag.StringVar(&listenAddress, "listen-address", ":8083", "Address for the fixture HTTP server")
	flag.Parse()

	config, err := huggingface.DefaultConfig()
	if err != nil {
		return fmt.Errorf("create fixture config: %w", err)
	}
	fixture, err := huggingface.New(config)
	if err != nil {
		return fmt.Errorf("create fixture server: %w", err)
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
		slog.Info("Starting fake Hugging Face server", "address", listenAddress)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down fake Hugging Face server: %w", err)
		}
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve fake Hugging Face server: %w", err)
	}
}
