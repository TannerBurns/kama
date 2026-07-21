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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	kamaruntime "github.com/TannerBurns/kama/internal/runtime"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	testRuntimeCPUImage  = "example.invalid/kama-runtime-cpu:test"
	testRuntimeCUDAImage = "example.invalid/kama-runtime-cuda:test"
	testLlamaCommit      = "af6528e6df5d798f7f1363ec1141699be0f638e2"
	testServingModelName = "model"
	testPodIP            = "127.0.0.9"
)

//nolint:gocyclo // This contract test deliberately checks the complete generated Pod in one fixture.
func TestModelDeploymentCreatesRestrictedCPUWorkload(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	artifact.Status.Location.MountScope = kamav1alpha1.MountScopeSingleNode
	artifact.Status.Location.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"worker-a"},
		}}}},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}

	var services corev1.ServiceList
	if err := kubeClient.List(context.Background(), &services, client.InNamespace(modelDeployment.Namespace)); err != nil {
		t.Fatalf("list Services: %v", err)
	}
	if len(services.Items) != 1 || len(services.Items[0].Spec.Ports) != 1 || services.Items[0].Spec.Ports[0].Port != runtimeHTTPPort {
		t.Fatalf("services = %+v, want one internal runtime Service", services.Items)
	}

	var workloads appsv1.DeploymentList
	if err := kubeClient.List(context.Background(), &workloads, client.InNamespace(modelDeployment.Namespace)); err != nil {
		t.Fatalf("list Deployments: %v", err)
	}
	if len(workloads.Items) != 1 {
		t.Fatalf("Deployments = %d, want 1", len(workloads.Items))
	}
	workload := workloads.Items[0]
	if workload.Spec.Replicas == nil || *workload.Spec.Replicas != 1 ||
		workload.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("workload execution policy = %+v", workload.Spec)
	}
	podSpec := workload.Spec.Template.Spec
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken ||
		podSpec.SecurityContext == nil || podSpec.SecurityContext.RunAsNonRoot == nil ||
		!*podSpec.SecurityContext.RunAsNonRoot || len(podSpec.Containers) != 1 ||
		!hasArtifactUnavailableSchedulingGate(podSpec.SchedulingGates) {
		t.Fatalf("Pod security contract = %+v", podSpec)
	}
	container := podSpec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem || container.ReadinessProbe == nil ||
		container.ReadinessProbe.HTTPGet.Path != "/readyz" || container.LivenessProbe.HTTPGet.Path != "/livez" ||
		container.StartupProbe.HTTPGet.Path != "/startupz" {
		t.Fatalf("runtime container contract = %+v", container)
	}
	if _, found := container.Resources.Requests[kamav1alpha1.DefaultAcceleratorResource]; found {
		t.Fatal("CPU workload contains an accelerator request")
	}
	if len(podSpec.Volumes) != 3 || len(container.VolumeMounts) != 3 || !container.VolumeMounts[0].ReadOnly {
		t.Fatalf("runtime volumes = %+v mounts = %+v", podSpec.Volumes, container.VolumeMounts)
	}
	if podSpec.Affinity == nil || podSpec.Affinity.NodeAffinity == nil ||
		podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		len(podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) != 1 {
		t.Fatalf("artifact node affinity was not preserved: %+v", podSpec.Affinity)
	}

	var configs corev1.ConfigMapList
	if err := kubeClient.List(context.Background(), &configs, client.InNamespace(modelDeployment.Namespace)); err != nil {
		t.Fatalf("list ConfigMaps: %v", err)
	}
	if len(configs.Items) != 1 || configs.Items[0].Immutable == nil || !*configs.Items[0].Immutable {
		t.Fatalf("runtime ConfigMaps = %+v", configs.Items)
	}
	config, err := kamaruntime.DecodeConfig(bytes.NewBufferString(configs.Items[0].Data[runtimeConfigKey]))
	if err != nil {
		t.Fatalf("DecodeConfig(): %v", err)
	}
	if config.Mode != kamaruntime.ModeCPU || config.MaxContextTokens != 0 || config.DesiredConcurrency != 1 ||
		config.Deployment.Fingerprint == "" || config.Artifact.Digest != artifact.Status.ArtifactDigest {
		t.Fatalf("runtime config = %+v", config)
	}

	var updated kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &updated); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	if !meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionDegraded) ||
		meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) {
		t.Fatalf("status conditions = %+v", updated.Status.Conditions)
	}
	if updated.Status.Runtime == nil || updated.Status.Runtime.LlamaCommit != "" ||
		updated.Status.Runtime.ObservedFingerprint != "" || updated.Status.Runtime.AcceleratorDetected != nil {
		t.Fatalf("status contains unobserved runtime facts: %+v", updated.Status.Runtime)
	}
}

func TestModelDeploymentAcceleratorInjectsExactlyOneGPU(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementAccelerator)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	var workloads appsv1.DeploymentList
	if err := kubeClient.List(context.Background(), &workloads); err != nil || len(workloads.Items) != 1 {
		t.Fatalf("list workload: count=%d err=%v", len(workloads.Items), err)
	}
	container := workloads.Items[0].Spec.Template.Spec.Containers[0]
	want := resource.MustParse("1")
	request := container.Resources.Requests[kamav1alpha1.DefaultAcceleratorResource]
	limit := container.Resources.Limits[kamav1alpha1.DefaultAcceleratorResource]
	if container.Image != testRuntimeCUDAImage ||
		request.Cmp(want) != 0 || limit.Cmp(want) != 0 ||
		workloads.Items[0].Spec.Template.Spec.NodeSelector[corev1.LabelArchStable] != "amd64" {
		t.Fatalf("accelerator workload = %+v", workloads.Items[0].Spec.Template.Spec)
	}
}

