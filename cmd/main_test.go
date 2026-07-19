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
