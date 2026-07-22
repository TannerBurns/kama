# End-to-End Test Suites

These suites install Kama's packaged chart and container images into disposable
two-node Kind clusters, then exercise the system through Kubernetes and external
service boundaries. They are separate from `test/integration`, which uses envtest
for in-process controller integration.

## Current suites

| Suite | Fixtures | Entrypoint | Evidence |
|---|---|---|---|
| Artifact-plane storage resilience | `test/e2e/storage` | `make test-e2e-storage` | `dist/e2e/storage` |
| Artifact-plane Hugging Face import | `test/e2e/huggingface` | `make test-e2e-huggingface` | `dist/e2e/huggingface` |
| Baseline CPU serving | Pinned public SmolLM2 and production CPU runtime | `make test-e2e-serving-cpu` | `dist/e2e/serving-cpu` |
| Protected NVIDIA serving | Dedicated NVIDIA cluster and production CUDA runtime | `make test-e2e-serving-nvidia` | `dist/e2e/serving-nvidia` |

The storage suite is deterministic and uses the repository's fake Hub plus an
in-runner NFS service. The Hugging Face suite always imports the pinned public
SmolLM2 GGUF and optionally exercises a protected private repository. Trusted pushes,
scheduled runs, and ordinary manual dispatches produce public-only regression
evidence. A manual dispatch with `require_private_hf=true` explicitly enables the
fail-closed private-source follow-up.

GitHub runs both suites as parallel jobs in
[the end-to-end workflow](../../.github/workflows/e2e.yml). The protected
Hugging Face inputs are scoped to the `e2e-huggingface` environment.

CPU serving runs on hosted Linux runners and owns a disposable Kind cluster. The
`Kind compatibility` workflow executes the full CPU suite for Kubernetes 1.34,
1.35, and 1.36. Its stable `M2 CPU/Kind acceptance` job succeeds only when every
matrix entry and its evidence verification succeed, so branch protection does not
need to track matrix-generated check names. The standalone end-to-end workflow also
runs a Kubernetes 1.36 CPU regression.

The CPU production images are built with Buildx, identified and verified by their
manifest digests and OCI labels, then loaded into each isolated cluster under
run-local tags with `imagePullPolicy: Never`. Those tags are transport aliases, not
published image identities. NVIDIA serving qualifies either from trusted `main`
history on `[self-hosted, Linux, X64, kama-nvidia]` with a protected kubeconfig, or
through the same strict suite in a trusted local operator environment when no
protected NVIDIA runner is available. Local orchestration and credentials remain
outside the repository; `preinstalled` mode can verify a separately installed signed
controller without owning cluster-scoped resources. Pull requests never receive
either kubeconfig, and the NVIDIA workflow is manual-only. Both serving suites use a
genuinely loadable model and production runtime images; the synthetic GGUF and fake
llama-server remain nonproduction controller fixtures.

Preinstalled mode requires an existing isolated namespace and refuses to continue if
any fixed-name test resource is present. Resources created by a run carry a random
ownership label; cleanup verifies that label and the observed UID before deleting
from a namespace the suite does not own. Trusted local operators must fetch
`origin/main` immediately before running; the suite rejects dirty source and commits
that are not ancestors of the fetched remote-tracking ref before it contacts the
cluster.

Suite-owned mode applies the same UID precondition to its run-labeled namespace,
tracks partially attempted Helm installs, and refuses namespace teardown when release
cleanup, fixed-resource deletion, or the residual-resource scan cannot be proved.
Qualifying evidence records `cleanupComplete=true`; an ownership mismatch preserves
the replacement resource for manual recovery and fails the gate.

`KEEP_NVIDIA_RESOURCES=1` is a diagnostic-only escape hatch. A run that retains
resources exits nonzero and writes explicitly non-qualifying evidence.

An operator may set `E2E_NVIDIA_EXISTING_CACHE_CLAIM` only with preinstalled mode and
an existing namespace. This narrow seam supports an out-of-tree bootstrap that binds a
disposable RWO claim before the suite runs, including on delayed-binding node-local
storage. The suite requires the named claim and its PV to be Bound, Filesystem, RWO,
unowned, unguarded, unused by active Pods or Jobs, and backed by the exact configured
StorageClass. It then adopts the claim through `ModelCache.spec.storage.existingClaim`
with `Retain`, verifies the cache and artifact preserve the prevalidated claim/PV
identity, and proves the claim still exists after gate-owned resources are removed.
Sanitized evidence cross-checks the ModelCache, ModelArtifact, PVC, and PV UIDs,
binding, StorageClass, access mode, and read-only serving location.
The suite never deletes adopted storage; the out-of-tree operator owns its eventual
cleanup after Kama finalizers and consumers are gone.

