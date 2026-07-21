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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// ModelDeploymentFinalizer coordinates drain-first serving workload deletion.
	ModelDeploymentFinalizer = "modeldeployment.kama.tannerburns.github.io/finalizer"

	// ModelDeploymentConditionArtifactReady reports whether the referenced artifact is safe to mount.
	ModelDeploymentConditionArtifactReady = "ArtifactReady"
	// ModelDeploymentConditionResourcesAvailable reports whether the requested serving resources are available.
	ModelDeploymentConditionResourcesAvailable = "ResourcesAvailable"
	// ModelDeploymentConditionRuntimeReady reports whether the selected runtime loaded the model successfully.
	ModelDeploymentConditionRuntimeReady = "RuntimeReady"
	// ModelDeploymentConditionServing reports whether the current runtime fingerprint has a ready endpoint.
	ModelDeploymentConditionServing = "Serving"
	// ModelDeploymentConditionDegraded reports an intentional or observed reduction in serving guarantees.
	ModelDeploymentConditionDegraded = "Degraded"

	// DefaultAcceleratorResource is the only accelerator resource supported by the M2 runtime.
	DefaultAcceleratorResource corev1.ResourceName = "nvidia.com/gpu"
	// DefaultModelDeploymentConcurrency is the default number of independent runtime slots.
	DefaultModelDeploymentConcurrency int32 = 1
	// MaximumModelDeploymentConcurrency bounds the pinned llama.cpp slot contract.
	MaximumModelDeploymentConcurrency int32 = 128
	// DefaultModelDeploymentBatchSize is the default logical prompt-processing batch size.
	DefaultModelDeploymentBatchSize int32 = 2048
	// DefaultModelDeploymentMicroBatchSize is the default physical prompt-processing batch size.
	DefaultModelDeploymentMicroBatchSize int32 = 512
	// DefaultModelDeploymentDrainTimeout bounds graceful serving termination by default.
	DefaultModelDeploymentDrainTimeout time.Duration = 10 * time.Minute
	// MinimumModelDeploymentDrainTimeout prevents endpoint propagation from consuming the drain window.
	MinimumModelDeploymentDrainTimeout time.Duration = 30 * time.Second
	// MaximumModelDeploymentDrainTimeout bounds Pod termination and rollout time.
	MaximumModelDeploymentDrainTimeout time.Duration = time.Hour
)

// ModelDeploymentPlacementMode selects one explicit serving runtime class.
// +kubebuilder:validation:Enum=CPU;Accelerator
type ModelDeploymentPlacementMode string

const (
	// ModelDeploymentPlacementCPU selects the CPU-only runtime.
	ModelDeploymentPlacementCPU ModelDeploymentPlacementMode = "CPU"
	// ModelDeploymentPlacementAccelerator selects the single-NVIDIA-GPU CUDA runtime.
	ModelDeploymentPlacementAccelerator ModelDeploymentPlacementMode = "Accelerator"
)

// ModelDeploymentKVCacheType selects a llama.cpp KV cache representation.
// +kubebuilder:validation:Enum=f16;q8_0;q4_0
type ModelDeploymentKVCacheType string

const (
	// ModelDeploymentKVCacheF16 uses full 16-bit floating-point KV cache values.
	ModelDeploymentKVCacheF16 ModelDeploymentKVCacheType = "f16"
	// ModelDeploymentKVCacheQ8 uses 8-bit quantized KV cache values.
	ModelDeploymentKVCacheQ8 ModelDeploymentKVCacheType = "q8_0"
	// ModelDeploymentKVCacheQ4 uses 4-bit quantized KV cache values.
	ModelDeploymentKVCacheQ4 ModelDeploymentKVCacheType = "q4_0"
)

// ModelDeploymentFlashAttention controls llama.cpp flash-attention selection.
// +kubebuilder:validation:Enum=Auto;Enabled;Disabled
type ModelDeploymentFlashAttention string

const (
	// ModelDeploymentFlashAttentionAuto lets the pinned runtime select compatible behavior.
	ModelDeploymentFlashAttentionAuto ModelDeploymentFlashAttention = "Auto"
	// ModelDeploymentFlashAttentionEnabled requires flash attention.
	ModelDeploymentFlashAttentionEnabled ModelDeploymentFlashAttention = "Enabled"
	// ModelDeploymentFlashAttentionDisabled disables flash attention.
	ModelDeploymentFlashAttentionDisabled ModelDeploymentFlashAttention = "Disabled"
)

// ModelDeploymentRuntimeState is the supervisor's bounded lifecycle state.
// +kubebuilder:validation:Enum=Initializing;Loading;Ready;Draining;LoadFailed;Exited
type ModelDeploymentRuntimeState string

