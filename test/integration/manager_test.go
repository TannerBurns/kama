//go:build integration

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

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	"github.com/TannerBurns/kama/internal/artifact"
	artifactcontroller "github.com/TannerBurns/kama/internal/controller"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	testTimeout              = 30 * time.Second
	testPollInterval         = 50 * time.Millisecond
	artifactOperationLabel   = "kama.tannerburns.github.io/operation"
	artifactUIDLabel         = "kama.tannerburns.github.io/model-artifact-uid"
	componentLabel           = "app.kubernetes.io/component"
	artifactCleanupComponent = "artifact-cleanup"
	modelDeploymentUIDLabel  = "kama.tannerburns.github.io/model-deployment-uid"
	runtimeFingerprintKey    = "kama.tannerburns.github.io/runtime-fingerprint-full"
	artifactSchedulingGate   = "kama.tannerburns.github.io/artifact-ready"
)

type resultLogStore struct {
	mu      sync.RWMutex
	payload []byte
}

func (s *resultLogStore) set(t *testing.T, result artifact.Result) {
	t.Helper()
	payload, err := artifact.MarshalResultLine(result)
	if err != nil {
		t.Fatalf("marshal simulated importer result: %v", err)
	}
	s.mu.Lock()
	s.payload = append(s.payload[:0], payload...)
	s.mu.Unlock()
}

func (s *resultLogStore) get() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.payload...)
}

type integrationSuite struct {
	environment   *envtest.Environment
	config        *rest.Config
	scheme        *runtime.Scheme
	apiClient     client.Client
	managerClient client.Client
	clientset     *fake.Clientset
	logs          *resultLogStore

	managerCancel context.CancelFunc
	managerDone   chan error
	gcCancel      context.CancelFunc
	gcDone        chan struct{}
}

func TestPersistentArtifactPlane(t *testing.T) {
	suite := newIntegrationSuite(t)

	t.Run("admission defaults validates and freezes ready content", suite.testAdmission)
	t.Run("model deployment admission gates workload and repairs service drift", suite.testModelDeploymentAdmissionAndGating)
	t.Run("managed cache probes imports and retains storage", suite.testManagedCacheAndHubImport)
	t.Run("adopted and delete retention policies preserve ownership", suite.testClaimRetention)
	t.Run("direct artifact recovers across restart and reports affinity", suite.testDirectRestartAndSuccess)
	t.Run("missing results retry deterministically and failures clean up", suite.testRetryFailureAndDeletion)
}

func newIntegrationSuite(t *testing.T) *integrationSuite {
	t.Helper()
	ctrl.SetLogger(logr.Discard())
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("register Kubernetes scheme: %v", err)
	}
	if err := kamav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register Kama scheme: %v", err)
	}

	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths:            []string{filepath.Join("..", "..", "config", "webhook", "manifests.yaml")},
			LocalServingHost: "127.0.0.1",
		},
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		_ = environment.Stop()
		t.Fatalf("create envtest client: %v", err)
	}

	logs := &resultLogStore{}
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &runtime.Unknown{Raw: logs.get()}, nil
	})

	suite := &integrationSuite{
		environment: environment,
		config:      config,
		scheme:      scheme,
		apiClient:   apiClient,
		clientset:   clientset,
		logs:        logs,
	}
	suite.startEnvtestGarbageCollector()
	t.Cleanup(func() {
		suite.stopManager(t)
		suite.stopEnvtestGarbageCollector()
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	suite.startManager(t)
	return suite
}

// startEnvtestGarbageCollector supplies the one cluster controller envtest
// intentionally does not run. Production uses foreground Job deletion so an
// artifact/cache finalizer cannot disappear while an importer Pod is alive;
// this loop applies the same owner-dependent ordering in the integration API
// server before removing the foreground-deletion finalizer.
func (s *integrationSuite) startEnvtestGarbageCollector() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.gcCancel = cancel
	s.gcDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(testPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.collectForegroundJobs(ctx)
			}
		}
	}()
}

func (s *integrationSuite) collectForegroundJobs(ctx context.Context) {
	var jobs batchv1.JobList
	if err := s.apiClient.List(ctx, &jobs); err != nil {
		return
	}
	for index := range jobs.Items {
		job := &jobs.Items[index]
		if job.DeletionTimestamp.IsZero() || !slices.Contains(job.Finalizers, metav1.FinalizerDeleteDependents) {
			continue
		}
		var pods corev1.PodList
		if err := s.apiClient.List(ctx, &pods, client.InNamespace(job.Namespace)); err != nil {
			continue
		}
		dependentsRemain := false
		for podIndex := range pods.Items {
			pod := &pods.Items[podIndex]
			for _, reference := range pod.OwnerReferences {
				if reference.UID == job.UID && reference.Controller != nil && *reference.Controller {
					dependentsRemain = true
					_ = s.apiClient.Delete(ctx, pod, client.PropagationPolicy(metav1.DeletePropagationBackground))
					break
				}
			}
		}
		if dependentsRemain {
			continue
		}
		job.Finalizers = slices.DeleteFunc(job.Finalizers, func(value string) bool {
			return value == metav1.FinalizerDeleteDependents
		})
		_ = s.apiClient.Update(ctx, job)
	}
}

func (s *integrationSuite) stopEnvtestGarbageCollector() {
	if s.gcCancel == nil {
		return
	}
	s.gcCancel()
	<-s.gcDone
	s.gcCancel = nil
	s.gcDone = nil
}

