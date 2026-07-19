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

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"strings"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	"github.com/TannerBurns/kama/internal/artifact"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	cacheProbeReason                  = "StorageProbe"
	cacheEventAction                  = "Reconcile"
	defaultStorageClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	betaDefaultStorageClassAnnotation = "storageclass.beta.kubernetes.io/is-default-class"
	cacheDeletionGuardAnnotation      = "kama.tannerburns.github.io/cache-deletion-guard"
	cacheDeletionGuardedAtAnnotation  = "kama.tannerburns.github.io/cache-deletion-guarded-at"
	cacheDeletionQuiescence           = 5 * time.Second
	maxCacheReadyStaleness            = 2 * defaultProbeInterval
	claimPendingReason                = "ClaimPending"
	artifactReferencesRemainReason    = "ArtifactReferencesRemain"
	annotationTrue                    = "true"
)

// ModelCacheReconciler manages durable cache claims and filesystem probes.
type ModelCacheReconciler struct {
	reconcilerBase
	// APIReader bypasses the controller-runtime cache for destructive reference
	// checks. SetupWithManager always supplies the manager's uncached reader;
	// direct unit reconcilers fall back to Client unless they inject one.
	APIReader client.Reader
}

// NewModelCacheReconciler builds a ModelCache reconciler.
func NewModelCacheReconciler(
	kubeClient client.Client,
	scheme *runtime.Scheme,
	recorder events.EventRecorder,
	clientset kubernetes.Interface,
	importer ImporterOptions,
) *ModelCacheReconciler {
	return &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: recorder, Clientset: clientset, Importer: importer,
		ProbeTimeout: defaultProbeInterval,
	}}
}

// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelcaches,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelcaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelcaches/finalizers,verbs=update
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelartifacts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile converges one ModelCache.
func (r *ModelCacheReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var cache kamav1alpha1.ModelCache
	if err := r.Get(ctx, request.NamespacedName, &cache); err != nil {
		if apierrors.IsNotFound(err) {
			deleteModelCacheMetrics(request.Namespace, request.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !cache.DeletionTimestamp.IsZero() {
		deleteModelCacheMetrics(cache.Namespace, cache.Name)
		return r.reconcileDelete(ctx, &cache)
	}
	if !controllerutil.ContainsFinalizer(&cache, kamav1alpha1.ModelCacheFinalizer) {
		controllerutil.AddFinalizer(&cache, kamav1alpha1.ModelCacheFinalizer)
		if err := r.Update(ctx, &cache); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	claimName, managed, err := r.ensureClaim(ctx, &cache)
	if err != nil {
		clearCacheStorageStatus(&cache, "", "")
		return r.cacheFailure(ctx, &cache, kamav1alpha1.ModelCacheConditionStorageUnavailable,
			"ClaimReconcileFailed", err.Error(), time.Minute)
	}
	var claim corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: cache.Namespace, Name: claimName}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			clearCacheStorageStatus(&cache, claimName, "")
			return r.cacheFailure(ctx, &cache, kamav1alpha1.ModelCacheConditionStorageUnavailable,
				"ClaimNotFound", "cache PersistentVolumeClaim does not exist", 10*time.Second)
		}
		return r.cacheFailure(ctx, &cache, kamav1alpha1.ModelCacheConditionStorageUnavailable,
			"ClaimReadFailed", fmt.Sprintf("read cache PersistentVolumeClaim: %v", err), time.Minute)
	}
	if !claim.DeletionTimestamp.IsZero() {
		return r.updateClaimStatus(ctx, &cache, &claim, nil, "ClaimTerminating",
			"cache PersistentVolumeClaim is terminating", 5*time.Second)
	}
	if managed {
		if err := validateManagedCacheClaim(&cache, &claim); err != nil {
			clearCacheStorageStatus(&cache, claimName, "")
			return r.cacheFailure(ctx, &cache, kamav1alpha1.ModelCacheConditionStorageUnavailable,
				"ClaimIdentityConflict", err.Error(), 0)
		}
	}
	if _, guarded := claim.Annotations[cacheDeletionGuardAnnotation]; guarded {
		return r.updateClaimStatus(ctx, &cache, &claim, nil, "ClaimDeletionGuarded",
			"cache PersistentVolumeClaim is guarded for deletion and cannot be adopted", 5*time.Second)
	}
	if claim.Status.Phase == corev1.ClaimLost {
		return r.updateClaimStatus(ctx, &cache, &claim, nil, "ClaimLost",
			"cache PersistentVolumeClaim has lost its bound volume", time.Minute)
	}
	if claim.Status.Phase != corev1.ClaimBound {
		waitsForConsumer, resolveErr := r.claimWaitsForFirstConsumer(ctx, &claim)
		if resolveErr != nil {
			return r.updateClaimStatus(ctx, &cache, &claim, nil, "StorageClassResolutionFailed",
				resolveErr.Error(), time.Minute)
		}
		if waitsForConsumer {
			job, _, err := r.ensureProbeResources(ctx, &cache, &claim)
			if err != nil {
				return r.updateClaimStatus(ctx, &cache, &claim, nil, "ProbeConsumerCreateFailed",
					err.Error(), time.Minute)
			}
			if jobComplete(job) || jobFailed(job) || !job.DeletionTimestamp.IsZero() {
				if job.DeletionTimestamp.IsZero() {
					if err := deleteForeground(ctx, r.Client, job); err != nil {
						return r.updateClaimStatus(ctx, &cache, &claim, nil, "ProbeConsumerRestartFailed",
							err.Error(), time.Minute)
					}
				}
				return r.updateClaimStatus(ctx, &cache, &claim, nil, "ProbeConsumerRestarting",
					"terminal probe consumer is being recreated to continue delayed volume binding", time.Second)
			}
			return r.updateClaimStatus(ctx, &cache, &claim, nil, claimPendingReason,
				"cache PersistentVolumeClaim is waiting for the probe consumer to schedule and bind storage", 5*time.Second)
		}
		return r.updateClaimStatus(ctx, &cache, &claim, nil, claimPendingReason,
			"cache PersistentVolumeClaim is not yet Bound", 10*time.Second)
	}
	identity, err := r.resolveVolume(ctx, &claim)
	if err != nil {
		return r.updateClaimStatus(ctx, &cache, &claim, nil, "InvalidVolume", err.Error(), time.Minute)
	}
	if !hasWritableMode(identity.AccessModes) {
		return r.updateClaimStatus(ctx, &cache, &claim, &identity, "ReadOnlyCache",
			"cache claim requires a writable access mode", 0)
	}
	if cache.Status.LastProbeTime != nil {
		age := time.Since(cache.Status.LastProbeTime.Time)
		if age >= 0 && age < defaultProbeInterval && canPreserveCacheReady(&cache, &claim, identity) {
			publishReadyModelCacheMetrics(&cache)
			return ctrl.Result{RequeueAfter: defaultProbeInterval - age}, nil
		}
	}
	return r.reconcileProbe(ctx, &cache, &claim, identity)
}

func (r *ModelCacheReconciler) claimWaitsForFirstConsumer(
	ctx context.Context,
	claim *corev1.PersistentVolumeClaim,
) (bool, error) {
	storageClass, err := r.resolveClaimStorageClass(ctx, claim)
	if err != nil || storageClass == nil || storageClass.VolumeBindingMode == nil {
		return false, err
	}
	return *storageClass.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer, nil
}

func (r *ModelCacheReconciler) resolveClaimStorageClass(
	ctx context.Context,
	claim *corev1.PersistentVolumeClaim,
) (*storagev1.StorageClass, error) {
	if claim.Spec.StorageClassName != nil {
		if *claim.Spec.StorageClassName == "" {
			return nil, nil
		}
		var storageClass storagev1.StorageClass
		if err := r.Get(ctx, client.ObjectKey{Name: *claim.Spec.StorageClassName}, &storageClass); err != nil {
			return nil, fmt.Errorf("resolve StorageClass %q: %w", *claim.Spec.StorageClassName, err)
		}
		return &storageClass, nil
	}

	var storageClasses storagev1.StorageClassList
	if err := r.List(ctx, &storageClasses); err != nil {
		return nil, fmt.Errorf("list default StorageClasses: %w", err)
	}
	var selected *storagev1.StorageClass
	for index := range storageClasses.Items {
		storageClass := &storageClasses.Items[index]
		if !isDefaultStorageClass(storageClass) {
			continue
		}
		if selected != nil {
			return nil, errors.New("PVC has no resolved storageClassName and the cluster has multiple default StorageClasses")
		}
		selected = storageClass
	}
	return selected, nil
}

func isDefaultStorageClass(storageClass *storagev1.StorageClass) bool {
	for _, annotation := range []string{defaultStorageClassAnnotation, betaDefaultStorageClassAnnotation} {
		if strings.EqualFold(strings.TrimSpace(storageClass.Annotations[annotation]), annotationTrue) {
			return true
		}
	}
	return false
}

func (r *ModelCacheReconciler) ensureClaim(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
) (string, bool, error) {
	if cache.Spec.Storage.ExistingClaim != nil {
		return cache.Spec.Storage.ExistingClaim.Name, false, nil
	}
	if cache.Spec.Storage.ClaimTemplate == nil {
		return "", false, errors.New("claimTemplate or existingClaim is required")
	}
	name := deterministicName(cache.Name+"-cache", string(cache.UID))
	template := cache.Spec.Storage.ClaimTemplate
	labels := make(map[string]string, len(template.Metadata.Labels)+4)
	maps.Copy(labels, template.Metadata.Labels)
	labels[managedByLabel] = kamaName
	labels[componentLabel] = modelCacheComponent
	labels[cacheNameLabel] = boundedLabelValue(cache.Name)
	labels[cacheUIDLabel] = string(cache.UID)
	annotations := make(map[string]string, len(template.Metadata.Annotations))
	for key, value := range template.Metadata.Annotations {
		if key == cacheDeletionGuardAnnotation || key == cacheDeletionGuardedAtAnnotation {
			return "", true, fmt.Errorf("claim template annotation %q is reserved for safe cache deletion", key)
		}
		annotations[key] = value
	}
	volumeMode := template.Spec.VolumeMode
	if volumeMode == "" {
		volumeMode = corev1.PersistentVolumeFilesystem
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cache.Namespace, Labels: labels, Annotations: annotations},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      append([]corev1.PersistentVolumeAccessMode(nil), template.Spec.AccessModes...),
			StorageClassName: template.Spec.StorageClassName,
			VolumeMode:       &volumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: template.Spec.Resources.Requests.DeepCopy(),
			},
		},
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", true, fmt.Errorf("create managed cache claim: %w", err)
	}
	return name, true, nil
}