const (
	// ModelDeploymentRuntimeInitializing means the supervisor is validating its configuration.
	ModelDeploymentRuntimeInitializing ModelDeploymentRuntimeState = "Initializing"
	// ModelDeploymentRuntimeLoading means llama-server is loading the artifact.
	ModelDeploymentRuntimeLoading ModelDeploymentRuntimeState = "Loading"
	// ModelDeploymentRuntimeReady means llama-server is ready to accept requests.
	ModelDeploymentRuntimeReady ModelDeploymentRuntimeState = "Ready"
	// ModelDeploymentRuntimeDraining means new requests are rejected while active work finishes.
	ModelDeploymentRuntimeDraining ModelDeploymentRuntimeState = "Draining"
	// ModelDeploymentRuntimeLoadFailed means the one-shot child failed to load the artifact.
	ModelDeploymentRuntimeLoadFailed ModelDeploymentRuntimeState = "LoadFailed"
	// ModelDeploymentRuntimeExited means the one-shot child exited after it was started.
	ModelDeploymentRuntimeExited ModelDeploymentRuntimeState = "Exited"
)

// ModelDeploymentSpec declares one fixed single-replica model-serving workload.
type ModelDeploymentSpec struct {
	// ModelRef identifies a ModelArtifact in this namespace.
	ModelRef corev1.LocalObjectReference `json:"modelRef"`

	// Placement explicitly selects CPU or a single accelerator.
	Placement ModelDeploymentPlacementSpec `json:"placement"`

	// Runtime contains safe llama.cpp serving controls.
	// +kubebuilder:default={}
	// +optional
	Runtime ModelDeploymentRuntimeSpec `json:"runtime,omitempty"`

	// Resources declares CPU and memory needed by the serving container.
	Resources ModelDeploymentResourceRequirements `json:"resources"`

	// Args is reserved so attempts to inject raw runtime arguments fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="args is controller-owned and forbidden"
	Args *ForbiddenModelDeploymentField `json:"args,omitempty"`

	// Env is reserved so attempts to inject environment variables fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="env is controller-owned and forbidden"
	Env *ForbiddenModelDeploymentField `json:"env,omitempty"`

	// Image is reserved so attempts to override runtime images fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="image is controller-owned and forbidden"
	Image *ForbiddenModelDeploymentField `json:"image,omitempty"`

	// Ports is reserved so attempts to override network ports fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="ports is controller-owned and forbidden"
	Ports *ForbiddenModelDeploymentField `json:"ports,omitempty"`

	// Paths is reserved so attempts to override controller-owned paths fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="paths is controller-owned and forbidden"
	Paths *ForbiddenModelDeploymentField `json:"paths,omitempty"`

	// Probes is reserved so attempts to override health probes fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="probes is controller-owned and forbidden"
	Probes *ForbiddenModelDeploymentField `json:"probes,omitempty"`

	// Topology is reserved so attempts to override placement topology fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="topology is controller-owned and forbidden"
	Topology *ForbiddenModelDeploymentField `json:"topology,omitempty"`

	// Replicas is reserved so attempts to override the fixed replica count fail schema validation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="replicas is controller-owned and forbidden"
	Replicas *ForbiddenModelDeploymentField `json:"replicas,omitempty"`
}

// ForbiddenModelDeploymentField is an empty schema tombstone used solely to
// reject controller-owned fields before admission pruning can discard them.
type ForbiddenModelDeploymentField struct{}

// ModelDeploymentPlacementSpec selects the runtime and accelerator resource.
type ModelDeploymentPlacementSpec struct {
	// Mode is required; M2 does not perform automatic placement.
	Mode ModelDeploymentPlacementMode `json:"mode"`

	// AcceleratorResource defaults to nvidia.com/gpu in Accelerator mode and is
	// forbidden in CPU mode. M2 admits exactly one full NVIDIA GPU.
	// +optional
	AcceleratorResource corev1.ResourceName `json:"acceleratorResource,omitempty"`
}

