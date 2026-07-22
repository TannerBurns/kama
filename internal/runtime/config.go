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

// Package runtime implements the stable configuration and process supervisor
// contract shared by the ModelDeployment controller and serving images.
package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// SchemaVersion is the only runtime configuration schema understood by M2.
	SchemaVersion = "kama.runtime/v1alpha1"
	// ModelMountRoot is the controller-owned, read-only artifact mount.
	ModelMountRoot = "/models"

	defaultConcurrency        = int32(1)
	defaultDrainSeconds       = int64(600)
	defaultBatchSize          = int32(2048)
	defaultMicroBatchSize     = int32(512)
	maximumConfigBytes        = 1 << 20
	maximumArtifactFiles      = 128
	minimumDrainSeconds       = int64(30)
	maximumDrainSeconds       = int64(3600)
	maximumDesiredConcurrency = int32(128)
	kvCacheF16                = "f16"
	kvCacheQ8                 = "q8_0"
	kvCacheQ4                 = "q4_0"
	llamaOptionOff            = "off"
)

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Mode selects the single CPU or accelerator runtime owned by Kama.
type Mode string

const (
	// ModeCPU disables every accelerator backend in llama-server.
	ModeCPU Mode = "CPU"
	// ModeAccelerator gives llama-server one visible NVIDIA GPU.
	ModeAccelerator Mode = "Accelerator"
)

// FlashAttention controls llama-server's typed flash-attention option.
type FlashAttention string

const (
	FlashAttentionAuto     FlashAttention = "Auto"
	FlashAttentionEnabled  FlashAttention = "Enabled"
	FlashAttentionDisabled FlashAttention = "Disabled"
)

// DeploymentIdentity binds runtime observations to one generated workload.
type DeploymentIdentity struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	UID         string `json:"uid"`
	Fingerprint string `json:"fingerprint"`
}

// ArtifactFile is one immutable file expected below ModelMountRoot.
type ArtifactFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ArtifactIdentity is the verified ModelArtifact content mounted into a Pod.
type ArtifactIdentity struct {
	UID        string         `json:"uid"`
	Digest     string         `json:"digest"`
	Entrypoint string         `json:"entrypoint"`
	Files      []ArtifactFile `json:"files"`
}

// KVCacheConfig contains the bounded KV-cache tuning exposed in M2.
type KVCacheConfig struct {
	KeyType   string `json:"keyType"`
	ValueType string `json:"valueType"`
}

// ExpertConfig contains typed, safe llama-server tuning. Zero thread counts
// mean that llama.cpp chooses its own default.
type ExpertConfig struct {
	BatchSize      int32          `json:"batchSize"`
	MicroBatchSize int32          `json:"microBatchSize"`
	Threads        int32          `json:"threads,omitempty"`
	BatchThreads   int32          `json:"batchThreads,omitempty"`
	FlashAttention FlashAttention `json:"flashAttention"`
}

// Config is the complete controller-to-supervisor contract. It deliberately
// has no fields for executables, arbitrary arguments, environment, addresses,
// ports, paths outside the artifact entrypoint, or GPU topology.
type Config struct {
	SchemaVersion       string             `json:"schemaVersion"`
	Deployment          DeploymentIdentity `json:"deployment"`
	Artifact            ArtifactIdentity   `json:"artifact"`
	Mode                Mode               `json:"mode"`
	MaxContextTokens    int64              `json:"maxContextTokens,omitempty"`
	DesiredConcurrency  int32              `json:"desiredConcurrency"`
	DrainTimeoutSeconds int64              `json:"drainTimeoutSeconds"`
	KVCache             KVCacheConfig      `json:"kvCache"`
	Expert              ExpertConfig       `json:"expert"`
}

// Default fills the M2 runtime defaults without changing identity fields.
func (config *Config) Default() {
	if config.DesiredConcurrency == 0 {
		config.DesiredConcurrency = defaultConcurrency
	}
	if config.DrainTimeoutSeconds == 0 {
		config.DrainTimeoutSeconds = defaultDrainSeconds
	}
	if config.KVCache.KeyType == "" {
		config.KVCache.KeyType = kvCacheF16
	}
	if config.KVCache.ValueType == "" {
		config.KVCache.ValueType = kvCacheF16
	}
	if config.Expert.BatchSize == 0 {
		config.Expert.BatchSize = defaultBatchSize
	}
	if config.Expert.MicroBatchSize == 0 {
		config.Expert.MicroBatchSize = defaultMicroBatchSize
	}
	if config.Expert.FlashAttention == "" {
		config.Expert.FlashAttention = FlashAttentionAuto
	}
}

