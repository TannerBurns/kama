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
llama_cpp_commit="$(make_version LLAMA_CPP_COMMIT)"
llama_cpp_build_number="$(make_version LLAMA_CPP_BUILD_NUMBER)"
llama_cpp_source_sha256="$(make_version LLAMA_CPP_SOURCE_SHA256)"
cuda_version="$(make_version CUDA_VERSION)"

for pin in go_version kubebuilder_version controller_runtime_version kubernetes_lib_version \
  keda_version llama_cpp_commit llama_cpp_build_number llama_cpp_source_sha256 cuda_version; do
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
if ! grep -Fq -- "--importer-image=ghcr.io/tannerburns/kama-importer:${version}" \
  "${repo_root}/config/manager/manager.yaml"; then
  echo "Kustomize importer image does not match VERSION ${version}" >&2
  exit 1
fi
for runtime in cpu cuda; do
  if ! grep -Fq -- "--runtime-${runtime}-image=ghcr.io/tannerburns/kama-runtime-${runtime}:${version}" \
    "${repo_root}/config/manager/manager.yaml"; then
    echo "Kustomize ${runtime} runtime image does not match VERSION ${version}" >&2
    exit 1
  fi
done
if ! grep -Fq -- "--llama-commit=${llama_cpp_commit}" "${repo_root}/config/manager/manager.yaml"; then
  echo "Kustomize llama.cpp commit does not match hack/versions.mk" >&2
  exit 1
fi
for workflow in "${repo_root}/.github/workflows/ci.yml" \
  "${repo_root}/.github/workflows/e2e.yml" \
  "${repo_root}/.github/workflows/e2e-nvidia.yml" \
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
  kind_kubernetes_version="${kind_image#kindest/node:}"
  kind_kubernetes_version="${kind_kubernetes_version%@sha256:*}"
  kubectl_version="$(make_version "KUBECTL_VERSION_${minor}")"
  if [[ -z "${kind_image}" ]] || ! grep -Fq "node-image: ${kind_image}" "${repo_root}/.github/workflows/kind.yml"; then
    echo "Kind workflow image for Kubernetes ${minor} does not match hack/versions.mk" >&2
    exit 1
  fi
  if [[ "${kubectl_version}" != "${kind_kubernetes_version}" ]]; then
    echo "kubectl for Kubernetes ${minor} does not match its Kind node image" >&2
    exit 1
  fi
  for arch in amd64 arm64; do
    kubectl_sha256="$(make_version "KUBECTL_SHA256_${minor}_${arch}")"
    if [[ ! "${kubectl_sha256}" =~ ^[a-f0-9]{64}$ ]]; then
      echo "kubectl ${kubectl_version} ${arch} does not have a pinned SHA-256" >&2
      exit 1
    fi
  done
done
if ! grep -Fq 'test-kind K8S_MINOR=${{ matrix.kubernetes }}' \
  "${repo_root}/.github/workflows/kind.yml"; then
  echo "Kind workflow does not select the matrix-matched kubectl" >&2
  exit 1
fi

makefile="${repo_root}/Makefile"
evidence_verifier="${repo_root}/hack/verify-m2-acceptance-evidence.sh"
if ! grep -Fq 'https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/linux/$(KUBECTL_ARCH)/kubectl' \
  "${makefile}" || ! grep -Fq 'sha256sum --check --strict' "${makefile}"; then
  echo "Makefile does not install checksum-verified, version-matched kubectl binaries" >&2
  exit 1
fi
if [[ ! -x "${evidence_verifier}" ]]; then
  echo "M2 acceptance evidence verifier is missing or not executable" >&2
  exit 1
fi
for mode in cpu nvidia; do
  target="verify-e2e-serving-${mode}-evidence"
  if ! grep -Fq ".PHONY: ${target}" "${makefile}" ||
    ! grep -Fq "${target}: ## Validate retained" "${makefile}" ||
    ! grep -Fq "bash hack/verify-m2-acceptance-evidence.sh ${mode}" "${makefile}"; then
    echo "Makefile does not expose the shared ${mode} M2 evidence verifier" >&2
    exit 1
  fi
