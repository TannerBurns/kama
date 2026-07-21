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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	kamaruntime "github.com/TannerBurns/kama/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	modelDeploymentComponent                = "model-serving"
	replicaSetKind                          = "ReplicaSet"
	modelDeploymentNameLabel                = "kama.tannerburns.github.io/model-deployment"
	modelDeploymentUIDLabel                 = "kama.tannerburns.github.io/model-deployment-uid"
	runtimeFingerprintLabel                 = "kama.tannerburns.github.io/runtime-fingerprint"
	runtimeFingerprintAnnotation            = "kama.tannerburns.github.io/runtime-fingerprint-full"
	artifactLocationHashAnnotation          = "kama.tannerburns.github.io/artifact-location-hash"
	runtimeConfigKey                        = "config.json"
	runtimeModelVolumeName                  = "model"
	runtimeConfigMount                      = "/etc/kama/runtime"
	runtimeConfigPath                       = runtimeConfigMount + "/" + runtimeConfigKey
	runtimeModelMount                       = "/models"
	runtimeTemporaryMount                   = "/tmp"
	runtimeContainerName                    = "runtime"
	runtimeHTTPPort                   int32 = 8080
	supervisorHTTPPort                int32 = 8081
	defaultRuntimePoll                      = 10 * time.Second
	defaultRuntimeHTTPTimeout               = 2 * time.Second
	runtimePendingReason                    = "WorkloadPending"
	runtimeMetricStateNone                  = "None"
	runtimeMetricStateLoadFailed            = "LoadFailed"
	modelDeploymentMetricLabel              = "model_deployment"
	artifactUnavailableSchedulingGate       = "kama.tannerburns.github.io/artifact-ready"
)

// RuntimeOptions configure the controller-owned serving images and immutable
// llama.cpp build identity. They are deliberately not exposed through the CRD.
type RuntimeOptions struct {
	CPUImage         string
	CUDAImage        string
	PullPolicy       corev1.PullPolicy
	ImagePullSecrets []corev1.LocalObjectReference
	LlamaCommit      string
}

type generatedResourceCollisionError struct {
	message string
}

func (err *generatedResourceCollisionError) Error() string {
	return err.message
}

func resourceCollisionf(format string, arguments ...any) error {
	return &generatedResourceCollisionError{message: fmt.Sprintf(format, arguments...)}
}

func isGeneratedResourceCollision(err error) bool {
	var collision *generatedResourceCollisionError
	return errors.As(err, &collision)
}

// Validate checks cluster-level runtime configuration before controllers start.
func (o RuntimeOptions) Validate() error {
	if strings.TrimSpace(o.CPUImage) == "" || strings.TrimSpace(o.CUDAImage) == "" {
		return errors.New("CPU and CUDA runtime images must not be empty")
	}
	switch o.PullPolicy {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		return fmt.Errorf("unsupported runtime image pull policy %q", o.PullPolicy)
	}
	if len(o.LlamaCommit) != 40 {
		return errors.New("llama.cpp commit must be a full 40-character commit")
	}
	for _, character := range o.LlamaCommit {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return errors.New("llama.cpp commit must be lowercase hexadecimal")
		}
	}
	for _, secret := range o.ImagePullSecrets {
		if problems := validation.IsDNS1123Subdomain(secret.Name); len(problems) != 0 {
			return fmt.Errorf("invalid runtime image pull Secret %q: %s", secret.Name, strings.Join(problems, "; "))
		}
	}
	return nil
}

// ModelDeploymentReconciler creates one fixed serving workload for a ready artifact.
type ModelDeploymentReconciler struct {
	client.Client
	APIReader  client.Reader
	Scheme     *runtime.Scheme
	Recorder   events.EventRecorder
	Runtime    RuntimeOptions
	HTTPClient *http.Client
}

// NewModelDeploymentReconciler builds a ModelDeployment reconciler.
func NewModelDeploymentReconciler(
	kubeClient client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	recorder events.EventRecorder,
	runtimeOptions RuntimeOptions,
) *ModelDeploymentReconciler {
	return &ModelDeploymentReconciler{
		Client: kubeClient, APIReader: apiReader, Scheme: scheme, Recorder: recorder, Runtime: runtimeOptions,
		HTTPClient: &http.Client{Timeout: defaultRuntimeHTTPTimeout},
	}
}

// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modeldeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=kama.tannerburns.github.io,resources=modelartifacts,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile converges a stable internal Service and at most one serving Pod.
func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var deployment kamav1alpha1.ModelDeployment
	if err := r.Get(ctx, request.NamespacedName, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			deleteModelDeploymentMetrics(request.Namespace, request.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !deployment.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &deployment)
	}
	if !controllerutil.ContainsFinalizer(&deployment, kamav1alpha1.ModelDeploymentFinalizer) {
		controllerutil.AddFinalizer(&deployment, kamav1alpha1.ModelDeploymentFinalizer)
		if err := r.Update(ctx, &deployment); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	service, err := r.reconcileService(ctx, &deployment)
	if err != nil {
		if !isGeneratedResourceCollision(err) {
			return ctrl.Result{}, err
		}
		if drainErr := r.drainOwnedServingWorkload(ctx, &deployment); drainErr != nil {
			return ctrl.Result{}, drainErr
		}
		return r.failStatus(ctx, &deployment, nil, "GeneratedResourceCollision", err.Error())
	}
	if err := kamav1alpha1.ValidateModelDeployment(&deployment); err != nil {
		return r.reconcileInvalidSpec(ctx, &deployment, service, err)
	}

	var artifact kamav1alpha1.ModelArtifact
	err = r.Get(ctx, types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Spec.ModelRef.Name}, &artifact)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.reconcileUnavailableArtifact(ctx, &deployment, service, nil, "ArtifactNotFound",
			"referenced ModelArtifact does not exist")
	}

	usable, reason, message := artifactServingReady(&artifact)
	if usable {
		usable, reason, message, err = r.servingClaimIdentityReady(ctx, &artifact)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if !usable {
		return r.reconcileUnavailableArtifact(ctx, &deployment, service, &artifact, reason, message)
	}

	fingerprint, locationHash, config, image, err := r.desiredRuntime(&deployment, &artifact)
	if err != nil {
		if drainErr := r.drainOwnedServingWorkload(ctx, &deployment); drainErr != nil {
			return ctrl.Result{}, drainErr
		}
		return r.failStatus(ctx, &deployment, &artifact, "ConfigurationRejected", err.Error())
	}
	configMap, err := r.reconcileRuntimeConfig(ctx, &deployment, fingerprint, config)
	if err != nil {
		if !isGeneratedResourceCollision(err) {
			return ctrl.Result{}, err
		}
		if drainErr := r.drainOwnedServingWorkload(ctx, &deployment); drainErr != nil {
			return ctrl.Result{}, drainErr
		}
		return r.failStatus(ctx, &deployment, &artifact, "GeneratedResourceCollision", err.Error())
	}
	if err := r.resumeWorkloadReplacement(ctx, &deployment, &artifact, fingerprint, locationHash); err != nil {
		return ctrl.Result{}, err
	}
	workload, err := r.reconcileWorkload(ctx, &deployment, &artifact, configMap, fingerprint, locationHash, image)
	if err != nil {
		if !isGeneratedResourceCollision(err) {
			return ctrl.Result{}, err
		}
		return r.failStatus(ctx, &deployment, &artifact, "GeneratedResourceCollision", err.Error())
	}
	if err := r.cleanupObsoleteRuntimeConfigs(ctx, &deployment, configMap.Name); err != nil {
		return ctrl.Result{}, err
	}
	return r.observeRuntime(ctx, &deployment, &artifact, service, workload, fingerprint, image,
		metav1.ConditionTrue, "ArtifactReady", "referenced ModelArtifact is verified and mounted")
}

func (r *ModelDeploymentReconciler) drainOwnedServingWorkload(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
) error {
	var workload appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: deployment.Namespace, Name: servingObjectName(deployment),
	}, &workload); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if !hasControllerOwner(workload.OwnerReferences, deployment.UID, "ModelDeployment") ||
		workload.Labels[managedByLabel] != kamaName || workload.Labels[componentLabel] != modelDeploymentComponent ||
		!workload.DeletionTimestamp.IsZero() {
		return nil
	}
	return deleteForeground(ctx, r.Client, &workload)
}