// DecodeConfig strictly decodes, defaults, and validates one bounded JSON
// document. Unknown fields are rejected so newer controllers cannot silently
// drive an older runtime with incomplete semantics.
func DecodeConfig(reader io.Reader) (Config, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maximumConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read runtime config: %w", err)
	}
	if len(payload) > maximumConfigBytes {
		return Config{}, fmt.Errorf("runtime config exceeds %d bytes", maximumConfigBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode runtime config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("runtime config must contain exactly one JSON object")
	}
	config.Default()
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate checks the complete runtime contract independently of CRD
// admission. The supervisor therefore fails closed if a malformed ConfigMap
// reaches a Pod.
func (config Config) Validate() error {
	if config.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", SchemaVersion)
	}
	if err := validateIdentity(config.Deployment); err != nil {
		return err
	}
	if err := validateArtifact(config.Artifact); err != nil {
		return err
	}
	if config.Mode != ModeCPU && config.Mode != ModeAccelerator {
		return errors.New("mode must be CPU or Accelerator")
	}
	if config.MaxContextTokens < 0 {
		return errors.New("maxContextTokens must not be negative")
	}
	if config.DesiredConcurrency < 1 || config.DesiredConcurrency > maximumDesiredConcurrency {
		return fmt.Errorf("desiredConcurrency must be between 1 and %d", maximumDesiredConcurrency)
	}
	if config.MaxContextTokens == 0 && config.DesiredConcurrency != 1 {
		return errors.New("model-native context requires desiredConcurrency to be 1")
	}
	if config.MaxContextTokens > 0 && config.MaxContextTokens > math.MaxInt64/int64(config.DesiredConcurrency) {
		return errors.New("maxContextTokens multiplied by desiredConcurrency overflows int64")
	}
	if config.DrainTimeoutSeconds < minimumDrainSeconds || config.DrainTimeoutSeconds > maximumDrainSeconds {
		return fmt.Errorf("drainTimeoutSeconds must be between %d and %d", minimumDrainSeconds, maximumDrainSeconds)
	}
	if !validKVType(config.KVCache.KeyType) || !validKVType(config.KVCache.ValueType) {
		return errors.New("kvCache keyType and valueType must be one of f16, q8_0, or q4_0")
	}
	if config.Expert.BatchSize < 1 || config.Expert.MicroBatchSize < 1 {
		return errors.New("expert batchSize and microBatchSize must be positive")
	}
	if config.Expert.MicroBatchSize > config.Expert.BatchSize {
		return errors.New("expert microBatchSize must not exceed batchSize")
	}
	if config.Expert.Threads < 0 || config.Expert.BatchThreads < 0 {
		return errors.New("expert thread counts must be positive when present")
	}
	switch config.Expert.FlashAttention {
	case FlashAttentionAuto, FlashAttentionEnabled, FlashAttentionDisabled:
	default:
		return errors.New("expert flashAttention must be Auto, Enabled, or Disabled")
	}
	return nil
}

func validateIdentity(identity DeploymentIdentity) error {
	if strings.TrimSpace(identity.Namespace) == "" || strings.TrimSpace(identity.Name) == "" ||
		strings.TrimSpace(identity.UID) == "" || strings.TrimSpace(identity.Fingerprint) == "" {
		return errors.New("deployment namespace, name, uid, and fingerprint are required")
	}
	if len(identity.Namespace) > 253 || len(identity.Name) > 253 || len(identity.UID) > 128 || len(identity.Fingerprint) > 128 {
		return errors.New("deployment identity field exceeds its size limit")
	}
	return nil
}