done

kind_workflow="${repo_root}/.github/workflows/kind.yml"
e2e_workflow="${repo_root}/.github/workflows/e2e.yml"
nvidia_workflow="${repo_root}/.github/workflows/e2e-nvidia.yml"
if ! grep -Fq 'make verify-e2e-serving-cpu-evidence K8S_MINOR=${{ matrix.kubernetes }}' "${kind_workflow}" ||
  ! grep -Fq 'name: M2 CPU/Kind acceptance' "${kind_workflow}" ||
  ! grep -Fq 'make verify-e2e-serving-cpu-evidence K8S_MINOR=1.36' "${e2e_workflow}"; then
  echo "hosted workflows do not enforce the M2 CPU evidence gates" >&2
  exit 1
fi
if ! grep -Fq 'runs-on: [self-hosted, Linux, X64, kama-nvidia]' "${nvidia_workflow}" ||
  ! grep -Fq 'run: make helm cosign' "${nvidia_workflow}" ||
  ! grep -Fq 'run: make verify-e2e-serving-nvidia-evidence' "${nvidia_workflow}" ||
  ! grep -Fq 'name: Protected NVIDIA acceptance gate' "${nvidia_workflow}" ||
  ! grep -Fq 'rm -f -- "${KUBECONFIG_PATH}"' "${nvidia_workflow}"; then
  echo "protected workflow does not enforce the NVIDIA evidence and credential-cleanup gate" >&2
  exit 1
fi

nvidia_suite="${repo_root}/hack/test-e2e-serving-nvidia.sh"
cpu_suite="${repo_root}/hack/test-e2e-serving-cpu.sh"
for serving_suite in "${cpu_suite}" "${nvidia_suite}"; do
  if grep -Fq 'kubernetes-version.json" 2>&1' "${serving_suite}" ||
    ! grep -Fq '2>"${evidence_dir}/kubernetes-version.stderr"' "${serving_suite}"; then
    echo "${serving_suite} must keep kubectl warnings out of Kubernetes JSON evidence" >&2
    exit 1
  fi
done
for contract in \
  'verify_supply_chain()' \
  '"${cosign_bin}" verify' \
  '"${cosign_bin}" verify-attestation' \
  '--type spdxjson' \
  'kubernetesMinorVerified' \
  'expected_repository="ghcr.io/tannerburns/kama-runtime-cuda"' \
  "dpkg-query -W" \
  'ldd /usr/local/bin/llama-server' \
  'libcudart\.so\.12' \
  'libcublas\.so\.12'; do
  if ! grep -Fq -- "${contract}" "${nvidia_suite}"; then
    echo "protected NVIDIA suite is missing acceptance contract: ${contract}" >&2
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
for dockerfile in "${repo_root}/Dockerfile" "${repo_root}/Dockerfile.importer" \
  "${repo_root}/Dockerfile.test-fixtures"; do
  if ! grep -Fq "FROM ${builder_ref} AS builder" "${dockerfile}"; then
    echo "${dockerfile} does not use the approved digest-pinned Go builder" >&2
    exit 1
  fi
done

