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

require_exact_count() {
  local expected=$1
  local pattern=$2
  local file=$3
  local count

  count="$(grep -Fc -- "${pattern}" "${file}" || true)"
  if [[ "${count}" -ne "${expected}" ]]; then
    echo "${file} contains ${count} occurrences of ${pattern}; expected ${expected}" >&2
    exit 1
  fi
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
evidence_contract_tests="${repo_root}/hack/test-m2-acceptance-evidence-contracts.sh"
preinstalled_contract_tests="${repo_root}/hack/test-e2e-serving-nvidia-preinstalled.sh"
published_image_contract_tests="${repo_root}/hack/test-verify-published-release-image.sh"
if ! grep -Fq 'https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/linux/$(KUBECTL_ARCH)/kubectl' \
  "${makefile}" || ! grep -Fq 'sha256sum --check --strict' "${makefile}"; then
  echo "Makefile does not install checksum-verified, version-matched kubectl binaries" >&2
  exit 1
fi
if [[ ! -x "${evidence_verifier}" ]]; then
  echo "M2 acceptance evidence verifier is missing or not executable" >&2
  exit 1
fi
if [[ ! -f "${evidence_contract_tests}" ]]; then
  echo "M2 acceptance evidence contract regression tests are missing" >&2
  exit 1
fi
if [[ ! -f "${preinstalled_contract_tests}" ]]; then
  echo "M2 NVIDIA preinstalled-controller regression tests are missing" >&2
  exit 1
fi
if [[ ! -f "${published_image_contract_tests}" ]]; then
  echo "published release image verification regression tests are missing" >&2
  exit 1
fi
if ! grep -Fq 'nvidia_service_contract="$(<"${repo_root}/hack/m2-nvidia-service-contract.jq")"' \
  "${evidence_verifier}" ||
  ! grep -Fq 'assert_json service.json "stable ClusterIP serving Service" "${nvidia_service_contract}"' \
    "${evidence_verifier}"; then
  echo "M2 NVIDIA evidence verifier is not using the regression-tested Service contract" >&2
  exit 1
fi
if ! grep -Fq 'nvidia_storage_contract="$(<"${repo_root}/hack/m2-nvidia-storage-contract.jq")"' \
  "${evidence_verifier}" ||
  ! grep -Fq 'assert_json storage.json "bound cache/artifact/PVC/PV identity and retention contract"' \
    "${evidence_verifier}"; then
  echo "M2 NVIDIA evidence verifier is not using the regression-tested storage contract" >&2
  exit 1
fi
bash "${evidence_contract_tests}" >/dev/null
bash "${preinstalled_contract_tests}" >/dev/null
bash "${published_image_contract_tests}" >/dev/null
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
release_workflow="${repo_root}/.github/workflows/release.yml"
if ! grep -Fq 'make verify-e2e-serving-cpu-evidence K8S_MINOR=${{ matrix.kubernetes }}' "${kind_workflow}" ||
  ! grep -Fq 'name: M2 CPU/Kind acceptance' "${kind_workflow}" ||
  ! grep -Fq 'make verify-e2e-serving-cpu-evidence K8S_MINOR=1.36' "${e2e_workflow}" ||
  ! grep -Fq "if: \${{ github.event_name == 'schedule' || (github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main') }}" \
    "${e2e_workflow}"; then
  echo "hosted workflows do not enforce the M2 CPU evidence gates" >&2
  exit 1
fi
if ! grep -Fq 'require_private_hf:' "${e2e_workflow}" ||
  ! grep -Fq "E2E_REQUIRE_PRIVATE_HF: \${{ github.event_name == 'workflow_dispatch' && inputs.require_private_hf" "${e2e_workflow}"; then
  echo "scheduled Hugging Face regression is not isolated from opt-in private qualification" >&2
  exit 1
fi
if ! grep -Fq 'runs-on: [self-hosted, Linux, X64, kama-nvidia]' "${nvidia_workflow}" ||
  ! grep -Fq 'run: make helm cosign' "${nvidia_workflow}" ||
  ! grep -Fq 'E2E_NVIDIA_RUNTIME_CLASS: ${{ vars.E2E_NVIDIA_RUNTIME_CLASS }}' "${nvidia_workflow}" ||
  ! grep -Fq 'run: make verify-e2e-serving-nvidia-evidence' "${nvidia_workflow}" ||
  ! grep -Fq 'name: Protected NVIDIA acceptance gate' "${nvidia_workflow}" ||
  ! grep -Fq 'rm -f -- "${KUBECONFIG_PATH}"' "${nvidia_workflow}"; then
  echo "protected workflow does not enforce the NVIDIA evidence and credential-cleanup gate" >&2
  exit 1
fi
if grep -Fq '  schedule:' "${nvidia_workflow}"; then
  echo "protected NVIDIA qualification must remain manual-only" >&2
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
require_exact_count 1 \
  '"max_tokens":256,"ignore_eos":true,"stream":true' "${cpu_suite}"
if ! grep -Fq 'run: make helm supply-chain-tools' "${release_workflow}" ||
  grep -Fq 'run: make bootstrap supply-chain-tools' "${release_workflow}"; then
  echo "release workflow installs unused bootstrap tools" >&2
  exit 1
fi
for contract in \
  'verify_supply_chain()' \
  '"${cosign_bin}" verify' \
  '"${cosign_bin}" verify-attestation' \
  '--type spdxjson' \
  'kubernetesMinorVerified' \
  'expected_repository="ghcr.io/tannerburns/kama-runtime-cuda"' \
  'E2E_NVIDIA_PREINSTALLED_CONTROLLER' \
  'E2E_NVIDIA_USE_EXISTING_NAMESPACE' \
  'E2E_NVIDIA_EXISTING_CACHE_CLAIM' \
  'E2E_NVIDIA_RUNTIME_CLASS' \
  'runtime.cuda.runtimeClassName=${runtime_class}' \
  '--runtime-cuda-runtime-class=' \
  'get runtimeclass "${runtime_class}" -o json' \
  'verify_preinstalled_controller()' \
  'verify_no_existing_controller()' \
  'signed_linux_manifest_digests()' \
  'status --porcelain=v1 --untracked-files=all' \
  'preconditions: {uid: $uid}' \
  'PENDING: NVIDIA suite cleanup has not completed' \
  'cleanupComplete' \
  'retainedStorageVerified' \
  'release_attempted=1' \
  '--atomic --wait --timeout 5m' \
  'verify_no_gate_residuals()' \
  'delete_owned_namespace()' \
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
  if ! grep -Fq "FROM --platform=\$BUILDPLATFORM ${builder_ref} AS builder" "${dockerfile}"; then
    echo "${dockerfile} does not use the approved native digest-pinned Go builder" >&2
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
  if ! grep -Fq "FROM --platform=\$BUILDPLATFORM ${builder_ref} AS supervisor-builder" "${dockerfile}"; then
    echo "${dockerfile} does not use the approved native digest-pinned Go supervisor builder" >&2
    exit 1
  fi
  require_exact_count 1 "FROM ${builder_ref} AS apt-tls" "${dockerfile}"
  require_exact_count 2 \
    'COPY --from=apt-tls /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt' \
    "${dockerfile}"
  require_exact_count 2 \
    "s|http://archive.ubuntu.com|https://archive.ubuntu.com|g" "${dockerfile}"
  require_exact_count 2 \
    "s|http://security.ubuntu.com|https://security.ubuntu.com|g" "${dockerfile}"
  require_exact_count 2 \
    "s|http://ports.ubuntu.com|https://ports.ubuntu.com|g" "${dockerfile}"
  require_exact_count 2 'Acquire::Retries "5";' "${dockerfile}"
  require_exact_count 2 'Acquire::http::Timeout "30";' "${dockerfile}"
  require_exact_count 2 'Acquire::https::Timeout "30";' "${dockerfile}"
  require_exact_count 2 'apt-get update' "${dockerfile}"
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
ci_cuda_architectures='60;90'
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
if ! grep -Fq "RUNTIME_CUDA_ARCHITECTURES ?= ${cuda_architectures}" "${repo_root}/Makefile" ||
  ! grep -Fq -- '--build-arg CUDA_ARCHITECTURES="$(RUNTIME_CUDA_ARCHITECTURES)"' \
    "${repo_root}/Makefile" ||
  ! grep -Fq "RUNTIME_CUDA_ARCHITECTURES: \"${ci_cuda_architectures}\"" \
    "${repo_root}/.github/workflows/ci.yml" ||
  grep -Fq "CUDA_ARCHITECTURES=${ci_cuda_architectures}" \
    "${repo_root}/.github/workflows/release.yml"; then
  echo "CUDA architecture coverage does not keep full release builds and bounded PR validation" >&2
  exit 1
fi
release_job_block() {
  local job=$1
  awk -v job="${job}" '
    $0 == "  " job ":" { in_job = 1; next }
    in_job && $0 ~ /^  [[:alnum:]_-]+:$/ { exit }
    in_job { print }
  ' "${release_workflow}"
}

for job in manager importer fixtures runtime_cpu runtime_cuda; do
  block="$(release_job_block "${job}")"
  if [[ -z "${block}" ]] ||
    ! grep -Fq 'needs: metadata' <<<"${block}" ||
    ! grep -Fq 'digest: ${{ steps.verify.outputs.digest }}' <<<"${block}" ||
    ! grep -Fq 'BUILD_DIGEST: ${{ steps.build.outputs.digest }}' <<<"${block}" ||
    ! grep -Fq 'ref: ${{ needs.metadata.outputs.commit }}' <<<"${block}" ||
    ! grep -Fq 'VCS_REF=${{ needs.metadata.outputs.commit }}' <<<"${block}" ||
    ! grep -Fq 'bash hack/verify-published-release-image.sh' <<<"${block}" ||
    ! grep -Fq 'provenance: mode=max' <<<"${block}"; then
    echo "release ${job} job must independently publish and verify a provenance-bearing digest from validated source" >&2
    exit 1
  fi
done

require_exact_count 6 'if: github.run_attempt != 1' "${release_workflow}"
if ! grep -Fq 'assert_tag_absent()' "${release_workflow}" ||
  ! grep -Fq 'tannerburns/charts/kama' "${release_workflow}" ||
  ! grep -Fq 'git merge-base --is-ancestor "${commit}" refs/remotes/origin/main' "${release_workflow}"; then
  echo "release metadata must fail closed on reused tags and non-main source commits" >&2
  exit 1
fi

runtime_cuda_block="$(release_job_block runtime_cuda)"
if ! grep -Fq 'timeout-minutes: 360' <<<"${runtime_cuda_block}"; then
  echo "release CUDA build timeout must be 360 minutes for the full architecture build" >&2
  exit 1
fi

finalize_block="$(release_job_block finalize)"
for job in metadata manager importer fixtures runtime_cpu runtime_cuda; do
  if ! grep -Fq -- "- ${job}" <<<"${finalize_block}"; then
    echo "release finalization must wait for ${job}" >&2
    exit 1
  fi
done
if ! grep -Fq 'id-token: write' <<<"${finalize_block}"; then
  echo "release finalization must retain keyless signing authority" >&2
  exit 1
fi
if ! grep -Fq 'ref: ${{ needs.metadata.outputs.commit }}' <<<"${finalize_block}"; then
  echo "release finalization must use the validated source commit" >&2
  exit 1
fi
if ! grep -Fq 'RUNTIME_CPU_PLATFORMS ?= linux/amd64,linux/arm64' "${repo_root}/Makefile" ||
  ! grep -Fq 'RUNTIME_CUDA_PLATFORMS ?= linux/amd64' "${repo_root}/Makefile" ||
  ! grep -Fq 'platforms: linux/amd64,linux/arm64' "${release_workflow}" ||
  ! grep -Fq 'platforms: linux/amd64' "${release_workflow}"; then
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
  ! grep -Fq 'runtime_cpu_arm64_digest=' "${repo_root}/.github/workflows/release.yml" ||
  ! grep -Fq 'runtime_cpu_arm64_ref=' "${repo_root}/.github/workflows/release.yml" ||
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
if grep -Fq -- "--runtime-cuda-runtime-class=" <<<"${rendered}"; then
  echo "chart unexpectedly configures a CUDA RuntimeClass by default" >&2
  exit 1
fi
runtime_class_rendered="$("${helm_bin}" template kama "${chart_package}" \
  --namespace kama-system \
  --set runtime.cuda.runtimeClassName=nvidia)"
if ! grep -Fq -- "--runtime-cuda-runtime-class=nvidia" <<<"${runtime_class_rendered}"; then
  echo "chart does not render the configured CUDA RuntimeClass" >&2
  exit 1
fi
if "${helm_bin}" template kama "${chart_package}" \
  --namespace kama-system \
  --set runtime.cuda.runtimeClassName=Not_A_RuntimeClass >/dev/null 2>&1; then
  echo "chart accepts an invalid CUDA RuntimeClass name" >&2
  exit 1
fi
overlong_runtime_class="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
if "${helm_bin}" template kama "${chart_package}" \
  --namespace kama-system \
  --set runtime.cuda.runtimeClassName="${overlong_runtime_class}" >/dev/null 2>&1; then
  echo "chart accepts a CUDA RuntimeClass name with an overlong DNS-1123 label" >&2
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
    runtime_version=""
    if ! runtime_version="$(docker run --rm "${image}" --version 2>&1)"; then
      echo "runtime image ${image} supervisor version command failed" >&2
      printf '%s\n' "${runtime_version}" >&2
      exit 1
    fi
    if [[ "${runtime_version}" != "${version}" ]]; then
      echo "runtime image ${image} supervisor version ${runtime_version} does not match VERSION ${version}" >&2
      exit 1
    fi
  done
  expected_llama_version="version: ${llama_cpp_build_number} (${llama_cpp_commit})"
  llama_version=""
  if ! llama_version="$(docker run --rm --entrypoint /usr/local/bin/llama-server \
    "${runtime_cpu_image}" --version 2>&1)"; then
    echo "CPU runtime image ${runtime_cpu_image} llama-server version command failed" >&2
    printf '%s\n' "${llama_version}" >&2
    exit 1
  fi
  if ! grep -Fq "${expected_llama_version}" <<<"${llama_version}"; then
    echo "CPU runtime image ${runtime_cpu_image} does not execute the pinned llama-server build" >&2
    printf '%s\n' "${llama_version}" >&2
    exit 1
  fi
  # libcuda is supplied by the NVIDIA container runtime and may be unresolved on
  # a hosted runner. Inspect the CUDA executable without starting it here; the
  # protected NVIDIA lane starts this image and proves device use/offload/SSE.
  if ! docker run --rm --entrypoint /bin/sh "${runtime_cuda_image}" -c \
    'test -s /usr/local/bin/llama-server && test -x /usr/local/bin/llama-server && LC_ALL=C grep -aF -- "$1" /usr/local/bin/llama-server >/dev/null' \
    _ "${llama_cpp_commit}"; then
    echo "CUDA runtime image ${runtime_cuda_image} does not contain the pinned executable llama-server" >&2
    exit 1
  fi
  cuda_linkage="$(docker run --rm --entrypoint /bin/sh "${runtime_cuda_image}" -c \
    'ldd /usr/local/bin/llama-server' 2>&1 || true)"
  cuda_unresolved="$(awk '$2 == "=>" && $3 == "not" && $4 == "found" && $1 != "libcuda.so.1" { print }' <<<"${cuda_linkage}")"
  if ! grep -Eq 'libcudart\.so\.12[[:space:]]+=>[[:space:]]+/[^[:space:]]+' <<<"${cuda_linkage}" ||
    ! grep -Eq 'libcublas\.so\.12[[:space:]]+=>[[:space:]]+/[^[:space:]]+' <<<"${cuda_linkage}" ||
    ! grep -Eq 'libcuda\.so\.1[[:space:]]+=>[[:space:]]+(not found|/[^[:space:]]+)' <<<"${cuda_linkage}" ||
    [[ -n "${cuda_unresolved}" ]]; then
    echo "CUDA runtime image ${runtime_cuda_image} has unexpected GPU-independent linkage" >&2
    printf '%s\n' "${cuda_linkage}" >&2
    exit 1
  fi
  image_cuda_version="$(docker image inspect --format '{{ index .Config.Labels "io.kama.cuda.version" }}' "${runtime_cuda_image}")"
  if [[ "${image_cuda_version}" != "${cuda_version}" ]]; then
    echo "CUDA runtime image version ${image_cuda_version} does not match ${cuda_version}" >&2
    exit 1
  fi
fi

echo "release metadata is consistent at ${version}"