func TestRuntimeFingerprintChangesForEveryMutableInputDomain(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	modelDeployment, artifact, _ := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	reconciler := testModelDeploymentReconciler(nil, scheme)
	baseline, _, _, _, err := reconciler.desiredRuntime(modelDeployment, artifact)
	if err != nil {
		t.Fatalf("baseline desiredRuntime(): %v", err)
	}

	tests := map[string]func(*kamav1alpha1.ModelDeployment, *kamav1alpha1.ModelArtifact, *ModelDeploymentReconciler){
		"artifact digest": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact, _ *ModelDeploymentReconciler) {
			artifact.Status.ArtifactDigest = strings.Repeat("b", 64)
			artifact.Status.Files[0].SHA256 = artifact.Status.ArtifactDigest
		},
		"artifact location": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact, _ *ModelDeploymentReconciler) {
			artifact.Status.Location.SubPath = "artifacts/replaced"
		},
		"model reference": func(deployment *kamav1alpha1.ModelDeployment, _ *kamav1alpha1.ModelArtifact, _ *ModelDeploymentReconciler) {
			deployment.Spec.ModelRef.Name = "replacement"
		},
		"placement": func(deployment *kamav1alpha1.ModelDeployment, _ *kamav1alpha1.ModelArtifact, _ *ModelDeploymentReconciler) {
			deployment.Spec.Placement = kamav1alpha1.ModelDeploymentPlacementSpec{
				Mode:                kamav1alpha1.ModelDeploymentPlacementAccelerator,
				AcceleratorResource: kamav1alpha1.DefaultAcceleratorResource,
			}
		},
		"resources": func(deployment *kamav1alpha1.ModelDeployment, _ *kamav1alpha1.ModelArtifact, _ *ModelDeploymentReconciler) {
			deployment.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("3Gi")
		},
		"runtime": func(deployment *kamav1alpha1.ModelDeployment, _ *kamav1alpha1.ModelArtifact, _ *ModelDeploymentReconciler) {
			value := int64(8192)
			deployment.Spec.Runtime.MaxContextTokens = &value
		},
		"runtime image": func(_ *kamav1alpha1.ModelDeployment, _ *kamav1alpha1.ModelArtifact, reconciler *ModelDeploymentReconciler) {
			reconciler.Runtime.CPUImage = "example.invalid/kama-runtime-cpu:replacement"
		},
		"llama commit": func(_ *kamav1alpha1.ModelDeployment, _ *kamav1alpha1.ModelArtifact, reconciler *ModelDeploymentReconciler) {
			reconciler.Runtime.LlamaCommit = strings.Repeat("b", 40)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			deploymentCopy := modelDeployment.DeepCopy()
			artifactCopy := artifact.DeepCopy()
			caseReconciler := testModelDeploymentReconciler(nil, scheme)
			mutate(deploymentCopy, artifactCopy, caseReconciler)
			fingerprint, _, _, _, err := caseReconciler.desiredRuntime(deploymentCopy, artifactCopy)
			if err != nil {
				t.Fatalf("desiredRuntime(): %v", err)
			}
			if fingerprint == baseline {
				t.Fatalf("fingerprint remained %q after %s change", fingerprint, name)
			}
		})
	}
}

func TestLoadedWorkloadPreservationRequiresExactArtifactIdentityAndLocation(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	modelDeployment, artifact, _ := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	reconciler := testModelDeploymentReconciler(nil, scheme)
	fingerprint, locationHash, _, image, err := reconciler.desiredRuntime(modelDeployment, artifact)
	if err != nil {
		t.Fatalf("desiredRuntime(): %v", err)
	}
	modelDeployment.Status.ObservedGeneration = modelDeployment.Generation
	modelDeployment.Status.Artifact = &kamav1alpha1.ModelDeploymentArtifactStatus{
		Name: artifact.Name, UID: artifact.UID, Digest: artifact.Status.ArtifactDigest,
	}
	modelDeployment.Status.Runtime = &kamav1alpha1.ModelDeploymentRuntimeStatus{
		DesiredImage: image, DesiredFingerprint: fingerprint, LoadedFingerprint: fingerprint,
	}
	workload := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			runtimeFingerprintAnnotation:   fingerprint,
			artifactLocationHashAnnotation: locationHash,
		}},
	}}}
	if !reconciler.canPreserveLoadedWorkload(modelDeployment, artifact, workload) {
		t.Fatal("exact loaded artifact identity was not preservable")
	}

	tests := map[string]func(*kamav1alpha1.ModelDeployment, *kamav1alpha1.ModelArtifact){
		"model reference": func(deployment *kamav1alpha1.ModelDeployment, _ *kamav1alpha1.ModelArtifact) {
			deployment.Spec.ModelRef.Name = "replacement-model"
		},
		"artifact UID": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact) {
			artifact.UID = types.UID("replacement-artifact-uid")
		},
		"artifact digest": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact) {
			artifact.Status.ArtifactDigest = strings.Repeat("b", 64)
			artifact.Status.Files[0].SHA256 = artifact.Status.ArtifactDigest
		},
		"location claim name": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact) {
			artifact.Status.Location.ClaimName = "replacement-claim"
		},
		"location claim UID": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact) {
			artifact.Status.Location.ClaimUID = types.UID("replacement-claim-uid")
		},
		"location subpath": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact) {
			artifact.Status.Location.SubPath = "artifacts/replacement"
		},
		"location volume UID": func(_ *kamav1alpha1.ModelDeployment, artifact *kamav1alpha1.ModelArtifact) {
			artifact.Status.Location.VolumeUID = types.UID("replacement-volume-uid")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			deploymentCopy := modelDeployment.DeepCopy()
			artifactCopy := artifact.DeepCopy()
			mutate(deploymentCopy, artifactCopy)
			if reconciler.canPreserveLoadedWorkload(deploymentCopy, artifactCopy, workload.DeepCopy()) {
				t.Fatalf("loaded workload was preserved after %s changed", name)
			}
		})
	}
}

