#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(tr -d '\r\n' < "${repo_root}/VERSION")"
kind_bin="${KIND:-${repo_root}/bin/kind}"
kubectl_bin="${KUBECTL:-kubectl}"
helm_bin="${HELM:-${repo_root}/bin/helm}"
cluster_name="${KIND_CLUSTER:-kama-m1-live}"
node_image="${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must be a digest-pinned Kind node image}"
namespace="kama-system"
manager_image="${IMG:-local/kama-manager:${version}}"
importer_image="${IMPORTER_IMG:-local/kama-importer:${version}}"
created="$(git -C "${repo_root}" show -s --format=%cI HEAD 2>/dev/null || printf '1970-01-01T00:00:00Z')"
revision="$(git -C "${repo_root}" rev-parse HEAD 2>/dev/null || printf 'unknown')"
evidence_dir="${EVIDENCE_DIR:-${repo_root}/dist/m1-live}"
tmp_dir="$(mktemp -d)"
cluster_created=0
evidence_captured=0
private_enabled=0
private_status="SKIPPED: HF_TOKEN is not configured"
require_private=0
public_passed=0
private_passed=0
manager_restart_started=""
manager_restart_completed=""
manager_pod_uid_before=""
manager_pod_uid_after=""

public_name="m1-live-public"
public_repository="HuggingFaceTB/SmolLM2-360M-Instruct-GGUF"
public_revision="593b5a2e04c8f3e4ee880263f93e0bd2901ad47f"
public_file="smollm2-360m-instruct-q8_0.gguf"
public_size=386404992
public_sha256="48ab3034d0dd401fbc721eb1df3217902fee7dab9078992d66431f09b7750201"

mkdir -p "${evidence_dir}"

case "${M1_REQUIRE_PRIVATE:-false}" in
  1 | true) require_private=1 ;;
  0 | false | "") ;;
  *) echo "M1_REQUIRE_PRIVATE must be true or false" >&2; exit 2 ;;
esac

if [[ ! -x "${kind_bin}" ]]; then
  kind_bin="$(command -v kind || true)"
fi
if [[ ! -x "${helm_bin}" ]]; then
  helm_bin="$(command -v helm || true)"
fi
for tool in "${kind_bin}" "${kubectl_bin}" "${helm_bin}" docker curl git jq sed; do
  if [[ -z "${tool}" ]] || ! command -v "${tool}" >/dev/null 2>&1; then
    echo "required command is unavailable: ${tool:-unset}" >&2
    exit 1
  fi
done

append_summary() {
  local line=$1
  echo "${line}"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    echo "${line}" >>"${GITHUB_STEP_SUMMARY}"
  fi
}

