# Kama

Kama (Kubernetes Llama) is a planned Kubernetes-native control and data plane for
running GGUF models with `llama.cpp` and `llama-server`. It will import and retain
models, determine a feasible CPU/GPU topology, create serving workloads, route
requests across ready replicas, and scale model capacity to and from zero.

The repository is currently in the architecture and project-planning stage.

## Project plan

The canonical, modular plan starts at
[docs/project-plan/README.md](docs/project-plan/README.md).

The current v1 direction is:

- Go and `controller-runtime`, distributed as an Apache-2.0 open-source operator.
- Namespaced CRDs and `kubectl` as the primary management interface.
- NVIDIA-first GPU support through the device plugin and GPU Feature Discovery,
  with an internal accelerator abstraction for future backends.
- Independent model replicas across Pods and same-node multi-GPU inference within
  one Pod; cross-node llama.cpp RPC is outside the production v1 boundary.
- Persistent, verified GGUF artifacts from Hugging Face or manually populated PVCs.
- A llama-server-compatible gateway with capacity-aware routing and KEDA-backed
  scale-to-zero.

## Planning status

No implementation has been bootstrapped yet. Accepted decisions, assumed defaults,
and remaining validation items are tracked in
[decisions and open questions](docs/project-plan/10-decisions-and-open-questions.md).