func (r *ModelDeploymentReconciler) reconcileInvalidSpec(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	service *corev1.Service,
	validationError error,
) (ctrl.Result, error) {
	var workload appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{
		Namespace: deployment.Namespace, Name: servingObjectName(deployment),
	}, &workload)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil && hasControllerOwner(workload.OwnerReferences, deployment.UID, "ModelDeployment") &&
		workload.Labels[managedByLabel] == kamaName && workload.Labels[componentLabel] == modelDeploymentComponent &&
		workload.DeletionTimestamp.IsZero() {
		if err := deleteForeground(ctx, r.Client, &workload); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.cleanupObsoleteRuntimeConfigs(ctx, deployment, ""); err != nil {
		return ctrl.Result{}, err
	}
	deployment.Status.DesiredReplicas = 0
	deployment.Status.ReadyReplicas = 0
	deployment.Status.DeploymentRef = nil
	deployment.Status.Runtime = nil
	deployment.Status.ServiceRef = &kamav1alpha1.ModelDeploymentObjectReference{Name: service.Name, UID: service.UID}
	deployment.Status.Artifact = &kamav1alpha1.ModelDeploymentArtifactStatus{Name: deployment.Spec.ModelRef.Name}
	return r.failStatus(ctx, deployment, nil, "InvalidSpec", validationError.Error())
}

func (r *ModelDeploymentReconciler) resumeWorkloadReplacement(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
	fingerprint, locationHash string,
) error {
	var workload appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: deployment.Namespace, Name: servingObjectName(deployment),
	}, &workload); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if !hasControllerOwner(workload.OwnerReferences, deployment.UID, "ModelDeployment") ||
		workload.Labels[managedByLabel] != kamaName || workload.Labels[componentLabel] != modelDeploymentComponent {
		return nil
	}
	if workload.Spec.Template.Annotations[runtimeFingerprintAnnotation] != fingerprint {
		return nil
	}
	ownedReplicaSets, err := r.ownedServingReplicaSetUIDs(ctx, deployment, &workload)
	if err != nil {
		return err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(deployment.Namespace),
		client.MatchingLabels{modelDeploymentUIDLabel: string(deployment.UID)}); err != nil {
		return err
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Labels[managedByLabel] != kamaName || pod.Labels[componentLabel] != modelDeploymentComponent ||
			pod.Annotations[runtimeFingerprintAnnotation] != fingerprint ||
			!ownedByAnyReplicaSet(pod.OwnerReferences, ownedReplicaSets) ||
			!hasArtifactUnavailableSchedulingGate(pod.Spec.SchedulingGates) {
			continue
		}
		allowed, err := r.currentArtifactAllowsScheduling(ctx, deployment, artifact, fingerprint, locationHash)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}
		updated := pod.DeepCopy()
		updated.Spec.SchedulingGates = withoutSchedulingGate(updated.Spec.SchedulingGates, artifactUnavailableSchedulingGate)
		if err := r.Update(ctx, updated); err != nil {
			return err
		}
	}
	return nil
}

func (r *ModelDeploymentReconciler) ownedServingReplicaSetUIDs(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	workload *appsv1.Deployment,
) (map[types.UID]struct{}, error) {
	var replicaSets appsv1.ReplicaSetList
	if err := r.List(ctx, &replicaSets, client.InNamespace(deployment.Namespace),
		client.MatchingLabels{modelDeploymentUIDLabel: string(deployment.UID)}); err != nil {
		return nil, err
	}
	owned := make(map[types.UID]struct{}, len(replicaSets.Items))
	for index := range replicaSets.Items {
		replicaSet := &replicaSets.Items[index]
		if replicaSet.Labels[managedByLabel] != kamaName ||
			replicaSet.Labels[componentLabel] != modelDeploymentComponent ||
			!hasControllerOwner(replicaSet.OwnerReferences, workload.UID, "Deployment") {
			continue
		}
		owned[replicaSet.UID] = struct{}{}
	}
	return owned, nil
}

func ownedByAnyReplicaSet(references []metav1.OwnerReference, replicaSetUIDs map[types.UID]struct{}) bool {
	for _, reference := range references {
		if reference.Controller == nil || !*reference.Controller || reference.Kind != replicaSetKind {
			continue
		}
		if _, found := replicaSetUIDs[reference.UID]; found {
			return true
		}
	}
	return false
}

func (r *ModelDeploymentReconciler) suspendWorkloadReplacement(
	ctx context.Context,
	workload *appsv1.Deployment,
) error {
	if !workload.Spec.Paused {
		updated := workload.DeepCopy()
		updated.Spec.Paused = true
		if err := r.Update(ctx, updated); err != nil {
			return err
		}
		*workload = *updated
	}
	return nil
}

func hasArtifactUnavailableSchedulingGate(gates []corev1.PodSchedulingGate) bool {
	for _, gate := range gates {
		if gate.Name == artifactUnavailableSchedulingGate {
			return true
		}
	}
	return false
}

func withoutSchedulingGate(gates []corev1.PodSchedulingGate, name string) []corev1.PodSchedulingGate {
	result := make([]corev1.PodSchedulingGate, 0, len(gates))
	for _, gate := range gates {
		if gate.Name != name {
			result = append(result, gate)
		}
	}
	return result
}

func (r *ModelDeploymentReconciler) desiredRuntime(
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
) (string, string, kamaruntime.Config, string, error) {
	image := r.Runtime.CPUImage
	if deployment.Spec.Placement.Mode == kamav1alpha1.ModelDeploymentPlacementAccelerator {
		image = r.Runtime.CUDAImage
	}
	locationHash, err := operationID(artifact.Status.Location)
	if err != nil {
		return "", "", kamaruntime.Config{}, "", fmt.Errorf("fingerprint artifact location: %w", err)
	}
	identity := struct {
		SchemaVersion string
		Namespace     string
		Name          string
		UID           types.UID
		Spec          kamav1alpha1.ModelDeploymentSpec
		ArtifactUID   types.UID
		Digest        string
		Entrypoint    string
		Files         []kamav1alpha1.ModelArtifactFileStatus
		Location      *kamav1alpha1.ModelArtifactLocationStatus
		Image         string
		PullPolicy    corev1.PullPolicy
		PullSecrets   []corev1.LocalObjectReference
		LlamaCommit   string
	}{
		SchemaVersion: kamaruntime.SchemaVersion,
		Namespace:     deployment.Namespace, Name: deployment.Name, UID: deployment.UID,
		Spec: deployment.Spec, ArtifactUID: artifact.UID, Digest: artifact.Status.ArtifactDigest,
		Entrypoint: artifact.Spec.Entrypoint, Files: artifact.Status.Files,
		Location: artifact.Status.Location, Image: image, PullPolicy: r.Runtime.PullPolicy,
		PullSecrets: r.Runtime.ImagePullSecrets, LlamaCommit: r.Runtime.LlamaCommit,
	}
	fingerprint, err := operationID(identity)
	if err != nil {
		return "", "", kamaruntime.Config{}, "", fmt.Errorf("fingerprint runtime: %w", err)
	}
	files := make([]kamaruntime.ArtifactFile, 0, len(artifact.Status.Files))
	for _, file := range artifact.Status.Files {
		files = append(files, kamaruntime.ArtifactFile{Path: file.Path, Size: file.Size, SHA256: file.SHA256})
	}
	config := kamaruntime.Config{
		SchemaVersion: kamaruntime.SchemaVersion,
		Deployment: kamaruntime.DeploymentIdentity{
			Namespace: deployment.Namespace, Name: deployment.Name, UID: string(deployment.UID), Fingerprint: fingerprint,
		},
		Artifact: kamaruntime.ArtifactIdentity{
			UID: string(artifact.UID), Digest: artifact.Status.ArtifactDigest,
			Entrypoint: artifact.Spec.Entrypoint, Files: files,
		},
		Mode:                kamaruntime.Mode(deployment.Spec.Placement.Mode),
		MaxContextTokens:    int64PointerValue(deployment.Spec.Runtime.MaxContextTokens),
		DesiredConcurrency:  int32PointerValue(deployment.Spec.Runtime.DesiredConcurrency),
		DrainTimeoutSeconds: int64(math.Ceil(runtimeDrainDuration(deployment).Seconds())),
		KVCache: kamaruntime.KVCacheConfig{
			KeyType: string(deployment.Spec.Runtime.KVCache.KeyType), ValueType: string(deployment.Spec.Runtime.KVCache.ValueType),
		},
		Expert: kamaruntime.ExpertConfig{
			BatchSize:      int32PointerValue(deployment.Spec.Runtime.Expert.BatchSize),
			MicroBatchSize: int32PointerValue(deployment.Spec.Runtime.Expert.MicroBatchSize),
			Threads:        int32PointerValue(deployment.Spec.Runtime.Expert.Threads),
			BatchThreads:   int32PointerValue(deployment.Spec.Runtime.Expert.BatchThreads),
			FlashAttention: kamaruntime.FlashAttention(deployment.Spec.Runtime.Expert.FlashAttention),
		},
	}
	config.Default()
	if err := config.Validate(); err != nil {
		return "", "", kamaruntime.Config{}, "", fmt.Errorf("validate runtime config: %w", err)
	}
	return fingerprint, locationHash, config, image, nil
}

