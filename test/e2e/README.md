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

The storage suite is deterministic and uses the repository's fake Hub plus an
in-runner NFS service. The Hugging Face suite always imports the pinned public
SmolLM2 GGUF and optionally exercises a protected private repository. Scheduled and
manual runs require that private lane; trusted pushes may produce public-only
regression evidence.

GitHub runs both suites as parallel jobs in
[the end-to-end workflow](../../.github/workflows/e2e.yml). The protected
Hugging Face inputs are scoped to the `e2e-huggingface` environment.

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
