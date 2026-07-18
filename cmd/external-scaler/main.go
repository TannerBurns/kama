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

// Package main runs the controllable KEDA external scaler fixture.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TannerBurns/kama/internal/testfixtures/externalscaler"
	externalpb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
)

type options struct {
	listenAddress  string
	controlAddress string
	scalerConfig   externalscaler.Config
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, parseOptions()); err != nil {
		slog.Error("External scaler fixture stopped", "error", err)
		os.Exit(1)
	}
}

func parseOptions() options {
	config := externalscaler.DefaultConfig()
	var result options
	flag.StringVar(&result.listenAddress, "listen-address", ":9090", "Address for the KEDA gRPC service")
	flag.StringVar(&result.controlAddress, "control-address", ":8080", "Address for the test control HTTP service")
	flag.StringVar(&config.MetricName, "metric-name", config.MetricName, "Name returned to KEDA for the fixture metric")
	flag.Float64Var(&config.TargetSize, "target-size", config.TargetSize, "Metric target per replica")
	flag.Float64Var(&config.InitialMetric, "initial-metric", 0, "Initial pending-request count")
	flag.DurationVar(&config.StreamHeartbeat, "stream-heartbeat", config.StreamHeartbeat,
		"Interval for repeated StreamIsActive events; zero disables heartbeats")
	flag.Parse()
	result.scalerConfig = config
	return result
}

func run(ctx context.Context, options options) error {
	fixture, err := externalscaler.New(options.scalerConfig)
	if err != nil {
		return fmt.Errorf("configure external scaler fixture: %w", err)
	}
	grpcListener, err := net.Listen("tcp", options.listenAddress)
	if err != nil {
		return fmt.Errorf("listen for external scaler gRPC: %w", err)
	}
	defer func() { _ = grpcListener.Close() }()
	controlListener, err := net.Listen("tcp", options.controlAddress)
	if err != nil {
		return fmt.Errorf("listen for external scaler control HTTP: %w", err)
	}
	defer func() { _ = controlListener.Close() }()

	grpcServer := grpc.NewServer()
	externalpb.RegisterExternalScalerServer(grpcServer, fixture)
	controlServer := &http.Server{
		Handler:           fixture.ControlHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 2)
	go func() {
		slog.Info("Starting external scaler fixture", "address", grpcListener.Addr().String())
		serverErrors <- grpcServer.Serve(grpcListener)
	}()
	go func() {
		slog.Info("Starting external scaler control service", "address", controlListener.Addr().String())
		serverErrors <- controlServer.Serve(controlListener)
	}()

	select {
	case <-ctx.Done():
		return stopServers(grpcServer, controlServer)
	case err := <-serverErrors:
		if errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		_ = stopServers(grpcServer, controlServer)
		return fmt.Errorf("serve external scaler fixture: %w", err)
	}
}

func stopServers(grpcServer *grpc.Server, controlServer *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpError := controlServer.Shutdown(shutdownCtx)

	grpcStopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(grpcStopped)
	}()
	select {
	case <-grpcStopped:
	case <-shutdownCtx.Done():
		grpcServer.Stop()
	}
	if httpError != nil {
		return fmt.Errorf("shut down control HTTP service: %w", httpError)
	}
	return nil
}