func (s *integrationSuite) startManager(t *testing.T) {
	t.Helper()
	if s.managerCancel != nil {
		t.Fatal("manager is already running")
	}
	webhookOptions := s.environment.WebhookInstallOptions
	webhookServer := webhook.NewServer(webhook.Options{
		Host:    webhookOptions.LocalServingHost,
		Port:    webhookOptions.LocalServingPort,
		CertDir: webhookOptions.LocalServingCertDir,
	})
	manager, err := ctrl.NewManager(s.config, ctrl.Options{
		Scheme:                 s.scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		WebhookServer:          webhookServer,
		// A fresh manager is intentionally constructed during the restart test.
		// Only one instance is active at a time, so duplicate process-local names
		// do not produce duplicate metrics series.
		Controller: controllerconfig.Controller{SkipNameValidation: boolPointer(true)},
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	importerOptions := artifactcontroller.ImporterOptions{
		Image:       "registry.invalid/kama-importer:test",
		PullPolicy:  corev1.PullNever,
		HubEndpoint: "https://hub.invalid",
	}
	if err := artifactcontroller.NewModelCacheReconciler(
		manager.GetClient(), manager.GetScheme(), manager.GetEventRecorder("modelcache-envtest"),
		s.clientset, importerOptions,
	).SetupWithManager(manager); err != nil {
		t.Fatalf("register ModelCache controller: %v", err)
	}
	if err := artifactcontroller.NewModelArtifactReconciler(
		manager.GetClient(), manager.GetScheme(), manager.GetEventRecorder("modelartifact-envtest"),
		s.clientset, importerOptions,
	).SetupWithManager(manager); err != nil {
		t.Fatalf("register ModelArtifact controller: %v", err)
	}
	if err := artifactcontroller.NewModelDeploymentReconciler(
		manager.GetClient(), manager.GetAPIReader(), manager.GetScheme(), manager.GetEventRecorder("modeldeployment-envtest"),
		artifactcontroller.RuntimeOptions{
			CPUImage:    "registry.invalid/kama-runtime-cpu:test",
			CUDAImage:   "registry.invalid/kama-runtime-cuda:test",
			PullPolicy:  corev1.PullNever,
			LlamaCommit: "b4d6c7d8ff69c2e05e4e8ee7e6e710a08abd7b45",
		},
	).SetupWithManager(manager); err != nil {
		t.Fatalf("register ModelDeployment controller: %v", err)
	}
	if err := kamav1alpha1.SetupModelCacheWebhookWithManager(manager); err != nil {
		t.Fatalf("register ModelCache webhooks: %v", err)
	}
	if err := kamav1alpha1.SetupModelArtifactWebhookWithManager(manager); err != nil {
		t.Fatalf("register ModelArtifact webhooks: %v", err)
	}
	if err := kamav1alpha1.SetupModelDeploymentWebhookWithManager(manager); err != nil {
		t.Fatalf("register ModelDeployment webhooks: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	s.managerCancel = cancel
	s.managerDone = done
	s.managerClient = manager.GetClient()

	syncContext, syncCancel := context.WithTimeout(context.Background(), testTimeout)
	defer syncCancel()
	if !manager.GetCache().WaitForCacheSync(syncContext) {
		s.stopManager(t)
		t.Fatal("manager cache did not synchronize")
	}
	eventually(t, "webhook server to listen", func() (bool, error) {
		connection, dialErr := net.DialTimeout("tcp", net.JoinHostPort(
			webhookOptions.LocalServingHost, fmt.Sprintf("%d", webhookOptions.LocalServingPort),
		), 200*time.Millisecond)
		if dialErr != nil {
			select {
			case startErr := <-done:
				return false, fmt.Errorf("manager exited before webhook readiness: %w", startErr)
			default:
				return false, nil
			}
		}
		_ = connection.Close()
		return true, nil
	})
}

func (s *integrationSuite) stopManager(t *testing.T) {
	t.Helper()
	if s.managerCancel == nil {
		return
	}
	s.managerCancel()
	select {
	case err := <-s.managerDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("manager stopped with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("manager did not stop within 10 seconds")
	}
	s.managerCancel = nil
	s.managerDone = nil
	s.managerClient = nil
}

func (s *integrationSuite) testAdmission(t *testing.T) {
	namespace := s.createNamespace(t, "admission")
	cache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: "admission-cache", Namespace: namespace},
		Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
			ExistingClaim: &corev1.LocalObjectReference{Name: "adopted"},
		}},
	}
	if err := s.apiClient.Create(context.Background(), cache); err != nil {
		t.Fatalf("create defaulted cache: %v", err)
	}
	if cache.Spec.RetentionPolicy != kamav1alpha1.RetentionPolicyRetain {
		t.Fatalf("retentionPolicy = %q, want Retain", cache.Spec.RetentionPolicy)
	}

	invalidCache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-cache", Namespace: namespace},
		Spec: kamav1alpha1.ModelCacheSpec{
			RetentionPolicy: kamav1alpha1.RetentionPolicyDelete,
			Storage: kamav1alpha1.ModelCacheStorageSpec{
				ExistingClaim: &corev1.LocalObjectReference{Name: "adopted"},
			},
		},
	}
	if err := s.apiClient.Create(context.Background(), invalidCache); err == nil {
		t.Fatal("adopted cache with Delete retention unexpectedly passed admission")
	}

	digest := strings.Repeat("AB", 32)
	modelArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulted-copy", Namespace: namespace},
		Spec: kamav1alpha1.ModelArtifactSpec{
			Format:     kamav1alpha1.ArtifactFormatGGUF,
			Entrypoint: "models/model.gguf",
			Source: kamav1alpha1.ModelArtifactSource{PersistentVolumeClaim: &kamav1alpha1.PersistentVolumeClaimSource{
				ClaimName: "source", RootPath: ".",
			}},
			CacheRef:     &corev1.LocalObjectReference{Name: cache.Name},
			Verification: kamav1alpha1.ModelArtifactVerificationSpec{ExpectedSHA256: digest},
		},
	}
	if err := s.apiClient.Create(context.Background(), modelArtifact); err != nil {
		t.Fatalf("create defaulted artifact: %v", err)
	}
	if modelArtifact.Spec.Source.PersistentVolumeClaim.ImportPolicy != kamav1alpha1.PVCImportPolicyCopy {
		t.Fatalf("importPolicy = %q, want Copy", modelArtifact.Spec.Source.PersistentVolumeClaim.ImportPolicy)
	}
	if modelArtifact.Spec.Verification.ExpectedSHA256 != strings.ToLower(digest) {
		t.Fatalf("expectedSHA256 was not canonicalized: %q", modelArtifact.Spec.Verification.ExpectedSHA256)
	}

	invalidPath := modelArtifact.DeepCopy()
	invalidPath.ResourceVersion = ""
	invalidPath.UID = ""
	invalidPath.Name = "invalid-path"
	invalidPath.Spec.Entrypoint = "../escape.gguf"
	if err := s.apiClient.Create(context.Background(), invalidPath); err == nil {
		t.Fatal("path traversal unexpectedly passed admission")
	}

	invalidDirect := modelArtifact.DeepCopy()
	invalidDirect.ResourceVersion = ""
	invalidDirect.UID = ""
	invalidDirect.Name = "invalid-direct"
	invalidDirect.Spec.Source.PersistentVolumeClaim.ImportPolicy = kamav1alpha1.PVCImportPolicyDirect
	if err := s.apiClient.Create(context.Background(), invalidDirect); err == nil {
		t.Fatal("Direct artifact with cacheRef unexpectedly passed admission")
	}

	eventually(t, "write a synthetic Ready status", func() (bool, error) {
		var current kamav1alpha1.ModelArtifact
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelArtifact), &current); err != nil {
			return false, err
		}
		current.Status.ObservedGeneration = current.Generation
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               kamav1alpha1.ModelArtifactConditionReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: current.Generation,
			Reason:             "EnvtestVerified",
			Message:            "synthetic admission immutability state",
		})
		if err := s.apiClient.Status().Update(context.Background(), &current); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		*modelArtifact = current
		return true, nil
	})
	mutated := modelArtifact.DeepCopy()
	mutated.Spec.Entrypoint = "models/replaced.gguf"
	if err := s.apiClient.Update(context.Background(), mutated); err == nil {
		t.Fatal("Ready artifact content mutation unexpectedly passed admission")
	}
}

func (s *integrationSuite) testModelDeploymentAdmissionAndGating(t *testing.T) {
	namespace := s.createNamespace(t, "serving")
	deployment := &kamav1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-artifact", Namespace: namespace},
		Spec: kamav1alpha1.ModelDeploymentSpec{
			ModelRef: corev1.LocalObjectReference{Name: "not-created"},
			Placement: kamav1alpha1.ModelDeploymentPlacementSpec{
				Mode: kamav1alpha1.ModelDeploymentPlacementCPU,
			},
			Resources: kamav1alpha1.ModelDeploymentResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			},
		},
	}
	if err := s.apiClient.Create(context.Background(), deployment); err != nil {
		t.Fatalf("create ModelDeployment: %v", err)
	}
	if deployment.Spec.Runtime.DesiredConcurrency == nil || *deployment.Spec.Runtime.DesiredConcurrency != 1 ||
		deployment.Spec.Runtime.DrainTimeout == nil ||
		deployment.Spec.Runtime.DrainTimeout.Duration != kamav1alpha1.DefaultModelDeploymentDrainTimeout ||
		deployment.Spec.Runtime.KVCache.KeyType != kamav1alpha1.ModelDeploymentKVCacheF16 ||
		deployment.Spec.Runtime.Expert.BatchSize == nil ||
		*deployment.Spec.Runtime.Expert.BatchSize != kamav1alpha1.DefaultModelDeploymentBatchSize {
		t.Fatalf("ModelDeployment admission defaults = %+v", deployment.Spec.Runtime)
	}

	invalid := deployment.DeepCopy()
	invalid.ResourceVersion = ""
	invalid.UID = ""
	invalid.Name = "user-owned-gpu"
	invalid.Spec.Resources.Requests[kamav1alpha1.DefaultAcceleratorResource] = resource.MustParse("1")
	if err := s.apiClient.Create(context.Background(), invalid); err == nil {
		t.Fatal("user-supplied accelerator resource unexpectedly passed admission")
	}

	protectedFields := []struct {
		location string
		name     string
	}{
		{"spec", "args"}, {"spec", "env"}, {"spec", "image"}, {"spec", "ports"},
		{"spec", "paths"}, {"spec", "probes"}, {"spec", "topology"}, {"spec", "replicas"},
		{"runtime", "args"}, {"runtime", "env"}, {"runtime", "image"}, {"runtime", "ports"},
		{"runtime", "paths"}, {"runtime", "probes"}, {"runtime", "topology"}, {"runtime", "replicas"},
	}
	for _, validationMode := range []string{metav1.FieldValidationWarn, metav1.FieldValidationIgnore} {
		for index, protected := range protectedFields {
			name := fmt.Sprintf("forbidden-%s-%s-%d", protected.location, protected.name, index)
			spec := map[string]any{
				"modelRef":  map[string]any{"name": "not-created"},
				"placement": map[string]any{"mode": "CPU"},
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "1", "memory": "1Gi"},
					"limits":   map[string]any{"memory": "2Gi"},
				},
			}
			if protected.location == "runtime" {
				spec["runtime"] = map[string]any{protected.name: map[string]any{}}
			} else {
				spec[protected.name] = map[string]any{}
			}
			object := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": kamav1alpha1.GroupVersion.String(),
				"kind":       "ModelDeployment",
				"metadata":   map[string]any{"name": name, "namespace": namespace},
				"spec":       spec,
			}}
			err := s.apiClient.Create(context.Background(), object, client.FieldValidation(validationMode))
			if err == nil {
				t.Fatalf("%s.%s passed with fieldValidation=%s", protected.location, protected.name, validationMode)
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("%s.%s error with fieldValidation=%s = %v, want Invalid",
					protected.location, protected.name, validationMode, err)
			}
		}
	}

	var serviceName string
	eventually(t, "gate serving workload on the missing artifact", func() (bool, error) {
		var current kamav1alpha1.ModelDeployment
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), &current); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(current.Status.Conditions,
			kamav1alpha1.ModelDeploymentConditionArtifactReady)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ArtifactNotFound" ||
			current.Status.ServiceRef == nil {
			return false, fmt.Errorf("current ModelDeployment status is %+v", current.Status)
		}
		serviceName = current.Status.ServiceRef.Name
		var workloads appsv1.DeploymentList
		if err := s.apiClient.List(context.Background(), &workloads, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		return len(workloads.Items) == 0, nil
	})

	var service corev1.Service
	serviceKey := types.NamespacedName{Namespace: namespace, Name: serviceName}
	if err := s.apiClient.Get(context.Background(), serviceKey, &service); err != nil {
		t.Fatalf("get stable serving Service: %v", err)
	}
	service.Spec.Ports[0].Port = 9090
	service.Spec.ExternalIPs = []string{"192.0.2.1"}
	if err := s.apiClient.Update(context.Background(), &service); err != nil {
		t.Fatalf("inject serving Service drift: %v", err)
	}
	eventually(t, "repair serving Service drift", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), serviceKey, &service); err != nil {
			return false, err
		}
		return service.Spec.Ports[0].Port == 8080 && len(service.Spec.ExternalIPs) == 0, nil
	})
}

