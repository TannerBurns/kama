# Kama Helm Chart

The chart installs the cluster-wide Kama manager and CRDs, namespaced artifact and
baseline-serving resources, RBAC, admission webhooks, and a release-owned webhook CA/certificate
Secret. Build the versioned package before installing from a source checkout so the
chart and default image tags are synchronized with `VERSION`:

```sh
make helm-package
helm upgrade --install kama "dist/kama-$(tr -d '\r\n' < VERSION).tgz" \
  --namespace kama-system \
  --create-namespace
```

The default repositories must already contain manager, importer, CPU runtime, and
CUDA runtime images tagged with that `VERSION`, as they do for a published release.
For an unreleased source checkout, push locally built images to a registry reachable
by the cluster and override the manager, importer, and `runtime.*.image`
repositories/tags when installing.

Important values:

| Value | Default | Purpose |
|---|---|---|
| `image.repository` | `ghcr.io/tannerburns/kama-manager` | Manager image |
| `importer.image.repository` | `ghcr.io/tannerburns/kama-importer` | Image used by short-lived import/probe Jobs |
| `importer.imagePullSecrets` | `[]` | Secret names copied to importer Jobs |
| `importer.hubEndpoint` | `https://huggingface.co` | Operator-wide Hugging Face endpoint; CRs cannot override it |
| `runtime.cpu.image.repository` | `ghcr.io/tannerburns/kama-runtime-cpu` | Linux amd64/arm64 llama.cpp CPU runtime |
| `runtime.cuda.image.repository` | `ghcr.io/tannerburns/kama-runtime-cuda` | Linux amd64 CUDA runtime |
| `runtime.cuda.runtimeClassName` | `""` | Optional RuntimeClass assigned only to accelerator serving Pods |
| `runtime.imagePullPolicy` | `IfNotPresent` | Pull policy copied to generated serving Pods |
| `runtime.imagePullSecrets` | `[]` | Secret names copied to generated serving Pods |
| `runtime.llamaCommit` | `b4d6c7d8ff69c2e05e4e8ee7e6e710a08abd7b45` | Exact llama.cpp build expected by the controller and runtime |
| `webhook.enabled` | `true` | Install and start fail-closed v1alpha1 admission |
| `webhook.tls.secretName` | `<fullname>-webhook-tls` | Helm-owned CA and leaf Secret |
| `minReadySeconds` | `10` | Keep a replacement manager ready before retiring the previous admission endpoint |

Empty image tags use the chart `appVersion`; a digest overrides its corresponding
tag. Runtime references are controller configuration and cannot be overridden by a
`ModelDeployment`. Set `runtime.cuda.runtimeClassName` when the cluster requires an
explicit container runtime handler for NVIDIA workloads; CPU serving Pods never use
this value. On upgrade, Helm reads the existing webhook Secret, preserves its CA, signs a
fresh serving leaf, updates both admission CA bundles, and rolls the manager. Keep
the Secret readable by Helm during upgrades. The manager uses a zero-unavailable
rolling update and a readiness hold so API-server webhook routing can converge before
the prior endpoint exits. Perform an upgrade before
`webhook.tls.certificateValidityDays` elapses because the chart has no autonomous leaf
renewal. For a planned CA rotation, delete the TLS Secret and immediately run a Helm
upgrade during a maintenance window; fail-closed admission may be briefly unavailable
until the new CA bundle and manager Pod are ready. cert-manager is not required.

Published releases attach `dist/release/immutable-values.json` to their release
evidence. Pass that file with `--values` for digest-pinned manager, importer, CPU
runtime, and CUDA runtime references; the release workflow validates every referenced
runtime manifest and binary before publishing the file. Source-checkout packages keep
version tags as development-friendly defaults.

## CRD-first upgrades

Helm installs files under `crds/` on first installation but does not upgrade or
delete existing CRDs. Before every Kama chart upgrade, apply the CRDs from the exact
chart package, wait for all three definitions to become established, and only then
upgrade the controller:

```sh
chart="dist/kama-$(tr -d '\r\n' < VERSION).tgz"
helm show crds "$chart" | kubectl apply --server-side \
  --field-manager=kama-crd-upgrade -f -
kubectl wait --for=condition=Established --timeout=2m \
  crd/modelcaches.kama.tannerburns.github.io \
  crd/modelartifacts.kama.tannerburns.github.io \
  crd/modeldeployments.kama.tannerburns.github.io
helm upgrade kama "$chart" --namespace kama-system --wait --timeout 5m
```

Use the same release package for both steps. Stop if server-side apply reports a
schema conflict; resolve the field owner or incompatible stored data before starting
the manager rollout. A chart rollback does not roll back or delete CRDs or existing
custom resources. Confirm the older manager remains compatible with the installed
schema before rolling back application components.

`rbac.create=false` disables both manager and leader-election RBAC. Supply equivalent
permissions before starting the chart. The manager intentionally has no permission
to read source Secrets; importer Jobs mount the named same-namespace Secret directly.

Helm does not delete CRDs on uninstall. `Retain` storage and adopted PVCs remain; see
the [recovery runbook](../../docs/runbooks/artifact-import-and-recovery.md) before
removing a production installation.
