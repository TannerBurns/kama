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
	"reflect"
	"strings"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	"github.com/TannerBurns/kama/internal/artifact"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	leaseDuration            = 90 * time.Second
	activeImportRequeue      = 20 * time.Second
	resultRetryDelay         = 5 * time.Second
	activeCleanupRequeue     = 10 * time.Second
	cleanupRetryDelay        = 30 * time.Second
	artifactCleanupComponent = "artifact-cleanup"
)

var artifactFailureConditions = []string{
	kamav1alpha1.ModelArtifactConditionInvalidGGUF,
	kamav1alpha1.ModelArtifactConditionChecksumMismatch,
	kamav1alpha1.ModelArtifactConditionMissingShard,
	kamav1alpha1.ModelArtifactConditionInsufficientStorage,
	kamav1alpha1.ModelArtifactConditionSourceUnavailable,
}

var errIncompatibleVolumePlacement = errors.New("source and cache PersistentVolumes have incompatible node affinity")

// ModelArtifactReconciler imports and validates immutable GGUF artifacts.
type ModelArtifactReconciler struct {
	reconcilerBase
	// APIReader bypasses the controller-runtime cache for operation-lifecycle
	// reads that immediately precede collision, quiescence, mount, or deletion
	// decisions. SetupWithManager always supplies the manager's uncached reader;
	// direct unit reconcilers fall back to Client unless they inject one.
	APIReader client.Reader
}

// NewModelArtifactReconciler builds a ModelArtifact reconciler.
func NewModelArtifactReconciler(
	kubeClient client.Client,
	scheme *runtime.Scheme,
	recorder events.EventRecorder,
	clientset kubernetes.Interface,
	importer ImporterOptions,
) *ModelArtifactReconciler {
	return &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: recorder, Clientset: clientset, Importer: importer,
	}}
}

// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelartifacts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelartifacts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelartifacts/finalizers,verbs=update
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelcaches,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile converges one ModelArtifact.
//
//nolint:gocyclo // Reconciliation intentionally keeps the artifact lifecycle state machine in one auditable flow.
func (r *ModelArtifactReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var modelArtifact kamav1alpha1.ModelArtifact
	if err := r.Get(ctx, request.NamespacedName, &modelArtifact); err != nil {
		if apierrors.IsNotFound(err) {
			deleteArtifactGaugeSeries(request.Namespace, request.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !modelArtifact.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &modelArtifact)
	}
	if !controllerutil.ContainsFinalizer(&modelArtifact, kamav1alpha1.ModelArtifactFinalizer) {
		controllerutil.AddFinalizer(&modelArtifact, kamav1alpha1.ModelArtifactFinalizer)
		if err := r.Update(ctx, &modelArtifact); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	storage, err := r.resolveArtifactStorage(ctx, &modelArtifact)
	if err != nil {
		reason := "StorageUnavailable"
		if errors.Is(err, errIncompatibleVolumePlacement) {
			reason = "IncompatibleVolumePlacement"
		}
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionStorageReady,
			reason, err.Error(), 15*time.Second)
	}
	if storage.location == nil {
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionStorageReady,
			"StorageIdentityUnavailable", "resolved artifact storage has no serving identity", 0)
	}
	if modelArtifact.Status.Location == nil {
		modelArtifact.Status.Location = storage.location.DeepCopy()
		if err := r.Status().Update(ctx, &modelArtifact); err != nil {
			return ctrl.Result{}, err
		}
		// Persist the exact claim/PV identity before any importer resource can
		// create transient state that deletion must later clean.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if !servingLocationIdentityEqual(modelArtifact.Status.Location, storage.location) {
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionStorageReady,
			"StorageIdentityChanged", "resolved artifact storage differs from its durable identity checkpoint", 0)
	}
	lifecycleReader := r.artifactLifecycleReader()
	if modelArtifact.Status.ObservedGeneration == modelArtifact.Generation &&
		meta.IsStatusConditionTrue(modelArtifact.Status.Conditions, kamav1alpha1.ModelArtifactConditionReady) &&
		servingLocationIdentityEqual(modelArtifact.Status.Location, storage.location) &&
		modelArtifact.Status.JobRef != nil && modelArtifact.Status.JobRef.Name != "" {
		var retainedJob batchv1.Job
		err := lifecycleReader.Get(ctx, types.NamespacedName{
			Namespace: modelArtifact.Namespace,
			Name:      modelArtifact.Status.JobRef.Name,
		}, &retainedJob)
		switch {
		case err == nil && jobComplete(&retainedJob) && jobReferenceMatches(modelArtifact.Status.JobRef, &retainedJob):
			// The completed Job and its bounded Pod log are retained as the audit
			// record. Storage was re-resolved above, so Ready still reflects a live
			// claim and its current placement identity.
			retainedOperation := retainedJob.Labels[operationIDLabel]
			retainedResult, resultErr := r.readJobResultWithReader(ctx, lifecycleReader, &retainedJob)
			retainedSpec := r.importerSpec(&modelArtifact, retainedOperation)
			if !hasControllerOwner(retainedJob.OwnerReferences, modelArtifact.UID, "ModelArtifact") ||
				retainedOperation == "" || resultErr != nil ||
				validateSuccessfulResult(retainedSpec, retainedOperation, retainedResult) != nil ||
				retainedResult.ArtifactDigest != modelArtifact.Status.ArtifactDigest ||
				retainedResult.ResolvedRevision != modelArtifact.Status.ResolvedRevision {
				if err := deleteIfPresent(ctx, r.Client, &retainedJob); err != nil {
					return ctrl.Result{}, err
				}
				return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionSourceUnavailable,
					"ResultUnavailable", "retained importer evidence is unavailable; recreating safely", time.Second)
			}
			// A status write can succeed even when the best-effort Lease release
			// immediately afterward is interrupted. Ready reconciliation must finish
			// that release so stale holders do not survive a manager restart.
			cleaningLease, cleanupErr := r.cleanupArtifactLeases(ctx, &modelArtifact)
			if cleanupErr != nil {
				return ctrl.Result{}, cleanupErr
			}
			if cleaningLease {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			modelArtifactReady.WithLabelValues(modelArtifact.Namespace, modelArtifact.Name).Set(1)
			modelArtifactSizeBytes.WithLabelValues(modelArtifact.Namespace, modelArtifact.Name).
				Set(float64(modelArtifact.Status.Size))
			return ctrl.Result{RequeueAfter: defaultProbeInterval}, nil
		case err == nil && jobComplete(&retainedJob):
			// A replacement completed Job must have its result revalidated before
			// its UID is accepted into status.
		case err == nil && !jobFailed(&retainedJob):
			return ctrl.Result{RequeueAfter: activeImportRequeue}, nil
		case err == nil:
			// A stale failed deterministic Job cannot be updated. Remove it and
			// recreate the same operation, which validates READY before transfer.
			if err := deleteIfPresent(ctx, r.Client, &retainedJob); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		case !apierrors.IsNotFound(err):
			return ctrl.Result{}, err
		}
		// If the retained result disappeared, recreate the deterministic Job.
	}
	operation, err := artifactOperationID(&modelArtifact, storage)
	if err != nil {
		return ctrl.Result{}, err
	}
	configName := deterministicName(modelArtifact.Name+"-import-config", operation)
	jobName := deterministicName(modelArtifact.Name+"-import", operation)
	cleaning, err := r.cleanupObsoleteArtifactOperations(ctx, &modelArtifact, operation, storage)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cleaning {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	leaseName, acquired, err := r.acquireLease(ctx, &modelArtifact, storage)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		return r.markImporting(ctx, &modelArtifact, nil, "WaitingForLease",
			"another artifact operation holds the source/cache lease", 15*time.Second)
	}

	spec := r.importerSpec(&modelArtifact, operation)
	configMap, err := newSpecConfigMap(&modelArtifact, r.Scheme, configName, spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	configMap.Labels[artifactNameLabel] = boundedLabelValue(modelArtifact.Name)
	configMap.Labels[artifactUIDLabel] = string(modelArtifact.UID)
	configMap.Labels[operationIDLabel] = operation
	replaced, err := r.replaceUnusedImporterConfig(ctx, &modelArtifact, configMap, jobName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if replaced {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err := ensureObjectWithReader(ctx, r.Client, lifecycleReader, configMap); err != nil {
		return ctrl.Result{}, fmt.Errorf("create importer config: %w", err)
	}
	job, err := newImportJob(r.Scheme, r.Importer, importJobOptions{
		Owner: &modelArtifact, Name: jobName, ConfigMapName: configName,
		CacheClaim: storage.cacheClaim, SourceClaim: storage.sourceClaim,
		TokenSecret: storage.tokenSecret, OperationID: operation,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	job.Labels[artifactNameLabel] = boundedLabelValue(modelArtifact.Name)
	job.Labels[artifactUIDLabel] = string(modelArtifact.UID)
	job.Spec.Template.Labels[artifactNameLabel] = boundedLabelValue(modelArtifact.Name)
	job.Spec.Template.Labels[artifactUIDLabel] = string(modelArtifact.UID)
	if err := ensureObjectWithReader(ctx, r.Client, lifecycleReader, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("create importer Job: %w", err)
	}
	if err := lifecycleReader.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
		return ctrl.Result{}, err
	}
	jobReference := &kamav1alpha1.ModelArtifactJobReference{Name: job.Name, UID: job.UID}
	if !jobComplete(job) && !jobFailed(job) {
		_, renewed, renewErr := r.acquireLease(ctx, &modelArtifact, storage)
		if renewErr != nil {
			return ctrl.Result{}, renewErr
		}
		if !renewed {
			return r.markImporting(ctx, &modelArtifact, jobReference, "WaitingForLease",
				"another artifact operation holds the source/cache lease", 15*time.Second)
		}
		condition, reason, message, blocked, inspectErr := r.inspectPendingJob(ctx, job)
		if inspectErr != nil {
			return ctrl.Result{}, inspectErr
		}
		if blocked {
			return r.artifactFailure(ctx, &modelArtifact, condition, reason, message, activeImportRequeue)
		}
		return r.markImporting(ctx, &modelArtifact, jobReference, "ImportRunning",
			"artifact import or validation Job is running", activeImportRequeue)
	}

	result, err := r.readJobResultWithReader(ctx, lifecycleReader, job)
	if err != nil {
		return r.retryUnavailableResult(ctx, &modelArtifact, job, configMap, leaseName)
	}
	if result.OperationID != "" && result.OperationID != operation {
		_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionSourceUnavailable,
			"ResultIdentityMismatch", "importer result does not match the requested operation", 0)
	}
	if result.Mode != "" && result.Mode != spec.Mode {
		_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionSourceUnavailable,
			"ResultIdentityMismatch", "importer result mode does not match the requested operation", 0)
	}
	if !result.Success && !artifact.ValidReason(result.Reason) {
		_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionSourceUnavailable,
			"InvalidResult", "importer result contains an unsupported failure reason", 0)
	}
	if !result.Success {
		condition, retry := conditionForReason(result.Reason)
		currentCondition := meta.FindStatusCondition(modelArtifact.Status.Conditions, condition)
		alreadyReported := currentCondition != nil && currentCondition.Status == metav1.ConditionTrue &&
			currentCondition.ObservedGeneration == modelArtifact.Generation && currentCondition.Reason == string(result.Reason)
		if !alreadyReported {
			source := artifactSourceLabel(&modelArtifact)
			artifactOperations.WithLabelValues(source, "failure", string(result.Reason)).Inc()
			artifactOperationDuration.WithLabelValues(source, "failure").Observe(float64(result.DurationMillis) / 1000)
			artifactValidationDuration.WithLabelValues(source, "failure").Observe(float64(result.ValidationMillis) / 1000)
		}
		_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
		if retry > 0 && alreadyReported {
			remaining := retry - time.Since(currentCondition.LastTransitionTime.Time)
			if remaining > 0 {
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
			if err := deleteIfPresent(ctx, r.Client, job); err != nil {
				return ctrl.Result{}, err
			}
			if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
				return ctrl.Result{}, err
			}
			artifactRetries.WithLabelValues(artifactSourceLabel(&modelArtifact), string(result.Reason)).Inc()
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return r.artifactFailure(ctx, &modelArtifact, condition, string(result.Reason), result.Message, retry)
	}
	if err := validateSuccessfulResult(spec, operation, result); err != nil {
		_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
		if deleteErr := deleteIfPresent(ctx, r.Client, job); deleteErr != nil {
			return ctrl.Result{}, deleteErr
		}
		if deleteErr := deleteIfPresent(ctx, r.Client, configMap); deleteErr != nil {
			return ctrl.Result{}, deleteErr
		}
		artifactRetries.WithLabelValues(artifactSourceLabel(&modelArtifact), "InvalidResult").Inc()
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionSourceUnavailable,
			"InvalidResult", "importer result failed controller-side identity validation; recreating safely", time.Minute)
	}
	if modelArtifact.Status.ResolvedRevision != "" &&
		result.ResolvedRevision != modelArtifact.Status.ResolvedRevision {
		_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionChecksumMismatch,
			"ImmutableRevisionMismatch", "recovered source resolved to different content", 0)
	}
	if modelArtifact.Status.ArtifactDigest != "" &&
		result.ArtifactDigest != modelArtifact.Status.ArtifactDigest {
		_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
		return r.artifactFailure(ctx, &modelArtifact, kamav1alpha1.ModelArtifactConditionChecksumMismatch,
			"ImmutableContentMismatch", "recovered source content differs from the previously verified artifact", 0)
	}
	if err := r.completeArtifact(ctx, &modelArtifact, storage, result, jobReference); err != nil {
		return ctrl.Result{}, err
	}
	source := artifactSourceLabel(&modelArtifact)
	artifactOperations.WithLabelValues(source, "success", "").Inc()
	artifactBytesTransferred.WithLabelValues(source).Add(float64(result.BytesTransferred))
	if result.CacheHit {
		artifactCacheHits.WithLabelValues(source).Inc()
	}
	artifactOperationDuration.WithLabelValues(source, "success").Observe(float64(result.DurationMillis) / 1000)
	artifactValidationDuration.WithLabelValues(source, "success").Observe(float64(result.ValidationMillis) / 1000)
	if err := r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID)); err != nil {
		return ctrl.Result{}, err
	}
	// Keep the completed Job, its Pod log, and immutable input ConfigMap until
	// ModelArtifact deletion. They are the bounded full-result recovery record.
	// Requeue explicitly so Ready reconciliation confirms the Lease disappeared;
	// status updates do not guarantee a second reconcile after a manager restart.
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func deleteArtifactGaugeSeries(namespace, name string) {
	modelArtifactReady.DeleteLabelValues(namespace, name)
	modelArtifactSizeBytes.DeleteLabelValues(namespace, name)
}

