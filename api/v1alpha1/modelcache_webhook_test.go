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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultModelCacheName = "default"

func TestModelCacheDefault(t *testing.T) {
	t.Parallel()

	cache := validManagedCache()
	cache.Spec.RetentionPolicy = ""
	cache.Spec.Storage.ClaimTemplate.Spec.VolumeMode = ""

	if err := (&ModelCacheDefaulter{}).Default(context.Background(), cache); err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if cache.Spec.RetentionPolicy != RetentionPolicyRetain {
		t.Fatalf("retentionPolicy = %q, want %q", cache.Spec.RetentionPolicy, RetentionPolicyRetain)
	}
	if cache.Spec.Storage.ClaimTemplate.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		t.Fatalf("volumeMode = %q, want %q", cache.Spec.Storage.ClaimTemplate.Spec.VolumeMode,
			corev1.PersistentVolumeFilesystem)
	}
}

func TestModelCacheValidationAcceptsSupportedStorage(t *testing.T) {
	t.Parallel()

	tests := map[string]*ModelCache{
		"managed RWX": validManagedCache(),
		"adopted": {
			ObjectMeta: metav1.ObjectMeta{Name: "adopted"},
			Spec: ModelCacheSpec{
				Storage: ModelCacheStorageSpec{
					ExistingClaim: &corev1.LocalObjectReference{Name: "existing-cache"},
				},
				RetentionPolicy: RetentionPolicyRetain,
			},
		},
	}

	validator := &ModelCacheValidator{}
	for name, cache := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := validator.ValidateCreate(context.Background(), cache); err != nil {
				t.Fatalf("ValidateCreate() error = %v", err)
			}
		})
	}
}

func TestModelCacheValidationRejectsInvalidContracts(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*ModelCache){
		"neither storage mode": func(cache *ModelCache) {
			cache.Spec.Storage = ModelCacheStorageSpec{}
		},
		"both storage modes": func(cache *ModelCache) {
			cache.Spec.Storage.ExistingClaim = &corev1.LocalObjectReference{Name: "existing"}
		},
		"adopted delete": func(cache *ModelCache) {
			cache.Spec.Storage = ModelCacheStorageSpec{
				ExistingClaim: &corev1.LocalObjectReference{Name: "existing"},
			}
			cache.Spec.RetentionPolicy = RetentionPolicyDelete
		},
		"cross namespace claim syntax": func(cache *ModelCache) {
			cache.Spec.Storage = ModelCacheStorageSpec{
				ExistingClaim: &corev1.LocalObjectReference{Name: "other/cache"},
			}
		},
		"read only managed cache": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}
		},
		"block volume": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Spec.VolumeMode = corev1.PersistentVolumeBlock
		},
		"missing storage request": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Spec.Resources.Requests = nil
		},
		"zero storage request": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("0")
		},
		"non-storage request": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
		},
		"invalid metadata": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Metadata.Labels = map[string]string{"bad key": "value"}
		},
		"reserved deletion guard annotation": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Metadata.Annotations[modelCacheDeletionGuardAnnotation] = "forged"
		},
		"reserved deletion guard timestamp annotation": func(cache *ModelCache) {
			cache.Spec.Storage.ClaimTemplate.Metadata.Annotations[modelCacheDeletionGuardedAtAnnotation] = time.Now().Format(time.RFC3339)
		},
		"unknown retention": func(cache *ModelCache) {
			cache.Spec.RetentionPolicy = "Archive"
		},
	}

	validator := &ModelCacheValidator{}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cache := validManagedCache()
			mutate(cache)
			_, err := validator.ValidateCreate(context.Background(), cache)
			if err == nil {
				t.Fatal("ValidateCreate() error = nil, want invalid object")
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("ValidateCreate() error = %T %v, want Invalid", err, err)
			}
		})
	}
}

func TestModelCacheRetentionPolicyIsImmutableAfterDeletionBegins(t *testing.T) {
	t.Parallel()

	oldCache := validManagedCache()
	deletionTime := metav1.Now()
	oldCache.DeletionTimestamp = &deletionTime
	updated := oldCache.DeepCopy()
	updated.Spec.RetentionPolicy = RetentionPolicyDelete

	validator := &ModelCacheValidator{}
	if _, err := validator.ValidateUpdate(context.Background(), oldCache, updated); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("ValidateUpdate() error = %v, want Invalid retention mutation", err)
	}

	beforeDeletion := oldCache.DeepCopy()
	beforeDeletion.DeletionTimestamp = nil
	if _, err := validator.ValidateUpdate(context.Background(), beforeDeletion, updated); err != nil {
		t.Fatalf("ValidateUpdate() rejected retention mutation before deletion: %v", err)
	}

	defaulted := oldCache.DeepCopy()
	defaulted.Spec.RetentionPolicy = ""
	if _, err := validator.ValidateUpdate(context.Background(), defaulted, oldCache); err != nil {
		t.Fatalf("ValidateUpdate() rejected equivalent default retention policy: %v", err)
	}
}

func validManagedCache() *ModelCache {
	return &ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: defaultModelCacheName},
		Spec: ModelCacheSpec{
			Storage: ModelCacheStorageSpec{
				ClaimTemplate: &ModelCacheClaimTemplate{
					Metadata: ModelCacheClaimTemplateMetadata{
						Labels:      map[string]string{"storage.example.com/tier": "shared"},
						Annotations: map[string]string{"storage.example.com/owner": "platform"},
					},
					Spec: ModelCacheClaimTemplateSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
						VolumeMode:  corev1.PersistentVolumeFilesystem,
						Resources: ModelCacheResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")},
						},
					},
				},
			},
			RetentionPolicy: RetentionPolicyRetain,
		},
	}
}
