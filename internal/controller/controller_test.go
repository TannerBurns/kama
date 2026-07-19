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
	"testing"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	"github.com/TannerBurns/kama/internal/artifact"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	controllerTestNamespace = "models"
	directArtifactName      = "direct"
	testModelEntrypoint     = "model.gguf"
	testImporterImage       = "example.invalid/kama-importer:test"
	testHubEndpoint         = "https://huggingface.co"
	testOperationID         = "operation"
	cachedArtifactName      = "cached"
	sharedCacheName         = "shared"
)

type guardedClaimKind int

const (
	guardedDirectSource guardedClaimKind = iota
	guardedCopySource
	guardedManagedCache
)

func TestImporterOptionsValidatePullPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		policy  corev1.PullPolicy
		wantErr bool
	}{
		{name: "Always", policy: corev1.PullAlways},
		{name: "IfNotPresent", policy: corev1.PullIfNotPresent},
		{name: "Never", policy: corev1.PullNever},
		{name: "empty", policy: "", wantErr: true},
		{name: "unknown", policy: corev1.PullPolicy("Sometimes"), wantErr: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := (ImporterOptions{
				Image: testImporterImage, PullPolicy: testCase.policy, HubEndpoint: testHubEndpoint,
			}).validate()
			if (err != nil) != testCase.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, testCase.wantErr)
			}
		})
	}
}

func TestNormalizeMountScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		modes []corev1.PersistentVolumeAccessMode
		want  kamav1alpha1.MountScope
	}{
		{name: "RWX", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, want: kamav1alpha1.MountScopeMultiNode},
		{name: "ROX", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}, want: kamav1alpha1.MountScopeMultiNode},
		{name: "RWO", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, want: kamav1alpha1.MountScopeSingleNode},
		{name: "RWOP", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, want: kamav1alpha1.MountScopeSinglePod},
		{name: "RWO remains strict with RWX", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteMany}, want: kamav1alpha1.MountScopeSingleNode},
		{name: "RWOP remains strict with RWO and RWX", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany, corev1.ReadWriteOnce, corev1.ReadWriteOncePod}, want: kamav1alpha1.MountScopeSinglePod},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeMountScope(testCase.modes)
			if err != nil {
				t.Fatalf("normalizeMountScope(): %v", err)
			}
			if got != testCase.want {
				t.Fatalf("scope = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestManagedCacheClaimIsIdentityBoundAndOwnerless(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	cache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: sharedCacheName, Namespace: controllerTestNamespace, UID: types.UID("cache-uid")},
		Spec: kamav1alpha1.ModelCacheSpec{
			Storage: kamav1alpha1.ModelCacheStorageSpec{ClaimTemplate: &kamav1alpha1.ModelCacheClaimTemplate{
				Spec: kamav1alpha1.ModelCacheClaimTemplateSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
					VolumeMode:  corev1.PersistentVolumeFilesystem,
					Resources: kamav1alpha1.ModelCacheResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("10Gi"),
					}},
				},
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{Client: kubeClient, Scheme: scheme}}
	name, managed, err := reconciler.ensureClaim(context.Background(), cache)
	if err != nil {
		t.Fatalf("ensureClaim(): %v", err)
	}
	if !managed {
		t.Fatal("managed = false, want true")
	}
	var claim corev1.PersistentVolumeClaim
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: cache.Namespace, Name: name}, &claim); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if len(claim.OwnerReferences) != 0 {
		t.Fatalf("managed Retain claim has owner references: %v", claim.OwnerReferences)
	}
	if claim.Labels[cacheUIDLabel] != string(cache.UID) {
		t.Fatalf("cache UID label = %q", claim.Labels[cacheUIDLabel])
	}
	if claim.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("storage request = %s", claim.Spec.Resources.Requests.Storage())
	}
}

func TestDirectArtifactLocationPreservesRWOConstraint(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	filesystem := corev1.PersistentVolumeFilesystem
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: controllerTestNamespace, UID: types.UID("claim-uid")},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &filesystem},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: directArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid")},
		Spec: kamav1alpha1.ModelArtifactSpec{
			Format: kamav1alpha1.ArtifactFormatGGUF, Entrypoint: testModelEntrypoint,
			Source: kamav1alpha1.ModelArtifactSource{PersistentVolumeClaim: &kamav1alpha1.PersistentVolumeClaimSource{
				ClaimName: claim.Name, RootPath: controllerTestNamespace, ImportPolicy: kamav1alpha1.PVCImportPolicyDirect,
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient, Scheme: scheme}}
	storage, err := reconciler.resolveArtifactStorage(context.Background(), modelArtifact)
	if err != nil {
		t.Fatalf("resolveArtifactStorage(): %v", err)
	}
	if storage.location == nil || storage.location.MountScope != kamav1alpha1.MountScopeSingleNode {
		t.Fatalf("location = %+v, want SingleNode", storage.location)
	}
	if storage.location.ClaimName != claim.Name || !storage.location.ReadOnly {
		t.Fatalf("location = %+v", storage.location)
	}
}

