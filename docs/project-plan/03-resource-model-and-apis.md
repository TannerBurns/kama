# Resource Model and APIs

## Outcome

Kama exposes compact, namespaced desired-state APIs. GGUF bytes, request queues,
transient metrics, and individual GPU IDs never belong in CRDs.

The permanent API group is `kama.tannerburns.github.io`, as recorded in
[ADR-0001](../adr/0001-project-identity.md). Initial resources use `v1alpha1` and
conversion-safe schemas.

## `ModelCache`

`ModelCache` identifies a durable storage pool and its lifecycle rules.

### Spec responsibilities

- Exactly one of `storage.existingClaim.name` or `storage.claimTemplate`.
- A managed template contains labels/annotations, storage class, access modes,
  filesystem volume mode, and `resources.requests.storage`.
- Layout: shared content-addressed cache by default; dedicated artifact claims may be
  added as a storage profile.
- `retentionPolicy: Retain|Delete`, defaulting to `Retain`. Adopted claims may use
  only `Retain`; `Delete` is limited to an unreferenced controller-created claim
  whose ownership identity matches the cache UID.
- Optional capacity watermarks and future node-cache policy.

### Status responsibilities

- Bound claim and volume identity, capacity, observed free-space report, access
  modes, volume/storage class, normalized PV node affinity, and mount validation.
- `Ready`, `StorageUnavailable`, `InsufficientCapacity`, and `Degraded` conditions.
- The controller never adopts ownership of a user-provided claim.

## `ModelArtifact`

`ModelArtifact` represents one immutable GGUF file or a verified set of GGUF shards.

### Source union

- `huggingFace`: repository, selected filenames or shard pattern, revision, and a
  same-namespace token Secret reference.
- `persistentVolumeClaim`: claim, relative path, and `Copy` or `Direct` import mode.

`Copy` is the safe default and publishes content into a `ModelCache`. `Direct` serves
an adopted claim in place and permanently uses `Retain` ownership semantics.

Hugging Face sources require a repository, revision, one or more file selectors, and
an optional same-namespace token Secret name/key. `cacheRef` is required for Hugging
Face and `Copy`, and forbidden for `Direct`. An artifact is limited to 128 selected
files so status and Job results stay bounded.

### Identity and verification

- Format and entrypoint.
- Optional expected size and SHA-256; policy may require the expected digest.
- Full resolved source revision, computed file digests, canonical manifest digest,
  size, architecture, quantization, and shard count in status.
- Source, storage, and verification fields become immutable when reconciliation
  starts, before importer work is created; a new revision, cache, or model is a new
  artifact. `expectedSHA256` is the file digest for one file and Kama's canonical
  content-manifest digest for a shard set; `expectedSize` is aggregate bytes.

### Conditions

`SourceResolved`, `StorageReady`, `Importing`, `Verified`, `Ready`, `InvalidGGUF`,
`ChecksumMismatch`, `MissingShard`, `InsufficientStorage`, and `SourceUnavailable`.

Status includes the immutable Hub commit, sorted file identities, artifact digest,
aggregate size, GGUF architecture/quantization/shard count, validation time, Job
reference, and the serving location contract. The location carries claim, subpath,
read-only requirement, access modes, mount scope, volume identity, and node affinity.
Before importer creation the controller persists that storage identity; during
deletion the status-only `cleanupOperationID` records validated transient cleanup and
is not part of the serving contract.

## `ModelDeployment`

`ModelDeployment` expresses service intent and references a ready `ModelArtifact`.

### Stable high-level fields

```yaml
spec:
  modelRef:
    name: llama-3-8b-q4
  placement:
    mode: Accelerator       # CPU or Accelerator in M2
    acceleratorResource: nvidia.com/gpu
  runtime:
    maxContextTokens: 8192
    desiredConcurrency: 4
    drainTimeout: 10m
    kvCache:
      keyType: f16
      valueType: f16
    expert:
      batchSize: 2048
      microBatchSize: 512
      flashAttention: Auto
  resources:
    requests:
      cpu: "4"
      memory: 16Gi
    limits:
      memory: 24Gi
```

M2 is an intentionally fixed serving slice. Placement is explicit `CPU` or
`Accelerator`; accelerator mode admits one full `nvidia.com/gpu`, and the controller
owns that resource request/limit. CPU and memory requests plus a memory limit are
required; a CPU limit is optional. Omitted context selects the model-advertised
native context and requires concurrency one. An explicit context is per request, and
Kama derives the exact total context for the declared concurrency. Context,
concurrency, KV precision, and expert values are hard constraints.

M3 adds `Auto`, hybrid/multi-GPU resolution, fitting, and profiling. M4 adds aliases,
routes, and multiple replicas. M5 adds autoscaling and ordered fallback. Those fields
are not admitted by the M2 schema.

### Guarded expert overrides

Expert fields may tune supported llama.cpp batching, thread, and flash-attention defaults,
but admission rejects flags that override artifact paths, HTTP binding, metrics and
slot endpoints, GPU visibility/count, split settings, RPC workers, or health probes.
The adapter schema is versioned against the pinned llama.cpp build.

The conventional `args`, `env`, `image`, `ports`, `paths`, `probes`, `topology`, and
`replicas` keys are reserved as always-invalid schema tombstones at the deployment and
runtime levels. Kubernetes otherwise prunes undeclared custom-resource fields before a
typed admission webhook can inspect them when field validation is not strict; reserving
these keys makes the protected-field contract fail closed during CRD-first upgrades too.

### Status responsibilities

- Artifact name, UID, digest, desired/observed runtime image, llama.cpp commit,
  desired/observed fingerprint, and a durable loaded-fingerprint checkpoint. The
  checkpoint proves that identity loaded previously; it never substitutes for a
  current readiness observation.
- Effective context/concurrency plus bounded accelerator/offloaded-layer facts.
- Generated Deployment and stable Service references and ready/desired replicas.
- `ArtifactReady`, `ResourcesAvailable`, `RuntimeReady`, `Serving`, and `Degraded`
  conditions. Requested CPU mode always reports `Degraded=True` with
  `CPUOnlyRequested` independently of serving readiness.

The M3-M5 resources extend status with estimates, profiles, resolved topology,
routes, usable capacity, queues, and autoscaling state without changing the meaning
of these baseline fields.

## `ModelProfile`

`ModelProfile` is controller-owned and normally read-only to users. It caches measured
load time, time to first token, token rate, memory high-water marks, and compatibility
for a fingerprint of:

- Artifact digest.
- llama.cpp image/build digest.
- GPU or CPU hardware class and device count.
- Runtime envelope and split mode.

Profiles prevent repeated benchmark Jobs and make automatic decisions explainable.

## Ownership and deletion

- A `ModelDeployment` owns its generated Deployments, Services, ConfigMaps, and
  ScaledObjects, but not its `ModelArtifact`.
- A `ModelArtifact` may reference a managed blob but does not own a shared
  `ModelCache` PVC.
- Adopted PVCs never receive owner references or automatic deletion.
- Deleting a deployment drains compute and routes only.
- Deleting an artifact that is still referenced is rejected or held by a finalizer.
- Managed blobs default to retained after the final reference. Explicit purge or an
  opted-in unreferenced-content policy is required for deletion.

## Reconciliation contract

Every status includes `observedGeneration`. Conditions follow normal Kubernetes
polarity and include actionable reason/message fields without credentials. Controllers
emit Events for state transitions, not for every poll.

See [storage](04-model-artifacts-and-storage.md) for artifact semantics and
[topology](05-runtime-topology-and-placement.md) for how intent resolves into Pods.