func TestModelDeploymentReconcileDrainsLoadedWorkloadForChangedArtifactIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*testing.T, client.Client, *kamav1alpha1.ModelDeployment, *kamav1alpha1.ModelArtifact) *kamav1alpha1.ModelArtifact
	}{
		{
			name: "same reference replacement UID",
			mutate: func(t *testing.T, kubeClient client.Client, _ *kamav1alpha1.ModelDeployment,
				artifact *kamav1alpha1.ModelArtifact,
			) *kamav1alpha1.ModelArtifact {
				t.Helper()
				if err := kubeClient.Delete(context.Background(), artifact); err != nil {
					t.Fatalf("delete original ModelArtifact: %v", err)
				}
				replacement := artifact.DeepCopy()
				replacement.ResourceVersion = ""
				replacement.UID = types.UID("replacement-artifact-uid")
				markTestArtifactUnavailable(replacement)
				if err := kubeClient.Create(context.Background(), replacement); err != nil {
					t.Fatalf("create replacement ModelArtifact: %v", err)
				}
				if err := kubeClient.Status().Update(context.Background(), replacement); err != nil {
					t.Fatalf("publish replacement ModelArtifact status: %v", err)
				}
				return replacement
			},
		},
		{
			name: "same reference digest",
			mutate: func(t *testing.T, kubeClient client.Client, _ *kamav1alpha1.ModelDeployment,
				artifact *kamav1alpha1.ModelArtifact,
			) *kamav1alpha1.ModelArtifact {
				t.Helper()
				var changed kamav1alpha1.ModelArtifact
				if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(artifact), &changed); err != nil {
					t.Fatalf("get ModelArtifact for digest change: %v", err)
				}
				changed.Status.ArtifactDigest = strings.Repeat("b", 64)
				changed.Status.Files[0].SHA256 = changed.Status.ArtifactDigest
				markTestArtifactUnavailable(&changed)
				if err := kubeClient.Status().Update(context.Background(), &changed); err != nil {
					t.Fatalf("publish changed digest: %v", err)
				}
				return &changed
			},
		},
		{
			name: "same reference location",
			mutate: func(t *testing.T, kubeClient client.Client, _ *kamav1alpha1.ModelDeployment,
				artifact *kamav1alpha1.ModelArtifact,
			) *kamav1alpha1.ModelArtifact {
				t.Helper()
				var changed kamav1alpha1.ModelArtifact
				if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(artifact), &changed); err != nil {
					t.Fatalf("get ModelArtifact for location change: %v", err)
				}
				changed.Status.Location.SubPath = "artifacts/replacement"
				markTestArtifactUnavailable(&changed)
				if err := kubeClient.Status().Update(context.Background(), &changed); err != nil {
					t.Fatalf("publish changed location: %v", err)
				}
				return &changed
			},
		},
		{
			name: "model reference",
			mutate: func(t *testing.T, kubeClient client.Client, deployment *kamav1alpha1.ModelDeployment,
				artifact *kamav1alpha1.ModelArtifact,
			) *kamav1alpha1.ModelArtifact {
				t.Helper()
				replacement := artifact.DeepCopy()
				replacement.Name = "replacement-model"
				replacement.ResourceVersion = ""
				replacement.UID = types.UID("replacement-artifact-uid")
				markTestArtifactUnavailable(replacement)
				if err := kubeClient.Create(context.Background(), replacement); err != nil {
					t.Fatalf("create referenced replacement ModelArtifact: %v", err)
				}
				if err := kubeClient.Status().Update(context.Background(), replacement); err != nil {
					t.Fatalf("publish referenced replacement status: %v", err)
				}
				var current kamav1alpha1.ModelDeployment
				if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), &current); err != nil {
					t.Fatalf("get ModelDeployment for reference change: %v", err)
				}
				current.Generation++
				current.Spec.ModelRef.Name = replacement.Name
				if err := kubeClient.Update(context.Background(), &current); err != nil {
					t.Fatalf("change model reference: %v", err)
				}
				return replacement
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := testScheme(t)
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add apps scheme: %v", err)
			}
			if err := discoveryv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add discovery scheme: %v", err)
			}
			modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
				WithObjects(modelDeployment, artifact, claim).Build()
			reconciler := testModelDeploymentReconciler(kubeClient, scheme)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("initial Reconcile(): %v", err)
			}

			var workload appsv1.Deployment
			if err := kubeClient.Get(context.Background(), types.NamespacedName{
				Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment),
			}, &workload); err != nil {
				t.Fatalf("get initial workload: %v", err)
			}
			fingerprint := workload.Spec.Template.Annotations[runtimeFingerprintAnnotation]
			if fingerprint == "" {
				t.Fatal("initial workload has no runtime fingerprint")
			}
			var loaded kamav1alpha1.ModelDeployment
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &loaded); err != nil {
				t.Fatalf("get ModelDeployment checkpoint: %v", err)
			}
			loaded.Status.ObservedGeneration = loaded.Generation
			loaded.Status.DesiredReplicas = 1
			loaded.Status.ReadyReplicas = 1
			loaded.Status.Artifact = &kamav1alpha1.ModelDeploymentArtifactStatus{
				Name: artifact.Name, UID: artifact.UID, Digest: artifact.Status.ArtifactDigest,
			}
			loaded.Status.Runtime = &kamav1alpha1.ModelDeploymentRuntimeStatus{
				DesiredImage:       testRuntimeCPUImage,
				DesiredFingerprint: fingerprint,
				LoadedFingerprint:  fingerprint,
			}
			loaded.Status.DeploymentRef = &kamav1alpha1.ModelDeploymentObjectReference{
				Name: workload.Name, UID: workload.UID,
			}
			if err := kubeClient.Status().Update(context.Background(), &loaded); err != nil {
				t.Fatalf("publish loaded checkpoint: %v", err)
			}

			selectedArtifact := test.mutate(t, kubeClient, modelDeployment, artifact)
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("identity-change Reconcile(): %v", err)
			}
			var remaining appsv1.Deployment
			err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(&workload), &remaining)
			if err == nil && remaining.DeletionTimestamp.IsZero() {
				t.Fatal("loaded workload remained active after artifact identity changed")
			}
			if err != nil && !apierrors.IsNotFound(err) {
				t.Fatalf("get drained workload: %v", err)
			}

			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("blocked replacement Reconcile(): %v", err)
			}
			var workloads appsv1.DeploymentList
			if err := kubeClient.List(context.Background(), &workloads,
				client.InNamespace(modelDeployment.Namespace),
				client.MatchingLabels{modelDeploymentUIDLabel: string(modelDeployment.UID)}); err != nil {
				t.Fatalf("list blocked replacement workloads: %v", err)
			}
			for index := range workloads.Items {
				if workloads.Items[index].DeletionTimestamp.IsZero() {
					t.Fatalf("replacement workload became active: %+v", workloads.Items[index])
				}
			}

			var current kamav1alpha1.ModelDeployment
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
				t.Fatalf("get blocked ModelDeployment status: %v", err)
			}
			artifactCondition := meta.FindStatusCondition(current.Status.Conditions,
				kamav1alpha1.ModelDeploymentConditionArtifactReady)
			resourceCondition := meta.FindStatusCondition(current.Status.Conditions,
				kamav1alpha1.ModelDeploymentConditionResourcesAvailable)
			if current.Status.ObservedGeneration != current.Generation ||
				current.Status.DesiredReplicas != 0 || current.Status.ReadyReplicas != 0 ||
				current.Status.DeploymentRef != nil || current.Status.Runtime != nil ||
				current.Status.ServiceRef == nil || current.Status.Artifact == nil ||
				current.Status.Artifact.Name != selectedArtifact.Name ||
				current.Status.Artifact.UID != selectedArtifact.UID ||
				current.Status.Artifact.Digest != selectedArtifact.Status.ArtifactDigest ||
				artifactCondition == nil || artifactCondition.Status != metav1.ConditionFalse ||
				resourceCondition == nil || resourceCondition.Status != metav1.ConditionFalse ||
				resourceCondition.Reason != "WaitingForArtifact" ||
				meta.IsStatusConditionTrue(current.Status.Conditions,
					kamav1alpha1.ModelDeploymentConditionServing) {
				t.Fatalf("blocked identity-change status = %+v", current.Status)
			}
		})
	}
}

func TestModelDeploymentNotReadyArtifactCreatesOnlyService(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	meta.SetStatusCondition(&artifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: artifact.Generation, Reason: "Importing", Message: "artifact import is active",
	})
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	var services corev1.ServiceList
	var workloads appsv1.DeploymentList
	_ = kubeClient.List(context.Background(), &services)
	_ = kubeClient.List(context.Background(), &workloads)
	if len(services.Items) != 1 || len(workloads.Items) != 0 {
		t.Fatalf("Services=%d Deployments=%d, want stable Service only", len(services.Items), len(workloads.Items))
	}
}

func TestModelDeploymentRefusesGeneratedServiceCollision(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	foreignService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: servingObjectName(modelDeployment), Namespace: modelDeployment.Namespace,
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim, foreignService).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(modelDeployment),
	}); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	var workloads appsv1.DeploymentList
	if err := kubeClient.List(context.Background(), &workloads); err != nil {
		t.Fatalf("list Deployments: %v", err)
	}
	if len(workloads.Items) != 0 {
		t.Fatalf("Deployments = %d, want none for a generated-resource collision", len(workloads.Items))
	}
	var updated kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &updated); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions,
		kamav1alpha1.ModelDeploymentConditionResourcesAvailable)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "GeneratedResourceCollision" {
		t.Fatalf("ResourcesAvailable = %+v, want GeneratedResourceCollision", condition)
	}
}