func (r *ModelArtifactReconciler) replaceUnusedImporterConfig(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	desired *corev1.ConfigMap,
	jobName string,
) (bool, error) {
	reader := r.artifactLifecycleReader()
	var existing corev1.ConfigMap
	if err := reader.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if reflect.DeepEqual(existing.Data, desired.Data) && reflect.DeepEqual(existing.BinaryData, desired.BinaryData) {
		return false, nil
	}
	if !hasControllerOwner(existing.OwnerReferences, modelArtifact.UID, "ModelArtifact") ||
		existing.Labels[operationIDLabel] != desired.Labels[operationIDLabel] {
		return false, fmt.Errorf("refusing to replace importer ConfigMap %s/%s with mismatched identity", existing.Namespace, existing.Name)
	}
	var job batchv1.Job
	if err := reader.Get(ctx, types.NamespacedName{Namespace: modelArtifact.Namespace, Name: jobName}, &job); err == nil {
		return false, fmt.Errorf("refusing to replace importer ConfigMap while Job %s/%s still exists", job.Namespace, job.Name)
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	if err := deleteIfPresent(ctx, r.Client, &existing); err != nil {
		return false, err
	}
	return true, nil
}

func (r *ModelArtifactReconciler) inspectPendingJob(
	ctx context.Context,
	job *batchv1.Job,
) (condition, reason, message string, blocked bool, returnedErr error) {
	reader := r.artifactLifecycleReader()
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{legacyJobNameLabel: job.Name}); err != nil {
		return "", "", "", false, err
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !hasControllerOwner(pod.OwnerReferences, job.UID, "Job") ||
			pod.Labels[operationIDLabel] != job.Labels[operationIDLabel] {
			continue
		}
		for _, podCondition := range pod.Status.Conditions {
			if podCondition.Type == corev1.PodScheduled && podCondition.Status == corev1.ConditionFalse &&
				podCondition.Reason == corev1.PodReasonUnschedulable {
				return kamav1alpha1.ModelArtifactConditionStorageReady, "Unschedulable",
					"importer Pod cannot be scheduled with the resolved volume placement", true, nil
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != importerContainer || status.State.Waiting == nil {
				continue
			}
			switch status.State.Waiting.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return kamav1alpha1.ModelArtifactConditionSourceUnavailable, "ImporterImageUnavailable",
					"importer image cannot be pulled", true, nil
			case "CreateContainerConfigError":
				return kamav1alpha1.ModelArtifactConditionSourceUnavailable, "ImporterConfigurationUnavailable",
					"importer Pod configuration or credential volume is unavailable", true, nil
			case "RunContainerError":
				return kamav1alpha1.ModelArtifactConditionSourceUnavailable, "ImporterRuntimeUnavailable",
					"importer container cannot start", true, nil
			}
		}

		var eventList corev1.EventList
		if err := reader.List(ctx, &eventList, client.InNamespace(job.Namespace)); err != nil {
			return "", "", "", false, err
		}
		for eventIndex := range eventList.Items {
			event := &eventList.Items[eventIndex]
			if event.InvolvedObject.UID != pod.UID || event.Type != corev1.EventTypeWarning {
				continue
			}
			switch event.Reason {
			case "FailedScheduling", "FailedAttachVolume", "Multi-AttachError":
				return kamav1alpha1.ModelArtifactConditionStorageReady, "VolumePlacementUnavailable",
					"importer Pod cannot attach or schedule the resolved volumes", true, nil
			case "FailedMount":
				if jobUsesSecretVolume(job) {
					return kamav1alpha1.ModelArtifactConditionSourceUnavailable, "CredentialMountUnavailable",
						"importer credential or input volume cannot be mounted", true, nil
				}
				return kamav1alpha1.ModelArtifactConditionStorageReady, "VolumeMountUnavailable",
					"importer source or cache volume cannot be mounted", true, nil
			case "Failed", "BackOff":
				return kamav1alpha1.ModelArtifactConditionSourceUnavailable, "ImporterRuntimeUnavailable",
					"importer container cannot start", true, nil
			}
		}
	}
	return "", "", "", false, nil
}

