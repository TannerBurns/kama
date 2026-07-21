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
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultServerBinary             = "/usr/local/bin/llama-server"
	defaultDiagnosticAddress        = ":8081"
	defaultLlamaBaseURL             = "http://127.0.0.1:8080"
	defaultProbeInterval            = 500 * time.Millisecond
	defaultProbeTimeout             = 2 * time.Second
	defaultEndpointPropagationDelay = 5 * time.Second
	defaultChildShutdownTimeout     = 10 * time.Second
	defaultHTTPShutdownTimeout      = 5 * time.Second
	maximumResponseBytes            = 1 << 20
	maximumLogFragmentBytes         = 4096
	diagnosticStatusKey             = "status"
)

var (
	// LlamaCPPCommit is set at link time in production serving images.
	LlamaCPPCommit = "unknown"
	// LlamaCPPBuildNumber is set at link time in production serving images.
	LlamaCPPBuildNumber = "unknown"
	sha1Pattern         = regexp.MustCompile(`^[a-f0-9]{40}$`)
	buildNumberPattern  = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
)

// Phase is the bounded supervisor lifecycle exported through /state.
type Phase string

const (
	PhaseInitializing Phase = "Initializing"
	PhaseLoading      Phase = "Loading"
	PhaseReady        Phase = "Ready"
	PhaseDraining     Phase = "Draining"
	PhaseLoadFailed   Phase = "LoadFailed"
	PhaseExited       Phase = "Exited"
)

// StateArtifact contains only the immutable artifact identity, not its path.
type StateArtifact struct {
	UID    string `json:"uid"`
	Digest string `json:"digest"`
}

// RuntimeState contains bounded facts safe for control-plane diagnostics.
type RuntimeState struct {
	Mode                   Mode   `json:"mode"`
	MaxContextTokens       int64  `json:"maxContextTokens,omitempty"`
	EffectiveContextTokens int64  `json:"effectiveContextTokens,omitempty"`
	DesiredConcurrency     int32  `json:"desiredConcurrency"`
	ActiveRequests         int32  `json:"activeRequests"`
	LlamaCPPCommit         string `json:"llamaCPPCommit"`
	LlamaCPPBuildNumber    string `json:"llamaCPPBuildNumber"`
	AcceleratorDetected    bool   `json:"acceleratorDetected"`
	VisibleAccelerators    *int32 `json:"visibleAccelerators,omitempty"`
	OffloadedLayers        *int32 `json:"offloadedLayers,omitempty"`
	TotalLayers            *int32 `json:"totalLayers,omitempty"`
	AcceleratorDevice      string `json:"acceleratorDevice,omitempty"`
}

// ChildState contains bounded process lifecycle information.
type ChildState struct {
	PID      int  `json:"pid,omitempty"`
	ExitCode *int `json:"exitCode,omitempty"`
}

// State is returned by the supervisor's Pod-internal /state endpoint.
type State struct {
	SchemaVersion string             `json:"schemaVersion"`
	Phase         Phase              `json:"phase"`
	Reason        string             `json:"reason,omitempty"`
	Message       string             `json:"message,omitempty"`
	Ready         bool               `json:"ready"`
	Deployment    DeploymentIdentity `json:"deployment"`
	Artifact      StateArtifact      `json:"artifact"`
	Runtime       RuntimeState       `json:"runtime"`
	Child         ChildState         `json:"child"`
	ObservedAt    time.Time          `json:"observedAt"`
}

// CommandFactory allows focused subprocess tests without making the child
// executable user-configurable.
type CommandFactory func(binary string, arguments ...string) *exec.Cmd

// Options controls process-owned supervisor mechanics. Controller-authored
// JSON cannot set any of these values.
type Options struct {
	ServerBinary             string
	ArtifactRoot             string
	DiagnosticAddress        string
	LlamaBaseURL             string
	ProbeInterval            time.Duration
	ProbeTimeout             time.Duration
	EndpointPropagationDelay time.Duration
	ChildShutdownTimeout     time.Duration
	HTTPShutdownTimeout      time.Duration
	LlamaCPPCommit           string
	LlamaCPPBuildNumber      string
	Command                  CommandFactory
	Logger                   *slog.Logger
}

