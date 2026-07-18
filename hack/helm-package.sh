#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(tr -d '\r\n' < "${repo_root}/VERSION")"
output_dir="${OUTPUT_DIR:-${DIST:-${repo_root}/dist}}"
helm_bin="${HELM:-${repo_root}/bin/helm}"

if [[ ! -x "${helm_bin}" ]]; then
  helm_bin="$(command -v helm || true)"
fi
if [[ -z "${helm_bin}" ]]; then
  echo "helm is required; run 'make bootstrap' first" >&2
  exit 1
fi
if [[ ! "${version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "VERSION must be SemVer without build metadata: ${version}" >&2
  exit 1
fi

mkdir -p "${output_dir}"
"${helm_bin}" lint --strict "${repo_root}/charts/kama"
"${helm_bin}" package "${repo_root}/charts/kama" \
  --version "${version}" \
  --app-version "${version}" \
  --destination "${output_dir}"
