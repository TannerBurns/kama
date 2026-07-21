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
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	metricsOnce = sync.Once{}

	modelCacheReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kama_model_cache_ready",
		Help: "Whether a ModelCache has passed its latest storage probe.",
	}, []string{metricNamespaceLabel, cacheName})
	modelCacheCapacityBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kama_model_cache_capacity_bytes",
		Help: "Provisioned capacity of a ModelCache filesystem.",
	}, []string{metricNamespaceLabel, cacheName})
	modelCacheFreeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kama_model_cache_free_bytes",
		Help: "Free bytes observed by the latest ModelCache filesystem probe.",
	}, []string{metricNamespaceLabel, cacheName})
	modelArtifactReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kama_model_artifact_ready",
		Help: "Whether a ModelArtifact is verified and ready for serving.",
	}, []string{metricNamespaceLabel, artifactName})
	modelArtifactSizeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kama_model_artifact_size_bytes",
		Help: "Aggregate verified size of a ModelArtifact.",
	}, []string{metricNamespaceLabel, artifactName})
	modelDeploymentReadyReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kama_model_deployment_ready_replicas",
		Help: "Ready replicas serving the current ModelDeployment runtime fingerprint.",
	}, []string{metricNamespaceLabel, modelDeploymentMetricLabel})
	modelDeploymentRuntimeState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kama_model_deployment_runtime_state",
		Help: "Current bounded runtime state for a ModelDeployment (exactly one state is 1).",
	}, []string{metricNamespaceLabel, modelDeploymentMetricLabel, "state"})
	modelDeploymentLoadFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kama_model_deployment_load_failures_total",
		Help: "Transitions to a terminal model runtime load failure.",
	}, []string{metricReasonLabel})
	artifactOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kama_model_artifact_operations_total",
		Help: "Completed artifact import or validation operations.",
	}, []string{sourceName, metricResultLabel, metricReasonLabel})
	artifactBytesTransferred = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kama_model_artifact_bytes_transferred_total",
		Help: "Bytes transferred by successful artifact operations.",
	}, []string{sourceName})
	artifactRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kama_model_artifact_retries_total",
		Help: "Deterministic artifact Job retries after a recoverable failure.",
	}, []string{sourceName, metricReasonLabel})
	artifactCacheHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kama_model_artifact_cache_hits_total",
		Help: "Artifact operations satisfied by validated cache publication.",
	}, []string{sourceName})
	artifactOperationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kama_model_artifact_operation_duration_seconds",
		Help:    "Artifact import or validation operation duration.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 16),
	}, []string{sourceName, metricResultLabel})
	artifactValidationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kama_model_artifact_validation_duration_seconds",
		Help:    "Artifact GGUF, shard, checksum, and manifest validation duration.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 18),
	}, []string{sourceName, metricResultLabel})
	cacheProbeOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kama_model_cache_probe_operations_total",
		Help: "Completed ModelCache filesystem probes by bounded result and reason.",
	}, []string{metricResultLabel, metricReasonLabel})
)

func registerControllerMetrics() {
	metricsOnce.Do(func() {
		controllermetrics.Registry.MustRegister(
			modelCacheReady,
			modelCacheCapacityBytes,
			modelCacheFreeBytes,
			modelArtifactReady,
			modelArtifactSizeBytes,
			modelDeploymentReadyReplicas,
			modelDeploymentRuntimeState,
			modelDeploymentLoadFailures,
			artifactOperations,
			artifactBytesTransferred,
			artifactRetries,
			artifactCacheHits,
			artifactOperationDuration,
			artifactValidationDuration,
			cacheProbeOperations,
		)
	})
}
