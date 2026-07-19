# Artifact Import and Storage Recovery

## First response

Work in the artifact's namespace and inspect the resource, its generated Job, and
recent Events. Condition messages are sanitized; source credentials and response
bodies are intentionally absent.

```sh
namespace=my-models
artifact=my-model
kubectl -n "$namespace" describe modelartifact "$artifact"
kubectl -n "$namespace" get events \
  --field-selector "involvedObject.kind=ModelArtifact,involvedObject.name=$artifact" \
  --sort-by=.lastTimestamp
kubectl -n "$namespace" get jobs,pods \
  -l "kama.tannerburns.github.io/model-artifact=$artifact"
```

Do not edit files under `blobs/sha256`, manufacture a `READY` file, or manually move
staging directories into the published tree. A valid manifest and `READY` marker are
the publication boundary.

## Unauthorized or unavailable source

`SourceUnavailable=True` with an authorization reason normally means the token is
missing, stored under the wrong key, expired, or lacks access to the gated repository.

1. Confirm `spec.source.huggingFace.tokenSecretRef` names a Secret and key in the
   artifact's namespace. Cross-namespace Secret references are not supported.
2. List keys without printing values:

   ```sh
   kubectl -n "$namespace" get secret TOKEN_SECRET \
     -o go-template='{{range $key, $_ := .data}}{{printf "%s\n" $key}}{{end}}'
   ```

3. Replace the token in that Secret, then delete only the failed importer Job. The
   controller recreates the deterministic Job and resumes validated partial data.
4. If Hugging Face reports a gated or nonexistent revision, create a replacement
   artifact with a repository revision the token may read. Ready source identity is
   immutable.

Never put a token in a manifest, status annotation, shell history, or support bundle.

## Checksum, GGUF, or missing-shard failure

- `ChecksumMismatch=True` means the downloaded bytes disagree with
  `verification.expectedSHA256` or `expectedSize`.
- `InvalidGGUF=True` means the entrypoint is not a supported GGUF v3 regular file or
  its required metadata is malformed.
- `MissingShard=True` means a standard `-00001-of-000NN.gguf` set is incomplete.

Verify the repository revision, selected `files`, `entrypoint`, expected aggregate
size, and digest source. A one-file expected SHA-256 is that file's digest; a shard
set uses Kama's canonical manifest digest, not a concatenation of file bytes. Correct
the source or create a replacement artifact with correct assertions. Failed staging
is never served and an existing ready digest is not overwritten.

## Cache full or storage probe failed

Inspect both provisioned capacity and the filesystem free-space value reported by the
latest probe:

```sh
kubectl -n "$namespace" describe modelcache CACHE_NAME
kubectl -n "$namespace" get pvc
```

For `InsufficientCapacity` or `InsufficientStorage`:

1. Stop submitting imports to the affected cache.
2. Expand the PVC only when its StorageClass supports expansion, or create a larger
   `ModelCache` and point a replacement artifact at it.
3. Wait for the cache probe to report `Ready=True` before retrying a failed Job.

M1 has no verified-blob garbage collection or supported manual purge command. Do not
delete unknown content-addressed directories to recover space. Deleting a cached
`ModelArtifact` stops its importer Jobs and runs an identity-bound cleanup Job that
removes only that artifact UID's staging, intent, revision-pin, operation-marker, and
lock state; it never removes `blobs/sha256` content. Outside that finalizer flow,
preserve `.kama/staging`, `.kama/operations`, and `.kama/locks` state so retries can
classify and resume it safely.

## Interrupted staging or manager restart

Importer work is idempotent. The controller reconstructs progress from the Lease,
Job, result, manifest, and `READY` marker. After a manager or Job restart:

1. Leave a valid final publication untouched.
2. Confirm no old Job is still running before deleting a failed Job.
3. Allow the controller to recreate the deterministic Job. It validates an existing
   publication first, then resumes range-capable downloads using matching metadata.
4. Escalate if two Jobs remain active for the same artifact fingerprint or if a Job
   repeatedly exits without a sanitized result.

Deleting a failed Job does not delete a cache PVC or verified blob.

Deleting a cached `ModelArtifact` can remain in `Terminating` while its scoped cleanup
Job runs. Do not remove its finalizer or delete the cleanup Job unless diagnosing a
documented failure: the detached Job must finish and be removed before the controller
releases the artifact. Kama checkpoints the exact claim/PV identity in
`status.location` before creating importer resources and writes
`status.cleanupOperationID` only after validating a successful cleanup result. Do not
edit either status field to force finalization. `Direct` artifacts skip this cache
cleanup and retain their adopted source claim.

## Retained and adopted claims

`Retain` is the default. Adopted claims are always retained and never owned by Kama.
Before removing Kama, list claims referenced by caches and artifacts and record PV
reclaim policy, StorageClass, capacity, and volume identity:

```sh
kubectl get modelcaches,modelartifacts -A
kubectl get pvc -A
kubectl get pv
```

Helm uninstall removes the controller and admission resources, not CRDs or retained
model storage. A managed cache with `retentionPolicy: Delete` is eligible for claim
deletion only when it is unreferenced and its ownership labels still match the cache
UID. Change the policy to `Retain` before planned control-plane removal when the data
must survive.

Before deleting an eligible managed claim, the controller adds the
`kama.tannerburns.github.io/cache-deletion-guard` annotation and waits for
reconciliation to quiesce. New artifact resolution treats a guarded claim as
unavailable, including when it is selected as a PVC `Copy` or `Direct` source. If
deletion stalls, inspect `ModelCache` and `ModelArtifact` references and their Events;
do not remove the guard or the cache finalizer to force deletion.

## Snapshot and restore

Use the CSI driver's `VolumeSnapshot` support for irreplaceable manual models. Quiesce
writes, snapshot the source or cache PVC, and record the artifact manifest digest with
the snapshot. Restore into a new PVC rather than overwriting a mounted claim, validate
the restored files with a new PVC `Direct` artifact, and switch consumers only after
it reaches `Ready=True`.

Snapshot APIs and consistency guarantees are CSI-specific. Follow the storage
provider's restore procedure and retain the original claim until validation succeeds.

## PVC Direct: `ValidatedOnce`

Direct mode avoids copying bytes but continues serving the adopted path. M1 does not
periodically scrub it. After `Ready=True`:

- do not replace, edit, truncate, relink, or add shards under the validated root;
- mount the claim read-only wherever possible;
- treat any out-of-band mutation as loss of the recorded integrity guarantee;
- after an intentional change, create a new artifact and run validation again;
- after suspected corruption, stop new serving use, restore a snapshot or known-good
  copy, and validate it as a new artifact.

The original status is evidence of validation at `status.validatedAt`, not proof that
mutable external storage has remained unchanged since then.