func TestArtifactStorageIdentityIsCheckpointedBeforeImporterCreation(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: sourceName, Namespace: controllerTestNamespace, UID: types.UID("source-claim-uid"),
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name: directArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
			Finalizers: []string{kamav1alpha1.ModelArtifactFinalizer},
		},
		Spec: kamav1alpha1.ModelArtifactSpec{
			Format: kamav1alpha1.ArtifactFormatGGUF, Entrypoint: testModelEntrypoint,
			Source: kamav1alpha1.ModelArtifactSource{PersistentVolumeClaim: &kamav1alpha1.PersistentVolumeClaimSource{
				ClaimName: claim.Name, ImportPolicy: kamav1alpha1.PVCImportPolicyDirect,
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelArtifact{}).
		WithObjects(claim, modelArtifact).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(4),
		Importer: ImporterOptions{
			Image: testImporterImage, PullPolicy: corev1.PullNever, HubEndpoint: testHubEndpoint,
		},
	}, APIReader: kubeClient}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(modelArtifact),
	})
	if err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("checkpoint result = %+v, want one-second requeue", result)
	}
	var current kamav1alpha1.ModelArtifact
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelArtifact), &current); err != nil {
		t.Fatalf("get checkpointed artifact: %v", err)
	}
	if current.Status.Location == nil || current.Status.Location.ClaimName != claim.Name ||
		current.Status.Location.ClaimUID != claim.UID {
		t.Fatalf("durable storage checkpoint = %+v", current.Status.Location)
	}
	var jobs batchv1.JobList
	if err := kubeClient.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	var configs corev1.ConfigMapList
	if err := kubeClient.List(context.Background(), &configs); err != nil {
		t.Fatalf("list ConfigMaps: %v", err)
	}
	var leases coordinationv1.LeaseList
	if err := kubeClient.List(context.Background(), &leases); err != nil {
		t.Fatalf("list Leases: %v", err)
	}
	if len(jobs.Items) != 0 || len(configs.Items) != 0 || len(leases.Items) != 0 {
		t.Fatalf("operation resources existed before storage checkpoint: jobs=%d configs=%d leases=%d",
			len(jobs.Items), len(configs.Items), len(leases.Items))
	}
	if err := kubeClient.Delete(context.Background(), claim); err != nil {
		t.Fatalf("delete original source claim: %v", err)
	}
	replacement := claim.DeepCopy()
	replacement.UID = types.UID("replacement-claim-uid")
	replacement.ResourceVersion = ""
	if err := kubeClient.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create replacement source claim: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(modelArtifact),
	}); err != nil {
		t.Fatalf("Reconcile() replacement identity: %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelArtifact), &current); err != nil {
		t.Fatalf("get artifact after replacement: %v", err)
	}
	identityRejected := false
	for _, condition := range current.Status.Conditions {
		if condition.Type == kamav1alpha1.ModelArtifactConditionStorageReady &&
			condition.Reason == "StorageIdentityChanged" && condition.Status == metav1.ConditionFalse {
			identityRejected = true
		}
	}
	if !identityRejected {
		t.Fatalf("replacement storage identity was not rejected: %+v", current.Status.Conditions)
	}
}

