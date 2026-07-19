# ADR-0004: Persistent artifact plane contract

- Status: Accepted
- Date: 2026-07-18

## Context

M1 introduces Kama's first public resources and the storage boundary that every later
serving controller will consume. The contract must keep model bytes durable across
manager, Job, and serving-Pod restarts without allowing a namespaced object to reach
another namespace or allowing chart removal to destroy adopted data.

The admission server also becomes a required installation dependency. Requiring an
unrelated certificate controller for the first two APIs would make the default chart
incomplete, while a regenerated CA on every upgrade would transiently break all
admission requests.

## Decision

### API and tenancy

- `ModelCache` and `ModelArtifact` are namespaced resources in
  `kama.tannerburns.github.io/v1alpha1`.
- References to PVCs, Secrets, and Kama resources are local object names. There is no
  namespace field and no cross-namespace reference in v1alpha1.
- One manager installation watches these resources across all namespaces. Kubernetes
  RBAC controls who may create them; admission and reconciliation enforce Kama's
  same-namespace data model.
- `ModelCache.spec.storage` contains exactly one of `existingClaim` or
  `claimTemplate`. Managed templates expose labels, annotations, storage class,
  access modes, filesystem volume mode, and a storage request.
- `ModelArtifact.spec.source` contains exactly one Hugging Face or PVC source.
  Hugging Face and PVC `Copy` require `cacheRef`; PVC `Direct` forbids it. `Copy` is
  the default PVC import policy.
- Source, storage, and content identity fields become immutable when reconciliation
  starts, before an importer Job can be created. This binds retry and cleanup state
  to one cache/source identity; a different revision, cache, or model is represented
  by a new artifact.

### Storage and retention

- M1 implements one shared content-addressed filesystem PVC per `ModelCache`.
  Dedicated per-artifact managed claims are deferred; serving-local copies and
  automatic garbage collection are not part of M1.
- Managed publications use a stable `.kama/staging/<operation-id>` directory, an
  operation lock at `.kama/locks/<operation-id>.lock`, and durable recovery metadata
  at `.kama/operations/<operation-id>.json`. Publication atomically renames within
  the same filesystem into `blobs/sha256/<publication-digest>` and writes `READY`
  last. The publication digest is the canonical manifest SHA-256 and therefore binds
  filenames and entrypoint; the API artifact digest remains the file SHA-256 for a
  single-file artifact. Only a valid manifest plus `READY` is published content.
- `Retain` is the default for managed and adopted storage. An adopted claim may not
  select `Delete` and never receives an owner reference.
- `Delete` may remove only an unreferenced claim created for that `ModelCache` and
  carrying the expected cache-UID ownership labels. It does not delete verified
  blobs independently.
- Before creating importer resources, `ModelArtifact` status checkpoints the exact
  claim/PV identity used by deletion cleanup. A successful identity-scoped cleanup
  is recorded through the protected status subresource before detached resources and
  the artifact finalizer are removed.
- PVC `Direct` is a `ValidatedOnce` contract: Kama records the verified identity and
  placement of the adopted path, but M1 does not periodically rehash it. Mutating
  that path invalidates the user's guarantee.

### Placement contract

- Artifact status is the sole serving-facing storage contract. It reports the claim,
  subpath, read-only requirement, access modes, volume identity, normalized mount
  scope, and PV node affinity.
- RWX and direct ROX are multi-node-capable, RWO is node-constrained, and RWOP is
  single-Pod-constrained. ROX is never treated as a writable cache.
- Filesystem storage must support regular-file `mmap`, durable file and directory
  `fsync`, same-filesystem atomic rename, read-only remount, free-space reporting,
  and restart recovery. Storage throughput is qualified per CSI implementation; M1
  establishes no universal performance SLA.

### Admission TLS

- The Helm chart owns a release-scoped CA and serving-certificate Secret. On first
  install Helm creates the CA and leaf certificate. On upgrade it reuses the stored
  CA, refreshes the leaf certificate, injects the matching CA bundle, and rolls the
  manager Pod so it reads the new Secret.
- Mutating and validating webhooks use `failurePolicy: Fail`. The certificate covers
  the release Service's cluster DNS names. cert-manager is not required.

## Consequences

- M2 can mount a ready artifact without resolving storage semantics again.
- A cluster-wide manager needs read/write access to artifact control resources and
  generated Jobs across namespaces, but it does not need permission to read Secrets;
  token Secret names are passed directly to short-lived Jobs.
- Shared caches reduce duplicate storage and downloads, but quota, explicit purge,
  automatic GC, and per-artifact isolation require later API work.
- Helm must retain access to the release Secret during upgrade. Deleting that Secret
  intentionally rotates the CA and requires a successful Helm upgrade before
  admission becomes available again.
- Direct-mode users own ongoing immutability and backup of their adopted data until a
  later integrity-scrubbing capability is introduced.

## Alternatives considered

- A manager per watched namespace was rejected because it duplicates control-plane
  state and makes shared platform operation harder without improving Kubernetes RBAC
  isolation.
- Dedicated managed PVCs for every artifact were deferred because lifecycle and CSI
  clone behavior are not yet justified by acceptance evidence.
- cert-manager-only webhook certificates were rejected as a mandatory dependency;
  operators may still place a certificate manager in front of future integrations.
- Automatically deleting retained content or adopted claims was rejected because a
  control-plane uninstall must not become a model-data deletion operation.
