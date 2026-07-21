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
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestModelDeploymentDefault(t *testing.T) {
	t.Parallel()

	deployment := validModelDeployment()
	deployment.Spec.Placement.AcceleratorResource = ""
	deployment.Spec.Runtime = ModelDeploymentRuntimeSpec{}

	if err := (&ModelDeploymentDefaulter{}).Default(context.Background(), deployment); err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if deployment.Spec.Placement.AcceleratorResource != DefaultAcceleratorResource {
		t.Fatalf("acceleratorResource = %q, want %q",
			deployment.Spec.Placement.AcceleratorResource, DefaultAcceleratorResource)
	}
	if deployment.Spec.Runtime.DesiredConcurrency == nil ||
		*deployment.Spec.Runtime.DesiredConcurrency != DefaultModelDeploymentConcurrency {
		t.Fatalf("desiredConcurrency = %v, want %d",
			deployment.Spec.Runtime.DesiredConcurrency, DefaultModelDeploymentConcurrency)
	}
	if deployment.Spec.Runtime.DrainTimeout == nil ||
		deployment.Spec.Runtime.DrainTimeout.Duration != DefaultModelDeploymentDrainTimeout {
		t.Fatalf("drainTimeout = %v, want %s",
			deployment.Spec.Runtime.DrainTimeout, DefaultModelDeploymentDrainTimeout)
	}
	if deployment.Spec.Runtime.KVCache.KeyType != ModelDeploymentKVCacheF16 ||
		deployment.Spec.Runtime.KVCache.ValueType != ModelDeploymentKVCacheF16 {
		t.Fatalf("kvCache = %+v, want f16/f16", deployment.Spec.Runtime.KVCache)
	}
	if deployment.Spec.Runtime.Expert.BatchSize == nil ||
		*deployment.Spec.Runtime.Expert.BatchSize != DefaultModelDeploymentBatchSize {
		t.Fatalf("batchSize = %v, want %d",
			deployment.Spec.Runtime.Expert.BatchSize, DefaultModelDeploymentBatchSize)
	}
	if deployment.Spec.Runtime.Expert.MicroBatchSize == nil ||
		*deployment.Spec.Runtime.Expert.MicroBatchSize != DefaultModelDeploymentMicroBatchSize {
		t.Fatalf("microBatchSize = %v, want %d",
			deployment.Spec.Runtime.Expert.MicroBatchSize, DefaultModelDeploymentMicroBatchSize)
	}
	if deployment.Spec.Runtime.Expert.FlashAttention != ModelDeploymentFlashAttentionAuto {
		t.Fatalf("flashAttention = %q, want %q", deployment.Spec.Runtime.Expert.FlashAttention,
			ModelDeploymentFlashAttentionAuto)
	}

	// Defaulting must be idempotent and must not replace explicitly configured values.
	configured := validModelDeployment()
	want := configured.DeepCopy()
	if err := (&ModelDeploymentDefaulter{}).Default(context.Background(), configured); err != nil {
		t.Fatalf("second Default() error = %v", err)
	}
	if configured.Spec.Runtime.DesiredConcurrency == nil || want.Spec.Runtime.DesiredConcurrency == nil ||
		*configured.Spec.Runtime.DesiredConcurrency != *want.Spec.Runtime.DesiredConcurrency {
		t.Fatalf("configured desiredConcurrency changed: got %v want %v",
			configured.Spec.Runtime.DesiredConcurrency, want.Spec.Runtime.DesiredConcurrency)
	}
}