func int32PointerValue(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func int64PointerValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func runtimeDrainDuration(deployment *kamav1alpha1.ModelDeployment) time.Duration {
	if deployment.Spec.Runtime.DrainTimeout == nil || deployment.Spec.Runtime.DrainTimeout.Duration <= 0 {
		return kamav1alpha1.DefaultModelDeploymentDrainTimeout
	}
	return deployment.Spec.Runtime.DrainTimeout.Duration
}

func artifactServingReady(artifact *kamav1alpha1.ModelArtifact) (bool, string, string) {
	if !artifact.DeletionTimestamp.IsZero() {
		return false, "ArtifactDeleting", "referenced ModelArtifact is pending deletion"
	}
	condition := meta.FindStatusCondition(artifact.Status.Conditions, kamav1alpha1.ModelArtifactConditionReady)
	if artifact.Status.ObservedGeneration != artifact.Generation || condition == nil ||
		condition.ObservedGeneration != artifact.Generation {
		return false, "ArtifactStatusStale", "referenced ModelArtifact has not reconciled its current generation"
	}
	if condition.Status != metav1.ConditionTrue {
		reason := condition.Reason
		if reason == "" {
			reason = "ArtifactNotReady"
		}
		message := condition.Message
		if message == "" {
			message = "referenced ModelArtifact is not ready"
		}
		return false, reason, message
	}
	location := artifact.Status.Location
	if artifact.UID == "" || artifact.Status.ArtifactDigest == "" || location == nil ||
		location.ClaimName == "" || location.ClaimUID == "" || location.SubPath == "" || !location.ReadOnly {
		return false, "InvalidArtifactIdentity", "referenced ModelArtifact lacks a complete immutable serving identity"
	}
	return true, "ArtifactReady", "referenced ModelArtifact is verified and ready"
}

func (r *ModelDeploymentReconciler) servingClaimIdentityReady(
	ctx context.Context,
	artifact *kamav1alpha1.ModelArtifact,
) (bool, string, string, error) {
	return servingClaimIdentityReadyWithReader(ctx, r.Client, artifact)
}

func servingClaimIdentityReadyWithReader(
	ctx context.Context,
	reader client.Reader,
	artifact *kamav1alpha1.ModelArtifact,
) (bool, string, string, error) {
	var claim corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: artifact.Namespace, Name: artifact.Status.Location.ClaimName}
	if err := reader.Get(ctx, key, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "ArtifactClaimNotFound", "artifact PersistentVolumeClaim does not exist", nil
		}
		return false, "", "", err
	}
	if !claim.DeletionTimestamp.IsZero() {
		return false, "ArtifactClaimDeleting", "artifact PersistentVolumeClaim is pending deletion", nil
	}
	if claim.UID != artifact.Status.Location.ClaimUID {
		return false, "ArtifactClaimIdentityChanged", "artifact PersistentVolumeClaim identity no longer matches status", nil
	}
	if claim.Status.Phase != corev1.ClaimBound {
		return false, "ArtifactClaimNotBound", "artifact PersistentVolumeClaim is not Bound", nil
	}
	return true, "ArtifactReady", "artifact claim identity is available", nil
}

func (r *ModelDeploymentReconciler) currentArtifactAllowsScheduling(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
	fingerprint, locationHash string,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var currentDeployment kamav1alpha1.ModelDeployment
	if err := reader.Get(ctx, client.ObjectKeyFromObject(deployment), &currentDeployment); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if currentDeployment.UID != deployment.UID || currentDeployment.Generation != deployment.Generation ||
		!currentDeployment.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if err := kamav1alpha1.ValidateModelDeployment(&currentDeployment); err != nil {
		return false, nil
	}

	var currentArtifact kamav1alpha1.ModelArtifact
	artifactKey := types.NamespacedName{
		Namespace: currentDeployment.Namespace,
		Name:      currentDeployment.Spec.ModelRef.Name,
	}
	if err := reader.Get(ctx, artifactKey, &currentArtifact); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if currentArtifact.UID != artifact.UID {
		return false, nil
	}
	ready, _, _ := artifactServingReady(&currentArtifact)
	if !ready {
		return false, nil
	}
	ready, _, _, err := servingClaimIdentityReadyWithReader(ctx, reader, &currentArtifact)
	if err != nil || !ready {
		return false, err
	}
	currentFingerprint, currentLocationHash, _, _, err := r.desiredRuntime(&currentDeployment, &currentArtifact)
	if err != nil {
		return false, nil
	}
	return currentFingerprint == fingerprint && currentLocationHash == locationHash, nil
}

func servingObjectName(deployment *kamav1alpha1.ModelDeployment) string {
	return deterministicName(deployment.Name+"-serve", string(deployment.UID))
}

func runtimeConfigName(deployment *kamav1alpha1.ModelDeployment, fingerprint string) string {
	return deterministicName(deployment.Name+"-runtime", fingerprint)
}

func servingLabels(deployment *kamav1alpha1.ModelDeployment) map[string]string {
	return map[string]string{
		managedByLabel:           kamaName,
		componentLabel:           modelDeploymentComponent,
		modelDeploymentNameLabel: boundedLabelValue(deployment.Name),
		modelDeploymentUIDLabel:  string(deployment.UID),
	}
}

// mergeMetadataMap converges Kama-owned metadata while retaining annotations
// and labels written by Kubernetes controllers. Deployments, in particular,
// receive deployment.kubernetes.io/revision updates that must not cause an
// endless update/conflict loop.
func mergeMetadataMap(existing, desired map[string]string) map[string]string {
	if len(existing) == 0 && len(desired) == 0 {
		return nil
	}
	merged := make(map[string]string, len(existing)+len(desired))
	for key, value := range existing {
		merged[key] = value
	}
	for key, value := range desired {
		merged[key] = value
	}
	return merged
}

