#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(tr -d '\r\n' < "${repo_root}/VERSION")"
kubectl_bin="${KUBECTL:-kubectl}"
helm_bin="${HELM:-${repo_root}/bin/helm}"
cosign_bin="${COSIGN:-${repo_root}/bin/cosign}"
namespace="${E2E_NVIDIA_NAMESPACE:-kama-e2e-serving-nvidia}"
release="${E2E_NVIDIA_RELEASE:-kama-e2e-serving-nvidia}"
manager_image="${E2E_NVIDIA_MANAGER_IMAGE:-${IMG:-}}"
importer_image="${E2E_NVIDIA_IMPORTER_IMAGE:-${IMPORTER_IMG:-}}"
fixtures_image="${E2E_NVIDIA_FIXTURES_IMAGE:-${FIXTURES_IMG:-}}"
runtime_cpu_image="${E2E_NVIDIA_RUNTIME_CPU_IMAGE:-${RUNTIME_CPU_IMG:-}}"
runtime_cuda_image="${E2E_NVIDIA_RUNTIME_CUDA_IMAGE:-${RUNTIME_CUDA_IMG:-}}"
storage_class="${E2E_NVIDIA_STORAGE_CLASS:-}"
driver_version="${E2E_NVIDIA_DRIVER_VERSION:-}"
cuda_version="${E2E_NVIDIA_CUDA_VERSION:-}"
llama_commit="${LLAMA_CPP_COMMIT:?LLAMA_CPP_COMMIT is required}"
llama_build_number="${LLAMA_CPP_BUILD_NUMBER:?LLAMA_CPP_BUILD_NUMBER is required}"
llama_source_sha256="${LLAMA_CPP_SOURCE_SHA256:-$(awk '$1 == "LLAMA_CPP_SOURCE_SHA256" {print $3}' "${repo_root}/hack/versions.mk")}"
model_revision="${E2E_NVIDIA_MODEL_REVISION:-}"
model_digest="${E2E_NVIDIA_MODEL_SHA256:-}"
model_size="${E2E_NVIDIA_MODEL_SIZE:-}"
expected_model_revision="593b5a2e04c8f3e4ee880263f93e0bd2901ad47f"
expected_model_digest="48ab3034d0dd401fbc721eb1df3217902fee7dab9078992d66431f09b7750201"
expected_model_size="386404992"
evidence_dir_input="${E2E_EVIDENCE_DIR:-${repo_root}/dist/e2e/serving-nvidia}"
if [[ "${evidence_dir_input}" != /* ]]; then
  evidence_dir_input="${repo_root}/${evidence_dir_input}"
fi
evidence_dir="$(realpath -m "${evidence_dir_input}")"
safe_evidence_dir="$(realpath -m "${repo_root}/dist/e2e/serving-nvidia")"
if [[ "${evidence_dir}" != "${safe_evidence_dir}" ||
  -L "${repo_root}/dist" || -L "${repo_root}/dist/e2e" ||
  -L "${repo_root}/dist/e2e/serving-nvidia" ]]; then
  echo "E2E_EVIDENCE_DIR must be the non-symlinked repository path dist/e2e/serving-nvidia" >&2
  exit 2
fi
revision="$(git -C "${repo_root}" rev-parse HEAD 2>/dev/null || printf unknown)"
expected_commit="${E2E_NVIDIA_EXPECTED_COMMIT:-}"
tmp_dir="$(mktemp -d)"
namespace_created=0
release_installed=0
passed=0
provenance_verified=0
supply_chain_verified=0
evidence_complete=0
credential_safe=1
qualified=0
kubernetes_minor_verified=0
provenance_method="unavailable"
provenance_revision=""
provenance_source=""
provenance_llama_commit=""
provenance_llama_build_number=""
provenance_llama_source_sha256=""
provenance_cuda_version=""
manager_observed_digest=""
importer_observed_digest=""
fixtures_observed_digest=""
runtime_cpu_observed_digest=""
runtime_cuda_observed_digest=""
source_clean=0
observed_driver_version=""
observed_cuda_version=""
observed_gpu_device=""
observed_gpu_uuid=""
client_pod_name=""
client_pod_uid=""
client_resolved_image=""
client_restart_count=-1
client_completed=0
port_forward_pids=()

rm -rf -- "${evidence_dir}"
mkdir -p "${evidence_dir}"
printf '%s\n' 'FAIL: suite exited before evidence capture completed' >"${evidence_dir}/outcome.txt"

if [[ ! -x "${helm_bin}" ]]; then
  helm_bin="$(command -v helm || true)"
fi
if [[ ! -x "${cosign_bin}" ]]; then
  cosign_bin="$(command -v cosign || true)"
fi
for tool in "${kubectl_bin}" "${helm_bin}" "${cosign_bin}" awk curl git grep jq realpath sed; do
  if [[ -z "${tool}" ]] || ! command -v "${tool}" >/dev/null 2>&1; then
    echo "required command is unavailable: ${tool:-unset}" >&2
    exit 1
  fi
done
if [[ -z "$(git -C "${repo_root}" status --porcelain)" ]]; then
  source_clean=1
fi
if [[ -z "${KUBECONFIG:-}" || ! -r "${KUBECONFIG}" ]]; then
  echo "KUBECONFIG must name the protected NVIDIA-cluster kubeconfig" >&2
  exit 2
fi

for variable in manager_image importer_image fixtures_image runtime_cpu_image runtime_cuda_image; do
  value="${!variable}"
  case "${variable}" in
    manager_image) expected_repository="ghcr.io/tannerburns/kama-manager" ;;
    importer_image) expected_repository="ghcr.io/tannerburns/kama-importer" ;;
    fixtures_image) expected_repository="ghcr.io/tannerburns/kama-test-fixtures" ;;
    runtime_cpu_image) expected_repository="ghcr.io/tannerburns/kama-runtime-cpu" ;;
    runtime_cuda_image) expected_repository="ghcr.io/tannerburns/kama-runtime-cuda" ;;
  esac
  if [[ "${value%@sha256:*}" != "${expected_repository}" ||
    ! "${value##*@}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    echo "${variable} must use ${expected_repository}@sha256:<digest>" >&2
    exit 2
  fi
done
if [[ -z "${storage_class}" || -z "${driver_version}" || -z "${cuda_version}" ]]; then
  echo "E2E_NVIDIA_STORAGE_CLASS, E2E_NVIDIA_DRIVER_VERSION, and E2E_NVIDIA_CUDA_VERSION are required" >&2
  exit 2
fi
if [[ ! "${expected_commit}" =~ ^[a-f0-9]{40}$ || "${expected_commit}" != "${revision}" ]]; then
  echo "E2E_NVIDIA_EXPECTED_COMMIT must equal the trusted checked-out commit" >&2
  exit 2
fi
if [[ "${model_revision}" != "${expected_model_revision}" ||
  "${model_digest}" != "${expected_model_digest}" ||
  "${model_size}" != "${expected_model_size}" ]]; then
  echo "protected NVIDIA inputs must select Kama's exact pinned SmolLM2 fixture" >&2
  exit 2
fi
if [[ "${cuda_version}" != "12.4.1" ]]; then
  echo "E2E_NVIDIA_CUDA_VERSION must match the M2 CUDA runtime pin 12.4.1" >&2
  exit 2
fi
if [[ ! "${llama_build_number}" =~ ^[1-9][0-9]*$ ||
  ! "${llama_source_sha256}" =~ ^[a-f0-9]{64}$ ]]; then
  echo "llama.cpp build number and source archive SHA-256 must match the repository pin" >&2
  exit 2
fi

sanitize_evidence() {
  local unsafe_list="${tmp_dir}/unsafe-evidence-files.txt"
  local unsafe_file=""
  local unsafe_names="${tmp_dir}/unsafe-evidence-names.txt"
  : >"${unsafe_list}"
  : >"${unsafe_names}"
  grep -RIlE -i \
    'authorization:|bearer[[:space:]]|hf_[a-z0-9]+|client-key-data:|client-certificate-data:|(^|[[:space:]])token:[[:space:]]|password[=:]' \
    "${evidence_dir}" >"${unsafe_list}" 2>/dev/null || true
  if [[ ! -s "${unsafe_list}" ]]; then
    return
  fi

  credential_safe=0
  while IFS= read -r unsafe_file; do
    [[ "${unsafe_file}" == "${evidence_dir}/"* ]] || continue
    basename "${unsafe_file}" >>"${unsafe_names}"
    rm -f -- "${unsafe_file}"
  done <"${unsafe_list}"
  jq -R -s '{redacted: true, removedFiles: (split("\n") | map(select(length > 0)) | unique)}' \
    "${unsafe_names}" >"${evidence_dir}/redaction.json"
}

validate_evidence_files() {
  local json_files=(
    identities.json
    image-provenance.json
    supply-chain.json
    kubernetes-version.json
    nodes.json
    direct-request.json
    direct-request-job.json
    direct-request.log
    client-pod.json
    cuda-runtime.json
    supervisor-state.json
    modeldeployment.json
    workload.json
    pods.json
    service.json
    endpointslices.json
  )
  local text_files=(resources.txt events.txt)
  local file=""
  evidence_complete=1
  for file in "${json_files[@]}"; do
    if [[ ! -s "${evidence_dir}/${file}" ]] ||
      ! jq -e . "${evidence_dir}/${file}" >/dev/null 2>&1; then
      evidence_complete=0
    fi
  done
  for file in "${text_files[@]}"; do
    if [[ ! -s "${evidence_dir}/${file}" ]]; then
      evidence_complete=0
    fi
  done
}

capture_evidence() {
  local outcome=$1
  printf '%s\n' "${outcome}" >"${evidence_dir}/outcome.txt"
  jq -n \
    --arg commit "${revision}" \
    --arg managerImage "${manager_image}" \
    --arg importerImage "${importer_image}" \
    --arg fixturesImage "${fixtures_image}" \
    --arg runtimeCPUImage "${runtime_cpu_image}" \
    --arg runtimeCUDAImage "${runtime_cuda_image}" \
    --arg llamaCommit "${llama_commit}" \
    --arg llamaBuildNumber "${llama_build_number}" \
    --arg llamaSourceSHA256 "${llama_source_sha256}" \
    --arg modelRevision "${model_revision}" \
    --arg modelDigest "${model_digest}" \
    --arg modelSize "${model_size}" \
    --arg expectedDriverVersion "${driver_version}" \
    --arg expectedCUDAVersion "${cuda_version}" \
    --arg observedDriverVersion "${observed_driver_version}" \
    --arg observedCUDAVersion "${observed_cuda_version}" \
    --arg observedGPUDevice "${observed_gpu_device}" \
    --arg observedGPUUUID "${observed_gpu_uuid}" \
    --arg provenanceMethod "${provenance_method}" \
    --arg provenanceRevision "${provenance_revision}" \
    --arg provenanceSource "${provenance_source}" \
    --arg provenanceLlamaCommit "${provenance_llama_commit}" \
    --arg provenanceLlamaBuildNumber "${provenance_llama_build_number}" \
    --arg provenanceLlamaSourceSHA256 "${provenance_llama_source_sha256}" \
    --arg provenanceCUDAVersion "${provenance_cuda_version}" \
    --arg runtimeCUDAObservedDigest "${runtime_cuda_observed_digest}" \
    --argjson provenanceVerified "${provenance_verified}" \
    --argjson sourceClean "${source_clean}" '{
      commit: $commit,
      images: {
        manager: $managerImage,
        importer: $importerImage,
        servingClient: $fixturesImage,
        runtimeCPU: $runtimeCPUImage,
        runtimeCUDA: $runtimeCUDAImage
      },
      llamaCommit: $llamaCommit,
      llamaBuildNumber: $llamaBuildNumber,
      llamaSourceSHA256: $llamaSourceSHA256,
      model: {revision: $modelRevision, sha256: $modelDigest, size: (try ($modelSize | tonumber) catch null)},
      nvidia: {
        expectedDriverVersion: $expectedDriverVersion,
        expectedCUDAVersion: $expectedCUDAVersion,
        observedDriverVersion: $observedDriverVersion,
        observedCUDAVersion: $observedCUDAVersion,
        observedDevice: $observedGPUDevice,
        observedDeviceUUID: $observedGPUUUID
      },
      runtimeImageProvenance: {
        verified: ($provenanceVerified == 1),
        sourceCheckoutClean: ($sourceClean == 1),
        method: $provenanceMethod,
        source: $provenanceSource,
        revision: $provenanceRevision,
        llamaCommit: $provenanceLlamaCommit,
        llamaBuildNumber: $provenanceLlamaBuildNumber,
        llamaSourceSHA256: $provenanceLlamaSourceSHA256,
        cudaVersion: $provenanceCUDAVersion,
        expectedObservedManifestDigest: $runtimeCUDAObservedDigest
      }
    }' >"${evidence_dir}/identities.json"
  "${kubectl_bin}" version -o json >"${evidence_dir}/kubernetes-version.json" 2>&1 || true
  "${kubectl_bin}" get nodes -o json | jq '{
    items: [.items[] | {
      name: .metadata.name,
      gpuProduct: .metadata.labels["nvidia.com/gpu.product"],
      gpuCount: .status.allocatable["nvidia.com/gpu"],
      systemInfo: {
        architecture: .status.nodeInfo.architecture,
        containerRuntimeVersion: .status.nodeInfo.containerRuntimeVersion,
        kernelVersion: .status.nodeInfo.kernelVersion,
        kubeletVersion: .status.nodeInfo.kubeletVersion,
        operatingSystem: .status.nodeInfo.operatingSystem,
        osImage: .status.nodeInfo.osImage
      }
    }]
  }' >"${evidence_dir}/nodes.json" 2>/dev/null || true
  if [[ ${namespace_created} -eq 1 ]]; then
    "${kubectl_bin}" -n "${namespace}" get modelcache,modelartifact,modeldeployment,deploy,svc,job,pod \
      -o wide >"${evidence_dir}/resources.txt" 2>&1 || true
    "${kubectl_bin}" -n "${namespace}" get modeldeployment e2e-serving-nvidia -o json | jq '{
      metadata: {name: .metadata.name, uid: .metadata.uid, generation: .metadata.generation},
      spec: .spec,
      status: .status
    }' >"${evidence_dir}/modeldeployment.json" 2>/dev/null || true
    local deployment_snapshot=""
    local captured_workload_name=""
    local captured_service_name=""
    deployment_snapshot="$("${kubectl_bin}" -n "${namespace}" get modeldeployment \
      e2e-serving-nvidia -o json 2>/dev/null || true)"
    captured_workload_name="$(jq -r '.status.deploymentRef.name // empty' \
      <<<"${deployment_snapshot:-null}" 2>/dev/null || true)"
    captured_service_name="$(jq -r '.status.serviceRef.name // empty' \
      <<<"${deployment_snapshot:-null}" 2>/dev/null || true)"
    if [[ -n "${captured_workload_name}" ]]; then
      "${kubectl_bin}" -n "${namespace}" get deployment "${captured_workload_name}" -o json | jq '{
        metadata: {
          name: .metadata.name,
          uid: .metadata.uid,
          generation: .metadata.generation,
          labels: .metadata.labels,
          ownerReferences: [.metadata.ownerReferences[]? | {apiVersion, kind, name, uid, controller}]
        },
        spec: {
          replicas: .spec.replicas,
          strategy: .spec.strategy,
          selector: .spec.selector,
          template: {
            metadata: {
              labels: .spec.template.metadata.labels,
              annotations: .spec.template.metadata.annotations
            },
            spec: {
              serviceAccountName: .spec.template.spec.serviceAccountName,
              automountServiceAccountToken: .spec.template.spec.automountServiceAccountToken,
              enableServiceLinks: .spec.template.spec.enableServiceLinks,
              terminationGracePeriodSeconds: .spec.template.spec.terminationGracePeriodSeconds,
              securityContext: .spec.template.spec.securityContext,
              affinity: .spec.template.spec.affinity,
              nodeSelector: .spec.template.spec.nodeSelector,
              schedulingGates: .spec.template.spec.schedulingGates,
              volumes: [.spec.template.spec.volumes[]? | {
                name,
                persistentVolumeClaim,
                configMap,
                emptyDir
              }],
              containers: [.spec.template.spec.containers[]? | select(.name == "runtime") | {
                name,
                image,
                imagePullPolicy,
                command,
                args,
                ports,
                resources,
                securityContext,
                startupProbe,
                livenessProbe,
                readinessProbe,
                volumeMounts
              }]
            }
          }
        },
        status: {
          observedGeneration: .status.observedGeneration,
          replicas: .status.replicas,
          readyReplicas: .status.readyReplicas,
          availableReplicas: .status.availableReplicas,
          unavailableReplicas: .status.unavailableReplicas,
          conditions: [.status.conditions[]? | {type, status, reason}]
        }
      }' >"${evidence_dir}/workload.json" 2>/dev/null || true
    fi
    "${kubectl_bin}" -n "${namespace}" get pods \
      -l kama.tannerburns.github.io/model-deployment=e2e-serving-nvidia -o json | jq '{
        apiVersion,
        kind,
        items: [.items[] | {
          metadata: {
            name: .metadata.name,
            uid: .metadata.uid,
            labels: .metadata.labels,
            annotations: .metadata.annotations,
            ownerReferences: [.metadata.ownerReferences[]? | {apiVersion, kind, name, uid, controller}]
          },
          spec: {
            nodeName: .spec.nodeName,
            serviceAccountName: .spec.serviceAccountName,
            automountServiceAccountToken: .spec.automountServiceAccountToken,
            securityContext: .spec.securityContext,
            affinity: .spec.affinity,
            nodeSelector: .spec.nodeSelector,
            volumes: [.spec.volumes[]? | {name, persistentVolumeClaim, configMap, emptyDir}],
            containers: [.spec.containers[]? | select(.name == "runtime") | {
              name,
              image,
              imagePullPolicy,
              resources,
              securityContext,
              ports,
              startupProbe,
              livenessProbe,
              readinessProbe,
              volumeMounts
            }]
          },
          status: {
            phase: .status.phase,
            conditions: [.status.conditions[]? | {type, status, reason}],
            containerStatuses: [.status.containerStatuses[]? | select(.name == "runtime") | {
              name,
              ready,
              restartCount,
              image,
              imageID,
              started,
              state: {
                running: (.state.running // null),
                waiting: (if .state.waiting then {reason: .state.waiting.reason} else null end),
                terminated: (if .state.terminated then {
                  exitCode: .state.terminated.exitCode,
                  signal: .state.terminated.signal,
                  reason: .state.terminated.reason,
                  startedAt: .state.terminated.startedAt,
                  finishedAt: .state.terminated.finishedAt
                } else null end)
              }
            }]
          }
        }]
      }' >"${evidence_dir}/pods.json" 2>/dev/null || true
    if [[ -n "${captured_service_name}" ]]; then
      "${kubectl_bin}" -n "${namespace}" get service "${captured_service_name}" -o json | jq '{
        metadata: {
          name: .metadata.name,
          uid: .metadata.uid,
          labels: .metadata.labels,
          ownerReferences: [.metadata.ownerReferences[]? | {apiVersion, kind, name, uid, controller}]
        },
        spec: {
          type: .spec.type,
          selector: .spec.selector,
          internalTrafficPolicy: .spec.internalTrafficPolicy,
          ports: .spec.ports
        }
      }' >"${evidence_dir}/service.json" 2>/dev/null || true
      "${kubectl_bin}" -n "${namespace}" get endpointslice \
        -l "kubernetes.io/service-name=${captured_service_name}" -o json | jq '{
          apiVersion,
          kind,
          items: [.items[] | {
            metadata: {name: .metadata.name, uid: .metadata.uid, labels: .metadata.labels},
            addressType: .addressType,
            endpoints: [.endpoints[]? | {
              addresses,
              conditions,
              hostname,
              nodeName,
              zone,
              targetRef: (if .targetRef then {
                apiVersion: .targetRef.apiVersion,
                kind: .targetRef.kind,
                name: .targetRef.name,
                uid: .targetRef.uid
              } else null end)
            }],
            ports: .ports
          }]
        }' >"${evidence_dir}/endpointslices.json" 2>/dev/null || true
    fi
    "${kubectl_bin}" -n "${namespace}" get events --sort-by=.lastTimestamp \
      >"${evidence_dir}/events.txt" 2>&1 || true
    if [[ ${release_installed} -eq 1 ]]; then
      "${kubectl_bin}" -n "${namespace}" logs "deployment/${release}" --container manager --tail=300 \
        >"${evidence_dir}/manager.log" 2>&1 || true
    fi
  fi

  sanitize_evidence
  validate_evidence_files
  if [[ ${passed} -eq 1 && ${provenance_verified} -eq 1 &&
    ${supply_chain_verified} -eq 1 && ${evidence_complete} -eq 1 &&
    ${credential_safe} -eq 1 && ${kubernetes_minor_verified} -eq 1 ]]; then
    qualified=1
  fi
  jq -n \
    --arg outcome "${outcome}" \
    --argjson qualifying "${qualified}" \
    --argjson imageProvenanceVerified "${provenance_verified}" \
    --argjson supplyChainVerified "${supply_chain_verified}" \
    --argjson evidenceComplete "${evidence_complete}" \
    --argjson kubernetesMinorVerified "${kubernetes_minor_verified}" \
    --argjson credentialSafe "${credential_safe}" '{
    schemaVersion: 1,
    suite: "baseline-serving/nvidia",
    outcome: $outcome,
    qualifying: ($qualifying == 1),
    imageProvenanceVerified: ($imageProvenanceVerified == 1),
    supplyChainVerified: ($supplyChainVerified == 1),
    evidenceComplete: ($evidenceComplete == 1),
    kubernetesMinorVerified: ($kubernetesMinorVerified == 1),
    credentialSafe: ($credentialSafe == 1)
  }' >"${evidence_dir}/qualification.json"
}

cleanup() {
  local exit_code=$?
  for pid in "${port_forward_pids[@]:-}"; do
    kill "${pid}" >/dev/null 2>&1 || true
  done
  if [[ ${exit_code} -eq 0 && ${passed} -eq 1 ]]; then
    capture_evidence "PASS: protected one-NVIDIA-GPU serving verified"
  else
    capture_evidence "FAIL (exit ${exit_code})"
  fi
  if [[ ${credential_safe} -ne 1 ]]; then
    echo "credential-shaped content was removed from NVIDIA serving evidence" >&2
    exit_code=1
  fi
  if [[ "${KEEP_NVIDIA_RESOURCES:-0}" != "1" ]]; then
    if [[ ${namespace_created} -eq 1 ]]; then
      "${kubectl_bin}" -n "${namespace}" delete modeldeployment e2e-serving-nvidia \
        --ignore-not-found --wait --timeout=3m >/dev/null 2>&1 || true
      "${kubectl_bin}" -n "${namespace}" delete modelartifact e2e-serving-nvidia-model \
        --ignore-not-found --wait --timeout=3m >/dev/null 2>&1 || true
      "${kubectl_bin}" -n "${namespace}" delete modelcache e2e-serving-nvidia-cache \
        --ignore-not-found --wait --timeout=3m >/dev/null 2>&1 || true
    fi
    if [[ ${release_installed} -eq 1 ]]; then
      "${helm_bin}" uninstall "${release}" --namespace "${namespace}" --wait --timeout 3m \
        >/dev/null 2>&1 || true
    fi
    if [[ ${namespace_created} -eq 1 ]]; then
      "${kubectl_bin}" delete namespace "${namespace}" --wait=false >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "${tmp_dir}"
  exit "${exit_code}"
}
trap cleanup EXIT

wait_for_http() {
  local url=$1
  for _ in $(seq 1 120); do
    if curl --fail --silent --show-error "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for ${url}" >&2
  return 1
}

read_image_labels() {
  local image=$1
  local config_json=""
  local method=""
  local image_name="${image%@sha256:*}"
  local image_digest="${image##*@}"
  local registry="${image_name%%/*}"
  local repository="${image_name#*/}"
  local resolved_digest=""
  if [[ "${registry}" == "ghcr.io" ]]; then
    local token=""
    local manifest=""
    local platform_digest=""
    local config_digest=""
    token="$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 --get \
      --data-urlencode "scope=repository:${repository}:pull" \
      https://ghcr.io/token 2>/dev/null | jq -r '.token // empty' || true)"
    if [[ -n "${token}" ]]; then
      manifest="$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
        -H "Authorization: Bearer ${token}" \
        -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
        "https://${registry}/v2/${repository}/manifests/${image_digest}" 2>/dev/null || true)"
      platform_digest="$(jq -r '
        if (.manifests | type) == "array" then
          [.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")][0].digest // empty
        else empty end
      ' <<<"${manifest:-null}" 2>/dev/null || true)"
      if [[ -n "${platform_digest}" ]]; then
        manifest="$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
          -H "Authorization: Bearer ${token}" \
          -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
          "https://${registry}/v2/${repository}/manifests/${platform_digest}" 2>/dev/null || true)"
        resolved_digest="${platform_digest}"
      else
        resolved_digest="${image_digest}"
      fi
      config_digest="$(jq -r '.config.digest // empty' <<<"${manifest:-null}" 2>/dev/null || true)"
      if [[ "${config_digest}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
        config_json="$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
          -H "Authorization: Bearer ${token}" \
          "https://${registry}/v2/${repository}/blobs/${config_digest}" 2>/dev/null | \
          jq '.config.Labels // {}' 2>/dev/null || true)"
        method="GHCR distribution API"
      fi
    fi
  fi
  if [[ -z "${config_json}" ]] && command -v crane >/dev/null 2>&1; then
    if config_json="$(crane config "${image}" 2>/dev/null)"; then
      method="crane config"
      config_json="$(jq '.config.Labels // {}' <<<"${config_json}")"
    fi
  fi
  if [[ -z "${config_json}" ]] && command -v docker >/dev/null 2>&1 && \
    docker info >/dev/null 2>&1; then
    if docker pull "${image}" >/dev/null 2>&1; then
      config_json="$(docker image inspect --format '{{json .Config.Labels}}' \
        "${image}" 2>/dev/null || true)"
      method="docker image inspect"
    fi
  fi

  if [[ -z "${config_json}" ]] || ! jq -e 'type == "object"' \
    <<<"${config_json}" >/dev/null 2>&1; then
    return 1
  fi
  jq -n --arg method "${method}" --arg resolvedDigest "${resolved_digest}" \
    --argjson labels "${config_json}" \
    '{method: $method, resolvedDigest: $resolvedDigest, labels: $labels}'
}

