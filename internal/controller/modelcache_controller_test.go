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
	"math"
	"strings"
	"testing"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	"github.com/TannerBurns/kama/internal/artifact"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	modelCacheTestNamespace     = "models"
	modelCacheTestClaimName     = "claim"
	modelCacheTestClaimUID      = "claim-uid"
	modelCacheTestImporterImage = "example.invalid/importer:test"
	modelCacheTestManagedName   = "managed"
)

func TestPendingWaitForFirstConsumerClaimCreatesProbeConsumer(t *testing.T) {
	delayed := storagev1.VolumeBindingWaitForFirstConsumer
	immediate := storagev1.VolumeBindingImmediate
	delayedName := "delayed"
	immediateName := "immediate"
	tests := []struct {
		name              string
		claimStorageClass *string
		storageClasses    []storagev1.StorageClass
		wantJob           bool
		wantReason        string
	}{
		{
			name:              "explicit delayed class",
			claimStorageClass: &delayedName,
			storageClasses: []storagev1.StorageClass{{
				ObjectMeta: metav1.ObjectMeta{Name: delayedName}, VolumeBindingMode: &delayed,
			}},
			wantJob: true, wantReason: claimPendingReason,
		},
		{
			name: "default delayed class",
			storageClasses: []storagev1.StorageClass{{
				ObjectMeta: metav1.ObjectMeta{Name: delayedName, Annotations: map[string]string{
					defaultStorageClassAnnotation: annotationTrue,
				}},
				VolumeBindingMode: &delayed,
			}},
			wantJob: true, wantReason: claimPendingReason,
		},
		{
			name:              "explicit class wins over default",
			claimStorageClass: &immediateName,
			storageClasses: []storagev1.StorageClass{
				{ObjectMeta: metav1.ObjectMeta{Name: immediateName}, VolumeBindingMode: &immediate},
				{
					ObjectMeta: metav1.ObjectMeta{Name: delayedName, Annotations: map[string]string{
						defaultStorageClassAnnotation: annotationTrue,
					}},
					VolumeBindingMode: &delayed,
				},
			},
			wantReason: claimPendingReason,
		},
		{
			name: "ambiguous defaults fail closed",
			storageClasses: []storagev1.StorageClass{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "first", Annotations: map[string]string{
						defaultStorageClassAnnotation: annotationTrue,
					}},
					VolumeBindingMode: &delayed,
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "second", Annotations: map[string]string{
						betaDefaultStorageClassAnnotation: "TRUE",
					}},
					VolumeBindingMode: &delayed,
				},
			},
			wantReason: "StorageClassResolutionFailed",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := modelCacheTestScheme(t)
			cache := testExistingClaimCache("wffc-"+strings.ReplaceAll(testCase.name, " ", "-"), modelCacheTestClaimName)
			claim := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: modelCacheTestClaimName, Namespace: cache.Namespace, UID: types.UID(modelCacheTestClaimUID),
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: testCase.claimStorageClass,
					VolumeMode:       ptr(corev1.PersistentVolumeFilesystem),
				},
				Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
			}
			objects := []client.Object{cache, claim}
			for index := range testCase.storageClasses {
				objects = append(objects, &testCase.storageClasses[index])
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(objects...).Build()
			reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
				Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(8),
				Importer: ImporterOptions{Image: modelCacheTestImporterImage},
			}}

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(cache),
			}); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			var jobs batchv1.JobList
			if err := kubeClient.List(context.Background(), &jobs, client.InNamespace(cache.Namespace)); err != nil {
				t.Fatalf("list probe Jobs: %v", err)
			}
			if got := len(jobs.Items); (got == 1) != testCase.wantJob {
				t.Fatalf("probe Job count = %d, wantJob=%v", got, testCase.wantJob)
			}
			if testCase.wantJob {
				assertJobMountsClaim(t, &jobs.Items[0], claim.Name)
			}
			var updated kamav1alpha1.ModelCache
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(cache), &updated); err != nil {
				t.Fatalf("get updated ModelCache: %v", err)
			}
			ready := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
			if ready == nil || ready.Reason != testCase.wantReason {
				t.Fatalf("Ready condition = %+v, want reason %q", ready, testCase.wantReason)
			}
		})
	}
}