// validateManagedCacheClaim deliberately checks the full immutable identity
// contract before the controller adopts or deletes an ownerless PVC.
//
//nolint:gocyclo // Keeping the fail-closed contract checks together makes omissions visible during review.
func validateManagedCacheClaim(
	cache *kamav1alpha1.ModelCache,
	claim *corev1.PersistentVolumeClaim,
) error {
	template := cache.Spec.Storage.ClaimTemplate
	if template == nil {
		return errors.New("managed cache has no claim template")
	}
	wantName := deterministicName(cache.Name+"-cache", string(cache.UID))
	if claim.Namespace != cache.Namespace || claim.Name != wantName {
		return fmt.Errorf("managed claim identity is %s/%s, want %s/%s",
			claim.Namespace, claim.Name, cache.Namespace, wantName)
	}
	wantLabels := map[string]string{
		managedByLabel: kamaName,
		componentLabel: modelCacheComponent,
		cacheNameLabel: boundedLabelValue(cache.Name),
		cacheUIDLabel:  string(cache.UID),
	}
	for key, value := range template.Metadata.Labels {
		if _, reserved := wantLabels[key]; !reserved {
			wantLabels[key] = value
		}
	}
	for key, value := range wantLabels {
		if claim.Labels[key] != value {
			return fmt.Errorf("managed claim label %q does not match the ModelCache identity", key)
		}
	}
	for key, value := range template.Metadata.Annotations {
		if key == cacheDeletionGuardAnnotation || key == cacheDeletionGuardedAtAnnotation {
			return fmt.Errorf("claim template annotation %q is reserved for safe cache deletion", key)
		}
		if claim.Annotations[key] != value {
			return fmt.Errorf("managed claim annotation %q does not match the claim template", key)
		}
	}
	if len(claim.OwnerReferences) != 0 {
		return errors.New("managed cache claim must remain ownerless")
	}
	if !reflect.DeepEqual(normalizeAccessModes(claim.Spec.AccessModes), normalizeAccessModes(template.Spec.AccessModes)) {
		return fmt.Errorf("managed claim accessModes %v do not match claim template %v",
			claim.Spec.AccessModes, template.Spec.AccessModes)
	}
	wantVolumeMode := template.Spec.VolumeMode
	if wantVolumeMode == "" {
		wantVolumeMode = corev1.PersistentVolumeFilesystem
	}
	gotVolumeMode := corev1.PersistentVolumeFilesystem
	if claim.Spec.VolumeMode != nil {
		gotVolumeMode = *claim.Spec.VolumeMode
	}
	if gotVolumeMode != wantVolumeMode {
		return fmt.Errorf("managed claim volumeMode %q does not match claim template %q", gotVolumeMode, wantVolumeMode)
	}
	if template.Spec.StorageClassName != nil {
		if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != *template.Spec.StorageClassName {
			return errors.New("managed claim storageClassName does not match claim template")
		}
	} else if claim.Spec.StorageClassName != nil && *claim.Spec.StorageClassName == "" {
		return errors.New("managed claim disables default StorageClass selection contrary to claim template")
	}
	wantStorage, found := template.Spec.Resources.Requests[corev1.ResourceStorage]
	if !found {
		return errors.New("claim template has no storage request")
	}
	gotStorage, found := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if !found || gotStorage.Cmp(wantStorage) < 0 {
		return fmt.Errorf("managed claim storage request is smaller than claim template request %s", wantStorage.String())
	}
	for name := range claim.Spec.Resources.Requests {
		if name != corev1.ResourceStorage {
			return fmt.Errorf("managed claim has unexpected resource request %q", name)
		}
	}
	if len(claim.Spec.Resources.Limits) != 0 {
		return errors.New("managed claim has unexpected resource limits")
	}
	if claim.Spec.Selector != nil || claim.Spec.DataSource != nil || claim.Spec.DataSourceRef != nil {
		return errors.New("managed claim has an unexpected selector or data source")
	}
	if claim.Spec.VolumeAttributesClassName != nil && *claim.Spec.VolumeAttributesClassName != "" {
		return errors.New("managed claim has an unexpected volumeAttributesClassName")
	}
	return nil
}