func TestModelDeploymentDrainsOldWorkloadWhenNewRuntimeConfigCollides(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}
	var oldWorkload appsv1.Deployment
	if err := kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment),
	}, &oldWorkload); err != nil {
		t.Fatalf("get old workload: %v", err)
	}
	var updated kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &updated); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	updated.Generation++
	updated.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("3Gi")
	if err := kubeClient.Update(context.Background(), &updated); err != nil {
		t.Fatalf("update ModelDeployment: %v", err)
	}
	fingerprint, _, _, _, err := reconciler.desiredRuntime(&updated, artifact)
	if err != nil {
		t.Fatalf("desiredRuntime(): %v", err)
	}
	collision := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: runtimeConfigName(&updated, fingerprint), Namespace: updated.Namespace,
	}}
	if err := kubeClient.Create(context.Background(), collision); err != nil {
		t.Fatalf("create runtime ConfigMap collision: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("collision Reconcile(): %v", err)
	}
	var remaining appsv1.Deployment
	err = kubeClient.Get(context.Background(), client.ObjectKeyFromObject(&oldWorkload), &remaining)
	if err == nil && remaining.DeletionTimestamp.IsZero() {
		t.Fatal("old serving workload remained active after the desired runtime input collided")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get drained old workload: %v", err)
	}
	var current kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
		t.Fatalf("get collision status: %v", err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions,
		kamav1alpha1.ModelDeploymentConditionResourcesAvailable)
	if condition == nil || condition.Reason != "GeneratedResourceCollision" ||
		meta.IsStatusConditionTrue(current.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) {
		t.Fatalf("collision status = %+v", current.Status)
	}
}

func TestModelDeploymentRepairsGeneratedWorkloadDrift(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}

	key := types.NamespacedName{Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment)}
	var service corev1.Service
	if err := kubeClient.Get(context.Background(), key, &service); err != nil {
		t.Fatalf("get Service: %v", err)
	}
	service.Spec.Selector = map[string]string{"foreign": "selector"}
	service.Spec.Ports[0].Port = 9090
	service.Spec.ExternalIPs = []string{"192.0.2.1"}
	if err := kubeClient.Update(context.Background(), &service); err != nil {
		t.Fatalf("inject Service drift: %v", err)
	}
	var workload appsv1.Deployment
	if err := kubeClient.Get(context.Background(), key, &workload); err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	replicas := int32(7)
	workload.Spec.Replicas = &replicas
	workload.Spec.Template.Spec.Containers[0].Image = "example.invalid/untrusted:latest"
	if err := kubeClient.Update(context.Background(), &workload); err != nil {
		t.Fatalf("inject Deployment drift: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("drift Reconcile(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), key, &service); err != nil {
		t.Fatalf("get repaired Service: %v", err)
	}
	if service.Spec.Ports[0].Port != runtimeHTTPPort || len(service.Spec.ExternalIPs) != 0 ||
		service.Spec.Selector[modelDeploymentUIDLabel] != string(modelDeployment.UID) {
		t.Fatalf("Service drift was not repaired: %+v", service.Spec)
	}
	if err := kubeClient.Get(context.Background(), key, &workload); err != nil {
		t.Fatalf("get repaired Deployment: %v", err)
	}
	if workload.Spec.Replicas == nil || *workload.Spec.Replicas != 1 ||
		workload.Spec.Template.Spec.Containers[0].Image != testRuntimeCPUImage {
		t.Fatalf("Deployment drift was not repaired: %+v", workload.Spec)
	}
}

func TestModelDeploymentPreservesKubernetesManagedDeploymentMetadata(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}

	key := types.NamespacedName{Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment)}
	var workload appsv1.Deployment
	if err := kubeClient.Get(context.Background(), key, &workload); err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	workload.Annotations = map[string]string{"deployment.kubernetes.io/revision": "1"}
	workload.Labels["example.test/control-plane-label"] = "retained"
	if err := kubeClient.Update(context.Background(), &workload); err != nil {
		t.Fatalf("add Kubernetes-managed metadata: %v", err)
	}
	resourceVersion := workload.ResourceVersion

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("metadata Reconcile(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), key, &workload); err != nil {
		t.Fatalf("get reconciled Deployment: %v", err)
	}
	if workload.Annotations["deployment.kubernetes.io/revision"] != "1" ||
		workload.Labels["example.test/control-plane-label"] != "retained" {
		t.Fatalf("Kubernetes-managed metadata was removed: labels=%v annotations=%v", workload.Labels, workload.Annotations)
	}
	if workload.ResourceVersion != resourceVersion {
		t.Fatalf("Deployment was rewritten only for unmanaged metadata: resourceVersion %q -> %q", resourceVersion, workload.ResourceVersion)
	}
}

func TestModelDeploymentFinalizerDeletesWorkloadBeforeReferenceRelease(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}
	if err := kubeClient.Delete(context.Background(), modelDeployment); err != nil {
		t.Fatalf("delete ModelDeployment: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first deletion Reconcile(): %v", err)
	}
	var current kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
		t.Fatalf("ModelDeployment reference was released before workload deletion: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&current, kamav1alpha1.ModelDeploymentFinalizer) {
		t.Fatal("ModelDeployment finalizer was removed while generated resources remained")
	}
	key := types.NamespacedName{Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment)}
	var workload appsv1.Deployment
	if err := kubeClient.Get(context.Background(), key, &workload); !apierrors.IsNotFound(err) {
		t.Fatalf("serving Deployment lookup error = %v, want NotFound before reference release", err)
	}

	for attempt := range 4 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("finalization Reconcile %d: %v", attempt+2, err)
		}
	}
	err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get finalized ModelDeployment: %v", err)
	}
	if err == nil && controllerutil.ContainsFinalizer(&current, kamav1alpha1.ModelDeploymentFinalizer) {
		t.Fatal("ModelDeployment finalizer remained after all generated resources disappeared")
	}
}

func TestModelDeploymentInvalidSpecDrainsExistingWorkload(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}

	var current kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	current.Spec.Resources.Requests[kamav1alpha1.DefaultAcceleratorResource] = resource.MustParse("1")
	if err := kubeClient.Update(context.Background(), &current); err != nil {
		t.Fatalf("write invalid spec with admission bypassed: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("invalid-spec Reconcile(): %v", err)
	}

	var workload appsv1.Deployment
	err := kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment),
	}, &workload)
	if err == nil {
		t.Fatal("previous workload remained after the current spec failed validation")
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
		t.Fatalf("get rejected ModelDeployment: %v", err)
	}
	resourceCondition := meta.FindStatusCondition(current.Status.Conditions,
		kamav1alpha1.ModelDeploymentConditionResourcesAvailable)
	if current.Status.DesiredReplicas != 0 || current.Status.Runtime != nil ||
		resourceCondition == nil || resourceCondition.Reason != "InvalidSpec" ||
		meta.IsStatusConditionTrue(current.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) {
		t.Fatalf("invalid-spec status = %+v", current.Status)
	}
	var services corev1.ServiceList
	if err := kubeClient.List(context.Background(), &services, client.InNamespace(modelDeployment.Namespace)); err != nil ||
		len(services.Items) != 1 {
		t.Fatalf("stable Services=%d err=%v, want one", len(services.Items), err)
	}
}

