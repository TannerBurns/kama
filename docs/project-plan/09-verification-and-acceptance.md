# Verification and Acceptance

## Outcome

Verification proves reconciliation correctness without GPUs on every commit and then
uses real GPU/storage environments for fit, performance, and release claims. Every
release publishes the exact Kubernetes, KEDA, NVIDIA, storage, llama.cpp, and model
fixtures used.

## Test layers

### Static and unit tests

- Go formatting, vet, lint, vulnerability/license checks, generated API drift, and
  Helm/schema validation.
- Pure tests for memory math, candidate generation/scoring, canonical manifests,
  path safety, routing scores, scaler formulas, conditions, and conversions.
- Golden tests for generated Deployments, Services, Jobs, ScaledObjects, RBAC, and
  runtime arguments.

### Controller integration tests

- `envtest` for CRD defaulting/validation, reconciliation, ownership/finalizers,
  status transitions, retries, and deletion.
- Fake Kubernetes Nodes, extended resources, GFD labels, PVC/PV topology, and KEDA
  APIs for deterministic placement and scaling cases.

### Container and Kind tests

- Importer against a local HTTP/Hugging Face-compatible fixture, including range
  resume, private-token behavior, duplicates, corruption, and storage exhaustion.
- Fake llama-server for readiness, slots, SSE, cancellation, overload, and API
  compatibility without a GPU.
- Kind clusters for installation, namespace isolation, NetworkPolicy manifests,
  gateway failover, KEDA activation, upgrade, and uninstall retention.

### Self-contained M1 functional acceptance

- A trusted hosted-runner workflow creates a two-node Kubernetes cluster and all
  functional storage services inside the runner, then imports immutable public and
  project-controlled private Hugging Face artifacts.
- Importer and manager interruptions prove local recovery without another artifact
  transfer. Runner-scoped request evidence is used instead of aggregate Hub download
  counters.
- Functional RWX cross-node reads, adopted RWO topology, corruption, incomplete
  content, authorization failure, and actual filesystem exhaustion are exercised in
  one evidence-producing workflow.
- A passing immutable run closes M1 functional acceptance. It does not qualify or
  advertise a named production CSI driver, backend, durability level, or performance
  profile.

### Real hardware end-to-end tests

- CPU-only Linux node.
- One NVIDIA GPU.
- One node with two or four homogeneous GPUs.
- Multiple GPU nodes for independent replicas and topology spread.
- At least two GPU products across node pools to exercise hardware-class profiling.
- Optional fast-interconnect system for feature-gated tensor-mode experiments.

Real tests use small redistributable GGUF fixtures for correctness plus documented
representative models for memory/performance qualification.

## Compatibility matrix

| Dimension | V1 matrix |
|---|---|
| Kubernetes | 1.34, 1.35, 1.36 |
| Autoscaling | KEDA 2.20.0 minimum; external scaler |
| Accelerator | NVIDIA device plugin/GPU Operator and GPU Feature Discovery |
| Runtime | One pinned llama.cpp commit/image per Kama release |
| Storage | RWX filesystem, direct/adopted RWO, retained PVC; ROX validation where available |
| Serving | CPU, one-GPU replica, N same-node GPUs, multiple independent replicas |
| API | Pinned list of public llama-server inference and streaming routes |

DRA, AMD, dynamic MIG changes, time-slicing, and cross-node llama.cpp RPC are not v1
compatibility claims.

## Required scenario suites

### Artifact and storage

- Pinned Hugging Face import, private token, retry/resume, duplicate Job execution.
- Manual PVC `Copy` and `Direct` with RWX, ROX, RWO, and PV node affinity.
- Single and standard sharded GGUF; invalid header, checksum mismatch, missing shard,
  unsafe path, and insufficient capacity.
- Controller/Job restart between staging, manifest write, rename, and status update.
- Scale-to-zero, deployment/artifact deletion, Helm uninstall, snapshot/restore, and
  retained/adopted claim protection.

### Planner and runtime