verify_image_provenance() {
  local records_file="${tmp_dir}/image-provenance.jsonl"
  local all_verified=1
  local role=""
  local image=""
  local result=""
  local labels=""
  local verified=0
  : >"${records_file}"

  for role in manager importer servingClient runtimeCPU runtimeCUDA; do
    case "${role}" in
      manager) image="${manager_image}" ;;
      importer) image="${importer_image}" ;;
      servingClient) image="${fixtures_image}" ;;
      runtimeCPU) image="${runtime_cpu_image}" ;;
      runtimeCUDA) image="${runtime_cuda_image}" ;;
    esac
    verified=0
    result="$(read_image_labels "${image}" || true)"
    labels="$(jq -c '.labels // {}' <<<"${result:-null}" 2>/dev/null || printf '{}')"
    if jq -e --arg revision "${revision}" '
      .["org.opencontainers.image.source"] == "https://github.com/TannerBurns/kama" and
      .["org.opencontainers.image.revision"] == $revision
    ' <<<"${labels}" >/dev/null 2>&1; then
      verified=1
    fi
    if [[ "${role}" == runtimeCPU || "${role}" == runtimeCUDA ]] && ! jq -e \
      --arg commit "${llama_commit}" \
      --arg buildNumber "${llama_build_number}" \
      --arg sourceSHA256 "${llama_source_sha256}" '
        .["io.kama.llama.cpp.commit"] == $commit and
        .["io.kama.llama.cpp.build-number"] == $buildNumber and
        .["io.kama.llama.cpp.source-sha256"] == $sourceSHA256
      ' \
      <<<"${labels}" >/dev/null 2>&1; then
      verified=0
    fi
    if [[ "${role}" == runtimeCUDA ]] && ! jq -e --arg cuda "${cuda_version}" \
      '.["io.kama.cuda.version"] == $cuda' <<<"${labels}" >/dev/null 2>&1; then
      verified=0
    fi
    if [[ ${verified} -ne 1 ]]; then
      all_verified=0
    fi
    resolved_digest="$(jq -r '.resolvedDigest // empty' <<<"${result:-null}" 2>/dev/null || true)"
    if [[ ! "${resolved_digest}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
      verified=0
      all_verified=0
    fi
    jq -n \
      --arg role "${role}" \
      --arg image "${image}" \
      --arg method "$(jq -r '.method // "unavailable"' <<<"${result:-null}" 2>/dev/null || printf unavailable)" \
      --arg resolvedDigest "$(jq -r '.resolvedDigest // empty' <<<"${result:-null}" 2>/dev/null || true)" \
      --arg source "$(jq -r '.["org.opencontainers.image.source"] // empty' <<<"${labels}")" \
      --arg revision "$(jq -r '.["org.opencontainers.image.revision"] // empty' <<<"${labels}")" \
      --arg llamaCommit "$(jq -r '.["io.kama.llama.cpp.commit"] // empty' <<<"${labels}")" \
      --arg llamaBuildNumber "$(jq -r '.["io.kama.llama.cpp.build-number"] // empty' <<<"${labels}")" \
      --arg llamaSourceSHA256 "$(jq -r '.["io.kama.llama.cpp.source-sha256"] // empty' <<<"${labels}")" \
      --arg cudaVersion "$(jq -r '.["io.kama.cuda.version"] // empty' <<<"${labels}")" \
      --argjson verified "${verified}" '{
        role: $role,
        image: $image,
        method: $method,
        resolvedDigest: $resolvedDigest,
        source: $source,
        revision: $revision,
        llamaCommit: $llamaCommit,
        llamaBuildNumber: $llamaBuildNumber,
        llamaSourceSHA256: $llamaSourceSHA256,
        cudaVersion: $cudaVersion,
        verified: ($verified == 1)
      }' >>"${records_file}"

    case "${role}" in
      manager) manager_observed_digest="${resolved_digest}" ;;
      importer) importer_observed_digest="${resolved_digest}" ;;
      servingClient) fixtures_observed_digest="${resolved_digest}" ;;
      runtimeCPU) runtime_cpu_observed_digest="${resolved_digest}" ;;
      runtimeCUDA) runtime_cuda_observed_digest="${resolved_digest}" ;;
    esac
    if [[ "${role}" == runtimeCUDA ]]; then
      provenance_method="$(jq -r '.method // "unavailable"' <<<"${result:-null}" 2>/dev/null || printf unavailable)"
      provenance_revision="$(jq -r '.["org.opencontainers.image.revision"] // empty' <<<"${labels}")"
      provenance_source="$(jq -r '.["org.opencontainers.image.source"] // empty' <<<"${labels}")"
      provenance_llama_commit="$(jq -r '.["io.kama.llama.cpp.commit"] // empty' <<<"${labels}")"
      provenance_llama_build_number="$(jq -r '.["io.kama.llama.cpp.build-number"] // empty' <<<"${labels}")"
      provenance_llama_source_sha256="$(jq -r '.["io.kama.llama.cpp.source-sha256"] // empty' <<<"${labels}")"
      provenance_cuda_version="$(jq -r '.["io.kama.cuda.version"] // empty' <<<"${labels}")"
    fi
  done

  jq -s --argjson sourceClean "${source_clean}" '{
    sourceCheckoutClean: ($sourceClean == 1),
    images: .
  }' "${records_file}" >"${evidence_dir}/image-provenance.json"
  if [[ ${source_clean} -eq 1 && ${all_verified} -eq 1 ]]; then
    provenance_verified=1
  fi
}

