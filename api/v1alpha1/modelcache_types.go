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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// ModelCacheFinalizer protects controller-managed cache lifecycle work.
	ModelCacheFinalizer = "modelcache.kama.tannerburns.github.io/finalizer"

	// ModelCacheConditionReady reports that the cache is mounted and has passed its storage probe.
	ModelCacheConditionReady = "Ready"
	// ModelCacheConditionStorageUnavailable reports that the cache claim cannot currently be used.
	ModelCacheConditionStorageUnavailable = "StorageUnavailable"
	// ModelCacheConditionInsufficientCapacity reports that the cache has insufficient free capacity.
	ModelCacheConditionInsufficientCapacity = "InsufficientCapacity"
	// ModelCacheConditionDegraded reports that the cache remains usable with reduced guarantees.
	ModelCacheConditionDegraded = "Degraded"
)

// RetentionPolicy controls the lifecycle of a controller-created cache claim.
// +kubebuilder:validation:Enum=Retain;Delete
type RetentionPolicy string

const (
	// RetentionPolicyRetain preserves storage when its ModelCache is deleted.
	RetentionPolicyRetain RetentionPolicy = "Retain"
	// RetentionPolicyDelete permits deletion of an unreferenced controller-created claim.
	RetentionPolicyDelete RetentionPolicy = "Delete"
)

// MountScope is Kama's normalized scheduling contract for a volume.
// +kubebuilder:validation:Enum=MultiNode;SingleNode;SinglePod
type MountScope string

const (
	// MountScopeMultiNode permits consumers on multiple nodes.
	MountScopeMultiNode MountScope = "MultiNode"
	// MountScopeSingleNode limits consumers to one compatible node.
	MountScopeSingleNode MountScope = "SingleNode"
	// MountScopeSinglePod limits the volume to one Pod at a time.
	MountScopeSinglePod MountScope = "SinglePod"
)

// ModelCacheSpec declares the storage backing a persistent artifact cache.
type ModelCacheSpec struct {
	// Storage selects either an adopted claim or a controller-created claim.
	Storage ModelCacheStorageSpec `json:"storage"`

	// RetentionPolicy controls a controller-created claim after this cache is deleted.
	// Adopted claims always use Retain.
	// +kubebuilder:default=Retain
	// +optional
	RetentionPolicy RetentionPolicy `json:"retentionPolicy,omitempty"`
}

// ModelCacheStorageSpec selects exactly one storage provisioning mode.
// +kubebuilder:validation:XValidation:rule="has(self.existingClaim) != has(self.claimTemplate)",message="exactly one of existingClaim or claimTemplate must be set"
type ModelCacheStorageSpec struct {
	// ExistingClaim adopts an existing PersistentVolumeClaim in the ModelCache namespace.
	// Kama never adds ownership to an adopted claim.
	// +optional
	ExistingClaim *corev1.LocalObjectReference `json:"existingClaim,omitempty"`

	// ClaimTemplate declares a PersistentVolumeClaim managed by Kama.
	// +optional
	ClaimTemplate *ModelCacheClaimTemplate `json:"claimTemplate,omitempty"`
}

// ModelCacheClaimTemplate contains the supported metadata and storage fields for a managed claim.
type ModelCacheClaimTemplate struct {
	// Metadata is copied to the generated claim.
	// +optional
	Metadata ModelCacheClaimTemplateMetadata `json:"metadata,omitempty"`

	// Spec declares the generated claim's storage requirements.
	Spec ModelCacheClaimTemplateSpec `json:"spec"`
}

// ModelCacheClaimTemplateMetadata contains user-supplied metadata copied to a generated claim.
type ModelCacheClaimTemplateMetadata struct {
	// Labels are copied to the generated claim in addition to Kama ownership labels.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations are copied to the generated claim.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ModelCacheClaimTemplateSpec is the supported subset of a PersistentVolumeClaim spec.
type ModelCacheClaimTemplateSpec struct {
	// StorageClassName selects the provisioner's storage class. Nil uses the cluster default;
	// an explicit empty string disables dynamic provisioning.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// AccessModes must contain at least one writable access mode.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +listType=set
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes"`

	// VolumeMode is Filesystem for GGUF regular-file and mmap semantics.
	// +kubebuilder:default=Filesystem
	// +kubebuilder:validation:Enum=Filesystem
	// +optional
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`

	// Resources contains the storage capacity request.
	Resources ModelCacheResourceRequirements `json:"resources"`
}

// ModelCacheResourceRequirements contains the generated claim's storage request.
type ModelCacheResourceRequirements struct {
	// Requests must contain one positive storage request.
	Requests corev1.ResourceList `json:"requests"`
}

// ModelCacheStatus reports the resolved claim and its most recent storage probe.
type ModelCacheStatus struct {
	// ObservedGeneration is the most recent generation reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ClaimName is the resolved PersistentVolumeClaim name.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// ClaimUID identifies the exact claim observed by the controller.
	// +optional
	ClaimUID types.UID `json:"claimUID,omitempty"`

	// Capacity is the bound claim's provisioned storage capacity.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`

	// FreeSpace is the filesystem free-space value reported by the latest probe.
	// +optional
	FreeSpace *resource.Quantity `json:"freeSpace,omitempty"`

	// AccessModes are the access modes advertised by the bound claim.
	// +optional
	// +listType=set
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// VolumeMode is the bound claim's volume mode.
	// +optional
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`

	// MountScope is Kama's normalized scheduling contract for the resolved access mode.
	// +optional
	MountScope MountScope `json:"mountScope,omitempty"`

	// StorageClassName is the resolved storage class.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// VolumeName is the bound PersistentVolume name.
	// +optional
	VolumeName string `json:"volumeName,omitempty"`

	// VolumeUID identifies the exact PersistentVolume observed by the controller.
	// +optional
	VolumeUID types.UID `json:"volumeUID,omitempty"`

	// NodeAffinity is the normalized node affinity from the bound PersistentVolume.
	// +optional
	NodeAffinity *corev1.VolumeNodeAffinity `json:"nodeAffinity,omitempty"`

	// LastProbeTime records completion of the latest capacity and filesystem probe.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// Conditions summarize cache availability and probe results.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelCache is a namespaced durable storage pool for verified artifacts.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mc
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.status.claimName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelCacheSpec   `json:"spec"`
	Status ModelCacheStatus `json:"status,omitempty"`
}

// ModelCacheList contains a list of ModelCache objects.
// +kubebuilder:object:root=true
type ModelCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelCache `json:"items"`
}