func jobUsesSecretVolume(job *batchv1.Job) bool {
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			return true
		}
	}
	return false
}

func validateSuccessfulResult(spec artifact.Spec, operation string, result artifact.Result) error {
	if result.OperationID != operation || result.Mode != spec.Mode || result.Manifest == nil || result.GGUF == nil ||
		result.ArtifactDigest == "" {
		return errors.New("result identity or verified metadata is incomplete")
	}
	if result.Manifest.Format != artifact.FormatGGUF || result.Manifest.Entrypoint != spec.Entrypoint {
		return errors.New("result manifest does not match requested artifact")
	}
	digest, err := artifact.ArtifactDigest(*result.Manifest)
	if err != nil {
		return fmt.Errorf("validate result manifest: %w", err)
	}
	if digest != result.ArtifactDigest {
		return errors.New("result digest does not match its canonical manifest")
	}
	if spec.Mode == artifact.ModeHub || spec.Mode == artifact.ModeCopy {
		publicationDigest, err := artifact.ManifestDigest(*result.Manifest)
		if err != nil {
			return fmt.Errorf("validate result publication digest: %w", err)
		}
		if result.PublishedPath != "blobs/sha256/"+publicationDigest {
			return errors.New("result publication path does not match its canonical manifest")
		}
	}
	if spec.Mode == artifact.ModeDirect && spec.PVC != nil && result.PublishedPath != spec.PVC.RootPath {
		return errors.New("direct result path does not match its source root")
	}
	if err := artifact.VerifyExpectations(*result.Manifest, spec.ExpectedSHA256, spec.ExpectedSize); err != nil {
		return fmt.Errorf("validate result expectations: %w", err)
	}
	if spec.Mode == artifact.ModeHub && result.ResolvedRevision == "" {
		return errors.New("hub result omitted immutable revision")
	}
	if spec.Mode == artifact.ModeHub && !artifact.ValidHubCommit(result.ResolvedRevision) {
		return errors.New("hub result contains an invalid immutable revision")
	}
	if spec.Mode == artifact.ModeHub && spec.Hub != nil && artifact.ValidHubCommit(spec.Hub.Revision) &&
		result.ResolvedRevision != spec.Hub.Revision {
		return errors.New("hub result does not match the pinned immutable revision")
	}
	return nil
}

type artifactStorage struct {
	cacheClaim      string
	cacheUID        string
	cacheClaimUID   types.UID
	cacheVolumeUID  types.UID
	sourceClaim     string
	sourceClaimUID  types.UID
	sourceVolumeUID types.UID
	tokenSecret     *kamav1alpha1.SecretKeyReference
	location        *kamav1alpha1.ModelArtifactLocationStatus
	cacheIdentity   *volumeIdentity
	sourceIdentity  *volumeIdentity
}

func claimHasCacheDeletionGuard(claim *corev1.PersistentVolumeClaim) bool {
	if claim == nil {
		return false
	}
	_, guarded := claim.Annotations[cacheDeletionGuardAnnotation]
	return guarded
}

func artifactOperationID(modelArtifact *kamav1alpha1.ModelArtifact, storage artifactStorage) (string, error) {
	identity := []string{
		string(modelArtifact.UID),
		storage.cacheUID,
		string(storage.cacheClaimUID),
		string(storage.cacheVolumeUID),
	}
	if source := modelArtifact.Spec.Source.PersistentVolumeClaim; source != nil &&
		source.ImportPolicy == kamav1alpha1.PVCImportPolicyDirect {
		identity = append(identity, string(storage.sourceClaimUID), string(storage.sourceVolumeUID))
	}
	fingerprint, err := operationID(modelArtifact.Spec, identity...)
	if err != nil {
		return "", err
	}
	return string(modelArtifact.UID) + "-" + fingerprint, nil
}

func servingLocationIdentityEqual(
	left, right *kamav1alpha1.ModelArtifactLocationStatus,
) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftIdentity := left.DeepCopy()
	rightIdentity := right.DeepCopy()
	// Managed artifact subpaths are derived from the verified digest and are
	// populated only after the importer succeeds; they are not storage identity.
	leftIdentity.SubPath = ""
	rightIdentity.SubPath = ""
	return reflect.DeepEqual(leftIdentity, rightIdentity)
}

func jobReferenceMatches(reference *kamav1alpha1.ModelArtifactJobReference, job *batchv1.Job) bool {
	return reference != nil && reference.Name == job.Name &&
		(reference.UID == "" || reference.UID == job.UID)
}