// ModelDeploymentRuntimeSpec contains the bounded llama.cpp controls exposed in M2.
// +kubebuilder:validation:XValidation:rule="has(self.maxContextTokens) || !has(self.desiredConcurrency) || self.desiredConcurrency == 1",message="model-native context requires desiredConcurrency to be 1"
type ModelDeploymentRuntimeSpec struct {
	// MaxContextTokens is the exact per-request context. Omission selects the
	// model-advertised native context and does not permit silent shrinking.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxContextTokens *int64 `json:"maxContextTokens,omitempty"`

	// DesiredConcurrency is the fixed number of independent llama.cpp slots.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	// +optional
	DesiredConcurrency *int32 `json:"desiredConcurrency,omitempty"`

	// DrainTimeout bounds how long active requests may finish during replacement or deletion.
	// +kubebuilder:default="10m"
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`

	// KVCache selects the key and value cache representations.
	// +kubebuilder:default={}
	// +optional
	KVCache ModelDeploymentKVCacheSpec `json:"kvCache,omitempty"`

	// Expert contains typed low-level performance controls that cannot override
	// Kama-owned model, network, topology, security, or lifecycle arguments.
	// +kubebuilder:default={}
	// +optional
	Expert ModelDeploymentExpertSpec `json:"expert,omitempty"`

	// Args is reserved so raw llama-server arguments cannot be supplied.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.args is controller-owned and forbidden"
	Args *ForbiddenModelDeploymentField `json:"args,omitempty"`

	// Env is reserved so runtime environment variables cannot be supplied.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.env is controller-owned and forbidden"
	Env *ForbiddenModelDeploymentField `json:"env,omitempty"`

	// Image is reserved so the controller remains the only runtime image authority.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.image is controller-owned and forbidden"
	Image *ForbiddenModelDeploymentField `json:"image,omitempty"`

	// Ports is reserved so runtime network ports cannot be supplied.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.ports is controller-owned and forbidden"
	Ports *ForbiddenModelDeploymentField `json:"ports,omitempty"`

	// Paths is reserved so runtime filesystem paths cannot be supplied.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.paths is controller-owned and forbidden"
	Paths *ForbiddenModelDeploymentField `json:"paths,omitempty"`

	// Probes is reserved so runtime health probes cannot be supplied.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.probes is controller-owned and forbidden"
	Probes *ForbiddenModelDeploymentField `json:"probes,omitempty"`

	// Topology is reserved so runtime topology cannot be supplied.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.topology is controller-owned and forbidden"
	Topology *ForbiddenModelDeploymentField `json:"topology,omitempty"`

	// Replicas is reserved so the runtime cannot bypass fixed single-replica serving.
	// +optional
	// +kubebuilder:validation:XValidation:rule="false",message="runtime.replicas is controller-owned and forbidden"
	Replicas *ForbiddenModelDeploymentField `json:"replicas,omitempty"`
}

// ModelDeploymentKVCacheSpec selects bounded key/value cache types.
type ModelDeploymentKVCacheSpec struct {
	// KeyType defaults to f16.
	// +kubebuilder:default=f16
	// +optional
	KeyType ModelDeploymentKVCacheType `json:"keyType,omitempty"`

	// ValueType defaults to f16.
	// +kubebuilder:default=f16
	// +optional
	ValueType ModelDeploymentKVCacheType `json:"valueType,omitempty"`
}

// ModelDeploymentExpertSpec contains the safe expert controls exposed in M2.
// +kubebuilder:validation:XValidation:rule="!has(self.microBatchSize) || !has(self.batchSize) || self.microBatchSize <= self.batchSize",message="microBatchSize must not exceed batchSize"
type ModelDeploymentExpertSpec struct {
	// BatchSize is the logical prompt-processing batch size.
	// +kubebuilder:default=2048
	// +kubebuilder:validation:Minimum=1
	// +optional
	BatchSize *int32 `json:"batchSize,omitempty"`

	// MicroBatchSize is the physical prompt-processing batch size.
	// +kubebuilder:default=512
	// +kubebuilder:validation:Minimum=1
	// +optional
	MicroBatchSize *int32 `json:"microBatchSize,omitempty"`

	// Threads explicitly sets generation threads when present.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Threads *int32 `json:"threads,omitempty"`

	// BatchThreads explicitly sets prompt-processing threads when present.
	// +kubebuilder:validation:Minimum=1
	// +optional
	BatchThreads *int32 `json:"batchThreads,omitempty"`

	// FlashAttention defaults to Auto.
	// +kubebuilder:default=Auto
	// +optional
	FlashAttention ModelDeploymentFlashAttention `json:"flashAttention,omitempty"`
}

// ModelDeploymentResourceRequirements is the deliberately narrow resource
// contract accepted from ModelDeployment authors. The controller owns any GPU request.
type ModelDeploymentResourceRequirements struct {
	// Requests must include positive CPU and memory values. Ephemeral storage is optional.
	Requests corev1.ResourceList `json:"requests"`

	// Limits must include memory. CPU and ephemeral-storage limits are optional.
	Limits corev1.ResourceList `json:"limits"`
}

// ModelDeploymentArtifactStatus identifies the exact artifact revision consumed by serving.
type ModelDeploymentArtifactStatus struct {
	// Name is the referenced ModelArtifact name.
	// +optional
	Name string `json:"name,omitempty"`

	// UID identifies the exact ModelArtifact object observed by the controller.
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// Digest is the verified artifact or canonical manifest SHA-256.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ModelDeploymentRuntimeStatus reports the bounded supervisor/runtime identity and state.
type ModelDeploymentRuntimeStatus struct {
	// State is the latest bounded supervisor lifecycle state.
	// +optional
	State ModelDeploymentRuntimeState `json:"state,omitempty"`

	// DesiredImage is the image selected by controller configuration.
	// +optional
	DesiredImage string `json:"desiredImage,omitempty"`

	// ObservedImage is the immutable image identity reported by the serving Pod.
	// +optional
	ObservedImage string `json:"observedImage,omitempty"`

	// LlamaCommit is the pinned llama.cpp source commit reported by the runtime.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{40}$`
	// +optional
	LlamaCommit string `json:"llamaCommit,omitempty"`

	// DesiredFingerprint identifies the current spec, artifact, image, and generated configuration.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{20}$`
	// +optional
	DesiredFingerprint string `json:"desiredFingerprint,omitempty"`

	// ObservedFingerprint identifies the configuration reported by the observed serving Pod's supervisor.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{20}$`
	// +optional
	ObservedFingerprint string `json:"observedFingerprint,omitempty"`

	// LoadedFingerprint is a durable checkpoint that this exact runtime
	// fingerprint completed loading successfully. It does not by itself imply
	// current readiness; Serving still requires a current supervisor response
	// and ready endpoint.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{20}$`
	// +optional
	LoadedFingerprint string `json:"loadedFingerprint,omitempty"`

	// EffectiveContextTokens is the actual per-slot context loaded by the runtime.
	// +kubebuilder:validation:Minimum=1
	// +optional
	EffectiveContextTokens *int64 `json:"effectiveContextTokens,omitempty"`

	// EffectiveConcurrency is the number of runtime slots observed by the supervisor.
	// +kubebuilder:validation:Minimum=1
	// +optional
	EffectiveConcurrency int32 `json:"effectiveConcurrency,omitempty"`

	// AcceleratorDetected reports whether the runtime observed its configured accelerator.
	// It is nil until a supervisor observation is available.
	// +optional
	AcceleratorDetected *bool `json:"acceleratorDetected,omitempty"`

	// OffloadedLayers is the bounded number of model layers reported as accelerator-offloaded.
	// +kubebuilder:validation:Minimum=0
	// +optional
	OffloadedLayers *int32 `json:"offloadedLayers,omitempty"`
}

