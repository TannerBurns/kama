# ADR-0001: Project identity and Kubernetes API group

- Status: Accepted
- Date: 2026-07-18

## Context

Kama needs stable public identifiers before it publishes a Go module or Kubernetes
custom resources. The project plan originally used an unowned domain provisionally.
Changing a Go module path or Kubernetes API group after users adopt it creates source,
manifest, stored-object, and conversion compatibility costs.

## Decision

- The canonical public repository and Go module path are
  `github.com/TannerBurns/kama`.
- The permanent Kubernetes API group is `kama.tannerburns.github.io`.
- The first API version is reserved as `kama.tannerburns.github.io/v1alpha1`; M0 does
  not introduce any custom resources.
- Go package imports retain the GitHub owner's case exactly. Container and OCI paths
  use the lowercase registry namespace required by GHCR conventions.

## Consequences

- All examples, generated manifests, webhook certificates, RBAC rules, and stored
  resource identities must use the permanent API group.
- A future repository transfer or vanity domain does not silently change either
  public identifier. Such a change requires a replacement ADR and an explicit
  compatibility and migration plan.
- The longer API group is accepted in exchange for an identity tied to the project's
  owned GitHub namespace.

## Alternatives considered

- The original short provisional group was rejected because the project does not
  control its domain.
- A short-lived local or `example.com` group was rejected because it could escape
  into user manifests and require migration.
- Deferring the decision was rejected because module initialization and API
  scaffolding require stable identifiers in M0.
