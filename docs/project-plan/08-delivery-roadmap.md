# Delivery Roadmap

## Outcome

The roadmap is dependency-ordered rather than calendar-based because staffing,
hardware access, and release cadence are not yet known. Every milestone ends in an
independently demonstrable increment and a documented release gate.

```text
Foundation
   |
Artifact plane
   |
Baseline serving
   |
Topology planner + profiling
   |
Gateway and routing
   |
KEDA elasticity + fallback pools
   |
Hardening and v1 release
```

## M0 - Repository and engineering foundation

**Status: Complete (2026-07-18).**

Completion evidence: [verified commit](https://github.com/TannerBurns/kama/commit/ef63e791a7435092ce04dd77f8c556c1993735a7),
[passing CI run](https://github.com/TannerBurns/kama/actions/runs/29665843344), and
[passing Kubernetes 1.34-1.36 Kind matrix](https://github.com/TannerBurns/kama/actions/runs/29665843277).

### Deliverables

- [x] Initialize Git, Apache-2.0 license, Go module, Kubebuilder/controller-runtime
  project, Makefile/task entrypoints, and contribution/security documentation.
- [x] Establish API generation, linting, unit/envtest, container builds, Helm
  packaging, SBOM generation, signing/attestation hooks, and CI. Artifact publication
  remains a release-time action and was not counted as completed.
- [x] Create a fake llama-server, KEDA external-scaler fixture, and project-owned
  synthetic GGUF metadata for non-GPU tests.
- [x] Write ADRs for the API group/domain, dependency/version policy, and artifact
  layout.

### Definition of done

- [x] A generated empty operator installs/uninstalls on every target Kubernetes minor.
- [x] CI reproduces code generation and rejects uncommitted generated changes.
- [x] Images and chart are versioned from one release metadata source.

## M1 - Persistent artifact plane

**Status: In Progress.** The repository-scoped implementation, CI/Kind gates, pinned
public Hugging Face lane, and functional storage/failure lanes are verified at
[`5a7390176fd6eecf37a5f6803197a87abe92fa40`](https://github.com/TannerBurns/kama/commit/5a7390176fd6eecf37a5f6803197a87abe92fa40)
and [workflow run 29763107454](https://github.com/TannerBurns/kama/actions/runs/29763107454).
M1 remains open only because the required project-controlled private Hugging Face
repository and read-only token are not configured, so the successful live job
correctly recorded `qualifying=false`. Qualification of a named production CSI
driver/backend is a separate compatibility and release gate; it does not block M1
functional acceptance.

### Deliverables

- [x] `ModelCache` and `ModelArtifact` CRDs, webhook validation, controllers, and status.
- [x] Managed shared-PVC layout and adopted PVC `Copy`/`Direct` modes.
- [x] Hugging Face resolver/importer, checksum/GGUF/shard validation, resumable staging,
  atomic publication, retention, Events, and metrics.
- [x] Manual PVC examples and recovery documentation.

### Definition of done

- [ ] A pinned public/private Hugging Face artifact imports once and remains ready across
  controller and Job restarts. The pinned public SmolLM2 artifact passes; the private
  lane remains pending.
- [x] Manual RWX and RWO artifacts validate and surface correct placement constraints.
- [x] Corrupt, incomplete, unauthorized, and storage-full cases fail safely.

Design: [model storage](04-model-artifacts-and-storage.md).
Evidence checklist: [M1 acceptance](../acceptance/m1.md).
Named storage compatibility results: [storage qualification](../storage-qualification.md).

## M2 - Baseline single-replica serving

### Deliverables

- `ModelDeployment` CRD and artifact dependency reconciliation.
- Pinned CUDA and CPU llama-server runtime images plus supervisor.
- One fixed single-GPU or CPU Deployment, ClusterIP Service, startup/readiness/drain
  behavior, and controlled runtime arguments.
- Basic status and native internal diagnostics.

### Definition of done

- A verified model starts on one NVIDIA GPU and serves direct internal streaming
  requests.
- CPU-only works with an explicit `Degraded` status.
- Artifact or load failure does not create a restart loop or ready endpoint.

Design: [resource APIs](03-resource-model-and-apis.md) and
[system architecture](02-system-architecture.md).

## M3 - Automatic topology planner and profiler

### Deliverables

- NVIDIA/GPU Feature Discovery inventory adapter and generic accelerator interface.
- GGUF/runtime VRAM and RAM estimator with transparent calculations.
- Candidate generation for full GPU, same-node multi-GPU layer split, hybrid, and
  CPU-only.
- Heuristic selection, bounded profile Jobs, `ModelProfile` cache, and Balanced,
  Throughput, and Latency policies.
- Stable profile status, safe topology-changing rollouts, and storage-aware placement.

### Definition of done

- Real hardware tests resolve representative models to correct one/two/four-GPU,
  hybrid, and CPU outcomes.
- Hard runtime inputs are never reduced.
- Profile results improve or confirm the heuristic and are reused by fingerprint.
- No-fit reports required versus available memory before server crash-looping.

Design: [topology and placement](05-runtime-topology-and-placement.md).

## M4 - Shared gateway and replica routing

### Deliverables

- HA Go reverse proxy, model registry, public/internal endpoint policy, and aggregated
  `/v1/models`.
- SSE/cancellation-correct proxying and upstream conformance tests.
- Slot/metrics collection, capacity-aware routing, draining, bounded per-model queues,
  and overload responses.
- Multiple independent replicas and topology spread.

### Definition of done

- OpenAI-style and selected native llama-server inference routes pass conformance.
- Requests balance by usable capacity across one- and multi-GPU replicas.
- Admin endpoints remain private; retry behavior never duplicates accepted generation.
- Gateway failover preserves service for ready backends.

Design: [serving and routing](06-serving-routing-and-autoscaling.md).

## M5 - KEDA elasticity and ordered fallback

### Deliverables

- KEDA external-scaler service and per-pool `ScaledObject` reconciliation.
- Aggregate queued/active/slot demand, activation streaming, scale-to-zero, idle
  cooldown, and cold-start request holding.
- Full-GPU, hybrid, and CPU-only ordered pools with faster-profile preference,
  placement timeout, fallback warnings, migration, and draining.
- `maxReplicas: auto` bounded by compatible theoretical capacity and namespace policy.

### Definition of done

- First traffic scales zero to one and completes within configured timeout.
- Sustained demand consumes compatible free GPUs as complete replicas; idle traffic
  returns them to zero.
- Active streams prevent scale-down.
- GPU exhaustion activates an allowed fallback or returns bounded, observable
  unavailability.

Design: [serving and autoscaling](06-serving-routing-and-autoscaling.md).

## M6 - Production hardening and beta

### Deliverables

- Least-privilege RBAC, secure Pod defaults, NetworkPolicy examples, secret redaction,
  image signing/SBOM, and dependency scanning.
- Full metrics/dashboards, structured logs, optional tracing, alerts, and runbooks.
- Failure injection for manager/gateway/Job/Pod/node/storage/KEDA failures.
- Upgrade, rollback, retained-storage restore, and llama.cpp API compatibility testing.
- Helm documentation, tutorials, examples, support matrix, and performance report.

### Definition of done

- All beta release gates in [verification](09-verification-and-acceptance.md) pass.
- No severity-critical security findings remain open.
- Two real cluster/storage configurations complete the supported end-to-end suite.
- Upgrade and rollback preserve model artifacts and serving intent.

## M7 - V1 release

### Deliverables

- Freeze and document the supported CRD/runtime contract.
- Publish signed images, Helm chart, checksums, SBOMs, compatibility matrix, examples,
  migration notes, and known limitations.
- Establish issue templates, security response, release cadence, and deprecation policy.

### Definition of done

- Every v1 success criterion in [product definition](01-product-definition.md) is
  demonstrated and linked to automated or repeatable acceptance evidence.
- The decision register contains no unresolved release-blocking item.

## Post-v1 workstreams

- Persistent node-local NVMe model cache and cache-aware scheduling.
- CSI clone/snapshot storage profiles and Kubernetes 1.36 OCI image-volume sources.
- Gateway API `InferencePool` integration and pluggable endpoint picker.
- Kubernetes DRA accelerator backend and exact device/fabric selection.
- AMD/ROCm runtime and hardware CI.
- Preconfigured MIG resource profiles after isolation/performance validation.
- Optional authenticated management CLI and resumable manual uploader.
- Experimental coordinator/worker llama.cpp RPC groups only after upstream security,
  reliability, and performance meet a separately documented gate.

## Workstream ownership model

The implementation can run in parallel after M0:

- Artifact/API team: M1 and API portions of M2.
- Runtime/planner team: runtime image, supervisor, estimator, and M3.
- Gateway/scaling team: M4 and M5 after stable readiness/status contracts.
- Platform/quality team: CI, Helm, security, observability, and verification throughout.

Shared API changes require an ADR and an update to every affected plan module and
acceptance test before implementation merges.
