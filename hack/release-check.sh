#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(tr -d '\r\n' < "${repo_root}/VERSION")"
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

versions_file="${repo_root}/hack/versions.mk"
make_version() {
  local name=$1
  awk -v name="${name}" '$1 == name && $2 == ":=" { print $3 }' "${versions_file}"
}

go_version="$(make_version GO_VERSION)"
kubebuilder_version="$(make_version KUBEBUILDER_VERSION)"
controller_runtime_version="$(make_version CONTROLLER_RUNTIME_VERSION)"
kubernetes_lib_version="$(make_version KUBERNETES_LIB_VERSION)"
keda_version="$(make_version KEDA_VERSION)"

for pin in go_version kubebuilder_version controller_runtime_version kubernetes_lib_version keda_version; do
  if [[ -z "${!pin}" ]]; then
    echo "hack/versions.mk does not define ${pin}" >&2
    exit 1
  fi
done

if [[ "$(tr -d '\r\n' < "${repo_root}/.go-version")" != "${go_version}" ]]; then
  echo ".go-version does not match GO_VERSION ${go_version}" >&2
  exit 1
fi
if ! grep -Fqx "toolchain go${go_version}" "${repo_root}/go.mod"; then
  echo "go.mod toolchain does not match GO_VERSION ${go_version}" >&2
  exit 1
fi
go_language_version="${go_version%.*}.0"
if ! grep -Fqx "go ${go_language_version}" "${repo_root}/go.mod"; then
  echo "go.mod language version does not match ${go_language_version}" >&2
  exit 1
fi
for module_pin in \
  "sigs.k8s.io/controller-runtime ${controller_runtime_version}" \
  "k8s.io/apimachinery ${kubernetes_lib_version}" \
  "k8s.io/client-go ${kubernetes_lib_version}" \
  "github.com/kedacore/keda/v2 v${keda_version}"; do
  if ! grep -Fq "${module_pin}" "${repo_root}/go.mod"; then
    echo "go.mod does not contain pinned module ${module_pin}" >&2
    exit 1
  fi
done
if ! grep -Fqx "cliVersion: ${kubebuilder_version#v}" "${repo_root}/PROJECT"; then
  echo "PROJECT does not match KUBEBUILDER_VERSION ${kubebuilder_version}" >&2
  exit 1
fi
if ! grep -Fq "image: ghcr.io/tannerburns/kama-manager:${version}" \
  "${repo_root}/config/manager/manager.yaml"; then
  echo "Kustomize manager image does not match VERSION ${version}" >&2
  exit 1
fi
for workflow in "${repo_root}/.github/workflows/ci.yml" \
  "${repo_root}/.github/workflows/kind.yml" \
  "${repo_root}/.github/workflows/release.yml"; do
  if ! grep -Fq "go-version-file: .go-version" "${workflow}"; then
    echo "${workflow} does not consume the exact .go-version pin" >&2
    exit 1
  fi
done
if ! grep -Fq "KEDA_VERSION: ${keda_version}" "${repo_root}/.github/workflows/kind.yml"; then
  echo "Kind workflow does not match KEDA_VERSION ${keda_version}" >&2
  exit 1
fi
for minor in 1.34 1.35 1.36; do
  kind_image="$(make_version "KIND_NODE_IMAGE_${minor}")"
  if [[ -z "${kind_image}" ]] || ! grep -Fq "node-image: ${kind_image}" "${repo_root}/.github/workflows/kind.yml"; then
    echo "Kind workflow image for Kubernetes ${minor} does not match hack/versions.mk" >&2
    exit 1
  fi
done

expected_tag="v${version}"
release_tag="${RELEASE_TAG:-${GITHUB_REF_NAME:-}}"
if [[ "${GITHUB_REF_TYPE:-}" == "tag" || -n "${RELEASE_TAG:-}" ]]; then
  if [[ "${release_tag}" != "${expected_tag}" ]]; then
    echo "release tag ${release_tag} does not match ${expected_tag}" >&2
    exit 1
  fi
fi

builder_ref="docker.io/library/golang:${go_version}-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2"
for dockerfile in "${repo_root}/Dockerfile" "${repo_root}/Dockerfile.test-fixtures"; do
  if ! grep -Fq "FROM ${builder_ref} AS builder" "${dockerfile}"; then
    echo "${dockerfile} does not use the approved digest-pinned Go builder" >&2
    exit 1
  fi
done

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

OUTPUT_DIR="${tmp_dir}" HELM="${helm_bin}" bash "${repo_root}/hack/helm-package.sh" >/dev/null
chart_package="${tmp_dir}/kama-${version}.tgz"
chart_metadata="$("${helm_bin}" show chart "${chart_package}")"
chart_version="$(printf '%s\n' "${chart_metadata}" | awk '$1 == "version:" {print $2}')"
app_version="$(printf '%s\n' "${chart_metadata}" | awk '$1 == "appVersion:" {gsub(/"/, "", $2); print $2}')"

if [[ "${chart_version}" != "${version}" ]]; then
  echo "packaged chart version ${chart_version} does not match VERSION ${version}" >&2
  exit 1
fi
if [[ "${app_version}" != "${version}" ]]; then
  echo "packaged chart appVersion ${app_version} does not match VERSION ${version}" >&2
  exit 1
fi

rendered="$("${helm_bin}" template kama "${chart_package}" \
  --namespace kama-system \
  --set image.repository=example.invalid/kama-manager)"
if ! grep -Fq "example.invalid/kama-manager:${version}" <<<"${rendered}"; then
  echo "chart's default image tag does not match VERSION ${version}" >&2
  exit 1
fi
if ! grep -Fq "app.kubernetes.io/version: \"${version}\"" <<<"${rendered}"; then
  echo "rendered chart version label does not match VERSION ${version}" >&2
  exit 1
fi

if [[ "${CHECK_BINARY:-0}" == "1" ]]; then
  binary="${MANAGER_BINARY:-${repo_root}/bin/manager}"
  binary_version="$("${binary}" --version)"
  if [[ "${binary_version}" != "${version}" ]]; then
    echo "manager binary version ${binary_version} does not match VERSION ${version}" >&2
    exit 1
  fi
fi

if [[ "${CHECK_IMAGES:-0}" == "1" ]]; then
  manager_image="${IMG:?IMG is required when CHECK_IMAGES=1}"
  fixtures_image="${FIXTURES_IMG:?FIXTURES_IMG is required when CHECK_IMAGES=1}"
  for image in "${manager_image}" "${fixtures_image}"; do
    label="$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "${image}")"
    if [[ "${label}" != "${version}" ]]; then
      echo "image ${image} version label ${label} does not match VERSION ${version}" >&2
      exit 1
    fi
  done

  manager_version="$(docker run --rm "${manager_image}" --version)"
  if [[ "${manager_version}" != "${version}" ]]; then
    echo "manager image binary version ${manager_version} does not match VERSION ${version}" >&2
    exit 1
  fi
fi

echo "release metadata is consistent at ${version}"