func TestResolveArtifactStorageRejectsCacheDeletionGuardedClaims(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		kind       guardedClaimKind
		guardOwner string
		wantError  string
	}{
		{
			name: "Direct source with empty guard value", kind: guardedDirectSource, guardOwner: "",
			wantError: "source claim is guarded for deletion",
		},
		{
			name: "Copy source", kind: guardedCopySource, guardOwner: "guarding-cache-uid",
			wantError: "source claim is guarded for deletion",
		},
		{
			name: "managed cache", kind: guardedManagedCache, guardOwner: "guarding-cache-uid",
			wantError: "cache claim is guarded for deletion",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			newClaim := func(name string) *corev1.PersistentVolumeClaim {
				return &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: name, Namespace: controllerTestNamespace, UID: types.UID(name + "-uid"),
					},
					Status: corev1.PersistentVolumeClaimStatus{
						Phase:       corev1.ClaimBound,
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
					},
				}
			}
			cacheClaim := newClaim(cacheName)
			sourceClaim := newClaim(sourceName)
			modelCache := &kamav1alpha1.ModelCache{
				ObjectMeta: metav1.ObjectMeta{
					Name: sharedCacheName, Namespace: controllerTestNamespace, UID: types.UID("cache-owner-uid"),
				},
				Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
					ExistingClaim: &corev1.LocalObjectReference{Name: cacheClaim.Name},
				}},
				Status: kamav1alpha1.ModelCacheStatus{
					ClaimName: cacheClaim.Name, ClaimUID: cacheClaim.UID,
					Conditions: []metav1.Condition{{
						Type: kamav1alpha1.ModelCacheConditionReady, Status: metav1.ConditionTrue,
					}},
				},
			}
			modelArtifact := &kamav1alpha1.ModelArtifact{
				ObjectMeta: metav1.ObjectMeta{
					Name: artifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
				},
				Spec: kamav1alpha1.ModelArtifactSpec{
					Format: kamav1alpha1.ArtifactFormatGGUF, Entrypoint: testModelEntrypoint,
				},
			}
			objects := make([]client.Object, 0, 3)
			switch testCase.kind {
			case guardedDirectSource:
				sourceClaim.Annotations = map[string]string{cacheDeletionGuardAnnotation: testCase.guardOwner}
				modelArtifact.Spec.Source.PersistentVolumeClaim = &kamav1alpha1.PersistentVolumeClaimSource{
					ClaimName: sourceClaim.Name, ImportPolicy: kamav1alpha1.PVCImportPolicyDirect,
				}
				objects = append(objects, sourceClaim)
			case guardedCopySource:
				sourceClaim.Annotations = map[string]string{cacheDeletionGuardAnnotation: testCase.guardOwner}
				modelArtifact.Spec.Source.PersistentVolumeClaim = &kamav1alpha1.PersistentVolumeClaimSource{
					ClaimName: sourceClaim.Name, ImportPolicy: kamav1alpha1.PVCImportPolicyCopy,
				}
				modelArtifact.Spec.CacheRef = &corev1.LocalObjectReference{Name: modelCache.Name}
				objects = append(objects, sourceClaim, cacheClaim, modelCache)
			case guardedManagedCache:
				cacheClaim.Annotations = map[string]string{cacheDeletionGuardAnnotation: testCase.guardOwner}
				modelCache.Spec.Storage.ExistingClaim = nil
				modelCache.Spec.Storage.ClaimTemplate = &kamav1alpha1.ModelCacheClaimTemplate{}
				modelArtifact.Spec.Source.HuggingFace = &kamav1alpha1.HuggingFaceSource{
					Repository: "owner/model", Revision: "main", Files: []string{testModelEntrypoint},
				}
				modelArtifact.Spec.CacheRef = &corev1.LocalObjectReference{Name: modelCache.Name}
				objects = append(objects, cacheClaim, modelCache)
			default:
				t.Fatalf("unsupported test kind %d", testCase.kind)
			}

			kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient}}
			if _, err := reconciler.resolveArtifactStorage(context.Background(), modelArtifact); err == nil ||
				err.Error() != testCase.wantError {
				t.Fatalf("resolveArtifactStorage() error = %v, want %q", err, testCase.wantError)
			}
		})
	}
}

func TestResolveArtifactStorageRejectsTerminatingStorage(t *testing.T) {
	t.Parallel()
	newClaim := func(name string) *corev1.PersistentVolumeClaim {
		now := metav1.Now()
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: controllerTestNamespace, UID: types.UID(name + "-uid"),
				DeletionTimestamp: &now, Finalizers: []string{"test.kama/finalizer"},
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			},
		}
	}
	t.Run("Direct source PVC", func(t *testing.T) {
		t.Parallel()
		claim := newClaim(sourceName)
		modelArtifact := &kamav1alpha1.ModelArtifact{
			ObjectMeta: metav1.ObjectMeta{Name: artifactName, Namespace: controllerTestNamespace},
			Spec: kamav1alpha1.ModelArtifactSpec{
				Source: kamav1alpha1.ModelArtifactSource{PersistentVolumeClaim: &kamav1alpha1.PersistentVolumeClaimSource{
					ClaimName: claim.Name, ImportPolicy: kamav1alpha1.PVCImportPolicyDirect,
				}},
			},
		}
		kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(claim).Build()
		reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient}}
		if _, err := reconciler.resolveArtifactStorage(context.Background(), modelArtifact); err == nil ||
			err.Error() != "source claim is terminating" {
			t.Fatalf("resolveArtifactStorage() error = %v, want source termination", err)
		}
	})
	t.Run("cache PVC", func(t *testing.T) {
		t.Parallel()
		claim := newClaim(cacheName)
		modelCache := &kamav1alpha1.ModelCache{
			ObjectMeta: metav1.ObjectMeta{Name: sharedCacheName, Namespace: controllerTestNamespace},
			Status: kamav1alpha1.ModelCacheStatus{
				ClaimName: claim.Name, ClaimUID: claim.UID,
				Conditions: []metav1.Condition{{
					Type: kamav1alpha1.ModelCacheConditionReady, Status: metav1.ConditionTrue,
				}},
			},
		}
		modelArtifact := &kamav1alpha1.ModelArtifact{
			ObjectMeta: metav1.ObjectMeta{Name: artifactName, Namespace: controllerTestNamespace},
			Spec: kamav1alpha1.ModelArtifactSpec{
				Source:   kamav1alpha1.ModelArtifactSource{HuggingFace: &kamav1alpha1.HuggingFaceSource{}},
				CacheRef: &corev1.LocalObjectReference{Name: modelCache.Name},
			},
		}
		kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(claim, modelCache).Build()
		reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient}}
		if _, err := reconciler.resolveArtifactStorage(context.Background(), modelArtifact); err == nil ||
			err.Error() != "cache claim is terminating" {
			t.Fatalf("resolveArtifactStorage() error = %v, want cache claim termination", err)
		}
	})
}