func (s *integrationSuite) testManagedCacheAndHubImport(t *testing.T) {
	namespace := s.createNamespace(t, "managed")
	storageClass := "manual"
	cache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: "managed-cache", Namespace: namespace},
		Spec: kamav1alpha1.ModelCacheSpec{
			RetentionPolicy: kamav1alpha1.RetentionPolicyRetain,
			Storage: kamav1alpha1.ModelCacheStorageSpec{ClaimTemplate: &kamav1alpha1.ModelCacheClaimTemplate{
				Metadata: kamav1alpha1.ModelCacheClaimTemplateMetadata{Labels: map[string]string{"purpose": "envtest"}},
				Spec: kamav1alpha1.ModelCacheClaimTemplateSpec{
					StorageClassName: &storageClass,
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: kamav1alpha1.ModelCacheResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("2Gi"),
					}},
				},
			}},
		},
	}
	if err := s.apiClient.Create(context.Background(), cache); err != nil {
		t.Fatalf("create managed cache: %v", err)
	}
	claim := s.waitForManagedClaim(t, namespace, cache.Name)
	if claim.OwnerReferences != nil {
		t.Fatalf("managed retained claim has ownerReferences: %#v", claim.OwnerReferences)
	}
	if claim.Labels["purpose"] != "envtest" || claim.Labels["kama.tannerburns.github.io/model-cache-uid"] != string(cache.UID) {
		t.Fatalf("managed claim identity metadata missing: labels=%v annotations=%v", claim.Labels, claim.Annotations)
	}
	pv := s.bindClaim(t, claim, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, "node-a")

	probeJob := s.waitForOwnedJob(t, cache.UID, namespace)
	probeConfig := s.waitForOwnedConfigMap(t, cache.UID, namespace)
	var probeSpec artifact.Spec
	if err := json.Unmarshal([]byte(probeConfig.Data["spec.json"]), &probeSpec); err != nil {
		t.Fatalf("decode probe spec: %v", err)
	}
	if probeSpec.Mode != artifact.ModeProbe || probeSpec.Probe == nil || probeSpec.Probe.Root != "/cache" {
		t.Fatalf("unexpected probe spec: %#v", probeSpec)
	}
	assertImporterJobSecurity(t, probeJob, true, false, false)
	eventually(t, "managed cache to observe running probe", func() (bool, error) {
		if err := s.managerClient.Get(context.Background(), client.ObjectKeyFromObject(cache), cache); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(
			cache.Status.Conditions, kamav1alpha1.ModelCacheConditionReady,
		)
		if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "ProbeRunning" {
			return true, nil
		}
		return false, fmt.Errorf("current ModelCache status is %+v", cache.Status)
	})
	s.completeJob(t, probeJob, artifact.Result{
		SchemaVersion: artifact.SchemaVersion,
		Mode:          artifact.ModeProbe,
		OperationID:   probeSpec.OperationID,
		Success:       true,
		Probe: &artifact.ProbeResult{
			CapacityBytes:   2 << 30,
			FreeBytes:       1536 << 20,
			Write:           true,
			Fsync:           true,
			AtomicRename:    true,
			DirectoryRename: true,
			Mmap:            true,
			Lock:            true,
		},
	})
	eventually(t, "managed cache Ready status", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(cache), cache); err != nil {
			return false, err
		}
		if meta.IsStatusConditionTrue(cache.Status.Conditions, kamav1alpha1.ModelCacheConditionReady) {
			return true, nil
		}
		return false, fmt.Errorf("current ModelCache status is %+v", cache.Status)
	})
	if cache.Status.ClaimName != claim.Name || cache.Status.VolumeName != pv.Name || cache.Status.VolumeUID != pv.UID {
		t.Fatalf("cache volume identity = %#v, want claim %s and PV %s/%s", cache.Status, claim.Name, pv.Name, pv.UID)
	}
	if cache.Status.MountScope != kamav1alpha1.MountScopeSingleNode || cache.Status.NodeAffinity == nil {
		t.Fatalf("cache placement = scope %q affinity %#v", cache.Status.MountScope, cache.Status.NodeAffinity)
	}
	if cache.Status.LastProbeTime == nil || cache.Status.FreeSpace == nil || cache.Status.FreeSpace.Value() != 1536<<20 {
		t.Fatalf("cache probe status incomplete: %#v", cache.Status)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hub-token", Namespace: namespace},
		StringData: map[string]string{"token": "private-test-token"},
	}
	if err := s.apiClient.Create(context.Background(), secret); err != nil {
		t.Fatalf("create Hub token Secret: %v", err)
	}
	hubArtifact := &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: "hub-artifact", Namespace: namespace},
		Spec: kamav1alpha1.ModelArtifactSpec{
			Format:     kamav1alpha1.ArtifactFormatGGUF,
			Entrypoint: "model.gguf",
			Source: kamav1alpha1.ModelArtifactSource{HuggingFace: &kamav1alpha1.HuggingFaceSource{
				Repository: "owner/model",
				Revision:   "release-v1",
				Files:      []string{"model.gguf"},
				TokenSecretRef: &kamav1alpha1.SecretKeyReference{
					Name: secret.Name, Key: "token",
				},
			}},
			CacheRef: &corev1.LocalObjectReference{Name: cache.Name},
		},
	}
	if err := s.apiClient.Create(context.Background(), hubArtifact); err != nil {
		t.Fatalf("create Hub artifact: %v", err)
	}
	hubJob := s.waitForOwnedJob(t, hubArtifact.UID, namespace)
	hubConfig := s.waitForOwnedConfigMap(t, hubArtifact.UID, namespace)
	var hubSpec artifact.Spec
	if err := json.Unmarshal([]byte(hubConfig.Data["spec.json"]), &hubSpec); err != nil {
		t.Fatalf("decode Hub importer spec: %v", err)
	}
	if hubSpec.Mode != artifact.ModeHub || hubSpec.HubEndpoint != "https://hub.invalid" || hubSpec.Hub == nil || hubSpec.Hub.TokenFile != "/var/run/secrets/kama/token" {
		t.Fatalf("unexpected Hub importer spec: %#v", hubSpec)
	}
	if strings.Contains(hubConfig.Data["spec.json"], "private-test-token") {
		t.Fatal("importer ConfigMap contains Secret token data")
	}
	assertImporterJobSecurity(t, hubJob, true, false, true)
	digest := strings.Repeat("a", 64)
	hubResult := successfulArtifactResult(
		artifact.ModeHub, digest, "0123456789abcdef0123456789abcdef01234567", "model.gguf", hubSpec.OperationID,
	)
	s.completeJob(t, hubJob, hubResult)
	eventually(t, "Hub artifact Ready status", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(hubArtifact), hubArtifact); err != nil {
			return false, err
		}
		if meta.IsStatusConditionTrue(hubArtifact.Status.Conditions, kamav1alpha1.ModelArtifactConditionReady) {
			return true, nil
		}
		var currentJob batchv1.Job
		jobErr := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(hubJob), &currentJob)
		return false, fmt.Errorf("current conditions: %#v; current Job status: %#v; Job get: %v",
			hubArtifact.Status.Conditions, currentJob.Status, jobErr)
	})
	if hubArtifact.Status.ResolvedRevision != "0123456789abcdef0123456789abcdef01234567" ||
		hubArtifact.Status.Location == nil || hubArtifact.Status.Location.ClaimName != claim.Name ||
		hubArtifact.Status.Location.SubPath != hubResult.PublishedPath {
		t.Fatalf("unexpected Hub artifact status: %#v", hubArtifact.Status)
	}
	if err := s.apiClient.Delete(context.Background(), hubArtifact); err != nil {
		t.Fatalf("delete Hub artifact: %v", err)
	}
	cleanupJob := s.waitForArtifactCleanupJob(t, hubArtifact.UID, namespace)
	cleanupConfig := s.waitForArtifactCleanupConfig(t, hubArtifact.UID, namespace)
	var cleanupSpec artifact.Spec
	if err := json.Unmarshal([]byte(cleanupConfig.Data["spec.json"]), &cleanupSpec); err != nil {
		t.Fatalf("decode cleanup importer spec: %v", err)
	}
	if cleanupSpec.Mode != artifact.ModeCleanup || cleanupSpec.OperationID == "" || cleanupSpec.Cleanup == nil ||
		cleanupSpec.Cleanup.OperationPrefix != string(hubArtifact.UID)+"-" {
		t.Fatalf("unexpected cleanup importer spec: %#v", cleanupSpec)
	}
	if len(cleanupJob.OwnerReferences) != 0 || len(cleanupConfig.OwnerReferences) != 0 {
		t.Fatalf("cleanup resources must be detached during deletion: job=%v config=%v",
			cleanupJob.OwnerReferences, cleanupConfig.OwnerReferences)
	}
	assertImporterJobSecurity(t, cleanupJob, true, false, false)
	s.completeJob(t, cleanupJob, artifact.Result{
		SchemaVersion: artifact.SchemaVersion,
		Mode:          artifact.ModeCleanup,
		OperationID:   cleanupSpec.OperationID,
		Success:       true,
	})
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(hubArtifact), &kamav1alpha1.ModelArtifact{})

	if err := s.apiClient.Delete(context.Background(), cache); err != nil {
		t.Fatalf("delete retained managed cache: %v", err)
	}
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(cache), &kamav1alpha1.ModelCache{})
	var retained corev1.PersistentVolumeClaim
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &retained); err != nil {
		t.Fatalf("retained managed claim was removed: %v", err)
	}
}