runtime_cpu_dockerfile="${repo_root}/Dockerfile.runtime-cpu"
runtime_cuda_dockerfile="${repo_root}/Dockerfile.runtime-cuda"
for dockerfile in "${runtime_cpu_dockerfile}" "${runtime_cuda_dockerfile}"; do
  if ! grep -Fq "ARG LLAMA_CPP_COMMIT=${llama_cpp_commit}" "${dockerfile}" ||
    ! grep -Fq "ARG LLAMA_CPP_BUILD_NUMBER=${llama_cpp_build_number}" "${dockerfile}" ||
    ! grep -Fq "ARG LLAMA_CPP_SOURCE_SHA256=${llama_cpp_source_sha256}" "${dockerfile}" ||
    ! grep -Fq 'io.kama.llama.cpp.commit="${LLAMA_CPP_COMMIT}"' "${dockerfile}" ||
    ! grep -Fq 'io.kama.llama.cpp.build-number="${LLAMA_CPP_BUILD_NUMBER}"' "${dockerfile}" ||
    ! grep -Fq 'io.kama.llama.cpp.source-sha256="${LLAMA_CPP_SOURCE_SHA256}"' "${dockerfile}" ||
    ! grep -Fq 'ARG BUILD_JOBS=2' "${dockerfile}" ||
    ! grep -Fq -- '--parallel "${BUILD_JOBS}"' "${dockerfile}" ||
    ! grep -Fq 'internal/runtime.LlamaCPPBuildNumber=${LLAMA_CPP_BUILD_NUMBER}' "${dockerfile}"; then
    echo "${dockerfile} does not match the pinned llama.cpp source identity" >&2
    exit 1
  fi
  if ! grep -Fq "FROM ${builder_ref} AS supervisor-builder" "${dockerfile}"; then
    echo "${dockerfile} does not use the approved digest-pinned Go supervisor builder" >&2
    exit 1
  fi
done
if ! grep -Fq \
  'FROM docker.io/library/ubuntu:22.04@sha256:0e0a0fc6d18feda9db1590da249ac93e8d5abfea8f4c3c0c849ce512b5ef8982' \
  "${runtime_cpu_dockerfile}"; then
  echo "CPU runtime does not use the approved digest-pinned Ubuntu 22.04 base" >&2
  exit 1
fi
if ! grep -Fq \
  "FROM docker.io/nvidia/cuda:${cuda_version}-devel-ubuntu22.04@sha256:da6791294b0b04d7e65d87b7451d6f2390b4d36225ab0701ee7dfec5769829f5 AS llama-builder" \
  "${runtime_cuda_dockerfile}" ||
  ! grep -Fq \
    "FROM docker.io/nvidia/cuda:${cuda_version}-runtime-ubuntu22.04@sha256:517da2300c184c9999ec203c2665244bdebd3578d12fcc7065e83667932643d9" \
    "${runtime_cuda_dockerfile}"; then
  echo "CUDA runtime does not use the approved ${cuda_version} digest-pinned bases" >&2
  exit 1
fi
if ! grep -Fq -- '-DGGML_NATIVE=OFF' "${runtime_cpu_dockerfile}"; then
  echo "CPU runtime must disable host-native llama.cpp optimizations" >&2
  exit 1
fi
cuda_architectures='60;61;70;75;80;86;89;90'
if ! grep -Fq "ARG CUDA_ARCHITECTURES=${cuda_architectures}" "${runtime_cuda_dockerfile}" ||
  ! grep -Fq -- '-DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCHITECTURES}"' "${runtime_cuda_dockerfile}" ||
  ! grep -Fq -- '-DGGML_NATIVE=OFF' "${runtime_cuda_dockerfile}" ||
  ! grep -Fq -- '-DGGML_CUDA=ON' "${runtime_cuda_dockerfile}" ||
  ! grep -Fq 'ARG TARGETARCH=amd64' "${runtime_cuda_dockerfile}" ||
  ! grep -Fq 'test "${TARGETOS}/${TARGETARCH}" = "linux/amd64"' "${runtime_cuda_dockerfile}" ||
  ! grep -Fq "io.kama.cuda.version=\"${cuda_version}\"" "${runtime_cuda_dockerfile}"; then
  echo "CUDA runtime does not enforce the approved portable amd64 CUDA build" >&2
  exit 1
fi
if ! grep -Fq 'RUNTIME_CPU_PLATFORMS ?= linux/amd64,linux/arm64' "${repo_root}/Makefile" ||
  ! grep -Fq 'RUNTIME_CUDA_PLATFORMS ?= linux/amd64' "${repo_root}/Makefile" ||
  ! grep -Fq 'platforms: linux/amd64,linux/arm64' "${repo_root}/.github/workflows/release.yml" ||
  ! grep -Fq 'platforms: linux/amd64' "${repo_root}/.github/workflows/release.yml"; then
  echo "runtime Buildx architecture matrices do not match the M2 contract" >&2
  exit 1