//nolint:gocyclo // This scenario intentionally verifies one complete outage-and-recovery transition.
func TestModelDeploymentPreservesLoadedWorkloadThroughTransientArtifactLoss(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}
	var workload appsv1.Deployment
	if err := kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment),
	}, &workload); err != nil {
		t.Fatalf("get workload: %v", err)
	}
	controller := true
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "chat-runtime-rs", Namespace: modelDeployment.Namespace, UID: types.UID("replicaset-uid"),
			Labels: workload.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment",
				Name: workload.Name, UID: workload.UID, Controller: &controller,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: workload.Spec.Selector.DeepCopy(), Template: *workload.Spec.Template.DeepCopy()},
	}
	if err := kubeClient.Create(context.Background(), replicaSet); err != nil {
		t.Fatalf("create serving ReplicaSet: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "runtime-ready", Namespace: modelDeployment.Namespace,
			Labels: workload.Spec.Template.Labels, Annotations: workload.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: replicaSetKind,
				Name: replicaSet.Name, UID: replicaSet.UID, Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}, PodIP: testPodIP},
	}
	if err := kubeClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create ready Pod: %v", err)
	}
	ready := true
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "chat-endpoints", Namespace: modelDeployment.Namespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: workload.Name},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{testPodIP}, Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name},
		}},
	}
	if err := kubeClient.Create(context.Background(), endpointSlice); err != nil {
		t.Fatalf("create ready EndpointSlice: %v", err)
	}
	fingerprint := workload.Spec.Template.Annotations[runtimeFingerprintAnnotation]
	healthyRuntimeTransport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"phase":"Ready","reason":"RuntimeReady","message":"llama-server is ready",` +
			`"ready":true,"deployment":{"uid":"deployment-uid","fingerprint":"` + fingerprint + `"},` +
			`"runtime":{"mode":"CPU","effectiveContextTokens":4096,"desiredConcurrency":1,` +
			`"llamaCPPCommit":"` + testLlamaCommit + `","acceleratorDetected":false}}`
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})
	reconciler.HTTPClient = &http.Client{Transport: healthyRuntimeTransport}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("ready Reconcile(): %v", err)
	}
	var loadedDeployment kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &loadedDeployment); err != nil {
		t.Fatalf("get loaded ModelDeployment: %v", err)
	}
	if loadedDeployment.Status.Runtime == nil ||
		loadedDeployment.Status.Runtime.LoadedFingerprint != fingerprint ||
		loadedDeployment.Status.Runtime.ObservedFingerprint != fingerprint ||
		loadedDeployment.Status.Runtime.LlamaCommit != testLlamaCommit ||
		loadedDeployment.Status.Runtime.AcceleratorDetected == nil ||
		*loadedDeployment.Status.Runtime.AcceleratorDetected ||
		!meta.IsStatusConditionTrue(loadedDeployment.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) {
		t.Fatalf("loaded runtime checkpoint = %+v", loadedDeployment.Status.Runtime)
	}
	changedDeployment := loadedDeployment.DeepCopy()
	changedDeployment.Generation++
	changedDeployment.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("3Gi")
	if reconciler.canPreserveLoadedWorkload(changedDeployment, artifact, &workload) {
		t.Fatal("runtime/resource update was preserved while its artifact was unavailable")
	}
	originalRuntime := reconciler.Runtime
	reconciler.Runtime.LlamaCommit = strings.Repeat("b", 40)
	if reconciler.canPreserveLoadedWorkload(&loadedDeployment, artifact, &workload) {
		t.Fatal("controller runtime identity update was preserved while its artifact was unavailable")
	}
	reconciler.Runtime = originalRuntime
	reconciler.HTTPClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("temporary supervisor diagnostics failure")
	})}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("diagnostics outage Reconcile(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &loadedDeployment); err != nil {
		t.Fatalf("get ModelDeployment after diagnostics outage: %v", err)
	}
	if loadedDeployment.Status.Runtime == nil ||
		loadedDeployment.Status.Runtime.State != kamav1alpha1.ModelDeploymentRuntimeInitializing ||
		loadedDeployment.Status.Runtime.LoadedFingerprint != fingerprint ||
		loadedDeployment.Status.Runtime.ObservedFingerprint != "" ||
		loadedDeployment.Status.Runtime.LlamaCommit != "" ||
		loadedDeployment.Status.Runtime.AcceleratorDetected != nil {
		t.Fatalf("diagnostics outage erased loaded checkpoint: %+v", loadedDeployment.Status.Runtime)
	}
	reconciler.HTTPClient = &http.Client{Transport: healthyRuntimeTransport}
	var currentArtifact kamav1alpha1.ModelArtifact
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(artifact), &currentArtifact); err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	meta.SetStatusCondition(&currentArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: currentArtifact.Generation, Reason: "CacheProbeFailed", Message: "cache probe is temporarily unavailable",
	})
	if err := kubeClient.Status().Update(context.Background(), &currentArtifact); err != nil {
		t.Fatalf("mark artifact unavailable: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("outage Reconcile(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(&workload), &workload); err != nil {
		t.Fatalf("loaded workload was removed during transient artifact outage: %v", err)
	}
	var updated kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &updated); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	artifactCondition := meta.FindStatusCondition(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionArtifactReady)
	if artifactCondition == nil || artifactCondition.Status != metav1.ConditionFalse ||
		!meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) {
		t.Fatalf("conditions after outage = %+v", updated.Status.Conditions)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(&workload), &workload); err != nil {
		t.Fatalf("get suspended workload: %v", err)
	}
	if !workload.Spec.Paused {
		t.Fatal("Deployment was not paused during the artifact outage")
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(replicaSet), replicaSet); err != nil {
		t.Fatalf("get suspended ReplicaSet: %v", err)
	}
	if !hasArtifactUnavailableSchedulingGate(replicaSet.Spec.Template.Spec.SchedulingGates) {
		t.Fatal("ReplicaSet replacement Pods were not scheduling-gated during the artifact outage")
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(endpointSlice), endpointSlice); err != nil {
		t.Fatalf("get ready EndpointSlice during outage: %v", err)
	}
	endpointReady := false
	endpointSlice.Endpoints[0].Conditions.Ready = &endpointReady
	if err := kubeClient.Update(context.Background(), endpointSlice); err != nil {
		t.Fatalf("make preserved endpoint unready: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("unready endpoint Reconcile(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &updated); err != nil {
		t.Fatalf("get ModelDeployment with unready endpoint: %v", err)
	}
	if meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) ||
		meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionArtifactReady) {
		t.Fatalf("unready preserved endpoint conditions = %+v", updated.Status.Conditions)
	}
	endpointReady = true
	endpointSlice.Endpoints[0].Conditions.Ready = &endpointReady
	if err := kubeClient.Update(context.Background(), endpointSlice); err != nil {
		t.Fatalf("restore preserved endpoint readiness: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("restored endpoint Reconcile(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &updated); err != nil {
		t.Fatalf("get ModelDeployment with restored endpoint: %v", err)
	}
	if !meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) ||
		meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionArtifactReady) {
		t.Fatalf("restored preserved endpoint conditions = %+v", updated.Status.Conditions)
	}
	if err := kubeClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete loaded Pod during outage: %v", err)
	}
	replacementPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "runtime-replacement", Namespace: modelDeployment.Namespace,
			Labels: replicaSet.Spec.Template.Labels, Annotations: replicaSet.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: replicaSetKind,
				Name: replicaSet.Name, UID: replicaSet.UID, Controller: &controller,
			}},
		},
		Spec: *replicaSet.Spec.Template.Spec.DeepCopy(),
	}
	if err := kubeClient.Create(context.Background(), replacementPod); err != nil {
		t.Fatalf("create scheduling-gated replacement Pod: %v", err)
	}

	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(artifact), &currentArtifact); err != nil {
		t.Fatalf("get artifact for recovery: %v", err)
	}
	meta.SetStatusCondition(&currentArtifact.Status.Conditions, metav1.Condition{
		Type: kamav1alpha1.ModelArtifactConditionReady, Status: metav1.ConditionTrue,
		ObservedGeneration: currentArtifact.Generation, Reason: "Verified", Message: "artifact is ready again",
	})
	if err := kubeClient.Status().Update(context.Background(), &currentArtifact); err != nil {
		t.Fatalf("restore artifact readiness: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("recovery Reconcile(): %v", err)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(&workload), &workload); err != nil {
		t.Fatalf("get resumed workload: %v", err)
	}
	if workload.Spec.Paused {
		t.Fatal("Deployment remained paused after artifact recovery")
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(replicaSet), replicaSet); err != nil {
		t.Fatalf("get resumed ReplicaSet: %v", err)
	}
	if !hasArtifactUnavailableSchedulingGate(replicaSet.Spec.Template.Spec.SchedulingGates) {
		t.Fatal("ReplicaSet lost its permanent artifact scheduling gate after recovery")
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(replacementPod), replacementPod); err != nil {
		t.Fatalf("get resumed replacement Pod: %v", err)
	}
	if hasArtifactUnavailableSchedulingGate(replacementPod.Spec.SchedulingGates) {
		t.Fatal("already-created replacement Pod remained scheduling-gated after artifact recovery")
	}
}

func TestModelDeploymentReportsTerminalLoadFailureEventsAndMetricsWithoutReplacingPod(t *testing.T) {
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	modelDeployment.Name = "terminal-load-failure"
	modelDeployment.Namespace = "modeldeployment-metrics"
	artifact.Namespace = modelDeployment.Namespace
	claim.Namespace = modelDeployment.Namespace
	deleteModelDeploymentMetrics(modelDeployment.Namespace, modelDeployment.Name)
	modelDeploymentLoadFailures.DeleteLabelValues(runtimeMetricStateLoadFailed)
	t.Cleanup(func() {
		deleteModelDeploymentMetrics(modelDeployment.Namespace, modelDeployment.Name)
		modelDeploymentLoadFailures.DeleteLabelValues(runtimeMetricStateLoadFailed)
	})
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	recorder := events.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}
	initialEvents := drainEvents(recorder)
	for _, want := range []string{
		"Normal ArtifactReady",
		"Warning WorkloadPending",
		"Warning RuntimeNotReady",
	} {
		if !strings.Contains(initialEvents, want) {
			t.Fatalf("initial events %q do not contain %q", initialEvents, want)
		}
	}
	var workload appsv1.Deployment
	if err := kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment),
	}, &workload); err != nil {
		t.Fatalf("get workload: %v", err)
	}
	fingerprint := workload.Spec.Template.Annotations[runtimeFingerprintAnnotation]
	controller := true
	replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "failed-runtime-rs", Namespace: modelDeployment.Namespace, UID: types.UID("failed-replicaset-uid"),
		Labels: workload.Spec.Template.Labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment",
			Name: workload.Name, UID: workload.UID, Controller: &controller,
		}},
	}}
	if err := kubeClient.Create(context.Background(), replicaSet); err != nil {
		t.Fatalf("create failed runtime ReplicaSet: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "runtime-failed", Namespace: modelDeployment.Namespace,
			Labels: workload.Spec.Template.Labels, Annotations: workload.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: replicaSetKind,
				Name: replicaSet.Name, UID: replicaSet.UID, Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{
			PodIP:      testPodIP,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: runtimeContainerName, Image: testRuntimeCPUImage,
				ImageID: "example.invalid/kama-runtime-cpu@sha256:abc", RestartCount: 0,
			}},
		},
	}
	if err := kubeClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create failed Pod: %v", err)
	}
	reconciler.HTTPClient = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"phase":"LoadFailed","reason":"ChildExited","message":"model load failed",` +
			`"ready":false,"deployment":{"uid":"deployment-uid","fingerprint":"` + fingerprint + `"},` +
			`"runtime":{"mode":"CPU","desiredConcurrency":1,"llamaCPPCommit":"unknown"}}`
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("failure Reconcile(): %v", err)
	}
	var updated kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &updated); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	if updated.Status.Runtime == nil || updated.Status.Runtime.State != kamav1alpha1.ModelDeploymentRuntimeLoadFailed ||
		updated.Status.Runtime.LlamaCommit != "" || updated.Status.Runtime.ObservedFingerprint != fingerprint ||
		updated.Status.Runtime.AcceleratorDetected == nil || *updated.Status.Runtime.AcceleratorDetected ||
		meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionRuntimeReady) ||
		meta.IsStatusConditionTrue(updated.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) {
		t.Fatalf("load failure status = %+v", updated.Status)
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(&workload), &workload); err != nil {
		t.Fatalf("controller replaced terminal failed workload: %v", err)
	}
	failureEvents := drainEvents(recorder)
	if !strings.Contains(failureEvents, "Warning ChildExited") {
		t.Fatalf("load-failure events %q do not contain Warning ChildExited", failureEvents)
	}
	assertModelDeploymentMetric(t, modelDeploymentReadyReplicas, map[string]string{
		metricNamespaceLabel: modelDeployment.Namespace, modelDeploymentMetricLabel: modelDeployment.Name,
	}, 0)
	for _, state := range []string{
		runtimeMetricStateNone,
		string(kamav1alpha1.ModelDeploymentRuntimeInitializing),
		string(kamav1alpha1.ModelDeploymentRuntimeLoading),
		string(kamav1alpha1.ModelDeploymentRuntimeReady),
		string(kamav1alpha1.ModelDeploymentRuntimeDraining),
		string(kamav1alpha1.ModelDeploymentRuntimeLoadFailed),
		string(kamav1alpha1.ModelDeploymentRuntimeExited),
	} {
		want := 0.0
		if state == string(kamav1alpha1.ModelDeploymentRuntimeLoadFailed) {
			want = 1
		}
		assertModelDeploymentMetric(t, modelDeploymentRuntimeState, map[string]string{
			metricNamespaceLabel:       modelDeployment.Namespace,
			modelDeploymentMetricLabel: modelDeployment.Name,
			"state":                    state,
		}, want)
	}
	assertModelDeploymentMetric(t, modelDeploymentLoadFailures, map[string]string{
		metricReasonLabel: runtimeMetricStateLoadFailed,
	}, 1)

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("repeat failure Reconcile(): %v", err)
	}
	if got := len(recorder.Events); got != 0 {
		t.Fatalf("repeat failure emitted %d duplicate condition events: %q", got, drainEvents(recorder))
	}
	assertModelDeploymentMetric(t, modelDeploymentLoadFailures, map[string]string{
		metricReasonLabel: runtimeMetricStateLoadFailed,
	}, 1)
}