func (s *integrationSuite) testClaimRetention(t *testing.T) {
	namespace := s.createNamespace(t, "retention")
	adopted := s.createBoundClaim(t, namespace, "adopted", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, "")
	cache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: "adopted-cache", Namespace: namespace},
		Spec: kamav1alpha1.ModelCacheSpec{Storage: kamav1alpha1.ModelCacheStorageSpec{
			ExistingClaim: &corev1.LocalObjectReference{Name: adopted.Name},
		}},
	}
	if err := s.apiClient.Create(context.Background(), cache); err != nil {
		t.Fatalf("create adopted cache: %v", err)
	}
	eventually(t, "adopted cache finalizer", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(cache), cache); err != nil {
			return false, err
		}
		return containsString(cache.Finalizers, kamav1alpha1.ModelCacheFinalizer), nil
	})
	if err := s.apiClient.Delete(context.Background(), cache); err != nil {
		t.Fatalf("delete adopted cache: %v", err)
	}
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(cache), &kamav1alpha1.ModelCache{})
	var preserved corev1.PersistentVolumeClaim
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(adopted), &preserved); err != nil {
		t.Fatalf("adopted claim was removed: %v", err)
	}

	storageClass := "manual"
	deleteCache := &kamav1alpha1.ModelCache{
		ObjectMeta: metav1.ObjectMeta{Name: "delete-cache", Namespace: namespace},
		Spec: kamav1alpha1.ModelCacheSpec{
			RetentionPolicy: kamav1alpha1.RetentionPolicyDelete,
			Storage: kamav1alpha1.ModelCacheStorageSpec{ClaimTemplate: &kamav1alpha1.ModelCacheClaimTemplate{Spec: kamav1alpha1.ModelCacheClaimTemplateSpec{
				StorageClassName: &storageClass,
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: kamav1alpha1.ModelCacheResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				}},
			}}},
		},
	}
	if err := s.apiClient.Create(context.Background(), deleteCache); err != nil {
		t.Fatalf("create Delete cache: %v", err)
	}
	deleteClaim := s.waitForManagedClaim(t, namespace, deleteCache.Name)
	if err := s.apiClient.Delete(context.Background(), deleteCache); err != nil {
		t.Fatalf("delete Delete-policy cache: %v", err)
	}
	eventually(t, "managed claim deletion request", func() (bool, error) {
		var current corev1.PersistentVolumeClaim
		err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(deleteClaim), &current)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if current.DeletionTimestamp == nil {
			return false, nil
		}
		if len(current.Finalizers) != 0 {
			current.Finalizers = nil // simulate the PVC protection controller absent from envtest.
			if updateErr := s.apiClient.Update(context.Background(), &current); updateErr != nil && !apierrors.IsConflict(updateErr) {
				return false, updateErr
			}
		}
		return true, nil
	})
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(deleteClaim), &corev1.PersistentVolumeClaim{})
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(deleteCache), &kamav1alpha1.ModelCache{})
}