func (r *ModelCacheReconciler) reconcileProbe(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	claim *corev1.PersistentVolumeClaim,
	identity volumeIdentity,
) (ctrl.Result, error) {
	job, configMap, err := r.ensureProbeResources(ctx, cache, claim)
	if err != nil {
		if canPreserveCacheReady(cache, claim, identity) {
			publishReadyModelCacheMetrics(cache)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return r.updateClaimStatus(ctx, cache, claim, &identity, "ProbeReconcileFailed", err.Error(), time.Minute)
	}
	if !jobComplete(job) && !jobFailed(job) {
		if canPreserveCacheReady(cache, claim, identity) {
			publishReadyModelCacheMetrics(cache)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return r.updateClaimStatus(ctx, cache, claim, &identity, probeRunningReason,
			"filesystem capability probe is running", 5*time.Second)
	}

	operation := deterministicName("probe", string(cache.UID), string(claim.UID))
	result, resultErr := r.readJobResult(ctx, job)
	if resultErr != nil {
		if canPreserveCacheReady(cache, claim, identity) {
			publishReadyModelCacheMetrics(cache)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return r.probeFailure(ctx, cache, claim, identity, job, configMap,
			kamav1alpha1.ModelCacheConditionStorageUnavailable, "ResultUnavailable",
			"filesystem capability probe result is unavailable; recreating safely")
	}
	if result.OperationID != operation || result.Mode != artifact.ModeProbe {
		return r.probeFailure(ctx, cache, claim, identity, job, configMap,
			kamav1alpha1.ModelCacheConditionStorageUnavailable, "ResultIdentityMismatch",
			"filesystem capability probe result does not match the requested operation")
	}
	if !result.Success || result.Probe == nil {
		message := result.Message
		if message == "" {
			message = "filesystem capability probe failed"
		}
		if result.Reason == artifact.ReasonInsufficientStorage {
			return r.probeFailure(ctx, cache, claim, identity, job, configMap,
				kamav1alpha1.ModelCacheConditionInsufficientCapacity, "InsufficientStorage", message)
		}
		return r.probeFailure(ctx, cache, claim, identity, job, configMap,
			kamav1alpha1.ModelCacheConditionStorageUnavailable, "ProbeFailed", message)
	}
	probe := result.Probe
	if err := validateCacheProbeResult(result); err != nil {
		return r.probeFailure(ctx, cache, claim, identity, job, configMap,
			kamav1alpha1.ModelCacheConditionStorageUnavailable, "InvalidProbeResult", err.Error())
	}
	if !probe.Write || !probe.Fsync || !probe.AtomicRename || !probe.DirectoryRename || !probe.Mmap || !probe.Lock {
		return r.updateClaimStatus(ctx, cache, claim, &identity, "UnsupportedFilesystem",
			"cache filesystem did not pass write, fsync, file/directory atomic rename, mmap, and lock checks", 0)
	}
	// Remove transient probe resources before publishing Ready. If cleanup fails,
	// the completed Job remains recoverable and the next reconciliation retries.
	if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
		return ctrl.Result{}, err
	}
	if err := deleteIfPresent(ctx, r.Client, job); err != nil {
		return ctrl.Result{}, err
	}
	before := cache.Status.DeepCopy()
	applyCacheIdentity(cache, claim, identity)
	freeSpace := *resource.NewQuantity(int64(probe.FreeBytes), resource.BinarySI)
	cache.Status.FreeSpace = &freeSpace
	now := metav1.Now()
	cache.Status.LastProbeTime = &now
	cache.Status.ObservedGeneration = cache.Generation
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelCacheConditionReady, Status: metav1.ConditionTrue,
		ObservedGeneration: cache.Generation, Reason: probeSucceededReason,
		Message: "cache claim passed filesystem capability and capacity checks",
	})
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelCacheConditionStorageUnavailable, Status: metav1.ConditionFalse,
		ObservedGeneration: cache.Generation, Reason: "StorageAvailable", Message: "cache storage is available",
	})
	for _, condition := range []string{
		kamav1alpha1.ModelCacheConditionInsufficientCapacity,
		kamav1alpha1.ModelCacheConditionDegraded,
	} {
		meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
			Type: condition, Status: metav1.ConditionFalse, ObservedGeneration: cache.Generation,
			Reason: probeSucceededReason, Message: conditionNotPresent,
		})
	}
	readyTransition := statusConditionTransitioned(
		before.Conditions, kamav1alpha1.ModelCacheConditionReady, metav1.ConditionTrue,
		cache.Generation, probeSucceededReason,
	)
	if !reflect.DeepEqual(before, &cache.Status) {
		if err := r.Status().Update(ctx, cache); err != nil {
			return ctrl.Result{}, err
		}
	}
	if readyTransition {
		r.recordCacheEvent(cache, corev1.EventTypeNormal, cacheProbeReason,
			"Cache filesystem probe succeeded")
	}
	modelCacheReady.WithLabelValues(cache.Namespace, cache.Name).Set(1)
	modelCacheCapacityBytes.WithLabelValues(cache.Namespace, cache.Name).Set(float64(probe.CapacityBytes))
	modelCacheFreeBytes.WithLabelValues(cache.Namespace, cache.Name).Set(float64(probe.FreeBytes))
	cacheProbeOperations.WithLabelValues("success", "").Inc()
	return ctrl.Result{RequeueAfter: defaultProbeInterval}, nil
}

func validateCacheProbeResult(result artifact.Result) error {
	probe := result.Probe
	if probe == nil || probe.CapacityBytes == 0 || probe.CapacityBytes > uint64(math.MaxInt64) ||
		probe.FreeBytes > uint64(math.MaxInt64) || probe.FreeBytes > probe.CapacityBytes ||
		result.Manifest != nil || result.GGUF != nil || result.ArtifactDigest != "" ||
		result.ResolvedRevision != "" || result.PublishedPath != "" || result.BytesTransferred != 0 {
		return errors.New("filesystem capability probe returned inconsistent or out-of-range data")
	}
	return nil
}

