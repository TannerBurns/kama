# Decisions, Assumptions, Risks, and Open Questions

## Purpose

This register distinguishes explicit product decisions from defaults selected where a
question was not answered. Assumed defaults are implementation-ready but can be
revisited through an ADR before their dependent milestone begins.

## Accepted decisions

| ID | Decision | Rationale / effect |
|---|---|---|
| D001 | Kama is an Apache-2.0 open-source Kubernetes operator written in Go with `controller-runtime`. | Aligns with the Kubernetes ecosystem and shared controller/proxy types. |
| D002 | CRDs plus `kubectl` are the primary v1 interface. | Keeps durable intent Kubernetes-native; a CLI is later convenience. |
| D003 | Resources are namespaced with RBAC isolation; end-user auth is external. | Supports multiple teams without making Kama a general API-management product. |
| D004 | Target Kubernetes 1.34-1.36 initially. | Covers the current supported-minor window selected for the project. |
| D005 | NVIDIA-first but accelerator-extensible; use device-plugin extended resources in v1. | NVIDIA is the first test target while public intent avoids exact GPU IDs. |
| D006 | Production v1 supports independent replicas and same-node multi-GPU within one Pod. | Matches Kubernetes Pod placement and supported llama.cpp behavior. |
| D007 | Cross-Pod/cross-node llama.cpp RPC is outside production v1. | Upstream RPC remains experimental/fragile and changes the failure/scheduling unit. |
| D008 | Scale-to-zero is required and the first request waits in a bounded activator queue. | Releases scarce accelerators while preserving a normal synchronous client flow. |
| D009 | Hugging Face and existing PVC are v1 model sources. | Covers connected and offline/manual cluster workflows. |
| D010 | A persistent shared cache PVC retains verified content independently of compute. | Prevents repeated downloads and supports scale-to-zero/cross-node replicas. |
| D011 | Placement is automatic from model/runtime/storage/hardware data. | Users specify service intent rather than llama.cpp GPU topology. |
| D012 | Heuristics generate candidates; bounded profiling measures them; results are cached. | Model size alone cannot predict memory and performance accurately. |
| D013 | Optimization policies are Balanced default plus Throughput and Latency. | Supports a general default and explicit service objectives. |
| D014 | Compatible free GPUs are consumed only under demand. | Maximizes active throughput without keeping idle models resident. |
| D015 | Automatic fallback may use hybrid CPU/GPU and CPU-only. | Maximizes availability; fallback warns and must fit combined RAM/VRAM. |
| D016 | The gateway publicly proxies inference/streaming APIs; lifecycle/admin APIs remain internal. | Preserves llama-server client behavior without conflicting control planes. |
| D017 | KEDA is a required v1 autoscaling dependency. | Provides Kubernetes-native scale-to-zero and replica management. |

## Assumed implementation defaults

| ID | Default | Revisit by |
|---|---|---|
| A001 | Use a Kama KEDA external scaler, not the KEDA HTTP add-on or Prometheus scaler. | M5 design freeze |
| A002 | Expose high-level runtime intent with guarded expert overrides. | M2 API freeze |
| A003 | Context, desired concurrency, KV precision, and artifact quantization are hard constraints; never silently shrink them. | M3 planner freeze |
| A004 | Use `split-mode=layer` for production same-node multi-GPU; tensor mode is feature-gated and profile-only. | M3 hardware validation |
| A005 | Shared content-addressed RWX cache per namespace/cache class; `Copy` is the default manual-PVC policy. | M1 storage validation |
| A006 | Queue defaults: 100 requests per model, 10-minute cold-start timeout, 10-minute idle cooldown; all are configurable. | M4/M5 load testing |
| A007 | Balanced scoring uses an equal-weight harmonic mean of normalized throughput and inverse p95 latency, tied by fewer GPUs/RAM. | M3 profiling study |
| A008 | Gateway queues remain in-memory and do not persist prompts. | M4 security/design review |
| A009 | Full-GPU, hybrid, and CPU-only may be separate ordered serving pools with independent KEDA resources. | M5 prototype |