//nolint:gocyclo // Storage resolution keeps the cache, Copy, and Direct safety checks in one auditable path.
func (r *ModelArtifactReconciler) resolveArtifactStorage(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (artifactStorage, error) {
	storage := artifactStorage{}
	reader := r.artifactLifecycleReader()
	if source := modelArtifact.Spec.Source.HuggingFace; source != nil &&
		(modelArtifact.Status.ValidatedAt == nil || modelArtifact.Status.ArtifactDigest == "") {
		// A validated artifact recovers from local operation metadata before any
		// network access. Do not make a deleted/rotated token Secret a mount-time
		// dependency for that local recovery path.
		storage.tokenSecret = source.TokenSecretRef
	}
	if modelArtifact.Spec.CacheRef != nil {
		var cache kamav1alpha1.ModelCache
		if err := reader.Get(ctx, types.NamespacedName{
			Namespace: modelArtifact.Namespace, Name: modelArtifact.Spec.CacheRef.Name,
		}, &cache); err != nil {
			return storage, fmt.Errorf("get ModelCache: %w", err)
		}
		if !cache.DeletionTimestamp.IsZero() {
			return storage, errors.New("referenced ModelCache is terminating")
		}
		if !meta.IsStatusConditionTrue(cache.Status.Conditions, kamav1alpha1.ModelCacheConditionReady) {
			return storage, errors.New("referenced ModelCache is not Ready")
		}
		if cache.Status.ClaimName == "" {
			return storage, errors.New("referenced ModelCache has no resolved claim")
		}
		var claim corev1.PersistentVolumeClaim
		if err := reader.Get(ctx, types.NamespacedName{
			Namespace: modelArtifact.Namespace, Name: cache.Status.ClaimName,
		}, &claim); err != nil {
			return storage, fmt.Errorf("get cache claim: %w", err)
		}
		if !claim.DeletionTimestamp.IsZero() {
			return storage, errors.New("cache claim is terminating")
		}
		if claimHasCacheDeletionGuard(&claim) {
			return storage, errors.New("cache claim is guarded for deletion")
		}
		if claim.Status.Phase != corev1.ClaimBound {
			return storage, errors.New("cache claim is not Bound")
		}
		if cache.Status.ClaimUID != "" && cache.Status.ClaimUID != claim.UID {
			return storage, errors.New("cache claim identity changed")
		}
		identity, err := r.resolveVolumeWithReader(ctx, reader, &claim)
		if err != nil {
			return storage, fmt.Errorf("resolve cache volume: %w", err)
		}
		if !hasWritableMode(identity.AccessModes) {
			return storage, errors.New("cache claim has no writable access mode")
		}
		storage.cacheClaim = claim.Name
		storage.cacheUID = string(cache.UID)
		storage.cacheClaimUID = claim.UID
		storage.cacheVolumeUID = identity.VolumeUID
		cacheIdentity := identity
		storage.cacheIdentity = &cacheIdentity
		storage.location = &kamav1alpha1.ModelArtifactLocationStatus{
			ClaimName: claim.Name, ClaimUID: claim.UID, ReadOnly: true,
			AccessModes: append([]corev1.PersistentVolumeAccessMode(nil), identity.AccessModes...),
			VolumeMode:  identity.VolumeMode, MountScope: identity.MountScope,
			VolumeName: identity.VolumeName, VolumeUID: identity.VolumeUID,
			NodeAffinity: identity.NodeAffinity.DeepCopy(),
		}
	}

	if source := modelArtifact.Spec.Source.PersistentVolumeClaim; source != nil {
		isDirect := source.ImportPolicy == kamav1alpha1.PVCImportPolicyDirect
		validatedCacheUnchanged := !isDirect && modelArtifact.Status.ValidatedAt != nil &&
			modelArtifact.Status.ArtifactDigest != "" &&
			servingLocationIdentityEqual(modelArtifact.Status.Location, storage.location)
		if validatedCacheUnchanged {
			// A verified Copy artifact is independent of its original source. If
			// the retained Job must be reconstructed, the importer can validate
			// READY from the cache without mounting a source claim.
			return storage, nil
		}

		var claim corev1.PersistentVolumeClaim
		if err := reader.Get(ctx, types.NamespacedName{
			Namespace: modelArtifact.Namespace, Name: source.ClaimName,
		}, &claim); err != nil {
			return storage, fmt.Errorf("get source claim: %w", err)
		}
		if !claim.DeletionTimestamp.IsZero() {
			return storage, errors.New("source claim is terminating")
		}
		if claimHasCacheDeletionGuard(&claim) {
			return storage, errors.New("source claim is guarded for deletion")
		}
		if claim.Status.Phase != corev1.ClaimBound {
			return storage, errors.New("source claim is not Bound")
		}
		identity, err := r.resolveVolumeWithReader(ctx, reader, &claim)
		if err != nil {
			return storage, err
		}
		storage.sourceClaim = claim.Name
		storage.sourceClaimUID = claim.UID
		storage.sourceVolumeUID = identity.VolumeUID
		sourceIdentity := identity
		storage.sourceIdentity = &sourceIdentity
		if isDirect {
			storage.location = locationFromVolume(claim.Name, claim.UID, source.RootPath, identity)
		}
	}
	if storage.cacheIdentity != nil && storage.sourceIdentity != nil &&
		!volumeNodeAffinitiesCompatible(storage.cacheIdentity.NodeAffinity, storage.sourceIdentity.NodeAffinity) {
		return storage, errIncompatibleVolumePlacement
	}
	return storage, nil
}

func (r *ModelArtifactReconciler) importerSpec(
	modelArtifact *kamav1alpha1.ModelArtifact,
	operation string,
) artifact.Spec {
	validated := modelArtifact.Status.ValidatedAt != nil && modelArtifact.Status.ArtifactDigest != ""
	spec := artifact.Spec{
		SchemaVersion:  artifact.SchemaVersion,
		OperationID:    operation,
		Format:         string(modelArtifact.Spec.Format),
		Entrypoint:     modelArtifact.Spec.Entrypoint,
		ExpectedSHA256: strings.ToLower(modelArtifact.Spec.Verification.ExpectedSHA256),
		ExpectedSize:   modelArtifact.Spec.Verification.ExpectedSize,
		CacheRoot:      cacheMountPath,
		HubEndpoint:    r.Importer.HubEndpoint,
		HTTP: artifact.HTTPClientOptions{
			AllowHTTP: strings.HasPrefix(r.Importer.HubEndpoint, "http://"),
			UserAgent: "kama-importer",
		},
	}
	if source := modelArtifact.Spec.Source.HuggingFace; source != nil {
		revision := source.Revision
		if validated && modelArtifact.Status.ResolvedRevision != "" {
			revision = modelArtifact.Status.ResolvedRevision
		}
		spec.Mode = artifact.ModeHub
		spec.Hub = &artifact.HubSpec{
			Repository: source.Repository, Revision: revision,
			FileSelectors: append([]string(nil), source.Files...),
		}
		if source.TokenSecretRef != nil {
			spec.Hub.TokenFile = tokenMountPath + "/token"
		}
	}
	if source := modelArtifact.Spec.Source.PersistentVolumeClaim; source != nil {
		spec.Mode = artifact.ModeCopy
		if source.ImportPolicy == kamav1alpha1.PVCImportPolicyDirect {
			spec.Mode = artifact.ModeDirect
			spec.CacheRoot = ""
		}
		spec.PVC = &artifact.PVCSpec{
			MountRoot: sourceMountPath, RootPath: source.RootPath,
			SelectedFiles: []string{modelArtifact.Spec.Entrypoint},
		}
	}
	if validated {
		// Once content has been verified, any cache/source recovery is pinned to
		// the same immutable identity even when the original API revision was a
		// mutable Hub tag or a Direct/Copy PVC has since changed.
		spec.ExpectedSHA256 = modelArtifact.Status.ArtifactDigest
		if modelArtifact.Status.Size > 0 {
			expectedSize := modelArtifact.Status.Size
			spec.ExpectedSize = &expectedSize
		}
	}
	return spec
}

func (r *ModelArtifactReconciler) completeArtifact(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	storage artifactStorage,
	result artifact.Result,
	jobReference *kamav1alpha1.ModelArtifactJobReference,
) error {
	before := modelArtifact.Status.DeepCopy()
	modelArtifact.Status.ObservedGeneration = modelArtifact.Generation
	modelArtifact.Status.ResolvedRevision = result.ResolvedRevision
	modelArtifact.Status.Files = make([]kamav1alpha1.ModelArtifactFileStatus, 0, len(result.Manifest.Files))
	var total int64
	for _, file := range result.Manifest.Files {
		modelArtifact.Status.Files = append(modelArtifact.Status.Files, kamav1alpha1.ModelArtifactFileStatus{
			Path: file.Path, Size: file.Size, SHA256: file.SHA256,
		})
		total += file.Size
	}
	modelArtifact.Status.ArtifactDigest = result.ArtifactDigest
	modelArtifact.Status.Size = total
	modelArtifact.Status.Architecture = result.GGUF.Architecture
	modelArtifact.Status.Quantization = result.GGUF.Quantization
	modelArtifact.Status.ShardCount = int32(result.GGUF.ShardCount)
	now := metav1.Now()
	modelArtifact.Status.ValidatedAt = &now
	modelArtifact.Status.JobRef = jobReference
	modelArtifact.Status.Location = storage.location.DeepCopy()
	if modelArtifact.Status.Location != nil && result.Mode != artifact.ModeDirect {
		modelArtifact.Status.Location.SubPath = result.PublishedPath
	}
	setArtifactSuccessConditions(modelArtifact)
	if reflect.DeepEqual(before, &modelArtifact.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, modelArtifact); err != nil {
		return err
	}
	r.Recorder.Eventf(modelArtifact, nil, corev1.EventTypeNormal, "ArtifactReady", "VerifyArtifact",
		"Artifact import and verification succeeded")
	modelArtifactReady.WithLabelValues(modelArtifact.Namespace, modelArtifact.Name).Set(1)
	modelArtifactSizeBytes.WithLabelValues(modelArtifact.Namespace, modelArtifact.Name).Set(float64(total))
	return nil
}

func setArtifactSuccessConditions(modelArtifact *kamav1alpha1.ModelArtifact) {
	for _, condition := range []string{
		kamav1alpha1.ModelArtifactConditionSourceResolved,
		kamav1alpha1.ModelArtifactConditionStorageReady,
		kamav1alpha1.ModelArtifactConditionVerified,
		kamav1alpha1.ModelArtifactConditionReady,
	} {
		meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
			Type: condition, Status: metav1.ConditionTrue, ObservedGeneration: modelArtifact.Generation,
			Reason: "Succeeded", Message: "artifact source and verified storage are ready",
		})
	}
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionImporting, Status: metav1.ConditionFalse,
		ObservedGeneration: modelArtifact.Generation, Reason: "Complete", Message: "artifact operation completed",
	})
	for _, condition := range artifactFailureConditions {
		meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
			Type: condition, Status: metav1.ConditionFalse, ObservedGeneration: modelArtifact.Generation,
			Reason: "Succeeded", Message: failureNotPresent,
		})
	}
}

