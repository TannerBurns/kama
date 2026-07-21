#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(tr -d '\r\n' < "${repo_root}/VERSION")"
kind_bin="${KIND:-${repo_root}/bin/kind}"
kubectl_bin="${KUBECTL:-kubectl}"
helm_bin="${HELM:-${repo_root}/bin/helm}"
cluster_name="${KIND_CLUSTER:-kama-e2e-serving-cpu}"
node_image="${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must be a digest-pinned Kind node image}"
expected_k8s_minor="${K8S_MINOR:?K8S_MINOR is required}"
namespace="kama-system"
manager_image="${IMG:-local/kama-manager:${version}}"
importer_image="${IMPORTER_IMG:-local/kama-importer:${version}}"
fixtures_image="${FIXTURES_IMG:-local/kama-test-fixtures:${version}}"
runtime_cpu_image="${RUNTIME_CPU_IMG:-local/kama-runtime-cpu:${version}}"
llama_commit="${LLAMA_CPP_COMMIT:?LLAMA_CPP_COMMIT is required}"
llama_build_number="${LLAMA_CPP_BUILD_NUMBER:?LLAMA_CPP_BUILD_NUMBER is required}"
llama_source_sha256="${LLAMA_CPP_SOURCE_SHA256:?LLAMA_CPP_SOURCE_SHA256 is required}"
evidence_dir="${E2E_EVIDENCE_DIR:-${repo_root}/dist/e2e/serving-cpu}"
model_digest="48ab3034d0dd401fbc721eb1df3217902fee7dab9078992d66431f09b7750201"
model_entrypoint="smollm2-360m-instruct-q8_0.gguf"
revision="$(git -C "${repo_root}" rev-parse HEAD 2>/dev/null || printf unknown)"
created="$(git -C "${repo_root}" show -s --format=%cI HEAD 2>/dev/null || printf 1970-01-01T00:00:00Z)"
tmp_dir="$(mktemp -d)"
cluster_created=0
passed=0
manager_manifest_digest=""
importer_manifest_digest=""
fixtures_manifest_digest=""
runtime_manifest_digest=""
image_provenance_verified=0
source_clean=0
evidence_complete=0
kubernetes_minor_verified=0
redaction_verified=0
port_forward_pids=()

mkdir -p "${evidence_dir}"
printf '%s\n' 'FAIL: suite exited before evidence capture completed' >"${evidence_dir}/outcome.txt"
printf '%s\n' '{"schemaVersion":1,"suite":"baseline-serving/cpu","outcome":"failed before capture","qualifying":false}' \
  >"${evidence_dir}/qualification.json"

if [[ ! -x "${kind_bin}" ]]; then
  kind_bin="$(command -v kind || true)"
fi
if [[ ! -x "${helm_bin}" ]]; then
  helm_bin="$(command -v helm || true)"
fi
for tool in "${kind_bin}" "${kubectl_bin}" "${helm_bin}" docker curl git grep jq sed; do
  if [[ -z "${tool}" ]] || ! command -v "${tool}" >/dev/null 2>&1; then
    echo "required command is unavailable: ${tool:-unset}" >&2
    exit 1
  fi
done
if [[ ! "${node_image}" =~ @sha256:[a-f0-9]{64}$ ]]; then
  echo "KIND_NODE_IMAGE must include an immutable sha256 digest" >&2
  exit 2
fi
if [[ ! "${expected_k8s_minor}" =~ ^1\.(34|35|36)$ ]]; then
  echo "K8S_MINOR must be one of 1.34, 1.35, or 1.36" >&2
  exit 2
fi
if [[ -z "$(git -C "${repo_root}" status --porcelain)" ]]; then
  source_clean=1
fi

evidence_is_complete() {
  local required_file
  local required_json_file
  local required_files=(
    identities.json
    local-images.json
    kubernetes-version.json
    nodes.txt
    resources.txt
    events.txt
    modeldeployments.json
    endpointslices.json
    runtime-pods.json
    artifact-gating.json
    serving-contract.json
    direct-request.log
    direct-request.json
    direct-request-job.json
    supervisor-state.json
    delayed-loading.json
    drain.json
    load-failure.json
  )
  local required_json_files=(
    identities.json
    local-images.json
    kubernetes-version.json
    modeldeployments.json
    endpointslices.json
    runtime-pods.json
    artifact-gating.json
    serving-contract.json
    direct-request.json
    direct-request-job.json
    supervisor-state.json
    delayed-loading.json
    drain.json
    load-failure.json
  )
  for required_file in "${required_files[@]}"; do
    if [[ ! -s "${evidence_dir}/${required_file}" ]]; then
      echo "CPU acceptance evidence is missing or empty: ${required_file}" >&2
      return 1
    fi
  done
  for required_json_file in "${required_json_files[@]}"; do
    if ! jq empty "${evidence_dir}/${required_json_file}" >/dev/null 2>&1; then
      echo "CPU acceptance evidence is not valid JSON: ${required_json_file}" >&2
      return 1
    fi
  done
  if ! jq -e '
    .serviceCreated == true and .artifactReady == false and
    .desiredReplicas == 0 and .readyReplicas == 0 and
    .deploymentCreated == false and .podCreated == false and .readyEndpoint == false
  ' "${evidence_dir}/artifact-gating.json" >/dev/null ||
    ! jq -e '
      .validated == true and .artifactIdentityMatched == true and
      .runtimeIdentityMatched == true and .restrictedWorkload == true and
      .servicePortContract == true and .rwoPlacementMatched == true
    ' "${evidence_dir}/serving-contract.json" >/dev/null ||
    ! jq -e '.completed == true and .stream == true' \
      "${evidence_dir}/direct-request.json" >/dev/null ||
    ! jq -e '
      .supervisorAliveWhileLoading == true and .runtimeReadyWhileLoading == false and
      .readyEndpointWhileLoading == false and .eventuallyRuntimeReady == true and
      .eventuallyReadyEndpoint == true and .restartCount == 0
    ' "${evidence_dir}/delayed-loading.json" >/dev/null ||
    ! jq -e '
      .firstEventObservedBeforeUpdate == true and .streamActiveBeforeUpdate == true and
      .completionAbsentBeforeUpdate == true and .readinessRemovedWhileActive == true and
      .activeStreamCompleted == true and .withinDrainTimeout == true and
      (.elapsedSeconds | type) == "number" and .elapsedSeconds <= .drainTimeoutSeconds and
      .oldPodUID != "" and .newPodUID != "" and .oldPodUID != .newPodUID
    ' "${evidence_dir}/drain.json" >/dev/null ||
    ! jq -e '
      .supervisorRunning == true and .restartCount == 0 and .readyEndpoint == false and
      .stableObservation == true and .pod != "" and .podUID != ""
    ' "${evidence_dir}/load-failure.json" >/dev/null ||
    ! jq -e '
      .phase == "Ready" and .ready == true and .runtime.mode == "CPU" and
      .runtime.desiredConcurrency == 1 and .runtime.effectiveContextTokens == 2048 and
      .runtime.acceleratorDetected == false
    ' "${evidence_dir}/supervisor-state.json" >/dev/null ||
    ! jq -e '.serverVersion.gitVersion | type == "string"' \
      "${evidence_dir}/kubernetes-version.json" >/dev/null ||
    ! jq -e 'length == 4 and all(.[]; .id | test("^sha256:[a-f0-9]{64}$"))' \
      "${evidence_dir}/local-images.json" >/dev/null ||
    ! jq -e '.items | type == "array"' "${evidence_dir}/modeldeployments.json" >/dev/null ||
    ! jq -e '.items | type == "array"' "${evidence_dir}/endpointslices.json" >/dev/null ||
    ! jq -e '.items | type == "array"' "${evidence_dir}/runtime-pods.json" >/dev/null; then
    echo "CPU acceptance evidence failed semantic validation" >&2
    return 1
  fi
}

