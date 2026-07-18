# Model Artifacts and Storage

## Outcome

Model persistence is a first-class subsystem. Each immutable GGUF is downloaded or
adopted once, verified, and retained independently of all inference workloads. A Pod
restart, horizontal scale-out, or scale-to-zero must not contact the model source.

```text
Hugging Face --+
Manual PVC ----+--> import/validation Job --> durable verified cache
                                              |
                                              +--> read-only serving mount
                                              +--> future node-local acceleration cache
```

## V1 storage model

The default `ModelCache` is one namespaced, content-addressed filesystem PVC shared
by artifacts. A cluster may configure multiple caches for different storage classes
or isolation requirements.

```text
/cache/
  .kama/staging/<artifact-uid>/<attempt>/
  blobs/sha256/<artifact-digest>/
    manifest.json
    READY
    model.gguf
```

- Use `volumeMode: Filesystem`; llama-server consumes regular file paths.
- Use a POSIX-like CSI filesystem with acceptable regular-file `mmap` behavior.
- Use RWX for a managed cache that must serve replicas on several nodes.
- Mount serving containers with `readOnly: true` and recursive read-only behavior
  where available. PVC access modes alone do not enforce in-container permissions.
- Keep the cache PVC and its underlying storage on `Retain` semantics by default.
- A shared cache PVC is never owned by a `ModelArtifact` or serving Deployment.

### Access-mode consequences

| Claim mode | Kama behavior |
|---|---|
| RWX | Importer writes; ready replicas may read from multiple nodes |
| ROX | Valid for pre-populated direct serving; Kama cannot populate it |
| RWO | Import and serving are limited to one node, although several Pods on that node may mount it |
| RWOP | Useful for an exclusive writer, not for shared serving |

Kama validates access mode, PV node affinity, volume binding, and requested serving
topology before creating workloads. It reports an incompatible combination instead
of leaving unexplained pending Pods.

## Hugging Face import

1. Resolve a tag or branch once to a full commit. A user-supplied full commit is
   preferred. Ready artifacts never follow a moving branch.
2. Determine selected file sizes before download when the source API supports it.
3. Acquire a digest/source-fingerprint Lease and create one deterministic import Job.
4. Mount the destination cache read-write and the token Secret only in the importer.
5. Download only selected GGUF files and required standard shards into staging.
6. Resume partial transfers where supported; never expose partial content to serving
   Pods.
7. Verify expected size/digest when supplied, always compute SHA-256, parse GGUF
   metadata, and validate shard completeness.
8. Write a canonical sorted manifest of relative path, size, and digest. The artifact
   digest is the SHA-256 of that manifest for multi-file content.
9. `fsync`, atomically rename within the same filesystem, and write `READY` last.
10. Mark the artifact ready and remove source credentials with the Job.

Import logic is idempotent because Kubernetes does not guarantee exactly-once Job
execution. A retry first validates an existing final manifest, then resumes or cleans
only its own stale staging attempt.

## Manual PVC import

Users may populate a same-namespace PVC using their storage tooling, an uploader Pod,
or a future `kama model import` command, then create a `ModelArtifact`.

```yaml
apiVersion: kama.tannerburns.github.io/v1alpha1
kind: ModelArtifact
metadata:
  name: manual-llama
spec:
  format: GGUF
  entrypoint: llama/model-00001-of-00004.gguf
  source:
    persistentVolumeClaim:
      claimName: manually-loaded-models
      rootPath: models
      importPolicy: Copy
  cacheRef:
    name: default
  verification:
    expectedSHA256: optional
```

### `Copy` mode

- Default and safest behavior.
- A Job mounts the source read-only, copies it to cache staging, and publishes the
  verified content-addressed artifact.
- The serving lifecycle no longer depends on the source claim, its access mode, or
  future user mutation.

### `Direct` mode

- Avoids a second copy for very large, already durable artifacts.
- A validation Job mounts the source read-only and records the canonical manifest.
- Serving Pods continue to mount that exact claim and path read-only.
- The claim is always unmanaged and retained. Users must not mutate the validated
  path; periodic integrity scrubbing is a later enhancement.
- RWO/PV node affinity becomes a hard topology-planner input.