func (s *integrationSuite) testDirectRestartAndSuccess(t *testing.T) {
	namespace := s.createNamespace(t, "restart")
	claim := s.createBoundClaim(t, namespace, "direct-source", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, "node-b")
	modelArtifact := directArtifact(namespace, "restart-direct", claim.Name, "models/model.gguf")
	if err := s.apiClient.Create(context.Background(), modelArtifact); err != nil {
		t.Fatalf("create Direct artifact: %v", err)
	}
	job := s.waitForOwnedJob(t, modelArtifact.UID, namespace)
	configMap := s.waitForOwnedConfigMap(t, modelArtifact.UID, namespace)
	lease := s.waitForHeldLease(t, modelArtifact.UID, namespace)
	var importerSpec artifact.Spec
	if err := json.Unmarshal([]byte(configMap.Data["spec.json"]), &importerSpec); err != nil {
		t.Fatalf("decode Direct importer spec: %v", err)
	}
	if importerSpec.Mode != artifact.ModeDirect || importerSpec.CacheRoot != "" || importerSpec.PVC == nil || importerSpec.PVC.RootPath != "." {
		t.Fatalf("unexpected Direct importer spec: %#v", importerSpec)
	}
	assertImporterJobSecurity(t, job, false, true, false)
	eventually(t, "artifact Importing transition", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelArtifact), modelArtifact); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(modelArtifact.Status.Conditions, kamav1alpha1.ModelArtifactConditionImporting), nil
	})

	jobName, configName, leaseName := job.Name, configMap.Name, lease.Name
	s.stopManager(t)
	s.startManager(t)
	eventually(t, "deterministic resources after manager restart", func() (bool, error) {
		jobs, err := s.ownedJobs(modelArtifact.UID, namespace)
		if err != nil {
			return false, err
		}
		configs, err := s.ownedConfigMaps(modelArtifact.UID, namespace)
		if err != nil {
			return false, err
		}
		var currentLease coordinationv1.Lease
		if err := s.apiClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: leaseName}, &currentLease); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return len(jobs) == 1 && jobs[0].Name == jobName && len(configs) == 1 && configs[0].Name == configName, nil
	})
	if err := s.apiClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: jobName}, job); err != nil {
		t.Fatalf("get restarted deterministic Job: %v", err)
	}
	digest := strings.Repeat("b", 64)
	s.completeJob(t, job, successfulArtifactResult(
		artifact.ModeDirect, digest, "", "models/model.gguf", importerSpec.OperationID,
	))
	eventually(t, "Direct artifact Ready status", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelArtifact), modelArtifact); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(modelArtifact.Status.Conditions, kamav1alpha1.ModelArtifactConditionReady), nil
	})
	location := modelArtifact.Status.Location
	if location == nil || location.ClaimName != claim.Name || location.SubPath != "." || !location.ReadOnly ||
		location.MountScope != kamav1alpha1.MountScopeSingleNode || location.VolumeName == "" || location.NodeAffinity == nil {
		t.Fatalf("unexpected Direct serving location: %#v", location)
	}
	if modelArtifact.Status.ArtifactDigest != digest || len(modelArtifact.Status.Files) != 1 ||
		modelArtifact.Status.Architecture != "llama" || modelArtifact.Status.ValidatedAt == nil {
		t.Fatalf("unexpected Direct verification status: %#v", modelArtifact.Status)
	}
	if condition := meta.FindStatusCondition(modelArtifact.Status.Conditions, kamav1alpha1.ModelArtifactConditionMissingShard); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("MissingShard success condition missing: %#v", condition)
	}
	eventually(t, "retained successful recovery record", func() (bool, error) {
		jobs, err := s.ownedJobs(modelArtifact.UID, namespace)
		if err != nil {
			return false, err
		}
		configs, err := s.ownedConfigMaps(modelArtifact.UID, namespace)
		if err != nil {
			return false, err
		}
		var currentLease coordinationv1.Lease
		leaseErr := s.apiClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: leaseName}, &currentLease)
		if len(jobs) == 1 && jobs[0].Name == jobName && len(configs) == 1 && configs[0].Name == configName &&
			apierrors.IsNotFound(leaseErr) {
			return true, nil
		}
		holder := ""
		if currentLease.Spec.HolderIdentity != nil {
			holder = *currentLease.Spec.HolderIdentity
		}
		return false, fmt.Errorf("artifactUID=%s jobs=%d configs=%d lease=%s holder=%s labels=%v get=%v",
			modelArtifact.UID, len(jobs), len(configs), currentLease.Name, holder, currentLease.Labels, leaseErr)
	})
	s.exerciseModelDeploymentLifecycle(t, modelArtifact)
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(modelArtifact), &kamav1alpha1.ModelArtifact{})
	eventually(t, "successful recovery record deletion", func() (bool, error) {
		jobs, err := s.ownedJobs(modelArtifact.UID, namespace)
		if err != nil {
			return false, err
		}
		configs, err := s.ownedConfigMaps(modelArtifact.UID, namespace)
		if err != nil {
			return false, err
		}
		return len(jobs) == 0 && len(configs) == 0, nil
	})
	var retained corev1.PersistentVolumeClaim
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &retained); err != nil {
		t.Fatalf("Direct source claim was not retained: %v", err)
	}
}

