# Runtime Topology and Placement

## Outcome

Kama automatically selects a feasible serving shape from the model, hard runtime
envelope, storage topology, and cluster hardware. Heuristics make an immediate safe
choice; bounded profiling improves that choice and caches the result.

## V1 topology boundary

### Single-GPU replicas

If one GPU can satisfy the runtime envelope, each Pod receives one GPU and loads a
complete model. Additional GPUs become additional replicas under demand rather than
being added to a process that may become slower.

### Same-node multi-GPU replicas

If the model cannot fit one GPU, a single Pod requests the smallest feasible number
of GPUs on one node. One `llama-server` process owns all slots and uses llama.cpp
multi-GPU distribution. The gateway still sees one endpoint.

`split-mode=layer` is the production default. Experimental tensor mode may be
profiled only behind an explicit feature gate for a compatible pinned build,
architecture, cache type, and fast-interconnect hardware. It is never an automatic
unprofiled fallback.

### Replicas across Pods

Several Pods are complete, independent model copies. Their slots and KV caches are
process-local. The gateway combines their request capacity but does not create shared
slots or shared KV state.

### Cross-Pod model sharding

A coordinator plus llama.cpp RPC workers would be one distributed replica, not a set
of load-balanced servers. Upstream describes RPC as experimental/fragile and not safe
for untrusted networks. Production v1 excludes it and `Auto` never selects it.

## Planner inputs

- Artifact byte size, GGUF architecture, layer/tensor metadata, quantization, and
  shard manifest.
- Requested maximum context per request, desired concurrency, KV-cache types,
  batching constraints, and llama.cpp build capabilities.
- NVIDIA extended-resource names and counts, GPU Feature Discovery product, memory,
  CUDA capability, count, and topology labels.
- Node allocatable CPU/RAM and resource requests already declared by workloads.
- PVC access mode, PV node affinity, and future node-cache presence.
- User selectors, taints/tolerations, namespace ResourceQuota, priority, and expert
  GPU-count bounds.
- Cached measured profiles for the exact runtime fingerprint.

Observed free capacity influences activation and fallback, but it is not treated as a
reservation. The Kubernetes scheduler remains authoritative.

## Fit model

For each candidate shape, estimate and then measure:

```text
VRAM = offloaded weights + KV cache + compute/scratch buffers + safety reserve
RAM  = non-offloaded weights + mapped-file/host overhead + runtime reserve
```

KV memory is derived from model dimensions, hard context tokens, slot count, and the
declared key/value cache types. The initial safety reserve is configurable and
calibrated from profiling results. Status exposes both the estimate and measured
high-water mark.

Kama never silently reduces context, concurrency, cache precision, or changes the
GGUF quantization to pass fit validation.

## Candidate generation

For each storage-compatible node hardware class, enumerate in order:

1. Full GPU offload using one through N homogeneous same-node GPUs.
2. Hybrid profiles using fewer GPUs plus sufficient same-node system RAM.
3. CPU-only with sufficient RAM and CPU resources.

Mixed GPU products inside one replica are not selected automatically in v1 because
the device plugin cannot guarantee exact device choice and asymmetric performance is
hard to predict. Different replicas may run on different profiled hardware classes.

Candidates that cannot mount the artifact, violate resource/affinity constraints,
use an unsupported llama.cpp mode, or exceed combined RAM/VRAM are removed before a
Pod is created.

## Heuristic, profiling, and cache pipeline

1. Parse the artifact and build conservative candidates.
2. Select a safe heuristic candidate so first service is not blocked on exhaustive
   benchmarks.
3. Run bounded, low-priority profile Jobs for viable unseen fingerprints as capacity
   permits.
4. Measure load time, peak VRAM/RAM, p95 time to first token, inter-token latency,
   aggregate token rate, and supported parallel slots.
5. Store results in controller-owned `ModelProfile` resources.
6. Recompute the optimum and roll to it only after current streams drain or during an
   explicit maintenance window.

The profile key includes artifact digest, llama.cpp build digest, hardware class and
count, split mode, context, concurrency, KV types, and relevant batching fields.

## Optimization policies

- **Throughput:** prefer the smallest fitting GPU group with the best measured
  tokens-per-second per GPU, then scale independent replicas across available GPUs.
- **Latency:** choose the candidate with the lowest measured p95 time to first token,
  then inter-token latency, even when it uses more GPUs per replica.
- **Balanced (default):** among Pareto-optimal candidates, maximize the equally
  weighted harmonic mean of normalized throughput and inverse p95 latency; ties use
  fewer GPUs and then less RAM.

Without measured data, all policies use the conservative heuristic. Status marks the
decision `Estimated`; it becomes `Profiled` after measurement.

## Elastic capacity and fallback

- Capacity is consumed only under queued or active demand; Kama does not prefill idle
  GPUs with replicas.
- KEDA may request replicas up to the theoretical number of compatible complete
  groups, or a user/ResourceQuota limit. The scheduler decides how many can run.
- The fastest ready profile receives new requests first.
- If primary Pods remain unschedulable beyond the placement timeout, the scaler may
  activate the next ordered hybrid or CPU pool.
- CPU-only is a valid last resort but always sets `Degraded=True`, records the reason,
  and emits a warning Event.
- If no node can satisfy combined RAM and VRAM, the deployment enters `FitFailed` and
  the gateway returns an unavailable response after its cold-start timeout.
- When faster GPU capacity returns, Kama warms a replacement, sends new requests to
  it, drains slower fallback replicas, and scales them down without interrupting
  existing streams.

Kama does not preempt unrelated workloads. Kubernetes PriorityClass and ResourceQuota
remain the cluster administrator's fairness and preemption tools.

## Replanning and rollout

Replanning is triggered by artifact/runtime/build changes, a failed active profile,
new measured profile data, explicit user request, or a newly supported hardware
class. Ordinary fluctuations in free GPU count do not rewrite a running Pod template.

Topology-changing rollouts use `maxSurge: 0` when no spare accelerator exists. A
zero-downtime surge is used only when compatible spare resources exist or the user
requires it. Singleton replacements expose expected downtime in status.

## Status and metrics

Status records candidate profiles, rejection reasons, selected profile, estimated and
measured memory, split arguments, storage constraints, and whether the decision is
estimated or profiled.

Metrics cover candidate counts, planning duration, fit failures, fallback activations,
profile duration/results, unschedulable time, GPU/RAM requested, and topology changes.

## Acceptance criteria

- Fixtures deterministically resolve to one, two, or four GPUs as expected.
- A one-GPU-fitting model favors replicas for `Throughput` and does not widen without
  a measured `Latency` benefit.
- Multi-GPU Pods request all devices from one node and present one gateway endpoint.
- Hard context/concurrency/KV settings survive all fit attempts unchanged.
- Direct RWO artifacts restrict candidate nodes before scheduling.
- Hybrid and CPU-only candidates request sufficient RAM and surface degradation.
- No-fit reports required versus available RAM/VRAM and creates no crash-looping
  server.
- Cached profiles are invalidated by any fingerprint input change.

## References

- [llama.cpp multi-GPU guide](https://github.com/ggml-org/llama.cpp/blob/master/docs/multi-gpu.md)
- [llama.cpp server options and metrics](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)
- [llama.cpp RPC](https://github.com/ggml-org/llama.cpp/blob/master/tools/rpc/README.md)
- [Kubernetes GPU scheduling](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/)
- [NVIDIA GPU Feature Discovery labels](https://github.com/NVIDIA/k8s-device-plugin/blob/main/docs/gpu-feature-discovery/README.md)
- [Kubernetes topology spread](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/)