func (options *Options) defaultValues() {
	if options.ServerBinary == "" {
		options.ServerBinary = defaultServerBinary
	}
	if options.ArtifactRoot == "" {
		options.ArtifactRoot = ModelMountRoot
	}
	if options.DiagnosticAddress == "" {
		options.DiagnosticAddress = defaultDiagnosticAddress
	}
	if options.LlamaBaseURL == "" {
		options.LlamaBaseURL = defaultLlamaBaseURL
	}
	if options.ProbeInterval == 0 {
		options.ProbeInterval = defaultProbeInterval
	}
	if options.ProbeTimeout == 0 {
		options.ProbeTimeout = defaultProbeTimeout
	}
	if options.EndpointPropagationDelay == 0 {
		options.EndpointPropagationDelay = defaultEndpointPropagationDelay
	}
	if options.ChildShutdownTimeout == 0 {
		options.ChildShutdownTimeout = defaultChildShutdownTimeout
	}
	if options.HTTPShutdownTimeout == 0 {
		options.HTTPShutdownTimeout = defaultHTTPShutdownTimeout
	}
	if options.LlamaCPPCommit == "" {
		options.LlamaCPPCommit = LlamaCPPCommit
	}
	if options.LlamaCPPBuildNumber == "" {
		options.LlamaCPPBuildNumber = LlamaCPPBuildNumber
	}
	if options.Command == nil {
		options.Command = exec.Command
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
}

// Supervisor owns one llama-server child for the lifetime of a Pod. It never
// retries a failed child; Kubernetes replacement or a new fingerprint is the
// only retry boundary.
type Supervisor struct {
	config       Config
	options      Options
	initialError error

	mu          sync.RWMutex
	state       State
	initialized bool
	address     string
	child       *exec.Cmd
	becameReady bool

	childDone     chan struct{}
	childDoneOnce sync.Once
	drainDone     chan struct{}
	drainOnce     sync.Once
}

// NewSupervisor constructs a supervisor. Config and artifact validation run
// after diagnostics start so deterministic failures remain live but unready.
func NewSupervisor(config Config, options Options) *Supervisor {
	options.defaultValues()
	return newSupervisor(config, options, nil)
}

// NewFailedSupervisor constructs a terminal, diagnosable supervisor for a
// configuration document that could not be decoded.
func NewFailedSupervisor(config Config, reason error, options Options) *Supervisor {
	options.defaultValues()
	return newSupervisor(config, options, reason)
}

func newSupervisor(config Config, options Options, initialError error) *Supervisor {
	now := time.Now().UTC()
	return &Supervisor{
		config:       config,
		options:      options,
		initialError: initialError,
		state: State{
			SchemaVersion: SchemaVersion,
			Phase:         PhaseInitializing,
			Deployment:    config.Deployment,
			Artifact: StateArtifact{
				UID:    config.Artifact.UID,
				Digest: config.Artifact.Digest,
			},
			Runtime: RuntimeState{
				Mode:                config.Mode,
				MaxContextTokens:    config.MaxContextTokens,
				DesiredConcurrency:  config.DesiredConcurrency,
				LlamaCPPCommit:      options.LlamaCPPCommit,
				LlamaCPPBuildNumber: options.LlamaCPPBuildNumber,
			},
			ObservedAt: now,
		},
		childDone: make(chan struct{}),
		drainDone: make(chan struct{}),
	}
}

// Address returns the bound diagnostics address after Run starts. It is useful
// for tests that request an ephemeral port.
func (supervisor *Supervisor) Address() string {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	return supervisor.address
}

// Snapshot returns a race-free copy of the current sanitized state.
func (supervisor *Supervisor) Snapshot() State {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	state := supervisor.state
	if supervisor.state.Runtime.OffloadedLayers != nil {
		value := *supervisor.state.Runtime.OffloadedLayers
		state.Runtime.OffloadedLayers = &value
	}
	if supervisor.state.Runtime.VisibleAccelerators != nil {
		value := *supervisor.state.Runtime.VisibleAccelerators
		state.Runtime.VisibleAccelerators = &value
	}
	if supervisor.state.Runtime.TotalLayers != nil {
		value := *supervisor.state.Runtime.TotalLayers
		state.Runtime.TotalLayers = &value
	}
	if supervisor.state.Child.ExitCode != nil {
		value := *supervisor.state.Child.ExitCode
		state.Child.ExitCode = &value
	}
	return state
}

// Run serves diagnostics and supervises exactly one child until the context is
// canceled or the diagnostics listener fails.
func (supervisor *Supervisor) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", supervisor.options.DiagnosticAddress)
	if err != nil {
		return fmt.Errorf("listen for runtime diagnostics: %w", err)
	}
	supervisor.mu.Lock()
	supervisor.address = listener.Addr().String()
	supervisor.initialized = true
	supervisor.touchLocked()
	supervisor.mu.Unlock()

	httpServer := &http.Server{
		Handler:           supervisor.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- httpServer.Serve(listener)
	}()

	if supervisor.initialError != nil {
		supervisor.fail("InvalidConfig", supervisor.initialError)
		supervisor.closeChildDone()
	} else if err := supervisor.config.Validate(); err != nil {
		supervisor.fail("InvalidConfig", err)
		supervisor.closeChildDone()
	} else if err := supervisor.validateArtifactFiles(); err != nil {
		supervisor.fail("ArtifactInvalid", err)
		supervisor.closeChildDone()
	} else {
		supervisor.startChild()
	}

	var runError error
	select {
	case <-ctx.Done():
		supervisor.initiateDrain()
		<-supervisor.drainDone
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			runError = fmt.Errorf("serve runtime diagnostics: %w", err)
			supervisor.initiateDrain()
			<-supervisor.drainDone
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), supervisor.options.HTTPShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil && runError == nil {
		runError = fmt.Errorf("shut down runtime diagnostics: %w", err)
	}
	return runError
}