func (r *ModelArtifactReconciler) markImporting(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	jobReference *kamav1alpha1.ModelArtifactJobReference,
	reason, message string,
	requeue time.Duration,
) (ctrl.Result, error) {
	before := modelArtifact.Status.DeepCopy()
	wasImporting := meta.IsStatusConditionTrue(
		modelArtifact.Status.Conditions,
		kamav1alpha1.ModelArtifactConditionImporting,
	)
	modelArtifact.Status.ObservedGeneration = modelArtifact.Generation
	modelArtifact.Status.JobRef = jobReference
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionStorageReady, Status: metav1.ConditionTrue,
		ObservedGeneration: modelArtifact.Generation, Reason: "StorageAvailable", Message: "source and cache storage are available",
	})
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionImporting, Status: metav1.ConditionTrue,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: message,
	})
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: message,
	})
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionSourceResolved, Status: metav1.ConditionFalse,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: "source resolution is not complete",
	})
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionVerified, Status: metav1.ConditionFalse,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: "artifact verification is not complete",
	})
	for _, condition := range artifactFailureConditions {
		meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
			Type: condition, Status: metav1.ConditionFalse, ObservedGeneration: modelArtifact.Generation,
			Reason: reason, Message: failureNotPresent,
		})
	}
	if !reflect.DeepEqual(before, &modelArtifact.Status) {
		if err := r.Status().Update(ctx, modelArtifact); err != nil {
			return ctrl.Result{}, err
		}
		if !wasImporting && reason == "ImportRunning" {
			r.Recorder.Eventf(modelArtifact, nil, corev1.EventTypeNormal, "ImportStarted", "ImportArtifact",
				"Artifact import or validation started")
		}
	}
	modelArtifactReady.WithLabelValues(modelArtifact.Namespace, modelArtifact.Name).Set(0)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *ModelArtifactReconciler) artifactFailure(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	condition, reason, message string,
	requeue time.Duration,
) (ctrl.Result, error) {
	message = sanitizeConditionMessage(message)
	if message == "" {
		message = "artifact operation failed"
	}
	before := modelArtifact.Status.DeepCopy()
	modelArtifact.Status.ObservedGeneration = modelArtifact.Generation
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: message,
	})
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionImporting, Status: metav1.ConditionFalse,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: "artifact operation is not running",
	})
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: condition, Status: metav1.ConditionTrue,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: message,
	})
	for _, other := range artifactFailureConditions {
		if other == condition {
			continue
		}
		meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
			Type: other, Status: metav1.ConditionFalse, ObservedGeneration: modelArtifact.Generation,
			Reason: reason, Message: failureNotPresent,
		})
	}
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionVerified, Status: metav1.ConditionFalse,
		ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: "artifact verification did not succeed",
	})
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type:               kamav1alpha1.ModelArtifactConditionStorageReady,
		Status:             boolConditionStatus(condition != kamav1alpha1.ModelArtifactConditionStorageReady),
		ObservedGeneration: modelArtifact.Generation, Reason: reason,
		Message: conditionMessage(
			condition != kamav1alpha1.ModelArtifactConditionStorageReady,
			"source and cache storage were available for the operation",
			"source or cache storage is unavailable",
		),
	})
	sourceResolved := condition != kamav1alpha1.ModelArtifactConditionSourceUnavailable &&
		condition != kamav1alpha1.ModelArtifactConditionStorageReady &&
		reason != string(artifact.ReasonInvalidSpec) && reason != string(artifact.ReasonUnsafePath)
	meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionSourceResolved, Status: boolConditionStatus(sourceResolved),
		ObservedGeneration: modelArtifact.Generation, Reason: reason,
		Message: conditionMessage(
			sourceResolved,
			"artifact source resolved before validation failed",
			"artifact source is not resolved",
		),
	})
	if condition == kamav1alpha1.ModelArtifactConditionStorageReady {
		meta.SetStatusCondition(&modelArtifact.Status.Conditions, metav1.Condition{
			Type: kamav1alpha1.ModelArtifactConditionStorageReady, Status: metav1.ConditionFalse,
			ObservedGeneration: modelArtifact.Generation, Reason: reason, Message: message,
		})
	}
	if !reflect.DeepEqual(before, &modelArtifact.Status) {
		if err := r.Status().Update(ctx, modelArtifact); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(modelArtifact, nil, corev1.EventTypeWarning, reason, "ImportArtifact", "%s", message)
	}
	modelArtifactReady.WithLabelValues(modelArtifact.Namespace, modelArtifact.Name).Set(0)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *ModelArtifactReconciler) retryUnavailableResult(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	job *batchv1.Job,
	configMap *corev1.ConfigMap,
	leaseName string,
) (ctrl.Result, error) {
	_ = r.releaseLease(ctx, modelArtifact.Namespace, leaseName, string(modelArtifact.UID))
	condition := meta.FindStatusCondition(
		modelArtifact.Status.Conditions,
		kamav1alpha1.ModelArtifactConditionSourceUnavailable,
	)
	alreadyReported := condition != nil && condition.Status == metav1.ConditionTrue &&
		condition.ObservedGeneration == modelArtifact.Generation && condition.Reason == "ResultUnavailable"
	if alreadyReported {
		remaining := resultRetryDelay - time.Since(condition.LastTransitionTime.Time)
		if remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
		if err := deleteIfPresent(ctx, r.Client, job); err != nil {
			return ctrl.Result{}, err
		}
		if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
			return ctrl.Result{}, err
		}
		artifactRetries.WithLabelValues(artifactSourceLabel(modelArtifact), "ResultUnavailable").Inc()
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return r.artifactFailure(ctx, modelArtifact, kamav1alpha1.ModelArtifactConditionSourceUnavailable,
		"ResultUnavailable", "importer result is unavailable; recreating the deterministic Job safely", resultRetryDelay)
}

func sanitizeConditionMessage(message string) string {
	message = strings.ToValidUTF8(artifact.Sanitize(message), "�")
	const maximumRunes = 1024
	runes := []rune(message)
	if len(runes) > maximumRunes {
		message = string(runes[:maximumRunes])
	}
	return message
}

func artifactSourceLabel(modelArtifact *kamav1alpha1.ModelArtifact) string {
	if modelArtifact.Spec.Source.HuggingFace != nil {
		return "hugging_face"
	}
	if modelArtifact.Spec.Source.PersistentVolumeClaim != nil &&
		modelArtifact.Spec.Source.PersistentVolumeClaim.ImportPolicy == kamav1alpha1.PVCImportPolicyDirect {
		return "pvc_direct"
	}
	return "pvc_copy"
}

func conditionForReason(reason artifact.Reason) (string, time.Duration) {
	switch reason {
	case artifact.ReasonChecksumMismatch:
		return kamav1alpha1.ModelArtifactConditionChecksumMismatch, 0
	case artifact.ReasonInvalidGGUF, artifact.ReasonUnsafePath, artifact.ReasonInvalidSpec:
		return kamav1alpha1.ModelArtifactConditionInvalidGGUF, 0
	case artifact.ReasonMissingShard:
		return kamav1alpha1.ModelArtifactConditionMissingShard, 0
	case artifact.ReasonInsufficientStorage:
		return kamav1alpha1.ModelArtifactConditionInsufficientStorage, 5 * time.Minute
	case artifact.ReasonUnauthorized, artifact.ReasonSourceUnavailable:
		return kamav1alpha1.ModelArtifactConditionSourceUnavailable, 5 * time.Minute
	default:
		return kamav1alpha1.ModelArtifactConditionSourceUnavailable, time.Minute
	}
}

func locationFromVolume(
	claimName string,
	claimUID types.UID,
	subPath string,
	identity volumeIdentity,
) *kamav1alpha1.ModelArtifactLocationStatus {
	return &kamav1alpha1.ModelArtifactLocationStatus{
		ClaimName: claimName, ClaimUID: claimUID, SubPath: subPath, ReadOnly: true,
		AccessModes: append([]corev1.PersistentVolumeAccessMode(nil), identity.AccessModes...),
		VolumeMode:  identity.VolumeMode, MountScope: identity.MountScope,
		VolumeName: identity.VolumeName, VolumeUID: identity.VolumeUID,
		NodeAffinity: identity.NodeAffinity.DeepCopy(),
	}
}

func boolConditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func conditionMessage(value bool, whenTrue, whenFalse string) string {
	if value {
		return whenTrue
	}
	return whenFalse
}

func (r *ModelArtifactReconciler) cleanupObsoleteArtifactOperations(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	currentOperation string,
	storage artifactStorage,
) (bool, error) {
	labels := client.MatchingLabels{artifactUIDLabel: string(modelArtifact.UID)}
	var jobs batchv1.JobList
	if err := r.artifactLifecycleReader().List(
		ctx, &jobs, client.InNamespace(modelArtifact.Namespace), labels,
	); err != nil {
		return false, err
	}
	cleaning := false
	for index := range jobs.Items {
		job := &jobs.Items[index]
		if job.Labels[managedByLabel] != kamaName || job.Labels[componentLabel] != artifactImportComponent ||
			!hasControllerOwner(job.OwnerReferences, modelArtifact.UID, "ModelArtifact") {
			continue
		}
		if job.Labels[operationIDLabel] == currentOperation {
			continue
		}
		cleaning = true
		if job.DeletionTimestamp.IsZero() {
			if err := deleteForeground(ctx, r.Client, job); err != nil {
				return false, err
			}
		}
	}
	if cleaning {
		return true, nil
	}

	var configMaps corev1.ConfigMapList
	if err := r.artifactLifecycleReader().List(
		ctx, &configMaps, client.InNamespace(modelArtifact.Namespace), labels,
	); err != nil {
		return false, err
	}
	for index := range configMaps.Items {
		configMap := &configMaps.Items[index]
		if configMap.Labels[managedByLabel] != kamaName ||
			configMap.Labels[componentLabel] != artifactImportComponent ||
			!hasControllerOwner(configMap.OwnerReferences, modelArtifact.UID, "ModelArtifact") {
			continue
		}
		if configMap.Labels[operationIDLabel] == currentOperation {
			continue
		}
		cleaning = true
		if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
			return false, err
		}
	}

	currentFingerprint, err := artifactLeaseFingerprint(modelArtifact, storage)
	if err != nil {
		return false, err
	}
	leases, err := r.listArtifactLeases(ctx, modelArtifact.Namespace)
	if err != nil {
		return false, err
	}
	for index := range leases {
		lease := &leases[index]
		if lease.Labels[managedByLabel] != kamaName || lease.Labels[componentLabel] != artifactImportComponent {
			continue
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(modelArtifact.UID) ||
			lease.Labels[leaseFingerprintLabel] == currentFingerprint {
			continue
		}
		cleaning = true
		if err := r.deleteArtifactLease(ctx, lease); err != nil {
			return false, err
		}
	}
	return cleaning, nil
}