- Deterministic one/two/four-GPU fixtures and measured estimator error.
- Context, slot/concurrency, KV-cache, scratch, safety-margin, hybrid, and CPU math.
- Homogeneous-node constraint, storage mountability, taints/affinity, and quota.
- Balanced/Throughput/Latency policy behavior and profile cache invalidation.
- OOM/load failure recovery, no-fit failure, CPU degradation, and GPU migration.
- Multi-GPU Pod registers as one endpoint; independent replicas register separately.

### Gateway and autoscaling

- Model alias resolution, aggregated model list, header-scoped native routes, and
  public admin denial.
- Unary and SSE routes, slow client backpressure, cancellation, body limits, timeouts,
  backend disconnect, and safe retry boundary.
- Slot-aware distribution, saturated backend exclusion, draining, and fallback pool
  preference.
- Zero-to-one queued request, multi-replica scale-up, active-stream scale protection,
  cooldown to zero, queue overflow, cold-start timeout, and GPU exhaustion.
- Gateway/scaler replica failure, stale metrics, duplicate activation, and KEDA outage.

### Security and tenancy

- RBAC cross-namespace denial, same-namespace Secret/PVC enforcement, and alias
  collision policy.
- Pod securityContext and ServiceAccount-token assertions.
- NetworkPolicy reachability: public gateway allowed; internal admin/metrics denied.
- Credential, prompt, and completion redaction in logs, status, metrics, and traces.
- Image/Helm signature, SBOM, dependency scan, and model/runtime provenance.

## Performance qualification

For each supported hardware/storage profile, record:

- Import throughput and resume overhead.
- Cold-cache and warm-cache model load time.
- Peak VRAM/RAM versus planner estimate.
- p50/p95 time to first token, inter-token latency, aggregate token rate, and slot
  saturation for one- and multi-GPU candidates.
- Gateway latency/throughput overhead and SSE memory use.
- Zero-to-ready time, scale-up responsiveness, cooldown churn, and node-local versus
  shared-storage results when node caching is added.

Balanced-policy benchmark selection must be reproducible from the published profile
measurements.

## Named storage compatibility qualification

A production CSI driver/backend becomes a supported named configuration only after a
protected workflow or repeatable operator-run procedure passes the M1 filesystem
floor, cross-node access and topology scenarios, restart recovery, and measured
import/load checks against that exact Kubernetes version, driver version, backend,
and mount configuration. Results are recorded in
[storage qualification](../storage-qualification.md) and must not be generalized.

This compatibility work is independent of M1 functional completion. It remains a
release gate wherever a beta or v1 release claims support for the named storage
configuration.

## Failure injection

- Kill manager leaders, gateway/scaler replicas, importer/profiler Jobs, llama-server
  Pods, and a GPU node at controlled lifecycle points.
- Temporarily remove source network, KEDA APIs, cache mounts, and backend endpoints.
- Fill cache staging space, corrupt partial/final files, and make PVCs unbindable.
- Drain a node during model load and during a streaming response.
- Change an adopted direct artifact after validation and confirm the documented
  detection/contract behavior.

## Release gates

### Alpha

- M0-M3 acceptance passes on Kind plus one real NVIDIA cluster.
- CRDs remain explicitly unstable; known estimator/profile limitations are published.
- No artifact corruption, silent runtime reduction, or destructive PVC behavior.

### Beta

- M4-M6 suites pass across all Kubernetes minors and two storage/GPU environments.
- Scale-to-zero, streaming, upgrade/rollback, HA, security, and failure-injection
  scenarios pass.
- Planner estimate error and gateway overhead meet published thresholds established
  from baseline measurements.

### V1

- Every product success criterion has evidence.
- API schemas, supported route matrix, compatibility versions, defaults, and
  deprecations are frozen and documented.
- No critical/high unmitigated security, data-loss, or request-duplication issue.
- Disaster recovery and retained-storage recovery have been exercised.

## Traceability

Each implementation PR identifies its project-plan module, work package/milestone,
acceptance scenario, and affected decision IDs. A behavior change is incomplete until
its plan, tests, and operator-facing documentation agree.

The M1 evidence checklist and separation between fixture coverage, self-contained
functional acceptance, and named production storage qualification are tracked in
[M1 acceptance](../acceptance/m1.md).