//nolint:gocyclo // This exercises one complete real-API serving and finalizer lifecycle.
func (s *integrationSuite) exerciseModelDeploymentLifecycle(
	t *testing.T,
	modelArtifact *kamav1alpha1.ModelArtifact,
) {
	t.Helper()
	namespace := modelArtifact.Namespace
	modelDeployment := &kamav1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "envtest-serving", Namespace: namespace},
		Spec: kamav1alpha1.ModelDeploymentSpec{
			ModelRef: corev1.LocalObjectReference{Name: modelArtifact.Name},
			Placement: kamav1alpha1.ModelDeploymentPlacementSpec{
				Mode: kamav1alpha1.ModelDeploymentPlacementCPU,
			},
			Resources: kamav1alpha1.ModelDeploymentResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			},
		},
	}
	if err := s.apiClient.Create(context.Background(), modelDeployment); err != nil {
		t.Fatalf("create ready-artifact ModelDeployment: %v", err)
	}

	var workload appsv1.Deployment
	eventually(t, "create gated serving workload for ready artifact", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), modelDeployment); err != nil {
			return false, err
		}
		if modelDeployment.Status.DeploymentRef == nil {
			return false, nil
		}
		key := client.ObjectKey{Namespace: namespace, Name: modelDeployment.Status.DeploymentRef.Name}
		if err := s.apiClient.Get(context.Background(), key, &workload); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return workload.Spec.Replicas != nil && *workload.Spec.Replicas == 1 &&
			workload.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType, nil
	})
	if workload.Spec.Template.Spec.SecurityContext == nil ||
		workload.Spec.Template.Spec.SecurityContext.RunAsUser == nil ||
		*workload.Spec.Template.Spec.SecurityContext.RunAsUser != 65532 ||
		!containsSchedulingGate(workload.Spec.Template.Spec.SchedulingGates, artifactSchedulingGate) ||
		workload.Spec.Template.Spec.Affinity == nil ||
		workload.Spec.Template.Spec.Affinity.NodeAffinity == nil ||
		len(workload.Spec.Template.Spec.Containers) != 1 ||
		workload.Spec.Template.Spec.Containers[0].SecurityContext == nil ||
		workload.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*workload.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("generated serving workload contract = %+v", workload.Spec.Template.Spec)
	}

	var configs corev1.ConfigMapList
	if err := s.apiClient.List(context.Background(), &configs, client.InNamespace(namespace),
		client.MatchingLabels{modelDeploymentUIDLabel: string(modelDeployment.UID)}); err != nil {
		t.Fatalf("list runtime ConfigMaps: %v", err)
	}
	if len(configs.Items) != 1 || configs.Items[0].Immutable == nil || !*configs.Items[0].Immutable {
		t.Fatalf("runtime ConfigMaps = %+v, want one immutable input", configs.Items)
	}

	controller := true
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "envtest-serving-rs", Namespace: namespace,
			Labels: workload.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment",
				Name: workload.Name, UID: workload.UID, Controller: &controller,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: workload.Spec.Selector.DeepCopy(), Template: *workload.Spec.Template.DeepCopy(),
		},
	}
	if err := s.apiClient.Create(context.Background(), replicaSet); err != nil {
		t.Fatalf("create serving ReplicaSet: %v", err)
	}
	servingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "envtest-serving-pod", Namespace: namespace,
			Labels: workload.Spec.Template.Labels, Annotations: workload.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "ReplicaSet",
				Name: replicaSet.Name, UID: replicaSet.UID, Controller: &controller,
			}},
		},
		Spec: *workload.Spec.Template.Spec.DeepCopy(),
	}
	if err := s.apiClient.Create(context.Background(), servingPod); err != nil {
		t.Fatalf("create scheduling-gated serving Pod: %v", err)
	}
	eventually(t, "release only the current ready artifact Pod", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(servingPod), servingPod); err != nil {
			return false, err
		}
		return !containsSchedulingGate(servingPod.Spec.SchedulingGates, artifactSchedulingGate), nil
	})
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(replicaSet), replicaSet); err != nil {
		t.Fatalf("get serving ReplicaSet: %v", err)
	}
	if !containsSchedulingGate(replicaSet.Spec.Template.Spec.SchedulingGates, artifactSchedulingGate) {
		t.Fatal("controller removed the permanent gate from the ReplicaSet template")
	}

	oldFingerprint := workload.Spec.Template.Annotations[runtimeFingerprintKey]
	if oldFingerprint == "" {
		t.Fatal("generated workload has no runtime fingerprint")
	}
	modelDeploymentUID := modelDeployment.UID
	listener, err := net.Listen("tcp", "127.0.0.9:8081")
	if err != nil {
		t.Fatalf("listen for synthetic supervisor state: %v", err)
	}
	runtimeServer := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/state" {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"phase": "Ready", "reason": "RuntimeReady", "message": "runtime is ready", "ready": true,
				"deployment": map[string]any{
					"uid": modelDeploymentUID, "fingerprint": oldFingerprint,
				},
				"runtime": map[string]any{
					"mode": "CPU", "effectiveContextTokens": 4096, "desiredConcurrency": 1,
					"llamaCPPCommit": "b4d6c7d8ff69c2e05e4e8ee7e6e710a08abd7b45",
				},
			})
		}),
	}
	go func() { _ = runtimeServer.Serve(listener) }()
	defer func() { _ = runtimeServer.Close() }()
	servingPod.Status.PodIP = "127.0.0.9"
	servingPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	servingPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runtime", Image: workload.Spec.Template.Spec.Containers[0].Image,
		ImageID: "registry.invalid/kama-runtime-cpu@sha256:" + strings.Repeat("c", 64),
	}}
	if err := s.apiClient.Status().Update(context.Background(), servingPod); err != nil {
		t.Fatalf("mark serving Pod ready: %v", err)
	}
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), modelDeployment); err != nil {
		t.Fatalf("get ModelDeployment before EndpointSlice: %v", err)
	}
	if modelDeployment.Status.ServiceRef == nil {
		t.Fatal("ModelDeployment has no stable Service reference")
	}
	ready := true
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "envtest-serving-endpoints", Namespace: namespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: modelDeployment.Status.ServiceRef.Name},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"192.0.2.10"},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef: &corev1.ObjectReference{
				APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Pod", Namespace: namespace,
				Name: servingPod.Name, UID: servingPod.UID,
			},
		}},
	}
	if err := s.apiClient.Create(context.Background(), endpointSlice); err != nil {
		t.Fatalf("create ready serving EndpointSlice: %v", err)
	}
	eventually(t, "publish current fingerprint only through a ready endpoint", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), modelDeployment); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(modelDeployment.Status.Conditions,
			kamav1alpha1.ModelDeploymentConditionRuntimeReady) &&
			meta.IsStatusConditionTrue(modelDeployment.Status.Conditions,
				kamav1alpha1.ModelDeploymentConditionServing) &&
			modelDeployment.Status.Runtime != nil &&
			modelDeployment.Status.Runtime.LoadedFingerprint == oldFingerprint, nil
	})
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	_ = runtimeServer.Shutdown(shutdownContext)
	shutdownCancel()
	eventually(t, "update mutable ModelDeployment", func() (bool, error) {
		var current kamav1alpha1.ModelDeployment
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &current); err != nil {
			return false, err
		}
		current.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("3Gi")
		if err := s.apiClient.Update(context.Background(), &current); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		*modelDeployment = current
		return true, nil
	})
	eventually(t, "apply drain-first fingerprinted Recreate update", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(&workload), &workload); err != nil {
			return false, err
		}
		fingerprint := workload.Spec.Template.Annotations[runtimeFingerprintKey]
		return fingerprint != "" && fingerprint != oldFingerprint &&
			workload.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType &&
			containsSchedulingGate(workload.Spec.Template.Spec.SchedulingGates, artifactSchedulingGate), nil
	})
	eventually(t, "retain old and new immutable runtime configs while old Pod drains", func() (bool, error) {
		configs = corev1.ConfigMapList{}
		if err := s.apiClient.List(context.Background(), &configs, client.InNamespace(namespace),
			client.MatchingLabels{modelDeploymentUIDLabel: string(modelDeployment.UID)}); err != nil {
			return false, err
		}
		return len(configs.Items) == 2, nil
	})

	if err := s.apiClient.Delete(context.Background(), modelArtifact); err != nil {
		t.Fatalf("delete referenced ModelArtifact: %v", err)
	}
	if err := s.apiClient.Delete(context.Background(), modelDeployment); err != nil {
		t.Fatalf("delete referring ModelDeployment: %v", err)
	}
	eventually(t, "hold artifact through ModelDeployment drain finalization", func() (bool, error) {
		var deletingArtifact kamav1alpha1.ModelArtifact
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelArtifact), &deletingArtifact); err != nil {
			return false, err
		}
		var deletingDeployment kamav1alpha1.ModelDeployment
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(modelDeployment), &deletingDeployment); err != nil {
			return false, err
		}
		return !deletingArtifact.DeletionTimestamp.IsZero() &&
			!deletingDeployment.DeletionTimestamp.IsZero() &&
			slices.Contains(deletingArtifact.Finalizers, kamav1alpha1.ModelArtifactFinalizer) &&
			slices.Contains(deletingDeployment.Finalizers, kamav1alpha1.ModelDeploymentFinalizer), nil
	})

	_ = s.apiClient.Delete(context.Background(), servingPod)
	_ = s.apiClient.Delete(context.Background(), replicaSet)
	eventually(t, "complete foreground serving workload deletion in envtest", func() (bool, error) {
		var deletingWorkload appsv1.Deployment
		err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(&workload), &deletingWorkload)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if deletingWorkload.DeletionTimestamp.IsZero() {
			return false, nil
		}
		if slices.Contains(deletingWorkload.Finalizers, metav1.FinalizerDeleteDependents) {
			deletingWorkload.Finalizers = slices.DeleteFunc(deletingWorkload.Finalizers, func(value string) bool {
				return value == metav1.FinalizerDeleteDependents
			})
			if err := s.apiClient.Update(context.Background(), &deletingWorkload); err != nil && !apierrors.IsConflict(err) {
				return false, err
			}
		}
		return false, nil
	})
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(modelDeployment), &kamav1alpha1.ModelDeployment{})
}

func containsSchedulingGate(gates []corev1.PodSchedulingGate, name string) bool {
	return slices.ContainsFunc(gates, func(gate corev1.PodSchedulingGate) bool {
		return gate.Name == name
	})
}

func (s *integrationSuite) testRetryFailureAndDeletion(t *testing.T) {
	namespace := s.createNamespace(t, "retry")
	claim := s.createBoundClaim(t, namespace, "retry-source", []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}, "")
	lost := directArtifact(namespace, "lost-result", claim.Name, "model.gguf")
	if err := s.apiClient.Create(context.Background(), lost); err != nil {
		t.Fatalf("create lost-result artifact: %v", err)
	}
	job := s.waitForOwnedJob(t, lost.UID, namespace)
	configMap := s.waitForOwnedConfigMap(t, lost.UID, namespace)
	lease := s.waitForHeldLease(t, lost.UID, namespace)
	oldJobUID, oldConfigUID := job.UID, configMap.UID
	oldJobName, oldConfigName, oldLeaseName := job.Name, configMap.Name, lease.Name
	s.markJobTerminal(t, job, batchv1.JobComplete)
	waitForEventReason(t, s.apiClient, namespace, lost.UID, "ResultUnavailable")
	eventually(t, "deterministic retry resources", func() (bool, error) {
		jobs, err := s.ownedJobs(lost.UID, namespace)
		if err != nil {
			return false, err
		}
		configs, err := s.ownedConfigMaps(lost.UID, namespace)
		if err != nil {
			return false, err
		}
		var retriedLease coordinationv1.Lease
		leaseErr := s.apiClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: oldLeaseName}, &retriedLease)
		return len(jobs) == 1 && jobs[0].Name == oldJobName && jobs[0].UID != oldJobUID &&
			len(configs) == 1 && configs[0].Name == oldConfigName && configs[0].UID != oldConfigUID && leaseErr == nil, nil
	})
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(lost), lost); err != nil {
		t.Fatalf("get retried artifact before deletion: %v", err)
	}
	if err := s.apiClient.Delete(context.Background(), lost); err != nil {
		t.Fatalf("delete retry artifact: %v", err)
	}
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(lost), &kamav1alpha1.ModelArtifact{})

	missingShard := directArtifact(namespace, "missing-shard", claim.Name, "model-00001-of-00002.gguf")
	if err := s.apiClient.Create(context.Background(), missingShard); err != nil {
		t.Fatalf("create missing-shard artifact: %v", err)
	}
	failureJob := s.waitForOwnedJob(t, missingShard.UID, namespace)
	s.completeFailedJob(t, failureJob, artifact.Result{
		SchemaVersion: artifact.SchemaVersion,
		Mode:          artifact.ModeDirect,
		Success:       false,
		Reason:        artifact.ReasonMissingShard,
		Message:       "standard GGUF shard set is incomplete",
	})
	eventually(t, "MissingShard failure condition", func() (bool, error) {
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(missingShard), missingShard); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(missingShard.Status.Conditions, kamav1alpha1.ModelArtifactConditionMissingShard)
		return condition != nil && condition.Status == metav1.ConditionTrue &&
			!meta.IsStatusConditionTrue(missingShard.Status.Conditions, kamav1alpha1.ModelArtifactConditionReady), nil
	})
	if err := s.apiClient.Delete(context.Background(), missingShard); err != nil {
		t.Fatalf("delete missing-shard artifact: %v", err)
	}
	waitForNotFound(t, s.apiClient, client.ObjectKeyFromObject(missingShard), &kamav1alpha1.ModelArtifact{})
	eventually(t, "failed artifact transient cleanup", func() (bool, error) {
		jobs, err := s.ownedJobs(missingShard.UID, namespace)
		if err != nil {
			return false, err
		}
		configs, err := s.ownedConfigMaps(missingShard.UID, namespace)
		if err != nil {
			return false, err
		}
		return len(jobs) == 0 && len(configs) == 0, nil
	})
	var retained corev1.PersistentVolumeClaim
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &retained); err != nil {
		t.Fatalf("failure cleanup removed adopted source claim: %v", err)
	}
}