func TestModelDeploymentIgnoresLabelSpoofedPodState(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	modelDeployment, artifact, claim := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kamav1alpha1.ModelDeployment{}, &kamav1alpha1.ModelArtifact{}).
		WithObjects(modelDeployment, artifact, claim).Build()
	reconciler := testModelDeploymentReconciler(kubeClient, scheme)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(modelDeployment)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}
	var workload appsv1.Deployment
	if err := kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: modelDeployment.Namespace, Name: servingObjectName(modelDeployment),
	}, &workload); err != nil {
		t.Fatalf("get workload: %v", err)
	}
	spoofed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "label-spoof", Namespace: modelDeployment.Namespace,
			Labels: workload.Spec.Template.Labels, Annotations: workload.Spec.Template.Annotations,
		},
		Status: corev1.PodStatus{
			PodIP: testPodIP,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if err := kubeClient.Create(context.Background(), spoofed); err != nil {
		t.Fatalf("create label-spoofed Pod: %v", err)
	}
	called := false
	reconciler.HTTPClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected request")
	})}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("spoof Reconcile(): %v", err)
	}
	var current kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	if called || current.Status.ReadyReplicas != 0 ||
		meta.IsStatusConditionTrue(current.Status.Conditions, kamav1alpha1.ModelDeploymentConditionServing) {
		t.Fatalf("label-spoofed Pod affected status: called=%v status=%+v", called, current.Status)
	}
}

