# Kama

Kama (Kubernetes Llama) is being built as a Kubernetes-native control and data plane
for running GGUF models with `llama.cpp` and `llama-server`. It will import and
retain models, determine a feasible CPU/GPU topology, create serving workloads,
route requests across ready replicas, and scale model capacity to and from zero.

The M0 engineering foundation and M1 persistent artifact plane are complete. M1
contains the v1alpha1 artifact API, importer and persistent-cache implementation,
packaging, examples, and recovery guidance; CI, the Kubernetes 1.34-1.36 matrix,
public Hugging Face E2E, and storage resilience E2E are verified. The project owner
closed M1 with the protected private-source repetition recorded as a non-blocking
follow-up, not as evidence that already passed.

The M2 baseline-serving repository implementation is also present: `ModelDeployment`
provides explicit CPU or one-NVIDIA-GPU intent, the controller creates one internal
serving workload from a ready artifact, and Kama-owned CPU/CUDA llama.cpp runtimes
share a one-shot supervisor with readiness, diagnostics, and drain behavior. M2
remains In Progress until its hosted CPU/failure and protected real-NVIDIA acceptance
evidence is recorded.

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

**M0 and M1 are complete; M2 is in progress.** M0 completion is backed by the passing
[CI run](https://github.com/TannerBurns/kama/actions/runs/29665843344),
[Kubernetes 1.34-1.36 compatibility matrix](https://github.com/TannerBurns/kama/actions/runs/29665843277),
and [verified commit](https://github.com/TannerBurns/kama/commit/ef63e791a7435092ce04dd77f8c556c1993735a7).
Accepted decisions, assumed defaults, and remaining validation items are tracked in
[decisions and open questions](docs/project-plan/10-decisions-and-open-questions.md).

Start with the [artifact-plane examples](examples/README.md), consult the
[artifact recovery](docs/runbooks/artifact-import-and-recovery.md) and
[model serving](docs/runbooks/model-serving-and-drain.md) runbooks. The unexecuted
private-source M1 repetition remains transparently tracked in the
[M1 acceptance record](docs/acceptance/m1.md) without reopening the closed milestone.
The reusable real-cluster suites and extension contract live under
[test/e2e](test/e2e/README.md). The Helm chart is the complete Kama installation path,
including the artifact plane and baseline-serving resources. `config/default` remains
a developer Kustomize bundle with
admission disabled until a downstream overlay provides webhook TLS.
