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

package externalscaler

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	externalpb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

var testReference = &externalpb.ScaledObjectRef{
	Name:      "fixture-consumer",
	Namespace: "default",
}

func TestPollingMetricContract(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.StreamHeartbeat = 0
	scaler := mustNewScaler(t, config)

	active, err := scaler.IsActive(context.Background(), testReference)
	if err != nil {
		t.Fatalf("IsActive(): %v", err)
	}
	if active.Result {
		t.Fatal("initial IsActive result = true, want false")
	}
	spec, err := scaler.GetMetricSpec(context.Background(), testReference)
	if err != nil {
		t.Fatalf("GetMetricSpec(): %v", err)
	}
	if len(spec.MetricSpecs) != 1 || spec.MetricSpecs[0].MetricName != config.MetricName ||
		spec.MetricSpecs[0].TargetSizeFloat != config.TargetSize {
		t.Fatalf("metric spec = %+v, want name %q and target %v", spec.MetricSpecs, config.MetricName, config.TargetSize)
	}

	if err := scaler.SetMetric(3); err != nil {
		t.Fatalf("SetMetric(): %v", err)
	}
	metrics, err := scaler.GetMetrics(context.Background(), &externalpb.GetMetricsRequest{
		ScaledObjectRef: testReference,
		MetricName:      config.MetricName,
	})
	if err != nil {
		t.Fatalf("GetMetrics(): %v", err)
	}
	if len(metrics.MetricValues) != 1 || metrics.MetricValues[0].MetricValueFloat != 3 {
		t.Fatalf("metric values = %+v, want one value of 3", metrics.MetricValues)
	}
	active, err = scaler.IsActive(context.Background(), testReference)
	if err != nil {
		t.Fatalf("IsActive() after update: %v", err)
	}
	if !active.Result {
		t.Fatal("IsActive result after update = false, want true")
	}
}

func TestPollingRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	scaler := mustNewScaler(t, DefaultConfig())
	if _, err := scaler.IsActive(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("IsActive(nil) code = %v, want %v", status.Code(err), codes.InvalidArgument)
	}
	_, err := scaler.GetMetrics(context.Background(), &externalpb.GetMetricsRequest{
		ScaledObjectRef: testReference,
		MetricName:      "unknown",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("GetMetrics(unknown) code = %v, want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestStreamIsActiveSendsInitialAndUpdatedState(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.StreamHeartbeat = 0
	scaler := mustNewScaler(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	responses := make(chan bool, 2)
	streamError := make(chan error, 1)
	go func() {
		streamError <- scaler.streamIsActive(ctx, func(active bool) error {
			responses <- active
			return nil
		})
	}()

	if active := receiveActiveState(t, responses); active {
		t.Fatal("initial streamed active state = true, want false")
	}
	if err := scaler.SetMetric(1); err != nil {
		t.Fatalf("SetMetric(): %v", err)
	}
	if active := receiveActiveState(t, responses); !active {
		t.Fatal("updated streamed active state = false, want true")
	}
	cancel()
	select {
	case err := <-streamError:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
}

func TestGRPCPollingAndStreamingContract(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.StreamHeartbeat = 0
	scaler := mustNewScaler(t, config)
	listener := bufconn.Listen(1 << 20)
	defer func() { _ = listener.Close() }()
	grpcServer := grpc.NewServer()
	defer grpcServer.Stop()
	externalpb.RegisterExternalScalerServer(grpcServer, scaler)
	go func() { _ = grpcServer.Serve(listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := grpc.NewClient(
		"passthrough:///fixture",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("create gRPC client: %v", err)
	}
	defer func() { _ = connection.Close() }()
	client := externalpb.NewExternalScalerClient(connection)

	polled, err := client.IsActive(ctx, testReference)
	if err != nil {
		t.Fatalf("gRPC IsActive(): %v", err)
	}
	if polled.Result {
		t.Fatal("initial gRPC IsActive result = true, want false")
	}
	stream, err := client.StreamIsActive(ctx, testReference)
	if err != nil {
		t.Fatalf("gRPC StreamIsActive(): %v", err)
	}
	initial, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive initial streamed state: %v", err)
	}
	if initial.Result {
		t.Fatal("initial gRPC streamed state = true, want false")
	}
	if err := scaler.SetMetric(2); err != nil {
		t.Fatalf("SetMetric(): %v", err)
	}
	updated, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive updated streamed state: %v", err)
	}
	if !updated.Result {
		t.Fatal("updated gRPC streamed state = false, want true")
	}
}

func TestControlHandlerUpdatesAndReportsState(t *testing.T) {
	t.Parallel()

	scaler := mustNewScaler(t, DefaultConfig())
	handler := scaler.ControlHandler()
	response := serveControlRequest(handler, http.MethodPut, "/state", `{"metric":4}`)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT /state status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var updated Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated state: %v", err)
	}
	if updated.Metric != 4 || !updated.Active || updated.Revision != 1 {
		t.Fatalf("updated state = %+v, want metric 4, active, revision 1", updated)
	}

	response = serveControlRequest(handler, http.MethodGet, "/state", "")
	var fetched Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode fetched state: %v", err)
	}
	if fetched != updated {
		t.Fatalf("GET /state = %+v, want %+v", fetched, updated)
	}

	response = serveControlRequest(handler, http.MethodPost, "/state", `{"metric":-1}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid POST /state status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestConfigAndMetricValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*Config)
	}{
		{name: "empty metric name", configure: func(config *Config) { config.MetricName = "" }},
		{name: "zero target", configure: func(config *Config) { config.TargetSize = 0 }},
		{name: "negative initial metric", configure: func(config *Config) { config.InitialMetric = -1 }},
		{name: "negative heartbeat", configure: func(config *Config) { config.StreamHeartbeat = -time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := DefaultConfig()
			test.configure(&config)
			if _, err := New(config); err == nil {
				t.Fatal("New() returned nil error")
			}
		})
	}

	scaler := mustNewScaler(t, DefaultConfig())
	if err := scaler.SetMetric(math.Inf(1)); err == nil {
		t.Fatal("SetMetric(+Inf) returned nil error")
	}
}

func mustNewScaler(t *testing.T, config Config) *Scaler {
	t.Helper()
	scaler, err := New(config)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return scaler
}

func receiveActiveState(t *testing.T, responses <-chan bool) bool {
	t.Helper()
	select {
	case active := <-responses:
		return active
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for streamed active state")
		return false
	}
}

func serveControlRequest(handler http.Handler, method string, path string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