func TestImporterJobSecurityAndMounts(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	owner := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: artifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid")},
	}
	job, err := newImportJob(scheme, ImporterOptions{
		Image: testImporterImage, PullPolicy: corev1.PullNever,
		HubEndpoint: testHubEndpoint,
	}, importJobOptions{
		Owner: owner, Name: "artifact-job", ConfigMapName: "artifact-spec", CacheClaim: cacheName,
		SourceClaim: sourceName, TokenSecret: &kamav1alpha1.SecretKeyReference{Name: "hub-token", Key: tokenName},
		OperationID: testOperationID, ActiveTimeout: time.Hour,
	})
	if err != nil {
		t.Fatalf("newImportJob(): %v", err)
	}
	podSpec := job.Spec.Template.Spec
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Fatal("service account token is automounted")
	}
	if podSpec.SecurityContext == nil || podSpec.SecurityContext.FSGroup != nil ||
		podSpec.SecurityContext.FSGroupChangePolicy != nil {
		t.Fatalf("Copy Job must not apply fsGroup to adopted source: %+v", podSpec.SecurityContext)
	}
	container := podSpec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("importer root filesystem is not read-only")
	}
	mounts := make(map[string]corev1.VolumeMount, len(container.VolumeMounts))
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount
	}
	if !mounts[sourceName].ReadOnly || !mounts[tokenName].ReadOnly || mounts[cacheName].ReadOnly {
		t.Fatalf("unexpected volume mounts: %+v", mounts)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != owner.UID {
		t.Fatalf("owner references = %+v", job.OwnerReferences)
	}
	for _, volume := range podSpec.Volumes {
		if volume.Name == tokenName && (volume.Secret == nil || volume.Secret.DefaultMode == nil ||
			*volume.Secret.DefaultMode != 0o440) {
			t.Fatalf("token volume mode = %+v", volume.Secret)
		}
	}
}

func TestCacheOnlyImporterJobAppliesFSGroup(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	owner := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: "hub", Namespace: controllerTestNamespace, UID: types.UID("artifact-uid")},
	}
	job, err := newImportJob(scheme, ImporterOptions{
		Image: testImporterImage, PullPolicy: corev1.PullNever,
		HubEndpoint: testHubEndpoint,
	}, importJobOptions{
		Owner: owner, Name: "hub-job", ConfigMapName: "hub-spec",
		CacheClaim: cacheName, OperationID: testOperationID,
	})
	if err != nil {
		t.Fatalf("newImportJob(): %v", err)
	}
	securityContext := job.Spec.Template.Spec.SecurityContext
	if securityContext == nil || securityContext.FSGroup == nil || *securityContext.FSGroup != 65532 ||
		securityContext.FSGroupChangePolicy == nil ||
		*securityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("cache-only Job fsGroup = %+v", securityContext)
	}
}

func TestDirectImporterJobDoesNotApplyFSGroupToAdoptedSource(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	owner := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: directArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid")},
	}
	job, err := newImportJob(scheme, ImporterOptions{
		Image: testImporterImage, PullPolicy: corev1.PullNever,
		HubEndpoint: testHubEndpoint,
	}, importJobOptions{
		Owner: owner, Name: "direct-job", ConfigMapName: "direct-spec",
		SourceClaim: "adopted", OperationID: testOperationID,
	})
	if err != nil {
		t.Fatalf("newImportJob(): %v", err)
	}
	if securityContext := job.Spec.Template.Spec.SecurityContext; securityContext == nil || securityContext.FSGroup != nil {
		t.Fatalf("Direct Job must not apply fsGroup to adopted source: %+v", securityContext)
	}
	if mount := job.Spec.Template.Spec.Containers[0].VolumeMounts[1]; !mount.ReadOnly {
		t.Fatalf("Direct source mount = %+v, want read-only", mount)
	}
}

