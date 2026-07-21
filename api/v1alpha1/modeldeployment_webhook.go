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
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	_ admission.Defaulter[*ModelDeployment] = &ModelDeploymentDefaulter{}
	_ admission.Validator[*ModelDeployment] = &ModelDeploymentValidator{}
)

// SetupModelDeploymentWebhookWithManager registers ModelDeployment defaulting and validation webhooks.
func SetupModelDeploymentWebhookWithManager(mgr manager.Manager) error {
	return builder.WebhookManagedBy(mgr, &ModelDeployment{}).
		WithDefaulter(&ModelDeploymentDefaulter{}).
		WithValidator(&ModelDeploymentValidator{}).
		Complete()
}

// ModelDeploymentDefaulter applies deterministic serving defaults without consulting cluster state.
// +kubebuilder:object:generate=false
type ModelDeploymentDefaulter struct{}

// Default selects the M2 runtime's safe baseline behavior.
func (*ModelDeploymentDefaulter) Default(_ context.Context, deployment *ModelDeployment) error {
	if deployment.Spec.Placement.Mode == ModelDeploymentPlacementAccelerator &&
		deployment.Spec.Placement.AcceleratorResource == "" {
		deployment.Spec.Placement.AcceleratorResource = DefaultAcceleratorResource
	}

	runtime := &deployment.Spec.Runtime
	if runtime.DesiredConcurrency == nil {
		value := DefaultModelDeploymentConcurrency
		runtime.DesiredConcurrency = &value
	}
	if runtime.DrainTimeout == nil {
		runtime.DrainTimeout = &metav1.Duration{Duration: DefaultModelDeploymentDrainTimeout}
	}
	if runtime.KVCache.KeyType == "" {
		runtime.KVCache.KeyType = ModelDeploymentKVCacheF16
	}
	if runtime.KVCache.ValueType == "" {
		runtime.KVCache.ValueType = ModelDeploymentKVCacheF16
	}
	if runtime.Expert.BatchSize == nil {
		value := DefaultModelDeploymentBatchSize
		runtime.Expert.BatchSize = &value
	}
	if runtime.Expert.MicroBatchSize == nil {
		value := DefaultModelDeploymentMicroBatchSize
		runtime.Expert.MicroBatchSize = &value
	}
	if runtime.Expert.FlashAttention == "" {
		runtime.Expert.FlashAttention = ModelDeploymentFlashAttentionAuto
	}
	return nil
}

// ModelDeploymentValidator enforces the static single-replica M2 serving contract.
// +kubebuilder:object:generate=false
type ModelDeploymentValidator struct{}

// ValidateCreate validates a new ModelDeployment.
func (*ModelDeploymentValidator) ValidateCreate(
	_ context.Context, deployment *ModelDeployment,
) (admission.Warnings, error) {
	return nil, validateModelDeployment(deployment)
}

// ValidateUpdate validates the current mutable ModelDeployment spec.
func (*ModelDeploymentValidator) ValidateUpdate(
	_ context.Context, _, deployment *ModelDeployment,
) (admission.Warnings, error) {
	return nil, validateModelDeployment(deployment)
}

// ValidateDelete permits admission; draining and reference release are reconciler responsibilities.
func (*ModelDeploymentValidator) ValidateDelete(
	_ context.Context, _ *ModelDeployment,
) (admission.Warnings, error) {
	return nil, nil
}

