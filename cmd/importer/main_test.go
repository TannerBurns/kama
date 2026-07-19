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
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TannerBurns/kama/internal/artifact"
	fixture "github.com/TannerBurns/kama/internal/testfixtures/gguf"
)

const commandModelFile = "model.gguf"

func TestRunDirectEmitsFullAndCompactResults(t *testing.T) {
	source := t.TempDir()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, commandModelFile), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := artifact.Spec{
		SchemaVersion: artifact.SchemaVersion,
		Mode:          artifact.ModeDirect,
		Format:        artifact.FormatGGUF,
		Entrypoint:    commandModelFile,
		PVC:           &artifact.PVCSpec{MountRoot: source, SelectedFiles: []string{commandModelFile}},
	}
	specPayload, _ := json.Marshal(spec)
	specFile := filepath.Join(t.TempDir(), "spec.json")
	resultFile := filepath.Join(t.TempDir(), "termination.json")
	if err := os.WriteFile(specFile, specPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exitCode := run(
		context.Background(),
		[]string{"--spec-file=" + specFile, "--result-file=" + resultFile},
		&stdout,
		&stderr,
	)
	if exitCode != 0 {
		t.Fatalf("run() exit = %d, stderr = %q, stdout = %q", exitCode, stderr.String(), stdout.String())
	}
	result, err := artifact.ParseResult(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Mode != artifact.ModeDirect || result.Manifest == nil {
		t.Fatalf("result = %#v", result)
	}
	summary, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) == 0 || len(summary) >= artifact.MaxSummaryBytes || !json.Valid(summary) {
		t.Fatalf("termination summary is invalid: %q", summary)
	}
}

func TestRunMissingSpecIsStructuredFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	resultFile := filepath.Join(t.TempDir(), "termination.json")
	exitCode := run(
		context.Background(),
		[]string{
			"--spec-file=" + filepath.Join(t.TempDir(), "missing.json"),
			"--result-file=" + resultFile,
		},
		&stdout,
		&stderr,
	)
	if exitCode != 1 {
		t.Fatalf("run() exit = %d, want 1", exitCode)
	}
	result, err := artifact.ParseResult(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.Reason != artifact.ReasonInvalidSpec {
		t.Fatalf("result = %#v", result)
	}
}