func TestPeriodicProbePreservesMatchingReadyStatus(t *testing.T) {
	tests := []struct {
		name               string
		observedGeneration int64
		mutateStatus       func(*kamav1alpha1.ModelCacheStatus)
		wantReady          bool
		wantReason         string
		wantProbeCleared   bool
	}{
		{name: "matching identity", observedGeneration: 7, wantReady: true, wantReason: probeSucceededReason},
		{name: "stale generation", observedGeneration: 6, wantReady: false, wantReason: probeRunningReason},
		{
			name: "changed volume identity", observedGeneration: 7,
			mutateStatus: func(status *kamav1alpha1.ModelCacheStatus) {
				status.VolumeName = "replaced-volume"
			},
			wantReady: false, wantReason: probeRunningReason, wantProbeCleared: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := modelCacheTestScheme(t)
			cache := testExistingClaimCache(
				"refresh-"+strings.ReplaceAll(testCase.name, " ", "-"), modelCacheTestClaimName,
			)
			cache.Generation = 7
			cache.Status.ObservedGeneration = testCase.observedGeneration
			cache.Status.ClaimName = modelCacheTestClaimName
			cache.Status.ClaimUID = types.UID(modelCacheTestClaimUID)
			cache.Status.Capacity = ptr(resource.MustParse("10Gi"))
			cache.Status.FreeSpace = ptr(resource.MustParse("8Gi"))
			cache.Status.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			cache.Status.VolumeMode = corev1.PersistentVolumeFilesystem
			cache.Status.MountScope = kamav1alpha1.MountScopeSingleNode
			lastProbe := metav1.NewTime(time.Now().Add(-defaultProbeInterval - time.Minute))
			cache.Status.LastProbeTime = &lastProbe
			meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
				Type: kamav1alpha1.ModelCacheConditionReady, Status: metav1.ConditionTrue,
				ObservedGeneration: testCase.observedGeneration,
				Reason:             probeSucceededReason,
				Message:            "previous probe succeeded",
			})
			if testCase.mutateStatus != nil {
				testCase.mutateStatus(&cache.Status)
			}
			claim := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: modelCacheTestClaimName, Namespace: cache.Namespace, UID: types.UID(modelCacheTestClaimUID),
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					VolumeMode:  ptr(corev1.PersistentVolumeFilesystem),
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimBound, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
				},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
			reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
				Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(8),
				Importer: ImporterOptions{Image: modelCacheTestImporterImage},
			}}

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(cache),
			}); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			var updated kamav1alpha1.ModelCache
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(cache), &updated); err != nil {
				t.Fatalf("get updated ModelCache: %v", err)
			}
			ready := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
			if ready == nil || (ready.Status == metav1.ConditionTrue) != testCase.wantReady || ready.Reason != testCase.wantReason {
				t.Fatalf("Ready condition = %+v, want ready=%v reason=%q", ready, testCase.wantReady, testCase.wantReason)
			}
			if testCase.wantProbeCleared && (updated.Status.LastProbeTime != nil || updated.Status.FreeSpace != nil) {
				t.Fatalf("stale probe measurements were retained: lastProbe=%v freeSpace=%v",
					updated.Status.LastProbeTime, updated.Status.FreeSpace)
			}
			deleteModelCacheMetrics(cache.Namespace, cache.Name)
		})
	}
}