func TestArtifactCleanupResourcesAreDetachedAndArtifactScoped(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: cachedArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid")},
	}
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Scheme: scheme,
		Importer: ImporterOptions{
			Image: testImporterImage, PullPolicy: corev1.PullNever,
			HubEndpoint: testHubEndpoint,
		},
	}}
	storage := artifactCleanupStorage{
		claimName: cacheName, claimUID: types.UID("claim-uid"),
		volumeUID: types.UID("volume-uid"),
	}
	operation, err := artifactCleanupOperationID(modelArtifact, storage)
	if err != nil {
		t.Fatalf("artifactCleanupOperationID(): %v", err)
	}
	configMap, job, err := reconciler.artifactCleanupResources(modelArtifact, storage, operation)
	if err != nil {
		t.Fatalf("artifactCleanupResources(): %v", err)
	}
	if len(configMap.OwnerReferences) != 0 || len(job.OwnerReferences) != 0 {
		t.Fatalf("cleanup resources must survive owner deletion: config=%v job=%v", configMap.OwnerReferences, job.OwnerReferences)
	}
	if !validDetachedCleanupMetadata(configMap.ObjectMeta, modelArtifact) ||
		!validDetachedCleanupMetadata(job.ObjectMeta, modelArtifact) {
		t.Fatalf("cleanup identity labels are incomplete: config=%v job=%v", configMap.Labels, job.Labels)
	}
	var spec artifact.Spec
	if err := json.Unmarshal([]byte(configMap.Data[importSpecKey]), &spec); err != nil {
		t.Fatalf("decode cleanup spec: %v", err)
	}
	if err := artifact.ValidateSpec(spec); err != nil {
		t.Fatalf("cleanup spec is invalid: %v", err)
	}
	if spec.Mode != artifact.ModeCleanup || spec.OperationID != operation || spec.CacheRoot != cacheMountPath ||
		spec.Cleanup == nil || spec.Cleanup.OperationPrefix != string(modelArtifact.UID)+"-" {
		t.Fatalf("cleanup spec = %#v", spec)
	}
	pod := job.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil {
		t.Fatalf("cleanup Job does not receive cache fsGroup: %+v", pod.SecurityContext)
	}
	for _, volume := range pod.Volumes {
		if volume.Name == sourceName || volume.Name == tokenName {
			t.Fatalf("cleanup Job mounted unrelated adopted input: %+v", pod.Volumes)
		}
	}

	forged := configMap.DeepCopy()
	controller := true
	forged.OwnerReferences = []metav1.OwnerReference{{UID: types.UID("foreign"), Controller: &controller}}
	if err := validateExistingConfigMap(configMap, forged); err == nil {
		t.Fatal("detached ConfigMap collision with an owner reference was accepted")
	}
}

func TestArtifactCleanupUsesDurableLocationInsteadOfCurrentCacheStatus(t *testing.T) {
	t.Parallel()
	oldClaim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "original-cache", Namespace: controllerTestNamespace, UID: types.UID("original-claim-uid"),
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
	}
	newClaim := oldClaim.DeepCopy()
	newClaim.Name = "replacement-cache"
	newClaim.UID = types.UID("replacement-claim-uid")
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name: cachedArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
		},
		Spec: kamav1alpha1.ModelArtifactSpec{
			CacheRef: &corev1.LocalObjectReference{Name: sharedCacheName},
		},
		Status: kamav1alpha1.ModelArtifactStatus{Location: &kamav1alpha1.ModelArtifactLocationStatus{
			ClaimName: oldClaim.Name, ClaimUID: oldClaim.UID, ReadOnly: true,
		}},
	}
	tests := []struct {
		name        string
		cacheStatus kamav1alpha1.ModelCacheStatus
	}{
		{name: "cache status identity lost"},
		{name: "cache now points at a replacement", cacheStatus: kamav1alpha1.ModelCacheStatus{
			ClaimName: newClaim.Name, ClaimUID: newClaim.UID,
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			modelCache := &kamav1alpha1.ModelCache{
				ObjectMeta: metav1.ObjectMeta{
					Name: sharedCacheName, Namespace: controllerTestNamespace, UID: types.UID("current-cache-uid"),
				},
				Status: testCase.cacheStatus,
			}
			kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(oldClaim.DeepCopy(), newClaim.DeepCopy(), modelCache).Build()
			reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient}, APIReader: kubeClient}
			storage, required, err := reconciler.resolveArtifactCleanupStorage(context.Background(), modelArtifact)
			if err != nil {
				t.Fatalf("resolveArtifactCleanupStorage(): %v", err)
			}
			if !required || storage.claimName != oldClaim.Name || storage.claimUID != oldClaim.UID {
				t.Fatalf("cleanup storage = %+v, required=%v; want original durable location", storage, required)
			}
		})
	}
}