func TestSupervisorStateMustMatchRuntimeBuildAndConfiguration(t *testing.T) {
	t.Parallel()
	deployment, _, _ := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	effectiveContext := int64(4096)
	state := supervisorState{
		Phase: string(kamaruntime.PhaseReady), Ready: true,
		Deployment: supervisorStateOwner{UID: deployment.UID, Fingerprint: testFingerprint},
		Runtime: supervisorStateRuntime{
			Mode:                   string(kamav1alpha1.ModelDeploymentPlacementCPU),
			EffectiveContextTokens: &effectiveContext, DesiredConcurrency: 1,
			LlamaCommit: testLlamaCommit,
		},
	}
	if !supervisorStateMatches(deployment, state, true, testFingerprint, testLlamaCommit) {
		t.Fatal("matching ready supervisor state was rejected")
	}
	state.Runtime.LlamaCommit = strings.Repeat("b", 40)
	if supervisorStateMatches(deployment, state, true, testFingerprint, testLlamaCommit) {
		t.Fatal("supervisor with a different llama.cpp commit was accepted")
	}
	state.Runtime.LlamaCommit = "unknown"
	if supervisorStateMatches(deployment, state, true, testFingerprint, testLlamaCommit) {
		t.Fatal("supervisor with a malformed llama.cpp commit was accepted")
	}

	acceleratorDeployment, _, _ := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementAccelerator)
	visibleAccelerators := int32(1)
	offloadedLayers := int32(24)
	totalLayers := int32(24)
	state = supervisorState{
		Phase: string(kamaruntime.PhaseReady), Ready: true,
		Deployment: supervisorStateOwner{UID: acceleratorDeployment.UID, Fingerprint: testFingerprint},
		Runtime: supervisorStateRuntime{
			Mode:                   string(kamav1alpha1.ModelDeploymentPlacementAccelerator),
			EffectiveContextTokens: &effectiveContext, DesiredConcurrency: 1,
			LlamaCommit: testLlamaCommit, AcceleratorDetected: true,
			VisibleAccelerators: &visibleAccelerators, OffloadedLayers: &offloadedLayers, TotalLayers: &totalLayers,
		},
	}
	if !supervisorStateMatches(acceleratorDeployment, state, true, testFingerprint, testLlamaCommit) {
		t.Fatal("matching full-offload accelerator supervisor state was rejected")
	}
	visibleAccelerators = 2
	if supervisorStateMatches(acceleratorDeployment, state, true, testFingerprint, testLlamaCommit) {
		t.Fatal("supervisor with multiple visible accelerators was accepted")
	}
	visibleAccelerators = 1
	offloadedLayers = 23
	if supervisorStateMatches(acceleratorDeployment, state, true, testFingerprint, testLlamaCommit) {
		t.Fatal("supervisor with partial accelerator offload was accepted")
	}
}

func TestArtifactDeletionWaitsForModelDeploymentReference(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	modelDeployment, artifact, _ := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(modelDeployment, artifact).Build()
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient}}
	referenced, err := reconciler.modelArtifactHasDeploymentReferences(context.Background(), artifact)
	if err != nil {
		t.Fatalf("modelArtifactHasDeploymentReferences(): %v", err)
	}
	if !referenced {
		t.Fatal("referenced = false, want true")
	}

	var current kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
		t.Fatalf("get ModelDeployment: %v", err)
	}
	current.Spec.ModelRef.Name = "replacement-model"
	if err := kubeClient.Update(context.Background(), &current); err != nil {
		t.Fatalf("update ModelDeployment reference: %v", err)
	}
	oldServingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "old-runtime", Namespace: artifact.Namespace,
		Labels: map[string]string{
			managedByLabel: kamaName, componentLabel: modelDeploymentComponent,
			artifactUIDLabel: string(artifact.UID), modelDeploymentUIDLabel: string(current.UID),
		},
	}, Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{{
			Name: runtimeModelVolumeName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: artifact.Status.Location.ClaimName, ReadOnly: true,
			}},
		}},
		Containers: []corev1.Container{{
			Name: runtimeContainerName,
			VolumeMounts: []corev1.VolumeMount{{
				Name: runtimeModelVolumeName, MountPath: runtimeModelMount,
				SubPath: artifact.Status.Location.SubPath, ReadOnly: true,
			}},
		}},
	}}
	if err := kubeClient.Create(context.Background(), oldServingPod); err != nil {
		t.Fatalf("create old serving Pod: %v", err)
	}
	referenced, err = reconciler.modelArtifactHasDeploymentReferences(context.Background(), artifact)
	if err != nil {
		t.Fatalf("modelArtifactHasDeploymentReferences() after update: %v", err)
	}
	if !referenced {
		t.Fatal("old loaded artifact was released before its serving Pod drained")
	}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
		t.Fatalf("get ModelDeployment before orphaned-Pod check: %v", err)
	}
	current.Finalizers = nil
	if err := kubeClient.Update(context.Background(), &current); err != nil {
		t.Fatalf("clear ModelDeployment finalizer for orphaned-Pod check: %v", err)
	}
	if err := kubeClient.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete ModelDeployment for orphaned-Pod check: %v", err)
	}
	referenced, err = reconciler.modelArtifactHasDeploymentReferences(context.Background(), artifact)
	if err != nil {
		t.Fatalf("modelArtifactHasDeploymentReferences() without deployment: %v", err)
	}
	if !referenced {
		t.Fatal("artifact was released while an orphaned generated Pod still mounted it")
	}
	if err := kubeClient.Delete(context.Background(), oldServingPod); err != nil {
		t.Fatalf("delete old serving Pod: %v", err)
	}
	referenced, err = reconciler.modelArtifactHasDeploymentReferences(context.Background(), artifact)
	if err != nil {
		t.Fatalf("modelArtifactHasDeploymentReferences() after drain: %v", err)
	}
	if referenced {
		t.Fatal("old artifact reference remains after its serving Pod drained")
	}
}