func TestManagedClaimDeletionBlocksEveryClaimReference(t *testing.T) {
	tests := []struct {
		name       string
		extra      func(namespace, claimName string) []client.Object
		wantReason string
	}{
		{
			name: "another cache adoption",
			extra: func(namespace, claimName string) []client.Object {
				return []client.Object{&kamav1alpha1.ModelCache{
					ObjectMeta: metav1.ObjectMeta{Name: "adopter", Namespace: namespace, UID: types.UID("adopter-uid")},
					Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
						ExistingClaim: &corev1.LocalObjectReference{Name: claimName},
					}},
				}}
			},
			wantReason: "ClaimAdoptedByAnotherCache",
		},
		{
			name: "artifact PVC source",
			extra: func(namespace, claimName string) []client.Object {
				return []client.Object{&kamav1alpha1.ModelArtifact{
					ObjectMeta: metav1.ObjectMeta{Name: sourceName, Namespace: namespace},
					Spec: kamav1alpha1.ModelArtifactSpec{Source: kamav1alpha1.ModelArtifactSource{
						PersistentVolumeClaim: &kamav1alpha1.PersistentVolumeClaimSource{ClaimName: claimName},
					}},
				}}
			},
			wantReason: artifactClaimRefsReason,
		},
		{
			name: "artifact cache reference",
			extra: func(namespace, _ string) []client.Object {
				return []client.Object{&kamav1alpha1.ModelArtifact{
					ObjectMeta: metav1.ObjectMeta{Name: "cache-ref", Namespace: namespace},
					Spec: kamav1alpha1.ModelArtifactSpec{
						CacheRef: &corev1.LocalObjectReference{Name: "delete"},
					},
				}}
			},
			wantReason: artifactReferencesRemainReason,
		},
		{
			name: "artifact status location",
			extra: func(namespace, claimName string) []client.Object {
				return []client.Object{&kamav1alpha1.ModelArtifact{
					ObjectMeta: metav1.ObjectMeta{Name: "location", Namespace: namespace},
					Status: kamav1alpha1.ModelArtifactStatus{Location: &kamav1alpha1.ModelArtifactLocationStatus{
						ClaimName: claimName,
					}},
				}}
			},
			wantReason: artifactClaimRefsReason,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := modelCacheTestScheme(t)
			deletionTime := metav1.NewTime(time.Now().Add(-2 * cacheDeletionQuiescence))
			cache := &kamav1alpha1.ModelCache{
				ObjectMeta: metav1.ObjectMeta{
					Name: "delete", Namespace: modelCacheTestNamespace,
					UID: types.UID("cache-uid"), ResourceVersion: "1",
					Finalizers: []string{kamav1alpha1.ModelCacheFinalizer}, DeletionTimestamp: &deletionTime,
				},
				Spec: kamav1alpha1.ModelCacheSpec{
					Storage: kamav1alpha1.ModelCacheStorageSpec{ClaimTemplate: &kamav1alpha1.ModelCacheClaimTemplate{
						Spec: kamav1alpha1.ModelCacheClaimTemplateSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							VolumeMode:  corev1.PersistentVolumeFilesystem,
							Resources: kamav1alpha1.ModelCacheResourceRequirements{Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							}},
						},
					}},
					RetentionPolicy: kamav1alpha1.RetentionPolicyDelete,
				},
			}
			claimName := deterministicName(cache.Name+"-cache", string(cache.UID))
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: claimName, Namespace: cache.Namespace,
				Labels: map[string]string{
					managedByLabel: kamaName, componentLabel: modelCacheComponent,
					cacheNameLabel: boundedLabelValue(cache.Name), cacheUIDLabel: string(cache.UID),
				},
				Annotations: map[string]string{
					cacheDeletionGuardAnnotation:     string(cache.UID),
					cacheDeletionGuardedAtAnnotation: time.Now().Add(-cacheDeletionQuiescence - time.Second).UTC().Format(time.RFC3339Nano),
				},
			}, Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeMode:  ptr(corev1.PersistentVolumeFilesystem),
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				}},
			}}
			extraObjects := testCase.extra(cache.Namespace, claimName)
			objects := make([]client.Object, 0, 2+len(extraObjects))
			objects = append(objects, cache, claim)
			objects = append(objects, extraObjects...)
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(objects...).Build()
			reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
				Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(8),
			}}

			result, err := reconciler.reconcileDelete(context.Background(), cache)
			if err != nil {
				t.Fatalf("reconcileDelete(): %v", err)
			}
			if result.RequeueAfter != 15*time.Second {
				t.Fatalf("RequeueAfter = %v, want 15s", result.RequeueAfter)
			}
			var remaining corev1.PersistentVolumeClaim
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &remaining); err != nil {
				t.Fatalf("managed claim was deleted despite reference: %v", err)
			}
			var updated kamav1alpha1.ModelCache
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(cache), &updated); err != nil {
				t.Fatalf("get updated cache: %v", err)
			}
			degraded := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionDegraded)
			if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != testCase.wantReason {
				t.Fatalf("Degraded condition = %+v, want reason %q", degraded, testCase.wantReason)
			}
		})
	}
}

func TestCacheFailureEventsOnlyOnTransitions(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache := testExistingClaimCache("events", modelCacheTestClaimName)
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: modelCacheTestClaimName, Namespace: cache.Namespace, UID: types.UID(modelCacheTestClaimUID),
		},
	}
	recorder := events.NewFakeRecorder(4)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: recorder,
	}}

	for range 2 {
		if _, err := reconciler.updateClaimStatus(context.Background(), cache, claim, nil,
			claimPendingReason, "cache claim has not bound", time.Second); err != nil {
			t.Fatalf("updateClaimStatus(): %v", err)
		}
	}
	if got := len(recorder.Events); got != 1 {
		t.Fatalf("event count = %d, want 1 for repeated failure", got)
	}
	event := <-recorder.Events
	if !strings.Contains(event, "Warning ClaimPending") {
		t.Fatalf("event = %q", event)
	}
	if _, err := reconciler.updateClaimStatus(context.Background(), cache, claim, nil,
		"ClaimLost", "cache claim lost its volume", time.Second); err != nil {
		t.Fatalf("updateClaimStatus() transition: %v", err)
	}
	if got := len(recorder.Events); got != 1 {
		t.Fatalf("event count after reason transition = %d, want 1", got)
	}
	deleteModelCacheMetrics(cache.Namespace, cache.Name)
}

func TestModelCacheMetricsDeletedWhenObjectIsNotFound(t *testing.T) {
	namespace, name := "metrics", "missing-cache"
	modelCacheReady.WithLabelValues(namespace, name).Set(1)
	modelCacheCapacityBytes.WithLabelValues(namespace, name).Set(10)
	modelCacheFreeBytes.WithLabelValues(namespace, name).Set(8)

	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: fake.NewClientBuilder().WithScheme(modelCacheTestScheme(t)).Build(),
	}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if modelCacheReady.DeleteLabelValues(namespace, name) ||
		modelCacheCapacityBytes.DeleteLabelValues(namespace, name) ||
		modelCacheFreeBytes.DeleteLabelValues(namespace, name) {
		t.Fatal("one or more ModelCache gauge label sets remained after NotFound")
	}
}

