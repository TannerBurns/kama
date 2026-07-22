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

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisorTransitionsProbesAndControlledArguments(t *testing.T) {
	fixture := newRuntimeFixture(t)
	var captured []string
	var captureMutex sync.Mutex
	supervisor, cancel, runDone := startTestSupervisor(t, fixture, func(_ string, arguments ...string) *exec.Cmd {
		captureMutex.Lock()
		captured = append([]string(nil), arguments...)
		captureMutex.Unlock()
		return exec.Command("sleep", "60")
	})

	waitForAddress(t, supervisor)
	assertStatus(t, supervisor.Address(), "/startupz", http.StatusOK)
	assertStatus(t, supervisor.Address(), "/livez", http.StatusOK)
	assertStatus(t, supervisor.Address(), "/readyz", http.StatusServiceUnavailable)
	fixture.ready.Store(true)
	waitForPhase(t, supervisor, PhaseReady)
	assertStatus(t, supervisor.Address(), "/readyz", http.StatusOK)
	state := readSupervisorState(t, supervisor.Address())
	if !state.Ready || state.Runtime.EffectiveContextTokens != 4096 || state.Child.PID < 1 {
		t.Fatalf("ready state = %#v", state)
	}
	captureMutex.Lock()
	arguments := append([]string(nil), captured...)
	captureMutex.Unlock()
	if !slices.Equal(arguments, validConfig().Arguments()) {
		t.Fatalf("child arguments = %#v, want %#v", arguments, validConfig().Arguments())
	}

	drainContext, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer drainCancel()
	if err := RequestDrain(drainContext, supervisor.Address()); err != nil {
		t.Fatal(err)
	}
	state = supervisor.Snapshot()
	if state.Phase != PhaseDraining || state.Ready {
		t.Fatalf("drained state = %#v", state)
	}
	assertStatus(t, supervisor.Address(), "/readyz", http.StatusServiceUnavailable)
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorDoesNotRetryFailedChild(t *testing.T) {
	fixture := newRuntimeFixture(t)
	var starts atomic.Int32
	supervisor, cancel, runDone := startTestSupervisor(t, fixture, func(_ string, _ ...string) *exec.Cmd {
		starts.Add(1)
		return exec.Command("sh", "-c", "exit 23")
	})
	waitForPhase(t, supervisor, PhaseLoadFailed)
	time.Sleep(5 * testProbeInterval)
	state := supervisor.Snapshot()
	if starts.Load() != 1 {
		t.Fatalf("child starts = %d, want 1", starts.Load())
	}
	if state.Child.ExitCode == nil || *state.Child.ExitCode != 23 || state.Ready {
		t.Fatalf("failed state = %#v", state)
	}
	waitForAddress(t, supervisor)
	assertStatus(t, supervisor.Address(), "/startupz", http.StatusOK)
	assertStatus(t, supervisor.Address(), "/livez", http.StatusOK)
	assertStatus(t, supervisor.Address(), "/readyz", http.StatusServiceUnavailable)
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestChildExitAfterReadinessIsTerminalExited(t *testing.T) {
	fixture := newRuntimeFixture(t)
	fixture.ready.Store(true)
	supervisor, cancel, runDone := startTestSupervisor(t, fixture, func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sleep", "60")
	})
	waitForPhase(t, supervisor, PhaseReady)
	fixture.ready.Store(false)
	waitForPhase(t, supervisor, PhaseLoading)
	supervisor.mu.RLock()
	child := supervisor.child
	supervisor.mu.RUnlock()
	if child == nil || child.Process == nil {
		t.Fatal("supervisor has no child process")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, supervisor, PhaseExited)
	if supervisor.Snapshot().Ready {
		t.Fatal("exited runtime remains ready")
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestArtifactFailureRemainsLiveWithoutStartingChild(t *testing.T) {
	fixture := newRuntimeFixture(t)
	var starts atomic.Int32
	config := validConfig()
	artifactRoot := t.TempDir()
	if err := os.WriteFile(artifactRoot+"/model.gguf", []byte("wrong-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := testOptions(fixture.server.URL, artifactRoot, func(_ string, _ ...string) *exec.Cmd {
		starts.Add(1)
		return exec.Command("sleep", "60")
	})
	supervisor := NewSupervisor(config, options)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- supervisor.Run(ctx) }()
	waitForPhase(t, supervisor, PhaseLoadFailed)
	if starts.Load() != 0 || supervisor.Snapshot().Reason != "ArtifactInvalid" {
		t.Fatalf("artifact failure = %#v, starts = %d", supervisor.Snapshot(), starts.Load())
	}
	assertStatus(t, supervisor.Address(), "/startupz", http.StatusOK)
	assertStatus(t, supervisor.Address(), "/livez", http.StatusOK)
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestArtifactDigestFailureRemainsLiveWithoutStartingChild(t *testing.T) {
	fixture := newRuntimeFixture(t)
	var starts atomic.Int32
	config := validConfig()
	artifactRoot := t.TempDir()
	if err := os.WriteFile(artifactRoot+"/model.gguf", []byte("GGUF"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := testOptions(fixture.server.URL, artifactRoot, func(_ string, _ ...string) *exec.Cmd {
		starts.Add(1)
		return exec.Command("sleep", "60")
	})
	supervisor := NewSupervisor(config, options)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- supervisor.Run(ctx) }()
	waitForPhase(t, supervisor, PhaseLoadFailed)
	if starts.Load() != 0 || supervisor.Snapshot().Reason != "ArtifactInvalid" {
		t.Fatalf("artifact digest failure = %#v, starts = %d", supervisor.Snapshot(), starts.Load())
	}
	assertStatus(t, supervisor.Address(), "/startupz", http.StatusOK)
	assertStatus(t, supervisor.Address(), "/livez", http.StatusOK)
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestDrainWaitsForActiveAndDeferredRequests(t *testing.T) {
	fixture := newRuntimeFixture(t)
	fixture.ready.Store(true)
	fixture.active.Store(1)
	fixture.deferred.Store(1)
	supervisor, cancel, runDone := startTestSupervisor(t, fixture, func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sleep", "60")
	})
	waitForPhase(t, supervisor, PhaseReady)

	drainDone := make(chan error, 1)
	go func() {
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		drainDone <- RequestDrain(ctx, supervisor.Address())
	}()
	waitForPhase(t, supervisor, PhaseDraining)
	select {
	case err := <-drainDone:
		t.Fatalf("drain returned while requests were active: %v", err)
	case <-time.After(5 * testProbeInterval):
	}
	fixture.active.Store(0)
	fixture.deferred.Store(0)
	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not complete after requests became idle")
	}
	if active := supervisor.Snapshot().Runtime.ActiveRequests; active != 0 {
		t.Fatalf("active requests after drain = %d", active)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestDrainAllowsActiveSSERequestToFinish(t *testing.T) {
	fixture := newRuntimeFixture(t)
	fixture.ready.Store(true)
	supervisor, cancel, runDone := startTestSupervisor(t, fixture, func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sleep", "60")
	})
	defer cancel()
	waitForPhase(t, supervisor, PhaseReady)

	streamRequest, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		fixture.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	streamRequest.Header.Set("Content-Type", "application/json")
	streamResponse, err := http.DefaultClient.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = streamResponse.Body.Close() }()
	if streamResponse.StatusCode != http.StatusOK {
		t.Fatalf("SSE response status = %d, want %d", streamResponse.StatusCode, http.StatusOK)
	}
	if contentType := streamResponse.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("SSE content type = %q, want text/event-stream", contentType)
	}
	streamReader := bufio.NewReader(streamResponse.Body)
	firstEvent, err := streamReader.ReadString('\n')
	if err != nil || !strings.Contains(firstEvent, `"content":"first"`) {
		t.Fatalf("first SSE event = %q, error = %v", firstEvent, err)
	}
	waitForActiveRuntimeRequest(t, supervisor)

	drainDone := make(chan error, 1)
	go func() {
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		drainDone <- RequestDrain(ctx, supervisor.Address())
	}()
	waitForPhase(t, supervisor, PhaseDraining)
	assertStatus(t, supervisor.Address(), "/readyz", http.StatusServiceUnavailable)
	select {
	case err := <-drainDone:
		t.Fatalf("drain returned while the SSE request was active: %v", err)
	case <-time.After(5 * testProbeInterval):
	}
	if supervisor.Snapshot().Child.ExitCode != nil {
		t.Fatal("child exited before the active SSE request completed")
	}

	fixture.releaseSSE()
	remainder, err := io.ReadAll(streamReader)
	if err != nil {
		t.Fatalf("read completed SSE response: %v", err)
	}
	body := firstEvent + string(remainder)
	if !strings.Contains(body, `"content":"second"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("completed SSE body = %q", body)
	}
	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not complete after the SSE stream released its slot")
	}
	state := supervisor.Snapshot()
	if state.Ready || state.Runtime.ActiveRequests != 0 || state.Child.ExitCode == nil {
		t.Fatalf("state after SSE drain = %#v", state)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestAcceleratorReadinessRequiresObservedCUDAOffload(t *testing.T) {
	fixture := newRuntimeFixture(t)
	fixture.ready.Store(true)
	config := validConfig()
	config.Mode = ModeAccelerator
	supervisor, cancel, runDone := startTestSupervisorWithConfig(t, config, fixture, func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sleep", "60")
	})
	waitForReason(t, supervisor, "AcceleratorNotReady")
	if supervisor.Snapshot().Ready {
		t.Fatal("accelerator runtime became ready without observed CUDA offload")
	}
	supervisor.observeStartupLog("device_info:")
	supervisor.observeStartupLog("cmn  common_param:   - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)")
	supervisor.observeStartupLog("load_tensors: offloaded 12/24 layers to GPU")
	time.Sleep(3 * testProbeInterval)
	if state := supervisor.Snapshot(); state.Ready || state.Phase == PhaseReady {
		t.Fatalf("accelerator runtime became ready with partial offload: %#v", state)
	}
	supervisor.observeStartupLog("load_tensors: offloaded 24/24 layers to GPU")
	waitForPhase(t, supervisor, PhaseReady)

	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := RequestDrain(ctx, supervisor.Address()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestAcceleratorReadinessRequiresExactlyOneVisibleDevice(t *testing.T) {
	fixture := newRuntimeFixture(t)
	fixture.ready.Store(true)
	config := validConfig()
	config.Mode = ModeAccelerator
	supervisor, cancel, runDone := startTestSupervisorWithConfig(t, config, fixture, func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sleep", "60")
	})
	supervisor.observeStartupLog("ggml_cuda_init: found 2 CUDA devices (Total VRAM: 81072 MiB):")
	supervisor.observeStartupLog("load_tensors: offloaded 24/24 layers to GPU")
	waitForReason(t, supervisor, "AcceleratorNotReady")
	if state := supervisor.Snapshot(); state.Ready || state.Runtime.VisibleAccelerators == nil ||
		*state.Runtime.VisibleAccelerators != 2 {
		t.Fatalf("multiple-device accelerator state = %#v", state)
	}
	supervisor.observeStartupLog("ggml_cuda_init: found 1 CUDA devices (Total VRAM: 40536 MiB):")
	time.Sleep(3 * testProbeInterval)
	if state := supervisor.Snapshot(); state.Ready || state.Runtime.VisibleAccelerators == nil ||
		*state.Runtime.VisibleAccelerators != 2 {
		t.Fatalf("later smaller inventory weakened multiple-device state = %#v", state)
	}

	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := RequestDrain(ctx, supervisor.Address()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestAcceleratorStartupFactsAndLogRedaction(t *testing.T) {
	config := validConfig()
	config.Mode = ModeAccelerator
	config.Default()
	var logs bytes.Buffer
	supervisor := NewSupervisor(config, Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	childOutput := "PRIVATE-BODY-" + strings.Repeat("x", maximumLogFragmentBytes*2) + "\n" +
		"device_info:\n" +
		"cmn  common_param:   - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)\n" +
		"  - CPU     : Intel(R) Xeon(R) CPU (193053 MiB, 191000 MiB free)\n" +
		"  Device 0: NVIDIA GeForce RTX 4090, compute capability 8.9, VMM: yes, VRAM: 24564 MiB\n" +
		"  - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)\n" +
		"load_tensors: offloaded 24/24 layers to GPU\n"
	supervisor.consumeChildLog("stderr", strings.NewReader(childOutput))
	state := supervisor.Snapshot()
	if !state.Runtime.AcceleratorDetected || state.Runtime.VisibleAccelerators == nil ||
		*state.Runtime.VisibleAccelerators != 1 || state.Runtime.OffloadedLayers == nil ||
		*state.Runtime.OffloadedLayers != 24 || state.Runtime.TotalLayers == nil ||
		*state.Runtime.TotalLayers != 24 || state.Runtime.AcceleratorDevice != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("accelerator state = %#v", state.Runtime)
	}
	if strings.Contains(logs.String(), "PRIVATE-BODY") || strings.Contains(logs.String(), "NVIDIA GeForce") ||
		strings.Contains(logs.String(), "24082 MiB") {
		t.Fatalf("forwarded log exposed child content: %s", logs.String())
	}
	if got := sanitizeLogLine("  - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)"); got != acceleratorInventoryLogEvent {
		t.Fatalf("CUDA inventory log = %q", got)
	}
	if got := sanitizeLogLine("cmn  common_param:   - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)"); got != acceleratorInventoryLogEvent {
		t.Fatalf("prefixed CUDA inventory log = %q", got)
	}
	if got := sanitizeLogLine("  - CPU     : Intel(R) Xeon(R) CPU (193053 MiB, 191000 MiB free)"); got != suppressedLogEvent {
		t.Fatalf("CPU inventory log = %q", got)
	}
	if got := sanitizeLogLine(`request: {"prompt":"private text"}`); got != "sensitive-output-redacted" {
		t.Fatalf("sensitive log = %q", got)
	}
	if got := sanitizeLogLine("user supplied CUDA0: private text"); got != suppressedLogEvent {
		t.Fatalf("unclassified log = %q", got)
	}
}

func TestAcceleratorInventoryRejectsMultipleAndMalformedDevices(t *testing.T) {
	config := validConfig()
	config.Mode = ModeAccelerator
	config.Default()
	supervisor := NewSupervisor(config, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	supervisor.observeStartupLog("user supplied CUDA0: private text")
	supervisor.observeStartupLog("user common_param: - CUDA0 : private text (24082 MiB, 23687 MiB free)")
	supervisor.observeStartupLog("  - CPU     : Intel(R) Xeon(R) CPU (193053 MiB, 191000 MiB free)")
	if state := supervisor.Snapshot(); state.Runtime.VisibleAccelerators != nil || state.Runtime.AcceleratorDetected {
		t.Fatalf("malformed inventory changed state = %#v", state.Runtime)
	}

	supervisor.observeStartupLog("  - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)")
	supervisor.observeStartupLog("  - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)")
	supervisor.observeStartupLog("  Device 0: NVIDIA GeForce RTX 4090, compute capability 8.9, VMM: yes, VRAM: 24564 MiB")
	state := supervisor.Snapshot()
	if state.Runtime.VisibleAccelerators == nil || *state.Runtime.VisibleAccelerators != 1 ||
		state.Runtime.AcceleratorDevice != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("duplicate inventory state = %#v", state.Runtime)
	}

	supervisor.observeStartupLog("  - CUDA1   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)")
	state = supervisor.Snapshot()
	if state.Runtime.VisibleAccelerators == nil || *state.Runtime.VisibleAccelerators != 2 ||
		state.Runtime.AcceleratorDevice != "" {
		t.Fatalf("multiple-device inventory state = %#v", state.Runtime)
	}
}

func TestCPUStartupIgnoresAcceleratorFacts(t *testing.T) {
	config := validConfig()
	config.Default()
	supervisor := NewSupervisor(config, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	supervisor.observeStartupLog("  - CUDA0   : NVIDIA GeForce RTX 4090 (24082 MiB, 23687 MiB free)")
	supervisor.observeStartupLog("load_tensors: offloaded 33/33 layers to GPU")
	state := supervisor.Snapshot()
	if state.Runtime.AcceleratorDetected || state.Runtime.VisibleAccelerators != nil ||
		state.Runtime.OffloadedLayers != nil || state.Runtime.TotalLayers != nil ||
		state.Runtime.AcceleratorDevice != "" {
		t.Fatalf("CPU startup accepted accelerator facts = %#v", state.Runtime)
	}
}

func TestPropertiesMustMatchPinnedRuntimeContract(t *testing.T) {
	config := validConfig()
	config.MaxContextTokens = 4096
	supervisor := NewSupervisor(config, Options{
		LlamaCPPCommit:      testLlamaCommit,
		LlamaCPPBuildNumber: testLlamaBuildNumber,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	valid := validProperties(config)
	if err := supervisor.validateProperties(valid); err != nil {
		t.Fatalf("valid properties rejected: %v", err)
	}

	tests := map[string]func(*propertiesState){
		"model path": func(properties *propertiesState) { properties.ModelPath = "/models/other.gguf" },
		"slot count": func(properties *propertiesState) { properties.TotalSlots = 2 },
		"context":    func(properties *propertiesState) { properties.DefaultGenerationSettings.NContext = 2048 },
		"build number": func(properties *propertiesState) {
			properties.BuildInfo = "b9444-" + testLlamaCommit
		},
		"build commit": func(properties *propertiesState) {
			properties.BuildInfo = "b" + testLlamaBuildNumber + "-" + strings.Repeat("b", 40)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			properties := valid
			mutate(&properties)
			if err := supervisor.validateProperties(properties); err == nil {
				t.Fatal("validateProperties() error = nil")
			}
		})
	}
}

func TestDrainEndpointRejectsNonLoopbackCaller(t *testing.T) {
	supervisor := NewSupervisor(validConfig(), Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := httptest.NewRequest(http.MethodPost, "http://runtime/drain", nil)
	request.RemoteAddr = "192.0.2.10:54321"
	response := httptest.NewRecorder()
	supervisor.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST /drain status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if err := RequestDrain(context.Background(), "192.0.2.10:8081"); err == nil {
		t.Fatal("RequestDrain() accepted a non-loopback address")
	}
}

const (
	testProbeInterval    = 10 * time.Millisecond
	testLlamaCommit      = "b4d6c7d8ff69c2e05e4e8ee7e6e710a08abd7b45"
	testLlamaBuildNumber = "10091"
)

type runtimeFixture struct {
	server     *httptest.Server
	ready      atomic.Bool
	active     atomic.Int32
	deferred   atomic.Int32
	streamDone chan struct{}
	streamOnce sync.Once
	context    int64
	totalSlots int32
	modelPath  string
	buildInfo  string
}

func newRuntimeFixture(t *testing.T) *runtimeFixture {
	t.Helper()
	fixture := &runtimeFixture{
		context:    4096,
		totalSlots: 1,
		modelPath:  "/models/model.gguf",
		buildInfo:  "b" + testLlamaBuildNumber + "-" + testLlamaCommit,
		streamDone: make(chan struct{}),
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/health":
			if !fixture.ready.Load() {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusOK)
		case "/slots":
			active := fixture.active.Load() > 0
			slots := make([]map[string]any, fixture.totalSlots)
			for index := range slots {
				isProcessing := active && index == 0
				idTask := -1
				if isProcessing {
					idTask = 7
				}
				slots[index] = map[string]any{
					"id": index, "id_task": idTask, "n_ctx": fixture.context, "is_processing": isProcessing,
				}
			}
			_ = json.NewEncoder(writer).Encode(slots)
		case "/props":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"default_generation_settings": map[string]any{"n_ctx": fixture.context},
				"total_slots":                 fixture.totalSlots,
				"model_path":                  fixture.modelPath,
				"build_info":                  fixture.buildInfo,
			})
		case "/metrics":
			_, _ = io.WriteString(writer, "llamacpp:requests_deferred "+
				strconv.FormatInt(int64(fixture.deferred.Load()), 10)+"\n")
		case "/v1/chat/completions":
			if !fixture.active.CompareAndSwap(0, 1) {
				writer.WriteHeader(http.StatusTooManyRequests)
				return
			}
			defer fixture.active.Store(0)
			flusher, ok := writer.(http.Flusher)
			if !ok {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
			flusher.Flush()
			select {
			case <-fixture.streamDone:
			case <-request.Context().Done():
				return
			}
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\n")
			_, _ = io.WriteString(writer, "data: [DONE]\n\n")
			flusher.Flush()
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		fixture.releaseSSE()
		fixture.server.Close()
	})
	return fixture
}

func (fixture *runtimeFixture) releaseSSE() {
	fixture.streamOnce.Do(func() { close(fixture.streamDone) })
}

func startTestSupervisor(t *testing.T, fixture *runtimeFixture, command CommandFactory) (*Supervisor, context.CancelFunc, <-chan error) {
	return startTestSupervisorWithConfig(t, validConfig(), fixture, command)
}

func startTestSupervisorWithConfig(
	t *testing.T,
	config Config,
	fixture *runtimeFixture,
	command CommandFactory,
) (*Supervisor, context.CancelFunc, <-chan error) {
	t.Helper()
	fixture.totalSlots = config.DesiredConcurrency
	fixture.modelPath = config.ModelPath()
	fixture.buildInfo = "b" + testLlamaBuildNumber + "-" + testLlamaCommit
	if config.MaxContextTokens > 0 {
		fixture.context = config.MaxContextTokens
	}
	artifactRoot := t.TempDir()
	if err := os.WriteFile(artifactRoot+"/model.gguf", []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	supervisor := NewSupervisor(config, testOptions(fixture.server.URL, artifactRoot, command))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	return supervisor, cancel, done
}

func testOptions(llamaURL, artifactRoot string, command CommandFactory) Options {
	return Options{
		ArtifactRoot:             artifactRoot,
		DiagnosticAddress:        "127.0.0.1:0",
		LlamaBaseURL:             llamaURL,
		ProbeInterval:            testProbeInterval,
		ProbeTimeout:             250 * time.Millisecond,
		EndpointPropagationDelay: 10 * time.Millisecond,
		ChildShutdownTimeout:     250 * time.Millisecond,
		HTTPShutdownTimeout:      250 * time.Millisecond,
		LlamaCPPCommit:           testLlamaCommit,
		LlamaCPPBuildNumber:      testLlamaBuildNumber,
		Command:                  command,
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func validProperties(config Config) propertiesState {
	contextTokens := config.MaxContextTokens
	if contextTokens == 0 {
		contextTokens = 4096
	}
	properties := propertiesState{
		TotalSlots: config.DesiredConcurrency,
		ModelPath:  config.ModelPath(),
		BuildInfo:  "b" + testLlamaBuildNumber + "-" + testLlamaCommit,
	}
	properties.DefaultGenerationSettings.NContext = contextTokens
	return properties
}

func waitForAddress(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if supervisor.Address() != "" {
			return
		}
		time.Sleep(testProbeInterval)
	}
	t.Fatal("supervisor did not bind diagnostics")
}

func waitForPhase(t *testing.T, supervisor *Supervisor, phase Phase) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if supervisor.Snapshot().Phase == phase {
			return
		}
		time.Sleep(testProbeInterval)
	}
	t.Fatalf("supervisor phase = %s, want %s; state = %#v", supervisor.Snapshot().Phase, phase, supervisor.Snapshot())
}

func waitForReason(t *testing.T, supervisor *Supervisor, reason string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if supervisor.Snapshot().Reason == reason {
			return
		}
		time.Sleep(testProbeInterval)
	}
	t.Fatalf("supervisor reason = %s, want %s; state = %#v", supervisor.Snapshot().Reason, reason, supervisor.Snapshot())
}

func waitForActiveRuntimeRequest(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if supervisor.Snapshot().Runtime.ActiveRequests == 1 {
			return
		}
		time.Sleep(testProbeInterval)
	}
	t.Fatalf("supervisor did not observe the active SSE request; state = %#v", supervisor.Snapshot())
}

func assertStatus(t *testing.T, address, path string, want int) {
	t.Helper()
	response, err := http.Get("http://" + address + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != want {
		t.Fatalf("GET %s status = %d, want %d", path, response.StatusCode, want)
	}
}

func readSupervisorState(t *testing.T, address string) State {
	t.Helper()
	response, err := http.Get("http://" + address + "/state")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var state State
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