func (r *ModelDeploymentReconciler) reconcileService(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
) (*corev1.Service, error) {
	references, err := ownerReference(deployment, r.Scheme)
	if err != nil {
		return nil, err
	}
	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: servingObjectName(deployment), Namespace: deployment.Namespace,
			Labels: servingLabels(deployment), OwnerReferences: references,
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeClusterIP,
			Selector:              map[string]string{modelDeploymentUIDLabel: string(deployment.UID)},
			SessionAffinity:       corev1.ServiceAffinityNone,
			InternalTrafficPolicy: &internalTrafficPolicy,
			Ports: []corev1.ServicePort{{
				Name: "http", Protocol: corev1.ProtocolTCP, Port: runtimeHTTPPort,
				TargetPort: intstr.FromString("http"),
			}},
		},
	}
	var existing corev1.Service
	key := client.ObjectKeyFromObject(desired)
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	} else if err != nil {
		return nil, err
	}
	if !hasControllerOwner(existing.OwnerReferences, deployment.UID, "ModelDeployment") ||
		existing.Labels[managedByLabel] != kamaName || existing.Labels[componentLabel] != modelDeploymentComponent {
		return nil, resourceCollisionf(
			"refusing Service %s/%s with mismatched owner identity", existing.Namespace, existing.Name,
		)
	}
	updated := existing.DeepCopy()
	updated.Labels = mergeMetadataMap(existing.Labels, desired.Labels)
	updated.Annotations = mergeMetadataMap(existing.Annotations, desired.Annotations)
	updated.OwnerReferences = desired.OwnerReferences
	updated.Spec = desired.Spec
	updated.Spec.ClusterIP = existing.Spec.ClusterIP
	updated.Spec.ClusterIPs = append([]string(nil), existing.Spec.ClusterIPs...)
	updated.Spec.IPFamilies = append([]corev1.IPFamily(nil), existing.Spec.IPFamilies...)
	updated.Spec.IPFamilyPolicy = existing.Spec.IPFamilyPolicy
	if !equality.Semantic.DeepEqual(existing.Labels, updated.Labels) ||
		!equality.Semantic.DeepEqual(existing.OwnerReferences, updated.OwnerReferences) ||
		!equality.Semantic.DeepEqual(existing.Spec, updated.Spec) {
		if err := r.Update(ctx, updated); err != nil {
			return nil, err
		}
		return updated, nil
	}
	return &existing, nil
}

func (r *ModelDeploymentReconciler) reconcileRuntimeConfig(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	fingerprint string,
	config kamaruntime.Config,
) (*corev1.ConfigMap, error) {
	payload, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode runtime config: %w", err)
	}
	references, err := ownerReference(deployment, r.Scheme)
	if err != nil {
		return nil, err
	}
	immutable := true
	labels := servingLabels(deployment)
	labels[runtimeFingerprintLabel] = boundedLabelValue(fingerprint)
	labels[artifactUIDLabel] = boundedLabelValue(config.Artifact.UID)
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: runtimeConfigName(deployment, fingerprint), Namespace: deployment.Namespace,
			Labels: labels, OwnerReferences: references,
		},
		Immutable: &immutable,
		Data:      map[string]string{runtimeConfigKey: string(payload)},
	}
	if err := r.Create(ctx, desired); err == nil {
		return desired, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	var existing corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		return nil, err
	}
	if !hasControllerOwner(existing.OwnerReferences, deployment.UID, "ModelDeployment") ||
		!requiredLabelsMatch(desired.Labels, existing.Labels) || existing.Immutable == nil || !*existing.Immutable ||
		!equality.Semantic.DeepEqual(desired.Data, existing.Data) || len(existing.BinaryData) != 0 {
		return nil, resourceCollisionf(
			"refusing ConfigMap %s/%s with mismatched immutable runtime input", existing.Namespace, existing.Name,
		)
	}
	updated := existing.DeepCopy()
	updated.Labels = mergeMetadataMap(existing.Labels, desired.Labels)
	updated.Annotations = mergeMetadataMap(existing.Annotations, desired.Annotations)
	updated.OwnerReferences = desired.OwnerReferences
	if !equality.Semantic.DeepEqual(existing.Labels, updated.Labels) ||
		!equality.Semantic.DeepEqual(existing.Annotations, updated.Annotations) ||
		!equality.Semantic.DeepEqual(existing.OwnerReferences, updated.OwnerReferences) {
		if err := r.Update(ctx, updated); err != nil {
			return nil, err
		}
		return updated, nil
	}
	return &existing, nil
}

func (r *ModelDeploymentReconciler) reconcileWorkload(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
	configMap *corev1.ConfigMap,
	fingerprint, locationHash, image string,
) (*appsv1.Deployment, error) {
	desired, err := r.newServingDeployment(deployment, artifact, configMap, fingerprint, locationHash, image)
	if err != nil {
		return nil, err
	}
	var existing appsv1.Deployment
	key := client.ObjectKeyFromObject(desired)
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	} else if err != nil {
		return nil, err
	}
	if !hasControllerOwner(existing.OwnerReferences, deployment.UID, "ModelDeployment") ||
		existing.Labels[managedByLabel] != kamaName || existing.Labels[componentLabel] != modelDeploymentComponent {
		return nil, resourceCollisionf(
			"refusing Deployment %s/%s with mismatched owner identity", existing.Namespace, existing.Name,
		)
	}
	updated := existing.DeepCopy()
	updated.Labels = mergeMetadataMap(existing.Labels, desired.Labels)
	updated.Annotations = mergeMetadataMap(existing.Annotations, desired.Annotations)
	updated.OwnerReferences = desired.OwnerReferences
	updated.Spec = desired.Spec
	if !equality.Semantic.DeepEqual(existing.Labels, updated.Labels) ||
		!equality.Semantic.DeepEqual(existing.Annotations, updated.Annotations) ||
		!equality.Semantic.DeepEqual(existing.OwnerReferences, updated.OwnerReferences) ||
		!equality.Semantic.DeepEqual(existing.Spec, updated.Spec) {
		if err := r.Update(ctx, updated); err != nil {
			return nil, err
		}
		return updated, nil
	}
	return &existing, nil
}

func (r *ModelDeploymentReconciler) newServingDeployment(
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
	configMap *corev1.ConfigMap,
	fingerprint, locationHash, image string,
) (*appsv1.Deployment, error) {
	references, err := ownerReference(deployment, r.Scheme)
	if err != nil {
		return nil, err
	}
	labels := servingLabels(deployment)
	labels[runtimeFingerprintLabel] = boundedLabelValue(fingerprint)
	labels[artifactUIDLabel] = boundedLabelValue(string(artifact.UID))
	selector := map[string]string{modelDeploymentUIDLabel: string(deployment.UID)}
	replicas := int32(1)
	revisionHistory := int32(1)
	nonRoot := true
	readOnlyRoot := true
	allowPrivilegeEscalation := false
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	fsGroup := int64(65532)
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch
	configMode := int32(0o444)
	automount := false
	enableServiceLinks := false
	terminationGrace := int64(math.Ceil(runtimeDrainDuration(deployment).Seconds())) + 20
	progressDeadline := int32(3600)
	resources := corev1.ResourceRequirements{
		Requests: copyResourceList(deployment.Spec.Resources.Requests),
		Limits:   copyResourceList(deployment.Spec.Resources.Limits),
	}
	if deployment.Spec.Placement.Mode == kamav1alpha1.ModelDeploymentPlacementAccelerator {
		resources.Requests[kamav1alpha1.DefaultAcceleratorResource] = resourceOne()
		resources.Limits[kamav1alpha1.DefaultAcceleratorResource] = resourceOne()
	}
	modelSubPath := artifact.Status.Location.SubPath
	if modelSubPath == "." {
		modelSubPath = ""
	}
	podSpec := corev1.PodSpec{
		AutomountServiceAccountToken: &automount,
		EnableServiceLinks:           &enableServiceLinks,
		SchedulingGates: []corev1.PodSchedulingGate{{
			Name: artifactUnavailableSchedulingGate,
		}},
		RestartPolicy:                 corev1.RestartPolicyAlways,
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 corev1.DefaultSchedulerName,
		ServiceAccountName:            "default",
		ImagePullSecrets:              append([]corev1.LocalObjectReference(nil), r.Runtime.ImagePullSecrets...),
		TerminationGracePeriodSeconds: &terminationGrace,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &nonRoot, RunAsUser: &runAsUser, RunAsGroup: &runAsGroup,
			FSGroup: &fsGroup, FSGroupChangePolicy: &fsGroupChangePolicy,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Volumes: []corev1.Volume{
			{
				Name: runtimeModelVolumeName,
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: artifact.Status.Location.ClaimName, ReadOnly: true,
				}},
			},
			{
				Name: "runtime-config",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name}, DefaultMode: &configMode,
				}},
			},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		Containers: []corev1.Container{{
			Name: runtimeContainerName, Image: image, ImagePullPolicy: r.Runtime.PullPolicy,
			Command: []string{"/kama-runtime-supervisor"}, Args: []string{"--config=" + runtimeConfigPath},
			Ports: []corev1.ContainerPort{
				{Name: "http", ContainerPort: runtimeHTTPPort, Protocol: corev1.ProtocolTCP},
				{Name: "supervisor", ContainerPort: supervisorHTTPPort, Protocol: corev1.ProtocolTCP},
			},
			Resources: resources,
			VolumeMounts: []corev1.VolumeMount{
				{Name: runtimeModelVolumeName, MountPath: runtimeModelMount, SubPath: modelSubPath, ReadOnly: true},
				{Name: "runtime-config", MountPath: runtimeConfigMount, ReadOnly: true},
				{Name: "tmp", MountPath: runtimeTemporaryMount},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &allowPrivilegeEscalation, ReadOnlyRootFilesystem: &readOnlyRoot,
				RunAsNonRoot: &nonRoot, RunAsUser: &runAsUser, RunAsGroup: &runAsGroup,
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			StartupProbe:   supervisorProbe("/startupz", 2, 30),
			ReadinessProbe: supervisorProbe("/readyz", 2, 1),
			LivenessProbe:  supervisorProbe("/livez", 10, 3),
			Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{
				"/kama-runtime-supervisor", "drain", "--address=127.0.0.1:8081",
			}}}},
			TerminationMessagePath:   "/dev/termination-log",
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}},
	}
	if locationAffinity := artifact.Status.Location.NodeAffinity; locationAffinity != nil && locationAffinity.Required != nil {
		podSpec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: locationAffinity.Required.DeepCopy(),
		}}
	}
	if deployment.Spec.Placement.Mode == kamav1alpha1.ModelDeploymentPlacementAccelerator {
		podSpec.NodeSelector = map[string]string{corev1.LabelArchStable: "amd64"}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: servingObjectName(deployment), Namespace: deployment.Namespace,
			Labels: servingLabels(deployment), OwnerReferences: references,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:                &replicas,
			Selector:                &metav1.LabelSelector{MatchLabels: selector},
			Strategy:                appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			RevisionHistoryLimit:    &revisionHistory,
			ProgressDeadlineSeconds: &progressDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						runtimeFingerprintAnnotation:   fingerprint,
						artifactLocationHashAnnotation: locationHash,
					},
				},
				Spec: podSpec,
			},
		},
	}, nil
}

