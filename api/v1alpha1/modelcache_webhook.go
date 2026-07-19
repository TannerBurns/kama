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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	modelCacheDeletionGuardAnnotation     = "kama.tannerburns.github.io/cache-deletion-guard"
	modelCacheDeletionGuardedAtAnnotation = "kama.tannerburns.github.io/cache-deletion-guarded-at"
)

var (
	_ admission.Defaulter[*ModelCache] = &ModelCacheDefaulter{}
	_ admission.Validator[*ModelCache] = &ModelCacheValidator{}
)

// SetupModelCacheWebhookWithManager registers ModelCache defaulting and validation webhooks.
func SetupModelCacheWebhookWithManager(mgr manager.Manager) error {
	return builder.WebhookManagedBy(mgr, &ModelCache{}).
		WithDefaulter(&ModelCacheDefaulter{}).
		WithValidator(&ModelCacheValidator{}).
		Complete()
}

// ModelCacheDefaulter applies API defaults without consulting cluster state.
// +kubebuilder:object:generate=false
type ModelCacheDefaulter struct{}

// Default sets Retain lifecycle and Filesystem volume semantics.
func (*ModelCacheDefaulter) Default(_ context.Context, cache *ModelCache) error {
	if cache.Spec.RetentionPolicy == "" {
		cache.Spec.RetentionPolicy = RetentionPolicyRetain
	}
	if cache.Spec.Storage.ClaimTemplate != nil && cache.Spec.Storage.ClaimTemplate.Spec.VolumeMode == "" {
		cache.Spec.Storage.ClaimTemplate.Spec.VolumeMode = corev1.PersistentVolumeFilesystem
	}
	return nil
}

// ModelCacheValidator enforces the static cache storage contract.
// +kubebuilder:object:generate=false
type ModelCacheValidator struct{}

// ValidateCreate validates a new ModelCache.
func (*ModelCacheValidator) ValidateCreate(_ context.Context, cache *ModelCache) (admission.Warnings, error) {
	return nil, validateModelCache(cache)
}

// ValidateUpdate validates an updated ModelCache.
func (*ModelCacheValidator) ValidateUpdate(_ context.Context, oldCache, cache *ModelCache) (admission.Warnings, error) {
	return nil, validateModelCacheUpdate(oldCache, cache)
}

// ValidateDelete permits deletion; reference and retention checks are reconciler responsibilities.
func (*ModelCacheValidator) ValidateDelete(_ context.Context, _ *ModelCache) (admission.Warnings, error) {
	return nil, nil
}

func validateModelCache(cache *ModelCache) error {
	specPath := field.NewPath("spec")
	storagePath := specPath.Child("storage")
	var allErrs field.ErrorList

	hasExisting := cache.Spec.Storage.ExistingClaim != nil
	hasTemplate := cache.Spec.Storage.ClaimTemplate != nil
	if hasExisting == hasTemplate {
		allErrs = append(allErrs, field.Invalid(storagePath, cache.Spec.Storage,
			"exactly one of existingClaim or claimTemplate must be set"))
	}

	retention := cache.Spec.RetentionPolicy
	if retention == "" {
		retention = RetentionPolicyRetain
	}
	if retention != RetentionPolicyRetain && retention != RetentionPolicyDelete {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("retentionPolicy"), retention,
			[]string{string(RetentionPolicyRetain), string(RetentionPolicyDelete)}))
	}

	if hasExisting {
		if err := validateLocalObjectName(cache.Spec.Storage.ExistingClaim.Name,
			storagePath.Child("existingClaim", "name")); err != nil {
			allErrs = append(allErrs, err)
		}
		if retention != RetentionPolicyRetain {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("retentionPolicy"),
				"adopted claims must use Retain"))
		}
	}

	if hasTemplate {
		allErrs = append(allErrs, validateClaimTemplate(cache.Spec.Storage.ClaimTemplate,
			storagePath.Child("claimTemplate"))...)
	}

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(GroupVersion.WithKind("ModelCache").GroupKind(), cache.Name, allErrs)
}

func validateModelCacheUpdate(oldCache, cache *ModelCache) error {
	if err := validateModelCache(cache); err != nil {
		return err
	}
	var allErrs field.ErrorList
	if oldCache != nil {
		if !equality.Semantic.DeepEqual(oldCache.Spec.Storage, cache.Spec.Storage) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "storage"),
				"cache storage identity is immutable; create a new ModelCache"))
		}
		if !oldCache.DeletionTimestamp.IsZero() &&
			effectiveRetentionPolicy(oldCache.Spec.RetentionPolicy) != effectiveRetentionPolicy(cache.Spec.RetentionPolicy) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "retentionPolicy"),
				"retentionPolicy is immutable after cache deletion begins"))
		}
	}
	if len(allErrs) != 0 {
		return apierrors.NewInvalid(GroupVersion.WithKind("ModelCache").GroupKind(), cache.Name, allErrs)
	}
	return nil
}