// Handler returns the Pod-internal supervisor diagnostics surface.
func (supervisor *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /startupz", supervisor.handleStartup)
	mux.HandleFunc("GET /livez", supervisor.handleLive)
	mux.HandleFunc("GET /readyz", supervisor.handleReady)
	mux.HandleFunc("GET /state", supervisor.handleState)
	mux.HandleFunc("POST /drain", supervisor.handleDrain)
	return mux
}

func (supervisor *Supervisor) startChild() {
	supervisor.setPhase(PhaseLoading, "ModelLoading", "llama-server is loading the verified artifact", false)
	command := supervisor.options.Command(supervisor.options.ServerBinary, supervisor.config.Arguments()...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		supervisor.fail("ChildStartFailed", err)
		supervisor.closeChildDone()
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		supervisor.fail("ChildStartFailed", err)
		supervisor.closeChildDone()
		return
	}
	if err := command.Start(); err != nil {
		supervisor.fail("ChildStartFailed", err)
		supervisor.closeChildDone()
		return
	}

	supervisor.mu.Lock()
	supervisor.child = command
	supervisor.state.Child.PID = command.Process.Pid
	supervisor.touchLocked()
	supervisor.mu.Unlock()
	go supervisor.consumeChildLog("stdout", stdout)
	go supervisor.consumeChildLog("stderr", stderr)
	go supervisor.waitForChild(command)
	go supervisor.monitorChild()
}

func (supervisor *Supervisor) waitForChild(command *exec.Cmd) {
	err := command.Wait()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}

	supervisor.mu.Lock()
	supervisor.state.Child.ExitCode = &exitCode
	if supervisor.state.Phase != PhaseDraining {
		supervisor.state.Ready = false
		if supervisor.becameReady {
			supervisor.state.Phase = PhaseExited
			supervisor.state.Reason = "ChildExited"
			supervisor.state.Message = "llama-server exited after becoming ready"
		} else {
			supervisor.state.Phase = PhaseLoadFailed
			supervisor.state.Reason = "ChildExited"
			supervisor.state.Message = "llama-server exited before becoming ready"
		}
		supervisor.touchLocked()
	}
	supervisor.mu.Unlock()
	supervisor.closeChildDone()
}

func (supervisor *Supervisor) monitorChild() {
	ticker := time.NewTicker(supervisor.options.ProbeInterval)
	defer ticker.Stop()
	for {
		supervisor.updateReadiness()
		select {
		case <-supervisor.childDone:
			return
		case <-ticker.C:
		}
	}
}

type slotState struct {
	NContext     int64 `json:"n_ctx"`
	IsProcessing bool  `json:"is_processing"`
}

type propertiesState struct {
	DefaultGenerationSettings struct {
		NContext int64 `json:"n_ctx"`
	} `json:"default_generation_settings"`
	TotalSlots int32  `json:"total_slots"`
	ModelPath  string `json:"model_path"`
	BuildInfo  string `json:"build_info"`
}