func supervisorProbe(probePath string, period, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: probePath, Port: intstr.FromInt32(supervisorHTTPPort), Scheme: corev1.URISchemeHTTP,
		}},
		TimeoutSeconds: 1, PeriodSeconds: period, FailureThreshold: failureThreshold, SuccessThreshold: 1,
	}
}

func copyResourceList(source corev1.ResourceList) corev1.ResourceList {
	result := make(corev1.ResourceList, len(source)+1)
	for name, quantity := range source {
		result[name] = quantity.DeepCopy()
	}
	return result
}

func resourceOne() resource.Quantity {
	return *resource.NewQuantity(1, resource.DecimalSI)
}

type supervisorState struct {
	Phase      string                 `json:"phase"`
	Reason     string                 `json:"reason,omitempty"`
	Message    string                 `json:"message,omitempty"`
	Ready      bool                   `json:"ready"`
	Deployment supervisorStateOwner   `json:"deployment"`
	Runtime    supervisorStateRuntime `json:"runtime"`
}

type supervisorStateOwner struct {
	UID         types.UID `json:"uid"`
	Fingerprint string    `json:"fingerprint"`
}

type supervisorStateRuntime struct {
	Mode                   string `json:"mode"`
	EffectiveContextTokens *int64 `json:"effectiveContextTokens,omitempty"`
	DesiredConcurrency     int32  `json:"desiredConcurrency"`
	LlamaCommit            string `json:"llamaCPPCommit"`
	AcceleratorDetected    bool   `json:"acceleratorDetected,omitempty"`
	VisibleAccelerators    *int32 `json:"visibleAccelerators,omitempty"`
	OffloadedLayers        *int32 `json:"offloadedLayers,omitempty"`
	TotalLayers            *int32 `json:"totalLayers,omitempty"`
}

//nolint:gocyclo // Status convergence intentionally evaluates all five lifecycle conditions together.
func (r *ModelDeploymentReconciler) observeRuntime(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
	service *corev1.Service,
	workload *appsv1.Deployment,
	fingerprint, image string,
	artifactStatus metav1.ConditionStatus,
	artifactReason, artifactMessage string,
) (ctrl.Result, error) {
	ownedReplicaSets, err := r.ownedServingReplicaSetUIDs(ctx, deployment, workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(deployment.Namespace),
		client.MatchingLabels{modelDeploymentUIDLabel: string(deployment.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	var currentPod *corev1.Pod
	readyReplicas := int32(0)
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Labels[managedByLabel] != kamaName || pod.Labels[componentLabel] != modelDeploymentComponent ||
			pod.Annotations[runtimeFingerprintAnnotation] != fingerprint || !pod.DeletionTimestamp.IsZero() ||
			!ownedByAnyReplicaSet(pod.OwnerReferences, ownedReplicaSets) {
			continue
		}
		if currentPod == nil || pod.CreationTimestamp.Before(&currentPod.CreationTimestamp) {
			currentPod = pod
		}
		if podConditionTrue(pod.Status.Conditions, corev1.PodReady) {
			if readyReplicas == 0 {
				readyReplicas = 1
			}
		}
	}

	state := supervisorState{}
	stateAvailable := false
	if currentPod != nil && currentPod.Status.PodIP != "" {
		observed, stateErr := r.readSupervisorState(ctx, currentPod.Status.PodIP)
		if stateErr == nil && observed.Deployment.UID == deployment.UID &&
			observed.Deployment.Fingerprint == fingerprint {
			state = observed
			stateAvailable = true
		}
	}

	before := deployment.DeepCopy().Status
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Artifact = &kamav1alpha1.ModelDeploymentArtifactStatus{
		Name: artifact.Name, UID: artifact.UID, Digest: artifact.Status.ArtifactDigest,
	}
	deployment.Status.DeploymentRef = &kamav1alpha1.ModelDeploymentObjectReference{Name: workload.Name, UID: workload.UID}
	deployment.Status.ServiceRef = &kamav1alpha1.ModelDeploymentObjectReference{Name: service.Name, UID: service.UID}
	deployment.Status.DesiredReplicas = 1
	deployment.Status.ReadyReplicas = readyReplicas
	runtimeStatus := &kamav1alpha1.ModelDeploymentRuntimeStatus{
		State:              kamav1alpha1.ModelDeploymentRuntimeInitializing,
		DesiredImage:       image,
		DesiredFingerprint: fingerprint,
	}
	if before.Runtime != nil && before.Runtime.LoadedFingerprint == fingerprint {
		runtimeStatus.LoadedFingerprint = fingerprint
	}
	if currentPod != nil {
		for _, container := range currentPod.Status.ContainerStatuses {
			if container.Name == runtimeContainerName {
				runtimeStatus.ObservedImage = container.ImageID
				break
			}
		}
	}
	if stateAvailable {
		runtimeStatus.State = boundedRuntimeState(state.Phase)
		if validRuntimeFingerprint(state.Deployment.Fingerprint) {
			runtimeStatus.ObservedFingerprint = state.Deployment.Fingerprint
		}
		if state.Runtime.EffectiveContextTokens != nil && *state.Runtime.EffectiveContextTokens > 0 {
			runtimeStatus.EffectiveContextTokens = state.Runtime.EffectiveContextTokens
		}
		if state.Runtime.DesiredConcurrency >= 1 &&
			state.Runtime.DesiredConcurrency <= kamav1alpha1.MaximumModelDeploymentConcurrency {
			runtimeStatus.EffectiveConcurrency = state.Runtime.DesiredConcurrency
		}
		acceleratorDetected := state.Runtime.AcceleratorDetected
		runtimeStatus.AcceleratorDetected = &acceleratorDetected
		if state.Runtime.OffloadedLayers != nil && *state.Runtime.OffloadedLayers >= 0 {
			runtimeStatus.OffloadedLayers = state.Runtime.OffloadedLayers
		}
		if validRuntimeCommit(state.Runtime.LlamaCommit) {
			runtimeStatus.LlamaCommit = state.Runtime.LlamaCommit
		}
	}
	deployment.Status.Runtime = runtimeStatus

	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionArtifactReady,
		artifactStatus, artifactReason, artifactMessage)
	resourceStatus, resourceReason, resourceMessage := resourceCondition(currentPod)
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionResourcesAvailable,
		resourceStatus, resourceReason, resourceMessage)
	runtimeContractReady := supervisorStateMatches(deployment, state, stateAvailable, fingerprint, r.Runtime.LlamaCommit)
	if runtimeContractReady {
		runtimeStatus.LoadedFingerprint = fingerprint
	}
	runtimeReady := readyReplicas > 0 && runtimeContractReady
	endpointReady := false
	if runtimeReady {
		var endpointErr error
		endpointReady, endpointErr = r.servingEndpointReady(ctx, service, currentPod)
		if endpointErr != nil {
			return ctrl.Result{}, endpointErr
		}
	}
	runtimeReason := runtimePendingReason
	runtimeMessage := "serving Pod has not completed model loading"
	if stateAvailable && state.Reason != "" {
		runtimeReason = boundedConditionReason(state.Reason, "RuntimeNotReady")
		runtimeMessage = state.Message
	}
	if readyReplicas > 0 && stateAvailable && state.Ready && state.Phase == string(kamaruntime.PhaseReady) &&
		!runtimeContractReady {
		runtimeReason = "RuntimeContractMismatch"
		runtimeMessage = "supervisor state does not match the expected runtime configuration and build identity"
	}
	if runtimeStatus.State == kamav1alpha1.ModelDeploymentRuntimeLoadFailed ||
		runtimeStatus.State == kamav1alpha1.ModelDeploymentRuntimeExited {
		if runtimeReason == runtimePendingReason {
			runtimeReason = string(runtimeStatus.State)
		}
		if runtimeMessage == "" {
			runtimeMessage = "runtime child exited and will not be restarted inside this Pod"
		}
	}
	if runtimeReady {
		runtimeReason = "Loaded"
		runtimeMessage = "runtime loaded the current artifact and configuration"
		setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionRuntimeReady,
			metav1.ConditionTrue, runtimeReason, runtimeMessage)
		if endpointReady {
			setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionServing,
				metav1.ConditionTrue, "Available", "current runtime fingerprint has a ready Service endpoint")
		} else {
			setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionServing,
				metav1.ConditionFalse, "EndpointPending", "current runtime is ready but is not yet published in a ready EndpointSlice")
		}
	} else {
		setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionRuntimeReady,
			metav1.ConditionFalse, runtimeReason, runtimeMessage)
		setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionServing,
			metav1.ConditionFalse, "RuntimeNotReady", "Service has no ready endpoint for the current runtime fingerprint")
	}
	setDeploymentDegradedCondition(deployment)
	if err := r.persistDeploymentStatus(ctx, deployment, before); err != nil {
		return ctrl.Result{}, err
	}
	publishModelDeploymentMetrics(deployment)
	return ctrl.Result{RequeueAfter: defaultRuntimePoll}, nil
}