func validateModelDeployment(deployment *ModelDeployment) error {
	specPath := field.NewPath("spec")
	var allErrs field.ErrorList

	if err := validateLocalObjectName(deployment.Spec.ModelRef.Name, specPath.Child("modelRef", "name")); err != nil {
		allErrs = append(allErrs, err)
	}

	allErrs = append(allErrs, validateModelDeploymentPlacement(
		deployment.Spec.Placement, specPath.Child("placement"))...)
	allErrs = append(allErrs, validateModelDeploymentRuntime(
		deployment.Spec.Runtime, specPath.Child("runtime"))...)
	allErrs = append(allErrs, validateModelDeploymentResources(
		deployment.Spec.Resources, specPath.Child("resources"))...)
	allErrs = append(allErrs, validateForbiddenModelDeploymentFields(deployment.Spec, specPath)...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(GroupVersion.WithKind("ModelDeployment").GroupKind(), deployment.Name, allErrs)
}

// ValidateModelDeployment applies the complete static serving contract outside
// admission. Controllers use it as a fail-closed boundary when reconciling
// objects created while webhooks were unavailable or intentionally disabled.
func ValidateModelDeployment(deployment *ModelDeployment) error {
	return validateModelDeployment(deployment)
}

func validateForbiddenModelDeploymentFields(
	spec ModelDeploymentSpec,
	specPath *field.Path,
) field.ErrorList {
	runtimePath := specPath.Child("runtime")
	fields := []struct {
		path  *field.Path
		value *ForbiddenModelDeploymentField
	}{
		{specPath.Child("args"), spec.Args},
		{specPath.Child("env"), spec.Env},
		{specPath.Child("image"), spec.Image},
		{specPath.Child("ports"), spec.Ports},
		{specPath.Child("paths"), spec.Paths},
		{specPath.Child("probes"), spec.Probes},
		{specPath.Child("topology"), spec.Topology},
		{specPath.Child("replicas"), spec.Replicas},
		{runtimePath.Child("args"), spec.Runtime.Args},
		{runtimePath.Child("env"), spec.Runtime.Env},
		{runtimePath.Child("image"), spec.Runtime.Image},
		{runtimePath.Child("ports"), spec.Runtime.Ports},
		{runtimePath.Child("paths"), spec.Runtime.Paths},
		{runtimePath.Child("probes"), spec.Runtime.Probes},
		{runtimePath.Child("topology"), spec.Runtime.Topology},
		{runtimePath.Child("replicas"), spec.Runtime.Replicas},
	}
	var allErrs field.ErrorList
	for _, protected := range fields {
		if protected.value != nil {
			allErrs = append(allErrs, field.Forbidden(protected.path, "field is controller-owned"))
		}
	}
	return allErrs
}

func validateModelDeploymentPlacement(
	placement ModelDeploymentPlacementSpec, placementPath *field.Path,
) field.ErrorList {
	var allErrs field.ErrorList
	resourcePath := placementPath.Child("acceleratorResource")

	switch placement.Mode {
	case ModelDeploymentPlacementCPU:
		if placement.AcceleratorResource != "" {
			allErrs = append(allErrs, field.Forbidden(resourcePath,
				"acceleratorResource is forbidden in CPU mode"))
		}
	case ModelDeploymentPlacementAccelerator:
		acceleratorResource := placement.AcceleratorResource
		if acceleratorResource == "" {
			acceleratorResource = DefaultAcceleratorResource
		}
		if acceleratorResource != DefaultAcceleratorResource {
			allErrs = append(allErrs, field.NotSupported(resourcePath, acceleratorResource,
				[]string{string(DefaultAcceleratorResource)}))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(placementPath.Child("mode"), placement.Mode,
			[]string{string(ModelDeploymentPlacementCPU), string(ModelDeploymentPlacementAccelerator)}))
	}

	return allErrs
}

func validateModelDeploymentRuntime(
	runtime ModelDeploymentRuntimeSpec, runtimePath *field.Path,
) field.ErrorList {
	var allErrs field.ErrorList

	concurrency := DefaultModelDeploymentConcurrency
	if runtime.DesiredConcurrency != nil {
		concurrency = *runtime.DesiredConcurrency
	}
	if concurrency <= 0 {
		allErrs = append(allErrs, field.Invalid(runtimePath.Child("desiredConcurrency"),
			concurrency, "must be greater than zero"))
	} else if concurrency > MaximumModelDeploymentConcurrency {
		allErrs = append(allErrs, field.Invalid(runtimePath.Child("desiredConcurrency"), concurrency,
			fmt.Sprintf("must not exceed %d", MaximumModelDeploymentConcurrency)))
	}

	if runtime.MaxContextTokens == nil {
		if concurrency != 1 {
			allErrs = append(allErrs, field.Invalid(runtimePath.Child("desiredConcurrency"), concurrency,
				"model-native context requires desiredConcurrency to be 1"))
		}
	} else {
		contextTokens := *runtime.MaxContextTokens
		if contextTokens <= 0 {
			allErrs = append(allErrs, field.Invalid(runtimePath.Child("maxContextTokens"), contextTokens,
				"must be greater than zero"))
		} else if concurrency > 0 && contextTokens > math.MaxInt64/int64(concurrency) {
			allErrs = append(allErrs, field.Invalid(runtimePath.Child("maxContextTokens"), contextTokens,
				"maxContextTokens multiplied by desiredConcurrency overflows a signed 64-bit value"))
		}
	}

	drainTimeout := DefaultModelDeploymentDrainTimeout
	if runtime.DrainTimeout != nil {
		drainTimeout = runtime.DrainTimeout.Duration
	}
	if drainTimeout < MinimumModelDeploymentDrainTimeout || drainTimeout > MaximumModelDeploymentDrainTimeout {
		allErrs = append(allErrs, field.Invalid(runtimePath.Child("drainTimeout"), drainTimeout.String(),
			fmt.Sprintf("must be between %s and %s", MinimumModelDeploymentDrainTimeout, MaximumModelDeploymentDrainTimeout)))
	}

	keyType := runtime.KVCache.KeyType
	if keyType == "" {
		keyType = ModelDeploymentKVCacheF16
	}
	if !supportedModelDeploymentKVCacheType(keyType) {
		allErrs = append(allErrs, field.NotSupported(runtimePath.Child("kvCache", "keyType"), keyType,
			supportedModelDeploymentKVCacheTypes()))
	}
	valueType := runtime.KVCache.ValueType
	if valueType == "" {
		valueType = ModelDeploymentKVCacheF16
	}
	if !supportedModelDeploymentKVCacheType(valueType) {
		allErrs = append(allErrs, field.NotSupported(runtimePath.Child("kvCache", "valueType"), valueType,
			supportedModelDeploymentKVCacheTypes()))
	}

	allErrs = append(allErrs, validateModelDeploymentExpert(runtime.Expert,
		runtimePath.Child("expert"))...)
	return allErrs
}

func validateModelDeploymentExpert(
	expert ModelDeploymentExpertSpec, expertPath *field.Path,
) field.ErrorList {
	var allErrs field.ErrorList

	batchSize := DefaultModelDeploymentBatchSize
	if expert.BatchSize != nil {
		batchSize = *expert.BatchSize
	}
	if batchSize <= 0 {
		allErrs = append(allErrs, field.Invalid(expertPath.Child("batchSize"), batchSize,
			"must be greater than zero"))
	}
	microBatchSize := DefaultModelDeploymentMicroBatchSize
	if expert.MicroBatchSize != nil {
		microBatchSize = *expert.MicroBatchSize
	}
	if microBatchSize <= 0 {
		allErrs = append(allErrs, field.Invalid(expertPath.Child("microBatchSize"), microBatchSize,
			"must be greater than zero"))
	} else if batchSize > 0 && microBatchSize > batchSize {
		allErrs = append(allErrs, field.Invalid(expertPath.Child("microBatchSize"), microBatchSize,
			"must not exceed batchSize"))
	}

	for name, value := range map[string]*int32{
		"threads":      expert.Threads,
		"batchThreads": expert.BatchThreads,
	} {
		if value != nil && *value <= 0 {
			allErrs = append(allErrs, field.Invalid(expertPath.Child(name), *value,
				"must be greater than zero"))
		}
	}

	flashAttention := expert.FlashAttention
	if flashAttention == "" {
		flashAttention = ModelDeploymentFlashAttentionAuto
	}
	switch flashAttention {
	case ModelDeploymentFlashAttentionAuto,
		ModelDeploymentFlashAttentionEnabled,
		ModelDeploymentFlashAttentionDisabled:
	default:
		allErrs = append(allErrs, field.NotSupported(expertPath.Child("flashAttention"), flashAttention,
			[]string{
				string(ModelDeploymentFlashAttentionAuto),
				string(ModelDeploymentFlashAttentionEnabled),
				string(ModelDeploymentFlashAttentionDisabled),
			}))
	}

	return allErrs
}

func validateModelDeploymentResources(
	resources ModelDeploymentResourceRequirements, resourcesPath *field.Path,
) field.ErrorList {
	var allErrs field.ErrorList
	requestsPath := resourcesPath.Child("requests")
	limitsPath := resourcesPath.Child("limits")

	allErrs = append(allErrs, validateServingResourceList(resources.Requests, requestsPath)...)
	allErrs = append(allErrs, validateServingResourceList(resources.Limits, limitsPath)...)

	cpuRequest, hasCPURequest := resources.Requests[corev1.ResourceCPU]
	if !hasCPURequest {
		allErrs = append(allErrs, field.Required(requestsPath.Key(string(corev1.ResourceCPU)),
			"a positive CPU request is required"))
	}
	memoryRequest, hasMemoryRequest := resources.Requests[corev1.ResourceMemory]
	if !hasMemoryRequest {
		allErrs = append(allErrs, field.Required(requestsPath.Key(string(corev1.ResourceMemory)),
			"a positive memory request is required"))
	}
	memoryLimit, hasMemoryLimit := resources.Limits[corev1.ResourceMemory]
	if !hasMemoryLimit {
		allErrs = append(allErrs, field.Required(limitsPath.Key(string(corev1.ResourceMemory)),
			"a positive memory limit is required"))
	}

	if hasMemoryRequest && memoryRequest.Sign() > 0 && hasMemoryLimit && memoryLimit.Sign() > 0 &&
		memoryLimit.Cmp(memoryRequest) < 0 {
		allErrs = append(allErrs, field.Invalid(limitsPath.Key(string(corev1.ResourceMemory)),
			memoryLimit.String(), "must be greater than or equal to the memory request"))
	}
	if cpuLimit, hasCPULimit := resources.Limits[corev1.ResourceCPU]; hasCPULimit {
		if hasCPURequest && cpuRequest.Sign() > 0 && cpuLimit.Sign() > 0 && cpuLimit.Cmp(cpuRequest) < 0 {
			allErrs = append(allErrs, field.Invalid(limitsPath.Key(string(corev1.ResourceCPU)),
				cpuLimit.String(), "must be greater than or equal to the CPU request"))
		}
	}
	if ephemeralRequest, hasRequest := resources.Requests[corev1.ResourceEphemeralStorage]; hasRequest {
		if ephemeralLimit, hasLimit := resources.Limits[corev1.ResourceEphemeralStorage]; hasLimit {
			if ephemeralRequest.Sign() > 0 && ephemeralLimit.Sign() > 0 &&
				ephemeralLimit.Cmp(ephemeralRequest) < 0 {
				allErrs = append(allErrs, field.Invalid(limitsPath.Key(string(corev1.ResourceEphemeralStorage)),
					ephemeralLimit.String(), "must be greater than or equal to the ephemeral-storage request"))
			}
		}
	}

	return allErrs
}

func validateServingResourceList(resources corev1.ResourceList, resourcePath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for name, quantity := range resources {
		namePath := resourcePath.Key(string(name))
		if !supportedServingResourceName(name) {
			message := "only cpu, memory, and ephemeral-storage may be specified; Kama owns accelerator resources"
			if strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix) {
				message = "hugepage resources are not supported by the M2 serving contract"
			}
			allErrs = append(allErrs, field.Forbidden(namePath, message))
			continue
		}
		if quantity.Sign() <= 0 {
			allErrs = append(allErrs, field.Invalid(namePath, quantity.String(), "must be greater than zero"))
		}
	}
	return allErrs
}

func supportedServingResourceName(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return true
	default:
		return false
	}
}

func supportedModelDeploymentKVCacheType(value ModelDeploymentKVCacheType) bool {
	switch value {
	case ModelDeploymentKVCacheF16, ModelDeploymentKVCacheQ8, ModelDeploymentKVCacheQ4:
		return true
	default:
		return false
	}
}

func supportedModelDeploymentKVCacheTypes() []string {
	return []string{
		string(ModelDeploymentKVCacheF16),
		string(ModelDeploymentKVCacheQ8),
		string(ModelDeploymentKVCacheQ4),
	}
}

// +kubebuilder:webhook:path=/mutate-kama-tannerburns-github-io-v1alpha1-modeldeployment,mutating=true,failurePolicy=fail,sideEffects=None,groups=kama.tannerburns.github.io,resources=modeldeployments,verbs=create;update,versions=v1alpha1,name=mmodeldeployment.kama.tannerburns.github.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kama-tannerburns-github-io-v1alpha1-modeldeployment,mutating=false,failurePolicy=fail,sideEffects=None,groups=kama.tannerburns.github.io,resources=modeldeployments,verbs=create;update,versions=v1alpha1,name=vmodeldeployment.kama.tannerburns.github.io,admissionReviewVersions=v1