func (supervisor *Supervisor) updateReadiness() {
	supervisor.mu.RLock()
	terminal := supervisor.state.Phase == PhaseDraining || supervisor.state.Phase == PhaseLoadFailed ||
		supervisor.state.Phase == PhaseExited
	supervisor.mu.RUnlock()
	if terminal {
		return
	}

	if err := supervisor.getOK("/health"); err != nil {
		supervisor.setLoading("ModelLoading", "llama-server health is not ready")
		return
	}
	properties, err := supervisor.getProperties()
	if err != nil {
		supervisor.setLoading("RuntimeContractMismatch", "llama-server properties are unavailable")
		return
	}
	if err := supervisor.validateProperties(properties); err != nil {
		supervisor.setLoading("RuntimeContractMismatch", "llama-server properties do not match the requested runtime contract")
		return
	}
	slots, err := supervisor.getSlots()
	if err != nil || len(slots) != int(supervisor.config.DesiredConcurrency) {
		supervisor.setLoading("RuntimeContractMismatch", "llama-server slot state does not match the requested concurrency")
		return
	}
	effectiveContext := slots[0].NContext
	if effectiveContext < 1 {
		supervisor.setLoading("RuntimeContractMismatch", "llama-server did not report an effective context")
		return
	}
	active := int32(0)
	for _, slot := range slots {
		if slot.NContext != effectiveContext ||
			(supervisor.config.MaxContextTokens > 0 && slot.NContext != supervisor.config.MaxContextTokens) {
			supervisor.setLoading("RuntimeContractMismatch", "llama-server context does not match the requested context")
			return
		}
		if slot.IsProcessing {
			active++
		}
	}
	if supervisor.config.Mode == ModeAccelerator {
		supervisor.mu.RLock()
		acceleratorDetected := supervisor.state.Runtime.AcceleratorDetected
		visibleAccelerators := supervisor.state.Runtime.VisibleAccelerators
		offloadedLayers := supervisor.state.Runtime.OffloadedLayers
		totalLayers := supervisor.state.Runtime.TotalLayers
		supervisor.mu.RUnlock()
		if !acceleratorDetected || visibleAccelerators == nil || *visibleAccelerators != 1 {
			supervisor.setLoading("AcceleratorNotReady", "llama-server has not confirmed exactly one visible CUDA device")
			return
		}
		if offloadedLayers == nil || totalLayers == nil || *totalLayers < 1 || *offloadedLayers != *totalLayers {
			supervisor.setLoading("AcceleratorNotReady", "llama-server has not confirmed full CUDA layer offload")
			return
		}
	}

	supervisor.mu.Lock()
	if supervisor.state.Phase != PhaseDraining && supervisor.state.Phase != PhaseLoadFailed && supervisor.state.Phase != PhaseExited {
		supervisor.state.Phase = PhaseReady
		supervisor.state.Reason = "RuntimeReady"
		supervisor.state.Message = "llama-server is ready"
		supervisor.state.Ready = true
		supervisor.becameReady = true
		supervisor.state.Runtime.EffectiveContextTokens = effectiveContext
		supervisor.state.Runtime.ActiveRequests = active
		supervisor.touchLocked()
	}
	supervisor.mu.Unlock()
}

func (supervisor *Supervisor) validateProperties(properties propertiesState) error {
	if properties.ModelPath != supervisor.config.ModelPath() {
		return errors.New("llama-server model path differs from the requested model")
	}
	if properties.TotalSlots != supervisor.config.DesiredConcurrency {
		return errors.New("llama-server total slots differ from the requested concurrency")
	}
	effectiveContext := properties.DefaultGenerationSettings.NContext
	if effectiveContext < 1 ||
		(supervisor.config.MaxContextTokens > 0 && effectiveContext != supervisor.config.MaxContextTokens) {
		return errors.New("llama-server context differs from the requested context")
	}
	if !validLlamaBuildNumber(supervisor.options.LlamaCPPBuildNumber) ||
		!sha1Pattern.MatchString(supervisor.options.LlamaCPPCommit) {
		return errors.New("supervisor llama.cpp build identity is invalid")
	}
	expectedBuildInfo := "b" + supervisor.options.LlamaCPPBuildNumber + "-" + supervisor.options.LlamaCPPCommit
	if properties.BuildInfo != expectedBuildInfo {
		return errors.New("llama-server build identity differs from the supervisor build identity")
	}
	return nil
}