func supervisorStateMatches(
	deployment *kamav1alpha1.ModelDeployment,
	state supervisorState,
	available bool,
	fingerprint, llamaCommit string,
) bool {
	if !available || !state.Ready || state.Phase != string(kamaruntime.PhaseReady) ||
		state.Deployment.UID != deployment.UID || state.Deployment.Fingerprint != fingerprint ||
		state.Runtime.Mode != string(deployment.Spec.Placement.Mode) ||
		state.Runtime.LlamaCommit != llamaCommit || !validRuntimeCommit(state.Runtime.LlamaCommit) {
		return false
	}
	expectedConcurrency := int32PointerValue(deployment.Spec.Runtime.DesiredConcurrency)
	if expectedConcurrency == 0 {
		expectedConcurrency = kamav1alpha1.DefaultModelDeploymentConcurrency
	}
	if state.Runtime.DesiredConcurrency != expectedConcurrency ||
		state.Runtime.EffectiveContextTokens == nil || *state.Runtime.EffectiveContextTokens < 1 {
		return false
	}
	if deployment.Spec.Runtime.MaxContextTokens != nil &&
		*state.Runtime.EffectiveContextTokens != *deployment.Spec.Runtime.MaxContextTokens {
		return false
	}
	if deployment.Spec.Placement.Mode == kamav1alpha1.ModelDeploymentPlacementAccelerator &&
		(!state.Runtime.AcceleratorDetected || state.Runtime.VisibleAccelerators == nil ||
			*state.Runtime.VisibleAccelerators != 1 || state.Runtime.OffloadedLayers == nil ||
			state.Runtime.TotalLayers == nil || *state.Runtime.TotalLayers < 1 ||
			*state.Runtime.OffloadedLayers != *state.Runtime.TotalLayers) {
		return false
	}
	return true
}

func validRuntimeCommit(value string) bool {
	return validLowerHex(value, 40)
}

func validRuntimeFingerprint(value string) bool {
	return validLowerHex(value, 20)
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func (r *ModelDeploymentReconciler) servingEndpointReady(
	ctx context.Context,
	service *corev1.Service,
	pod *corev1.Pod,
) (bool, error) {
	if service == nil || pod == nil {
		return false, nil
	}
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace(service.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service.Name},
	); err != nil {
		return false, err
	}
	for sliceIndex := range slices.Items {
		for endpointIndex := range slices.Items[sliceIndex].Endpoints {
			endpoint := &slices.Items[sliceIndex].Endpoints[endpointIndex]
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready ||
				(endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating) ||
				endpoint.TargetRef == nil || endpoint.TargetRef.Name != pod.Name {
				continue
			}
			if endpoint.TargetRef.Kind != "" && endpoint.TargetRef.Kind != "Pod" {
				continue
			}
			if endpoint.TargetRef.Namespace != "" && endpoint.TargetRef.Namespace != pod.Namespace {
				continue
			}
			if endpoint.TargetRef.UID != "" && pod.UID != "" && endpoint.TargetRef.UID != pod.UID {
				continue
			}
			return true, nil
		}
	}
	return false, nil
}

func podConditionTrue(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func resourceCondition(pod *corev1.Pod) (metav1.ConditionStatus, string, string) {
	if pod == nil {
		return metav1.ConditionFalse, runtimePendingReason, "serving Pod has not been created by the Deployment controller"
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type != corev1.PodScheduled {
			continue
		}
		if condition.Status == corev1.ConditionTrue {
			return metav1.ConditionTrue, "Scheduled", "serving Pod has been assigned to a compatible node"
		}
		reason := boundedConditionReason(condition.Reason, "Unschedulable")
		return metav1.ConditionFalse, reason, condition.Message
	}
	return metav1.ConditionFalse, "SchedulingPending", "serving Pod has no scheduling decision"
}

func boundedRuntimeState(value string) kamav1alpha1.ModelDeploymentRuntimeState {
	switch kamav1alpha1.ModelDeploymentRuntimeState(value) {
	case kamav1alpha1.ModelDeploymentRuntimeInitializing,
		kamav1alpha1.ModelDeploymentRuntimeLoading,
		kamav1alpha1.ModelDeploymentRuntimeReady,
		kamav1alpha1.ModelDeploymentRuntimeDraining,
		kamav1alpha1.ModelDeploymentRuntimeLoadFailed,
		kamav1alpha1.ModelDeploymentRuntimeExited:
		return kamav1alpha1.ModelDeploymentRuntimeState(value)
	default:
		return kamav1alpha1.ModelDeploymentRuntimeInitializing
	}
}

func boundedConditionReason(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return fallback
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return fallback
	}
	return value
}