capture_evidence() {
  local outcome=$1
  local qualifying=false
  local public_result=false
  local private_result="skipped"
  if [[ ${evidence_captured} -eq 1 ]]; then
    return
  fi
  evidence_captured=1

  if [[ ${private_enabled} -eq 1 ]]; then
    private_result="failed"
  fi
  if [[ ${private_passed} -eq 1 ]]; then
    private_result="passed"
  fi
  if [[ ${public_passed} -eq 1 && ${private_passed} -eq 1 ]]; then
    qualifying=true
  fi
  if [[ ${public_passed} -eq 1 ]]; then
    public_result=true
  fi

  printf '%s\n' "${outcome}" >"${evidence_dir}/outcome.txt"
  printf '%s\n' "${private_status}" >"${evidence_dir}/private-lane.txt"
  jq -n \
    --arg outcome "${outcome}" \
    --arg private "${private_result}" \
    --argjson publicPassed "${public_result}" \
    --argjson qualifying "${qualifying}" '{
      schemaVersion: 1,
      outcome: $outcome,
      public: (if $publicPassed then "passed" else "failed" end),
      private: $private,
      qualifying: $qualifying
    }' >"${evidence_dir}/qualification.json"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "qualifying=${qualifying}" >>"${GITHUB_OUTPUT}"
  fi
  if [[ ${cluster_created} -eq 1 ]]; then
    "${kubectl_bin}" get nodes -o wide >"${evidence_dir}/nodes.txt" 2>&1 || true
    "${kubectl_bin}" get pods -A -o wide >"${evidence_dir}/pods.txt" 2>&1 || true
    "${kubectl_bin}" -n "${namespace}" get modelcache,modelartifact,job \
      >"${evidence_dir}/resources.txt" 2>&1 || true
    "${kubectl_bin}" -n "${namespace}" get events --sort-by=.lastTimestamp \
      >"${evidence_dir}/events.txt" 2>&1 || true
    "${kubectl_bin}" -n "${namespace}" logs deployment/kama --all-containers=true \
      --prefix=true --tail=300 >"${evidence_dir}/manager.log" 2>&1 || true
    while IFS= read -r job; do
      [[ -n "${job}" ]] || continue
      "${kubectl_bin}" -n "${namespace}" logs "job/${job}" --all-containers=true \
        >"${evidence_dir}/job-${job}.log" 2>&1 || true
    done < <("${kubectl_bin}" -n "${namespace}" get jobs \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
    "${kubectl_bin}" -n "${namespace}" get modelartifact -o json 2>/dev/null | jq '{
      apiVersion,
      kind,
      items: [.items[] | {
        metadata: {name: .metadata.name, generation: .metadata.generation},
        status: .status
      }]
    }' >"${evidence_dir}/artifact-status.json" || true
  fi
}

cleanup() {
  local exit_code=$?
  if [[ ${exit_code} -eq 0 ]]; then
    if [[ ${public_passed} -eq 1 && ${private_passed} -eq 1 ]]; then
      capture_evidence "PASS: live public/private Hugging Face qualification complete"
    else
      capture_evidence "PASS (public only): M1 NOT QUALIFIED because the private lane was skipped"
    fi
  else
    capture_evidence "FAIL (exit ${exit_code})"
  fi
  if [[ ${cluster_created} -eq 1 && "${KEEP_KIND_CLUSTER:-0}" != "1" ]]; then
    "${kind_bin}" delete cluster --name "${cluster_name}" || true
  fi
  rm -rf "${tmp_dir}"
  exit "${exit_code}"
}
trap cleanup EXIT

wait_for_condition() {
  local resource=$1
  local name=$2
  local condition=$3
  local timeout=${4:-20m}
  "${kubectl_bin}" -n "${namespace}" wait \
    --for="condition=${condition}=True" "${resource}/${name}" --timeout="${timeout}"
}

wait_for_admission() {
  local rollout_context=$1
  local attempt
  local admission_log="${tmp_dir}/admission-readiness.log"

  for attempt in $(seq 1 8); do
    if "${kubectl_bin}" -n "${namespace}" create --dry-run=server -f - \
      >/dev/null 2>"${admission_log}" <<'EOF'
apiVersion: kama.tannerburns.github.io/v1alpha1
kind: ModelCache
metadata:
  name: admission-routing-readiness
spec:
  storage:
    existingClaim:
      name: admission-only
EOF
    then
      return 0
    fi
    if ! grep -Eq \
      'failed calling webhook|no endpoints available|connection refused|context deadline exceeded|TLS handshake timeout' \
      "${admission_log}"; then
      break
    fi
    if [[ ${attempt} -lt 8 ]]; then
      sleep 2
    fi
  done

  echo "admission did not become reachable after ${rollout_context}:" >&2
  sed -n '1,20p' "${admission_log}" >&2
  return 1
}