func TestTerminalWaitForFirstConsumerJobIsRecreated(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	delayed := storagev1.VolumeBindingWaitForFirstConsumer
	storageClassName := "delayed"
	cache := testExistingClaimCache("terminal-wffc", modelCacheTestClaimName)
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: modelCacheTestClaimName, Namespace: cache.Namespace, UID: types.UID(modelCacheTestClaimUID),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClassName,
			VolumeMode:       ptr(corev1.PersistentVolumeFilesystem),
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	storageClass := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: storageClassName}, VolumeBindingMode: &delayed,
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim, storageClass).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(8),
		Importer: ImporterOptions{Image: modelCacheTestImporterImage},
	}}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}
	var jobs batchv1.JobList
	if err := kubeClient.List(context.Background(), &jobs, client.InNamespace(cache.Namespace)); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("initial probe Jobs = %d, err=%v", len(jobs.Items), err)
	}
	job := &jobs.Items[0]
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := kubeClient.Update(context.Background(), job); err != nil {
		t.Fatalf("mark probe Job failed: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("terminal-job Reconcile(): %v", err)
	}
	for range 3 {
		jobs = batchv1.JobList{}
		if err := kubeClient.List(context.Background(), &jobs, client.InNamespace(cache.Namespace)); err != nil {
			t.Fatalf("list replacement probe Jobs: %v", err)
		}
		if len(jobs.Items) == 1 && !jobComplete(&jobs.Items[0]) && !jobFailed(&jobs.Items[0]) &&
			jobs.Items[0].DeletionTimestamp.IsZero() {
			assertJobMountsClaim(t, &jobs.Items[0], claim.Name)
			return
		}
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("replacement Reconcile(): %v", err)
		}
	}
	t.Fatalf("terminal WFFC consumer was not replaced: %+v", jobs.Items)
}

func TestTerminatingClaimIsUnavailableAndClearsDependentStatus(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, claim := readyCacheAndBoundClaim("terminating", time.Now().Add(-time.Minute))
	deletionTime := metav1.Now()
	claim.DeletionTimestamp = &deletionTime
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(4),
	}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cache),
	}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	updated := getModelCache(t, kubeClient, cache)
	ready := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ClaimTerminating" {
		t.Fatalf("Ready condition = %+v", ready)
	}
	assertCacheDependentStatusCleared(t, &updated.Status)
}

func TestManagedClaimCollisionValidation(t *testing.T) {
	cache, claim := validManagedCacheAndClaim()
	expanded := claim.DeepCopy()
	expanded.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
	if err := validateManagedCacheClaim(cache, expanded); err != nil {
		t.Fatalf("expanded valid managed claim rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*corev1.PersistentVolumeClaim)
	}{
		{name: "identity label", mutate: func(claim *corev1.PersistentVolumeClaim) { delete(claim.Labels, componentLabel) }},
		{name: "template label", mutate: func(claim *corev1.PersistentVolumeClaim) { claim.Labels["purpose"] = "other" }},
		{name: "access modes", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		}},
		{name: "undersized request", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("512Mi")
		}},
		{name: "owner reference", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.OwnerReferences = []metav1.OwnerReference{{Name: "collision", UID: types.UID("other")}}
		}},
		{name: "data source", mutate: func(claim *corev1.PersistentVolumeClaim) {
			claim.Spec.DataSource = &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: "seed"}
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			collision := claim.DeepCopy()
			testCase.mutate(collision)
			if err := validateManagedCacheClaim(cache, collision); err == nil {
				t.Fatal("validateManagedCacheClaim() accepted collision")
			}
		})
	}
}

func TestRefreshTransientEnsureFailurePreservesReadyWithinBound(t *testing.T) {
	tests := []struct {
		name      string
		probeTime time.Time
		wantReady bool
	}{
		{name: "within staleness bound", probeTime: time.Now().Add(-defaultProbeInterval - time.Minute), wantReady: true},
		{name: "past staleness bound", probeTime: time.Now().Add(-maxCacheReadyStaleness - time.Minute)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := modelCacheTestScheme(t)
			cache, claim := readyCacheAndBoundClaim("ensure-"+strings.ReplaceAll(testCase.name, " ", "-"), testCase.probeTime)
			operation := deterministicName("probe", string(cache.UID), string(claim.UID))
			configName := deterministicName(cache.Name+"-probe-config", operation)
			collision := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: cache.Namespace}}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim, collision).Build()
			reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
				Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(4),
				Importer: ImporterOptions{Image: modelCacheTestImporterImage},
			}}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(cache),
			}); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			updated := getModelCache(t, kubeClient, cache)
			ready := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
			if ready == nil || (ready.Status == metav1.ConditionTrue) != testCase.wantReady {
				t.Fatalf("Ready condition = %+v, want ready=%v", ready, testCase.wantReady)
			}
		})
	}
}