func effectiveRetentionPolicy(retention RetentionPolicy) RetentionPolicy {
	if retention == "" {
		return RetentionPolicyRetain
	}
	return retention
}

func validateClaimTemplate(template *ModelCacheClaimTemplate, templatePath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	for key, value := range template.Metadata.Labels {
		keyPath := templatePath.Child("metadata", "labels").Key(key)
		if err := validateMetadataKey(key, keyPath); err != nil {
			allErrs = append(allErrs, err)
		}
		if messages := validation.IsValidLabelValue(value); len(messages) > 0 {
			allErrs = append(allErrs, field.Invalid(keyPath, value, strings.Join(messages, "; ")))
		}
	}
	for key := range template.Metadata.Annotations {
		keyPath := templatePath.Child("metadata", "annotations").Key(key)
		if key == modelCacheDeletionGuardAnnotation || key == modelCacheDeletionGuardedAtAnnotation {
			allErrs = append(allErrs, field.Forbidden(keyPath,
				"annotation is reserved for the ModelCache deletion protocol"))
		}
		if err := validateMetadataKey(key, keyPath); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	specPath := templatePath.Child("spec")
	if template.Spec.StorageClassName != nil && *template.Spec.StorageClassName != "" {
		if messages := validation.IsDNS1123Subdomain(*template.Spec.StorageClassName); len(messages) > 0 {
			allErrs = append(allErrs, field.Invalid(specPath.Child("storageClassName"),
				*template.Spec.StorageClassName, strings.Join(messages, "; ")))
		}
	}

	if len(template.Spec.AccessModes) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("accessModes"),
			"at least one writable access mode is required"))
	}
	if len(template.Spec.AccessModes) > 4 {
		allErrs = append(allErrs, field.TooMany(specPath.Child("accessModes"), len(template.Spec.AccessModes), 4))
	}
	writable := false
	seen := make(map[corev1.PersistentVolumeAccessMode]struct{}, len(template.Spec.AccessModes))
	for index, mode := range template.Spec.AccessModes {
		modePath := specPath.Child("accessModes").Index(index)
		if _, exists := seen[mode]; exists {
			allErrs = append(allErrs, field.Duplicate(modePath, mode))
			continue
		}
		seen[mode] = struct{}{}
		switch mode {
		case corev1.ReadWriteOnce, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
			writable = true
		case corev1.ReadOnlyMany:
			// ROX is a valid Direct artifact source, but is not sufficient for a writable cache.
		default:
			allErrs = append(allErrs, field.NotSupported(modePath, mode, []string{
				string(corev1.ReadWriteOnce), string(corev1.ReadOnlyMany),
				string(corev1.ReadWriteMany), string(corev1.ReadWriteOncePod),
			}))
		}
	}
	if len(template.Spec.AccessModes) > 0 && !writable {
		allErrs = append(allErrs, field.Invalid(specPath.Child("accessModes"), template.Spec.AccessModes,
			"a managed cache requires ReadWriteOnce, ReadWriteMany, or ReadWriteOncePod"))
	}

	volumeMode := template.Spec.VolumeMode
	if volumeMode == "" {
		volumeMode = corev1.PersistentVolumeFilesystem
	}
	if volumeMode != corev1.PersistentVolumeFilesystem {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("volumeMode"), volumeMode,
			[]string{string(corev1.PersistentVolumeFilesystem)}))
	}

	requestsPath := specPath.Child("resources", "requests")
	storage, present := template.Spec.Resources.Requests[corev1.ResourceStorage]
	if !present {
		allErrs = append(allErrs, field.Required(requestsPath.Key(string(corev1.ResourceStorage)),
			"a positive storage request is required"))
	} else if storage.Sign() <= 0 {
		allErrs = append(allErrs, field.Invalid(requestsPath.Key(string(corev1.ResourceStorage)),
			storage.String(), "must be greater than zero"))
	}
	for resourceName := range template.Spec.Resources.Requests {
		if resourceName != corev1.ResourceStorage {
			allErrs = append(allErrs, field.Forbidden(requestsPath.Key(string(resourceName)),
				fmt.Sprintf("only %q may be requested", corev1.ResourceStorage)))
		}
	}

	return allErrs
}

// +kubebuilder:webhook:path=/mutate-kama-tannerburns-github-io-v1alpha1-modelcache,mutating=true,failurePolicy=fail,sideEffects=None,groups=kama.tannerburns.github.io,resources=modelcaches,verbs=create;update,versions=v1alpha1,name=mmodelcache.kama.tannerburns.github.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kama-tannerburns-github-io-v1alpha1-modelcache,mutating=false,failurePolicy=fail,sideEffects=None,groups=kama.tannerburns.github.io,resources=modelcaches,verbs=create;update,versions=v1alpha1,name=vmodelcache.kama.tannerburns.github.io,admissionReviewVersions=v1