func (s *integrationSuite) createNamespace(t *testing.T, suffix string) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "artifact-" + suffix + "-"}}
	if err := s.apiClient.Create(context.Background(), namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return namespace.Name
}

func (s *integrationSuite) createBoundClaim(
	t *testing.T,
	namespace, name string,
	modes []corev1.PersistentVolumeAccessMode,
	node string,
) *corev1.PersistentVolumeClaim {
	t.Helper()
	storageClass := "manual"
	volumeMode := corev1.PersistentVolumeFilesystem
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "artifact-pv-"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			AccessModes:                   append([]corev1.PersistentVolumeAccessMode(nil), modes...),
			StorageClassName:              storageClass,
			VolumeMode:                    &volumeMode,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kama-envtest/" + namespace + "/" + name},
			},
			NodeAffinity: testNodeAffinity(node),
		},
	}
	if err := s.apiClient.Create(context.Background(), pv); err != nil {
		t.Fatalf("create PersistentVolume: %v", err)
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      append([]corev1.PersistentVolumeAccessMode(nil), modes...),
			StorageClassName: &storageClass,
			VolumeMode:       &volumeMode,
			VolumeName:       pv.Name,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}
	if err := s.apiClient.Create(context.Background(), claim); err != nil {
		t.Fatalf("create PersistentVolumeClaim: %v", err)
	}
	s.setClaimBound(t, claim, modes)
	return claim
}

func (s *integrationSuite) bindClaim(
	t *testing.T,
	claim *corev1.PersistentVolumeClaim,
	modes []corev1.PersistentVolumeAccessMode,
	node string,
) *corev1.PersistentVolume {
	t.Helper()
	volumeMode := corev1.PersistentVolumeFilesystem
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "artifact-managed-pv-"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			AccessModes:                   append([]corev1.PersistentVolumeAccessMode(nil), modes...),
			StorageClassName:              "manual",
			VolumeMode:                    &volumeMode,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kama-envtest/" + claim.Namespace + "/" + claim.Name},
			},
			ClaimRef:     &corev1.ObjectReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
			NodeAffinity: testNodeAffinity(node),
		},
	}
	if err := s.apiClient.Create(context.Background(), pv); err != nil {
		t.Fatalf("create managed PersistentVolume: %v", err)
	}
	var current corev1.PersistentVolumeClaim
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &current); err != nil {
		t.Fatalf("get managed claim before binding: %v", err)
	}
	current.Spec.VolumeName = pv.Name
	if err := s.apiClient.Update(context.Background(), &current); err != nil {
		t.Fatalf("bind managed claim to PV: %v", err)
	}
	s.setClaimBound(t, &current, modes)
	if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(pv), pv); err != nil {
		t.Fatalf("refresh PersistentVolume: %v", err)
	}
	return pv
}

func (s *integrationSuite) setClaimBound(t *testing.T, claim *corev1.PersistentVolumeClaim, modes []corev1.PersistentVolumeAccessMode) {
	t.Helper()
	eventually(t, "mark claim Bound", func() (bool, error) {
		var current corev1.PersistentVolumeClaim
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(claim), &current); err != nil {
			return false, err
		}
		current.Status.Phase = corev1.ClaimBound
		current.Status.AccessModes = append([]corev1.PersistentVolumeAccessMode(nil), modes...)
		current.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}
		if err := s.apiClient.Status().Update(context.Background(), &current); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		*claim = current
		return true, nil
	})
}

func testNodeAffinity(node string) *corev1.VolumeNodeAffinity {
	if node == "" {
		return nil
	}
	return &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{node},
		}},
	}}}}
}

func directArtifact(namespace, name, claimName, entrypoint string) *kamav1alpha1.ModelArtifact {
	return &kamav1alpha1.ModelArtifact{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: kamav1alpha1.ModelArtifactSpec{
			Format:     kamav1alpha1.ArtifactFormatGGUF,
			Entrypoint: entrypoint,
			Source: kamav1alpha1.ModelArtifactSource{PersistentVolumeClaim: &kamav1alpha1.PersistentVolumeClaimSource{
				ClaimName: claimName, RootPath: ".", ImportPolicy: kamav1alpha1.PVCImportPolicyDirect,
			}},
		},
	}
}

func successfulArtifactResult(mode artifact.Mode, digest, revision, entrypoint, operationID string) artifact.Result {
	manifest := &artifact.Manifest{
		SchemaVersion: artifact.SchemaVersion,
		Format:        artifact.FormatGGUF,
		Entrypoint:    entrypoint,
		Files:         []artifact.FileRecord{{Path: entrypoint, Size: 4096, SHA256: digest}},
	}
	publishedPath := "."
	if mode != artifact.ModeDirect {
		publicationDigest, err := artifact.ManifestDigest(*manifest)
		if err != nil {
			panic(err)
		}
		publishedPath = "blobs/sha256/" + publicationDigest
	}
	return artifact.Result{
		SchemaVersion:    artifact.SchemaVersion,
		OperationID:      operationID,
		Mode:             mode,
		Success:          true,
		ResolvedRevision: revision,
		ArtifactDigest:   digest,
		Manifest:         manifest,
		GGUF: &artifact.GGUFMetadata{
			Version: 3, Architecture: "llama", Quantization: "Q4_K", ShardCount: 1, TensorCount: 42,
		},
		PublishedPath: publishedPath,
	}
}

func (s *integrationSuite) completeJob(t *testing.T, job *batchv1.Job, result artifact.Result) {
	t.Helper()
	s.logs.set(t, result)
	s.createResultPod(t, job)
	s.markJobTerminal(t, job, batchv1.JobComplete)
}

func (s *integrationSuite) completeFailedJob(t *testing.T, job *batchv1.Job, result artifact.Result) {
	t.Helper()
	s.logs.set(t, result)
	s.createResultPod(t, job)
	s.markJobTerminal(t, job, batchv1.JobFailed)
}

func (s *integrationSuite) createResultPod(t *testing.T, job *batchv1.Job) {
	t.Helper()
	controller := true
	blockOwnerDeletion := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: job.Name[:min(len(job.Name), 45)] + "-result-",
			Namespace:    job.Namespace,
			Labels: map[string]string{
				"job-name":             job.Name,
				artifactOperationLabel: job.Labels[artifactOperationLabel],
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         batchv1.SchemeGroupVersion.String(),
				Kind:               "Job",
				Name:               job.Name,
				UID:                job.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "importer", Image: "registry.invalid/importer:test"}},
		},
	}
	if err := s.apiClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create simulated importer Pod: %v", err)
	}
	eventually(t, "manager cache to observe simulated importer Pod", func() (bool, error) {
		var observed corev1.Pod
		if err := s.managerClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &observed); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return true, nil
	})
}

