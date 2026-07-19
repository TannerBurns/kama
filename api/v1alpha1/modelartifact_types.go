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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// ModelArtifactFinalizer protects transient import resources and reference checks.
	ModelArtifactFinalizer = "modelartifact.kama.tannerburns.github.io/finalizer"

	// ModelArtifactConditionSourceResolved reports successful immutable source resolution.
	ModelArtifactConditionSourceResolved = "SourceResolved"
	// ModelArtifactConditionStorageReady reports that source and destination storage are mountable.
	ModelArtifactConditionStorageReady = "StorageReady"
	// ModelArtifactConditionImporting reports active import or validation work.
	ModelArtifactConditionImporting = "Importing"
	// ModelArtifactConditionVerified reports successful size, digest, GGUF, and shard validation.
	ModelArtifactConditionVerified = "Verified"
	// ModelArtifactConditionReady reports that verified content is safe for serving.
	ModelArtifactConditionReady = "Ready"
	// ModelArtifactConditionInvalidGGUF reports malformed or unsupported GGUF content.
	ModelArtifactConditionInvalidGGUF = "InvalidGGUF"
	// ModelArtifactConditionChecksumMismatch reports a mismatch with declared verification data.
	ModelArtifactConditionChecksumMismatch = "ChecksumMismatch"
	// ModelArtifactConditionMissingShard reports an incomplete standard GGUF shard set.
	ModelArtifactConditionMissingShard = "MissingShard"
	// ModelArtifactConditionInsufficientStorage reports insufficient destination capacity.
	ModelArtifactConditionInsufficientStorage = "InsufficientStorage"
	// ModelArtifactConditionSourceUnavailable reports a source resolution, authorization, or read failure.
	ModelArtifactConditionSourceUnavailable = "SourceUnavailable"
)

// ArtifactFormat is the on-disk model format validated by Kama.
// +kubebuilder:validation:Enum=GGUF
type ArtifactFormat string

const (
	// ArtifactFormatGGUF selects the GGUF v3 artifact format.
	ArtifactFormatGGUF ArtifactFormat = "GGUF"
)

// PVCImportPolicy controls whether PVC content is copied into a cache or served in place.
// +kubebuilder:validation:Enum=Copy;Direct
type PVCImportPolicy string

const (
	// PVCImportPolicyCopy verifies and publishes source content into a ModelCache.
	PVCImportPolicyCopy PVCImportPolicy = "Copy"
	// PVCImportPolicyDirect validates and serves an adopted claim in place.
	PVCImportPolicyDirect PVCImportPolicy = "Direct"
)

// ModelArtifactSpec declares one immutable GGUF file or verified GGUF shard set.
type ModelArtifactSpec struct {
	// Format is the artifact format. M1 supports GGUF.
	Format ArtifactFormat `json:"format"`

	// Entrypoint is a clean POSIX relative path to the primary GGUF file.
	Entrypoint string `json:"entrypoint"`

	// Source selects exactly one artifact source.
	Source ModelArtifactSource `json:"source"`

	// CacheRef identifies a ModelCache in this namespace. It is required for
	// Hugging Face and Copy sources, and forbidden for Direct sources.
	// +optional
	CacheRef *corev1.LocalObjectReference `json:"cacheRef,omitempty"`

	// Verification contains optional expected content identity assertions.
	// +optional
	Verification ModelArtifactVerificationSpec `json:"verification,omitempty"`
}

// ModelArtifactSource selects exactly one remote or persistent-volume source.
// +kubebuilder:validation:XValidation:rule="has(self.huggingFace) != has(self.persistentVolumeClaim)",message="exactly one of huggingFace or persistentVolumeClaim must be set"
type ModelArtifactSource struct {
	// HuggingFace downloads selected files from one pinned or resolvable repository revision.
	// +optional
	HuggingFace *HuggingFaceSource `json:"huggingFace,omitempty"`

	// PersistentVolumeClaim copies or directly validates content on a same-namespace claim.
	// +optional
	PersistentVolumeClaim *PersistentVolumeClaimSource `json:"persistentVolumeClaim,omitempty"`
}

// HuggingFaceSource selects GGUF files from a Hugging Face model repository.
type HuggingFaceSource struct {
	// Repository is a Hugging Face model repository ID, such as owner/model.
	Repository string `json:"repository"`

	// Revision is a required commit, tag, or branch resolved once to an immutable commit.
	Revision string `json:"revision"`

	// Files contains one or more clean POSIX relative file selectors.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	Files []string `json:"files"`

	// TokenSecretRef optionally selects a token key from a Secret in this namespace.
	// +optional
	TokenSecretRef *SecretKeyReference `json:"tokenSecretRef,omitempty"`
}

// SecretKeyReference selects one key from a same-namespace Secret.
type SecretKeyReference struct {
	// Name is the Secret name.
	Name string `json:"name"`

	// Key is the Secret data key containing the token.
	Key string `json:"key"`
}

// PersistentVolumeClaimSource selects files from a same-namespace claim.
type PersistentVolumeClaimSource struct {
	// ClaimName is the source PersistentVolumeClaim name.
	ClaimName string `json:"claimName"`

	// RootPath is a clean POSIX relative directory containing the entrypoint.
	// A value of "." selects the volume root.
	RootPath string `json:"rootPath"`

	// ImportPolicy defaults to Copy. Direct retains and serves the adopted claim in place.
	// +kubebuilder:default=Copy
	// +optional
	ImportPolicy PVCImportPolicy `json:"importPolicy,omitempty"`
}