fi
for runtime in cpu cuda; do
  if ! grep -Fq "dist/sbom/kama-runtime-${runtime}.spdx.json" "${repo_root}/.github/workflows/release.yml" ||
    ! grep -Fq "runtime_${runtime}_ref=\"\${RUNTIME_${runtime^^}_IMAGE}@" "${repo_root}/.github/workflows/release.yml" ||
    ! grep -Fq "./bin/cosign sign \"\${runtime_${runtime}_ref}\"" "${repo_root}/.github/workflows/release.yml" ||
    ! grep -Fq "./bin/cosign attest --type spdxjson --predicate dist/sbom/kama-runtime-${runtime}.spdx.json \"\${runtime_${runtime}_ref}\"" \
      "${repo_root}/.github/workflows/release.yml"; then
    echo "release workflow does not produce, sign, and attest the ${runtime} runtime SBOM" >&2
    exit 1
  fi
done
if ! grep -Fq 'name: Validate published runtime manifests and binaries' "${repo_root}/.github/workflows/release.yml" ||
  ! grep -Fq 'CHECK_IMAGES=1' "${repo_root}/.github/workflows/release.yml" ||
  ! grep -Fq -- '--entrypoint /usr/local/bin/llama-server' "${repo_root}/.github/workflows/release.yml" ||
  ! grep -Fq 'dist/release/immutable-values.json' "${repo_root}/.github/workflows/release.yml"; then
  echo "release workflow does not validate published runtime binaries and record immutable install values" >&2
  exit 1
fi
if ! cmp -s \
  "${repo_root}/config/crd/bases/kama.tannerburns.github.io_modeldeployments.yaml" \
  "${repo_root}/charts/kama/crds/kama.tannerburns.github.io_modeldeployments.yaml"; then
  echo "packaged ModelDeployment CRD is not synchronized with the generated source" >&2
  exit 1
fi

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
if ! grep -Fq "ghcr.io/tannerburns/kama-importer:${version}" <<<"${rendered}"; then
  echo "chart's default importer image tag does not match VERSION ${version}" >&2
  exit 1
fi
if ! grep -Fq "ghcr.io/tannerburns/kama-runtime-cpu:${version}" <<<"${rendered}"; then
  echo "chart's default CPU runtime image tag does not match VERSION ${version}" >&2
  exit 1
fi
if ! grep -Fq "ghcr.io/tannerburns/kama-runtime-cuda:${version}" <<<"${rendered}"; then
  echo "chart's default CUDA runtime image tag does not match VERSION ${version}" >&2
  exit 1
fi
if ! grep -Fq -- "--llama-commit=${llama_cpp_commit}" <<<"${rendered}"; then
  echo "chart's llama.cpp commit does not match hack/versions.mk" >&2
  exit 1
fi
for webhook in \
  "mmodeldeployment.kama.tannerburns.github.io:/mutate-kama-tannerburns-github-io-v1alpha1-modeldeployment" \
  "vmodeldeployment.kama.tannerburns.github.io:/validate-kama-tannerburns-github-io-v1alpha1-modeldeployment"; do
  webhook_name="${webhook%%:*}"
  webhook_path="${webhook#*:}"
  if ! grep -Fq "name: ${webhook_name}" <<<"${rendered}" ||
    ! grep -Fq "path: ${webhook_path}" <<<"${rendered}"; then
    echo "chart does not render fail-closed ModelDeployment admission ${webhook_name}" >&2
    exit 1
  fi