func (r *ModelDeploymentReconciler) readSupervisorState(ctx context.Context, podIP string) (supervisorState, error) {
	if net.ParseIP(podIP) == nil {
		return supervisorState{}, errors.New("pod IP is invalid")
	}
	address := net.JoinHostPort(podIP, fmt.Sprintf("%d", supervisorHTTPPort))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/state", nil)
	if err != nil {
		return supervisorState{}, err
	}
	response, err := r.HTTPClient.Do(request)
	if err != nil {
		return supervisorState{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return supervisorState{}, fmt.Errorf("supervisor state returned HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, 64*1024)
	var state supervisorState
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&state); err != nil {
		return supervisorState{}, fmt.Errorf("decode supervisor state: %w", err)
	}
	state.Message = sanitizeConditionMessage(state.Message)
	return state, nil
}

func (r *ModelDeploymentReconciler) reconcileUnavailableArtifact(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	service *corev1.Service,
	artifact *kamav1alpha1.ModelArtifact,
	reason, message string,
) (ctrl.Result, error) {
	var workload appsv1.Deployment
	key := types.NamespacedName{Namespace: deployment.Namespace, Name: servingObjectName(deployment)}
	err := r.Get(ctx, key, &workload)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil && (!hasControllerOwner(workload.OwnerReferences, deployment.UID, "ModelDeployment") ||
		workload.Labels[managedByLabel] != kamaName || workload.Labels[componentLabel] != modelDeploymentComponent) {
		return r.failStatus(ctx, deployment, artifact, "GeneratedResourceCollision",
			fmt.Sprintf("refusing Deployment %s/%s with mismatched owner identity", workload.Namespace, workload.Name))
	}
	if err == nil && artifact != nil && r.canPreserveLoadedWorkload(deployment, artifact, &workload) {
		if err := r.suspendWorkloadReplacement(ctx, &workload); err != nil {
			return ctrl.Result{}, err
		}
		fingerprint := workload.Spec.Template.Annotations[runtimeFingerprintAnnotation]
		image := ""
		if len(workload.Spec.Template.Spec.Containers) == 1 {
			image = workload.Spec.Template.Spec.Containers[0].Image
		}
		return r.observeRuntime(ctx, deployment, artifact, service, &workload, fingerprint, image,
			metav1.ConditionFalse, boundedConditionReason(reason, "ArtifactNotReady"), message)
	}
	if err == nil && workload.DeletionTimestamp.IsZero() {
		if err := deleteForeground(ctx, r.Client, &workload); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := deployment.DeepCopy().Status
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.DesiredReplicas = 0
	deployment.Status.ReadyReplicas = 0
	deployment.Status.DeploymentRef = nil
	deployment.Status.ServiceRef = &kamav1alpha1.ModelDeploymentObjectReference{Name: service.Name, UID: service.UID}
	deployment.Status.Runtime = nil
	if artifact == nil {
		deployment.Status.Artifact = &kamav1alpha1.ModelDeploymentArtifactStatus{Name: deployment.Spec.ModelRef.Name}
	} else {
		deployment.Status.Artifact = &kamav1alpha1.ModelDeploymentArtifactStatus{
			Name: artifact.Name, UID: artifact.UID, Digest: artifact.Status.ArtifactDigest,
		}
	}
	reason = boundedConditionReason(reason, "ArtifactNotReady")
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionArtifactReady,
		metav1.ConditionFalse, reason, message)
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionResourcesAvailable,
		metav1.ConditionFalse, "WaitingForArtifact", "serving resources are not created until the artifact is ready")
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionRuntimeReady,
		metav1.ConditionFalse, "WaitingForArtifact", "runtime is blocked on its artifact dependency")
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionServing,
		metav1.ConditionFalse, "ServiceUnavailable", "Service has no ready serving endpoint")
	setDeploymentDegradedCondition(deployment)
	if err := r.persistDeploymentStatus(ctx, deployment, before); err != nil {
		return ctrl.Result{}, err
	}
	publishModelDeploymentMetrics(deployment)
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *ModelDeploymentReconciler) canPreserveLoadedWorkload(
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
	workload *appsv1.Deployment,
) bool {
	if deployment.Status.Artifact == nil || deployment.Status.Runtime == nil || artifact.Status.Location == nil ||
		deployment.Status.ObservedGeneration != deployment.Generation ||
		deployment.Status.Artifact.Name != artifact.Name || deployment.Status.Artifact.UID != artifact.UID ||
		deployment.Status.Artifact.Digest == "" || deployment.Status.Artifact.Digest != artifact.Status.ArtifactDigest ||
		workload.Spec.Template.Annotations[runtimeFingerprintAnnotation] == "" ||
		workload.Spec.Template.Annotations[runtimeFingerprintAnnotation] != deployment.Status.Runtime.DesiredFingerprint ||
		workload.Spec.Template.Annotations[runtimeFingerprintAnnotation] != deployment.Status.Runtime.LoadedFingerprint {
		return false
	}
	expectedFingerprint, locationHash, _, expectedImage, err := r.desiredRuntime(deployment, artifact)
	return err == nil && locationHash == workload.Spec.Template.Annotations[artifactLocationHashAnnotation] &&
		expectedFingerprint == workload.Spec.Template.Annotations[runtimeFingerprintAnnotation] &&
		expectedFingerprint == deployment.Status.Runtime.DesiredFingerprint &&
		expectedImage == deployment.Status.Runtime.DesiredImage
}

func (r *ModelDeploymentReconciler) failStatus(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	artifact *kamav1alpha1.ModelArtifact,
	reason, message string,
) (ctrl.Result, error) {
	before := deployment.DeepCopy().Status
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.ReadyReplicas = 0
	if artifact != nil {
		deployment.Status.Artifact = &kamav1alpha1.ModelDeploymentArtifactStatus{
			Name: artifact.Name, UID: artifact.UID, Digest: artifact.Status.ArtifactDigest,
		}
	}
	reason = boundedConditionReason(reason, "ReconcileFailed")
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionArtifactReady,
		deploymentConditionStatus(artifact != nil), deploymentConditionReason(artifact != nil, "ArtifactReady", "ArtifactUnavailable"),
		deploymentConditionMessage(artifact != nil, "referenced artifact is ready", "referenced artifact is unavailable"))
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionResourcesAvailable,
		metav1.ConditionFalse, reason, message)
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionRuntimeReady,
		metav1.ConditionFalse, reason, message)
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionServing,
		metav1.ConditionFalse, "ServiceUnavailable", "Service has no ready endpoint for the current runtime fingerprint")
	setDeploymentDegradedCondition(deployment)
	if err := r.persistDeploymentStatus(ctx, deployment, before); err != nil {
		return ctrl.Result{}, err
	}
	publishModelDeploymentMetrics(deployment)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func deploymentConditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func deploymentConditionReason(value bool, whenTrue, whenFalse string) string {
	if value {
		return whenTrue
	}
	return whenFalse
}

func deploymentConditionMessage(value bool, whenTrue, whenFalse string) string {
	if value {
		return whenTrue
	}
	return whenFalse
}

func setDeploymentCondition(
	deployment *kamav1alpha1.ModelDeployment,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, ObservedGeneration: deployment.Generation,
		Reason: boundedConditionReason(reason, "Unknown"), Message: sanitizeConditionMessage(message),
	})
}

func setDeploymentDegradedCondition(deployment *kamav1alpha1.ModelDeployment) {
	if deployment.Spec.Placement.Mode == kamav1alpha1.ModelDeploymentPlacementCPU {
		setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionDegraded,
			metav1.ConditionTrue, "CPUOnlyRequested", "CPU mode is supported with reduced performance guarantees")
		return
	}
	setDeploymentCondition(deployment, kamav1alpha1.ModelDeploymentConditionDegraded,
		metav1.ConditionFalse, "AcceleratorRequested", "no intentional runtime degradation is configured")
}