func artifactLeaseFingerprint(
	modelArtifact *kamav1alpha1.ModelArtifact,
	storage artifactStorage,
) (string, error) {
	return operationID(
		modelArtifact.Spec.Source,
		modelArtifact.Spec.Entrypoint,
		storage.cacheUID,
		string(storage.cacheClaimUID),
		string(storage.cacheVolumeUID),
		string(storage.sourceClaimUID),
		string(storage.sourceVolumeUID),
	)
}

func (r *ModelArtifactReconciler) acquireLease(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	storage artifactStorage,
) (string, bool, error) {
	identity, err := artifactLeaseFingerprint(modelArtifact, storage)
	if err != nil {
		return "", false, err
	}
	name := deterministicName("kama-artifact-lease", identity)
	holder := string(modelArtifact.UID)
	now := metav1.NewMicroTime(time.Now().UTC())
	duration := int32(leaseDuration.Seconds())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: modelArtifact.Namespace,
			Labels: map[string]string{
				managedByLabel: kamaName, componentLabel: artifactImportComponent, leaseFingerprintLabel: identity,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder, LeaseDurationSeconds: &duration,
			AcquireTime: &now, RenewTime: &now,
		},
	}
	if err := r.Create(ctx, lease); err == nil {
		return name, true, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return "", false, err
	}
	if err := r.artifactLifecycleReader().Get(ctx, client.ObjectKeyFromObject(lease), lease); err != nil {
		return "", false, err
	}
	if lease.Labels[managedByLabel] != kamaName || lease.Labels[componentLabel] != artifactImportComponent ||
		lease.Labels[leaseFingerprintLabel] != identity {
		return "", false, errors.New("refusing Lease with mismatched source/cache fingerprint")
	}
	expired := leaseExpired(lease, time.Now())
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != holder && !expired {
		return name, false, nil
	}
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &duration
	lease.Spec.RenewTime = &now
	if lease.Spec.AcquireTime == nil || expired {
		lease.Spec.AcquireTime = &now
	}
	if err := r.Update(ctx, lease); err != nil {
		if apierrors.IsConflict(err) {
			return name, false, nil
		}
		return "", false, err
	}
	return name, true, nil
}

func leaseExpired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).Before(now)
}

func (r *ModelArtifactReconciler) releaseLease(
	ctx context.Context,
	namespace, name, holder string,
) error {
	lease, err := r.getArtifactLease(ctx, namespace, name)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return nil
	}
	if lease.Labels[managedByLabel] != kamaName || lease.Labels[componentLabel] != artifactImportComponent ||
		lease.Labels[leaseFingerprintLabel] == "" {
		return errors.New("refusing to delete Lease with mismatched Kama identity")
	}
	return r.deleteArtifactLease(ctx, lease)
}

func (r *ModelArtifactReconciler) getArtifactLease(
	ctx context.Context,
	namespace, name string,
) (*coordinationv1.Lease, error) {
	var lease coordinationv1.Lease
	if err := r.artifactLifecycleReader().Get(
		ctx,
		types.NamespacedName{Namespace: namespace, Name: name},
		&lease,
	); err != nil {
		return nil, err
	}
	return &lease, nil
}