func (r *ModelCacheReconciler) ensureProbeResources(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	claim *corev1.PersistentVolumeClaim,
) (*batchv1.Job, *corev1.ConfigMap, error) {
	operation := deterministicName("probe", string(cache.UID), string(claim.UID))
	configName := deterministicName(cache.Name+"-probe-config", operation)
	jobName := deterministicName(cache.Name+"-probe", operation)
	probeSpec := artifact.Spec{
		SchemaVersion: artifact.SchemaVersion,
		Mode:          artifact.ModeProbe,
		OperationID:   operation,
		Probe:         &artifact.ProbeSpec{Root: cacheMountPath},
	}
	configMap, err := newSpecConfigMap(cache, r.Scheme, configName, probeSpec)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureObject(ctx, r.Client, configMap); err != nil {
		return nil, nil, fmt.Errorf("create cache probe config: %w", err)
	}
	job, err := newImportJob(r.Scheme, r.Importer, importJobOptions{
		Owner: cache, Name: jobName, ConfigMapName: configName, CacheClaim: claim.Name,
		OperationID: operation, ActiveTimeout: r.ProbeTimeout,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := ensureObject(ctx, r.Client, job); err != nil {
		return nil, nil, fmt.Errorf("create cache probe Job: %w", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
		return nil, nil, err
	}
	return job, configMap, nil
}

func canPreserveCacheReady(
	cache *kamav1alpha1.ModelCache,
	claim *corev1.PersistentVolumeClaim,
	identity volumeIdentity,
) bool {
	if cache.Status.LastProbeTime == nil {
		return false
	}
	age := time.Since(cache.Status.LastProbeTime.Time)
	return age >= 0 && age <= maxCacheReadyStaleness &&
		cache.Status.ObservedGeneration == cache.Generation &&
		cacheStatusMatchesVolumeIdentity(&cache.Status, claim, identity) &&
		meta.IsStatusConditionTrue(cache.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
}

func cacheStatusMatchesVolumeIdentity(
	status *kamav1alpha1.ModelCacheStatus,
	claim *corev1.PersistentVolumeClaim,
	identity volumeIdentity,
) bool {
	if status.ClaimName != claim.Name || status.ClaimUID == "" || status.ClaimUID != claim.UID ||
		!reflect.DeepEqual(status.AccessModes, identity.AccessModes) ||
		status.VolumeMode != identity.VolumeMode || status.MountScope != identity.MountScope ||
		status.StorageClassName != identity.StorageClassName || status.VolumeName != identity.VolumeName ||
		status.VolumeUID != identity.VolumeUID || !reflect.DeepEqual(status.NodeAffinity, identity.NodeAffinity) {
		return false
	}
	if status.Capacity == nil || identity.Capacity == nil {
		return status.Capacity == nil && identity.Capacity == nil
	}
	return status.Capacity.Cmp(*identity.Capacity) == 0
}

func (r *ModelCacheReconciler) probeFailure(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	claim *corev1.PersistentVolumeClaim,
	identity volumeIdentity,
	job *batchv1.Job,
	configMap *corev1.ConfigMap,
	condition, reason, message string,
) (ctrl.Result, error) {
	const retry = time.Minute
	observedType := kamav1alpha1.ModelCacheConditionReady
	observedStatus := metav1.ConditionFalse
	if condition != kamav1alpha1.ModelCacheConditionStorageUnavailable {
		observedType = condition
		observedStatus = metav1.ConditionTrue
	}
	current := meta.FindStatusCondition(cache.Status.Conditions, observedType)
	alreadyReported := current != nil && current.Status == observedStatus &&
		current.ObservedGeneration == cache.Generation && current.Reason == reason
	if alreadyReported {
		remaining := retry - time.Since(current.LastTransitionTime.Time)
		if remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
		if err := deleteIfPresent(ctx, r.Client, job); err != nil {
			return ctrl.Result{}, err
		}
		if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	cacheProbeOperations.WithLabelValues("failure", reason).Inc()

	if condition == kamav1alpha1.ModelCacheConditionInsufficientCapacity {
		applyCacheIdentity(cache, claim, identity)
		return r.cacheFailure(ctx, cache, condition, reason, message, retry)
	}
	return r.updateClaimStatus(ctx, cache, claim, &identity, reason, message, retry)
}

func (r *ModelCacheReconciler) updateClaimStatus(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	claim *corev1.PersistentVolumeClaim,
	identity *volumeIdentity,
	reason, message string,
	requeue time.Duration,
) (ctrl.Result, error) {
	message = sanitizeConditionMessage(message)
	before := cache.Status.DeepCopy()
	failureTransition := reason != probeRunningReason && statusConditionTransitioned(
		before.Conditions, kamav1alpha1.ModelCacheConditionReady, metav1.ConditionFalse,
		cache.Generation, reason,
	)
	if identity != nil {
		if !cacheStatusMatchesVolumeIdentity(&cache.Status, claim, *identity) {
			clearCacheStorageStatus(cache, claim.Name, claim.UID)
		}
		applyCacheIdentity(cache, claim, *identity)
	} else {
		clearCacheStorageStatus(cache, claim.Name, claim.UID)
	}
	cache.Status.ObservedGeneration = cache.Generation
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelCacheConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: cache.Generation, Reason: reason, Message: message,
	})
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelCacheConditionStorageUnavailable, Status: metav1.ConditionTrue,
		ObservedGeneration: cache.Generation, Reason: reason, Message: message,
	})
	for _, condition := range []string{
		kamav1alpha1.ModelCacheConditionInsufficientCapacity,
		kamav1alpha1.ModelCacheConditionDegraded,
	} {
		meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
			Type: condition, Status: metav1.ConditionFalse,
			ObservedGeneration: cache.Generation, Reason: reason, Message: conditionNotPresent,
		})
	}
	if !reflect.DeepEqual(before, &cache.Status) {
		if err := r.Status().Update(ctx, cache); err != nil {
			return ctrl.Result{}, err
		}
	}
	if failureTransition {
		r.recordCacheEvent(cache, corev1.EventTypeWarning, reason, message)
	}
	setModelCacheReadyMetric(cache, 0)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *ModelCacheReconciler) cacheFailure(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	condition, reason, message string,
	requeue time.Duration,
) (ctrl.Result, error) {
	message = sanitizeConditionMessage(message)
	before := cache.Status.DeepCopy()
	failureTransition := statusConditionTransitioned(
		before.Conditions, condition, metav1.ConditionTrue, cache.Generation, reason,
	)
	cache.Status.ObservedGeneration = cache.Generation
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelCacheConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: cache.Generation, Reason: reason, Message: message,
	})
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type: condition, Status: metav1.ConditionTrue,
		ObservedGeneration: cache.Generation, Reason: reason, Message: message,
	})
	for _, other := range []string{
		kamav1alpha1.ModelCacheConditionStorageUnavailable,
		kamav1alpha1.ModelCacheConditionInsufficientCapacity,
		kamav1alpha1.ModelCacheConditionDegraded,
	} {
		if other == condition {
			continue
		}
		meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
			Type: other, Status: metav1.ConditionFalse,
			ObservedGeneration: cache.Generation, Reason: reason, Message: conditionNotPresent,
		})
	}
	if !reflect.DeepEqual(before, &cache.Status) {
		if err := r.Status().Update(ctx, cache); err != nil {
			return ctrl.Result{}, err
		}
	}
	if failureTransition {
		r.recordCacheEvent(cache, corev1.EventTypeWarning, reason, message)
	}
	setModelCacheReadyMetric(cache, 0)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func statusConditionTransitioned(
	conditions []metav1.Condition,
	conditionType string,
	status metav1.ConditionStatus,
	generation int64,
	reason string,
) bool {
	condition := meta.FindStatusCondition(conditions, conditionType)
	return condition == nil || condition.Status != status || condition.ObservedGeneration != generation ||
		condition.Reason != reason
}