func TestArtifactDeletionWaitsForDeletingModelDeploymentReference(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	modelDeployment, artifact, _ := readyServingObjects(kamav1alpha1.ModelDeploymentPlacementCPU)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(modelDeployment, artifact).Build()
	if err := kubeClient.Delete(context.Background(), modelDeployment); err != nil {
		t.Fatalf("delete ModelDeployment: %v", err)
	}
	var deleting kamav1alpha1.ModelDeployment
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &deleting); err != nil {
		t.Fatalf("get deleting ModelDeployment: %v", err)
	}
	if deleting.DeletionTimestamp.IsZero() {
		t.Fatal("ModelDeployment with finalizer did not enter deletion")
	}
	reconciler := &ModelArtifactReconciler{reconcilerBase: reconcilerBase{Client: kubeClient}}
	referenced, err := reconciler.modelArtifactHasDeploymentReferences(context.Background(), artifact)
	if err != nil {
		t.Fatalf("modelArtifactHasDeploymentReferences(): %v", err)
	}
	if !referenced {
		t.Fatal("artifact was released while its deleting ModelDeployment still held the drain finalizer")
	}
}

func readyServingObjects(mode kamav1alpha1.ModelDeploymentPlacementMode) (
	*kamav1alpha1.ModelDeployment,
	*kamav1alpha1.ModelArtifact,
	*corev1.PersistentVolumeClaim,
) {
	concurrency := int32(1)
	batch := int32(2048)
	microBatch := int32(512)
	drain := metav1.Duration{Duration: 10 * time.Minute}
	modelDeployment := &kamav1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "chat", Namespace: controllerTestNamespace, UID: types.UID("deployment-uid"),
			Generation: 1, Finalizers: []string{kamav1alpha1.ModelDeploymentFinalizer},
		},
		Spec: kamav1alpha1.ModelDeploymentSpec{
			ModelRef:  corev1.LocalObjectReference{Name: testServingModelName},
			Placement: kamav1alpha1.ModelDeploymentPlacementSpec{Mode: mode},
			Runtime: kamav1alpha1.ModelDeploymentRuntimeSpec{
				DesiredConcurrency: &concurrency, DrainTimeout: &drain,
				KVCache: kamav1alpha1.ModelDeploymentKVCacheSpec{
					KeyType:   kamav1alpha1.ModelDeploymentKVCacheF16,
					ValueType: kamav1alpha1.ModelDeploymentKVCacheF16,
				},
				Expert: kamav1alpha1.ModelDeploymentExpertSpec{
					BatchSize: &batch, MicroBatchSize: &microBatch,
					FlashAttention: kamav1alpha1.ModelDeploymentFlashAttentionAuto,
				},
			},
			Resources: kamav1alpha1.ModelDeploymentResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			},
		},
	}
	if mode == kamav1alpha1.ModelDeploymentPlacementAccelerator {
		modelDeployment.Spec.Placement.AcceleratorResource = kamav1alpha1.DefaultAcceleratorResource
	}
	digest := strings.Repeat("a", 64)
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "models", Namespace: controllerTestNamespace, UID: types.UID("claim-uid"),
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	artifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name: testServingModelName, Namespace: controllerTestNamespace, UID: types.UID("artifact-uid"), Generation: 1,
		},
		Spec: kamav1alpha1.ModelArtifactSpec{Entrypoint: "model.gguf"},
		Status: kamav1alpha1.ModelArtifactStatus{
			ObservedGeneration: 1, ArtifactDigest: digest,
			Files: []kamav1alpha1.ModelArtifactFileStatus{{Path: "model.gguf", Size: 1024, SHA256: digest}},
			Location: &kamav1alpha1.ModelArtifactLocationStatus{
				ClaimName: claim.Name, ClaimUID: claim.UID, SubPath: "artifacts/model", ReadOnly: true,
				MountScope: kamav1alpha1.MountScopeMultiNode,
			},
			Conditions: []metav1.Condition{{
				Type: kamav1alpha1.ModelArtifactConditionReady, Status: metav1.ConditionTrue,
				ObservedGeneration: 1, Reason: "Verified", Message: "artifact is ready",
			}},
		},
	}
	return modelDeployment, artifact, claim
}

func markTestArtifactUnavailable(artifact *kamav1alpha1.ModelArtifact) {
	meta.SetStatusCondition(&artifact.Status.Conditions, metav1.Condition{
		Type:               kamav1alpha1.ModelArtifactConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: artifact.Generation,
		Reason:             "CacheProbeFailed",
		Message:            "artifact is temporarily unavailable",
	})
}

func testModelDeploymentReconciler(kubeClient client.Client, scheme *runtime.Scheme) *ModelDeploymentReconciler {
	return NewModelDeploymentReconciler(kubeClient, kubeClient, scheme, events.NewFakeRecorder(32), RuntimeOptions{
		CPUImage: testRuntimeCPUImage, CUDAImage: testRuntimeCUDAImage,
		PullPolicy: corev1.PullNever, LlamaCommit: testLlamaCommit,
	})
}

func assertModelDeploymentMetric(
	t *testing.T,
	collector prometheus.Collector,
	wantLabels map[string]string,
	wantValue float64,
) {
	t.Helper()
	metrics := make(chan prometheus.Metric, 32)
	collector.Collect(metrics)
	close(metrics)
	found := false
	for metric := range metrics {
		var observed dto.Metric
		if err := metric.Write(&observed); err != nil {
			t.Fatalf("write Prometheus metric: %v", err)
		}
		if !metricLabelsEqual(observed.Label, wantLabels) {
			continue
		}
		if found {
			t.Fatalf("multiple Prometheus metrics matched labels %+v", wantLabels)
		}
		found = true
		var value float64
		switch {
		case observed.Gauge != nil:
			value = observed.GetGauge().GetValue()
		case observed.Counter != nil:
			value = observed.GetCounter().GetValue()
		default:
			t.Fatalf("metric with labels %+v is not a gauge or counter", wantLabels)
		}
		if value != wantValue {
			t.Fatalf("metric with labels %+v = %v, want %v", wantLabels, value, wantValue)
		}
	}
	if !found {
		t.Fatalf("no Prometheus metric found with exact labels %+v", wantLabels)
	}
}

func metricLabelsEqual(labels []*dto.LabelPair, want map[string]string) bool {
	if len(labels) != len(want) {
		return false
	}
	for _, label := range labels {
		wantValue, found := want[label.GetName()]
		if !found || wantValue != label.GetValue() {
			return false
		}
	}
	return true
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
