# ADR-0005: Baseline serving API and runtime contract

- Status: Accepted
- Date: 2026-07-20

## Context

M2 must turn a verified `ModelArtifact` into one predictable serving process without
preempting M3 placement or M4 routing design. Allowing arbitrary llama.cpp arguments,
automatic fit changes, or mutable upstream runtime tags would make the first serving
contract unsafe to operate and impossible to reproduce.

Kama also needs failures to remain inspectable. A permanently invalid model must not
cycle through container restarts, and a long model load must not be mistaken for a
dead supervisor.

## Decision

### API boundary

- `ModelDeployment` is namespaced and references a `ModelArtifact` by name in the
  same namespace.
- M2 requires explicit `placement.mode: CPU|Accelerator`. `Auto`, hybrid execution,
  multi-GPU, replica count, routes, profiling, fallback, and autoscaling remain in
  later milestones.
- Accelerator mode supports exactly one full `nvidia.com/gpu` device. The controller
  owns the extended-resource request and limit; users cannot place accelerator or
  other extended resources in `spec.resources`.
- CPU and memory requests plus a memory limit are required. A CPU limit is optional.
- Context, concurrency, KV types, and the typed expert fields are hard inputs. Kama
  never silently reduces them. An omitted context uses the model-native context and
  requires concurrency one. Concurrency greater than one requires an explicit
  per-request context.
- Defaults are concurrency one, `f16` key/value KV caches, batch 2048, micro-batch
  512, automatic flash attention, and a ten-minute drain timeout. Raw arguments,
  environment variables, paths, images, ports, probes, topology, and replica knobs
  are not configurable. Their conventional JSON keys are reserved as always-invalid
  schema tombstones so the API rejects them even with `fieldValidation=Warn` or
  `Ignore` and before a validating webhook is available during CRD-first upgrades.

### Reconciliation and lifecycle

- A stable internal ClusterIP Service exists while the resource exists. Kama creates
  a one-replica `Recreate` Deployment only after the referenced artifact reports an
  exact ready UID, digest, and read-only location.
- A fingerprinted immutable ConfigMap carries the supervisor configuration. The Pod
  mounts only the artifact location at `/models`, read-only, and preserves any PV
  node affinity reported by the artifact plane.
- Generated Pods run as UID/GID 65532 with a read-only root filesystem, dropped
  capabilities, `RuntimeDefault` seccomp, no privilege escalation, no API token, and
  a writable empty `/tmp`.
- Spec or artifact identity changes use drain-first singleton replacement. A
  transient loss of artifact readiness does not evict an already-loaded process when
  its recorded UID, digest, and location are unchanged, but it blocks new rollout.
- A `ModelDeployment` finalizer drains and removes its generated workload. A
  `ModelArtifact` remains in deletion finalization while any deployment object
  references it (including one still draining) or a generated Pod still mounts its
  exact claim/subpath, preventing mounted storage from being cleaned up early.

### Runtime boundary

- Kama builds `kama-runtime-cpu` for Linux amd64/arm64 and `kama-runtime-cuda` for
  Linux amd64 from llama.cpp release `b10091`, commit
  `b4d6c7d8ff69c2e05e4e8ee7e6e710a08abd7b45`. Both images share a Go supervisor and
  the Kama release version.
- The supervisor is PID 1 and starts `llama-server` once. It owns the model path,
  bind address, ports, diagnostics, device selection, split mode, offline mode, and
  fit behavior. It explicitly uses mmap and disables llama.cpp's independent RAM
  prompt cache and idle-slot cache so memory is not expanded by a runtime default.
  User intent is translated only through the typed API.
- Model-native context maps to `--ctx-size 0 --parallel 1`. Explicit per-request
  context `N` and concurrency `P` map to total context `N*P`, `--parallel P`, and
  partitioned KV state. `--fit off` prevents upstream from silently shrinking hard
  inputs.
- Port 8080 carries the native llama-server interface. Supervisor diagnostics on
  Pod-only port 8081 separate startup, liveness, readiness, and sanitized state.
- Readiness requires `/health`, `/slots`, and read-only `/props` to agree on the exact
  model path, slot/context contract, and pinned llama.cpp build. Accelerator readiness
  additionally requires exactly one visible CUDA device and all reported model layers
  offloaded; partial or hybrid offload cannot qualify.
- Every generated Pod starts with an artifact scheduling gate. The controller
  removes that gate only after an uncached API-server read confirms the current
  deployment generation, artifact UID/digest/location, and bound claim identity.
- Before launching the child, the supervisor checks every declared file's type,
  size, and SHA-256 against the controller-owned artifact manifest.
- Loading or terminal child failure leaves the supervisor responsive but unready and
  does not retry the child. Termination first rejects readiness, allows endpoint
  propagation, drains active slots to the declared timeout, and then terminates the
  child within a bounded hard deadline.

### Distribution and status

- The chart supplies both runtime image references, pull policy and Secrets, and the
  exact llama.cpp commit to the manager. Runtime image digests, SBOMs, signatures,
  and provenance are release artifacts alongside the existing images.
- Helm installs CRDs on a fresh release but does not own CRD upgrades. Operators
  server-side apply the release CRD bundle and wait for `Established` before running
  `helm upgrade`.
- Status records the artifact and runtime identities, fingerprint, durable
  loaded-fingerprint checkpoint, effective runtime envelope, generated resources,
  ready replicas, and the conditions
  `ArtifactReady`, `ResourcesAvailable`, `RuntimeReady`, `Serving`, and `Degraded`.
  Requested CPU mode is deliberately reported as `Degraded=True` with reason
  `CPUOnlyRequested`.

## Consequences

- M2 is deterministic and testable without inventing the later topology planner.
- One-replica `Recreate` updates have an expected serving gap; HA routing and surge
  decisions belong to M4.
- Explicit settings may fail to load instead of being reduced. Status and supervisor
  diagnostics make that failure stable and actionable.
- CUDA support claims require protected real-NVIDIA evidence. Presence of a CUDA
  image or workflow alone is not qualifying evidence.
- CRD schema upgrades are an explicit operator action and survive chart rollback or
  uninstall.

## Alternatives considered

- `Auto` placement in M2 was rejected because fit estimation and profiling are M3.
- Passing arbitrary llama.cpp flags was rejected because it could override storage,
  networking, diagnostics, device allocation, and hard runtime constraints.
- Restarting the server inside the supervisor was rejected because deterministic
  load failures would become an opaque retry loop.
- Using mutable upstream runtime images was rejected because source, compiler flags,
  supervisor behavior, provenance, and supported platforms must be one Kama-owned
  release contract.