func (s *integrationSuite) markJobTerminal(t *testing.T, job *batchv1.Job, conditionType batchv1.JobConditionType) {
	t.Helper()
	eventually(t, "mark importer Job terminal", func() (bool, error) {
		var current batchv1.Job
		if err := s.apiClient.Get(context.Background(), client.ObjectKeyFromObject(job), &current); err != nil {
			return false, err
		}
		now := metav1.Now()
		started := metav1.NewTime(now.Add(-time.Second))
		current.Status.StartTime = &started
		terminal := batchv1.JobCondition{
			Type: conditionType, Status: corev1.ConditionTrue, Reason: "Envtest",
			Message: "simulated importer completion", LastTransitionTime: now,
		}
		switch conditionType {
		case batchv1.JobComplete:
			current.Status.CompletionTime = &now
			current.Status.Succeeded = 1
			current.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: "Envtest",
				Message: "simulated success criteria", LastTransitionTime: now,
			}, terminal}
		case batchv1.JobFailed:
			current.Status.Failed = 1
			current.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "Envtest",
				Message: "simulated failure target", LastTransitionTime: now,
			}, terminal}
		default:
			return false, fmt.Errorf("unsupported terminal Job condition %q", conditionType)
		}
		if err := s.apiClient.Status().Update(context.Background(), &current); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		*job = current
		return true, nil
	})
}

func assertImporterJobSecurity(t *testing.T, job *batchv1.Job, wantCache, wantSource, wantToken bool) {
	t.Helper()
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("importer Job enables service-account token mounting")
	}
	if len(pod.Containers) != 1 || pod.Containers[0].SecurityContext == nil || pod.Containers[0].SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*pod.Containers[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("importer container security context is incomplete: %#v", pod.Containers)
	}
	wantFSGroup := wantCache && !wantSource
	if (pod.SecurityContext != nil && pod.SecurityContext.FSGroup != nil) != wantFSGroup {
		t.Fatalf("FSGroup presence = %v, want %v", pod.SecurityContext != nil && pod.SecurityContext.FSGroup != nil, wantFSGroup)
	}
	volumes := map[string]corev1.Volume{}
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	_, hasCache := volumes["cache"]
	_, hasSource := volumes["source"]
	_, hasToken := volumes["token"]
	if hasCache != wantCache || hasSource != wantSource || hasToken != wantToken {
		t.Fatalf("Job volume contract cache/source/token=%v/%v/%v, want %v/%v/%v", hasCache, hasSource, hasToken, wantCache, wantSource, wantToken)
	}
	if hasSource && (volumes["source"].PersistentVolumeClaim == nil || !volumes["source"].PersistentVolumeClaim.ReadOnly) {
		t.Fatal("source PVC is not mounted read-only")
	}
	if hasToken {
		secret := volumes["token"].Secret
		if secret == nil || secret.DefaultMode == nil || *secret.DefaultMode != 0o440 {
			t.Fatalf("token Secret mode = %#v, want 0440", secret)
		}
	}
}

func (s *integrationSuite) waitForManagedClaim(t *testing.T, namespace, cacheName string) *corev1.PersistentVolumeClaim {
	t.Helper()
	var found corev1.PersistentVolumeClaim
	eventually(t, "managed cache claim", func() (bool, error) {
		var claims corev1.PersistentVolumeClaimList
		if err := s.apiClient.List(context.Background(), &claims, client.InNamespace(namespace), client.MatchingLabels{
			"kama.tannerburns.github.io/model-cache": cacheName,
		}); err != nil {
			return false, err
		}
		if len(claims.Items) != 1 {
			return false, nil
		}
		found = claims.Items[0]
		return true, nil
	})
	return &found
}

func (s *integrationSuite) waitForOwnedJob(t *testing.T, uid types.UID, namespace string) *batchv1.Job {
	t.Helper()
	var found batchv1.Job
	eventually(t, "owned importer Job", func() (bool, error) {
		jobs, err := s.ownedJobs(uid, namespace)
		if err != nil {
			return false, err
		}
		if len(jobs) != 1 {
			return false, nil
		}
		found = jobs[0]
		return true, nil
	})
	return &found
}

func (s *integrationSuite) waitForArtifactCleanupJob(
	t *testing.T,
	uid types.UID,
	namespace string,
) *batchv1.Job {
	t.Helper()
	var found batchv1.Job
	eventually(t, "detached artifact cleanup Job", func() (bool, error) {
		var jobs batchv1.JobList
		if err := s.apiClient.List(context.Background(), &jobs, client.InNamespace(namespace), client.MatchingLabels{
			artifactUIDLabel: string(uid), componentLabel: artifactCleanupComponent,
		}); err != nil {
			return false, err
		}
		if len(jobs.Items) != 1 {
			return false, nil
		}
		found = jobs.Items[0]
		return true, nil
	})
	return &found
}

func (s *integrationSuite) waitForArtifactCleanupConfig(
	t *testing.T,
	uid types.UID,
	namespace string,
) *corev1.ConfigMap {
	t.Helper()
	var found corev1.ConfigMap
	eventually(t, "detached artifact cleanup ConfigMap", func() (bool, error) {
		var configs corev1.ConfigMapList
		if err := s.apiClient.List(context.Background(), &configs, client.InNamespace(namespace), client.MatchingLabels{
			artifactUIDLabel: string(uid), componentLabel: artifactCleanupComponent,
		}); err != nil {
			return false, err
		}
		if len(configs.Items) != 1 {
			return false, nil
		}
		found = configs.Items[0]
		return true, nil
	})
	return &found
}

func (s *integrationSuite) ownedJobs(uid types.UID, namespace string) ([]batchv1.Job, error) {
	var list batchv1.JobList
	if err := s.apiClient.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	items := make([]batchv1.Job, 0, len(list.Items))
	for index := range list.Items {
		if controlledByUID(&list.Items[index], uid) {
			items = append(items, list.Items[index])
		}
	}
	return items, nil
}

func (s *integrationSuite) waitForOwnedConfigMap(t *testing.T, uid types.UID, namespace string) *corev1.ConfigMap {
	t.Helper()
	var found corev1.ConfigMap
	eventually(t, "owned importer ConfigMap", func() (bool, error) {
		configs, err := s.ownedConfigMaps(uid, namespace)
		if err != nil {
			return false, err
		}
		if len(configs) != 1 {
			return false, nil
		}
		found = configs[0]
		return true, nil
	})
	return &found
}

func (s *integrationSuite) ownedConfigMaps(uid types.UID, namespace string) ([]corev1.ConfigMap, error) {
	var list corev1.ConfigMapList
	if err := s.apiClient.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	items := make([]corev1.ConfigMap, 0, len(list.Items))
	for index := range list.Items {
		if controlledByUID(&list.Items[index], uid) {
			items = append(items, list.Items[index])
		}
	}
	return items, nil
}

func (s *integrationSuite) waitForHeldLease(t *testing.T, uid types.UID, namespace string) *coordinationv1.Lease {
	t.Helper()
	var found coordinationv1.Lease
	eventually(t, "artifact fingerprint Lease", func() (bool, error) {
		var leases coordinationv1.LeaseList
		if err := s.apiClient.List(context.Background(), &leases, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		for index := range leases.Items {
			lease := &leases.Items[index]
			if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == string(uid) {
				found = *lease
				return true, nil
			}
		}
		return false, nil
	})
	return &found
}

func controlledByUID(object metav1.Object, uid types.UID) bool {
	for _, reference := range object.GetOwnerReferences() {
		if reference.UID == uid && reference.Controller != nil && *reference.Controller {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func boolPointer(value bool) *bool { return &value }

func waitForNotFound(t *testing.T, kubeClient client.Client, key client.ObjectKey, object client.Object) {
	t.Helper()
	eventually(t, fmt.Sprintf("%T %s deletion", object, key), func() (bool, error) {
		err := kubeClient.Get(context.Background(), key, object)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func waitForEventReason(
	t *testing.T,
	kubeClient client.Client,
	namespace string,
	uid types.UID,
	reason string,
) {
	t.Helper()
	eventually(t, "Event reason "+reason, func() (bool, error) {
		var events eventsv1.EventList
		if err := kubeClient.List(context.Background(), &events, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		for index := range events.Items {
			event := &events.Items[index]
			if event.Regarding.UID == uid && event.Reason == reason {
				return true, nil
			}
		}
		return false, nil
	})
}

func eventually(t *testing.T, description string, check func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	var lastError error
	for time.Now().Before(deadline) {
		done, err := check()
		if done {
			return
		}
		if err != nil {
			lastError = err
		}
		time.Sleep(testPollInterval)
	}
	if lastError != nil {
		t.Fatalf("timed out waiting for %s: %v", description, lastError)
	}
	t.Fatalf("timed out waiting for %s", description)
}
