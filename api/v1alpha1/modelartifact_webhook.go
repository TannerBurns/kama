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
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const maxHuggingFaceRepositoryLength = 96

var (
	_ admission.Defaulter[*ModelArtifact] = &ModelArtifactDefaulter{}
	_ admission.Validator[*ModelArtifact] = &ModelArtifactValidator{}

	sha256Pattern            = regexp.MustCompile(`^[A-Fa-f0-9]{64}$`)
	huggingFaceRepoComponent = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9._-]*[A-Za-z0-9_])?$`)
	standardShardEntrypoint  = regexp.MustCompile(`^.*-(\d{5})-of-(\d{5})\.gguf$`)
)

// SetupModelArtifactWebhookWithManager registers ModelArtifact defaulting and validation webhooks.
func SetupModelArtifactWebhookWithManager(mgr manager.Manager) error {
	return builder.WebhookManagedBy(mgr, &ModelArtifact{}).
		WithDefaulter(&ModelArtifactDefaulter{}).
		WithValidator(&ModelArtifactValidator{}).
		Complete()
}

// ModelArtifactDefaulter applies API defaults without consulting source or storage state.
// +kubebuilder:object:generate=false
type ModelArtifactDefaulter struct{}

// Default selects safe Copy semantics and canonicalizes an expected digest.
func (*ModelArtifactDefaulter) Default(_ context.Context, artifact *ModelArtifact) error {
	if source := artifact.Spec.Source.PersistentVolumeClaim; source != nil && source.ImportPolicy == "" {
		source.ImportPolicy = PVCImportPolicyCopy
	}
	artifact.Spec.Verification.ExpectedSHA256 = strings.ToLower(artifact.Spec.Verification.ExpectedSHA256)
	return nil
}

// ModelArtifactValidator enforces the immutable artifact source, storage, and verification contract.
// +kubebuilder:object:generate=false
type ModelArtifactValidator struct{}

// ValidateCreate validates a new ModelArtifact.
func (*ModelArtifactValidator) ValidateCreate(_ context.Context, artifact *ModelArtifact) (admission.Warnings, error) {
	return nil, validateModelArtifact(artifact, nil)
}

// ValidateUpdate validates an updated ModelArtifact and prevents content or
// storage mutation after reconciliation starts.
func (*ModelArtifactValidator) ValidateUpdate(_ context.Context, oldArtifact, artifact *ModelArtifact) (admission.Warnings, error) {
	return nil, validateModelArtifact(artifact, oldArtifact)
}

// ValidateDelete permits admission; reference checks and transient cleanup are reconciler responsibilities.
func (*ModelArtifactValidator) ValidateDelete(_ context.Context, _ *ModelArtifact) (admission.Warnings, error) {
	return nil, nil
}

func validateModelArtifact(artifact, oldArtifact *ModelArtifact) error {
	specPath := field.NewPath("spec")
	var allErrs field.ErrorList

	if artifact.Spec.Format != ArtifactFormatGGUF {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("format"), artifact.Spec.Format,
			[]string{string(ArtifactFormatGGUF)}))
	}
	if err := validateCleanRelativePath(artifact.Spec.Entrypoint, specPath.Child("entrypoint"), false, false); err != nil {
		allErrs = append(allErrs, err)
	} else if !strings.EqualFold(path.Ext(artifact.Spec.Entrypoint), ".gguf") {
		allErrs = append(allErrs, field.Invalid(specPath.Child("entrypoint"), artifact.Spec.Entrypoint,
			"GGUF entrypoints must use the .gguf extension"))
	} else if parts := standardShardEntrypoint.FindStringSubmatch(path.Base(artifact.Spec.Entrypoint)); parts != nil && parts[1] != "00001" {
		allErrs = append(allErrs, field.Invalid(specPath.Child("entrypoint"), artifact.Spec.Entrypoint,
			"a standard sharded entrypoint must identify shard 00001"))
	}

	hasHuggingFace := artifact.Spec.Source.HuggingFace != nil
	hasPVC := artifact.Spec.Source.PersistentVolumeClaim != nil
	if hasHuggingFace == hasPVC {
		allErrs = append(allErrs, field.Invalid(specPath.Child("source"), artifact.Spec.Source,
			"exactly one of huggingFace or persistentVolumeClaim must be set"))
	}

	hasCache := artifact.Spec.CacheRef != nil
	if hasCache {
		if err := validateLocalObjectName(artifact.Spec.CacheRef.Name, specPath.Child("cacheRef", "name")); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	if hasHuggingFace {
		allErrs = append(allErrs, validateHuggingFaceSource(artifact.Spec.Source.HuggingFace,
			specPath.Child("source", "huggingFace"))...)
		if !hasCache {
			allErrs = append(allErrs, field.Required(specPath.Child("cacheRef"),
				"cacheRef is required for a Hugging Face source"))
		}
	}

	if hasPVC {
		pvcSource := artifact.Spec.Source.PersistentVolumeClaim
		allErrs = append(allErrs, validatePVCSource(pvcSource,
			specPath.Child("source", "persistentVolumeClaim"))...)
		policy := pvcSource.ImportPolicy
		if policy == "" {
			policy = PVCImportPolicyCopy
		}
		switch policy {
		case PVCImportPolicyCopy:
			if !hasCache {
				allErrs = append(allErrs, field.Required(specPath.Child("cacheRef"),
					"cacheRef is required for Copy import policy"))
			}
		case PVCImportPolicyDirect:
			if hasCache {
				allErrs = append(allErrs, field.Forbidden(specPath.Child("cacheRef"),
					"cacheRef is forbidden for Direct import policy"))
			}
		}
	}

	verificationPath := specPath.Child("verification")
	if digest := artifact.Spec.Verification.ExpectedSHA256; digest != "" && !sha256Pattern.MatchString(digest) {
		allErrs = append(allErrs, field.Invalid(verificationPath.Child("expectedSHA256"), digest,
			"must contain exactly 64 hexadecimal characters"))
	}
	if expectedSize := artifact.Spec.Verification.ExpectedSize; expectedSize != nil && *expectedSize <= 0 {
		allErrs = append(allErrs, field.Invalid(verificationPath.Child("expectedSize"), *expectedSize,
			"must be greater than zero"))
	}

	if oldArtifact != nil && artifactReconciliationStarted(oldArtifact) &&
		!equality.Semantic.DeepEqual(oldArtifact.Spec, artifact.Spec) {
		allErrs = append(allErrs, field.Forbidden(specPath,
			"artifact source, storage, and content fields are immutable after reconciliation starts; create a new ModelArtifact"))
	}

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(GroupVersion.WithKind("ModelArtifact").GroupKind(), artifact.Name, allErrs)
}

func artifactReconciliationStarted(artifact *ModelArtifact) bool {
	return slices.Contains(artifact.Finalizers, ModelArtifactFinalizer) ||
		meta.IsStatusConditionTrue(artifact.Status.Conditions, ModelArtifactConditionReady) ||
		artifact.Status.ValidatedAt != nil || artifact.Status.ArtifactDigest != ""
}

func validateHuggingFaceSource(source *HuggingFaceSource, sourcePath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	repository := source.Repository
	if repository == "" {
		allErrs = append(allErrs, field.Required(sourcePath.Child("repository"), "repository is required"))
	} else if len(repository) > maxHuggingFaceRepositoryLength {
		allErrs = append(allErrs, field.TooLong(sourcePath.Child("repository"), repository,
			maxHuggingFaceRepositoryLength))
	} else {
		parts := strings.Split(repository, "/")
		invalid := len(parts) < 1 || len(parts) > 2 || strings.HasSuffix(strings.ToLower(repository), ".git")
		for _, part := range parts {
			if !huggingFaceRepoComponent.MatchString(part) || strings.Contains(part, "--") || strings.Contains(part, "..") {
				invalid = true
			}
		}
		if invalid {
			allErrs = append(allErrs, field.Invalid(sourcePath.Child("repository"), repository,
				"must be repo_name or namespace/repo_name using Hugging Face repo_id syntax, not a URL"))
		}
	}

	revision := source.Revision
	if revision == "" {
		allErrs = append(allErrs, field.Required(sourcePath.Child("revision"), "revision is required"))
	} else if len(revision) > 255 || strings.TrimSpace(revision) != revision || strings.Contains(revision, "\\") ||
		strings.IndexFunc(revision, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		allErrs = append(allErrs, field.Invalid(sourcePath.Child("revision"), revision,
			"must be at most 255 characters and contain no whitespace, control characters, or backslashes"))
	}

	filesPath := sourcePath.Child("files")
	if len(source.Files) == 0 {
		allErrs = append(allErrs, field.Required(filesPath, "at least one file selector is required"))
	}
	if len(source.Files) > 128 {
		allErrs = append(allErrs, field.TooMany(filesPath, len(source.Files), 128))
	}
	seen := make(map[string]struct{}, len(source.Files))
	for index, selector := range source.Files {
		selectorPath := filesPath.Index(index)
		if _, exists := seen[selector]; exists {
			allErrs = append(allErrs, field.Duplicate(selectorPath, selector))
		} else {
			seen[selector] = struct{}{}
		}
		if err := validateCleanRelativePath(selector, selectorPath, false, true); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	if source.TokenSecretRef != nil {
		secretPath := sourcePath.Child("tokenSecretRef")
		if err := validateLocalObjectName(source.TokenSecretRef.Name, secretPath.Child("name")); err != nil {
			allErrs = append(allErrs, err)
		}
		if source.TokenSecretRef.Key == "" {
			allErrs = append(allErrs, field.Required(secretPath.Child("key"), "key is required"))
		} else if messages := validation.IsConfigMapKey(source.TokenSecretRef.Key); len(messages) > 0 {
			allErrs = append(allErrs, field.Invalid(secretPath.Child("key"), source.TokenSecretRef.Key,
				strings.Join(messages, "; ")))
		}
	}

	return allErrs
}

func validatePVCSource(source *PersistentVolumeClaimSource, sourcePath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if err := validateLocalObjectName(source.ClaimName, sourcePath.Child("claimName")); err != nil {
		allErrs = append(allErrs, err)
	}
	if err := validateCleanRelativePath(source.RootPath, sourcePath.Child("rootPath"), true, false); err != nil {
		allErrs = append(allErrs, err)
	}
	policy := source.ImportPolicy
	if policy == "" {
		policy = PVCImportPolicyCopy
	}
	if policy != PVCImportPolicyCopy && policy != PVCImportPolicyDirect {
		allErrs = append(allErrs, field.NotSupported(sourcePath.Child("importPolicy"), policy,
			[]string{string(PVCImportPolicyCopy), string(PVCImportPolicyDirect)}))
	}
	return allErrs
}

// +kubebuilder:webhook:path=/mutate-kama-tannerburns-github-io-v1alpha1-modelartifact,mutating=true,failurePolicy=fail,sideEffects=None,groups=kama.tannerburns.github.io,resources=modelartifacts,verbs=create;update,versions=v1alpha1,name=mmodelartifact.kama.tannerburns.github.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kama-tannerburns-github-io-v1alpha1-modelartifact,mutating=false,failurePolicy=fail,sideEffects=None,groups=kama.tannerburns.github.io,resources=modelartifacts,verbs=create;update,versions=v1alpha1,name=vmodelartifact.kama.tannerburns.github.io,admissionReviewVersions=v1
