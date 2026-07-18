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

// Package externalscaler provides a controllable KEDA external scaler for
// non-GPU end-to-end tests. It is test infrastructure, not a production scaler.
package externalscaler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	externalpb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxControlBodySize = 64 << 10

// Config controls the metric exposed to KEDA.
type Config struct {
	MetricName      string
	TargetSize      float64
	InitialMetric   float64
	StreamHeartbeat time.Duration
}

// DefaultConfig returns a scaler that starts inactive and targets one pending
// request per replica.
func DefaultConfig() Config {
	return Config{
		MetricName:      "kama_pending_requests",
		TargetSize:      1,
		StreamHeartbeat: 30 * time.Second,
	}
}

// Snapshot is the externally visible state of the fixture scaler.
type Snapshot struct {
	Metric   float64 `json:"metric"`
	Active   bool    `json:"active"`
	Revision uint64  `json:"revision"`
}

type state struct {
	mu       sync.RWMutex
	metric   float64
	revision uint64
	watchers map[chan struct{}]struct{}
}

func newState(initialMetric float64) (*state, error) {
	if err := validateMetric(initialMetric); err != nil {
		return nil, err
	}
	return &state{
		metric:   initialMetric,
		watchers: make(map[chan struct{}]struct{}),
	}, nil
}

func (s *state) snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *state) snapshotLocked() Snapshot {
	return Snapshot{
		Metric:   s.metric,
		Active:   s.metric > 0,
		Revision: s.revision,
	}
}

func (s *state) setMetric(metric float64) error {
	if err := validateMetric(metric); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if metric == s.metric {
		return nil
	}
	s.metric = metric
	s.revision++
	for watcher := range s.watchers {
		select {
		case watcher <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *state) subscribe() (Snapshot, <-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	updates := make(chan struct{}, 1)
	s.watchers[updates] = struct{}{}
	unsubscribe := func() {
		s.mu.Lock()
		delete(s.watchers, updates)
		s.mu.Unlock()
	}
	return s.snapshotLocked(), updates, unsubscribe
}

func validateMetric(metric float64) error {
	if math.IsNaN(metric) || math.IsInf(metric, 0) || metric < 0 {
		return errors.New("metric must be a finite, non-negative number")
	}
	return nil
}

// Scaler implements KEDA v2.20's external and external-push scaler contract.
type Scaler struct {
	externalpb.UnimplementedExternalScalerServer

	config Config
	state  *state
}

var _ externalpb.ExternalScalerServer = (*Scaler)(nil)

// New returns a controllable external scaler.
func New(config Config) (*Scaler, error) {
	if config.MetricName == "" {
		return nil, errors.New("metric name must not be empty")
	}
	if math.IsNaN(config.TargetSize) || math.IsInf(config.TargetSize, 0) || config.TargetSize <= 0 {
		return nil, errors.New("target size must be a finite number greater than zero")
	}
	if config.StreamHeartbeat < 0 {
		return nil, errors.New("stream heartbeat must not be negative")
	}
	fixtureState, err := newState(config.InitialMetric)
	if err != nil {
		return nil, fmt.Errorf("initial metric: %w", err)
	}
	return &Scaler{config: config, state: fixtureState}, nil
}

// Snapshot returns a consistent copy of the current fixture state.
func (s *Scaler) Snapshot() Snapshot {
	return s.state.snapshot()
}

// SetMetric changes the value returned by polling and streaming RPCs.
func (s *Scaler) SetMetric(metric float64) error {
	return s.state.setMetric(metric)
}

// IsActive implements the polling activation RPC.
func (s *Scaler) IsActive(
	_ context.Context,
	reference *externalpb.ScaledObjectRef,
) (*externalpb.IsActiveResponse, error) {
	if reference == nil {
		return nil, status.Error(codes.InvalidArgument, "scaled object reference is required")
	}
	return &externalpb.IsActiveResponse{Result: s.Snapshot().Active}, nil
}

// GetMetricSpec returns the single deterministic metric exposed by the fixture.
func (s *Scaler) GetMetricSpec(
	_ context.Context,
	reference *externalpb.ScaledObjectRef,
) (*externalpb.GetMetricSpecResponse, error) {
	if reference == nil {
		return nil, status.Error(codes.InvalidArgument, "scaled object reference is required")
	}
	return &externalpb.GetMetricSpecResponse{
		MetricSpecs: []*externalpb.MetricSpec{{
			MetricName:      s.config.MetricName,
			TargetSizeFloat: s.config.TargetSize,
		}},
	}, nil
}

// GetMetrics returns the current synthetic pending-request count.
func (s *Scaler) GetMetrics(
	_ context.Context,
	request *externalpb.GetMetricsRequest,
) (*externalpb.GetMetricsResponse, error) {
	if request == nil || request.ScaledObjectRef == nil {
		return nil, status.Error(codes.InvalidArgument, "metrics request and scaled object reference are required")
	}
	if request.MetricName != s.config.MetricName {
		return nil, status.Errorf(codes.InvalidArgument, "unknown metric %q", request.MetricName)
	}
	return &externalpb.GetMetricsResponse{
		MetricValues: []*externalpb.MetricValue{{
			MetricName:       s.config.MetricName,
			MetricValueFloat: s.Snapshot().Metric,
		}},
	}, nil
}

// StreamIsActive implements KEDA's external-push activation RPC.
func (s *Scaler) StreamIsActive(
	reference *externalpb.ScaledObjectRef,
	stream externalpb.ExternalScaler_StreamIsActiveServer,
) error {
	if reference == nil {
		return status.Error(codes.InvalidArgument, "scaled object reference is required")
	}
	return s.streamIsActive(stream.Context(), func(active bool) error {
		return stream.Send(&externalpb.IsActiveResponse{Result: active})
	})
}

func (s *Scaler) streamIsActive(ctx context.Context, send func(bool) error) error {
	current, updates, unsubscribe := s.state.subscribe()
	defer unsubscribe()
	if err := send(current.Active); err != nil {
		return err
	}

	heartbeats, stopHeartbeat := heartbeatChannel(s.config.StreamHeartbeat)
	defer stopHeartbeat()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-updates:
			if err := send(s.Snapshot().Active); err != nil {
				return err
			}
		case <-heartbeats:
			if err := send(s.Snapshot().Active); err != nil {
				return err
			}
		}
	}
}