func TestRefreshTransientResultReadPreservesReady(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, claim := readyCacheAndBoundClaim("result-read", time.Now().Add(-defaultProbeInterval-time.Minute))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(4),
		Importer: ImporterOptions{Image: modelCacheTestImporterImage},
	}}
	job, _, err := reconciler.ensureProbeResources(context.Background(), cache, claim)
	if err != nil {
		t.Fatalf("ensureProbeResources(): %v", err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := kubeClient.Update(context.Background(), job); err != nil {
		t.Fatalf("complete probe Job: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cache),
	}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	updated := getModelCache(t, kubeClient, cache)
	if !meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionReady) {
		t.Fatalf("Ready was not preserved while the terminal Job result was transiently unavailable: %+v",
			updated.Status.Conditions)
	}
}

func TestMissingAndReplacementClaimsClearDependentStatus(t *testing.T) {
	tests := []struct {
		name         string
		claim        func(*corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim
		wantClaimUID types.UID
		wantReason   string
	}{
		{name: "missing", wantReason: "ClaimNotFound"},
		{
			name: "pending replacement",
			claim: func(claim *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
				claim.UID = types.UID("replacement-uid")
				claim.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}
				claim.Spec.StorageClassName = ptr("")
				return claim
			},
			wantClaimUID: types.UID("replacement-uid"), wantReason: claimPendingReason,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := modelCacheTestScheme(t)
			cache, claim := readyCacheAndBoundClaim("stale-"+testCase.name, time.Now().Add(-time.Minute))
			objects := []client.Object{cache}
			if testCase.claim != nil {
				objects = append(objects, testCase.claim(claim))
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(objects...).Build()
			reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
				Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(4),
			}}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(cache),
			}); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			updated := getModelCache(t, kubeClient, cache)
			ready := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
			if ready == nil || ready.Reason != testCase.wantReason {
				t.Fatalf("Ready condition = %+v, want reason %q", ready, testCase.wantReason)
			}
			if updated.Status.ClaimUID != testCase.wantClaimUID {
				t.Fatalf("ClaimUID = %q, want %q", updated.Status.ClaimUID, testCase.wantClaimUID)
			}
			assertCacheDependentStatusCleared(t, &updated.Status)
		})
	}
}

func TestProbeResultRejectsUnsignedValuesOutsideQuantityRange(t *testing.T) {
	valid := artifact.Result{Probe: &artifact.ProbeResult{
		CapacityBytes: uint64(math.MaxInt64), FreeBytes: uint64(math.MaxInt64),
	}}
	if err := validateCacheProbeResult(valid); err != nil {
		t.Fatalf("maximum signed quantity rejected: %v", err)
	}
	for _, result := range []artifact.Result{
		{Probe: &artifact.ProbeResult{CapacityBytes: uint64(math.MaxInt64) + 1}},
		{Probe: &artifact.ProbeResult{CapacityBytes: uint64(math.MaxInt64), FreeBytes: uint64(math.MaxInt64) + 1}},
	} {
		if err := validateCacheProbeResult(result); err == nil {
			t.Fatalf("out-of-range probe accepted: %+v", result.Probe)
		}
	}
}

func TestManagedClaimDeletionGuardsQuiescesAndEmitsDecisionEvents(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, claim := deletingManagedCacheAndClaim()
	recorder := events.NewFakeRecorder(8)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
	trackedClient := &recordingDeleteClient{Client: kubeClient}
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: trackedClient, Scheme: scheme, Recorder: recorder,
	}}
	result, err := reconciler.reconcileDelete(context.Background(), cache)
	if err != nil {
		t.Fatalf("guard reconcileDelete(): %v", err)
	}
	if result.RequeueAfter != cacheDeletionQuiescence {
		t.Fatalf("guard RequeueAfter = %v, want %v", result.RequeueAfter, cacheDeletionQuiescence)
	}
	var guarded corev1.PersistentVolumeClaim
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &guarded); err != nil {
		t.Fatalf("get guarded claim: %v", err)
	}
	if guarded.Annotations[cacheDeletionGuardAnnotation] != string(cache.UID) {
		t.Fatalf("deletion guard = %q", guarded.Annotations[cacheDeletionGuardAnnotation])
	}
	guarded.Annotations[cacheDeletionGuardedAtAnnotation] = time.Now().Add(-cacheDeletionQuiescence - time.Second).
		UTC().Format(time.RFC3339Nano)
	if err := kubeClient.Update(context.Background(), &guarded); err != nil {
		t.Fatalf("age deletion guard: %v", err)
	}
	result, err = reconciler.reconcileDelete(context.Background(), cache)
	if err != nil {
		t.Fatalf("delete reconcileDelete(): %v", err)
	}
	if result.RequeueAfter != 2*time.Second {
		t.Fatalf("delete RequeueAfter = %v, want 2s", result.RequeueAfter)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &guarded); err == nil {
		t.Fatal("guarded unreferenced managed claim was not deleted")
	}
	if trackedClient.pvcDeletePreconditions == nil || trackedClient.pvcDeletePreconditions.UID == nil ||
		*trackedClient.pvcDeletePreconditions.UID != claim.UID ||
		trackedClient.pvcDeletePreconditions.ResourceVersion == nil ||
		*trackedClient.pvcDeletePreconditions.ResourceVersion == "" {
		t.Fatalf("PVC delete preconditions = %+v, want exact UID and resourceVersion", trackedClient.pvcDeletePreconditions)
	}
	eventText := drainEvents(recorder)
	for _, reason := range []string{"ManagedClaimDeletionGuarded", "ManagedClaimDeletionRequested"} {
		if !strings.Contains(eventText, reason) {
			t.Fatalf("events %q do not contain %q", eventText, reason)
		}
	}
}

