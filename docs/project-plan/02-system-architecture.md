# System Architecture

## Outcome

Kama separates artifact lifecycle, serving intent, topology selection, workload
reconciliation, and request routing. This prevents per-Pod download logic and
transient traffic state from becoming part of the declarative control plane.

## Components

### Kama manager

A Go `controller-runtime` manager hosts reconcilers and admission webhooks:

- `ModelCache` reconciliation and storage validation.
- `ModelArtifact` import, verification, retention, and provenance.
- `ModelDeployment` planning, profiling, serving-pool creation, and status.
- Gateway route registry reconciliation.
- KEDA `ScaledObject` generation.

Leader election is required. Reconcilers must be idempotent and use owner references
for ordinary generated resources. Finalizers are reserved for reference release or
external cleanup that cannot be represented with owner references.

### Artifact importer

A short-lived, non-root Job downloads from Hugging Face or copies from an existing
PVC, validates GGUF content, and publishes an immutable artifact atomically. Serving
Pods never carry source credentials and never download from the Internet.

### Inventory and topology planner

The planner combines stable node/GPU capabilities, GGUF metadata, runtime intent,
storage mountability, heuristic memory estimates, and cached benchmarks. It produces
an ordered set of serving profiles: GPU, hybrid, and optionally CPU-only.

### Profiler

Bounded Jobs load and benchmark viable, previously unseen combinations. Results are
cached by model digest, llama.cpp build, hardware class/count, split mode, and runtime
envelope. A safe heuristic profile can serve while background profiling completes.

### Runtime supervisor

The Kama runtime image contains a pinned `llama-server` build and a small supervisor
that translates a resolved profile into controlled command-line arguments, reports
startup state, and coordinates readiness and draining. Raw expert arguments cannot
override model paths, bind addresses, metrics, slots, or controller-owned topology.

### Kama gateway and scaler

An always-on Go reverse proxy discovers ready serving endpoints, resolves model
aliases, observes llama slots, and routes to the least-loaded compatible replica. It
holds bounded cold-start requests and implements the KEDA external-scaler contract so
KEDA can scale generated Deployments from zero.

## Control flow

```text
ModelCache
    |
    v
ModelArtifact ---> import/validate Job ---> verified immutable GGUF
    |                                           |
    +---------------- Ready --------------------+
                                                v
ModelDeployment ---> planner/profiler ---> resolved serving profiles
       |                                        |
       +--> Deployments + Services + ScaledObjects
                              |
                              v
                    ready llama-server endpoints
                              |
                              v
                        gateway registry
```

## Request flow

```text
Client
  |
  v
Kama Gateway -- model alias --> ready serving pool --> llama-server
  |                                  ^                    |
  +-- queued demand --> KEDA scaler -+                    |
  +<------------------ SSE / response / cancellation -----+
```

When no replica is ready, the gateway holds the request in memory, signals demand,
and waits up to the model's cold-start timeout. Model data is already on persistent
storage; scale-to-zero removes only compute.

## Generated Kubernetes resources

- Import/validation/profiling Jobs.
- Deployments for homogeneous serving pools.
- ClusterIP Services and EndpointSlices selected through normal readiness.
- ConfigMaps for resolved runtime configuration.
- KEDA `ScaledObject` resources for each independently scalable pool.
- ServiceAccounts, Roles, RoleBindings, PodDisruptionBudgets, and optional
  NetworkPolicies.

StatefulSets are not needed for normal interchangeable replicas. A future distributed
coordinator/worker mode may use LeaderWorkerSet or another group abstraction, but it
is not part of production v1.

## Reconciliation invariants

- No serving workload is created until its artifact has `Ready=True`.
- A resolved profile is recorded in status and remains stable through transient
  capacity changes. Replanning requires an input fingerprint change, an explicit
  request, a failed profile, or an idle optimization rollout.
- GPU allocation is expressed through integer extended-resource requests and node
  affinity. The Kubernetes scheduler makes the final placement decision.
- Only readiness-approved endpoints enter the gateway registry.
- A topology-changing update drains existing requests before replacing Pods.
- The artifact and cache are not owned by serving Deployments and survive their
  deletion or scaling.

## Platform prerequisites

- Kubernetes 1.34-1.36.
- KEDA, initially through its external-scaler interface.
- NVIDIA device plugin/GPU Operator and GPU Feature Discovery for GPU deployments.
- A filesystem StorageClass or existing PVC suitable for regular GGUF files and
  memory mapping.

## References

- [Kubernetes operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Kubernetes controllers](https://kubernetes.io/docs/concepts/architecture/controller/)
- [llama.cpp server](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)
- [llama.cpp multi-GPU guide](https://github.com/ggml-org/llama.cpp/blob/master/docs/multi-gpu.md)

Detailed contracts live in [resource APIs](03-resource-model-and-apis.md),
[storage](04-model-artifacts-and-storage.md), [placement](05-runtime-topology-and-placement.md),
and [serving](06-serving-routing-and-autoscaling.md).