func TestCachedArtifactDeletionCreatesDetachedCleanupJob(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	now := metav1.Now()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: cacheName, Namespace: controllerTestNamespace, UID: types.UID("claim-uid")},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
	}
	cache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: sharedCacheName, Namespace: controllerTestNamespace, UID: types.UID("cache-uid")},
		Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
			ExistingClaim: &corev1.LocalObjectReference{Name: claim.Name},
		}},
		Status: kamav1alpha1.ModelCacheStatus{ClaimName: claim.Name, ClaimUID: claim.UID},
	}
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name: cachedArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
			DeletionTimestamp: &now, Finalizers: []string{kamav1alpha1.ModelArtifactFinalizer},
			Annotations: map[string]string{
				"kama.tannerburns.github.io/artifact-cleanup-complete": "0123456789abcdef0123",
			},
		},
		Spec: kamav1alpha1.ModelArtifactSpec{
			Format: kamav1alpha1.ArtifactFormatGGUF, Entrypoint: testModelEntrypoint,
			Source: kamav1alpha1.ModelArtifactSource{HuggingFace: &kamav1alpha1.HuggingFaceSource{
				Repository: "owner/model", Revision: "main", Files: []string{testModelEntrypoint},
			}},
			CacheRef: &corev1.LocalObjectReference{Name: cache.Name},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, cache, modelArtifact).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme,
		Importer: ImporterOptions{
			Image: testImporterImage, PullPolicy: corev1.PullNever,
			HubEndpoint: testHubEndpoint,
		},
	}}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(modelArtifact),
	})
	if err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if result.RequeueAfter != activeCleanupRequeue {
		t.Fatalf("cleanup requeue = %v, want %v", result.RequeueAfter, activeCleanupRequeue)
	}
	var jobs batchv1.JobList
	if err := kubeClient.List(context.Background(), &jobs, client.InNamespace(modelArtifact.Namespace)); err != nil {
		t.Fatalf("list cleanup Jobs: %v", err)
	}
	if len(jobs.Items) != 1 || !validDetachedCleanupMetadata(jobs.Items[0].ObjectMeta, modelArtifact) {
		t.Fatalf("cleanup Jobs = %#v", jobs.Items)
	}
	assertArtifactFinalizer(t, kubeClient, client.ObjectKeyFromObject(modelArtifact), true)
	var configs corev1.ConfigMapList
	if err := kubeClient.List(context.Background(), &configs, client.InNamespace(modelArtifact.Namespace)); err != nil {
		t.Fatalf("list cleanup ConfigMaps: %v", err)
	}
	if len(configs.Items) != 1 || !validDetachedCleanupMetadata(configs.Items[0].ObjectMeta, modelArtifact) {
		t.Fatalf("cleanup ConfigMaps = %#v", configs.Items)
	}
	var retainedClaim corev1.PersistentVolumeClaim
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &retainedClaim); err != nil {
		t.Fatalf("cleanup removed adopted cache claim: %v", err)
	}
}

func TestArtifactCleanupCompletionRemovesResourcesBeforeFinalizer(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	now := metav1.Now()
	operation := "0123456789abcdef0123"
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name: cachedArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
			DeletionTimestamp: &now, Finalizers: []string{kamav1alpha1.ModelArtifactFinalizer},
		},
		Status: kamav1alpha1.ModelArtifactStatus{CleanupOperationID: operation},
	}
	resourceReconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Scheme: scheme,
		Importer: ImporterOptions{
			Image: testImporterImage, PullPolicy: corev1.PullNever,
			HubEndpoint: testHubEndpoint,
		},
	}}
	configMap, job, err := resourceReconciler.artifactCleanupResources(modelArtifact,
		artifactCleanupStorage{claimName: cacheName}, operation)
	if err != nil {
		t.Fatalf("artifactCleanupResources(): %v", err)
	}
	priorConfig, priorJob, err := resourceReconciler.artifactCleanupResources(modelArtifact,
		artifactCleanupStorage{claimName: cacheName}, "abcdef0123456789abcd")
	if err != nil {
		t.Fatalf("prior artifactCleanupResources(): %v", err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(modelArtifact, configMap, job, priorConfig, priorJob).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient, Scheme: scheme}}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelArtifact)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("delete cleanup Job: %v", err)
	}
	assertArtifactFinalizer(t, kubeClient, request.NamespacedName, true)
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("delete cleanup ConfigMap: %v", err)
	}
	assertArtifactFinalizer(t, kubeClient, request.NamespacedName, true)
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("remove artifact finalizer: %v", err)
	}
	var current kamav1alpha1.ModelArtifact
	err = kubeClient.Get(context.Background(), request.NamespacedName, &current)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get deleted artifact: %v", err)
	}
	if err == nil && controllerutil.ContainsFinalizer(&current, kamav1alpha1.ModelArtifactFinalizer) {
		t.Fatal("artifact finalizer remains after detached resources disappeared")
	}
}

func TestDirectArtifactDeletionDoesNotCreateCleanupJob(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	now := metav1.Now()
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name: directArtifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
			DeletionTimestamp: &now, Finalizers: []string{kamav1alpha1.ModelArtifactFinalizer},
		},
		Spec: kamav1alpha1.ModelArtifactSpec{
			Source: kamav1alpha1.ModelArtifactSource{PersistentVolumeClaim: &kamav1alpha1.PersistentVolumeClaimSource{
				ClaimName: "adopted", ImportPolicy: kamav1alpha1.PVCImportPolicyDirect,
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(modelArtifact).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient, Scheme: scheme}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(modelArtifact),
	}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	var jobs batchv1.JobList
	if err := kubeClient.List(context.Background(), &jobs, client.InNamespace(modelArtifact.Namespace)); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("Direct deletion created cache cleanup Jobs: %#v", jobs.Items)
	}
}