// ModelArtifactVerificationSpec declares optional expected artifact identity.
type ModelArtifactVerificationSpec struct {
	// ExpectedSHA256 is the lowercase or uppercase hexadecimal SHA-256 of a single
	// file, or of the canonical content manifest for a shard set.
	// +kubebuilder:validation:Pattern=`^[A-Fa-f0-9]{64}$`
	// +optional
	ExpectedSHA256 string `json:"expectedSHA256,omitempty"`

	// ExpectedSize is the aggregate selected content size in bytes.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ExpectedSize *int64 `json:"expectedSize,omitempty"`
}

// ModelArtifactFileStatus records the immutable identity of one selected file.
type ModelArtifactFileStatus struct {
	// Path is the clean relative path within the validated content root.
	Path string `json:"path"`

	// Size is the file size in bytes.
	// +kubebuilder:validation:Minimum=0
	Size int64 `json:"size"`

	// SHA256 is the lowercase hexadecimal file digest.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
}

// ModelArtifactJobReference identifies the deterministic import or validation Job.
type ModelArtifactJobReference struct {
	// Name is the Job name.
	Name string `json:"name"`

	// UID identifies the exact Job observed by the controller.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// ModelArtifactLocationStatus is the complete storage contract consumed by serving reconciliation.
type ModelArtifactLocationStatus struct {
	// ClaimName is the claim serving Pods mount.
	ClaimName string `json:"claimName"`

	// ClaimUID identifies the exact serving claim validated by Kama.
	// +optional
	ClaimUID types.UID `json:"claimUID,omitempty"`

	// SubPath is the clean path within the claim containing the artifact manifest and content.
	SubPath string `json:"subPath"`

	// ReadOnly requires serving containers to mount the location read-only.
	ReadOnly bool `json:"readOnly"`

	// AccessModes are the access modes advertised by the serving claim.
	// +optional
	// +listType=set
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// VolumeMode is the serving claim's volume mode.
	// +optional
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`

	// MountScope is Kama's normalized scheduling contract for this location.
	MountScope MountScope `json:"mountScope"`

	// VolumeName is the bound PersistentVolume name.
	// +optional
	VolumeName string `json:"volumeName,omitempty"`

	// VolumeUID identifies the exact PersistentVolume observed by the controller.
	// +optional
	VolumeUID types.UID `json:"volumeUID,omitempty"`

	// NodeAffinity is the normalized node affinity from the bound PersistentVolume.
	// +optional
	NodeAffinity *corev1.VolumeNodeAffinity `json:"nodeAffinity,omitempty"`
}

// ModelArtifactStatus reports resolved source identity, verification, and serving location.
type ModelArtifactStatus struct {
	// ObservedGeneration is the most recent generation reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedRevision is the immutable full Hugging Face commit for a remote source.
	// +optional
	ResolvedRevision string `json:"resolvedRevision,omitempty"`

	// Files is the bounded, sorted set of selected file identities.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=path
	Files []ModelArtifactFileStatus `json:"files,omitempty"`

	// ArtifactDigest is the single-file digest or canonical content-manifest digest.
	// +optional
	ArtifactDigest string `json:"artifactDigest,omitempty"`

	// Size is the aggregate selected content size in bytes.
	// +optional
	Size int64 `json:"size,omitempty"`

	// Architecture is the architecture reported by GGUF metadata.
	// +optional
	Architecture string `json:"architecture,omitempty"`

	// Quantization is the quantization description derived from GGUF metadata.
	// +optional
	Quantization string `json:"quantization,omitempty"`

	// ShardCount is the complete standard GGUF shard count.
	// +optional
	ShardCount int32 `json:"shardCount,omitempty"`

	// ValidatedAt records successful completion of content validation.
	// +optional
	ValidatedAt *metav1.Time `json:"validatedAt,omitempty"`

	// JobRef identifies the most recent deterministic import or validation Job.
	// +optional
	JobRef *ModelArtifactJobReference `json:"jobRef,omitempty"`

	// Location is present only when storage identity has been resolved.
	// +optional
	Location *ModelArtifactLocationStatus `json:"location,omitempty"`

	// CleanupOperationID is the controller-authenticated checkpoint for a
	// successful deletion cleanup operation. It is written only through the
	// status subresource and is not artifact-serving state.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{20}$`
	CleanupOperationID string `json:"cleanupOperationID,omitempty"`

	// Conditions summarize source, import, verification, and readiness state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelArtifact represents immutable verified GGUF content independently of serving workloads.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ma
// +kubebuilder:printcolumn:name="Format",type=string,JSONPath=`.spec.format`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.status.artifactDigest`,priority=1
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelArtifact struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelArtifactSpec   `json:"spec"`
	Status ModelArtifactStatus `json:"status,omitempty"`
}

// ModelArtifactList contains a list of ModelArtifact objects.
// +kubebuilder:object:root=true
type ModelArtifactList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelArtifact `json:"items"`
}