job_result() {
  local job_name=$1
  local job_uid pod_name
  job_uid="$("${kubectl_bin}" -n "${namespace}" get job "${job_name}" \
    -o jsonpath='{.metadata.uid}')"
  pod_name="$("${kubectl_bin}" -n "${namespace}" get pods --selector "job-name=${job_name}" \
    -o json | jq -r --arg uid "${job_uid}" '[.items[] | select(
      .status.phase == "Succeeded" and
      any(.metadata.ownerReferences[]?;
        .controller == true and .kind == "Job" and .uid == $uid)
    )][0].metadata.name // ""')"
  if [[ -z "${pod_name}" ]]; then
    echo "completed importer Pod for Job ${job_name} is unavailable" >&2
    return 1
  fi
  "${kubectl_bin}" -n "${namespace}" logs "pod/${pod_name}" --container importer --tail=1
}

validate_artifact_status() {
  local artifact_name=$1
  local expected_revision=$2
  local expected_file=$3
  local expected_size=$4
  local expected_sha256=$5
  local artifact_json
  artifact_json="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact_name}" -o json)"
  if ! jq -e \
    --arg revision "${expected_revision}" \
    --arg file "${expected_file}" \
    --arg sha256 "${expected_sha256}" \
    --argjson size "${expected_size}" '
      .status.observedGeneration == .metadata.generation and
      .status.resolvedRevision == $revision and
      .status.artifactDigest == $sha256 and
      .status.size == $size and
      .status.location.readOnly == true and
      ([.status.conditions[] | select(.type == "Verified" and .status == "True")] | length) == 1 and
      ([.status.conditions[] | select(.type == "Ready" and .status == "True")] | length) == 1 and
      .status.files == [{path: $file, size: $size, sha256: $sha256}]
    ' <<<"${artifact_json}" >/dev/null; then
    echo "ModelArtifact ${artifact_name} did not publish the expected immutable identity" >&2
    jq '.status' <<<"${artifact_json}" >&2
    return 1
  fi
}

validate_initial_result() {
  local artifact_name=$1
  local expected_revision=$2
  local expected_size=$3
  local expected_sha256=$4
  local job_name result
  job_name="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact_name}" \
    -o jsonpath='{.status.jobRef.name}')"
  "${kubectl_bin}" -n "${namespace}" wait --for=condition=complete \
    "job/${job_name}" --timeout=5m
  result="$(job_result "${job_name}")"
  if ! jq -e \
    --arg revision "${expected_revision}" \
    --arg sha256 "${expected_sha256}" \
    --argjson size "${expected_size}" '
      .schemaVersion == 1 and
      .mode == "hub" and
      .success == true and
      .resolvedRevision == $revision and
      .artifactDigest == $sha256 and
      .bytesTransferred == $size and
      ((.cacheHit // false) == false)
    ' <<<"${result}" >/dev/null; then
    echo "initial importer result for ${artifact_name} was not a full verified Hub transfer" >&2
    echo "${result}" >&2
    return 1
  fi
  printf '%s\n' "${result}" >"${evidence_dir}/${artifact_name}-initial-result.json"
}

recover_without_hub() {
  local artifact_name=$1
  local expected_revision=$2
  local expected_sha256=$3
  local job_name old_uid new_uid result deletion_started recovery_completed
  job_name="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact_name}" \
    -o jsonpath='{.status.jobRef.name}')"
  old_uid="$("${kubectl_bin}" -n "${namespace}" get job "${job_name}" \
    -o jsonpath='{.metadata.uid}')"
  deletion_started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  "${kubectl_bin}" -n "${namespace}" delete job "${job_name}" --wait --timeout=60s

  new_uid=""
  for _ in $(seq 1 180); do
    new_uid="$("${kubectl_bin}" -n "${namespace}" get job "${job_name}" \
      -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    if [[ -n "${new_uid}" && "${new_uid}" != "${old_uid}" ]]; then
      break
    fi
    sleep 1
  done
  if [[ -z "${new_uid}" || "${new_uid}" == "${old_uid}" ]]; then
    echo "controller did not recreate importer Job ${job_name}" >&2
    return 1
  fi

  "${kubectl_bin}" -n "${namespace}" wait --for=condition=complete \
    "job/${job_name}" --timeout=5m
  for _ in $(seq 1 120); do
    local recorded_uid ready
    recorded_uid="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact_name}" \
      -o jsonpath='{.status.jobRef.uid}' 2>/dev/null || true)"
    ready="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact_name}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    if [[ "${recorded_uid}" == "${new_uid}" && "${ready}" == "True" ]]; then
      break
    fi
    sleep 1
  done
  if [[ "${recorded_uid:-}" != "${new_uid}" || "${ready:-}" != "True" ]]; then
    echo "ModelArtifact ${artifact_name} did not accept the recreated recovery Job" >&2
    return 1
  fi

  result="$(job_result "${job_name}")"
  if ! jq -e \
    --arg revision "${expected_revision}" \
    --arg sha256 "${expected_sha256}" '
      .schemaVersion == 1 and
      .mode == "hub" and
      .success == true and
      .resolvedRevision == $revision and
      .artifactDigest == $sha256 and
      (.bytesTransferred // 0) == 0 and
      .cacheHit == true
    ' <<<"${result}" >/dev/null; then
    echo "recreated Job for ${artifact_name} did not prove zero-transfer cache recovery" >&2
    echo "${result}" >&2
    return 1
  fi
  printf '%s\n' "${result}" >"${evidence_dir}/${artifact_name}-recovery-result.json"
  recovery_completed="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  jq -n \
    --arg deletedAt "${deletion_started}" \
    --arg recoveredAt "${recovery_completed}" \
    --arg oldJobUID "${old_uid}" \
    --arg newJobUID "${new_uid}" \
    --argjson result "${result}" '{
      deletedAt: $deletedAt,
      recoveredAt: $recoveredAt,
      oldJobUID: $oldJobUID,
      newJobUID: $newJobUID,
      result: $result
    }' >"${evidence_dir}/${artifact_name}-recovery-evidence.json"
}

