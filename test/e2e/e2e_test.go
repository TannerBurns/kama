//go:build e2e

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

package e2e

import "testing"

// TestKubebuilderScaffoldMarkers preserves Kubebuilder's extension points while
// M0's executable cluster acceptance suite lives in hack/test-kind.sh.
func TestKubebuilderScaffoldMarkers(t *testing.T) {
	t.Skip("M0 cluster acceptance runs through make test-kind")

	// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

	// +kubebuilder:scaffold:e2e-webhooks-checks
}