verify_supply_chain() {
  local records_file="${tmp_dir}/supply-chain.jsonl"
  local identity_regexp='^https://github\.com/TannerBurns/kama/\.github/workflows/release\.yml@refs/tags/v[0-9A-Za-z._-]+$'
  local issuer='https://token.actions.githubusercontent.com'
  local all_verified=1
  local role=""
  local image=""
  local signature_verified=0
  local sbom_verified=0
  : >"${records_file}"

  for role in manager importer servingClient runtimeCPU runtimeCUDA; do
    case "${role}" in
      manager) image="${manager_image}" ;;
      importer) image="${importer_image}" ;;
      servingClient) image="${fixtures_image}" ;;
      runtimeCPU) image="${runtime_cpu_image}" ;;
      runtimeCUDA) image="${runtime_cuda_image}" ;;
    esac
    signature_verified=0
    sbom_verified=0
    if "${cosign_bin}" verify \
      --certificate-identity-regexp "${identity_regexp}" \
      --certificate-oidc-issuer "${issuer}" \
      --certificate-github-workflow-name Release \
      --certificate-github-workflow-repository TannerBurns/kama \
      --certificate-github-workflow-sha "${revision}" \
      --certificate-github-workflow-trigger push \
      "${image}" >"${tmp_dir}/cosign-${role}-signature.json" 2>/dev/null; then
      signature_verified=1
    fi
    if "${cosign_bin}" verify-attestation \
      --type spdxjson \
      --certificate-identity-regexp "${identity_regexp}" \
      --certificate-oidc-issuer "${issuer}" \
      --certificate-github-workflow-name Release \
      --certificate-github-workflow-repository TannerBurns/kama \
      --certificate-github-workflow-sha "${revision}" \
      --certificate-github-workflow-trigger push \
      "${image}" >"${tmp_dir}/cosign-${role}-sbom.json" 2>/dev/null; then
      sbom_verified=1
    fi
    if [[ ${signature_verified} -ne 1 || ${sbom_verified} -ne 1 ]]; then
      all_verified=0
    fi
    jq -n \
      --arg role "${role}" \
      --arg image "${image}" \
      --argjson signatureVerified "${signature_verified}" \
      --argjson sbomAttestationVerified "${sbom_verified}" '{
      role: $role,
      image: $image,
      signatureVerified: ($signatureVerified == 1),
      sbomAttestationVerified: ($sbomAttestationVerified == 1)
    }' >>"${records_file}"
  done

  jq -s \
    --arg identityRegexp "${identity_regexp}" \
    --arg issuer "${issuer}" \
    --arg commit "${revision}" \
    --argjson verified "${all_verified}" '{
    verified: ($verified == 1),
    certificateIdentityRegexp: $identityRegexp,
    certificateOIDCIssuer: $issuer,
    workflowCommit: $commit,
    images: .
  }' "${records_file}" >"${evidence_dir}/supply-chain.json"
  if [[ ${all_verified} -eq 1 ]]; then
    supply_chain_verified=1
  fi
}