func validateArtifact(artifact ArtifactIdentity) error {
	if strings.TrimSpace(artifact.UID) == "" {
		return errors.New("artifact uid is required")
	}
	if !sha256Pattern.MatchString(artifact.Digest) {
		return errors.New("artifact digest must be a lowercase SHA-256")
	}
	if err := validateRelativePath(artifact.Entrypoint); err != nil {
		return fmt.Errorf("artifact entrypoint: %w", err)
	}
	if len(artifact.Files) < 1 || len(artifact.Files) > maximumArtifactFiles {
		return fmt.Errorf("artifact files must contain between 1 and %d entries", maximumArtifactFiles)
	}
	seen := make(map[string]struct{}, len(artifact.Files))
	foundEntrypoint := false
	for _, file := range artifact.Files {
		if err := validateRelativePath(file.Path); err != nil {
			return fmt.Errorf("artifact file %q: %w", file.Path, err)
		}
		if _, exists := seen[file.Path]; exists {
			return fmt.Errorf("artifact file path %q is duplicated", file.Path)
		}
		seen[file.Path] = struct{}{}
		if file.Path == artifact.Entrypoint {
			foundEntrypoint = true
		}
		if file.Size < 1 {
			return fmt.Errorf("artifact file %q size must be positive", file.Path)
		}
		if !sha256Pattern.MatchString(file.SHA256) {
			return fmt.Errorf("artifact file %q sha256 must be lowercase hexadecimal", file.Path)
		}
	}
	if !foundEntrypoint {
		return errors.New("artifact entrypoint is not present in files")
	}
	return nil
}

func validateRelativePath(value string) error {
	if value == "" || value == "." || path.IsAbs(value) || path.Clean(value) != value || strings.Contains(value, `\`) {
		return errors.New("must be a clean POSIX relative path")
	}
	if value == ".." || strings.HasPrefix(value, "../") {
		return errors.New("must remain below the model mount")
	}
	return nil
}

func validKVType(value string) bool {
	switch value {
	case kvCacheF16, kvCacheQ8, kvCacheQ4:
		return true
	default:
		return false
	}
}

// ModelPath returns the fixed absolute path to the configured entrypoint.
func (config Config) ModelPath() string {
	return filepath.Join(ModelMountRoot, filepath.FromSlash(config.Artifact.Entrypoint))
}

// Arguments translates the validated high-level runtime contract into the
// complete llama-server argv. Callers must Validate before using the result.
func (config Config) Arguments() []string {
	contextTokens := config.MaxContextTokens
	if contextTokens > 0 {
		contextTokens *= int64(config.DesiredConcurrency)
	}
	flashAttention := "auto"
	switch config.Expert.FlashAttention {
	case FlashAttentionEnabled:
		flashAttention = "on"
	case FlashAttentionDisabled:
		flashAttention = llamaOptionOff
	}
	logVerbosity := "3"
	if config.Mode == ModeAccelerator {
		// llama.cpp emits the trace-level device inventory used to prove that the
		// child sees exactly one CUDA device. Keep CPU serving at the quieter
		// default information level.
		logVerbosity = "4"
	}

	arguments := []string{
		"--model", config.ModelPath(),
		"--mmap",
		"--alias", config.Deployment.Name,
		"--host", "0.0.0.0",
		"--port", "8080",
		"--ctx-size", strconv.FormatInt(contextTokens, 10),
		"--parallel", strconv.FormatInt(int64(config.DesiredConcurrency), 10),
		"--cache-type-k", config.KVCache.KeyType,
		"--cache-type-v", config.KVCache.ValueType,
		"--cache-ram", "0",
		"--no-cache-idle-slots",
		"--batch-size", strconv.FormatInt(int64(config.Expert.BatchSize), 10),
		"--ubatch-size", strconv.FormatInt(int64(config.Expert.MicroBatchSize), 10),
		"--flash-attn", flashAttention,
	}
	if config.Expert.Threads > 0 {
		arguments = append(arguments, "--threads", strconv.FormatInt(int64(config.Expert.Threads), 10))
	}
	if config.Expert.BatchThreads > 0 {
		arguments = append(arguments, "--threads-batch", strconv.FormatInt(int64(config.Expert.BatchThreads), 10))
	}
	arguments = append(arguments,
		"--metrics",
		"--slots",
		"--no-ui",
		"--no-mmproj",
		"--offline",
		"--log-verbosity", logVerbosity,
		"--log-colors", llamaOptionOff,
		"--no-log-prefix",
		"--no-log-timestamps",
		"--fit", llamaOptionOff,
		"--no-kv-unified",
		"--no-context-shift",
		"--warmup",
		"--cont-batching",
	)
	if config.Mode == ModeCPU {
		arguments = append(arguments, "--device", "none", "--n-gpu-layers", "0")
	} else {
		arguments = append(arguments, "--split-mode", "none", "--n-gpu-layers", "all")
	}
	return arguments
}