func TestArtifactNotFoundRemovesGaugeSeries(t *testing.T) {
	namespace, name := "metrics-not-found", artifactName
	modelArtifactReady.WithLabelValues(namespace, name).Set(1)
	modelArtifactSizeBytes.WithLabelValues(namespace, name).Set(123)
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
	}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if modelArtifactReady.DeleteLabelValues(namespace, name) || modelArtifactSizeBytes.DeleteLabelValues(namespace, name) {
		t.Fatal("artifact gauge series remained after NotFound reconciliation")
	}
}

func assertArtifactFinalizer(
	t *testing.T,
	kubeClient client.Client,
	key client.ObjectKey,
	want bool,
) {
	t.Helper()
	var current kamav1alpha1.ModelArtifact
	if err := kubeClient.Get(context.Background(), key, &current); err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	if got := controllerutil.ContainsFinalizer(&current, kamav1alpha1.ModelArtifactFinalizer); got != want {
		t.Fatalf("artifact finalizer = %v, want %v", got, want)
	}
}

func TestLeaseExpiry(t *testing.T) {
	t.Parallel()
	duration := int32(30)
	renewed := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	lease := &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{
		RenewTime: &renewed, LeaseDurationSeconds: &duration,
	}}
	if !leaseExpired(lease, time.Now()) {
		t.Fatal("old Lease did not expire")
	}
}

func TestReleaseLeaseUsesUncachedAPIReader(t *testing.T) {
	t.Parallel()
	const (
		namespace = controllerTestNamespace
		name      = "artifact-operation"
		holder    = "artifact-uid"
	)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: types.UID("lease-uid"), ResourceVersion: "1",
			Labels: map[string]string{
				managedByLabel: kamaName, componentLabel: artifactImportComponent, leaseFingerprintLabel: "fingerprint",
			},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr(holder)},
	}
	liveClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(lease).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Client: &staleLeaseReadClient{Client: liveClient},
	}, APIReader: liveClient}
	if err := reconciler.releaseLease(context.Background(), namespace, name, holder); err != nil {
		t.Fatalf("releaseLease(): %v", err)
	}
	var released coordinationv1.Lease
	if err := liveClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &released); !apierrors.IsNotFound(err) {
		t.Fatalf("released Lease still exists: %v", err)
	}
}

func TestAcquireLeaseUsesUncachedAPIReaderAfterCreateCollision(t *testing.T) {
	t.Parallel()
	modelArtifact := &kamav1alpha1.ModelArtifact{ObjectMeta: metav1.ObjectMeta{
		Name: artifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
	}}
	storage := artifactStorage{
		cacheUID: "cache-uid", cacheClaimUID: types.UID("claim-uid"),
		cacheVolumeUID: types.UID("volume-uid"),
	}
	fingerprint, err := artifactLeaseFingerprint(modelArtifact, storage)
	if err != nil {
		t.Fatalf("artifactLeaseFingerprint(): %v", err)
	}
	holder := string(modelArtifact.UID)
	duration := int32(leaseDuration.Seconds())
	now := metav1.NewMicroTime(time.Now().UTC())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deterministicName("kama-artifact-lease", fingerprint),
			Namespace: controllerTestNamespace,
			Labels: map[string]string{
				managedByLabel: kamaName, componentLabel: artifactImportComponent,
				leaseFingerprintLabel: fingerprint,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder, LeaseDurationSeconds: &duration, AcquireTime: &now, RenewTime: &now,
		},
	}
	liveClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(lease).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Client: &staleLeaseReadClient{Client: liveClient},
	}, APIReader: liveClient}
	name, acquired, err := reconciler.acquireLease(context.Background(), modelArtifact, storage)
	if err != nil {
		t.Fatalf("acquireLease(): %v", err)
	}
	if !acquired || name != lease.Name {
		t.Fatalf("acquireLease() = (%q, %v), want (%q, true)", name, acquired, lease.Name)
	}
}