// ModelDeploymentObjectReference identifies one generated serving object.
type ModelDeploymentObjectReference struct {
	// Name is the generated object's name.
	Name string `json:"name"`

	// UID identifies the exact object observed by the controller.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// ModelDeploymentStatus reports artifact, workload, runtime, and serving state.
type ModelDeploymentStatus struct {
	// ObservedGeneration is the most recent generation reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Artifact identifies the exact verified artifact selected for this workload.
	// +optional
	Artifact *ModelDeploymentArtifactStatus `json:"artifact,omitempty"`

	// Runtime reports the current desired and observed runtime identity.
	// +optional
	Runtime *ModelDeploymentRuntimeStatus `json:"runtime,omitempty"`

	// DeploymentRef identifies the generated singleton Deployment.
	// +optional
	DeploymentRef *ModelDeploymentObjectReference `json:"deploymentRef,omitempty"`

	// ServiceRef identifies the stable internal ClusterIP Service.
	// +optional
	ServiceRef *ModelDeploymentObjectReference `json:"serviceRef,omitempty"`

	// DesiredReplicas is one whenever an artifact-backed serving workload should exist.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// ReadyReplicas is the number of ready replicas for the desired runtime fingerprint.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Conditions summarize dependency, scheduling, runtime, serving, and degradation state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelDeployment is a fixed single-replica llama.cpp serving workload for one ModelArtifact.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=md
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.placement.mode`
// +kubebuilder:printcolumn:name="Serving",type=string,JSONPath=`.status.conditions[?(@.type=='Serving')].status`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelDeploymentSpec   `json:"spec"`
	Status ModelDeploymentStatus `json:"status,omitempty"`
}

// ModelDeploymentList contains a list of ModelDeployment objects.
// +kubebuilder:object:root=true
type ModelDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelDeployment `json:"items"`
}