func (r *ModelDeploymentReconciler) persistDeploymentStatus(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	before kamav1alpha1.ModelDeploymentStatus,
) error {
	loadFailed := deployment.Status.Runtime != nil &&
		deployment.Status.Runtime.State == kamav1alpha1.ModelDeploymentRuntimeLoadFailed
	loadFailureTransition := loadFailed && (before.Runtime == nil ||
		before.Runtime.State != kamav1alpha1.ModelDeploymentRuntimeLoadFailed)
	if reflect.DeepEqual(before, deployment.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, deployment); err != nil {
		return err
	}
	if loadFailureTransition {
		modelDeploymentLoadFailures.WithLabelValues(runtimeMetricStateLoadFailed).Inc()
	}
	if r.Recorder != nil {
		for _, conditionType := range []string{
			kamav1alpha1.ModelDeploymentConditionArtifactReady,
			kamav1alpha1.ModelDeploymentConditionRuntimeReady,
			kamav1alpha1.ModelDeploymentConditionServing,
		} {
			condition := meta.FindStatusCondition(deployment.Status.Conditions, conditionType)
			if condition == nil || !statusConditionTransitioned(
				before.Conditions, condition.Type, condition.Status, condition.ObservedGeneration, condition.Reason,
			) {
				continue
			}
			eventType := corev1.EventTypeNormal
			if condition.Status == metav1.ConditionFalse {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Eventf(deployment, nil, eventType, condition.Reason, "ReconcileServing", "%s", condition.Message)
		}
	}
	return nil
}

func publishModelDeploymentMetrics(deployment *kamav1alpha1.ModelDeployment) {
	modelDeploymentReadyReplicas.WithLabelValues(deployment.Namespace, deployment.Name).
		Set(float64(deployment.Status.ReadyReplicas))
	state := runtimeMetricStateNone
	if deployment.Status.Runtime != nil && deployment.Status.Runtime.State != "" {
		state = string(deployment.Status.Runtime.State)
	}
	for _, candidate := range []string{
		runtimeMetricStateNone, "Initializing", "Loading", "Ready", "Draining", runtimeMetricStateLoadFailed, "Exited",
	} {
		value := 0.0
		if candidate == state {
			value = 1
		}
		modelDeploymentRuntimeState.WithLabelValues(deployment.Namespace, deployment.Name, candidate).Set(value)
	}
}

func deleteModelDeploymentMetrics(namespace, name string) {
	modelDeploymentReadyReplicas.DeleteLabelValues(namespace, name)
	for _, state := range []string{
		runtimeMetricStateNone, "Initializing", "Loading", "Ready", "Draining", runtimeMetricStateLoadFailed, "Exited",
	} {
		modelDeploymentRuntimeState.DeleteLabelValues(namespace, name, state)
	}
}

func (r *ModelDeploymentReconciler) cleanupObsoleteRuntimeConfigs(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
	desiredName string,
) error {
	labels := client.MatchingLabels{modelDeploymentUIDLabel: string(deployment.UID)}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(deployment.Namespace), labels); err != nil {
		return err
	}
	inUse := map[string]struct{}{desiredName: {}}
	for index := range pods.Items {
		for _, volume := range pods.Items[index].Spec.Volumes {
			if volume.ConfigMap != nil {
				inUse[volume.ConfigMap.Name] = struct{}{}
			}
		}
	}
	var configs corev1.ConfigMapList
	if err := r.List(ctx, &configs, client.InNamespace(deployment.Namespace), labels); err != nil {
		return err
	}
	for index := range configs.Items {
		config := &configs.Items[index]
		if config.Labels[componentLabel] != modelDeploymentComponent ||
			!hasControllerOwner(config.OwnerReferences, deployment.UID, "ModelDeployment") {
			continue
		}
		if _, found := inUse[config.Name]; found || !config.DeletionTimestamp.IsZero() {
			continue
		}
		if err := deleteIfPresent(ctx, r.Client, config); err != nil {
			return err
		}
	}
	return nil
}

func (r *ModelDeploymentReconciler) reconcileDelete(
	ctx context.Context,
	deployment *kamav1alpha1.ModelDeployment,
) (ctrl.Result, error) {
	deleteModelDeploymentMetrics(deployment.Namespace, deployment.Name)
	if !controllerutil.ContainsFinalizer(deployment, kamav1alpha1.ModelDeploymentFinalizer) {
		return ctrl.Result{}, nil
	}
	name := servingObjectName(deployment)
	var service corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: deployment.Namespace, Name: name}, &service); err == nil {
		if hasControllerOwner(service.OwnerReferences, deployment.UID, "ModelDeployment") && service.DeletionTimestamp.IsZero() {
			if err := deleteIfPresent(ctx, r.Client, &service); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	var workload appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: deployment.Namespace, Name: name}, &workload); err == nil {
		if hasControllerOwner(workload.OwnerReferences, deployment.UID, "ModelDeployment") {
			if workload.DeletionTimestamp.IsZero() {
				if err := deleteForeground(ctx, r.Client, &workload); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	labels := client.MatchingLabels{modelDeploymentUIDLabel: string(deployment.UID)}
	var configs corev1.ConfigMapList
	if err := r.List(ctx, &configs, client.InNamespace(deployment.Namespace), labels); err != nil {
		return ctrl.Result{}, err
	}
	configsRemain := false
	for index := range configs.Items {
		config := &configs.Items[index]
		if config.Labels[componentLabel] != modelDeploymentComponent ||
			!hasControllerOwner(config.OwnerReferences, deployment.UID, "ModelDeployment") {
			continue
		}
		configsRemain = true
		if config.DeletionTimestamp.IsZero() {
			if err := deleteIfPresent(ctx, r.Client, config); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if configsRemain {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	controllerutil.RemoveFinalizer(deployment, kamav1alpha1.ModelDeploymentFinalizer)
	if err := r.Update(ctx, deployment); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the ModelDeployment controller and its generated objects.
func (r *ModelDeploymentReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Client == nil || r.Scheme == nil || r.Recorder == nil {
		return errors.New("client, scheme, and recorder are required")
	}
	if err := r.Runtime.Validate(); err != nil {
		return err
	}
	if r.HTTPClient == nil {
		r.HTTPClient = &http.Client{Timeout: defaultRuntimeHTTPTimeout}
	}
	if r.APIReader == nil {
		r.APIReader = manager.GetAPIReader()
	}
	registerControllerMetrics()
	return ctrl.NewControllerManagedBy(manager).
		For(&kamav1alpha1.ModelDeployment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&kamav1alpha1.ModelArtifact{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, object client.Object) []reconcile.Request {
				var deployments kamav1alpha1.ModelDeploymentList
				if err := r.List(ctx, &deployments, client.InNamespace(object.GetNamespace())); err != nil {
					return nil
				}
				requests := make([]reconcile.Request, 0)
				for index := range deployments.Items {
					item := &deployments.Items[index]
					if item.Spec.ModelRef.Name == object.GetName() {
						requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(item)})
					}
				}
				return requests
			},
		)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, object client.Object) []reconcile.Request {
				if object.GetLabels()[managedByLabel] != kamaName ||
					object.GetLabels()[componentLabel] != modelDeploymentComponent {
					return nil
				}
				name := object.GetLabels()[modelDeploymentNameLabel]
				if name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: object.GetNamespace(), Name: name,
				}}}
			},
		)).
		Watches(&appsv1.ReplicaSet{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, object client.Object) []reconcile.Request {
				if object.GetLabels()[managedByLabel] != kamaName ||
					object.GetLabels()[componentLabel] != modelDeploymentComponent {
					return nil
				}
				name := object.GetLabels()[modelDeploymentNameLabel]
				if name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: object.GetNamespace(), Name: name,
				}}}
			},
		)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, object client.Object) []reconcile.Request {
				serviceName := object.GetLabels()[discoveryv1.LabelServiceName]
				if serviceName == "" {
					return nil
				}
				var deployments kamav1alpha1.ModelDeploymentList
				if err := r.List(ctx, &deployments, client.InNamespace(object.GetNamespace())); err != nil {
					return nil
				}
				for index := range deployments.Items {
					item := &deployments.Items[index]
					if servingObjectName(item) == serviceName {
						return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(item)}}
					}
				}
				return nil
			},
		)).
		Complete(r)
}
