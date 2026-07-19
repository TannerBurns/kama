# Operations, Security, and Observability

## Outcome

Kama operates as a namespace-isolated platform component with least-privilege
controllers, private model lifecycle endpoints, reproducible artifacts/images, and
enough status and telemetry to explain every import, placement, route, and scaling
decision.

## Namespace and identity model

- `ModelCache`, `ModelArtifact`, `ModelDeployment`, source Secrets, PVCs, and generated
  workloads are namespaced.
- The manager has cluster-wide read access only where required for Nodes, resource
  inventory, StorageClasses, and discovery. Namespaced reconcilers use scoped write
  permissions.
- Cross-namespace PVC, Secret, artifact, and deployment references are rejected in
  v1.
- Default model aliases are namespace-qualified. A short global alias requires a
  uniqueness check and explicit gateway policy.
- Kubernetes RBAC controls platform authors. End-client authentication, TLS, and
  organization-level authorization are supplied by the ingress/API gateway in front
  of Kama.

## Workload security

### Manager, gateway, scaler, and supervisor

- Run as non-root with a read-only root filesystem, dropped Linux capabilities,
  seccomp `RuntimeDefault`, no privilege escalation, and explicit CPU/memory limits.
- Use dedicated ServiceAccounts with no automounted token where Kubernetes API access
  is unnecessary.
- Pin images by digest in release manifests and publish an SBOM and signature.

### Importer and profiler

- Source credentials come from same-namespace Secrets and exist only for the Job.
- Do not write credentials, signed URLs, gated-model details, or authorization bodies
  to status or logs.
- Importers have write access only to staging/final cache paths and read-only access
  to manual source PVCs.
- Validate paths, regular-file types, expected size/digest, GGUF structure, and shard
  completeness before publication.
- Apply timeout, retry, resource, and temporary-storage limits.

### llama-server Pods

- Mount artifacts read-only and expose native administrative endpoints only on the
  internal network.
- Do not expose llama.cpp RPC in production v1.
- Treat model files as untrusted parser input; isolate server Pods, pin the upstream
  build, and promptly test and roll security updates.

## Network policy

When the cluster supports NetworkPolicy, the chart supplies opt-in policies that:

- Allow public traffic only to the Kama gateway.
- Allow gateway traffic to serving HTTP ports and internal telemetry endpoints.
- Allow manager/scaler traffic only to required internal services.
- Allow importer egress to configured Hugging Face endpoints and cluster DNS.
- Deny direct external access to llama-server health, metrics, slot, and admin routes.
- Deny all llama.cpp RPC ports because distributed mode is disabled.

The chart cannot assume one ingress implementation, CNI, certificate manager, or
external authentication product.

## Supply-chain and provenance

- Pin the llama.cpp source commit and final runtime image digest for every Kama
  release.
- Maintain an adapter/conformance matrix because upstream REST endpoints and defaults
  can change.
- Record model repository, resolved revision, filenames, digests, import time, and
  declared license metadata in `ModelArtifact.status` without claiming to validate
  a model's legal terms.
- Generate SBOMs, scan images and Go dependencies, sign release images/charts, and
  publish checksums.
- Expert llama.cpp flags pass through an allowlist tied to the pinned build.

## Observability model

### Kubernetes status and Events

Every reconciled resource reports `observedGeneration`, conditions with reasons, and
references to generated resources. Events are emitted for meaningful transitions:
import start/failure/success, topology resolution/change, fallback, readiness,
activation timeout, and retention/purge decisions.

### Prometheus metrics

- **Controller:** reconcile duration/errors, work queue depth, status transitions.
- **Artifact:** import bytes/duration/retries, validation failure, cache capacity/hits.
- **Planner:** candidates, fit rejection, estimate error, profile results/fallback.
- **Serving:** ready/loading replicas, startup/load time, usable slots, token rate.
- **Gateway:** requests, active/queued/rejected, TTFT, latency, cancellation, routing.
- **Scaler:** demand, desired/actual replicas, cold starts, pending capacity, cooldown.

Labels use namespace, artifact/deployment/pool, topology profile, and bounded reason
values. Model prompts, completions, authorization headers, and unbounded request IDs
are never metric labels or logs.

### Logs and traces

Use structured JSON logs with resource identity, generation, request correlation ID,
and sanitized errors. Request/response bodies are off by default. OpenTelemetry trace
propagation through the gateway is optional; spans cover queue, route selection,
backend connect, TTFT, and completion without prompt content.

## Availability and recovery

- Run two or more gateway replicas with topology spread and a PodDisruptionBudget.
- Run multiple manager/scaler replicas with leader election where state mutation must
  be serialized.
- Import and profiling Jobs are idempotent; controller restart does not republish or
  redownload verified artifacts.
- Gateway queues are intentionally in-memory. A failed gateway drops only its held
  connections; clients retry against another replica.
- Conservative liveness and long startup probes avoid restart loops during model load
  or saturation.
- Cache PVC outage blocks new load but does not immediately terminate an already
  running process. Recovery reconciles readiness and artifact integrity.
- CSI snapshots are an optional backup for irreplaceable manual artifacts. The
  `Retain` default protects storage from chart uninstall or compute deletion.

## Upgrade and rollout policy

- CRDs begin at `v1alpha1`; schema changes preserve conversion paths before beta.
- Pin and test one llama.cpp build per Kama release. Runtime upgrade creates a new
  profile fingerprint and controlled serving rollout.
- GPU-scarce pools use `maxSurge: 0`; zero-downtime rollout requires explicitly spare
  compatible capacity.
- Gateway and manager support at least one adjacent CRD/runtime version during an
  upgrade.
- Rollback never mutates verified artifact content.

## Required runbooks

- [Artifact import and storage recovery](../runbooks/artifact-import-and-recovery.md):
  stuck imports, source authorization, checksum/shard failures, cache full, snapshots,
  retained claims, and the `Direct` `ValidatedOnce` contract.
- Model cannot fit RAM/VRAM or repeatedly fails load.
- GPU Pod remains unschedulable or falls back to CPU.
- Cold start exceeds timeout or KEDA cannot activate a pool.
- Gateway saturation, high rejection rate, or backend slot imbalance.
- PVC unavailable, snapshot restore, and retained-volume recovery.
- llama.cpp runtime upgrade/rollback and API conformance failure.

## Acceptance criteria

- RBAC tests prove tenants cannot read another namespace's source Secret or mutate its
  Kama resources.
- Public scans cannot reach llama-server lifecycle or metrics endpoints.
- Logs/status remain credential- and prompt-free during representative failures.
- Manager/gateway restart, node drain, and importer retry converge without artifact
  corruption or permanent route loss.
- Metrics explain why a request queued, why a topology was selected, and why a
  deployment fell back or failed.
- Helm uninstall does not delete retained/adopted model storage.

## References

- [Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [Kubernetes Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [Kubernetes NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- [llama.cpp security policy](https://github.com/ggml-org/llama.cpp/security)
- [llama.cpp REST API changelog](https://github.com/ggml-org/llama.cpp/issues/9291)