Clusters that require an explicit Kubernetes RuntimeClass for NVIDIA containers set
`E2E_NVIDIA_RUNTIME_CLASS` to its name. The suite verifies that the RuntimeClass
exists before mutation, configures it through `runtime.cuda.runtimeClassName` for a
suite-owned controller, or verifies the exact `--runtime-cuda-runtime-class` manager
argument in preinstalled mode. Qualifying evidence requires both the generated
Deployment and the running inference Pod to use that exact RuntimeClass. Leave the
variable empty only when the cluster's default container runtime exposes allocated
NVIDIA devices.

The NVIDIA runner should be ephemeral; if that is not possible, its runner group and
cluster credentials must be limited to this repository. Configure the protected
`e2e-nvidia` environment with required reviewers and a `main`-only deployment branch,
and make the stable `Protected NVIDIA acceptance gate` check required for M2
promotion. A hosted validation job deliberately fails a manual dispatch from a
non-`main` ref. The GPU job checks out the immutable
`E2E_NVIDIA_EXPECTED_COMMIT` rather than the moving workflow SHA, fetches full
history, and accepts it only when it is an ancestor of current `origin/main`; this
lets manual validation exercise the published images' exact source commit without
allowing stale or untrusted branch code. Its protected kubeconfig is written to a
run-specific file with mode `0600` and removed in an `always()` cleanup step. The
protected cluster API server must be in the supported Kubernetes 1.34–1.36 range.

The serving data-path check runs Kama's static client in a bounded in-cluster Job and
requires a valid OpenAI-compatible SSE chunk containing nonempty generated assistant
content followed by `data: [DONE]` through the generated ClusterIP Service DNS. Role,
usage, and other metadata-only chunks are accepted but do not satisfy the generated
content assertion. Pod port-forwarding is reserved for supervisor diagnostics and
the CPU suite's active drain orchestration; it is not ClusterIP reachability evidence.

Serving evidence is qualifying only when every image used for the run has an
immutable manifest digest and OCI source/revision labels matching the checked-out
commit. In addition, the protected NVIDIA lane accepts only the canonical Kama GHCR
repositories at immutable digests and verifies each image's keyless release signature
and SPDX JSON attestation with cosign, bound to the trusted release workflow and
qualification commit. It queries the allocated container for its actual device and
driver, reads the installed CUDA version, checks the installed CUDA runtime package,
and verifies that `llama-server` links to CUDA runtime and BLAS libraries. Supplied
labels are corroborating provenance, not hardware/runtime evidence. The suite also
verifies status against the exact artifact, workload, Service, Pod, EndpointSlice,
runtime image, and fingerprint identities and retains sanitized snapshots. Missing
OCI inspection, signature, attestation, CUDA, or linkage evidence leaves a functional
run explicitly `qualifying=false` rather than converting an unverifiable run into
acceptance evidence.

Both serving workflows run a shared, strict cross-file verifier after the suite:

```sh
make verify-e2e-serving-cpu-evidence K8S_MINOR=1.36
make verify-e2e-serving-nvidia-evidence
```

The targets validate the complete retained evidence set, including qualification,
source/image identity, Kubernetes version, status/object identity, readiness,
streaming, failure, drain, restart, redaction, and mode-specific runtime facts. The
CPU suite also changes an already-serving deployment to a different, existing but
not-ready artifact identity, proves the old workload and endpoint remain absent,
then restores the original identity and requires inference from a new Pod behind the
same Service. It retains separate pre-transition and post-recovery request records,
including `direct-request.{log,json}`, `direct-request-job.json`,
`recovery-request.{log,json}`, and `recovery-request-job.json`, plus Kubernetes
object summaries immediately after drain and at the stability recheck. A suite exit
code alone is therefore insufficient to satisfy an M2 acceptance gate.

## Suite contract

Every suite added here should:

- own an isolated cluster or namespace and clean it unless an explicit keep flag is
  set;
- pin Kubernetes, third-party services, images, and external artifacts;
- exercise published charts and images rather than bypassing deployment boundaries;
- upload useful evidence even when a scenario fails;
- emit a machine-readable result containing `schemaVersion`, `suite`, and `outcome`;
- keep credentials, authorization headers, Secret values, and response bodies out of
  logs and uploaded artifacts; and
- fail on missing evidence instead of reporting a false pass.

## Adding a suite

Add fixtures below `test/e2e/<suite>`, an executable `hack/test-e2e-<suite>.sh`
entrypoint, a focused `make test-e2e-<suite>` target, and a parallel job in
`.github/workflows/e2e.yml`. Extend that workflow's path filters when the suite adds
new source areas. Use `dist/e2e/<suite>` and
`e2e-<suite>-${run_id}-${run_attempt}` for evidence so future serving and full-stack
suites can follow the same layout.

Milestone documents may consume an E2E suite as acceptance evidence, but suite names
and runtime resources must describe the behavior under test rather than a milestone
number.