configure_private_lane() {
  if [[ -z "${HF_TOKEN:-}" ]]; then
    if [[ ${require_private} -eq 1 ]]; then
      private_status="FAILED: protected HF_TOKEN is required for this qualification run"
      echo "private Hugging Face qualification requires the m1-live environment secret HF_TOKEN" >&2
      return 1
    fi
    append_summary "Private Hugging Face lane: SKIPPED (protected HF_TOKEN is not configured)."
    return
  fi
  private_enabled=1
  private_status="FAILED: HF_TOKEN is present but the private lane did not complete"

  local required=(
    M1_PRIVATE_HF_REPOSITORY
    M1_PRIVATE_HF_REVISION
    M1_PRIVATE_HF_FILE
    M1_PRIVATE_HF_SHA256
    M1_PRIVATE_HF_SIZE
  )
  local variable
  for variable in "${required[@]}"; do
    if [[ -z "${!variable:-}" ]]; then
      echo "HF_TOKEN is configured but ${variable} is missing" >&2
      return 1
    fi
  done
  if [[ ! "${M1_PRIVATE_HF_REPOSITORY}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
    [[ ! "${M1_PRIVATE_HF_REVISION}" =~ ^[a-fA-F0-9]{40}$ ]] ||
    [[ ! "${M1_PRIVATE_HF_FILE}" =~ ^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$ ]] ||
    [[ "${M1_PRIVATE_HF_FILE}" == *..* ]] ||
    [[ ! "${M1_PRIVATE_HF_SHA256}" =~ ^[a-fA-F0-9]{64}$ ]] ||
    [[ ! "${M1_PRIVATE_HF_SIZE}" =~ ^[1-9][0-9]*$ ]]; then
    echo "private Hugging Face variables do not contain safe, immutable artifact metadata" >&2
    return 1
  fi

  local anonymous_status
  anonymous_status="$(curl --silent --show-error --connect-timeout 10 --max-time 30 \
    --output /dev/null --write-out '%{http_code}' \
    "https://huggingface.co/api/models/${M1_PRIVATE_HF_REPOSITORY}/revision/${M1_PRIVATE_HF_REVISION}" || true)"
  if [[ "${anonymous_status}" == "200" ]]; then
    echo "configured private Hugging Face repository is anonymously readable" >&2
    return 1
  fi
  if [[ "${anonymous_status}" != "401" && "${anonymous_status}" != "403" && \
    "${anonymous_status}" != "404" ]]; then
    echo "anonymous private-repository check returned HTTP ${anonymous_status:-unavailable}" >&2
    return 1
  fi

  umask 077
  printf '%s' "${HF_TOKEN}" >"${tmp_dir}/huggingface-token"
  "${kubectl_bin}" -n "${namespace}" create secret generic m1-live-huggingface-token \
    --from-file="token=${tmp_dir}/huggingface-token"
  sed \
    -e "s|M1_PRIVATE_HF_REPOSITORY|${M1_PRIVATE_HF_REPOSITORY}|g" \
    -e "s|M1_PRIVATE_HF_REVISION|${M1_PRIVATE_HF_REVISION}|g" \
    -e "s|M1_PRIVATE_HF_FILE|${M1_PRIVATE_HF_FILE}|g" \
    -e "s|M1_PRIVATE_HF_SHA256|${M1_PRIVATE_HF_SHA256,,}|g" \
    -e "s|M1_PRIVATE_HF_SIZE|${M1_PRIVATE_HF_SIZE}|g" \
    "${repo_root}/test/live/m1-private-artifact.yaml" >"${tmp_dir}/m1-private-artifact.yaml"
  docker exec "${worker_node}" mkdir -p /var/local/kama-m1-live-private-cache
  docker exec "${worker_node}" chmod 0777 /var/local/kama-m1-live-private-cache
  sed "s|KAMA_WORKER_NODE|${worker_node}|g" \
    "${repo_root}/test/live/m1-private-storage.yaml" >"${tmp_dir}/m1-private-storage.yaml"
  "${kubectl_bin}" apply -f "${tmp_dir}/m1-private-storage.yaml"
  wait_for_condition modelcache m1-live-private-cache Ready 5m
  private_status="RUNNING: authenticated import and cache-only recovery are required"
  append_summary "Private Hugging Face lane: ENABLED (anonymous access denied; protected token import scheduled)."
}

if "${kind_bin}" get clusters | grep -Fxq "${cluster_name}"; then
  echo "Kind cluster ${cluster_name} already exists; choose a disposable KIND_CLUSTER" >&2
  exit 1
fi

docker build \
  --build-arg "VERSION=${version}" \
  --build-arg "VCS_REF=${revision}" \
  --build-arg "CREATED=${created}" \
  --tag "${manager_image}" \
  "${repo_root}"
docker build \
  --file "${repo_root}/Dockerfile.importer" \
  --build-arg "VERSION=${version}" \
  --build-arg "VCS_REF=${revision}" \
  --build-arg "CREATED=${created}" \
  --tag "${importer_image}" \
  "${repo_root}"

"${kind_bin}" create cluster --name "${cluster_name}" --image "${node_image}" \
  --config "${repo_root}/test/kind/cluster.yaml" --wait 5m
cluster_created=1
"${kind_bin}" load docker-image --name "${cluster_name}" "${manager_image}" "${importer_image}"

"${kubectl_bin}" wait --for=condition=Ready node --all --timeout=2m
ready_node_count="$("${kubectl_bin}" get nodes -o json | jq '
  [.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length
')"
if [[ "${ready_node_count}" != "2" ]]; then
  echo "M1 live lane requires exactly two Ready Kind nodes; found ${ready_node_count}" >&2
  exit 1
fi

"${kubectl_bin}" create namespace "${namespace}"
worker_node="${cluster_name}-worker"
docker exec "${worker_node}" mkdir -p /var/local/kama-m1-live-cache
docker exec "${worker_node}" chmod 0777 /var/local/kama-m1-live-cache

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
  --wait \
  --timeout 3m
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama --timeout=3m
"${kubectl_bin}" wait --for=condition=Established \
  crd/modelcaches.kama.tannerburns.github.io \
  crd/modelartifacts.kama.tannerburns.github.io \
  --timeout=60s
wait_for_admission "the initial manager rollout"

sed "s|KAMA_WORKER_NODE|${worker_node}|g" \
  "${repo_root}/test/live/m1-storage.yaml" >"${tmp_dir}/m1-storage.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/m1-storage.yaml"
wait_for_condition modelcache m1-live-cache Ready 5m

configure_private_lane

"${kubectl_bin}" apply -f "${repo_root}/test/live/m1-public-artifact.yaml"
if [[ ${private_enabled} -eq 1 ]]; then
  "${kubectl_bin}" apply -f "${tmp_dir}/m1-private-artifact.yaml"
fi

wait_for_condition modelartifact "${public_name}" Ready 20m
validate_artifact_status "${public_name}" "${public_revision}" "${public_file}" \
  "${public_size}" "${public_sha256}"
validate_initial_result "${public_name}" "${public_revision}" "${public_size}" "${public_sha256}"

if [[ ${private_enabled} -eq 1 ]]; then
  wait_for_condition modelartifact m1-live-private Ready 20m
  validate_artifact_status m1-live-private "${M1_PRIVATE_HF_REVISION}" "${M1_PRIVATE_HF_FILE}" \
    "${M1_PRIVATE_HF_SIZE}" "${M1_PRIVATE_HF_SHA256,,}"
  validate_initial_result m1-live-private "${M1_PRIVATE_HF_REVISION}" \
    "${M1_PRIVATE_HF_SIZE}" "${M1_PRIVATE_HF_SHA256,,}"
fi

public_job_uid="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${public_name}" \
  -o jsonpath='{.status.jobRef.uid}')"
public_digest="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${public_name}" \
  -o jsonpath='{.status.artifactDigest}')"
private_job_uid=""
private_digest=""
if [[ ${private_enabled} -eq 1 ]]; then
  private_job_uid="$("${kubectl_bin}" -n "${namespace}" get modelartifact m1-live-private \
    -o jsonpath='{.status.jobRef.uid}')"
  private_digest="$("${kubectl_bin}" -n "${namespace}" get modelartifact m1-live-private \
    -o jsonpath='{.status.artifactDigest}')"
fi

manager_pod_uid_before="$("${kubectl_bin}" -n "${namespace}" get pods \
  --selector app.kubernetes.io/name=kama,app.kubernetes.io/instance=kama \
  -o json | jq -r '[.items[] | select(.metadata.deletionTimestamp == null)] |
    sort_by(.metadata.creationTimestamp) | last | .metadata.uid // ""')"
manager_restart_started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
"${helm_bin}" upgrade kama "${chart_package}" \
  --namespace "${namespace}" \
  --reuse-values \
  --set-string importer.hubEndpoint=http://m1-live-hub-disabled.invalid \
  --wait \
  --timeout 3m
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama --timeout=3m
wait_for_admission "the endpoint-change manager rollout"
manager_restart_completed="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
manager_pod_uid_after="$("${kubectl_bin}" -n "${namespace}" get pods \
  --selector app.kubernetes.io/name=kama,app.kubernetes.io/instance=kama \
  -o json | jq -r '[.items[] | select(.metadata.deletionTimestamp == null)] |
    sort_by(.metadata.creationTimestamp) | last | .metadata.uid // ""')"
if [[ -z "${manager_pod_uid_before}" || -z "${manager_pod_uid_after}" || \
  "${manager_pod_uid_before}" == "${manager_pod_uid_after}" ]]; then
  echo "Helm endpoint change did not prove replacement of the manager Pod" >&2
  exit 1
fi
wait_for_condition modelartifact "${public_name}" Ready 2m

if [[ "$("${kubectl_bin}" -n "${namespace}" get modelartifact "${public_name}" \
  -o jsonpath='{.status.jobRef.uid}')" != "${public_job_uid}" ]] ||
  [[ "$("${kubectl_bin}" -n "${namespace}" get modelartifact "${public_name}" \
    -o jsonpath='{.status.artifactDigest}')" != "${public_digest}" ]]; then
  echo "manager restart changed retained public importer evidence or artifact identity" >&2
  exit 1
fi
if [[ ${private_enabled} -eq 1 ]]; then
  wait_for_condition modelartifact m1-live-private Ready 2m
  if [[ "$("${kubectl_bin}" -n "${namespace}" get modelartifact m1-live-private \
    -o jsonpath='{.status.jobRef.uid}')" != "${private_job_uid}" ]] ||
    [[ "$("${kubectl_bin}" -n "${namespace}" get modelartifact m1-live-private \
      -o jsonpath='{.status.artifactDigest}')" != "${private_digest}" ]]; then
    echo "manager restart changed retained private importer evidence or artifact identity" >&2
    exit 1
  fi
fi

recover_without_hub "${public_name}" "${public_revision}" "${public_sha256}"
validate_artifact_status "${public_name}" "${public_revision}" "${public_file}" \
  "${public_size}" "${public_sha256}"
public_passed=1

if [[ ${private_enabled} -eq 1 ]]; then
  recover_without_hub m1-live-private "${M1_PRIVATE_HF_REVISION}" "${M1_PRIVATE_HF_SHA256,,}"
  validate_artifact_status m1-live-private "${M1_PRIVATE_HF_REVISION}" "${M1_PRIVATE_HF_FILE}" \
    "${M1_PRIVATE_HF_SIZE}" "${M1_PRIVATE_HF_SHA256,,}"
  private_status="PASS: anonymous access denied, token import succeeded, and recovery transferred zero bytes"
  private_passed=1
fi

append_summary "M1 live public lane: PASS (pinned SmolLM2 GGUF import, manager restart, and zero-transfer Job recovery)."
append_summary "Source contact was disabled before recovery with http://m1-live-hub-disabled.invalid."
if [[ ${private_passed} -eq 1 ]]; then
  append_summary "M1 live Hugging Face qualification: PASS (public and private lanes)."
else
  append_summary "M1 live Hugging Face qualification: NOT COMPLETE (private lane skipped; qualifying=false)."
fi
jq -n \
  --arg startedAt "${manager_restart_started}" \
  --arg completedAt "${manager_restart_completed}" \
  --arg podUIDBefore "${manager_pod_uid_before}" \
  --arg podUIDAfter "${manager_pod_uid_after}" '{
    managerRestart: {
      startedAt: $startedAt,
      completedAt: $completedAt,
      podUIDBefore: $podUIDBefore,
      podUIDAfter: $podUIDAfter
    }
  }' >"${evidence_dir}/restart-timestamps.json"
jq -n \
  --arg publicRepository "${public_repository}" \
  --arg publicRevision "${public_revision}" \
  --arg publicFile "${public_file}" \
  --arg publicSHA256 "${public_sha256}" \
  --argjson publicSize "${public_size}" \
  --arg privateRepository "${M1_PRIVATE_HF_REPOSITORY:-}" \
  --arg privateRevision "${M1_PRIVATE_HF_REVISION:-}" \
  --arg privateFile "${M1_PRIVATE_HF_FILE:-}" \
  --arg privateSHA256 "${M1_PRIVATE_HF_SHA256:-}" \
  --arg privateSize "${M1_PRIVATE_HF_SIZE:-}" \
  --argjson privateEnabled "$(if [[ ${private_enabled} -eq 1 ]]; then echo true; else echo false; fi)" '{
    public: {
      repository: $publicRepository,
      revision: $publicRevision,
      file: $publicFile,
      sha256: $publicSHA256,
      size: $publicSize
    },
    private: (if $privateEnabled then {
      repository: $privateRepository,
      revision: $privateRevision,
      file: $privateFile,
      sha256: ($privateSHA256 | ascii_downcase),
      size: ($privateSize | tonumber)
    } else null end)
  }' >"${evidence_dir}/source-identities.json"