Only relative paths are accepted. Validation rejects `..`, symlink escapes, devices,
sockets, missing shards, non-regular files, and entrypoints outside the validated
root.

## Serving integration

- `ModelDeployment` waits for `ModelArtifact Ready=True`.
- The resolved PVC subpath is mounted read-only at a stable runtime path.
- There is no Internet downloader init container in a serving Pod.
- A lightweight init gate may validate `READY` and the manifest digest without
  rehashing the full artifact on every start.
- Scale-to-zero deletes only compute resources.
- Artifact unavailability makes new replicas unready and produces an actionable
  deployment condition; existing loaded processes are not killed solely by a
  transient control-plane check.

## Retention, capacity, and recovery

- `Retain` is the default for both managed and adopted storage.
- Deleting a referenced artifact is blocked. Deleting an unreferenced artifact
  releases its logical reference but does not erase bytes unless explicit purge or
  an opted-in GC policy requests it.
- Automatic GC, when added, deletes only unreferenced verified blobs and expired
  staging attempts. It uses high/low watermarks and never deletes adopted PVC data.
- A cache agent/importer reports filesystem free space because PVC status reports
  provisioned capacity, not current filesystem usage.
- CSI `VolumeSnapshot` backup is recommended for manually supplied models that have
  no reproducible remote source.
- On manager restart, reconciliation trusts only a valid final manifest plus `READY`;
  it never treats an incomplete directory as an artifact.

## Performance tiers

GGUF is mmap-oriented, so shared-filesystem latency and readahead affect model load
and CPU-backed inference.

1. **V1 shared:** serve directly from the durable RWX cache and benchmark supported
   storage classes.
2. **Node-local cache:** copy each digest once from the durable cache to persistent
   node-local NVMe. The planner prefers warm compatible nodes; local bytes are never
   the only durable copy.
3. **CSI clone/snapshot:** create block-backed RWO serving copies where a CSI driver
   supports efficient cloning and RWX performance is insufficient.
4. **OCI image volume:** add an immutable registry source for Kubernetes 1.36+ after
   runtime and large-artifact behavior are validated.

## Status and metrics

Conditions include `SourceResolved`, `Importing`, `StorageReady`, `Verified`, `Ready`,
`ChecksumMismatch`, `InvalidGGUF`, `MissingShard`, `InsufficientStorage`, and
`SourceUnavailable`.

Metrics include artifact readiness/size, bytes transferred, import duration,
attempts/failures by reason, validation duration, cache capacity, and cache hits.
Status and logs never contain access tokens, signed URLs, or sensitive authorization
responses.

## V1 work packages

- `ModelCache` and `ModelArtifact` types, validation, conditions, and lifecycle.
- Importer image with Hugging Face and PVC adapters.
- Immutable manifest format and GGUF/shard inspector.
- Lease-based idempotency, resume, atomic publication, and retry recovery.
- Read-only serving mounts and access-mode-aware placement inputs.
- Retention defaults, Events, metrics, and manual-PVC documentation.

## Acceptance criteria

- The same immutable Hugging Face revision is downloaded once across repeated Job and
  Pod restarts.
- Concurrent duplicate imports publish one verified artifact without corruption.
- Checksum mismatch or a missing shard never creates `Ready=True`.
- Two cross-node replicas reuse one RWX artifact.
- A direct RWO model is limited to compatible nodes and reports why.
- Scale-to-zero and deployment deletion preserve the artifact.
- Adopted PVCs never receive owner references or automatic deletion.
- Failed staging content can be resumed or safely collected without touching ready
  content.

## References

- [Kubernetes PersistentVolume access modes and retention](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)
- [Kubernetes Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
- [Hugging Face downloads](https://huggingface.co/docs/huggingface_hub/guides/download)
- [Hugging Face cache design](https://huggingface.co/docs/huggingface_hub/main/guides/manage-cache)
- [GGUF specification](https://github.com/ggml-org/ggml/blob/master/docs/gguf.md)
- [llama.cpp GGUF splitting](https://github.com/ggml-org/llama.cpp/blob/master/tools/gguf-split/README.md)
- [Kubernetes VolumeSnapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/)
- [Kubernetes PVC cloning](https://kubernetes.io/docs/concepts/storage/volume-pvc-datasource/)