func heartbeatChannel(interval time.Duration) (<-chan time.Time, func()) {
	if interval == 0 {
		return nil, func() {}
	}
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

// ControlHandler returns an HTTP handler used by tests to inspect and update
// scaler state. PUT or POST /state accepts {"metric": number}.
func (s *Scaler) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writeControlJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /state", func(writer http.ResponseWriter, _ *http.Request) {
		writeControlJSON(writer, http.StatusOK, s.Snapshot())
	})
	mux.HandleFunc("PUT /state", s.handleStateUpdate)
	mux.HandleFunc("POST /state", s.handleStateUpdate)
	return mux
}

type stateUpdate struct {
	Metric *float64 `json:"metric"`
}

func (s *Scaler) handleStateUpdate(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxControlBodySize)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var update stateUpdate
	if err := decoder.Decode(&update); err != nil {
		writeControlError(writer, fmt.Sprintf("decode state update: %v", err))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeControlError(writer, "state update must contain one JSON object")
		return
	}
	if update.Metric == nil {
		writeControlError(writer, "metric is required")
		return
	}
	if err := s.SetMetric(*update.Metric); err != nil {
		writeControlError(writer, err.Error())
		return
	}
	writeControlJSON(writer, http.StatusOK, s.Snapshot())
}

func writeControlError(writer http.ResponseWriter, message string) {
	writeControlJSON(writer, http.StatusBadRequest, map[string]string{"error": message})
}

func writeControlJSON(writer http.ResponseWriter, statusCode int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	_ = json.NewEncoder(writer).Encode(value)
}