func TestModelDeploymentValidationAcceptsSupportedContracts(t *testing.T) {
	t.Parallel()

	cpu := validModelDeployment()
	cpu.Spec.Placement = ModelDeploymentPlacementSpec{Mode: ModelDeploymentPlacementCPU}
	cpu.Spec.Runtime.MaxContextTokens = nil
	one := int32(1)
	cpu.Spec.Runtime.DesiredConcurrency = &one
	cpu.Spec.Resources.Limits = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}

	acceleratorDefaults := validModelDeployment()
	acceleratorDefaults.Spec.Placement.AcceleratorResource = ""
	acceleratorDefaults.Spec.Runtime = ModelDeploymentRuntimeSpec{}

	tests := map[string]*ModelDeployment{
		"explicit accelerator":                           validModelDeployment(),
		"accelerator with effective defaults":            acceleratorDefaults,
		"CPU without CPU limit and model-native context": cpu,
	}
	validator := &ModelDeploymentValidator{}
	for name, deployment := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := validator.ValidateCreate(context.Background(), deployment); err != nil {
				t.Fatalf("ValidateCreate() error = %v", err)
			}
		})
	}
}

func TestModelDeploymentValidationRejectsInvalidContracts(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*ModelDeployment){
		"missing model reference": func(deployment *ModelDeployment) {
			deployment.Spec.ModelRef.Name = ""
		},
		"cross namespace model reference syntax": func(deployment *ModelDeployment) {
			deployment.Spec.ModelRef.Name = "other/model"
		},
		"unknown placement mode": func(deployment *ModelDeployment) {
			deployment.Spec.Placement.Mode = "Auto"
		},
		"CPU accelerator resource": func(deployment *ModelDeployment) {
			deployment.Spec.Placement.Mode = ModelDeploymentPlacementCPU
		},
		"unsupported accelerator resource": func(deployment *ModelDeployment) {
			deployment.Spec.Placement.AcceleratorResource = "amd.com/gpu"
		},
		"native context concurrency": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.MaxContextTokens = nil
		},
		"zero context": func(deployment *ModelDeployment) {
			value := int64(0)
			deployment.Spec.Runtime.MaxContextTokens = &value
		},
		"zero concurrency": func(deployment *ModelDeployment) {
			value := int32(0)
			deployment.Spec.Runtime.DesiredConcurrency = &value
		},
		"excessive concurrency": func(deployment *ModelDeployment) {
			value := MaximumModelDeploymentConcurrency + 1
			deployment.Spec.Runtime.DesiredConcurrency = &value
		},
		"context multiplication overflow": func(deployment *ModelDeployment) {
			value := int64(math.MaxInt64)
			deployment.Spec.Runtime.MaxContextTokens = &value
		},
		"short drain": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.DrainTimeout = durationPointer(29 * time.Second)
		},
		"long drain": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.DrainTimeout = durationPointer(time.Hour + time.Second)
		},
		"unsupported key cache type": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.KVCache.KeyType = "q2_K"
		},
		"unsupported value cache type": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.KVCache.ValueType = "bf16"
		},
		"zero batch size": func(deployment *ModelDeployment) {
			value := int32(0)
			deployment.Spec.Runtime.Expert.BatchSize = &value
		},
		"micro batch exceeds batch": func(deployment *ModelDeployment) {
			value := int32(4096)
			deployment.Spec.Runtime.Expert.MicroBatchSize = &value
		},
		"zero threads": func(deployment *ModelDeployment) {
			value := int32(0)
			deployment.Spec.Runtime.Expert.Threads = &value
		},
		"unknown flash attention": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.Expert.FlashAttention = "Required"
		},
		"missing CPU request": func(deployment *ModelDeployment) {
			delete(deployment.Spec.Resources.Requests, corev1.ResourceCPU)
		},
		"missing memory request": func(deployment *ModelDeployment) {
			delete(deployment.Spec.Resources.Requests, corev1.ResourceMemory)
		},
		"missing memory limit": func(deployment *ModelDeployment) {
			delete(deployment.Spec.Resources.Limits, corev1.ResourceMemory)
		},
		"zero CPU request": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("0")
		},
		"memory limit below request": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
		},
		"CPU limit below request": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("100m")
		},
		"ephemeral limit below request": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("2Gi")
			deployment.Spec.Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
		},
		"user accelerator request": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Requests[DefaultAcceleratorResource] = resource.MustParse("1")
		},
		"user accelerator limit": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Limits[DefaultAcceleratorResource] = resource.MustParse("1")
		},
		"hugepages": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Requests[corev1.ResourceName("hugepages-2Mi")] = resource.MustParse("2Mi")
		},
		"unsupported storage resource": func(deployment *ModelDeployment) {
			deployment.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("1Gi")
		},
		"raw runtime arguments": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.Args = &ForbiddenModelDeploymentField{}
		},
		"runtime environment": func(deployment *ModelDeployment) {
			deployment.Spec.Runtime.Env = &ForbiddenModelDeploymentField{}
		},
		"runtime image": func(deployment *ModelDeployment) {
			deployment.Spec.Image = &ForbiddenModelDeploymentField{}
		},
		"runtime ports": func(deployment *ModelDeployment) {
			deployment.Spec.Ports = &ForbiddenModelDeploymentField{}
		},
		"runtime paths": func(deployment *ModelDeployment) {
			deployment.Spec.Paths = &ForbiddenModelDeploymentField{}
		},
		"runtime probes": func(deployment *ModelDeployment) {
			deployment.Spec.Probes = &ForbiddenModelDeploymentField{}
		},
		"runtime topology": func(deployment *ModelDeployment) {
			deployment.Spec.Topology = &ForbiddenModelDeploymentField{}
		},
		"runtime replicas": func(deployment *ModelDeployment) {
			deployment.Spec.Replicas = &ForbiddenModelDeploymentField{}
		},
	}

	validator := &ModelDeploymentValidator{}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deployment := validModelDeployment()
			mutate(deployment)
			_, err := validator.ValidateCreate(context.Background(), deployment)
			if err == nil {
				t.Fatal("ValidateCreate() error = nil, want invalid object")
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("ValidateCreate() error = %T %v, want Invalid", err, err)
			}
		})
	}
}