func TestManagedClaimDeletionUsesLiveReaderForFinalReferenceScan(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, claim := deletingManagedCacheAndClaim()
	claim.Annotations[cacheDeletionGuardAnnotation] = string(cache.UID)
	claim.Annotations[cacheDeletionGuardedAtAnnotation] = time.Now().Add(-cacheDeletionQuiescence - time.Second).
		UTC().Format(time.RFC3339Nano)
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
	liveCache := cache.DeepCopy()
	liveClaim := claim.DeepCopy()
	adopter := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: "live-adopter", Namespace: cache.Namespace, UID: types.UID("adopter")},
		Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
			ExistingClaim: &corev1.LocalObjectReference{Name: claim.Name},
		}},
	}
	liveReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(liveCache, liveClaim, adopter).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: cachedClient, Scheme: scheme, Recorder: events.NewFakeRecorder(8),
	}, APIReader: liveReader}
	result, err := reconciler.reconcileDelete(context.Background(), cache)
	if err != nil {
		t.Fatalf("reconcileDelete(): %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("RequeueAfter = %v, want 15s", result.RequeueAfter)
	}
	var remaining corev1.PersistentVolumeClaim
	if err := cachedClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &remaining); err != nil {
		t.Fatalf("cached claim was deleted despite live reference: %v", err)
	}
}

func TestManagedClaimDeletionRevalidatesExactGuardedObject(t *testing.T) {
	cache, claim := deletingManagedCacheAndClaim()
	guardedAt := time.Now().Add(-cacheDeletionQuiescence - time.Second).UTC()
	claim.Annotations[cacheDeletionGuardAnnotation] = string(cache.UID)
	claim.Annotations[cacheDeletionGuardedAtAnnotation] = guardedAt.Format(time.RFC3339Nano)
	tests := []struct {
		name      string
		mutate    func(*corev1.PersistentVolumeClaim)
		wantError bool
	}{
		{name: "exact object"},
		{name: "replacement UID", mutate: func(live *corev1.PersistentVolumeClaim) {
			live.UID = types.UID("replacement-uid")
		}, wantError: true},
		{name: "removed guard", mutate: func(live *corev1.PersistentVolumeClaim) {
			delete(live.Annotations, cacheDeletionGuardAnnotation)
		}, wantError: true},
		{name: "changed guard timestamp", mutate: func(live *corev1.PersistentVolumeClaim) {
			live.Annotations[cacheDeletionGuardedAtAnnotation] = guardedAt.Add(time.Second).Format(time.RFC3339Nano)
		}, wantError: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := modelCacheTestScheme(t)
			liveClaim := claim.DeepCopy()
			if testCase.mutate != nil {
				testCase.mutate(liveClaim)
			}
			liveReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(liveClaim).Build()
			reconciler := &ModelCacheReconciler{APIReader: liveReader}
			confirmed, err := reconciler.revalidateManagedClaimDeletion(
				context.Background(), cache, claim, guardedAt,
			)
			if (err != nil) != testCase.wantError {
				t.Fatalf("revalidateManagedClaimDeletion() claim=%+v error=%v, wantError=%v",
					confirmed, err, testCase.wantError)
			}
		})
	}
}

func TestPrearmedManagedClaimDeletionGuardIsRestarted(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, claim := deletingManagedCacheAndClaim()
	prearmedAt := cache.DeletionTimestamp.Add(-time.Minute)
	claim.Annotations[cacheDeletionGuardAnnotation] = string(cache.UID)
	claim.Annotations[cacheDeletionGuardedAtAnnotation] = prearmedAt.Format(time.RFC3339Nano)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{Client: kubeClient, Scheme: scheme}}
	result, err := reconciler.reconcileDelete(context.Background(), cache)
	if err != nil {
		t.Fatalf("reconcileDelete(): %v", err)
	}
	if result.RequeueAfter != cacheDeletionQuiescence {
		t.Fatalf("RequeueAfter = %v, want restarted quiescence %v", result.RequeueAfter, cacheDeletionQuiescence)
	}
	var guarded corev1.PersistentVolumeClaim
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &guarded); err != nil {
		t.Fatalf("get guarded claim: %v", err)
	}
	restartedAt, err := time.Parse(time.RFC3339Nano, guarded.Annotations[cacheDeletionGuardedAtAnnotation])
	if err != nil || restartedAt.Before(cache.DeletionTimestamp.Time) || !restartedAt.After(prearmedAt) {
		t.Fatalf("restarted guard timestamp = %q, error=%v", guarded.Annotations[cacheDeletionGuardedAtAnnotation], err)
	}
}