func (r *ModelArtifactReconciler) listArtifactLeases(
	ctx context.Context,
	namespace string,
) ([]coordinationv1.Lease, error) {
	var leases coordinationv1.LeaseList
	if err := r.artifactLifecycleReader().List(ctx, &leases, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return leases.Items, nil
}

func (r *ModelArtifactReconciler) deleteArtifactLease(ctx context.Context, lease *coordinationv1.Lease) error {
	uid := lease.UID
	resourceVersion := lease.ResourceVersion
	err := r.Delete(ctx, lease, &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *ModelArtifactReconciler) artifactLifecycleReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ModelArtifactReconciler) reconcileDelete(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (ctrl.Result, error) {
	deleteArtifactGaugeSeries(modelArtifact.Namespace, modelArtifact.Name)
	if !controllerutil.ContainsFinalizer(modelArtifact, kamav1alpha1.ModelArtifactFinalizer) {
		return ctrl.Result{}, nil
	}
	referenced, err := r.modelArtifactHasDeploymentReferences(ctx, modelArtifact)
	if err != nil {
		return ctrl.Result{}, err
	}
	if referenced {
		// Keep the artifact identity and backing storage intact until every
		// referring ModelDeployment has completed its drain-first finalizer.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	cleaning, err := r.cleanupArtifactImporterResources(ctx, modelArtifact)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cleaning {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	cleaning, err = r.cleanupArtifactLeases(ctx, modelArtifact)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cleaning {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if completedOperation := modelArtifact.Status.CleanupOperationID; completedOperation != "" {
		if !validCleanupOperationID(completedOperation) {
			return ctrl.Result{}, errors.New("refusing invalid artifact cleanup completion identity")
		}
		cleaning, err = r.cleanupDetachedArtifactResources(ctx, modelArtifact)
		if err != nil {
			return ctrl.Result{}, err
		}
		if cleaning {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return r.finalizeArtifactDeletion(ctx, modelArtifact)
	}

	storage, cleanupRequired, err := r.resolveArtifactCleanupStorage(ctx, modelArtifact)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !cleanupRequired {
		// Direct artifacts never mount a cache. This also covers cached artifacts
		// deleted before a Ready cache existed, when no importer operation could
		// have created transient state.
		cleaning, err = r.cleanupDetachedArtifactResources(ctx, modelArtifact)
		if err != nil {
			return ctrl.Result{}, err
		}
		if cleaning {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return r.finalizeArtifactDeletion(ctx, modelArtifact)
	}
	operation, err := artifactCleanupOperationID(modelArtifact, storage)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileArtifactCleanupJob(ctx, modelArtifact, storage, operation)
}

func (r *ModelArtifactReconciler) modelArtifactHasDeploymentReferences(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (bool, error) {
	var deployments kamav1alpha1.ModelDeploymentList
	if err := r.artifactLifecycleReader().List(
		ctx, &deployments, client.InNamespace(modelArtifact.Namespace),
	); err != nil {
		return false, err
	}
	for index := range deployments.Items {
		if deployments.Items[index].Spec.ModelRef.Name == modelArtifact.Name {
			return true, nil
		}
	}

	// A mutable or deleting ModelDeployment can disappear or point at a new
	// artifact before a defensive drain completes. Keep the old storage
	// identity held while a controller-labeled serving Pod still mounts its
	// exact claim/subpath, even if the referring API object has gone away.
	var pods corev1.PodList
	if err := r.artifactLifecycleReader().List(
		ctx, &pods, client.InNamespace(modelArtifact.Namespace),
		client.MatchingLabels{artifactUIDLabel: boundedLabelValue(string(modelArtifact.UID))},
	); err != nil {
		return false, err
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Labels[managedByLabel] != kamaName ||
			pod.Labels[componentLabel] != modelDeploymentComponent {
			continue
		}
		if podMountsArtifactLocation(pod, modelArtifact.Status.Location) {
			return true, nil
		}
	}
	return false, nil
}

func podMountsArtifactLocation(pod *corev1.Pod, location *kamav1alpha1.ModelArtifactLocationStatus) bool {
	if location == nil || location.ClaimName == "" || location.SubPath == "" {
		return false
	}
	volumeFound := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == runtimeModelVolumeName && volume.PersistentVolumeClaim != nil &&
			volume.PersistentVolumeClaim.ClaimName == location.ClaimName && volume.PersistentVolumeClaim.ReadOnly {
			volumeFound = true
			break
		}
	}
	if !volumeFound {
		return false
	}
	expectedSubPath := location.SubPath
	if expectedSubPath == "." {
		expectedSubPath = ""
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != runtimeContainerName {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == runtimeModelVolumeName && mount.MountPath == runtimeModelMount &&
				mount.SubPath == expectedSubPath && mount.ReadOnly {
				return true
			}
		}
	}
	return false
}

// cleanupArtifactImporterResources stops ordinary importer Pods before a
// detached cleanup Job touches the same cache, then removes their retained
// audit ConfigMaps. Only resources controller-owned by this exact artifact UID
// are eligible.
func (r *ModelArtifactReconciler) cleanupArtifactImporterResources(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (bool, error) {
	labels := client.MatchingLabels{artifactUIDLabel: string(modelArtifact.UID)}
	var jobs batchv1.JobList
	if err := r.artifactLifecycleReader().List(
		ctx, &jobs, client.InNamespace(modelArtifact.Namespace), labels,
	); err != nil {
		return false, err
	}
	jobsRemain := false
	for index := range jobs.Items {
		job := &jobs.Items[index]
		if job.Labels[managedByLabel] != kamaName || job.Labels[componentLabel] != artifactImportComponent ||
			!hasControllerOwner(job.OwnerReferences, modelArtifact.UID, "ModelArtifact") {
			continue
		}
		jobsRemain = true
		if job.DeletionTimestamp.IsZero() {
			if err := deleteForeground(ctx, r.Client, job); err != nil {
				return false, err
			}
		}
	}
	if jobsRemain {
		// Foreground deletion ensures every importer Pod has stopped before the
		// cleanup Job is allowed to mount and mutate transient cache state.
		return true, nil
	}

	var configMaps corev1.ConfigMapList
	if err := r.artifactLifecycleReader().List(
		ctx, &configMaps, client.InNamespace(modelArtifact.Namespace), labels,
	); err != nil {
		return false, err
	}
	configsRemain := false
	for index := range configMaps.Items {
		configMap := &configMaps.Items[index]
		if configMap.Labels[managedByLabel] != kamaName || configMap.Labels[componentLabel] != artifactImportComponent ||
			!hasControllerOwner(configMap.OwnerReferences, modelArtifact.UID, "ModelArtifact") {
			continue
		}
		configsRemain = true
		if configMap.DeletionTimestamp.IsZero() {
			if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
				return false, err
			}
		}
	}
	return configsRemain, nil
}

func (r *ModelArtifactReconciler) cleanupArtifactLeases(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (bool, error) {
	leases, err := r.listArtifactLeases(ctx, modelArtifact.Namespace)
	if err != nil {
		return false, err
	}
	leasesRemain := false
	for index := range leases {
		lease := &leases[index]
		if lease.Labels[managedByLabel] != kamaName || lease.Labels[componentLabel] != artifactImportComponent {
			continue
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(modelArtifact.UID) {
			continue
		}
		if lease.Labels[leaseFingerprintLabel] == "" {
			return false, errors.New("refusing artifact Lease with incomplete Kama identity")
		}
		leasesRemain = true
		if lease.DeletionTimestamp.IsZero() {
			if err := r.deleteArtifactLease(ctx, lease); err != nil {
				return false, err
			}
		}
	}
	return leasesRemain, nil
}

type artifactCleanupStorage struct {
	claimName string
	claimUID  types.UID
	volumeUID types.UID
}

func (r *ModelArtifactReconciler) resolveArtifactCleanupStorage(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (artifactCleanupStorage, bool, error) {
	if modelArtifact.Spec.CacheRef == nil {
		return artifactCleanupStorage{}, false, nil
	}
	if modelArtifact.Status.Location != nil {
		return r.resolveArtifactCleanupFromLocation(ctx, modelArtifact)
	}

	var cache kamav1alpha1.ModelCache
	reader := r.artifactLifecycleReader()
	err := reader.Get(ctx, types.NamespacedName{
		Namespace: modelArtifact.Namespace,
		Name:      modelArtifact.Spec.CacheRef.Name,
	}, &cache)
	if apierrors.IsNotFound(err) {
		return r.resolveArtifactCleanupFromLocation(ctx, modelArtifact)
	}
	if err != nil {
		return artifactCleanupStorage{}, false, err
	}
	if cache.Status.ClaimName == "" || cache.Status.ClaimUID == "" {
		// Artifact reconciliation requires a Ready cache with this identity before
		// it creates an importer Job. A recorded Job without the durable location
		// checkpoint is ambiguous and must never be treated as proof that cleanup
		// can be skipped.
		if modelArtifact.Status.JobRef != nil {
			return artifactCleanupStorage{}, false,
				errors.New("artifact cleanup storage identity is unavailable for a recorded importer Job")
		}
		return artifactCleanupStorage{}, false, nil
	}
	if cache.Spec.Storage.ExistingClaim != nil && cache.Spec.Storage.ExistingClaim.Name != cache.Status.ClaimName {
		return artifactCleanupStorage{}, false, errors.New("refusing cleanup with mismatched adopted cache claim identity")
	}
	if cache.Spec.Storage.ClaimTemplate != nil {
		expected := deterministicName(cache.Name+"-cache", string(cache.UID))
		if cache.Status.ClaimName != expected {
			return artifactCleanupStorage{}, false, errors.New("refusing cleanup with mismatched managed cache claim identity")
		}
	}

	var claim corev1.PersistentVolumeClaim
	if err := reader.Get(ctx, types.NamespacedName{
		Namespace: modelArtifact.Namespace,
		Name:      cache.Status.ClaimName,
	}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return artifactCleanupStorage{}, false, nil
		}
		return artifactCleanupStorage{}, false, err
	}
	if claim.UID != cache.Status.ClaimUID {
		return artifactCleanupStorage{}, false, errors.New("refusing to mount replacement cache claim during artifact cleanup")
	}
	if !claim.DeletionTimestamp.IsZero() {
		return artifactCleanupStorage{}, false, errors.New("artifact cleanup cache claim is terminating")
	}
	if cache.Spec.Storage.ClaimTemplate != nil &&
		(claim.Labels[managedByLabel] != kamaName || claim.Labels[componentLabel] != modelCacheComponent ||
			claim.Labels[cacheNameLabel] != boundedLabelValue(cache.Name) ||
			claim.Labels[cacheUIDLabel] != string(cache.UID)) {
		return artifactCleanupStorage{}, false, errors.New("refusing cleanup with mismatched managed cache claim labels")
	}
	identity, err := r.resolveVolumeWithReader(ctx, reader, &claim)
	if err != nil {
		return artifactCleanupStorage{}, false, fmt.Errorf("resolve cleanup cache volume: %w", err)
	}
	if claim.Status.Phase != corev1.ClaimBound || !hasWritableMode(identity.AccessModes) {
		return artifactCleanupStorage{}, false, errors.New("artifact cleanup cache claim is not bound and writable")
	}
	if cache.Status.VolumeName != "" && cache.Status.VolumeName != identity.VolumeName ||
		cache.Status.VolumeUID != "" && cache.Status.VolumeUID != identity.VolumeUID {
		return artifactCleanupStorage{}, false, errors.New("refusing cleanup after cache volume identity changed")
	}
	return artifactCleanupStorage{
		claimName: claim.Name,
		claimUID:  claim.UID,
		volumeUID: identity.VolumeUID,
	}, true, nil
}

func (r *ModelArtifactReconciler) resolveArtifactCleanupFromLocation(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (artifactCleanupStorage, bool, error) {
	location := modelArtifact.Status.Location
	if location == nil {
		// The referenced ModelCache normally cannot disappear while this artifact
		// exists. New reconciliation checkpoints storage before importer creation;
		// this fallback is only for an artifact that never started an operation.
		if modelArtifact.Status.JobRef != nil {
			return artifactCleanupStorage{}, false,
				errors.New("artifact cleanup location is unavailable for a recorded importer Job")
		}
		return artifactCleanupStorage{}, false, nil
	}
	if location.ClaimName == "" || location.ClaimUID == "" {
		return artifactCleanupStorage{}, false, errors.New("artifact cleanup location has incomplete claim identity")
	}
	reader := r.artifactLifecycleReader()
	var claim corev1.PersistentVolumeClaim
	if err := reader.Get(ctx, types.NamespacedName{
		Namespace: modelArtifact.Namespace,
		Name:      location.ClaimName,
	}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return artifactCleanupStorage{}, false, nil
		}
		return artifactCleanupStorage{}, false, err
	}
	if claim.UID != location.ClaimUID {
		return artifactCleanupStorage{}, false, errors.New("refusing to mount replacement serving claim during artifact cleanup")
	}
	if !claim.DeletionTimestamp.IsZero() {
		return artifactCleanupStorage{}, false, errors.New("artifact cleanup serving claim is terminating")
	}
	identity, err := r.resolveVolumeWithReader(ctx, reader, &claim)
	if err != nil {
		return artifactCleanupStorage{}, false, fmt.Errorf("resolve cleanup serving volume: %w", err)
	}
	if claim.Status.Phase != corev1.ClaimBound || !hasWritableMode(identity.AccessModes) {
		return artifactCleanupStorage{}, false, errors.New("artifact cleanup serving claim is not bound and writable")
	}
	if location.VolumeName != "" && location.VolumeName != identity.VolumeName ||
		location.VolumeUID != "" && location.VolumeUID != identity.VolumeUID {
		return artifactCleanupStorage{}, false, errors.New("refusing cleanup after serving volume identity changed")
	}
	return artifactCleanupStorage{
		claimName: claim.Name,
		claimUID:  claim.UID,
		volumeUID: identity.VolumeUID,
	}, true, nil
}

func artifactCleanupOperationID(
	modelArtifact *kamav1alpha1.ModelArtifact,
	storage artifactCleanupStorage,
) (string, error) {
	prefix := string(modelArtifact.UID) + "-"
	if err := artifact.ValidateOperationPrefix(prefix); err != nil {
		return "", fmt.Errorf("validate artifact cleanup identity: %w", err)
	}
	return operationID(
		artifact.CleanupSpec{OperationPrefix: prefix},
		string(modelArtifact.UID),
		storage.claimName,
		string(storage.claimUID),
		string(storage.volumeUID),
	)
}

func (r *ModelArtifactReconciler) artifactCleanupResources(
	modelArtifact *kamav1alpha1.ModelArtifact,
	storage artifactCleanupStorage,
	operation string,
) (*corev1.ConfigMap, *batchv1.Job, error) {
	spec := artifact.Spec{
		SchemaVersion: artifact.SchemaVersion,
		Mode:          artifact.ModeCleanup,
		OperationID:   operation,
		CacheRoot:     cacheMountPath,
		Cleanup: &artifact.CleanupSpec{
			OperationPrefix: string(modelArtifact.UID) + "-",
		},
	}
	configName := deterministicName(modelArtifact.Name+"-cleanup-config", operation)
	jobName := deterministicName(modelArtifact.Name+"-cleanup", operation)
	configMap, err := newSpecConfigMap(modelArtifact, r.Scheme, configName, spec)
	if err != nil {
		return nil, nil, err
	}
	configMap.OwnerReferences = nil
	configMap.Labels[componentLabel] = artifactCleanupComponent
	configMap.Labels[artifactNameLabel] = boundedLabelValue(modelArtifact.Name)
	configMap.Labels[artifactUIDLabel] = string(modelArtifact.UID)
	configMap.Labels[operationIDLabel] = operation

	job, err := newImportJob(r.Scheme, r.Importer, importJobOptions{
		Owner: modelArtifact, Name: jobName, ConfigMapName: configName,
		CacheClaim: storage.claimName, OperationID: operation,
	})
	if err != nil {
		return nil, nil, err
	}
	job.OwnerReferences = nil
	job.Labels[componentLabel] = artifactCleanupComponent
	job.Labels[artifactNameLabel] = boundedLabelValue(modelArtifact.Name)
	job.Labels[artifactUIDLabel] = string(modelArtifact.UID)
	job.Spec.Template.Labels[componentLabel] = artifactCleanupComponent
	job.Spec.Template.Labels[artifactNameLabel] = boundedLabelValue(modelArtifact.Name)
	job.Spec.Template.Labels[artifactUIDLabel] = string(modelArtifact.UID)
	return configMap, job, nil
}

func (r *ModelArtifactReconciler) reconcileArtifactCleanupJob(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
	storage artifactCleanupStorage,
	operation string,
) (ctrl.Result, error) {
	configMap, job, err := r.artifactCleanupResources(modelArtifact, storage, operation)
	if err != nil {
		return ctrl.Result{}, err
	}
	reader := r.artifactLifecycleReader()
	if err := ensureObjectWithReader(ctx, r.Client, reader, configMap); err != nil {
		return ctrl.Result{}, fmt.Errorf("create artifact cleanup config: %w", err)
	}
	if err := ensureObjectWithReader(ctx, r.Client, reader, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("create artifact cleanup Job: %w", err)
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
		return ctrl.Result{}, err
	}
	if !jobComplete(job) && !jobFailed(job) {
		return ctrl.Result{RequeueAfter: activeCleanupRequeue}, nil
	}

	result, resultErr := r.readJobResultWithReader(ctx, reader, job)
	if resultErr != nil || !validArtifactCleanupResult(result, operation) {
		if job.DeletionTimestamp.IsZero() {
			if err := deleteForeground(ctx, r.Client, job); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: cleanupRetryDelay}, nil
	}
	modelArtifact.Status.CleanupOperationID = operation
	if err := r.Status().Update(ctx, modelArtifact); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func validArtifactCleanupResult(result artifact.Result, operation string) bool {
	return result.SchemaVersion == artifact.SchemaVersion && result.Mode == artifact.ModeCleanup &&
		result.OperationID == operation && result.Success && result.Reason == "" &&
		result.Manifest == nil && result.GGUF == nil && result.Probe == nil &&
		result.ArtifactDigest == "" && result.ResolvedRevision == "" && result.PublishedPath == "" &&
		result.BytesTransferred == 0 && !result.CacheHit
}

func validCleanupOperationID(value string) bool {
	if len(value) != 20 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func (r *ModelArtifactReconciler) cleanupDetachedArtifactResources(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (bool, error) {
	// A protected status checkpoint proves one exact cleanup operation completed.
	// Remove every otherwise-valid detached retry for this artifact UID so an
	// earlier operation identity cannot wedge finalization after storage recovery.
	labels := client.MatchingLabels{artifactUIDLabel: string(modelArtifact.UID)}
	var jobs batchv1.JobList
	if err := r.artifactLifecycleReader().List(
		ctx, &jobs, client.InNamespace(modelArtifact.Namespace), labels,
	); err != nil {
		return false, err
	}
	jobsRemain := false
	for index := range jobs.Items {
		job := &jobs.Items[index]
		if job.Labels[componentLabel] != artifactCleanupComponent {
			continue
		}
		if !validDetachedCleanupMetadata(job.ObjectMeta, modelArtifact) {
			return false, fmt.Errorf("refusing cleanup Job %s/%s with mismatched detached identity", job.Namespace, job.Name)
		}
		jobsRemain = true
		if job.DeletionTimestamp.IsZero() {
			if err := deleteForeground(ctx, r.Client, job); err != nil {
				return false, err
			}
		}
	}
	if jobsRemain {
		return true, nil
	}

	var configMaps corev1.ConfigMapList
	if err := r.artifactLifecycleReader().List(
		ctx, &configMaps, client.InNamespace(modelArtifact.Namespace), labels,
	); err != nil {
		return false, err
	}
	configsRemain := false
	for index := range configMaps.Items {
		configMap := &configMaps.Items[index]
		if configMap.Labels[componentLabel] != artifactCleanupComponent {
			continue
		}
		if !validDetachedCleanupMetadata(configMap.ObjectMeta, modelArtifact) {
			return false, fmt.Errorf("refusing cleanup ConfigMap %s/%s with mismatched detached identity", configMap.Namespace, configMap.Name)
		}
		configsRemain = true
		if configMap.DeletionTimestamp.IsZero() {
			if err := deleteIfPresent(ctx, r.Client, configMap); err != nil {
				return false, err
			}
		}
	}
	return configsRemain, nil
}

func validDetachedCleanupMetadata(metadata metav1.ObjectMeta, modelArtifact *kamav1alpha1.ModelArtifact) bool {
	return len(metadata.OwnerReferences) == 0 && metadata.Labels[managedByLabel] == kamaName &&
		metadata.Labels[componentLabel] == artifactCleanupComponent &&
		metadata.Labels[artifactNameLabel] == boundedLabelValue(modelArtifact.Name) &&
		metadata.Labels[artifactUIDLabel] == string(modelArtifact.UID) &&
		validCleanupOperationID(metadata.Labels[operationIDLabel])
}

func (r *ModelArtifactReconciler) finalizeArtifactDeletion(
	ctx context.Context,
	modelArtifact *kamav1alpha1.ModelArtifact,
) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(modelArtifact, kamav1alpha1.ModelArtifactFinalizer)
	if err := r.Update(ctx, modelArtifact); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the ModelArtifact controller and owned resources.
func (r *ModelArtifactReconciler) SetupWithManager(manager ctrl.Manager) error {
	if err := r.validate(); err != nil {
		return err
	}
	r.APIReader = manager.GetAPIReader()
	registerControllerMetrics()
	return ctrl.NewControllerManagedBy(manager).
		For(&kamav1alpha1.ModelArtifact{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&kamav1alpha1.ModelCache{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, object client.Object) []reconcile.Request {
				var artifacts kamav1alpha1.ModelArtifactList
				if err := r.List(ctx, &artifacts, client.InNamespace(object.GetNamespace())); err != nil {
					return nil
				}
				requests := make([]reconcile.Request, 0)
				for index := range artifacts.Items {
					item := &artifacts.Items[index]
					if item.Spec.CacheRef != nil && item.Spec.CacheRef.Name == object.GetName() {
						requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(item)})
					}
				}
				return requests
			},
		)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, object client.Object) []reconcile.Request {
				var artifacts kamav1alpha1.ModelArtifactList
				if err := r.List(ctx, &artifacts, client.InNamespace(object.GetNamespace())); err != nil {
					return nil
				}
				requests := make([]reconcile.Request, 0)
				for index := range artifacts.Items {
					item := &artifacts.Items[index]
					source := item.Spec.Source.PersistentVolumeClaim
					if source != nil && source.ClaimName == object.GetName() {
						requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(item)})
					}
				}
				return requests
			},
		)).
		Complete(r)
}