func TestArtifactLifecycleQuiescenceUsesUncachedLists(t *testing.T) {
	t.Parallel()
	modelArtifact := &kamav1alpha1.ModelArtifact{ObjectMeta: metav1.ObjectMeta{
		Name: artifactName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"),
	}}
	controller := true
	oldJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "old-import", Namespace: controllerTestNamespace,
		Labels: map[string]string{
			managedByLabel: kamaName, componentLabel: artifactImportComponent,
			artifactUIDLabel: string(modelArtifact.UID), operationIDLabel: "old-operation",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: kamav1alpha1.GroupVersion.String(), Kind: "ModelArtifact",
			Name: modelArtifact.Name, UID: modelArtifact.UID, Controller: &controller,
		}},
	}}
	foreignJob := oldJob.DeepCopy()
	foreignJob.Name = "foreign-import"
	foreignJob.OwnerReferences = nil
	foreignJob.Labels = map[string]string{
		artifactUIDLabel: string(modelArtifact.UID), operationIDLabel: "foreign-operation",
	}
	storage := artifactStorage{cacheUID: "cache-uid"}
	currentFingerprint, err := artifactLeaseFingerprint(modelArtifact, storage)
	if err != nil {
		t.Fatalf("artifactLeaseFingerprint(): %v", err)
	}
	holder := string(modelArtifact.UID)
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: "old-lease", Namespace: controllerTestNamespace,
		Labels: map[string]string{
			managedByLabel: kamaName, componentLabel: artifactImportComponent,
			leaseFingerprintLabel: "old-" + currentFingerprint,
		},
	}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder}}
	liveClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(oldJob, foreignJob, lease).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{
		Client: &staleArtifactListClient{Client: liveClient},
	}, APIReader: liveClient}

	cleaning, err := reconciler.cleanupObsoleteArtifactOperations(
		context.Background(), modelArtifact, "current-operation", storage,
	)
	if err != nil {
		t.Fatalf("cleanupObsoleteArtifactOperations(): %v", err)
	}
	if !cleaning {
		t.Fatal("cleanup did not observe the importer Job through APIReader")
	}
	var retainedLease coordinationv1.Lease
	if err := liveClient.Get(context.Background(), client.ObjectKeyFromObject(lease), &retainedLease); err != nil {
		t.Fatalf("cleanup removed Lease before importer quiescence: %v", err)
	}
	var retainedForeign batchv1.Job
	if err := liveClient.Get(context.Background(), client.ObjectKeyFromObject(foreignJob), &retainedForeign); err != nil {
		t.Fatalf("cleanup removed a Job without exact Kama ownership: %v", err)
	}

	secondJob := oldJob.DeepCopy()
	secondJob.Name = "deleting-import"
	secondJob.ResourceVersion = ""
	secondJob.UID = ""
	if err := liveClient.Create(context.Background(), secondJob); err != nil {
		t.Fatalf("create deletion importer Job: %v", err)
	}
	cleaning, err = reconciler.cleanupArtifactImporterResources(context.Background(), modelArtifact)
	if err != nil {
		t.Fatalf("cleanupArtifactImporterResources(): %v", err)
	}
	if !cleaning {
		t.Fatal("deletion cleanup did not observe the importer Job through APIReader")
	}
}

type staleLeaseReadClient struct {
	client.Client
}

func (c *staleLeaseReadClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, isLease := object.(*coordinationv1.Lease); isLease {
		return apierrors.NewNotFound(coordinationv1.Resource("leases"), key.Name)
	}
	return c.Client.Get(ctx, key, object, options...)
}

type staleArtifactListClient struct {
	client.Client
}

func (c *staleArtifactListClient) List(
	ctx context.Context,
	list client.ObjectList,
	options ...client.ListOption,
) error {
	switch list.(type) {
	case *batchv1.JobList, *corev1.ConfigMapList:
		return nil
	default:
		return c.Client.List(ctx, list, options...)
	}
}

func TestDeleteHelpersUseExactObjectPreconditions(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "delete-preconditions", Namespace: controllerTestNamespace,
		UID: types.UID("job-uid"), ResourceVersion: "7",
	}}
	liveClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(job).Build()
	recordingClient := &recordingObjectDeleteClient{Client: liveClient}
	if err := deleteForeground(context.Background(), recordingClient, job); err != nil {
		t.Fatalf("deleteForeground(): %v", err)
	}
	if recordingClient.preconditions == nil || recordingClient.preconditions.UID == nil ||
		*recordingClient.preconditions.UID != job.UID ||
		recordingClient.preconditions.ResourceVersion == nil ||
		*recordingClient.preconditions.ResourceVersion != job.ResourceVersion {
		t.Fatalf("delete preconditions = %+v, want exact UID and resourceVersion", recordingClient.preconditions)
	}
}

type recordingObjectDeleteClient struct {
	client.Client
	preconditions *metav1.Preconditions
}

func (c *recordingObjectDeleteClient) Delete(
	ctx context.Context,
	object client.Object,
	options ...client.DeleteOption,
) error {
	applied := (&client.DeleteOptions{}).ApplyOptions(options)
	if applied.Preconditions != nil {
		c.preconditions = applied.Preconditions.DeepCopy()
	}
	return c.Client.Delete(ctx, object, options...)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add batch scheme: %v", err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}
	if err := kamav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kama scheme: %v", err)
	}
	return scheme
}