func TestModelDeploymentSpecRemainsMutable(t *testing.T) {
	t.Parallel()

	oldDeployment := validModelDeployment()
	updated := oldDeployment.DeepCopy()
	contextTokens := int64(16384)
	updated.Spec.Runtime.MaxContextTokens = &contextTokens
	updated.Spec.Placement = ModelDeploymentPlacementSpec{Mode: ModelDeploymentPlacementCPU}
	updated.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("12Gi")

	if _, err := (&ModelDeploymentValidator{}).ValidateUpdate(
		context.Background(), oldDeployment, updated,
	); err != nil {
		t.Fatalf("ValidateUpdate() rejected valid mutable serving spec: %v", err)
	}
}

func validModelDeployment() *ModelDeployment {
	contextTokens := int64(8192)
	concurrency := int32(2)
	batchSize := int32(2048)
	microBatchSize := int32(512)
	threads := int32(8)
	batchThreads := int32(8)
	return &ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "smollm2-serving"},
		Spec: ModelDeploymentSpec{
			ModelRef: corev1.LocalObjectReference{Name: "smollm2"},
			Placement: ModelDeploymentPlacementSpec{
				Mode:                ModelDeploymentPlacementAccelerator,
				AcceleratorResource: DefaultAcceleratorResource,
			},
			Runtime: ModelDeploymentRuntimeSpec{
				MaxContextTokens:   &contextTokens,
				DesiredConcurrency: &concurrency,
				DrainTimeout:       durationPointer(DefaultModelDeploymentDrainTimeout),
				KVCache: ModelDeploymentKVCacheSpec{
					KeyType:   ModelDeploymentKVCacheQ8,
					ValueType: ModelDeploymentKVCacheQ4,
				},
				Expert: ModelDeploymentExpertSpec{
					BatchSize:      &batchSize,
					MicroBatchSize: &microBatchSize,
					Threads:        &threads,
					BatchThreads:   &batchThreads,
					FlashAttention: ModelDeploymentFlashAttentionEnabled,
				},
			},
			Resources: ModelDeploymentResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("2"),
					corev1.ResourceMemory:           resource.MustParse("4Gi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("4"),
					corev1.ResourceMemory:           resource.MustParse("8Gi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}
}

func durationPointer(value time.Duration) *metav1.Duration {
	return &metav1.Duration{Duration: value}
}