## Resolved validation items

The identity and minimum autoscaling dependency selected for M0 are recorded here so
their original validation IDs remain traceable.

| ID | Resolution | Decision evidence / remaining gate |
|---|---|---|
| O001 | Use Go module `github.com/TannerBurns/kama` and permanent API group `kama.tannerburns.github.io`. | Resolved by [ADR-0001](../adr/0001-project-identity.md). |
| O002 | Require KEDA 2.20.0 or newer. | Resolved by [ADR-0002](../adr/0002-dependency-and-version-policy.md). The Kubernetes 1.36 activation test remains an M0 completion gate; this entry does not claim it has passed. |

## Open validation items

| ID | Item | Default until resolved | Blocking milestone |
|---|---|---|---|
| O003 | Default context, concurrency, KV types, queue body limit, and placement timeout. | Use conservative runtime defaults; publish measured tuning before beta. | M2-M5 |
| O004 | Reference RWX storage classes and performance floor. | Require POSIX filesystem semantics and publish tested configurations rather than naming an untested universal default. | M1/M6 |
| O005 | Whether dedicated per-artifact PVCs ship alongside shared cache layout in v1. | Shared cache is required; dedicated claims remain optional until lifecycle tests justify them. | M1 |
| O006 | Preconfigured MIG resources in v1. | Full GPUs only in the support claim; do not dynamically reconfigure MIG. | M3/beta |
| O007 | Exact public llama-server endpoint matrix. | Start with OpenAI chat/completions/embeddings/responses and selected native inference routes, then pin through conformance tests. | M4 |
| O008 | Maximum acceptable planner estimate error and gateway overhead. | Establish baselines on reference hardware and turn them into beta thresholds. | M3/M6 |
| O009 | Profile Job budget and maintenance-rollout policy. | Low priority, bounded concurrency, safe heuristic first, optimize only while idle. | M3 |

## Major risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| VRAM estimates miss context/KV/scratch overhead. | OOM, slow fallback, poor placement. | Conservative margin, exact runtime envelope, measured profiles, no-fit/load feedback. |
| Device plugin cannot select an exact NVLink-connected subset. | Variable multi-GPU performance. | Homogeneous node pools, stable labels, layer split default, DRA later. |
| Shared filesystem mmap/load is slow. | Long cold starts and poor CPU offload. | Benchmark RWX, retain golden copy, add node-local NVMe/CSI clone tiers. |
| Upstream llama.cpp API/defaults change. | Client or runtime regressions. | Pin build/image, version adapter, conformance matrix, controlled upgrades. |
| Scale-from-zero exceeds client timeout. | Failed first requests. | Persistent artifacts, bounded queue, explicit timeout, prewarm option, cold-start metrics. |
| Gateway/scaler aggregate demand is stale. | Under/over-scaling. | HA, activation streaming, endpoint heartbeats, bounded queues, KEDA failure tests. |
| CPU fallback is technically available but unusably slow. | Queue growth and misleading availability. | Profile CPU, weight capacity, `Degraded` status, queue limit, allow policy to disable fallback. |
| One busy model consumes all compatible GPUs. | Starvation of other namespaces/models. | Demand-only scale, ResourceQuota/PriorityClass, user max limits; fair-share policy later. |
| Automatic artifact cleanup loses manually supplied data. | Irrecoverable model loss. | `Retain` default, adopted claims never owned/deleted, explicit purge, snapshot guidance. |
| Cross-Pod sharding is mistaken for replica load balancing. | Incorrect routing/failure behavior. | Explicit vocabulary, v1 exclusion, gateway registers only complete replicas. |

## Decision process

- Accepted product decisions change only through a documented ADR and updates to all
  affected modules and tests.
- An assumed default becomes accepted at its listed milestone design freeze unless an
  ADR replaces it.
- Open items must have an owner and evidence before their blocking milestone exits.
- Experimental capabilities never enter `Auto` placement until their compatibility,
  security, failure, and performance gates are documented and passed.