done
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
  importer_binary="${IMPORTER_BINARY:-${repo_root}/bin/kama-importer}"
  importer_binary_version="$("${importer_binary}" --version)"
  if [[ "${importer_binary_version}" != "${version}" ]]; then
    echo "importer binary version ${importer_binary_version} does not match VERSION ${version}" >&2
    exit 1
  fi
  supervisor_binary="${RUNTIME_SUPERVISOR_BINARY:-${repo_root}/bin/kama-runtime-supervisor}"
  supervisor_binary_version="$("${supervisor_binary}" --version)"
  if [[ "${supervisor_binary_version}" != "${version}" ]]; then
    echo "runtime supervisor binary version ${supervisor_binary_version} does not match VERSION ${version}" >&2
    exit 1
  fi
fi

if [[ "${CHECK_IMAGES:-0}" == "1" ]]; then
  manager_image="${IMG:?IMG is required when CHECK_IMAGES=1}"
  importer_image="${IMPORTER_IMG:?IMPORTER_IMG is required when CHECK_IMAGES=1}"
  fixtures_image="${FIXTURES_IMG:?FIXTURES_IMG is required when CHECK_IMAGES=1}"
  runtime_cpu_image="${RUNTIME_CPU_IMG:?RUNTIME_CPU_IMG is required when CHECK_IMAGES=1}"
  runtime_cuda_image="${RUNTIME_CUDA_IMG:?RUNTIME_CUDA_IMG is required when CHECK_IMAGES=1}"
  for image in "${manager_image}" "${importer_image}" "${fixtures_image}" \
    "${runtime_cpu_image}" "${runtime_cuda_image}"; do
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

  importer_version="$(docker run --rm "${importer_image}" --version)"
  if [[ "${importer_version}" != "${version}" ]]; then
    echo "importer image binary version ${importer_version} does not match VERSION ${version}" >&2
    exit 1
  fi

  for image in "${runtime_cpu_image}" "${runtime_cuda_image}"; do
    image_llama_commit="$(docker image inspect --format '{{ index .Config.Labels "io.kama.llama.cpp.commit" }}' "${image}")"
    if [[ "${image_llama_commit}" != "${llama_cpp_commit}" ]]; then
      echo "runtime image ${image} llama.cpp commit ${image_llama_commit} does not match ${llama_cpp_commit}" >&2
      exit 1
    fi
    image_llama_build_number="$(docker image inspect --format '{{ index .Config.Labels "io.kama.llama.cpp.build-number" }}' "${image}")"
    if [[ "${image_llama_build_number}" != "${llama_cpp_build_number}" ]]; then
      echo "runtime image ${image} llama.cpp build ${image_llama_build_number} does not match ${llama_cpp_build_number}" >&2
      exit 1
    fi
    image_llama_source_sha256="$(docker image inspect --format '{{ index .Config.Labels "io.kama.llama.cpp.source-sha256" }}' "${image}")"
    if [[ "${image_llama_source_sha256}" != "${llama_cpp_source_sha256}" ]]; then
      echo "runtime image ${image} llama.cpp source hash ${image_llama_source_sha256} does not match ${llama_cpp_source_sha256}" >&2
      exit 1
    fi
    runtime_version="$(docker run --rm "${image}" --version)"
    if [[ "${runtime_version}" != "${version}" ]]; then
      echo "runtime image ${image} supervisor version ${runtime_version} does not match VERSION ${version}" >&2
      exit 1
    fi
    llama_version="$(docker run --rm --entrypoint /usr/local/bin/llama-server "${image}" --version 2>&1)"
    expected_llama_version="version: ${llama_cpp_build_number} (${llama_cpp_commit})"
    if ! grep -Fq "${expected_llama_version}" <<<"${llama_version}"; then
      echo "runtime image ${image} does not execute the pinned llama-server build" >&2
      printf '%s\n' "${llama_version}" >&2
      exit 1
    fi
  done
  image_cuda_version="$(docker image inspect --format '{{ index .Config.Labels "io.kama.cuda.version" }}' "${runtime_cuda_image}")"
  if [[ "${image_cuda_version}" != "${cuda_version}" ]]; then
    echo "CUDA runtime image version ${image_cuda_version} does not match ${cuda_version}" >&2
    exit 1
  fi
fi

echo "release metadata is consistent at ${version}"