func TestFinalManagedClaimScanIncludesCacheReference(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, claim := deletingManagedCacheAndClaim()
	lateArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: "late-cache-reference", Namespace: cache.Namespace},
		Spec: kamav1alpha1.ModelArtifactSpec{
			CacheRef: &corev1.LocalObjectReference{Name: cache.Name},
		},
	}
	liveReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cache, claim, lateArtifact).Build()
	reconciler := &ModelCacheReconciler{APIReader: liveReader}
	blocked, reason, _, err := reconciler.managedClaimDeletionBlocked(context.Background(), cache, claim.Name)
	if err != nil {
		t.Fatalf("managedClaimDeletionBlocked(): %v", err)
	}
	if !blocked || reason != artifactReferencesRemainReason {
		t.Fatalf("blocked=%v reason=%q, want late cache reference blocked", blocked, reason)
	}
}

func TestGuardedClaimCannotBecomeReadyInAdoptingCache(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache := testExistingClaimCache("guard-adopter", "guarded")
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "guarded", Namespace: cache.Namespace, UID: types.UID("claim-uid"),
			Annotations: map[string]string{cacheDeletionGuardAnnotation: ""},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: events.NewFakeRecorder(4),
	}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cache),
	}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	updated := getModelCache(t, kubeClient, cache)
	ready := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelCacheConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ClaimDeletionGuarded" {
		t.Fatalf("Ready condition = %+v", ready)
	}
}

func TestRetainDecisionEmitsTransitionEvent(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, _ := deletingManagedCacheAndClaim()
	cache.Spec.RetentionPolicy = kamav1alpha1.RetentionPolicyRetain
	recorder := events.NewFakeRecorder(4)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: recorder,
	}}
	if _, err := reconciler.reconcileDelete(context.Background(), cache); err != nil {
		t.Fatalf("reconcileDelete(): %v", err)
	}
	if eventText := drainEvents(recorder); !strings.Contains(eventText, "ManagedClaimRetained") {
		t.Fatalf("retain events = %q", eventText)
	}
}

func TestRetainRecoveryClearsSelfOwnedDeletionGuardBeforeFinalizing(t *testing.T) {
	scheme := modelCacheTestScheme(t)
	cache, claim := deletingManagedCacheAndClaim()
	cache.Spec.RetentionPolicy = kamav1alpha1.RetentionPolicyRetain
	claim.Annotations[cacheDeletionGuardAnnotation] = string(cache.UID)
	claim.Annotations[cacheDeletionGuardedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	recorder := events.NewFakeRecorder(8)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelCache{}).WithObjects(cache, claim).Build()
	reconciler := &ModelCacheReconciler{reconcilerBase: reconcilerBase{
		Client: kubeClient, Scheme: scheme, Recorder: recorder,
	}}
	result, err := reconciler.reconcileDelete(context.Background(), cache)
	if err != nil {
		t.Fatalf("guard recovery reconcileDelete(): %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("RequeueAfter = %v, want 1s after guard removal", result.RequeueAfter)
	}
	var retained corev1.PersistentVolumeClaim
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &retained); err != nil {
		t.Fatalf("get retained claim: %v", err)
	}
	if _, guarded := retained.Annotations[cacheDeletionGuardAnnotation]; guarded {
		t.Fatalf("self-owned deletion guard remains on retained claim: %v", retained.Annotations)
	}
	if _, guardedAt := retained.Annotations[cacheDeletionGuardedAtAnnotation]; guardedAt {
		t.Fatalf("deletion guard timestamp remains on retained claim: %v", retained.Annotations)
	}
	if eventText := drainEvents(recorder); !strings.Contains(eventText, "ManagedClaimDeletionGuardRemoved") {
		t.Fatalf("guard recovery events = %q", eventText)
	}
	if _, err := reconciler.reconcileDelete(context.Background(), cache); err != nil {
		t.Fatalf("retain finalization reconcileDelete(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &retained); err != nil {
		t.Fatalf("retained claim was deleted during finalization: %v", err)
	}
}

func readyCacheAndBoundClaim(
	name string,
	lastProbe time.Time,
) (*kamav1alpha1.ModelCache, *corev1.PersistentVolumeClaim) {
	cache := testExistingClaimCache(name, modelCacheTestClaimName)
	cache.Status.ObservedGeneration = cache.Generation
	cache.Status.ClaimName = modelCacheTestClaimName
	cache.Status.ClaimUID = types.UID(modelCacheTestClaimUID)
	cache.Status.Capacity = ptr(resource.MustParse("10Gi"))
	cache.Status.FreeSpace = ptr(resource.MustParse("8Gi"))
	cache.Status.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	cache.Status.VolumeMode = corev1.PersistentVolumeFilesystem
	cache.Status.MountScope = kamav1alpha1.MountScopeSingleNode
	probeTime := metav1.NewTime(lastProbe)
	cache.Status.LastProbeTime = &probeTime
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelCacheConditionReady, Status: metav1.ConditionTrue,
		ObservedGeneration: cache.Generation, Reason: probeSucceededReason, Message: "previous probe succeeded",
	})
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: modelCacheTestClaimName, Namespace: cache.Namespace, UID: types.UID(modelCacheTestClaimUID),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  ptr(corev1.PersistentVolumeFilesystem),
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		},
	}
	return cache, claim
}

