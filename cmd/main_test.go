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

package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildImporterOptions(t *testing.T) {
	t.Parallel()
	options, err := buildImporterOptions(
		"registry.example/kama-importer:v1", string(corev1.PullNever), " first,second,first ",
		"https://hub.example/",
	)
	if err != nil {
		t.Fatalf("buildImporterOptions(): %v", err)
	}
	if options.Image != "registry.example/kama-importer:v1" || options.PullPolicy != corev1.PullNever {
		t.Fatalf("image options = %+v", options)
	}
	if options.HubEndpoint != "https://hub.example" {
		t.Fatalf("Hub endpoint = %q", options.HubEndpoint)
	}
	if len(options.ImagePullSecrets) != 3 || options.ImagePullSecrets[1].Name != "second" {
		t.Fatalf("pull secrets = %+v", options.ImagePullSecrets)
	}
}

func TestBuildImporterOptionsRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := buildImporterOptions("image", "Sometimes", "", "https://huggingface.co"); err == nil {
		t.Fatal("invalid pull policy was accepted")
	}
	if _, err := buildImporterOptions("image", string(corev1.PullIfNotPresent), "", "file:///models"); err == nil {
		t.Fatal("non-HTTP Hub endpoint was accepted")
	}
}

func TestBuildRuntimeOptions(t *testing.T) {
	t.Parallel()
	options, err := buildRuntimeOptions(
		"registry.example/kama-runtime-cpu:v1", "registry.example/kama-runtime-cuda:v1",
		"nvidia.example", string(corev1.PullNever), " first,second ",
		"b4d6c7d8ff69c2e05e4e8ee7e6e710a08abd7b45",
	)
	if err != nil {
		t.Fatalf("buildRuntimeOptions(): %v", err)
	}
	if options.CPUImage != "registry.example/kama-runtime-cpu:v1" ||
		options.CUDAImage != "registry.example/kama-runtime-cuda:v1" ||
		options.CUDARuntimeClassName != "nvidia.example" || options.PullPolicy != corev1.PullNever {
		t.Fatalf("runtime options = %+v", options)
	}
	if len(options.ImagePullSecrets) != 2 || options.ImagePullSecrets[1].Name != "second" {
		t.Fatalf("runtime pull secrets = %+v", options.ImagePullSecrets)
	}
}

func TestBuildRuntimeOptionsRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := buildRuntimeOptions("cpu", "cuda", "", "Sometimes", "", strings.Repeat("a", 40)); err == nil {
		t.Fatal("invalid runtime pull policy was accepted")
	}
	if _, err := buildRuntimeOptions("cpu", "cuda", "", string(corev1.PullIfNotPresent), "", "short"); err == nil {
		t.Fatal("short llama.cpp commit was accepted")
	}
	if _, err := buildRuntimeOptions(
		"cpu", "cuda", "", string(corev1.PullIfNotPresent), "NOT_A_SECRET", strings.Repeat("a", 40),
	); err == nil {
		t.Fatal("invalid runtime image pull Secret name was accepted")
	}
	if _, err := buildRuntimeOptions(
		"cpu", "cuda", "Not_A_RuntimeClass", string(corev1.PullIfNotPresent), "", strings.Repeat("a", 40),
	); err == nil {
		t.Fatal("invalid CUDA RuntimeClass name was accepted")
	}
	if _, err := buildRuntimeOptions(
		"cpu", "cuda", strings.Repeat("a", 64), string(corev1.PullIfNotPresent), "", strings.Repeat("a", 40),
	); err == nil {
		t.Fatal("CUDA RuntimeClass name with an overlong DNS-1123 label was accepted")
	}
}