# Qualification requires registry-image provenance that binds the immutable
# production and serving-client images to this checkout and the pinned llama.cpp/CUDA
# sources. The functional hardware test may still run when neither supported OCI inspector
# is available, but its evidence remains explicitly non-qualifying.
verify_image_provenance
verify_supply_chain

"${kubectl_bin}" version -o json >"${evidence_dir}/kubernetes-version.json"
server_major="$(jq -r '.serverVersion.major // empty' "${evidence_dir}/kubernetes-version.json")"
server_minor="$(jq -r '.serverVersion.minor // empty | sub("[+]$"; "")' \
  "${evidence_dir}/kubernetes-version.json")"
if [[ "${server_major}" != "1" || ! "${server_minor}" =~ ^(34|35|36)$ ]]; then
  echo "protected NVIDIA cluster must run a supported Kubernetes 1.34, 1.35, or 1.36 API server" >&2
  exit 1
fi
kubernetes_minor_verified=1

wait_for_nvidia_serving() {
  local status
  for _ in $(seq 1 300); do
    status="$("${kubectl_bin}" -n "${namespace}" get modeldeployment e2e-serving-nvidia -o json 2>/dev/null || true)"
    if [[ -n "${status}" ]] && jq -e \
      --arg commit "${llama_commit}" \
      --arg digest "${model_digest}" \
      --arg artifactName "e2e-serving-nvidia-model" \
      --arg desiredImage "${runtime_cuda_image}" \
      --arg runtimeDigest "${runtime_cuda_observed_digest}" '
      .status.observedGeneration == .metadata.generation and
      .status.desiredReplicas == 1 and
      .status.readyReplicas == 1 and
      .status.artifact.name == $artifactName and
      (.status.artifact.uid | type == "string" and length > 0) and
      .status.artifact.digest == $digest and
      (.status.deploymentRef.name | type == "string" and length > 0) and
      (.status.deploymentRef.uid | type == "string" and length > 0) and
      (.status.serviceRef.name | type == "string" and length > 0) and
      (.status.serviceRef.uid | type == "string" and length > 0) and
      .status.runtime.state == "Ready" and
      .status.runtime.desiredImage == $desiredImage and
      (.status.runtime.observedImage | type == "string" and
        test("@sha256:[a-f0-9]{64}$") and
        ($runtimeDigest == "" or endswith("@" + $runtimeDigest))) and
      .status.runtime.llamaCommit == $commit and
      (.status.runtime.desiredFingerprint | type == "string" and length == 20) and
      .status.runtime.observedFingerprint == .status.runtime.desiredFingerprint and
      .status.runtime.loadedFingerprint == .status.runtime.desiredFingerprint and
      .status.runtime.effectiveContextTokens == 2048 and
      .status.runtime.effectiveConcurrency == 1 and
      .status.runtime.acceleratorDetected == true and
      .status.runtime.offloadedLayers > 0 and
      ([.status.conditions[] | select(.type == "RuntimeReady" and .status == "True")] | length) == 1 and
      ([.status.conditions[] | select(.type == "Serving" and .status == "True")] | length) == 1 and
      ([.status.conditions[] | select(.type == "Degraded" and .status == "False")] | length) == 1
    ' <<<"${status}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "timed out waiting for one-GPU serving with observed layer offload" >&2
  [[ -z "${status:-}" ]] || jq '.status' <<<"${status}" >&2
  return 1
}

