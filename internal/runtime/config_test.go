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
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestDecodeConfigDefaultsAndRejectsUnknownFields(t *testing.T) {
	config := validConfig()
	config.DesiredConcurrency = 0
	config.DrainTimeoutSeconds = 0
	config.KVCache = KVCacheConfig{}
	config.Expert = ExpertConfig{}
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeConfig(strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.DesiredConcurrency != 1 || decoded.DrainTimeoutSeconds != 600 ||
		decoded.KVCache.KeyType != kvCacheF16 || decoded.KVCache.ValueType != kvCacheF16 ||
		decoded.Expert.BatchSize != 2048 || decoded.Expert.MicroBatchSize != 512 ||
		decoded.Expert.FlashAttention != FlashAttentionAuto {
		t.Fatalf("defaults were not applied: %#v", decoded)
	}

	unknown := strings.TrimSuffix(string(payload), "}") + `,"rawArgs":["--model-url","https://example.invalid"]}`
	if _, err := DecodeConfig(strings.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodeConfig() error = %v, want unknown-field error", err)
	}
}

func TestValidateConfigBoundaries(t *testing.T) {
	tests := map[string]func(*Config){
		"native context with concurrency": func(config *Config) { config.DesiredConcurrency = 2 },
		"context multiplication overflow": func(config *Config) {
			config.MaxContextTokens = math.MaxInt64
			config.DesiredConcurrency = 2
		},
		"invalid kv type":           func(config *Config) { config.KVCache.KeyType = "f32" },
		"micro batch exceeds batch": func(config *Config) { config.Expert.MicroBatchSize = config.Expert.BatchSize + 1 },
		"short drain":               func(config *Config) { config.DrainTimeoutSeconds = 29 },
		"entrypoint traversal": func(config *Config) {
			config.Artifact.Entrypoint = "../model.gguf"
			config.Artifact.Files[0].Path = "../model.gguf"
		},
		"entrypoint absent": func(config *Config) { config.Artifact.Entrypoint = "other.gguf" },
		"duplicate file":    func(config *Config) { config.Artifact.Files = append(config.Artifact.Files, config.Artifact.Files[0]) },
		"bad digest":        func(config *Config) { config.Artifact.Digest = "sha256:abcd" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := validConfig()
			mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func TestArgumentsOwnEveryLlamaServerControl(t *testing.T) {
	config := validConfig()
	config.Mode = ModeAccelerator
	config.MaxContextTokens = 8192
	config.DesiredConcurrency = 2
	config.KVCache = KVCacheConfig{KeyType: kvCacheQ8, ValueType: kvCacheQ4}
	config.Expert = ExpertConfig{
		BatchSize:      1024,
		MicroBatchSize: 256,
		Threads:        8,
		BatchThreads:   6,
		FlashAttention: FlashAttentionEnabled,
	}
	want := []string{
		"--model", "/models/model.gguf",
		"--mmap",
		"--alias", "smollm2",
		"--host", "0.0.0.0",
		"--port", "8080",
		"--ctx-size", "16384",
		"--parallel", "2",
		"--cache-type-k", kvCacheQ8,
		"--cache-type-v", kvCacheQ4,
		"--cache-ram", "0",
		"--no-cache-idle-slots",
		"--batch-size", "1024",
		"--ubatch-size", "256",
		"--flash-attn", "on",
		"--threads", "8",
		"--threads-batch", "6",
		"--metrics", "--slots", "--no-ui", "--no-mmproj", "--offline",
		"--log-verbosity", "3", "--log-colors", llamaOptionOff, "--no-log-prefix", "--no-log-timestamps",
		"--fit", llamaOptionOff,
		"--no-kv-unified", "--no-context-shift", "--warmup", "--cont-batching",
		"--split-mode", "none", "--n-gpu-layers", "all",
	}
	if arguments := config.Arguments(); !slices.Equal(arguments, want) {
		t.Fatalf("Arguments() = %#v, want %#v", arguments, want)
	}

	config = validConfig()
	arguments := config.Arguments()
	if !containsSequence(arguments, "--ctx-size", "0") ||
		!containsSequence(arguments, "--parallel", "1") ||
		!containsSequence(arguments, "--device", "none") ||
		!containsSequence(arguments, "--n-gpu-layers", "0") {
		t.Fatalf("CPU native-context arguments = %#v", arguments)
	}
}

func containsSequence(values []string, sequence ...string) bool {
	for index := 0; index+len(sequence) <= len(values); index++ {
		if slices.Equal(values[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func validConfig() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Deployment: DeploymentIdentity{
			Namespace:   "default",
			Name:        "smollm2",
			UID:         "deployment-uid",
			Fingerprint: strings.Repeat("f", 64),
		},
		Artifact: ArtifactIdentity{
			UID:        "artifact-uid",
			Digest:     strings.Repeat("a", 64),
			Entrypoint: "model.gguf",
			Files: []ArtifactFile{{
				Path:   "model.gguf",
				Size:   4,
				SHA256: "1cb1b7e0f8b96cee3445e317b8064d8805bf35c7dc7de82cddcb9f78d4c95e0e",
			}},
		},
		Mode:                ModeCPU,
		DesiredConcurrency:  1,
		DrainTimeoutSeconds: 30,
		KVCache:             KVCacheConfig{KeyType: kvCacheF16, ValueType: kvCacheF16},
		Expert: ExpertConfig{
			BatchSize:      2048,
			MicroBatchSize: 512,
			FlashAttention: FlashAttentionAuto,
		},
	}
}