func validManagedCacheAndClaim() (*kamav1alpha1.ModelCache, *corev1.PersistentVolumeClaim) {
	storageClass := "fast"
	cache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{
			Name: modelCacheTestManagedName, Namespace: modelCacheTestNamespace, UID: types.UID("cache-uid"),
		},
		Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
			ClaimTemplate: &kamav1alpha1.ModelCacheClaimTemplate{
				Metadata: kamav1alpha1.ModelCacheClaimTemplateMetadata{
					Labels:      map[string]string{"purpose": cacheName},
					Annotations: map[string]string{"note": modelCacheTestManagedName},
				},
				Spec: kamav1alpha1.ModelCacheClaimTemplateSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &storageClass,
					VolumeMode:       corev1.PersistentVolumeFilesystem,
					Resources: kamav1alpha1.ModelCacheResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					}},
				},
			},
		}},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: deterministicName(cache.Name+"-cache", string(cache.UID)), Namespace: cache.Namespace,
			Labels: map[string]string{
				managedByLabel: kamaName, componentLabel: modelCacheComponent,
				cacheNameLabel: boundedLabelValue(cache.Name), cacheUIDLabel: string(cache.UID), "purpose": cacheName,
			},
			Annotations: map[string]string{"note": modelCacheTestManagedName},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			VolumeMode:       ptr(corev1.PersistentVolumeFilesystem),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}
	return cache, claim
}

func deletingManagedCacheAndClaim() (*kamav1alpha1.ModelCache, *corev1.PersistentVolumeClaim) {
	cache, claim := validManagedCacheAndClaim()
	deletionTime := metav1.NewTime(time.Now().Add(-2 * cacheDeletionQuiescence))
	cache.ResourceVersion = "1"
	cache.Generation = 1
	cache.Finalizers = []string{kamav1alpha1.ModelCacheFinalizer}
	cache.DeletionTimestamp = &deletionTime
	cache.Spec.RetentionPolicy = kamav1alpha1.RetentionPolicyDelete
	claim.UID = types.UID("claim-uid")
	return cache, claim
}

func drainEvents(recorder *events.FakeRecorder) string {
	items := make([]string, 0, len(recorder.Events))
	for len(recorder.Events) > 0 {
		items = append(items, <-recorder.Events)
	}
	return strings.Join(items, "\n")
}

func getModelCache(
	t *testing.T,
	kubeClient client.Client,
	cache *kamav1alpha1.ModelCache,
) *kamav1alpha1.ModelCache {
	t.Helper()
	var updated kamav1alpha1.ModelCache
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(cache), &updated); err != nil {
		t.Fatalf("get ModelCache: %v", err)
	}
	return &updated
}

func assertCacheDependentStatusCleared(t *testing.T, status *kamav1alpha1.ModelCacheStatus) {
	t.Helper()
	if status.Capacity != nil || status.FreeSpace != nil || len(status.AccessModes) != 0 || status.VolumeMode != "" ||
		status.MountScope != "" || status.StorageClassName != "" || status.VolumeName != "" || status.VolumeUID != "" ||
		status.NodeAffinity != nil || status.LastProbeTime != nil {
		t.Fatalf("dependent storage status was not cleared: %+v", status)
	}
}

func testExistingClaimCache(name, claimName string) *kamav1alpha1.ModelCache {
	return &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: modelCacheTestNamespace, UID: types.UID(name + "-uid"), Generation: 1,
			Finalizers: []string{kamav1alpha1.ModelCacheFinalizer},
		},
		Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
			ExistingClaim: &corev1.LocalObjectReference{Name: claimName},
		}},
	}
}

func assertJobMountsClaim(t *testing.T, job *batchv1.Job, claimName string) {
	t.Helper()
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
			return
		}
	}
	t.Fatalf("probe Job volumes do not mount claim %q: %+v", claimName, job.Spec.Template.Spec.Volumes)
}

type recordingDeleteClient struct {
	client.Client
	pvcDeletePreconditions *metav1.Preconditions
}

func (c *recordingDeleteClient) Delete(
	ctx context.Context,
	object client.Object,
	options ...client.DeleteOption,
) error {
	if _, isClaim := object.(*corev1.PersistentVolumeClaim); isClaim {
		applied := (&client.DeleteOptions{}).ApplyOptions(options)
		if applied.Preconditions != nil {
			c.pvcDeletePreconditions = applied.Preconditions.DeepCopy()
		}
	}
	return c.Client.Delete(ctx, object, options...)
}

func modelCacheTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for name, addToScheme := range map[string]func(*runtime.Scheme) error{
		"core":          corev1.AddToScheme,
		"batch":         batchv1.AddToScheme,
		"storage":       storagev1.AddToScheme,
		"Kama v1alpha1": kamav1alpha1.AddToScheme,
	} {
		if err := addToScheme(scheme); err != nil {
			t.Fatalf("add %s scheme: %v", name, err)
		}
	}
	return scheme
}
