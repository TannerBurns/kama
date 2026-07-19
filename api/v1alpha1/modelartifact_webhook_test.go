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

package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultCacheName = "default"

func TestModelArtifactDefault(t *testing.T) {
	t.Parallel()

	artifact := validCopyArtifact()
	artifact.Spec.Source.PersistentVolumeClaim.ImportPolicy = ""
	artifact.Spec.Verification.ExpectedSHA256 = strings.Repeat("AB", 32)

	if err := (&ModelArtifactDefaulter{}).Default(context.Background(), artifact); err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if artifact.Spec.Source.PersistentVolumeClaim.ImportPolicy != PVCImportPolicyCopy {
		t.Fatalf("importPolicy = %q, want %q", artifact.Spec.Source.PersistentVolumeClaim.ImportPolicy,
			PVCImportPolicyCopy)
	}
	if artifact.Spec.Verification.ExpectedSHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("expectedSHA256 was not canonicalized: %q", artifact.Spec.Verification.ExpectedSHA256)
	}
}

func TestModelArtifactValidationAcceptsSupportedSources(t *testing.T) {
	t.Parallel()

	tests := map[string]*ModelArtifact{
		"hugging face":              validHuggingFaceArtifact(),
		"unnamespaced hugging face": validHuggingFaceArtifact(),
		"PVC copy":                  validCopyArtifact(),
		"PVC direct":                validDirectArtifact(),
	}
	tests["unnamespaced hugging face"].Spec.Source.HuggingFace.Repository = "gpt2"

	validator := &ModelArtifactValidator{}
	for name, artifact := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := validator.ValidateCreate(context.Background(), artifact); err != nil {
				t.Fatalf("ValidateCreate() error = %v", err)
			}
		})
	}
}

func TestModelArtifactValidationRejectsInvalidContracts(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*ModelArtifact){
		"unsupported format": func(artifact *ModelArtifact) {
			artifact.Spec.Format = "SafeTensors"
		},
		"absolute entrypoint": func(artifact *ModelArtifact) {
			artifact.Spec.Entrypoint = "/model.gguf"
		},
		"traversing entrypoint": func(artifact *ModelArtifact) {
			artifact.Spec.Entrypoint = "../model.gguf"
		},
		"wildcard entrypoint": func(artifact *ModelArtifact) {
			artifact.Spec.Entrypoint = "*.gguf"
		},
		"non-GGUF entrypoint": func(artifact *ModelArtifact) {
			artifact.Spec.Entrypoint = "model.bin"
		},
		"neither source": func(artifact *ModelArtifact) {
			artifact.Spec.Source = ModelArtifactSource{}
		},
		"both sources": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace = validHuggingFaceArtifact().Spec.Source.HuggingFace
		},
		"copy without cache": func(artifact *ModelArtifact) {
			artifact.Spec.CacheRef = nil
		},
		"direct with cache": func(artifact *ModelArtifact) {
			artifact.Spec.Source.PersistentVolumeClaim.ImportPolicy = PVCImportPolicyDirect
		},
		"cross namespace cache syntax": func(artifact *ModelArtifact) {
			artifact.Spec.CacheRef.Name = "other/cache"
		},
		"cross namespace claim syntax": func(artifact *ModelArtifact) {
			artifact.Spec.Source.PersistentVolumeClaim.ClaimName = "other/source"
		},
		"traversing root": func(artifact *ModelArtifact) {
			artifact.Spec.Source.PersistentVolumeClaim.RootPath = "models/../private"
		},
		"unknown import policy": func(artifact *ModelArtifact) {
			artifact.Spec.Source.PersistentVolumeClaim.ImportPolicy = "Move"
		},
		"bad digest": func(artifact *ModelArtifact) {
			artifact.Spec.Verification.ExpectedSHA256 = "secret-token-value"
		},
		"zero expected size": func(artifact *ModelArtifact) {
			zero := int64(0)
			artifact.Spec.Verification.ExpectedSize = &zero
		},
	}

	validator := &ModelArtifactValidator{}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			artifact := validCopyArtifact()
			mutate(artifact)
			_, err := validator.ValidateCreate(context.Background(), artifact)
			if err == nil {
				t.Fatal("ValidateCreate() error = nil, want invalid object")
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("ValidateCreate() error = %T %v, want Invalid", err, err)
			}
		})
	}
}

func TestHuggingFaceValidationRejectsUnsafeOrUnboundedSelectors(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*ModelArtifact){
		"missing revision": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.Revision = ""
		},
		"repository URL": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.Repository = "https://huggingface.co/owner/model"
		},
		"repository with too many components": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.Repository = "models/owner/model"
		},
		"invalid selector": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.Files = []string{"../*.gguf"}
		},
		"malformed selector": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.Files = []string{"model-[.gguf"}
		},
		"duplicate selector": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.Files = []string{"model.gguf", "model.gguf"}
		},
		"too many selectors": func(artifact *ModelArtifact) {
			files := make([]string, 129)
			for index := range files {
				files[index] = fmt.Sprintf("model-%03d.gguf", index)
			}
			artifact.Spec.Source.HuggingFace.Files = files
		},
		"cross namespace token syntax": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.TokenSecretRef.Name = "other/token"
		},
		"invalid token key": func(artifact *ModelArtifact) {
			artifact.Spec.Source.HuggingFace.TokenSecretRef.Key = "bad key"
		},
	}

	validator := &ModelArtifactValidator{}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			artifact := validHuggingFaceArtifact()
			mutate(artifact)
			if _, err := validator.ValidateCreate(context.Background(), artifact); err == nil {
				t.Fatal("ValidateCreate() error = nil, want invalid object")
			}
		})
	}
}

