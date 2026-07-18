# Product Definition

## Outcome

Kama makes `llama.cpp` a Kubernetes-native serving runtime for GGUF models. Platform
operators declare model and service intent; Kama manages artifact persistence,
hardware fit, workload lifecycle, routing, and elasticity.

## Primary users

- Cluster operators exposing local-model inference as a shared service.
- Application teams that want a stable llama-server-compatible endpoint without
  knowing GPU topology or managing model files in individual Pods.
- Homelab and on-premises operators with scarce or heterogeneous NVIDIA GPUs.

Resources are namespaced and protected with Kubernetes RBAC. End-user authentication
and organization-specific policy are delegated to the ingress or API gateway in
front of Kama.

## V1 use cases

1. Import a selected GGUF from a pinned Hugging Face revision exactly once.
2. Adopt or copy a GGUF that an operator placed manually in a PVC.
3. Run a model that fits on one GPU as independent one-GPU replicas across Pods.
4. Run a larger model in one Pod using multiple GPUs from the same node.
5. Fall back automatically to hybrid offload or CPU-only when the hard runtime
   envelope cannot fit available GPUs, provided combined RAM and VRAM are sufficient.
6. Optimize placement with `Balanced` by default and optional `Throughput` or
   `Latency` policies.
7. Use all compatible free GPUs under actual demand, then release them after the
   idle cooldown.
8. Preserve llama-server inference and streaming behavior through one stable,
   model-aware gateway.
9. Queue a first request while a model scales from zero and loads.

## Supported topology vocabulary

- **Replica:** one complete model copy and one routable `llama-server` endpoint.
- **Single-GPU replica:** one Pod, one process, and one GPU.
- **Local multi-GPU replica:** one Pod and one process using multiple GPUs on one
  node. llama.cpp divides model work inside the process.
- **Serving pool:** interchangeable replicas with the same artifact, runtime, and
  topology profile.
- **Distributed replica:** one model coordinated across multiple Pods or nodes.
  This requires llama.cpp RPC workers and is experimental, not a production v1 mode.
- **GGUF file shard:** one file in a split GGUF artifact. File sharding is storage
  layout and does not imply distributed inference.

## Non-goals for production v1

- Model training, fine-tuning, conversion, or automatic requantization.
- Cross-Pod or cross-node sharding of one model through llama.cpp RPC.
- Dynamic NVIDIA MIG reconfiguration, GPU time-slicing, or device overcommit.
- AMD/ROCm production support, although accelerator interfaces must not hard-code
  NVIDIA concepts into the public API.
- Multi-cluster scheduling, disaggregated prefill/decode, shared KV cache, or prompt
  persistence across replicas.
- Built-in user identity, billing, or a general-purpose API management product.
- Guaranteed byte-for-byte compatibility with every unpinned llama.cpp release.

## Product requirements

- **Declarative:** all durable intent is represented through CRDs and normal
  Kubernetes resources.
- **Observable:** resolved topology, model provenance, load progress, fallback,
  queued demand, and failures are available through status, Events, and metrics.
- **Reproducible:** model content and llama.cpp builds are pinned by immutable
  revision or digest.
- **Safe under scarcity:** no silent runtime reductions, unbounded queues, GPU
  overcommit, or destructive artifact cleanup.
- **Streaming-correct:** the gateway preserves SSE, cancellation, backpressure,
  and long model-load timeouts.
- **Recoverable:** controller, gateway, Job, and Pod restarts converge without
  redownloading verified content or publishing partial artifacts.

## V1 success criteria

- A single-GPU model scales from zero to a ready response and back to zero while its
  cached artifact remains intact.
- A model that requires two or more same-node GPUs is automatically recognized and
  starts with a valid llama.cpp layer split.
- Three available GPUs can run three one-GPU replicas when that is the selected
  balanced/throughput result and demand warrants the capacity.
- An impossible GPU fit falls back to hybrid or CPU-only with a `Degraded` condition;
  an impossible combined RAM/VRAM fit fails before serving with actionable numbers.
- Two replicas on different nodes reuse one verified RWX artifact without downloading
  from Hugging Face again.
- A manually populated PVC works in both safe `Copy` and zero-copy `Direct` modes.
- Administrative llama-server lifecycle endpoints are not exposed publicly and
  cannot conflict with the operator.

See [system architecture](02-system-architecture.md) for component boundaries and
[delivery roadmap](08-delivery-roadmap.md) for implementation order.