func (r *ModelCacheReconciler) recordCacheEvent(
	cache *kamav1alpha1.ModelCache,
	eventType, reason, note string,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(cache, nil, eventType, reason, cacheEventAction, "%s", note)
}

func setModelCacheReadyMetric(cache *kamav1alpha1.ModelCache, value float64) {
	if !cache.DeletionTimestamp.IsZero() {
		deleteModelCacheMetrics(cache.Namespace, cache.Name)
		return
	}
	modelCacheReady.WithLabelValues(cache.Namespace, cache.Name).Set(value)
	if cache.Status.Capacity == nil {
		modelCacheCapacityBytes.DeleteLabelValues(cache.Namespace, cache.Name)
	}
	if cache.Status.FreeSpace == nil {
		modelCacheFreeBytes.DeleteLabelValues(cache.Namespace, cache.Name)
	}
}

func publishReadyModelCacheMetrics(cache *kamav1alpha1.ModelCache) {
	if !cache.DeletionTimestamp.IsZero() {
		deleteModelCacheMetrics(cache.Namespace, cache.Name)
		return
	}
	modelCacheReady.WithLabelValues(cache.Namespace, cache.Name).Set(1)
	if cache.Status.Capacity != nil {
		modelCacheCapacityBytes.WithLabelValues(cache.Namespace, cache.Name).
			Set(float64(cache.Status.Capacity.Value()))
	} else {
		modelCacheCapacityBytes.DeleteLabelValues(cache.Namespace, cache.Name)
	}
	if cache.Status.FreeSpace != nil {
		modelCacheFreeBytes.WithLabelValues(cache.Namespace, cache.Name).
			Set(float64(cache.Status.FreeSpace.Value()))
	} else {
		modelCacheFreeBytes.DeleteLabelValues(cache.Namespace, cache.Name)
	}
}

func deleteModelCacheMetrics(namespace, name string) {
	modelCacheReady.DeleteLabelValues(namespace, name)
	modelCacheCapacityBytes.DeleteLabelValues(namespace, name)
	modelCacheFreeBytes.DeleteLabelValues(namespace, name)
}

func clearCacheStorageStatus(
	cache *kamav1alpha1.ModelCache,
	claimName string,
	claimUID types.UID,
) {
	cache.Status.ClaimName = claimName
	cache.Status.ClaimUID = claimUID
	cache.Status.Capacity = nil
	cache.Status.FreeSpace = nil
	cache.Status.AccessModes = nil
	cache.Status.VolumeMode = ""
	cache.Status.MountScope = ""
	cache.Status.StorageClassName = ""
	cache.Status.VolumeName = ""
	cache.Status.VolumeUID = ""
	cache.Status.NodeAffinity = nil
	cache.Status.LastProbeTime = nil
}

func applyCacheIdentity(cache *kamav1alpha1.ModelCache, claim *corev1.PersistentVolumeClaim, identity volumeIdentity) {
	cache.Status.ClaimName = claim.Name
	cache.Status.ClaimUID = claim.UID
	cache.Status.AccessModes = append([]corev1.PersistentVolumeAccessMode(nil), identity.AccessModes...)
	cache.Status.VolumeMode = identity.VolumeMode
	cache.Status.MountScope = identity.MountScope
	cache.Status.StorageClassName = identity.StorageClassName
	cache.Status.VolumeName = identity.VolumeName
	cache.Status.VolumeUID = identity.VolumeUID
	cache.Status.NodeAffinity = identity.NodeAffinity.DeepCopy()
	if identity.Capacity != nil {
		capacity := identity.Capacity.DeepCopy()
		cache.Status.Capacity = &capacity
	} else {
		cache.Status.Capacity = nil
	}
}