func (supervisor *Supervisor) validateArtifactFiles() error {
	for _, file := range supervisor.config.Artifact.Files {
		filename := filepathForArtifact(supervisor.options.ArtifactRoot, file.Path)
		info, err := os.Lstat(filename)
		if err != nil {
			return fmt.Errorf("inspect artifact file %q: %w", file.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact file %q is not a regular file", file.Path)
		}
		if info.Size() != file.Size {
			return fmt.Errorf("artifact file %q size does not match its verified identity", file.Path)
		}
		if err := verifyArtifactFileDigest(filename, file, sha256.New()); err != nil {
			return err
		}
	}
	return nil
}

func verifyArtifactFileDigest(filename string, identity ArtifactFile, digest hash.Hash) error {
	opened, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open artifact file %q: %w", identity.Path, err)
	}
	defer func() { _ = opened.Close() }()

	written, err := io.Copy(digest, opened)
	if err != nil {
		return fmt.Errorf("hash artifact file %q: %w", identity.Path, err)
	}
	if written != identity.Size || fmt.Sprintf("%x", digest.Sum(nil)) != identity.SHA256 {
		return fmt.Errorf("artifact file %q content does not match its verified identity", identity.Path)
	}
	return nil
}

func filepathForArtifact(root, relativePath string) string {
	return strings.TrimRight(root, "/") + "/" + strings.ReplaceAll(relativePath, "\\", "/")
}