run_in_cluster_serving_client() {
  local job_name=$1
  local service_name=$2
  local model_name=$3
  local manifest="${tmp_dir}/${job_name}.yaml"
  local completed=0
  local client_pods_json=""
  local client_pod_json=""

  sed \
    -e "s|KAMA_SERVING_CLIENT_NAME|${job_name}|g" \
    -e "s|KAMA_SERVING_CLIENT_IMAGE|${fixtures_image}|g" \
    -e 's|KAMA_SERVING_CLIENT_PULL_POLICY|IfNotPresent|g' \
    -e "s|KAMA_SERVING_SERVICE|${service_name}|g" \
    -e "s|KAMA_SERVING_NAMESPACE|${namespace}|g" \
    -e "s|KAMA_SERVING_MODEL|${model_name}|g" \
    "${repo_root}/test/e2e/serving/client-job.yaml.tmpl" >"${manifest}"
  "${kubectl_bin}" -n "${namespace}" apply -f "${manifest}"
  if "${kubectl_bin}" -n "${namespace}" wait --for=condition=Complete \
    "job/${job_name}" --timeout=4m; then
    completed=1
  fi
  "${kubectl_bin}" -n "${namespace}" logs "job/${job_name}" --container client \
    >"${evidence_dir}/direct-request.log" 2>&1 || true
  "${kubectl_bin}" -n "${namespace}" get job "${job_name}" -o json | jq '{
    metadata: {name: .metadata.name, uid: .metadata.uid},
    spec: {activeDeadlineSeconds: .spec.activeDeadlineSeconds, backoffLimit: .spec.backoffLimit},
    status: .status
  }' >"${evidence_dir}/direct-request-job.json" 2>/dev/null || true
  if [[ ${completed} -ne 1 ]] || ! jq -e \
    '.schemaVersion == 1 and .sseDataEvents > 0 and .done == true' \
    "${evidence_dir}/direct-request.log" >/dev/null 2>&1; then
    echo "in-cluster NVIDIA serving client did not observe a complete SSE response" >&2
    return 1
  fi

  client_pods_json="$("${kubectl_bin}" -n "${namespace}" get pods \
    -l "batch.kubernetes.io/job-name=${job_name}" -o json)"
  if ! jq -e '(.items | length) == 1' <<<"${client_pods_json}" >/dev/null; then
    echo "serving-client Job did not create exactly one Pod" >&2
    return 1
  fi
  client_pod_name="$(jq -r '.items[0].metadata.name' <<<"${client_pods_json}")"
  client_pod_uid="$(jq -r '.items[0].metadata.uid' <<<"${client_pods_json}")"
  client_pod_json="$(jq -c '.items[0]' <<<"${client_pods_json}")"
  jq --arg requestedImage "${fixtures_image}" '
    ([.spec.containers[] | select(.name == "client")][0]) as $container |
    ([.status.containerStatuses[] | select(.name == "client")][0]) as $status |
    {
      metadata: {name: .metadata.name, uid: .metadata.uid},
      requestedImage: $container.image,
      resolvedImage: $status.imageID,
      ready: ([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) == 1,
      succeeded: (.status.phase == "Succeeded" and $status.state.terminated.exitCode == 0),
      restartCount: $status.restartCount,
      automountServiceAccountToken: .spec.automountServiceAccountToken,
      runAsNonRoot: .spec.securityContext.runAsNonRoot,
      runAsUser: .spec.securityContext.runAsUser,
      runAsGroup: .spec.securityContext.runAsGroup,
      seccompProfile: .spec.securityContext.seccompProfile.type,
      allowPrivilegeEscalation: $container.securityContext.allowPrivilegeEscalation,
      readOnlyRootFilesystem: $container.securityContext.readOnlyRootFilesystem,
      capabilitiesDropAll: (($container.securityContext.capabilities.drop // []) | index("ALL") != null),
      requestedImageMatches: ($container.image == $requestedImage)
    }
  ' <<<"${client_pod_json}" >"${evidence_dir}/client-pod.json"
  if ! jq -e --arg image "${fixtures_image}" --arg digest "${fixtures_observed_digest}" '
    .requestedImage == $image and .requestedImageMatches == true and
    (.resolvedImage | type == "string" and test("@sha256:[a-f0-9]{64}$") and
      ($digest == "" or endswith("@" + $digest))) and
    .ready == false and .succeeded == true and .restartCount == 0 and
    .automountServiceAccountToken == false and .runAsNonRoot == true and
    .runAsUser == 65532 and .runAsGroup == 65532 and .seccompProfile == "RuntimeDefault" and
    .allowPrivilegeEscalation == false and .readOnlyRootFilesystem == true and
    .capabilitiesDropAll == true
  ' "${evidence_dir}/client-pod.json" >/dev/null; then
    echo "serving-client Pod did not preserve its immutable image and restricted security contract" >&2
    return 1
  fi
  client_resolved_image="$(jq -r '.resolvedImage' "${evidence_dir}/client-pod.json")"
  client_restart_count="$(jq -r '.restartCount' "${evidence_dir}/client-pod.json")"
  client_completed=${completed}
}

if "${kubectl_bin}" get namespace "${namespace}" >/dev/null 2>&1; then
  echo "namespace ${namespace} already exists; refusing to adopt protected-cluster resources" >&2
  exit 1
fi
if "${helm_bin}" status "${release}" --namespace "${namespace}" >/dev/null 2>&1; then
  echo "Helm release ${namespace}/${release} already exists; refusing to adopt it" >&2
  exit 1
fi
if ! "${kubectl_bin}" get storageclass "${storage_class}" >/dev/null 2>&1; then
  echo "storage class ${storage_class} is unavailable on the protected cluster" >&2
  exit 1
fi

gpu_total="$("${kubectl_bin}" get nodes -o json | jq '[.items[].status.allocatable["nvidia.com/gpu"] // "0" | tonumber] | add // 0')"
gpu_product=""
if [[ "${gpu_total}" -lt 1 ]]; then
  echo "protected cluster does not expose an allocatable full NVIDIA GPU" >&2
  exit 1
fi

OUTPUT_DIR="${repo_root}/dist" HELM="${helm_bin}" bash "${repo_root}/hack/helm-package.sh"
chart_package="${repo_root}/dist/kama-${version}.tgz"
"${helm_bin}" show crds "${chart_package}" | "${kubectl_bin}" apply --server-side \
  --field-manager=kama-crd-upgrade -f -
"${kubectl_bin}" wait --for=condition=Established --timeout=2m \
  crd/modelcaches.kama.tannerburns.github.io \
  crd/modelartifacts.kama.tannerburns.github.io \
  crd/modeldeployments.kama.tannerburns.github.io

"${kubectl_bin}" create namespace "${namespace}"
namespace_created=1

manager_repository="${manager_image%@sha256:*}"
manager_digest="${manager_image##*@}"
importer_repository="${importer_image%@sha256:*}"
importer_digest="${importer_image##*@}"
runtime_cpu_repository="${runtime_cpu_image%@sha256:*}"
runtime_cpu_digest="${runtime_cpu_image##*@}"
runtime_cuda_repository="${runtime_cuda_image%@sha256:*}"
runtime_cuda_digest="${runtime_cuda_image##*@}"

"${helm_bin}" upgrade --install "${release}" "${chart_package}" \
  --namespace "${namespace}" \
  --set "image.repository=${manager_repository}" \
  --set-string "image.digest=${manager_digest}" \
  --set "importer.image.repository=${importer_repository}" \
  --set-string "importer.image.digest=${importer_digest}" \
  --set "runtime.cpu.image.repository=${runtime_cpu_repository}" \
  --set-string "runtime.cpu.image.digest=${runtime_cpu_digest}" \
  --set "runtime.cuda.image.repository=${runtime_cuda_repository}" \
  --set-string "runtime.cuda.image.digest=${runtime_cuda_digest}" \
  --set-string "runtime.llamaCommit=${llama_commit}" \
  --wait --timeout 5m
release_installed=1
"${kubectl_bin}" -n "${namespace}" rollout status "deployment/${release}" --timeout=3m

sed "s|E2E_NVIDIA_STORAGE_CLASS|${storage_class}|g" \
  "${repo_root}/test/e2e/serving/nvidia-storage.yaml.tmpl" >"${tmp_dir}/nvidia-storage.yaml"
"${kubectl_bin}" -n "${namespace}" apply -f "${tmp_dir}/nvidia-storage.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=Ready=True \
  modelcache/e2e-serving-nvidia-cache --timeout=10m
"${kubectl_bin}" -n "${namespace}" wait --for=condition=Ready=True \
  modelartifact/e2e-serving-nvidia-model --timeout=20m
"${kubectl_bin}" -n "${namespace}" apply -f "${repo_root}/test/e2e/serving/nvidia-deployment.yaml"
wait_for_nvidia_serving

deployment_json="$("${kubectl_bin}" -n "${namespace}" get modeldeployment e2e-serving-nvidia -o json)"
service_name="$(jq -r '.status.serviceRef.name' <<<"${deployment_json}")"
workload_name="$(jq -r '.status.deploymentRef.name' <<<"${deployment_json}")"
runtime_pods_json="$("${kubectl_bin}" -n "${namespace}" get pods \
  -l kama.tannerburns.github.io/model-deployment=e2e-serving-nvidia -o json)"
if ! jq -e '([.items[] | select(.status.phase == "Running")] | length) == 1' \
  <<<"${runtime_pods_json}" >/dev/null; then
  echo "NVIDIA serving status did not identify exactly one running Pod" >&2
  exit 1
fi
pod_name="$(jq -r '[.items[] | select(.status.phase == "Running")][0].metadata.name' \
  <<<"${runtime_pods_json}")"
if [[ -z "${service_name}" || -z "${workload_name}" || -z "${pod_name}" ]]; then
  echo "NVIDIA serving status did not identify generated resources" >&2
  exit 1
fi

workload_json="$("${kubectl_bin}" -n "${namespace}" get deployment "${workload_name}" -o json)"
service_json="$("${kubectl_bin}" -n "${namespace}" get service "${service_name}" -o json)"
artifact_json="$("${kubectl_bin}" -n "${namespace}" get modelartifact e2e-serving-nvidia-model -o json)"
if ! jq -e \
  --arg artifactName "$(jq -r '.metadata.name' <<<"${artifact_json}")" \
  --arg artifactUID "$(jq -r '.metadata.uid' <<<"${artifact_json}")" \
  --arg artifactDigest "$(jq -r '.status.artifactDigest' <<<"${artifact_json}")" \
  --arg deploymentName "${workload_name}" \
  --arg deploymentUID "$(jq -r '.metadata.uid' <<<"${workload_json}")" \
  --arg serviceName "${service_name}" \
  --arg serviceUID "$(jq -r '.metadata.uid' <<<"${service_json}")" '
  .status.artifact.name == $artifactName and
  .status.artifact.uid == $artifactUID and
  .status.artifact.digest == $artifactDigest and
  .status.deploymentRef.name == $deploymentName and
  .status.deploymentRef.uid == $deploymentUID and
  .status.serviceRef.name == $serviceName and
  .status.serviceRef.uid == $serviceUID
' <<<"${deployment_json}" >/dev/null; then
  echo "ModelDeployment status resource references do not match generated object identities" >&2
  exit 1
fi
fingerprint="$(jq -r '.status.runtime.desiredFingerprint' <<<"${deployment_json}")"
if ! jq -e --arg image "${runtime_cuda_image}" --arg fingerprint "${fingerprint}" '
  .spec.replicas == 1 and
  .spec.strategy.type == "Recreate" and
  .spec.template.metadata.annotations["kama.tannerburns.github.io/runtime-fingerprint-full"] == $fingerprint and
  ([.spec.template.spec.containers[] | select(.name == "runtime") |
    .image == $image and
    .resources.requests["nvidia.com/gpu"] == "1" and
    .resources.limits["nvidia.com/gpu"] == "1"] | length) == 1
' <<<"${workload_json}" >/dev/null; then
  echo "generated accelerator workload does not request one immutable CUDA runtime and one full GPU" >&2
  exit 1
fi

pod_json="$("${kubectl_bin}" -n "${namespace}" get pod "${pod_name}" -o json)"
pod_uid="$(jq -r '.metadata.uid' <<<"${pod_json}")"
pod_node="$(jq -r '.spec.nodeName' <<<"${pod_json}")"
if ! jq -e --arg observedImage "$(jq -r '.status.runtime.observedImage' <<<"${deployment_json}")" '
  ([.status.containerStatuses[] | select(
    .name == "runtime" and
    .ready == true and
    .restartCount == 0 and
    .imageID == $observedImage
  )] | length) == 1
' <<<"${pod_json}" >/dev/null; then
  echo "NVIDIA serving Pod is not ready with restartCount=0 and the observed image identity" >&2
  exit 1
fi

endpoint_slices_json="$("${kubectl_bin}" -n "${namespace}" get endpointslice \
  -l "kubernetes.io/service-name=${service_name}" -o json)"
if ! jq -e --arg podUID "${pod_uid}" --arg podIP "$(jq -r '.status.podIP' <<<"${pod_json}")" '
  ([.items[].endpoints[]? | select(
    .conditions.ready == true and .targetRef.kind == "Pod" and .targetRef.uid == $podUID and
    (.addresses | index($podIP) != null)
  )] | length) == 1 and
  ([.items[].endpoints[]? | select(.conditions.ready == true)] | length) == 1
' <<<"${endpoint_slices_json}" >/dev/null; then
  echo "serving Service does not publish exactly one ready endpoint for the current Pod UID" >&2
  exit 1
fi

scheduled_node_json="$("${kubectl_bin}" get node "${pod_node}" -o json)"
gpu_product="$(jq -r '.metadata.labels["nvidia.com/gpu.product"] // empty' <<<"${scheduled_node_json}")"
if [[ -z "${gpu_product}" ]] || ! jq -e '
  (.status.allocatable["nvidia.com/gpu"] // "0" | tonumber) > 0
' <<<"${scheduled_node_json}" >/dev/null; then
  echo "scheduled runtime Pod node does not expose a labeled allocatable NVIDIA GPU" >&2
  exit 1
fi

nvidia_query="$("${kubectl_bin}" -n "${namespace}" exec "${pod_name}" --container runtime -- \
  nvidia-smi --query-gpu=name,uuid,driver_version --format=csv,noheader,nounits)"
if [[ "$(sed '/^[[:space:]]*$/d' <<<"${nvidia_query}" | wc -l | tr -d '[:space:]')" != "1" ]]; then
  echo "allocated runtime container did not expose exactly one queryable NVIDIA device" >&2
  exit 1
fi
IFS=',' read -r observed_gpu_device observed_gpu_uuid observed_driver_version <<<"${nvidia_query}"
observed_gpu_device="$(sed 's/^[[:space:]]*//;s/[[:space:]]*$//' <<<"${observed_gpu_device}")"
observed_gpu_uuid="$(sed 's/^[[:space:]]*//;s/[[:space:]]*$//' <<<"${observed_gpu_uuid}")"
observed_driver_version="$(sed 's/^[[:space:]]*//;s/[[:space:]]*$//' <<<"${observed_driver_version}")"
declared_cuda_version="$("${kubectl_bin}" -n "${namespace}" exec "${pod_name}" --container runtime -- \
  printenv CUDA_VERSION | tr -d '\r\n')"
cuda_version_metadata="$("${kubectl_bin}" -n "${namespace}" exec "${pod_name}" --container runtime -- \
  cat /usr/local/cuda/version.json)"
observed_cuda_version="$(jq -r '.cuda.version // empty' <<<"${cuda_version_metadata}")"
package_inventory="$("${kubectl_bin}" -n "${namespace}" exec "${pod_name}" --container runtime -- \
  dpkg-query -W '-f=${binary:Package}\t${Version}\n')"
cuda_package_inventory="$(awk '$1 ~ /^(cuda-|libcu|nvidia)/ {print}' <<<"${package_inventory}")"
linked_libraries="$("${kubectl_bin}" -n "${namespace}" exec "${pod_name}" --container runtime -- \
  ldd /usr/local/bin/llama-server)"
if [[ -z "${observed_gpu_device}" || -z "${observed_gpu_uuid}" || \
  "${observed_driver_version}" != "${driver_version}" || \
  "${declared_cuda_version}" != "${cuda_version}" || \
  "${observed_cuda_version}" != "${cuda_version}" ]] ||
  ! awk '$1 ~ /^cuda-cudart-12-4(:amd64)?$/ && $2 ~ /^12\.4\./ {found=1} END {exit !found}' \
    <<<"${cuda_package_inventory}" ||
  ! grep -Eq 'libcudart\.so\.12[[:space:]]+=>[[:space:]]+/[^[:space:]]+' <<<"${linked_libraries}" ||
  ! grep -Eq 'libcublas\.so\.12[[:space:]]+=>[[:space:]]+/[^[:space:]]+' <<<"${linked_libraries}"; then
  echo "runtime-observed GPU/driver/CUDA facts do not match protected inputs" >&2
  exit 1
fi
jq -n \
  --arg expectedVersion "${cuda_version}" \
  --arg declaredVersion "${declared_cuda_version}" \
  --arg observedVersion "${observed_cuda_version}" \
  --argjson versionMetadata "${cuda_version_metadata}" \
  --arg packages "${cuda_package_inventory}" \
  --arg linkedLibraries "${linked_libraries}" '{
  expectedVersion: $expectedVersion,
  declaredVersion: $declaredVersion,
  observedVersion: $observedVersion,
  versionMetadata: $versionMetadata,
  packages: ($packages | split("\n") | map(select(length > 0))),
  linkedLibraries: ($linkedLibraries | split("\n") | map(select(length > 0)))
}' >"${evidence_dir}/cuda-runtime.json"

run_in_cluster_serving_client e2e-serving-nvidia-client "${service_name}" e2e-serving-nvidia
jq -n \
  --arg clientImage "${fixtures_image}" \
  --arg clientResolvedImage "${client_resolved_image}" \
  --arg clientPod "${client_pod_name}" \
  --arg job "e2e-serving-nvidia-client" \
  --arg serviceDNS "${service_name}.${namespace}.svc" \
  --arg gpuProduct "${gpu_product}" \
  --arg gpuDevice "${observed_gpu_device}" \
  --arg gpuUUID "${observed_gpu_uuid}" \
  --arg driverVersion "${observed_driver_version}" \
  --arg cudaVersion "${observed_cuda_version}" \
  --argjson completed "${client_completed}" \
  --argjson restartCount "${client_restart_count}" '{
  transport: "in-cluster ClusterIP Service DNS",
  route: "/v1/chat/completions",
  stream: true,
  completed: ($completed == 1),
  clientImage: $clientImage,
  clientResolvedImage: $clientResolvedImage,
  clientPod: $clientPod,
  job: $job,
  serviceDNS: $serviceDNS,
  gpuProduct: $gpuProduct,
  gpuDevice: $gpuDevice,
  gpuUUID: $gpuUUID,
  driverVersion: $driverVersion,
  cudaVersion: $cudaVersion,
  restartCount: $restartCount
}' >"${evidence_dir}/direct-request.json"

"${kubectl_bin}" -n "${namespace}" port-forward "pod/${pod_name}" 18081:8081 \
  >"${tmp_dir}/supervisor-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18081/readyz
curl --fail --silent --show-error http://127.0.0.1:18081/state |
  jq '{
    schemaVersion,
    phase,
    ready,
    reason,
    message,
    deployment: {
      namespace: .deployment.namespace,
      name: .deployment.name,
      uid: .deployment.uid,
      fingerprint: .deployment.fingerprint
    },
    artifact,
    runtime: {
      mode: .runtime.mode,
      effectiveContextTokens: .runtime.effectiveContextTokens,
      desiredConcurrency: .runtime.desiredConcurrency,
      llamaCPPCommit: .runtime.llamaCPPCommit,
      llamaCPPBuildNumber: .runtime.llamaCPPBuildNumber,
      acceleratorDetected: .runtime.acceleratorDetected,
      visibleAccelerators: .runtime.visibleAccelerators,
      offloadedLayers: .runtime.offloadedLayers,
      totalLayers: .runtime.totalLayers,
      acceleratorDevice: .runtime.acceleratorDevice
    },
    child,
    observedAt
  }' \
    >"${evidence_dir}/supervisor-state.json"
