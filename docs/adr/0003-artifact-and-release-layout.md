# ADR-0003: Artifact and release layout

- Status: Accepted
- Date: 2026-07-18

## Context

Kama needs one version identity across binaries, images, Helm packages, labels, SBOMs,
and signed release artifacts. Independently supplied version flags or mutable image
tags would allow an apparently coherent release to contain mismatched components.

## Decision

- The tracked root `VERSION` file is the sole release-version source. Its initial
  value is `0.1.0-dev.0`.
- The manager image is `ghcr.io/tannerburns/kama-manager`.
- The M1 importer image is `ghcr.io/tannerburns/kama-importer` and follows the same
  release version as the manager.
- The nonproduction fixture image is
  `ghcr.io/tannerburns/kama-test-fixtures` and is never part of the supported runtime
  contract.
- Future independently deployed component images use
  `ghcr.io/tannerburns/kama-<component>`.
- The Helm chart is named `kama` and is published at
  `oci://ghcr.io/tannerburns/charts/kama`.
- Packaging derives the chart `version`, chart `appVersion`, default manager image
  tag, binary version, and OCI image labels from `VERSION`. The chart defaults the
  manager tag to `.Chart.AppVersion` while permitting an explicit tag or digest for
  controlled deployments.
- Release tags use `v${VERSION}`. A release workflow rejects a tag that does not
  exactly match the tracked version.
- Published manager, importer, and fixture images are multi-platform for
  `linux/amd64` and `linux/arm64`, run as non-root, and use digest-pinned base images.
- Release publication signs immutable image and chart digests using Cosign keyless
  signing and attaches Syft-generated SPDX SBOM attestations. M0 requires working
  hooks and validation; it does not require publishing a release.

## Consequences

- Changing `VERSION` is the intentional entry point for a release version change;
  generated package metadata must not become a competing source of truth.
- CI can reject inconsistencies by comparing binary output, image labels, image tags,
  and the packaged chart against the same value.
- Consumers can pin immutable digests even though human-readable version tags are
  also published.
- Fixture artifacts remain clearly separated from supported production images.

## Alternatives considered

- Deriving versions from Git alone was rejected because source archives and local
  packaging may not contain repository metadata.
- Maintaining independent chart, image, and binary versions was rejected because it
  permits mismatched releases.
- Publishing fixtures beside the manager under ambiguous tags was rejected because
  test-only behavior must not be mistaken for a production component.