func (supervisor *Supervisor) getOK(endpoint string) error {
	requestContext, cancel := context.WithTimeout(context.Background(), supervisor.options.ProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, supervisor.options.LlamaBaseURL+endpoint, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (supervisor *Supervisor) getProperties() (propertiesState, error) {
	requestContext, cancel := context.WithTimeout(context.Background(), supervisor.options.ProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, supervisor.options.LlamaBaseURL+"/props", nil)
	if err != nil {
		return propertiesState{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return propertiesState{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes))
		return propertiesState{}, fmt.Errorf("properties endpoint returned HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return propertiesState{}, fmt.Errorf("read llama-server properties: %w", err)
	}
	if len(payload) > maximumResponseBytes {
		return propertiesState{}, errors.New("llama-server properties exceed the response limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	var properties propertiesState
	if err := decoder.Decode(&properties); err != nil {
		return propertiesState{}, fmt.Errorf("decode llama-server properties: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return propertiesState{}, errors.New("llama-server properties must contain exactly one JSON object")
	}
	return properties, nil
}

func (supervisor *Supervisor) getSlots() ([]slotState, error) {
	requestContext, cancel := context.WithTimeout(context.Background(), supervisor.options.ProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, supervisor.options.LlamaBaseURL+"/slots", nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slots endpoint returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maximumResponseBytes))
	var slots []slotState
	if err := decoder.Decode(&slots); err != nil {
		return nil, fmt.Errorf("decode slot state: %w", err)
	}
	return slots, nil
}

func validLlamaBuildNumber(value string) bool {
	return buildNumberPattern.MatchString(value)
}

func (supervisor *Supervisor) activeRequests() (int32, error) {
	slots, err := supervisor.getSlots()
	if err != nil {
		return 0, err
	}
	active := int32(0)
	for _, slot := range slots {
		if slot.IsProcessing {
			active++
		}
	}
	deferred, err := supervisor.deferredRequests()
	if err != nil {
		return 0, err
	}
	active += deferred
	return active, nil
}

func (supervisor *Supervisor) deferredRequests() (int32, error) {
	requestContext, cancel := context.WithTimeout(context.Background(), supervisor.options.ProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, supervisor.options.LlamaBaseURL+"/metrics", nil)
	if err != nil {
		return 0, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("metrics endpoint returned HTTP %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, maximumResponseBytes))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "llamacpp:requests_deferred" {
			value, err := strconv.ParseFloat(fields[1], 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > math.MaxInt32 {
				return 0, errors.New("invalid deferred request metric")
			}
			return int32(value), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("deferred request metric is absent")
}

func (supervisor *Supervisor) initiateDrain() <-chan struct{} {
	supervisor.drainOnce.Do(func() {
		go supervisor.drain()
	})
	return supervisor.drainDone
}

func (supervisor *Supervisor) drain() {
	defer close(supervisor.drainDone)
	supervisor.setPhase(PhaseDraining, "Terminating", "runtime is draining requests", false)

	propagationTimer := time.NewTimer(supervisor.options.EndpointPropagationDelay)
	<-propagationTimer.C

	deadline := time.Now().Add(time.Duration(supervisor.config.DrainTimeoutSeconds) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-supervisor.childDone:
			return
		default:
		}
		active, err := supervisor.activeRequests()
		if err == nil {
			supervisor.mu.Lock()
			supervisor.state.Runtime.ActiveRequests = active
			supervisor.touchLocked()
			supervisor.mu.Unlock()
			if active == 0 {
				break
			}
		}
		timer := time.NewTimer(supervisor.options.ProbeInterval)
		<-timer.C
	}

	supervisor.mu.RLock()
	child := supervisor.child
	supervisor.mu.RUnlock()
	if child == nil || child.Process == nil {
		return
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		supervisor.options.Logger.Warn("Unable to terminate llama-server cleanly", "error", sanitizeMessage(err.Error()))
	}
	shutdownTimer := time.NewTimer(supervisor.options.ChildShutdownTimeout)
	defer shutdownTimer.Stop()
	select {
	case <-supervisor.childDone:
		return
	case <-shutdownTimer.C:
		if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			supervisor.options.Logger.Warn("Unable to kill llama-server at drain deadline", "error", sanitizeMessage(err.Error()))
		}
		<-supervisor.childDone
	}
}

func (supervisor *Supervisor) closeChildDone() {
	supervisor.childDoneOnce.Do(func() { close(supervisor.childDone) })
}

func (supervisor *Supervisor) fail(reason string, err error) {
	supervisor.setPhase(PhaseLoadFailed, reason, sanitizeMessage(err.Error()), false)
}

func (supervisor *Supervisor) setPhase(phase Phase, reason, message string, ready bool) {
	supervisor.mu.Lock()
	supervisor.state.Phase = phase
	supervisor.state.Reason = reason
	supervisor.state.Message = sanitizeMessage(message)
	supervisor.state.Ready = ready
	supervisor.touchLocked()
	supervisor.mu.Unlock()
}

func (supervisor *Supervisor) setLoading(reason, message string) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.state.Phase == PhaseDraining || supervisor.state.Phase == PhaseLoadFailed || supervisor.state.Phase == PhaseExited {
		return
	}
	supervisor.state.Phase = PhaseLoading
	supervisor.state.Reason = reason
	supervisor.state.Message = sanitizeMessage(message)
	supervisor.state.Ready = false
	supervisor.touchLocked()
}

func (supervisor *Supervisor) touchLocked() {
	supervisor.state.ObservedAt = time.Now().UTC()
}

func (supervisor *Supervisor) handleStartup(writer http.ResponseWriter, _ *http.Request) {
	supervisor.mu.RLock()
	initialized := supervisor.initialized
	supervisor.mu.RUnlock()
	if !initialized {
		http.Error(writer, "supervisor is initializing", http.StatusServiceUnavailable)
		return
	}
	writeStatus(writer, http.StatusOK, map[string]string{diagnosticStatusKey: "initialized"})
}

func (supervisor *Supervisor) handleLive(writer http.ResponseWriter, _ *http.Request) {
	writeStatus(writer, http.StatusOK, map[string]string{diagnosticStatusKey: "live"})
}

func (supervisor *Supervisor) handleReady(writer http.ResponseWriter, _ *http.Request) {
	state := supervisor.Snapshot()
	if !state.Ready || state.Phase != PhaseReady {
		writeStatus(writer, http.StatusServiceUnavailable, map[string]string{diagnosticStatusKey: "not-ready", "phase": string(state.Phase)})
		return
	}
	writeStatus(writer, http.StatusOK, map[string]string{diagnosticStatusKey: "ready"})
}

func (supervisor *Supervisor) handleState(writer http.ResponseWriter, _ *http.Request) {
	writeStatus(writer, http.StatusOK, supervisor.Snapshot())
}

func (supervisor *Supervisor) handleDrain(writer http.ResponseWriter, request *http.Request) {
	if !remoteIsLoopback(request.RemoteAddr) {
		writeStatus(writer, http.StatusForbidden, map[string]string{"error": "drain is restricted to loopback"})
		return
	}
	select {
	case <-request.Context().Done():
		return
	case <-supervisor.initiateDrain():
		writeStatus(writer, http.StatusOK, map[string]string{diagnosticStatusKey: "drained"})
	}
}

func remoteIsLoopback(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return false
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func writeStatus(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// RequestDrain invokes the loopback-only blocking drain endpoint used by a Pod
// preStop exec hook.
func RequestDrain(ctx context.Context, address string) error {
	host, port, err := net.SplitHostPort(address)
	parsedPort, portError := strconv.Atoi(port)
	parsedIP := net.ParseIP(host)
	if err != nil || portError != nil || parsedPort < 1 || parsedPort > 65535 || parsedIP == nil || !parsedIP.IsLoopback() {
		return errors.New("drain address must be a loopback IP and port")
	}
	endpoint := "http://" + net.JoinHostPort(host, port) + "/drain"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create drain request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("request runtime drain: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime drain returned HTTP %d", response.StatusCode)
	}
	return nil
}

var (
	cudaCountPattern = regexp.MustCompile(`(?i)^ggml_cuda_init: found ([0-9]+) CUDA devices`)
	offloadPattern   = regexp.MustCompile(`(?i)^(?:llm_)?load_tensors: offloaded ([0-9]+)/([1-9][0-9]*) layers to GPU`)
	devicePattern    = regexp.MustCompile(`(?i)^[[:space:]]*Device [0-9]+:[[:space:]]*([^,;]{1,96}),[[:space:]]*compute capability`)
)

func (supervisor *Supervisor) consumeChildLog(stream string, reader io.Reader) {
	buffered := bufio.NewReaderSize(reader, maximumLogFragmentBytes)
	for {
		fragment, err := buffered.ReadSlice('\n')
		if len(fragment) > 0 {
			raw := strings.TrimRight(string(fragment), "\r\n")
			if !errors.Is(err, bufio.ErrBufferFull) {
				supervisor.observeStartupLog(raw)
			}
			supervisor.options.Logger.Info("llama-server", "stream", stream, "event", sanitizeLogLine(raw))
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return
		default:
			supervisor.options.Logger.Warn("Unable to consume llama-server log", "stream", stream, "error", sanitizeMessage(err.Error()))
			return
		}
	}
}

func (supervisor *Supervisor) observeStartupLog(line string) {
	if supervisor.config.Mode != ModeAccelerator {
		return
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if match := cudaCountPattern.FindStringSubmatch(line); len(match) == 2 {
		if value, err := strconv.ParseInt(match[1], 10, 32); err == nil {
			count := int32(value)
			supervisor.state.Runtime.VisibleAccelerators = &count
			supervisor.state.Runtime.AcceleratorDetected = count > 0
		}
	}
	if match := offloadPattern.FindStringSubmatch(line); len(match) == 3 {
		offloaded, offloadedError := strconv.ParseInt(match[1], 10, 32)
		total, totalError := strconv.ParseInt(match[2], 10, 32)
		if offloadedError == nil && totalError == nil && offloaded <= total {
			offloadedLayers := int32(offloaded)
			totalLayers := int32(total)
			supervisor.state.Runtime.OffloadedLayers = &offloadedLayers
			supervisor.state.Runtime.TotalLayers = &totalLayers
			if offloadedLayers > 0 {
				supervisor.state.Runtime.AcceleratorDetected = true
			}
		}
	}
	if match := devicePattern.FindStringSubmatch(line); len(match) == 2 {
		supervisor.state.Runtime.AcceleratorDevice = sanitizeDevice(match[1])
		if supervisor.state.Runtime.AcceleratorDevice != "" {
			supervisor.state.Runtime.AcceleratorDetected = true
		}
	}
	supervisor.touchLocked()
}

func sanitizeDevice(value string) string {
	value = sanitizeMessage(value)
	if len(value) > 96 {
		value = value[:96]
	}
	return value
}

func sanitizeLogLine(value string) string {
	lower := strings.ToLower(value)
	for _, sensitive := range []string{"authorization", "api-key", "api_key", `"messages"`, `"prompt"`, "bearer "} {
		if strings.Contains(lower, sensitive) {
			return "sensitive-output-redacted"
		}
	}
	if cudaCountPattern.MatchString(value) || devicePattern.MatchString(value) {
		return "accelerator-inventory-observed"
	}
	if offloadPattern.MatchString(value) {
		return "accelerator-layer-offload-observed"
	}
	if strings.Contains(lower, "error") || strings.Contains(lower, "failed") {
		return "error-detail-suppressed"
	}
	return "output-suppressed"
}

func sanitizeMessage(value string) string {
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' {
			return ' '
		}
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 512 {
		value = value[:512] + "...[truncated]"
	}
	return value
}