if ! jq -e \
  --arg commit "${llama_commit}" \
  --arg build "${llama_build_number}" \
  --arg deploymentUID "$(jq -r '.metadata.uid' <<<"${deployment_json}")" \
  --arg fingerprint "${fingerprint}" \
  --arg artifactUID "$(jq -r '.metadata.uid' <<<"${artifact_json}")" \
  --arg artifactDigest "${model_digest}" '
  .phase == "Ready" and .ready == true and
  .deployment.uid == $deploymentUID and .deployment.fingerprint == $fingerprint and
  .artifact.uid == $artifactUID and .artifact.digest == $artifactDigest and
  .runtime.llamaCPPCommit == $commit and .runtime.llamaCPPBuildNumber == $build and
  .runtime.mode == "Accelerator" and
  .runtime.effectiveContextTokens == 2048 and .runtime.desiredConcurrency == 1 and
  .runtime.acceleratorDetected == true and .runtime.visibleAccelerators == 1 and
  .runtime.offloadedLayers > 0 and .runtime.totalLayers == .runtime.offloadedLayers and
  (.runtime.acceleratorDevice | type == "string" and length > 0)
' "${evidence_dir}/supervisor-state.json" >/dev/null; then
  echo "sanitized supervisor state does not prove the expected GPU offload" >&2
  exit 1
fi

passed=1