capture_evidence() {
  local outcome=$1
  local qualifying=0
  local immutable_digests_present=0
  local -a unsafe_evidence=()
  local -a runtime_pods=()
  local unsafe_file
  local runtime_pod
  printf '%s\n' "${outcome}" >"${evidence_dir}/outcome.txt"
  jq -n \
    --arg commit "${revision}" \
    --arg managerImage "${manager_image}" \
    --arg managerManifestDigest "${manager_manifest_digest}" \
    --arg importerImage "${importer_image}" \
    --arg importerManifestDigest "${importer_manifest_digest}" \
    --arg fixturesImage "${fixtures_image}" \
    --arg fixturesManifestDigest "${fixtures_manifest_digest}" \
    --arg runtimeImage "${runtime_cpu_image}" \
    --arg runtimeManifestDigest "${runtime_manifest_digest}" \
    --argjson imageProvenanceVerified "${image_provenance_verified}" \
    --argjson sourceClean "${source_clean}" \
    --arg llamaCommit "${llama_commit}" \
    --arg llamaBuildNumber "${llama_build_number}" \
    --arg llamaSourceSHA256 "${llama_source_sha256}" \
    --arg kubernetesMinor "${expected_k8s_minor}" \
    --arg kindNodeImage "${node_image}" \
    --arg modelDigest "${model_digest}" '{
      commit: $commit,
      images: {
        manager: {reference: $managerImage, manifestDigest: $managerManifestDigest},
        importer: {reference: $importerImage, manifestDigest: $importerManifestDigest},
        servingClient: {reference: $fixturesImage, manifestDigest: $fixturesManifestDigest},
        runtimeCPU: {
          reference: $runtimeImage,
          manifestDigest: $runtimeManifestDigest,
          provenanceVerified: ($imageProvenanceVerified == 1),
          sourceCheckoutClean: ($sourceClean == 1)
        }
      },
      llamaCommit: $llamaCommit,
      llamaBuildNumber: $llamaBuildNumber,
      llamaSourceSHA256: $llamaSourceSHA256,
      kubernetes: {expectedMinor: $kubernetesMinor, kindNodeImage: $kindNodeImage},
      modelDigest: $modelDigest
    }' >"${evidence_dir}/identities.json"
  docker image inspect "${manager_image}" "${importer_image}" "${fixtures_image}" "${runtime_cpu_image}" 2>/dev/null |
    jq '[.[] | {id: .Id, repoDigests: .RepoDigests, labels: .Config.Labels}]' \
      >"${evidence_dir}/local-images.json" || true
  if [[ ${cluster_created} -eq 1 ]]; then
    "${kubectl_bin}" version -o json >"${evidence_dir}/kubernetes-version.json" \
      2>"${evidence_dir}/kubernetes-version.stderr" || true
    "${kubectl_bin}" get nodes -o wide >"${evidence_dir}/nodes.txt" 2>&1 || true
    "${kubectl_bin}" -n "${namespace}" get modelartifact,modeldeployment,deploy,svc,job,pod \
      -o wide >"${evidence_dir}/resources.txt" 2>&1 || true
    "${kubectl_bin}" -n "${namespace}" get modeldeployment -o json | jq '{
      apiVersion,
      kind,
      items: [.items[] | {
        metadata: {name: .metadata.name, generation: .metadata.generation},
        spec: .spec,
        status: .status
      }]
    }' >"${evidence_dir}/modeldeployments.json" 2>/dev/null || true
    "${kubectl_bin}" -n "${namespace}" get endpointslice -o json | jq '{
      apiVersion,
      kind,
      items: [.items[] | {
        metadata: {name: .metadata.name, labels: .metadata.labels},
        endpoints: .endpoints,
        ports: .ports
      }]
    }' >"${evidence_dir}/endpointslices.json" 2>/dev/null || true
    "${kubectl_bin}" -n "${namespace}" get events --sort-by=.lastTimestamp \
      >"${evidence_dir}/events.txt" 2>&1 || true
    "${kubectl_bin}" -n "${namespace}" logs deployment/kama --container manager --tail=300 \
      >"${evidence_dir}/manager.log" 2>&1 || true
    mapfile -t runtime_pods < <("${kubectl_bin}" -n "${namespace}" get pods \
      -l "kama.tannerburns.github.io/model-deployment-uid" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
    for runtime_pod in "${runtime_pods[@]:-}"; do
      [[ -n "${runtime_pod}" ]] || continue
      "${kubectl_bin}" -n "${namespace}" logs "pod/${runtime_pod}" --container runtime --tail=500 \
        >"${evidence_dir}/runtime-${runtime_pod}.log" 2>&1 || true
    done
    "${kubectl_bin}" -n "${namespace}" get pods \
      -l "kama.tannerburns.github.io/model-deployment-uid" -o json | jq '{
      apiVersion,
      kind,
      items: [.items[] | {
        metadata: {name: .metadata.name, uid: .metadata.uid, labels: .metadata.labels, annotations: .metadata.annotations},
        spec: {
          nodeName: .spec.nodeName,
          securityContext: .spec.securityContext,
          containers: [.spec.containers[] | {
            name, image, resources, securityContext, startupProbe, livenessProbe, readinessProbe, volumeMounts
          }]
        },
        status: {
          phase: .status.phase,
          conditions: .status.conditions,
          containerStatuses: [.status.containerStatuses[]? | {
            name, ready, restartCount, image, imageID, state, lastState
          }]
        }
      }]
    }' >"${evidence_dir}/runtime-pods.json" 2>/dev/null || true
  fi

  if [[ "${manager_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ &&
    "${importer_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ &&
    "${fixtures_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ &&
    "${runtime_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    immutable_digests_present=1
  fi
  redaction_verified=0
  mapfile -t unsafe_evidence < <(
    grep -RIlE -i 'authorization:|bearer[[:space:]]|hf_[a-z0-9]+' \
      "${evidence_dir}" 2>/dev/null || true
  )
  if [[ ${#unsafe_evidence[@]} -eq 0 ]]; then
    redaction_verified=1
  else
    echo "credential-shaped content found in CPU serving evidence; replacing unsafe files" >&2
    for unsafe_file in "${unsafe_evidence[@]}"; do
      printf '%s\n' '[REDACTED: credential-shaped content removed]' >"${unsafe_file}"
    done
  fi
  evidence_complete=0
  if [[ ${passed} -eq 1 ]] && evidence_is_complete; then
    evidence_complete=1
  fi
  if [[ ${passed} -eq 1 && ${evidence_complete} -eq 1 && ${kubernetes_minor_verified} -eq 1 &&
    ${redaction_verified} -eq 1 &&
    ${image_provenance_verified} -eq 1 && ${immutable_digests_present} -eq 1 ]]; then
    qualifying=1
  fi
  jq -n \
    --arg outcome "${outcome}" \
    --argjson qualifying "${qualifying}" \
    --argjson evidenceComplete "${evidence_complete}" \
    --argjson kubernetesMinorVerified "${kubernetes_minor_verified}" \
    --argjson redactionVerified "${redaction_verified}" \
    --argjson imageProvenanceVerified "${image_provenance_verified}" \
    --argjson immutableManifestDigestsPresent "${immutable_digests_present}" '{
    schemaVersion: 1,
    suite: "baseline-serving/cpu",
    outcome: $outcome,
    qualifying: ($qualifying == 1),
    evidenceComplete: ($evidenceComplete == 1),
    kubernetesMinorVerified: ($kubernetesMinorVerified == 1),
    redactionVerified: ($redactionVerified == 1),
    imageProvenanceVerified: ($imageProvenanceVerified == 1),
    immutableManifestDigestsPresent: ($immutableManifestDigestsPresent == 1)
  }' >"${evidence_dir}/.qualification.json.tmp"
  mv "${evidence_dir}/.qualification.json.tmp" "${evidence_dir}/qualification.json"
}

cleanup() {
  local exit_code=$?
  for pid in "${port_forward_pids[@]:-}"; do
    kill "${pid}" >/dev/null 2>&1 || true
  done
  if [[ ${exit_code} -eq 0 && ${passed} -eq 1 ]]; then
    capture_evidence "PASS: CPU serving, drain, and terminal load failure verified"
  else
    capture_evidence "FAIL (exit ${exit_code})"
  fi
  if [[ ${passed} -eq 1 && (${evidence_complete} -ne 1 || ${redaction_verified} -ne 1) ]]; then
    exit_code=1
  fi
  if grep -RIEq -i 'authorization:|bearer[[:space:]]|hf_[a-z0-9]+' \
    "${evidence_dir}" >/dev/null 2>&1; then
    echo "credential-shaped content found in CPU serving evidence" >&2
    exit_code=1
  fi
  if [[ ${cluster_created} -eq 1 && "${KEEP_KIND_CLUSTER:-0}" != "1" ]]; then
    "${kind_bin}" delete cluster --name "${cluster_name}" || true
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

wait_for_modeldeployment() {
  local name=$1
  local predicate=$2
  local description=$3
  local attempts=${4:-180}
  local status
  for _ in $(seq 1 "${attempts}"); do
    status="$("${kubectl_bin}" -n "${namespace}" get modeldeployment "${name}" -o json 2>/dev/null || true)"
    if [[ -n "${status}" ]] && jq -e "${predicate}" <<<"${status}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "timed out waiting for ${description}" >&2
  [[ -z "${status:-}" ]] || jq '.status' <<<"${status}" >&2
  return 1
}

run_in_cluster_serving_client() {
  local job_name=$1
  local service_name=$2
  local model_name=$3
  local manifest="${tmp_dir}/${job_name}.yaml"
  local completed=0

  sed \
    -e "s|KAMA_SERVING_CLIENT_NAME|${job_name}|g" \
    -e "s|KAMA_SERVING_CLIENT_IMAGE|${fixtures_image}|g" \
    -e 's|KAMA_SERVING_CLIENT_PULL_POLICY|Never|g' \
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
    echo "in-cluster CPU serving client did not observe a complete SSE response" >&2
    return 1
  fi
  jq -n \
    --arg image "${fixtures_image}" \
    --arg imageDigest "${fixtures_manifest_digest}" \
    --arg job "${job_name}" \
    --arg serviceDNS "${service_name}.${namespace}.svc" '{
      transport: "in-cluster ClusterIP Service DNS",
      route: "/v1/chat/completions",
      stream: true,
      completed: true,
      clientImage: $image,
      clientImageManifestDigest: $imageDigest,
      job: $job,
      serviceDNS: $serviceDNS
    }' >"${evidence_dir}/direct-request.json"
}

if "${kind_bin}" get clusters | grep -Fxq "${cluster_name}"; then
  echo "Kind cluster ${cluster_name} already exists; choose a disposable KIND_CLUSTER" >&2
  exit 1
fi

buildx_available=0
if docker buildx version >/dev/null 2>&1; then
  buildx_available=1
  docker buildx build \
    --load \
    --metadata-file "${tmp_dir}/manager-build-metadata.json" \
    --file "${repo_root}/Dockerfile" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --tag "${manager_image}" "${repo_root}"
  manager_manifest_digest="$(jq -r '
    .["containerimage.digest"] // .["containerimage.descriptor"].digest // empty
  ' "${tmp_dir}/manager-build-metadata.json")"
  docker buildx build \
    --load \
    --metadata-file "${tmp_dir}/importer-build-metadata.json" \
    --file "${repo_root}/Dockerfile.importer" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --tag "${importer_image}" "${repo_root}"
  importer_manifest_digest="$(jq -r '
    .["containerimage.digest"] // .["containerimage.descriptor"].digest // empty
  ' "${tmp_dir}/importer-build-metadata.json")"
  docker buildx build \
    --load \
    --metadata-file "${tmp_dir}/fixtures-build-metadata.json" \
    --file "${repo_root}/Dockerfile.test-fixtures" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --tag "${fixtures_image}" "${repo_root}"
  fixtures_manifest_digest="$(jq -r '
    .["containerimage.digest"] // .["containerimage.descriptor"].digest // empty
  ' "${tmp_dir}/fixtures-build-metadata.json")"
  docker buildx build \
    --load \
    --metadata-file "${tmp_dir}/runtime-cpu-build-metadata.json" \
    --file "${repo_root}/Dockerfile.runtime-cpu" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --build-arg "LLAMA_CPP_COMMIT=${llama_commit}" \
    --build-arg "LLAMA_CPP_BUILD_NUMBER=${llama_build_number}" \
    --build-arg "LLAMA_CPP_SOURCE_SHA256=${llama_source_sha256}" \
    --tag "${runtime_cpu_image}" "${repo_root}"
  runtime_manifest_digest="$(jq -r '
    .["containerimage.digest"] // .["containerimage.descriptor"].digest // empty
  ' "${tmp_dir}/runtime-cpu-build-metadata.json")"
else
  docker build \
    --file "${repo_root}/Dockerfile" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --tag "${manager_image}" "${repo_root}"
  docker build \
    --file "${repo_root}/Dockerfile.importer" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --tag "${importer_image}" "${repo_root}"
  docker build \
    --file "${repo_root}/Dockerfile.test-fixtures" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --tag "${fixtures_image}" "${repo_root}"
  docker build \
    --file "${repo_root}/Dockerfile.runtime-cpu" \
    --build-arg "VERSION=${version}" \
    --build-arg "VCS_REF=${revision}" \
    --build-arg "CREATED=${created}" \
    --build-arg "LLAMA_CPP_COMMIT=${llama_commit}" \
    --build-arg "LLAMA_CPP_BUILD_NUMBER=${llama_build_number}" \
    --build-arg "LLAMA_CPP_SOURCE_SHA256=${llama_source_sha256}" \
    --tag "${runtime_cpu_image}" "${repo_root}"
fi
manager_labels="$(docker image inspect "${manager_image}" -f '{{json .Config.Labels}}')"
importer_labels="$(docker image inspect "${importer_image}" -f '{{json .Config.Labels}}')"
fixtures_labels="$(docker image inspect "${fixtures_image}" -f '{{json .Config.Labels}}')"
runtime_labels="$(docker image inspect "${runtime_cpu_image}" -f '{{json .Config.Labels}}')"
if [[ ${source_clean} -eq 1 && ${buildx_available} -eq 1 && \
  "${manager_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ && \
  "${importer_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ && \
  "${fixtures_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ && \
  "${runtime_manifest_digest}" =~ ^sha256:[a-f0-9]{64}$ ]] && \
  jq -e --arg revision "${revision}" '
    .["org.opencontainers.image.source"] == "https://github.com/TannerBurns/kama" and
    .["org.opencontainers.image.revision"] == $revision
  ' <<<"${manager_labels}" >/dev/null && \
  jq -e --arg revision "${revision}" '
    .["org.opencontainers.image.source"] == "https://github.com/TannerBurns/kama" and
    .["org.opencontainers.image.revision"] == $revision
  ' <<<"${importer_labels}" >/dev/null && \
  jq -e --arg revision "${revision}" '
    .["org.opencontainers.image.source"] == "https://github.com/TannerBurns/kama" and
    .["org.opencontainers.image.revision"] == $revision
  ' <<<"${fixtures_labels}" >/dev/null && jq -e \
  --arg revision "${revision}" \
  --arg commit "${llama_commit}" \
  --arg build "${llama_build_number}" \
  --arg sourceSHA256 "${llama_source_sha256}" '
    .["org.opencontainers.image.source"] == "https://github.com/TannerBurns/kama" and
    .["org.opencontainers.image.revision"] == $revision and
    .["io.kama.llama.cpp.commit"] == $commit and
    .["io.kama.llama.cpp.build-number"] == $build and
    .["io.kama.llama.cpp.source-sha256"] == $sourceSHA256
  ' <<<"${runtime_labels}" >/dev/null; then
  image_provenance_verified=1
fi

"${kind_bin}" create cluster --name "${cluster_name}" --image "${node_image}" \
  --config "${repo_root}/test/kind/cluster.yaml" --wait 5m
cluster_created=1
"${kubectl_bin}" version -o json >"${evidence_dir}/kubernetes-version.json" \
  2>"${evidence_dir}/kubernetes-version.stderr"
client_git_version="$(jq -r '.clientVersion.gitVersion // empty' \
  "${evidence_dir}/kubernetes-version.json")"
server_git_version="$(jq -r '.serverVersion.gitVersion // empty' \
  "${evidence_dir}/kubernetes-version.json")"
if [[ "${client_git_version}" != "v${expected_k8s_minor}."* ]]; then
  echo "kubectl ${client_git_version:-unknown} does not match K8S_MINOR=${expected_k8s_minor}" >&2
  exit 1
fi
if [[ "${server_git_version}" != "v${expected_k8s_minor}."* ]]; then
  echo "Kind API server ${server_git_version:-unknown} does not match K8S_MINOR=${expected_k8s_minor}" >&2
  exit 1
fi
kubernetes_minor_verified=1
"${kind_bin}" load docker-image --name "${cluster_name}" \
  "${manager_image}" "${importer_image}" "${fixtures_image}" "${runtime_cpu_image}"

"${kubectl_bin}" create namespace "${namespace}"
worker_node="${cluster_name}-worker"
docker exec "${worker_node}" mkdir -p /var/local/kama-e2e-serving-cache
docker exec "${worker_node}" chmod 0777 /var/local/kama-e2e-serving-cache
sed \
  -e "s|KAMA_WORKER_NODE|${worker_node}|g" \
  -e 's|e2e-hf-cache|e2e-serving-cache|g' \
  -e 's|kama-e2e-hf-cache|kama-e2e-serving-cache|g' \
  "${repo_root}/test/e2e/huggingface/public-storage.yaml" >"${tmp_dir}/storage.yaml"

OUTPUT_DIR="${repo_root}/dist" HELM="${helm_bin}" bash "${repo_root}/hack/helm-package.sh"
chart_package="${repo_root}/dist/kama-${version}.tgz"
"${helm_bin}" upgrade --install kama "${chart_package}" \
  --namespace "${namespace}" \
  --set "image.repository=${manager_image%:*}" \
  --set "image.tag=${manager_image##*:}" \
  --set image.pullPolicy=Never \
  --set "importer.image.repository=${importer_image%:*}" \
  --set "importer.image.tag=${importer_image##*:}" \
  --set importer.image.pullPolicy=Never \
  --set "runtime.cpu.image.repository=${runtime_cpu_image%:*}" \
  --set "runtime.cpu.image.tag=${runtime_cpu_image##*:}" \
  --set runtime.imagePullPolicy=Never \
  --set-string "runtime.llamaCommit=${llama_commit}" \
  --wait --timeout 5m
"${kubectl_bin}" wait --for=condition=Established --timeout=2m \
  crd/modeldeployments.kama.tannerburns.github.io
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama --timeout=3m

# A never-created artifact must produce only the stable Service. This runs before
# any ModelArtifact exists so a fast import cannot race the dependency-gating
# observation.
"${kubectl_bin}" -n "${namespace}" apply -f - <<'EOF'
apiVersion: kama.tannerburns.github.io/v1alpha1
kind: ModelDeployment
metadata:
  name: e2e-serving-artifact-gating
spec:
  modelRef:
    name: artifact-that-never-loaded
  placement:
    mode: CPU
  runtime:
    maxContextTokens: 2048
    desiredConcurrency: 1
    drainTimeout: 30s
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      memory: 512Mi
EOF
wait_for_modeldeployment e2e-serving-artifact-gating \
  '.status.observedGeneration == .metadata.generation and
   (.status.desiredReplicas // 0) == 0 and (.status.readyReplicas // 0) == 0 and
   .status.serviceRef.name != "" and .status.serviceRef.uid != "" and
   (.status.deploymentRef == null) and
   ([.status.conditions[] | select(.type == "ArtifactReady" and .status == "False" and
      .reason == "ArtifactNotFound")] | length) == 1 and
   ([.status.conditions[] | select(.type == "Serving" and .status == "False")] | length) == 1' \
  'the never-loaded artifact to gate serving'
gating_json="$("${kubectl_bin}" -n "${namespace}" get modeldeployment \
  e2e-serving-artifact-gating -o json)"
gating_service="$(jq -r '.status.serviceRef.name // empty' <<<"${gating_json}")"
gating_service_json="$("${kubectl_bin}" -n "${namespace}" get service "${gating_service}" -o json)"
gating_deployment_count="$("${kubectl_bin}" -n "${namespace}" get deployment \
  -l kama.tannerburns.github.io/model-deployment=e2e-serving-artifact-gating \
  -o json | jq '.items | length')"
gating_pod_count="$("${kubectl_bin}" -n "${namespace}" get pod \
  -l kama.tannerburns.github.io/model-deployment=e2e-serving-artifact-gating \
  -o json | jq '.items | length')"
gating_config_count="$("${kubectl_bin}" -n "${namespace}" get configmap \
  -l kama.tannerburns.github.io/model-deployment=e2e-serving-artifact-gating \
  -o json | jq '.items | length')"
gating_endpoints="$("${kubectl_bin}" -n "${namespace}" get endpointslice \
  -l "kubernetes.io/service-name=${gating_service}" -o json)"
if ! jq -e '.spec.type == "ClusterIP" and .spec.clusterIP != "" and .spec.clusterIP != "None"' \
  <<<"${gating_service_json}" >/dev/null ||
  [[ "${gating_deployment_count}" != "0" || "${gating_pod_count}" != "0" ||
    "${gating_config_count}" != "0" ]] ||
  jq -e 'any(.items[].endpoints[]?; .conditions.ready == true)' \
    <<<"${gating_endpoints}" >/dev/null; then
  echo "never-loaded artifact created a workload, runtime config, or ready endpoint" >&2
  exit 1
fi
jq -n \
  --arg modelDeployment "e2e-serving-artifact-gating" \
  --arg service "${gating_service}" \
  --argjson status "$(jq '.status' <<<"${gating_json}")" \
  --argjson configMapCount "${gating_config_count}" '{
    modelDeployment: $modelDeployment,
    service: $service,
    serviceCreated: true,
    artifactReady: false,
    desiredReplicas: 0,
    readyReplicas: 0,
    deploymentCreated: false,
    podCreated: false,
    runtimeConfigCreated: ($configMapCount > 0),
    readyEndpoint: false,
    status: $status
  }' >"${evidence_dir}/artifact-gating.json"
"${kubectl_bin}" -n "${namespace}" delete modeldeployment e2e-serving-artifact-gating \
  --wait --timeout=2m

"${kubectl_bin}" apply -f "${tmp_dir}/storage.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=Ready=True \
  modelcache/e2e-serving-cache --timeout=5m
sed \
  -e 's|e2e-hf-public|e2e-serving-model|g' \
  -e 's|e2e-hf-cache|e2e-serving-cache|g' \
  "${repo_root}/test/e2e/huggingface/public-artifact.yaml" >"${tmp_dir}/artifact.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/artifact.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=Ready=True \
  modelartifact/e2e-serving-model --timeout=20m

# Deliberately throttle a production CPU runtime, then temporarily stop only
# its llama-server child. This makes the Loading window deterministic without
# replacing the production image or changing the supervisor. The supervisor
# must remain live while readiness and endpoint publication are false; after
# the child continues, the same Pod must become ready without a restart.
"${kubectl_bin}" -n "${namespace}" apply \
  -f "${repo_root}/test/e2e/serving/cpu-delayed-deployment.yaml"
delayed_service=""
delayed_pod=""
for _ in $(seq 1 180); do
  delayed_json="$(${kubectl_bin} -n "${namespace}" get modeldeployment \
    e2e-serving-cpu-delayed -o json 2>/dev/null || true)"
  delayed_service="$(jq -r '.status.serviceRef.name // empty' <<<"${delayed_json:-null}")"
  delayed_pod="$(${kubectl_bin} -n "${namespace}" get pods \
    -l kama.tannerburns.github.io/model-deployment=e2e-serving-cpu-delayed \
    -o json 2>/dev/null | jq -r '[.items[] | select(.status.phase == "Running")][0].metadata.name // ""')"
  if [[ -n "${delayed_service}" && -n "${delayed_pod}" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "${delayed_service}" || -z "${delayed_pod}" ]]; then
  echo "delayed CPU scenario did not create its stable Service and running Pod" >&2
  exit 1
fi
"${kubectl_bin}" -n "${namespace}" port-forward "pod/${delayed_pod}" 18082:8081 \
  >"${tmp_dir}/delayed-supervisor-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18082/startupz
delayed_child_pid=""
for _ in $(seq 1 300); do
  if curl --fail --silent --show-error http://127.0.0.1:18082/state \
    >"${tmp_dir}/delayed-state.json"; then
    delayed_child_pid="$(jq -r 'select(.phase == "Loading" and .ready == false) | .child.pid // empty' \
      "${tmp_dir}/delayed-state.json")"
    if [[ "${delayed_child_pid}" =~ ^[1-9][0-9]*$ ]]; then
      break
    fi
    if jq -e '.ready == true or .phase == "LoadFailed" or .phase == "Exited"' \
      "${tmp_dir}/delayed-state.json" >/dev/null; then
      break
    fi
  fi
  sleep 0.1
done
if [[ ! "${delayed_child_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "delayed CPU scenario did not expose a loading llama-server child" >&2
  jq . "${tmp_dir}/delayed-state.json" >&2 || true
  exit 1
fi
"${kubectl_bin}" -n "${namespace}" exec "${delayed_pod}" --container runtime -- \
  sh -c 'kill -STOP "$1"' -- "${delayed_child_pid}"
delayed_loading_observed=0
for _ in $(seq 1 120); do
  curl --fail --silent --show-error http://127.0.0.1:18082/state \
    >"${tmp_dir}/delayed-state.json"
  if jq -e '.phase == "Loading" and .ready == false and .child.pid > 0' \
    "${tmp_dir}/delayed-state.json" >/dev/null; then
    delayed_loading_observed=1
    break
  fi
  if jq -e '.ready == true or .phase == "Failed" or .phase == "Exited"' \
    "${tmp_dir}/delayed-state.json" >/dev/null; then
    break
  fi
  sleep 1
done
if [[ ${delayed_loading_observed} -ne 1 ]]; then
  echo "throttled CPU runtime was not observed alive and loading before readiness" >&2
  jq . "${tmp_dir}/delayed-state.json" >&2
  exit 1
fi
wait_for_modeldeployment e2e-serving-cpu-delayed \
  '.status.observedGeneration == .metadata.generation and (.status.readyReplicas // 0) == 0 and
   ([.status.conditions[] | select(.type == "RuntimeReady" and .status == "False")] | length) == 1 and
   ([.status.conditions[] | select(.type == "Serving" and .status == "False")] | length) == 1' \
  'the deliberately throttled CPU runtime to remain unready'
delayed_endpoints="$(${kubectl_bin} -n "${namespace}" get endpointslice \
  -l "kubernetes.io/service-name=${delayed_service}" -o json)"
if jq -e 'any(.items[].endpoints[]?; .conditions.ready == true)' \
  <<<"${delayed_endpoints}" >/dev/null; then
  echo "delayed CPU runtime exposed a ready endpoint while still loading" >&2
  exit 1
fi
"${kubectl_bin}" -n "${namespace}" exec "${delayed_pod}" --container runtime -- \
  sh -c 'kill -CONT "$1"' -- "${delayed_child_pid}"
wait_for_modeldeployment e2e-serving-cpu-delayed \
  '.status.observedGeneration == .metadata.generation and .status.readyReplicas == 1 and
   .status.runtime.state == "Ready" and
   ([.status.conditions[] | select(.type == "RuntimeReady" and .status == "True")] | length) == 1 and
   ([.status.conditions[] | select(.type == "Serving" and .status == "True")] | length) == 1' \
  'the deliberately throttled CPU runtime to finish loading' 900
delayed_ready_pod_json="$(${kubectl_bin} -n "${namespace}" get pod "${delayed_pod}" -o json)"
if ! jq -e '
  ([.status.containerStatuses[] | select(.name == "runtime" and .ready == true and .restartCount == 0)] | length) == 1
' <<<"${delayed_ready_pod_json}" >/dev/null; then
  echo "delayed CPU runtime did not become ready in the same Pod with restartCount=0" >&2
  exit 1
fi
delayed_ready_endpoints="$(${kubectl_bin} -n "${namespace}" get endpointslice \
  -l "kubernetes.io/service-name=${delayed_service}" -o json)"
if ! jq -e 'any(.items[].endpoints[]?; .conditions.ready == true)' \
  <<<"${delayed_ready_endpoints}" >/dev/null; then
  echo "delayed CPU runtime became ready without publishing a ready endpoint" >&2
  exit 1
fi
jq -n \
  --slurpfile loading "${tmp_dir}/delayed-state.json" \
  --arg pod "${delayed_pod}" \
  --arg service "${delayed_service}" '{
    pod: $pod,
    service: $service,
    childTemporarilyStopped: true,
    supervisorAliveWhileLoading: true,
    runtimeReadyWhileLoading: false,
    readyEndpointWhileLoading: false,
    eventuallyRuntimeReady: true,
    eventuallyReadyEndpoint: true,
    restartCount: 0,
    loadingState: $loading[0]
  }' >"${evidence_dir}/delayed-loading.json"
"${kubectl_bin}" -n "${namespace}" delete modeldeployment e2e-serving-cpu-delayed \
  --wait --timeout=5m

"${kubectl_bin}" -n "${namespace}" apply -f "${repo_root}/test/e2e/serving/cpu-deployment.yaml"
wait_for_modeldeployment e2e-serving-cpu \
  '.status.observedGeneration == .metadata.generation and
   .status.desiredReplicas == 1 and .status.readyReplicas == 1 and
   .status.runtime.state == "Ready" and
   .status.runtime.llamaCommit == "'"${llama_commit}"'" and
   .status.artifact.digest == "'"${model_digest}"'" and
   ([.status.conditions[] | select(.type == "ArtifactReady" and .status == "True")] | length) == 1 and
   ([.status.conditions[] | select(.type == "ResourcesAvailable" and .status == "True")] | length) == 1 and
   ([.status.conditions[] | select(.type == "RuntimeReady" and .status == "True")] | length) == 1 and
   ([.status.conditions[] | select(.type == "Serving" and .status == "True")] | length) == 1 and
   ([.status.conditions[] | select(.type == "Degraded" and .status == "True" and .reason == "CPUOnlyRequested")] | length) == 1' \
  'the CPU model to become serving and degraded'

deployment_json="$("${kubectl_bin}" -n "${namespace}" get modeldeployment e2e-serving-cpu -o json)"
artifact_json="$("${kubectl_bin}" -n "${namespace}" get modelartifact e2e-serving-model -o json)"
artifact_uid="$(jq -r '.metadata.uid // empty' <<<"${artifact_json}")"
artifact_claim="$(jq -r '.status.location.claimName // empty' <<<"${artifact_json}")"
artifact_subpath="$(jq -r '.status.location.subPath // empty' <<<"${artifact_json}")"
service_name="$(jq -r '.status.serviceRef.name // empty' <<<"${deployment_json}")"
workload_name="$(jq -r '.status.deploymentRef.name // empty' <<<"${deployment_json}")"
pod_name="$("${kubectl_bin}" -n "${namespace}" get pods \
  -l kama.tannerburns.github.io/model-deployment=e2e-serving-cpu \
  -o json | jq -r '[.items[] | select(.status.phase == "Running")][0].metadata.name // ""')"
if [[ -z "${artifact_uid}" || -z "${artifact_claim}" || -z "${artifact_subpath}" ||
  -z "${service_name}" || -z "${workload_name}" || -z "${pod_name}" ]]; then
  echo "CPU serving status did not identify generated resources" >&2
  exit 1
fi

workload_json="$("${kubectl_bin}" -n "${namespace}" get deployment "${workload_name}" -o json)"
service_json="$("${kubectl_bin}" -n "${namespace}" get service "${service_name}" -o json)"
pod_json="$("${kubectl_bin}" -n "${namespace}" get pod "${pod_name}" -o json)"
workload_uid="$(jq -r '.metadata.uid // empty' <<<"${workload_json}")"
service_uid="$(jq -r '.metadata.uid // empty' <<<"${service_json}")"
pod_uid="$(jq -r '.metadata.uid // empty' <<<"${pod_json}")"
pod_image_id="$(jq -r '
  [.status.containerStatuses[] | select(.name == "runtime")][0].imageID // empty
' <<<"${pod_json}")"
runtime_fingerprint="$(jq -r '
  .spec.template.metadata.annotations["kama.tannerburns.github.io/runtime-fingerprint-full"] // empty
' <<<"${workload_json}")"
runtime_config_name="$(jq -r '
  [.spec.template.spec.volumes[] | select(.name == "runtime-config")][0].configMap.name // empty
' <<<"${workload_json}")"
runtime_config_json="$("${kubectl_bin}" -n "${namespace}" get configmap "${runtime_config_name}" -o json)"
if ! jq -e \
  --arg artifactUID "${artifact_uid}" \
  --arg artifactDigest "${model_digest}" \
  --arg deploymentName "${workload_name}" \
  --arg deploymentUID "${workload_uid}" \
  --arg serviceName "${service_name}" \
  --arg serviceUID "${service_uid}" \
  --arg image "${runtime_cpu_image}" \
  --arg imageID "${pod_image_id}" \
  --arg fingerprint "${runtime_fingerprint}" '
    . as $modelDeployment |
    .status.observedGeneration == .metadata.generation and
    .status.artifact.name == "e2e-serving-model" and
    .status.artifact.uid == $artifactUID and .status.artifact.digest == $artifactDigest and
    .status.deploymentRef.name == $deploymentName and .status.deploymentRef.uid == $deploymentUID and
    .status.serviceRef.name == $serviceName and .status.serviceRef.uid == $serviceUID and
    .status.desiredReplicas == 1 and .status.readyReplicas == 1 and
    .status.runtime.state == "Ready" and .status.runtime.desiredImage == $image and
    .status.runtime.observedImage == $imageID and
    .status.runtime.desiredFingerprint == $fingerprint and
    .status.runtime.observedFingerprint == $fingerprint and
    .status.runtime.loadedFingerprint == $fingerprint and
    .status.runtime.effectiveContextTokens == 2048 and
    .status.runtime.effectiveConcurrency == 1 and .status.runtime.acceleratorDetected == false and
    all(.status.conditions[]; .observedGeneration == $modelDeployment.metadata.generation) and
    any(.status.conditions[]; .type == "ArtifactReady" and .status == "True") and
    any(.status.conditions[]; .type == "ResourcesAvailable" and .status == "True") and
    any(.status.conditions[]; .type == "RuntimeReady" and .status == "True") and
    any(.status.conditions[]; .type == "Serving" and .status == "True") and
    any(.status.conditions[]; .type == "Degraded" and .status == "True" and .reason == "CPUOnlyRequested")
  ' <<<"${deployment_json}" >/dev/null; then
  echo "CPU serving status does not match its exact artifact, workload, image, and runtime fingerprint" >&2
  exit 1
fi
if ! jq -e \
  --arg image "${runtime_cpu_image}" \
  --arg claim "${artifact_claim}" \
  --arg subpath "${artifact_subpath}" \
  --arg worker "${worker_node}" '
  .spec.replicas == 1 and
  .spec.strategy.type == "Recreate" and
  .spec.template.spec.terminationGracePeriodSeconds == 140 and
  .spec.template.spec.automountServiceAccountToken == false and
  .spec.template.spec.enableServiceLinks == false and
  .spec.template.spec.serviceAccountName == "default" and
  .spec.template.spec.securityContext.runAsNonRoot == true and
  .spec.template.spec.securityContext.runAsUser == 65532 and
  .spec.template.spec.securityContext.runAsGroup == 65532 and
  .spec.template.spec.securityContext.fsGroup == 65532 and
  .spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" and
  .spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
  (.spec.template.spec.containers | length) == 1 and
  ((.spec.template.spec.initContainers // []) | length) == 0 and
  ((.spec.template.spec.ephemeralContainers // []) | length) == 0 and
  (.spec.template.spec.volumes | length) == 3 and
  ([.spec.template.spec.volumes[] | select(.name == "model" and
    .persistentVolumeClaim.claimName == $claim and .persistentVolumeClaim.readOnly == true)] | length) == 1 and
  ([.spec.template.spec.volumes[] | select(.name == "tmp" and .emptyDir != null)] | length) == 1 and
  ([.spec.template.spec.volumes[] | select(.secret != null or .projected != null)] | length) == 0 and
  any(.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[].matchExpressions[]?;
    .key == "kubernetes.io/hostname" and .operator == "In" and (.values | index($worker) != null)) and
  ([.spec.template.spec.containers[] | select(.name == "runtime" and
    .image == $image and
    .resources.requests.cpu == "1" and
    .resources.requests.memory == "512Mi" and
    .resources.limits.memory == "2Gi" and
    .resources.requests["nvidia.com/gpu"] == null and
    .resources.limits["nvidia.com/gpu"] == null and
    ([.resources.requests | keys[] | select(startswith("hugepages-"))] | length) == 0 and
    ([.resources.limits | keys[] | select(startswith("hugepages-"))] | length) == 0 and
    .command == ["/kama-runtime-supervisor"] and
    .args == ["--config=/etc/kama/runtime/config.json"] and
    ((.env // []) | length) == 0 and ((.envFrom // []) | length) == 0 and
    .securityContext.readOnlyRootFilesystem == true and
    .securityContext.allowPrivilegeEscalation == false and
    .securityContext.runAsNonRoot == true and .securityContext.runAsUser == 65532 and
    .securityContext.runAsGroup == 65532 and
    (.securityContext.capabilities.drop | index("ALL") != null) and
    (.ports | length) == 2 and
    ([.ports[] | select(.name == "http" and .containerPort == 8080)] | length) == 1 and
    ([.ports[] | select(.name == "supervisor" and .containerPort == 8081)] | length) == 1 and
    .startupProbe.httpGet.path == "/startupz" and .startupProbe.httpGet.port == 8081 and
    .livenessProbe.httpGet.path == "/livez" and .livenessProbe.httpGet.port == 8081 and
    .readinessProbe.httpGet.path == "/readyz" and .readinessProbe.httpGet.port == 8081 and
    (.volumeMounts | length) == 3 and
    ([.volumeMounts[] | select(.mountPath == "/models" and .name == "model" and
      .subPath == $subpath and .readOnly == true)] | length) == 1 and
    ([.volumeMounts[] | select(.mountPath == "/tmp" and .name == "tmp")] | length) == 1)] | length) == 1
' <<<"${workload_json}" >/dev/null; then
  echo "generated CPU serving workload violates the M2 security/resource contract" >&2
  exit 1
fi
if ! jq -e \
  --arg deploymentUID "$(jq -r '.metadata.uid' <<<"${deployment_json}")" '
    .spec.type == "ClusterIP" and .spec.clusterIP != "" and .spec.clusterIP != "None" and
    (.spec.ports | length) == 1 and .spec.ports[0].port == 8080 and
    .spec.ports[0].targetPort == "http" and
    .spec.selector["kama.tannerburns.github.io/model-deployment-uid"] == $deploymentUID
  ' <<<"${service_json}" >/dev/null; then
  echo "generated serving Service exposes a port outside the M2 data-plane contract" >&2
  exit 1
fi
if ! jq -e --arg worker "${worker_node}" '
  .spec.nodeName == $worker and
  ([.status.containerStatuses[] | select(.name == "runtime" and .ready == true and
    .restartCount == 0 and .state.running != null)] | length) == 1
' <<<"${pod_json}" >/dev/null || ! jq -e '.immutable == true' <<<"${runtime_config_json}" >/dev/null; then
  echo "CPU runtime Pod placement/readiness or immutable runtime configuration is incorrect" >&2
  exit 1
fi
jq -n \
  --arg artifactUID "${artifact_uid}" \
  --arg deployment "${workload_name}" \
  --arg deploymentUID "${workload_uid}" \
  --arg service "${service_name}" \
  --arg serviceUID "${service_uid}" \
  --arg pod "${pod_name}" \
  --arg podUID "${pod_uid}" \
  --arg fingerprint "${runtime_fingerprint}" \
  --arg runtimeImage "${runtime_cpu_image}" \
  --arg observedImage "${pod_image_id}" \
  --arg node "${worker_node}" '{
    validated: true,
    artifactIdentityMatched: true,
    runtimeIdentityMatched: true,
    restrictedWorkload: true,
    servicePortContract: true,
    rwoPlacementMatched: true,
    artifactUID: $artifactUID,
    deployment: {name: $deployment, uid: $deploymentUID},
    service: {name: $service, uid: $serviceUID},
    pod: {name: $pod, uid: $podUID, node: $node},
    runtime: {fingerprint: $fingerprint, desiredImage: $runtimeImage, observedImage: $observedImage}
  }' >"${evidence_dir}/serving-contract.json"

run_in_cluster_serving_client e2e-serving-cpu-client "${service_name}" e2e-serving-cpu

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
    deployment: {fingerprint: .deployment.fingerprint},
    artifact,
    runtime: {
      mode: .runtime.mode,
      effectiveContextTokens: .runtime.effectiveContextTokens,
      desiredConcurrency: .runtime.desiredConcurrency,
      llamaCPPCommit: .runtime.llamaCPPCommit,
      llamaCPPBuildNumber: .runtime.llamaCPPBuildNumber,
      acceleratorDetected: .runtime.acceleratorDetected,
      offloadedLayers: .runtime.offloadedLayers
    },
    child,
    observedAt
  }' >"${evidence_dir}/supervisor-state.json"
if ! jq -e --arg commit "${llama_commit}" --arg build "${llama_build_number}" '
  .phase == "Ready" and .ready == true and .runtime.mode == "CPU" and
  .runtime.llamaCPPCommit == $commit and .runtime.llamaCPPBuildNumber == $build and
  .runtime.desiredConcurrency == 1 and
  .runtime.effectiveContextTokens == 2048 and
  .runtime.acceleratorDetected == false
' "${evidence_dir}/supervisor-state.json" >/dev/null; then
  echo "sanitized CPU supervisor state does not match the requested runtime envelope" >&2
  exit 1
fi

old_pod_uid="$("${kubectl_bin}" -n "${namespace}" get pod "${pod_name}" -o jsonpath='{.metadata.uid}')"
"${kubectl_bin}" -n "${namespace}" port-forward "service/${service_name}" 18080:8080 \
  >"${tmp_dir}/drain-service-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18080/health
curl --no-buffer --fail-with-body --silent --show-error \
  http://127.0.0.1:18080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"e2e-serving-cpu","messages":[{"role":"user","content":"Count upward for as long as allowed."}],"max_tokens":1024,"ignore_eos":true,"stream":true}' \
  --output "${tmp_dir}/drain-sse.txt" &
stream_pid=$!
port_forward_pids+=("${stream_pid}")
first_event_observed=0
for _ in $(seq 1 120); do
  if grep -Fq 'data:' "${tmp_dir}/drain-sse.txt" 2>/dev/null; then
    first_event_observed=1
    break
  fi
  if ! kill -0 "${stream_pid}" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if [[ ${first_event_observed} -ne 1 ]] || \
  ! kill -0 "${stream_pid}" >/dev/null 2>&1 || \
  grep -Fq '[DONE]' "${tmp_dir}/drain-sse.txt"; then
  echo "SSE drain request did not remain active after its first event" >&2
  exit 1
fi
drain_timeout_seconds=120
drain_started_seconds=${SECONDS}
"${kubectl_bin}" -n "${namespace}" patch modeldeployment e2e-serving-cpu --type=merge \
  -p '{"spec":{"runtime":{"expert":{"threads":1}}}}'
readiness_removed=0
for _ in $(seq 1 120); do
  old_pod_ready="$(${kubectl_bin} -n "${namespace}" get pod "${pod_name}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  drain_endpoints="$(${kubectl_bin} -n "${namespace}" get endpointslice \
    -l "kubernetes.io/service-name=${service_name}" -o json 2>/dev/null || true)"
  if [[ "${old_pod_ready}" != "True" ]] && [[ -n "${drain_endpoints}" ]] && \
    ! jq -e 'any(.items[].endpoints[]?; .conditions.ready == true)' \
      <<<"${drain_endpoints}" >/dev/null 2>&1; then
    readiness_removed=1
    break
  fi
  if ! kill -0 "${stream_pid}" >/dev/null 2>&1; then
    break
  fi
  if ((SECONDS - drain_started_seconds >= drain_timeout_seconds)); then
    break
  fi
  sleep 1
done
if [[ ${readiness_removed} -ne 1 ]] || \
  ! kill -0 "${stream_pid}" >/dev/null 2>&1 || \
  grep -Fq '[DONE]' "${tmp_dir}/drain-sse.txt"; then
  echo "drain-first update did not remove readiness while the SSE request was active" >&2
  exit 1
fi
while kill -0 "${stream_pid}" >/dev/null 2>&1; do
  if ((SECONDS - drain_started_seconds >= drain_timeout_seconds)); then
    kill "${stream_pid}" >/dev/null 2>&1 || true
    wait "${stream_pid}" >/dev/null 2>&1 || true
    echo "active SSE request exceeded the declared ${drain_timeout_seconds}s drain timeout" >&2
    exit 1
  fi
  sleep 1
done
if ! wait "${stream_pid}"; then
  echo "active SSE request failed while the runtime was draining" >&2
  exit 1
fi
drain_elapsed_seconds=$((SECONDS - drain_started_seconds))
if ! grep -Fq '[DONE]' "${tmp_dir}/drain-sse.txt"; then
  echo "active SSE request did not complete during drain-first replacement" >&2
  exit 1
fi
wait_for_modeldeployment e2e-serving-cpu \
  '.status.observedGeneration == .metadata.generation and .status.readyReplicas == 1 and
   .status.runtime.state == "Ready" and
   ([.status.conditions[] | select(.type == "Serving" and .status == "True")] | length) == 1' \
  'the updated CPU fingerprint to become serving'
new_pod_uid="$("${kubectl_bin}" -n "${namespace}" get pods \
  -l kama.tannerburns.github.io/model-deployment=e2e-serving-cpu \
  -o json | jq -r '[.items[] | select(.status.phase == "Running")][0].metadata.uid // ""')"
if [[ -z "${new_pod_uid}" || "${new_pod_uid}" == "${old_pod_uid}" ]]; then
  echo "runtime change did not replace the singleton Pod" >&2
  exit 1
fi
jq -n \
  --arg oldPodUID "${old_pod_uid}" \
  --arg newPodUID "${new_pod_uid}" \
  --argjson elapsedSeconds "${drain_elapsed_seconds}" \
  --argjson drainTimeoutSeconds "${drain_timeout_seconds}" '{
  firstEventObservedBeforeUpdate: true,
  streamActiveBeforeUpdate: true,
  completionAbsentBeforeUpdate: true,
  readinessRemovedWhileActive: true,
  activeStreamCompleted: true,
  elapsedSeconds: $elapsedSeconds,
  drainTimeoutSeconds: $drainTimeoutSeconds,
  withinDrainTimeout: ($elapsedSeconds <= $drainTimeoutSeconds),
  oldPodUID: $oldPodUID,
  newPodUID: $newPodUID
}' >"${evidence_dir}/drain.json"

artifact_subpath="$("${kubectl_bin}" -n "${namespace}" get modelartifact e2e-serving-model \
  -o jsonpath='{.status.location.subPath}')"
if [[ -z "${artifact_subpath}" || "${artifact_subpath}" == /* || "${artifact_subpath}" == *..* ]]; then
  echo "artifact published an unsafe subpath" >&2
  exit 1
fi
docker exec "${worker_node}" mkdir -p /var/local/kama-e2e-serving-failure/models
docker exec "${worker_node}" cp \
  "/var/local/kama-e2e-serving-cache/${artifact_subpath}/${model_entrypoint}" \
  "/var/local/kama-e2e-serving-failure/models/${model_entrypoint}"
docker exec "${worker_node}" chmod -R a+rX /var/local/kama-e2e-serving-failure
sed "s|KAMA_WORKER_NODE|${worker_node}|g" \
  "${repo_root}/test/e2e/serving/failure-storage.yaml.tmpl" >"${tmp_dir}/failure-storage.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/failure-storage.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=Ready=True \
  modelartifact/e2e-serving-failure-model --timeout=5m
docker exec "${worker_node}" truncate -s 64 \
  "/var/local/kama-e2e-serving-failure/models/${model_entrypoint}"
"${kubectl_bin}" -n "${namespace}" apply -f "${repo_root}/test/e2e/serving/failure-deployment.yaml"
wait_for_modeldeployment e2e-serving-load-failure \
  '.status.runtime.state == "LoadFailed" and (.status.readyReplicas // 0) == 0 and
   ([.status.conditions[] | select(.type == "RuntimeReady" and .status == "False" and
      (.reason == "ArtifactInvalid" or .reason == "ChildExited" or .reason == "LoadFailed"))] | length) == 1 and
   ([.status.conditions[] | select(.type == "Serving" and .status == "False")] | length) == 1' \
  'the corrupt post-validation copy to reach terminal LoadFailed'

failure_json="$("${kubectl_bin}" -n "${namespace}" get modeldeployment e2e-serving-load-failure -o json)"
failure_service="$(jq -r '.status.serviceRef.name // empty' <<<"${failure_json}")"
failure_pod="$("${kubectl_bin}" -n "${namespace}" get pods \
  -l kama.tannerburns.github.io/model-deployment=e2e-serving-load-failure \
  -o json | jq -r '.items[0].metadata.name // ""')"
failure_pod_json="$("${kubectl_bin}" -n "${namespace}" get pod "${failure_pod}" -o json)"
failure_pod_uid="$(jq -r '.metadata.uid // empty' <<<"${failure_pod_json}")"
if ! jq -e '
  ([.status.containerStatuses[] | select(.name == "runtime" and .ready == false and
    .restartCount == 0 and .state.running != null)] | length) == 1
' <<<"${failure_pod_json}" >/dev/null; then
  echo "terminal load failure did not leave the supervisor running with restartCount=0" >&2
  exit 1
fi
terminal_stability_seconds=10
terminal_observation_started=${SECONDS}
terminal_stable=1
while ((SECONDS - terminal_observation_started < terminal_stability_seconds)); do
  failure_json="$("${kubectl_bin}" -n "${namespace}" get modeldeployment \
    e2e-serving-load-failure -o json)"
  failure_pods_json="$("${kubectl_bin}" -n "${namespace}" get pods \
    -l kama.tannerburns.github.io/model-deployment=e2e-serving-load-failure -o json)"
  failure_endpoints="$("${kubectl_bin}" -n "${namespace}" get endpointslice \
    -l "kubernetes.io/service-name=${failure_service}" -o json)"
  if ! jq -e '
      .status.observedGeneration == .metadata.generation and
      .status.runtime.state == "LoadFailed" and (.status.readyReplicas // 0) == 0 and
      any(.status.conditions[]; .type == "RuntimeReady" and .status == "False") and
      any(.status.conditions[]; .type == "Serving" and .status == "False")
    ' <<<"${failure_json}" >/dev/null ||
    ! jq -e --arg pod "${failure_pod}" --arg uid "${failure_pod_uid}" '
      (.items | length) == 1 and .items[0].metadata.name == $pod and .items[0].metadata.uid == $uid and
      ([.items[0].status.containerStatuses[] | select(.name == "runtime" and .ready == false and
        .restartCount == 0 and .state.running != null)] | length) == 1
    ' <<<"${failure_pods_json}" >/dev/null ||
    jq -e 'any(.items[].endpoints[]?; .conditions.ready == true)' \
      <<<"${failure_endpoints}" >/dev/null; then
    terminal_stable=0
    break
  fi
  sleep 1
done
if [[ ${terminal_stable} -ne 1 ]]; then
  echo "terminal load failure did not remain stable, unready, and restart-free" >&2
  exit 1
fi
jq -n \
  --arg pod "${failure_pod}" \
  --arg podUID "${failure_pod_uid}" \
  --argjson observationSeconds "${terminal_stability_seconds}" '{
  pod: $pod,
  podUID: $podUID,
  supervisorRunning: true,
  restartCount: 0,
  readyEndpoint: false,
  stableObservation: true,
  observationSeconds: $observationSeconds
}' >"${evidence_dir}/load-failure.json"

passed=1
