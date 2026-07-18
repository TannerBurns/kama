# ADR-0002: Dependency and version policy

- Status: Accepted
- Date: 2026-07-18

## Context

Kama depends on a coupled Go, Kubernetes, controller-runtime, Kubebuilder, KEDA, and
packaging toolchain. Uncoordinated upgrades can produce an operator that compiles but
does not install or scale correctly across the supported Kubernetes window. M0 needs
a reproducible baseline and a clear rule for accepting updates.

## Decision

The M0 baseline is:

| Component | Version or range |
|---|---|
| Go | 1.26.5 |
| Kubebuilder | 4.15.0 |
| controller-runtime | 0.24.1 |
| Kubernetes Go libraries | 0.36.0 |
| Kubernetes clusters | 1.34, 1.35, and 1.36 |
| KEDA | 2.20.0 minimum |
| Helm | 4.2.0 |
| Kind | 0.32.0 |

Developer and CI tools are pinned in one machine-readable version manifest and
installed into the repository-local `bin/` directory. Builds and CI must consume
those pins instead of whichever tools happen to be installed globally.

Patch updates may be proposed through normal dependency automation and verification.
A Go, Kubernetes library, or controller-runtime minor update requires the complete
unit, envtest, image, Helm, and Kubernetes 1.34-1.36 Kind compatibility matrix before
merge. Changes to the supported Kubernetes window, minimum KEDA release, or an
incompatible major tool version also require an ADR update.

KEDA 2.20.0 is the minimum supported autoscaling release. Because its upstream
compatibility coverage stops before Kubernetes 1.36, Kama's KEDA activation test on
Kubernetes 1.36 is an explicit M0 project gate. A failure keeps M0 incomplete until a
compatible KEDA version is selected or the support policy is changed through an ADR.

## Consequences

- Dependency upgrades are evaluated as a compatibility set, not only as individual
  version bumps.
- Local development and CI use the same tool versions and commands.
- Kubernetes 1.36 support for KEDA is a claim backed by Kama's test evidence rather
  than inferred from upstream coverage.
- Pinning increases routine maintenance work but makes releases and failures
  reproducible.

## Alternatives considered

- Floating latest tool versions were rejected because they make generation and CI
  non-reproducible.
- Supporting whichever KEDA version is already installed was rejected because it
  prevents a meaningful compatibility contract.
- Dropping Kubernetes 1.36 from M0 was rejected because the selected support window
  explicitly includes it; the compatibility gate makes the risk visible.
