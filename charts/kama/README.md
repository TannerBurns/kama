# Kama Helm Chart

The chart installs the cluster-wide Kama manager and CRDs, namespaced M1 custom
resources, RBAC, admission webhooks, and a release-owned webhook CA/certificate
Secret. Build the versioned package before installing from a source checkout so the
chart and default image tags are synchronized with `VERSION`:

```sh
make helm-package
helm upgrade --install kama "dist/kama-$(tr -d '\r\n' < VERSION).tgz" \
  --namespace kama-system \
  --create-namespace
```

The default repositories must already contain manager and importer images tagged
with that `VERSION`, as they do for a published release. For an unreleased source
checkout, push locally built images to a registry reachable by the cluster and
override `image.repository`, `image.tag`, `importer.image.repository`, and
`importer.image.tag` when installing.

Important values:

| Value | Default | Purpose |
|---|---|---|
| `image.repository` | `ghcr.io/tannerburns/kama-manager` | Manager image |
| `importer.image.repository` | `ghcr.io/tannerburns/kama-importer` | Image used by short-lived import/probe Jobs |
| `importer.imagePullSecrets` | `[]` | Secret names copied to importer Jobs |
| `importer.hubEndpoint` | `https://huggingface.co` | Operator-wide Hugging Face endpoint; CRs cannot override it |
| `webhook.enabled` | `true` | Install and start fail-closed v1alpha1 admission |
| `webhook.tls.secretName` | `<fullname>-webhook-tls` | Helm-owned CA and leaf Secret |

Empty image tags use the chart `appVersion`; a digest overrides its corresponding
tag. On upgrade, Helm reads the existing webhook Secret, preserves its CA, signs a
fresh serving leaf, updates both admission CA bundles, and rolls the manager. Keep
the Secret readable by Helm during upgrades, and perform an upgrade before
`webhook.tls.certificateValidityDays` elapses because M1 has no autonomous leaf
renewal. For a planned CA rotation, delete the TLS Secret and immediately run a Helm
upgrade during a maintenance window; fail-closed admission may be briefly unavailable
until the new CA bundle and manager Pod are ready. cert-manager is not required.

`rbac.create=false` disables both manager and leader-election RBAC. Supply equivalent
permissions before starting the chart. The manager intentionally has no permission
to read source Secrets; importer Jobs mount the named same-namespace Secret directly.

Helm does not delete CRDs on uninstall. `Retain` storage and adopted PVCs remain; see
the [recovery runbook](../../docs/runbooks/artifact-import-and-recovery.md) before
removing a production installation.
