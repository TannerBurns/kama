# Kama

Kama (Kubernetes Llama) is being built as a Kubernetes-native control and data plane
for running GGUF models with `llama.cpp` and `llama-server`. It will import and
retain models, determine a feasible CPU/GPU topology, create serving workloads,
route requests across ready replicas, and scale model capacity to and from zero.

The M0 engineering foundation is complete. The M1 persistent-artifact-plane
repository implementation is present: the repository contains the v1alpha1 artifact
API, importer and persistent-cache implementation, packaging, examples, and recovery
guidance. The M1 milestone remains In Progress because immutable fixture-backed Kind
results and strict live storage/private-source acceptance evidence remain outstanding.

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

**M0 is complete; M1 repository implementation is complete and acceptance is in
progress.** M0 completion is backed by the passing
[CI run](https://github.com/TannerBurns/kama/actions/runs/29665843344),
[Kubernetes 1.34-1.36 compatibility matrix](https://github.com/TannerBurns/kama/actions/runs/29665843277),
and [verified commit](https://github.com/TannerBurns/kama/commit/ef63e791a7435092ce04dd77f8c556c1993735a7).
Accepted decisions, assumed defaults, and remaining validation items are tracked in
[decisions and open questions](docs/project-plan/10-decisions-and-open-questions.md).

Start with the [M1 examples](examples/README.md), consult the
[artifact recovery runbook](docs/runbooks/artifact-import-and-recovery.md), and do
not treat M1 as complete until the fixture-backed Kind matrix and every strict live
definition-of-done gate in the
[delivery roadmap](docs/project-plan/08-delivery-roadmap.md) have linked evidence.
The Helm chart is the complete M1 installation path. `config/default` remains a
developer Kustomize bundle with admission disabled until a downstream overlay
provides webhook TLS.