func TestModelArtifactSpecBecomesImmutableWhenReconciliationStarts(t *testing.T) {
	t.Parallel()

	validator := &ModelArtifactValidator{}
	oldArtifact := validHuggingFaceArtifact()
	oldArtifact.Finalizers = []string{ModelArtifactFinalizer}
	oldArtifact.Status.Conditions = []metav1.Condition{{
		Type:   ModelArtifactConditionReady,
		Status: metav1.ConditionTrue,
		Reason: "Verified",
	}}

	unchanged := oldArtifact.DeepCopy()
	unchanged.Labels = map[string]string{"team": "inference"}
	if _, err := validator.ValidateUpdate(context.Background(), oldArtifact, unchanged); err != nil {
		t.Fatalf("metadata-only update error = %v", err)
	}

	changed := oldArtifact.DeepCopy()
	changed.Spec.Source.HuggingFace.Revision = "new-revision"
	if _, err := validator.ValidateUpdate(context.Background(), oldArtifact, changed); err == nil {
		t.Fatal("ready content update error = nil, want immutable spec rejection")
	}

	reconciling := oldArtifact.DeepCopy()
	reconciling.Status.Conditions[0].Status = metav1.ConditionFalse
	reconciling.Status.ValidatedAt = nil
	reconciling.Status.ArtifactDigest = ""
	changedWhileReconciling := reconciling.DeepCopy()
	changedWhileReconciling.Spec.CacheRef.Name = "different-cache"
	if _, err := validator.ValidateUpdate(context.Background(), reconciling, changedWhileReconciling); err == nil {
		t.Fatal("cacheRef changed after reconciliation started")
	}

	notStarted := reconciling.DeepCopy()
	notStarted.Finalizers = nil
	changedBeforeStart := notStarted.DeepCopy()
	changedBeforeStart.Spec.Source.HuggingFace.Revision = "new-revision"
	if _, err := validator.ValidateUpdate(context.Background(), notStarted, changedBeforeStart); err != nil {
		t.Fatalf("pre-reconciliation spec update error = %v", err)
	}

	previouslyReady := oldArtifact.DeepCopy()
	previouslyReady.Status.Conditions[0].Status = metav1.ConditionFalse
	previouslyReady.Status.ValidatedAt = &metav1.Time{Time: metav1.Now().Time}
	changedAfterStorageLoss := previouslyReady.DeepCopy()
	changedAfterStorageLoss.Spec.Entrypoint = "replacement.gguf"
	if _, err := validator.ValidateUpdate(context.Background(), previouslyReady, changedAfterStorageLoss); err == nil {
		t.Fatal("previously-ready content became mutable after Ready changed to False")
	}
}

func validHuggingFaceArtifact() *ModelArtifact {
	return &ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-model"},
		Spec: ModelArtifactSpec{
			Format:     ArtifactFormatGGUF,
			Entrypoint: "model-00001-of-00002.gguf",
			Source: ModelArtifactSource{
				HuggingFace: &HuggingFaceSource{
					Repository: "owner/model",
					Revision:   "0123456789abcdef0123456789abcdef01234567",
					Files:      []string{"model-*-of-00002.gguf"},
					TokenSecretRef: &SecretKeyReference{
						Name: "hugging-face-token",
						Key:  "token",
					},
				},
			},
			CacheRef: &corev1.LocalObjectReference{Name: defaultCacheName},
		},
	}
}

func validCopyArtifact() *ModelArtifact {
	return &ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: "copied-model"},
		Spec: ModelArtifactSpec{
			Format:     ArtifactFormatGGUF,
			Entrypoint: "llama/model.gguf",
			Source: ModelArtifactSource{
				PersistentVolumeClaim: &PersistentVolumeClaimSource{
					ClaimName:    "manual-models",
					RootPath:     "models",
					ImportPolicy: PVCImportPolicyCopy,
				},
			},
			CacheRef: &corev1.LocalObjectReference{Name: defaultCacheName},
		},
	}
}

func validDirectArtifact() *ModelArtifact {
	artifact := validCopyArtifact()
	artifact.Name = "direct-model"
	artifact.Spec.Source.PersistentVolumeClaim.RootPath = "."
	artifact.Spec.Source.PersistentVolumeClaim.ImportPolicy = PVCImportPolicyDirect
	artifact.Spec.CacheRef = nil
	return artifact
}