// reconcileDelete coordinates transient cleanup, live reference scans, a PVC
// deletion guard, and the retention-policy decision as one fail-closed lifecycle.
//
//nolint:gocyclo // The lifecycle branches intentionally remain explicit and auditable.
func (r *ModelCacheReconciler) reconcileDelete(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cache, kamav1alpha1.ModelCacheFinalizer) {
		return ctrl.Result{}, nil
	}
	ready := meta.FindStatusCondition(cache.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.ObservedGeneration != cache.Generation {
		if _, err := r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
			"CacheTerminating", "ModelCache is terminating and cannot accept new artifact references", 0); err != nil {
			return ctrl.Result{}, err
		}
	}
	var probeJobs batchv1.JobList
	if err := r.List(ctx, &probeJobs, client.InNamespace(cache.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	probeJobsRemain := false
	for index := range probeJobs.Items {
		job := &probeJobs.Items[index]
		if !hasControllerOwner(job.OwnerReferences, cache.UID, "ModelCache") {
			continue
		}
		probeJobsRemain = true
		if job.DeletionTimestamp.IsZero() {
			if err := deleteForeground(ctx, r.Client, job); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if probeJobsRemain {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	var probeConfigs corev1.ConfigMapList
	if err := r.List(ctx, &probeConfigs, client.InNamespace(cache.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for index := range probeConfigs.Items {
		configMap := &probeConfigs.Items[index]
		if hasControllerOwner(configMap.OwnerReferences, cache.UID, "ModelCache") {
			if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	reader := r.referenceReader()
	var artifacts kamav1alpha1.ModelArtifactList
	if err := reader.List(ctx, &artifacts, client.InNamespace(cache.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for index := range artifacts.Items {
		item := &artifacts.Items[index]
		if item.Spec.CacheRef != nil && item.Spec.CacheRef.Name == cache.Name {
			return r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
				artifactReferencesRemainReason,
				"cache deletion is blocked until every referenced ModelArtifact is fully removed", 15*time.Second)
		}
	}
	if cache.Spec.Storage.ClaimTemplate != nil && cache.Spec.RetentionPolicy == kamav1alpha1.RetentionPolicyDelete {
		claimName := deterministicName(cache.Name+"-cache", string(cache.UID))
		var claim corev1.PersistentVolumeClaim
		if err := reader.Get(ctx, types.NamespacedName{Namespace: cache.Namespace, Name: claimName}, &claim); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else {
			if !claim.DeletionTimestamp.IsZero() {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if err := validateManagedCacheClaim(cache, &claim); err != nil {
				return r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
					"ClaimIdentityConflict", fmt.Sprintf("refusing to delete managed cache claim: %v", err), 0)
			}
			guardedAt, guardAdded, err := r.ensureClaimDeletionGuard(ctx, cache, &claim)
			if err != nil {
				return r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
					"ClaimDeletionGuardConflict", err.Error(), 0)
			}
			if guardAdded {
				return ctrl.Result{RequeueAfter: cacheDeletionQuiescence}, nil
			}
			if remaining := time.Until(guardedAt.Add(cacheDeletionQuiescence)); remaining > 0 {
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
			blocked, reason, message, err := r.managedClaimDeletionBlocked(ctx, cache, claimName)
			if err != nil {
				return ctrl.Result{}, err
			}
			if blocked {
				return r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
					reason, message, 15*time.Second)
			}
			confirmedClaim, err := r.revalidateManagedClaimDeletion(ctx, cache, &claim, guardedAt)
			if apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if err != nil {
				return r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
					"ClaimDeletionGuardChanged", err.Error(), 5*time.Second)
			}
			uid := confirmedClaim.UID
			resourceVersion := confirmedClaim.ResourceVersion
			if err := r.Delete(ctx, confirmedClaim, &client.DeleteOptions{Preconditions: &metav1.Preconditions{
				UID: &uid, ResourceVersion: &resourceVersion,
			}}); err != nil && !apierrors.IsNotFound(err) {
				return r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
					"ClaimDeleteFailed", fmt.Sprintf("delete managed cache claim: %v", err), time.Minute)
			}
			r.recordCacheEvent(cache, corev1.EventTypeNormal, "ManagedClaimDeletionRequested",
				"Delete retention policy selected; the guarded, unreferenced managed claim is being deleted")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}
	if cache.Spec.Storage.ClaimTemplate != nil && cache.Spec.RetentionPolicy != kamav1alpha1.RetentionPolicyDelete {
		guardCleared, err := r.clearRetainedManagedClaimDeletionGuard(ctx, cache)
		if err != nil {
			return r.cacheFailure(ctx, cache, kamav1alpha1.ModelCacheConditionDegraded,
				"RetainedClaimGuardCleanupFailed", err.Error(), 5*time.Second)
		}
		if guardCleared {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}
	retentionReason := "ManagedClaimDeletionComplete"
	retentionMessage := "Delete retention policy selected; the managed cache claim is absent"
	if cache.Spec.Storage.ClaimTemplate == nil {
		retentionReason = "AdoptedClaimRetained"
		retentionMessage = "The adopted cache claim is retained"
	} else if cache.Spec.RetentionPolicy != kamav1alpha1.RetentionPolicyDelete {
		retentionReason = "ManagedClaimRetained"
		retentionMessage = "Retain retention policy selected; the managed cache claim is retained"
	}
	controllerutil.RemoveFinalizer(cache, kamav1alpha1.ModelCacheFinalizer)
	if err := r.Update(ctx, cache); err != nil {
		return ctrl.Result{}, err
	}
	r.recordCacheEvent(cache, corev1.EventTypeNormal, retentionReason, retentionMessage)
	return ctrl.Result{}, nil
}

func (r *ModelCacheReconciler) ensureClaimDeletionGuard(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	claim *corev1.PersistentVolumeClaim,
) (time.Time, bool, error) {
	guardOwner, guarded := claim.Annotations[cacheDeletionGuardAnnotation]
	if guarded && guardOwner != string(cache.UID) {
		return time.Time{}, false, fmt.Errorf("claim deletion guard belongs to another ModelCache identity")
	}
	if guarded {
		guardedAt, err := time.Parse(time.RFC3339Nano, claim.Annotations[cacheDeletionGuardedAtAnnotation])
		guardStartedAfterDeletion := cache.DeletionTimestamp.IsZero() ||
			!guardedAt.Before(cache.DeletionTimestamp.Time)
		if err == nil && !guardedAt.After(time.Now()) && guardStartedAfterDeletion {
			return guardedAt, false, nil
		}
	}

	if claim.Annotations == nil {
		claim.Annotations = make(map[string]string, 2)
	}
	guardedAt := time.Now().UTC()
	claim.Annotations[cacheDeletionGuardAnnotation] = string(cache.UID)
	claim.Annotations[cacheDeletionGuardedAtAnnotation] = guardedAt.Format(time.RFC3339Nano)
	if err := r.Update(ctx, claim); err != nil {
		return time.Time{}, false, fmt.Errorf("apply managed claim deletion guard: %w", err)
	}
	r.recordCacheEvent(cache, corev1.EventTypeNormal, "ManagedClaimDeletionGuarded",
		"Delete retention policy selected; guarding the managed claim before the final live reference check")
	return guardedAt, true, nil
}

func (r *ModelCacheReconciler) revalidateManagedClaimDeletion(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	original *corev1.PersistentVolumeClaim,
	guardedAt time.Time,
) (*corev1.PersistentVolumeClaim, error) {
	var claim corev1.PersistentVolumeClaim
	if err := r.referenceReader().Get(ctx, client.ObjectKeyFromObject(original), &claim); err != nil {
		return nil, err
	}
	if claim.UID == "" || claim.UID != original.UID {
		return nil, errors.New("managed cache claim identity changed during final deletion checks")
	}
	if !claim.DeletionTimestamp.IsZero() {
		return nil, errors.New("managed cache claim began terminating during final deletion checks")
	}
	if err := validateManagedCacheClaim(cache, &claim); err != nil {
		return nil, fmt.Errorf("revalidate managed cache claim: %w", err)
	}
	guardOwner, guarded := claim.Annotations[cacheDeletionGuardAnnotation]
	if !guarded || guardOwner != string(cache.UID) {
		return nil, errors.New("managed cache claim deletion guard changed during final deletion checks")
	}
	confirmedAt, err := time.Parse(time.RFC3339Nano, claim.Annotations[cacheDeletionGuardedAtAnnotation])
	if err != nil || !confirmedAt.Equal(guardedAt) {
		return nil, errors.New("managed cache claim deletion guard timestamp changed during final deletion checks")
	}
	if !cache.DeletionTimestamp.IsZero() && confirmedAt.Before(cache.DeletionTimestamp.Time) {
		return nil, errors.New("managed cache claim deletion guard predates cache deletion")
	}
	if time.Now().Before(confirmedAt.Add(cacheDeletionQuiescence)) {
		return nil, errors.New("managed cache claim deletion guard has not completed its quiescence period")
	}
	return &claim, nil
}

func (r *ModelCacheReconciler) clearRetainedManagedClaimDeletionGuard(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
) (bool, error) {
	claimName := deterministicName(cache.Name+"-cache", string(cache.UID))
	var claim corev1.PersistentVolumeClaim
	if err := r.referenceReader().Get(ctx, types.NamespacedName{
		Namespace: cache.Namespace, Name: claimName,
	}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	guardOwner, guarded := claim.Annotations[cacheDeletionGuardAnnotation]
	if !guarded || guardOwner != string(cache.UID) || !claim.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if err := validateManagedCacheClaim(cache, &claim); err != nil {
		return false, fmt.Errorf("revalidate retained managed cache claim: %w", err)
	}
	delete(claim.Annotations, cacheDeletionGuardAnnotation)
	delete(claim.Annotations, cacheDeletionGuardedAtAnnotation)
	if err := r.Update(ctx, &claim); err != nil {
		return false, fmt.Errorf("remove retained managed claim deletion guard: %w", err)
	}
	r.recordCacheEvent(cache, corev1.EventTypeNormal, "ManagedClaimDeletionGuardRemoved",
		"Retain retention policy selected; the managed claim deletion guard was removed")
	return true, nil
}

func (r *ModelCacheReconciler) managedClaimDeletionBlocked(
	ctx context.Context,
	cache *kamav1alpha1.ModelCache,
	claimName string,
) (bool, string, string, error) {
	reader := r.referenceReader()
	var caches kamav1alpha1.ModelCacheList
	if err := reader.List(ctx, &caches, client.InNamespace(cache.Namespace)); err != nil {
		return false, "", "", err
	}
	for index := range caches.Items {
		other := &caches.Items[index]
		if other.Name == cache.Name {
			continue
		}
		if other.Spec.Storage.ExistingClaim != nil && other.Spec.Storage.ExistingClaim.Name == claimName {
			return true, "ClaimAdoptedByAnotherCache",
				"managed claim deletion is blocked while another ModelCache adopts it", nil
		}
	}
	var artifacts kamav1alpha1.ModelArtifactList
	if err := reader.List(ctx, &artifacts, client.InNamespace(cache.Namespace)); err != nil {
		return false, "", "", err
	}
	for index := range artifacts.Items {
		modelArtifact := &artifacts.Items[index]
		if modelArtifact.Spec.CacheRef != nil && modelArtifact.Spec.CacheRef.Name == cache.Name {
			return true, artifactReferencesRemainReason,
				"managed claim deletion is blocked while a ModelArtifact references the ModelCache", nil
		}
		claimSource := modelArtifact.Spec.Source.PersistentVolumeClaim
		if claimSource != nil && claimSource.ClaimName == claimName {
			return true, artifactClaimRefsReason,
				"managed claim deletion is blocked while a ModelArtifact PVC source references it", nil
		}
		if modelArtifact.Status.Location != nil && modelArtifact.Status.Location.ClaimName == claimName {
			return true, artifactClaimRefsReason,
				"managed claim deletion is blocked while a ModelArtifact status location references it", nil
		}
	}
	return false, "", "", nil
}

func (r *ModelCacheReconciler) referenceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// SetupWithManager registers the ModelCache controller and its watches.
func (r *ModelCacheReconciler) SetupWithManager(manager ctrl.Manager) error {
	if err := r.validate(); err != nil {
		return err
	}
	r.APIReader = manager.GetAPIReader()
	registerControllerMetrics()
	return ctrl.NewControllerManagedBy(manager).
		For(&kamav1alpha1.ModelCache{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, object client.Object) []reconcile.Request {
				var caches kamav1alpha1.ModelCacheList
				if err := r.List(ctx, &caches, client.InNamespace(object.GetNamespace())); err != nil {
					return nil
				}
				requests := make([]reconcile.Request, 0, 1)
				for index := range caches.Items {
					cache := &caches.Items[index]
					if cache.Status.ClaimName == object.GetName() ||
						(cache.Spec.Storage.ExistingClaim != nil && cache.Spec.Storage.ExistingClaim.Name == object.GetName()) {
						requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cache)})
					}
				}
				return requests
			},
		)).
		Complete(r)
}
