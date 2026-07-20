#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kubectl_bin="${KUBECTL:-kubectl}"
kind_bin="${KIND:-${repo_root}/bin/kind}"
cluster_name="${KIND_CLUSTER:-kama}"
namespace="${E2E_STORAGE_NAMESPACE:-kama-e2e-storage}"
operator_namespace="${E2E_OPERATOR_NAMESPACE:-kama-system}"
manager_deployment="${E2E_MANAGER_DEPLOYMENT:-kama}"
rwx_storage_class="${E2E_RWX_STORAGE_CLASS:?E2E_RWX_STORAGE_CLASS must name a preinstalled RWX CSI StorageClass}"
expected_csi_driver="${E2E_RWX_CSI_DRIVER:-nfs.csi.k8s.io}"
helper_image="${E2E_STORAGE_HELPER_IMAGE:-local/kama-e2e-storage-helper:dev}"
fake_hub_endpoint="${E2E_FAKE_HUB_ENDPOINT:-http://kama-fake-huggingface.kama-system.svc.cluster.local:8083}"
evidence_dir="${E2E_EVIDENCE_DIR:-${repo_root}/dist/e2e/storage}"
fixture_dir="${repo_root}/test/e2e/storage"
valid_sha256="0e1ec3e53960fdc1556515b1368222af8acbd0d346447b1a724bdaaeb3fa1e94"
tmp_dir="$(mktemp -d)"
worker_container=""
worker_hostname=""
control_hostname=""
rwx_pv=""
created_resources=0
evidence_collected=0
summary_written=0

log() {
  printf '[e2e-storage] %s\n' "$*"
}

fail() {
  printf '[e2e-storage] ERROR: %s\n' "$*" >&2
  return 1
}

require_dns_name() {
  local name=$1
  local description=$2
  if [[ ! "${name}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]]; then
    fail "${description} is not a safe DNS-style name: ${name}"
  fi
}

require_subpath() {
  local subpath=$1
  if [[ -z "${subpath}" || "${subpath}" == /* || "${subpath}" == *".."* || \
    ! "${subpath}" =~ ^[A-Za-z0-9._/-]+$ ]]; then
    fail "artifact status returned an unsafe subPath: ${subpath}"
  fi
}

collect_evidence() {
  if [[ ${evidence_collected} -eq 1 ]]; then
    return
  fi
  evidence_collected=1
  mkdir -p "${evidence_dir}"
  "${kubectl_bin}" get nodes -o wide >"${evidence_dir}/nodes.txt" 2>&1 || true
  "${kubectl_bin}" get pv e2e-storage-source e2e-storage-full \
    -o yaml >"${evidence_dir}/local-pvs.yaml" 2>&1 || true
  "${kubectl_bin}" get storageclass "${rwx_storage_class}" \
    -o yaml >"${evidence_dir}/rwx-storage-class.yaml" 2>&1 || true
  if [[ -z "${rwx_pv}" ]]; then
    rwx_pv="$("${kubectl_bin}" -n "${namespace}" get pvc e2e-storage-rwx \
      -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)"
  fi
  if [[ -n "${rwx_pv}" ]]; then
    "${kubectl_bin}" get pv "${rwx_pv}" -o yaml \
      >"${evidence_dir}/rwx-pv.yaml" 2>&1 || true
  fi
  "${kubectl_bin}" -n "${operator_namespace}" get deployment/e2e-storage-nfs \
    -o yaml >"${evidence_dir}/nfs-server-deployment.yaml" 2>&1 || true
  "${kubectl_bin}" -n "${operator_namespace}" logs deployment/e2e-storage-nfs \
    --all-containers=true >"${evidence_dir}/nfs-server.log" 2>&1 || true
  "${kubectl_bin}" -n kube-system get deployment/csi-nfs-controller daemonset/csi-nfs-node \
    -o yaml >"${evidence_dir}/nfs-csi-workloads.yaml" 2>&1 || true
  "${kubectl_bin}" -n kube-system get pods -l app.kubernetes.io/name=csi-driver-nfs \
    -o wide >"${evidence_dir}/nfs-csi-pods.txt" 2>&1 || true
  "${kubectl_bin}" -n "${namespace}" get \
    modelcaches,modelartifacts,persistentvolumeclaims,pods,jobs \
    -o yaml >"${evidence_dir}/resources.yaml" 2>&1 || true
  "${kubectl_bin}" -n "${namespace}" get events --sort-by=.metadata.creationTimestamp \
    >"${evidence_dir}/events.txt" 2>&1 || true
  "${kubectl_bin}" -n "${namespace}" describe pods \
    >"${evidence_dir}/pods-describe.txt" 2>&1 || true
  while IFS= read -r job; do
    [[ -n "${job}" ]] || continue
    "${kubectl_bin}" -n "${namespace}" logs "job/${job}" --all-containers=true \
      >"${evidence_dir}/job-${job}.log" 2>&1 || true
  done < <("${kubectl_bin}" -n "${namespace}" get jobs -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
}

cleanup() {
  local exit_code=$?
  trap - EXIT
  if [[ ${exit_code} -ne 0 && ${summary_written} -eq 0 ]]; then
    mkdir -p "${evidence_dir}"
    printf '{\n  "schemaVersion": 1,\n  "suite": "artifact-plane/storage",\n  "outcome": "failed",\n  "exitCode": %d\n}\n' \
      "${exit_code}" >"${evidence_dir}/summary.json"
    summary_written=1
  fi
  collect_evidence
  if [[ ${created_resources} -eq 1 && "${E2E_KEEP_RESOURCES:-0}" != "1" ]]; then
    "${kubectl_bin}" delete namespace "${namespace}" --ignore-not-found \
      --wait --timeout=2m >/dev/null 2>&1 || true
    "${kubectl_bin}" delete pv e2e-storage-source e2e-storage-full \
      --ignore-not-found --wait --timeout=60s >/dev/null 2>&1 || true
    "${kubectl_bin}" delete storageclass e2e-storage-source-local e2e-storage-full-local \
      --ignore-not-found >/dev/null 2>&1 || true
    if [[ -n "${worker_container}" ]]; then
      docker exec "${worker_container}" umount /var/local/kama-e2e-storage-full \
        >/dev/null 2>&1 || true
      docker exec "${worker_container}" rm -rf -- \
        /var/local/kama-e2e-storage-source /var/local/kama-e2e-storage-full \
        >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "${tmp_dir}"
  exit "${exit_code}"
}
trap cleanup EXIT

if [[ ! -x "${kind_bin}" ]]; then
  kind_bin="$(command -v kind || true)"
fi
for tool in "${kind_bin}" "${kubectl_bin}" docker jq base64 sed; do
  if [[ -z "${tool}" ]] || ! command -v "${tool}" >/dev/null 2>&1; then
    fail "required command is unavailable: ${tool:-unset}"
  fi
done
require_dns_name "${namespace}" "E2E storage namespace"
require_dns_name "${operator_namespace}" "operator namespace"
require_dns_name "${manager_deployment}" "manager Deployment"
require_dns_name "${rwx_storage_class}" "RWX StorageClass"
if [[ ! "${expected_csi_driver}" =~ ^[A-Za-z0-9._/-]+$ ]]; then
  fail "E2E_RWX_CSI_DRIVER contains unsafe characters: ${expected_csi_driver}"
fi
if [[ ! "${helper_image}" =~ ^[A-Za-z0-9./_:@-]+$ ]]; then
  fail "E2E_STORAGE_HELPER_IMAGE contains unsafe characters"
fi

if ! "${kind_bin}" get clusters | grep -Fxq "${cluster_name}"; then
  fail "Kind cluster ${cluster_name} does not exist"
fi
if "${kubectl_bin}" get namespace "${namespace}" >/dev/null 2>&1; then
  fail "namespace ${namespace} already exists; refusing to reuse or delete it"
fi
for cluster_resource in \
  pv/e2e-storage-source pv/e2e-storage-full \
  storageclass/e2e-storage-source-local storageclass/e2e-storage-full-local; do
  if "${kubectl_bin}" get "${cluster_resource}" >/dev/null 2>&1; then
    fail "${cluster_resource} already exists; refusing to reuse it"
  fi
done

"${kubectl_bin}" -n "${operator_namespace}" rollout status \
  "deployment/${manager_deployment}" --timeout=2m
manager_json="$("${kubectl_bin}" -n "${operator_namespace}" get \
  "deployment/${manager_deployment}" -o json)"
if ! jq -e --arg endpoint "${fake_hub_endpoint}" '
  any(.spec.template.spec.containers[].args[]?; . == "--hub-endpoint=" + $endpoint)
' <<<"${manager_json}" >/dev/null; then
  fail "manager is not configured for the deterministic fake Hub endpoint ${fake_hub_endpoint}"
fi
"${kubectl_bin}" -n "${operator_namespace}" get service kama-fake-huggingface >/dev/null

worker_container="$("${kubectl_bin}" get nodes -o json | jq -r '
  [.items[] | select(
    (.metadata.labels["node-role.kubernetes.io/control-plane"] == null) and
    (.metadata.labels["node-role.kubernetes.io/master"] == null)
  ) | .metadata.name][0] // ""
')"
worker_hostname="$("${kubectl_bin}" get node "${worker_container}" \
  -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}' 2>/dev/null || true)"
control_hostname="$("${kubectl_bin}" get nodes -o json | jq -r '
  [.items[] | select(
    (.metadata.labels["node-role.kubernetes.io/control-plane"] != null) or
    (.metadata.labels["node-role.kubernetes.io/master"] != null)
  ) | .metadata.labels["kubernetes.io/hostname"]][0] // ""
')"
if [[ -z "${worker_container}" || -z "${worker_hostname}" || -z "${control_hostname}" || \
  "${worker_hostname}" == "${control_hostname}" ]]; then
  fail "a distinct worker and control-plane node are required"
fi
require_dns_name "${worker_hostname}" "worker hostname"
require_dns_name "${control_hostname}" "control-plane hostname"

log "building and loading the repo-pinned helper image"
docker build --file "${fixture_dir}/helper/Dockerfile" --tag "${helper_image}" "${repo_root}"
"${kind_bin}" load docker-image --name "${cluster_name}" "${helper_image}"

log "preparing a static RWO source and an 8 MiB tmpfs ENOSPC fixture on ${worker_container}"
if ! docker exec "${worker_container}" sh -c \
  'test ! -e /var/local/kama-e2e-storage-source && test ! -e /var/local/kama-e2e-storage-full'; then
  fail "E2E storage host paths already exist on ${worker_container}"
fi
docker exec "${worker_container}" mkdir -p \
  /var/local/kama-e2e-storage-source/models /var/local/kama-e2e-storage-full
created_resources=1
docker exec "${worker_container}" mount -t tmpfs \
  -o size=8m,mode=0777,nosuid,nodev tmpfs /var/local/kama-e2e-storage-full

base64 --decode "${repo_root}/internal/testfixtures/gguf/testdata/valid-minimal.gguf.b64" \
  >"${tmp_dir}/model.gguf"
base64 --decode "${repo_root}/internal/testfixtures/gguf/testdata/malformed-magic.gguf.b64" \
  >"${tmp_dir}/malformed.gguf"
base64 --decode "${repo_root}/internal/testfixtures/gguf/testdata/truncated-metadata.gguf.b64" \
  >"${tmp_dir}/truncated.gguf"
cp "${tmp_dir}/model.gguf" "${tmp_dir}/model-00001-of-00002.gguf"
for fixture in model.gguf malformed.gguf truncated.gguf model-00001-of-00002.gguf; do
  docker cp "${tmp_dir}/${fixture}" \
    "${worker_container}:/var/local/kama-e2e-storage-source/models/${fixture}"
done
docker exec "${worker_container}" chmod -R a+rX /var/local/kama-e2e-storage-source

sed \
  -e "s|E2E_NAMESPACE|${namespace}|g" \
  -e "s|E2E_WORKER_NODE|${worker_hostname}|g" \
  -e "s|E2E_RWX_STORAGE_CLASS|${rwx_storage_class}|g" \
  "${fixture_dir}/storage.yaml.tmpl" >"${tmp_dir}/storage.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/storage.yaml"
for claim in e2e-storage-source e2e-storage-full e2e-storage-rwx; do
  "${kubectl_bin}" -n "${namespace}" wait \
    --for=jsonpath='{.status.phase}'=Bound "pvc/${claim}" --timeout=3m
done

rwx_pv="$("${kubectl_bin}" -n "${namespace}" get pvc e2e-storage-rwx \
  -o jsonpath='{.spec.volumeName}')"
rwx_driver="$("${kubectl_bin}" get pv "${rwx_pv}" -o jsonpath='{.spec.csi.driver}')"
if [[ -z "${rwx_pv}" || "${rwx_driver}" != "${expected_csi_driver}" ]]; then
  fail "RWX PVC resolved CSI driver ${rwx_driver:-none}, want ${expected_csi_driver}"
fi
for cache in e2e-storage-rwx e2e-storage-full; do
  "${kubectl_bin}" -n "${namespace}" wait \
    --for=condition=Ready=True "modelcache/${cache}" --timeout=5m
done

sed -e "s|E2E_NAMESPACE|${namespace}|g" \
  "${fixture_dir}/baselines.yaml.tmpl" >"${tmp_dir}/baselines.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/baselines.yaml"
for artifact in e2e-rwx-baseline e2e-rwo-direct e2e-full-baseline; do
  "${kubectl_bin}" -n "${namespace}" wait \
    --for=condition=Ready=True "modelartifact/${artifact}" --timeout=5m
done

artifact_snapshot() {
  local artifact=$1
  "${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact}" -o json | jq -cS '
    {
      artifactDigest: .status.artifactDigest,
      files: .status.files,
      location: .status.location,
      ready: (.status.conditions | map(select(.type == "Ready")) | first)
    }
  '
}

artifact_location() {
  local artifact=$1
  local field=$2
  "${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact}" \
    -o "jsonpath={.status.location.${field}}"
}

render_reader() {
  local reader_name=$1
  local node=$2
  local claim=$3
  local subpath=$4
  local output=$5
  require_dns_name "${reader_name}" "reader name"
  require_dns_name "${node}" "reader node"
  require_dns_name "${claim}" "reader claim"
  require_subpath "${subpath}"
  sed \
    -e "s|E2E_READER_NAME|${reader_name}|g" \
    -e "s|E2E_NAMESPACE|${namespace}|g" \
    -e "s|E2E_READER_NODE|${node}|g" \
    -e "s|E2E_HELPER_IMAGE|${helper_image}|g" \
    -e "s|E2E_ARTIFACT_CLAIM|${claim}|g" \
    -e "s|E2E_ARTIFACT_SUBPATH|${subpath}|g" \
    "${fixture_dir}/reader-pod.yaml.tmpl" >"${output}"
}

reader_result() {
  local reader_name=$1
  local output_file="${evidence_dir}/reader-${reader_name}.log"
  mkdir -p "${evidence_dir}"
  "${kubectl_bin}" -n "${namespace}" logs "pod/${reader_name}" >"${output_file}"
  if ! grep -Fq 'E2E_MMAP_HELD=true' "${output_file}"; then
    fail "reader ${reader_name} did not report mmap verification"
  fi
  sed -n 's/^E2E_TREE_DIGEST=//p' "${output_file}" | tail -n 1
}

start_reader() {
  local reader_name=$1
  local node=$2
  local claim=$3
  local subpath=$4
  render_reader "${reader_name}" "${node}" "${claim}" "${subpath}" \
    "${tmp_dir}/${reader_name}.yaml"
  "${kubectl_bin}" apply -f "${tmp_dir}/${reader_name}.yaml"
}

wait_reader() {
  local reader_name=$1
  local node=$2
  "${kubectl_bin}" -n "${namespace}" wait --for=condition=Ready=True \
    "pod/${reader_name}" --timeout=3m
  local actual_node
  local actual_hostname
  actual_node="$("${kubectl_bin}" -n "${namespace}" get pod "${reader_name}" \
    -o jsonpath='{.spec.nodeName}')"
  actual_hostname="$("${kubectl_bin}" get node "${actual_node}" \
    -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}')"
  if [[ "${actual_hostname}" != "${node}" ]]; then
    fail "reader ${reader_name} ran on ${actual_hostname}, want ${node}"
  fi
}

rwx_before_status="$(artifact_snapshot e2e-rwx-baseline)"
rwx_claim="$(artifact_location e2e-rwx-baseline claimName)"
rwx_subpath="$(artifact_location e2e-rwx-baseline subPath)"
require_subpath "${rwx_subpath}"
if ! jq -e --arg digest "${valid_sha256}" '
  .status.location.mountScope == "MultiNode" and
  (.status.location.accessModes | index("ReadWriteMany") != null) and
  .status.files == [{"path":"model.gguf","sha256":$digest,"size":180}]
' <<<"$("${kubectl_bin}" -n "${namespace}" get modelartifact e2e-rwx-baseline -o json)" >/dev/null; then
  fail "RWX baseline status does not report a MultiNode verified artifact"
fi

log "proving simultaneous read-only mmap and hash access on both Kind nodes"
start_reader e2e-rwx-reader-worker "${worker_hostname}" "${rwx_claim}" "${rwx_subpath}"
start_reader e2e-rwx-reader-control "${control_hostname}" "${rwx_claim}" "${rwx_subpath}"
wait_reader e2e-rwx-reader-worker "${worker_hostname}"
wait_reader e2e-rwx-reader-control "${control_hostname}"
rwx_worker_digest="$(reader_result e2e-rwx-reader-worker)"
rwx_control_digest="$(reader_result e2e-rwx-reader-control)"
if [[ -z "${rwx_worker_digest}" || "${rwx_worker_digest}" != "${rwx_control_digest}" ]]; then
  fail "concurrent RWX readers returned different publication digests"
fi
"${kubectl_bin}" -n "${namespace}" delete pod \
  e2e-rwx-reader-worker e2e-rwx-reader-control --wait --timeout=60s >/dev/null

log "strengthening RWO placement evidence with matching and conflicting consumers"
rwo_json="$("${kubectl_bin}" -n "${namespace}" get modelartifact e2e-rwo-direct -o json)"
rwo_pv_json="$("${kubectl_bin}" get pv e2e-storage-source -o json)"
rwo_job="$(jq -r '.status.jobRef.name' <<<"${rwo_json}")"
rwo_job_node="$("${kubectl_bin}" -n "${namespace}" get pods -l "job-name=${rwo_job}" -o json | \
  jq -r '.items[0].spec.nodeName // ""')"
rwo_job_hostname="$("${kubectl_bin}" get node "${rwo_job_node}" \
  -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}' 2>/dev/null || true)"
if [[ "${rwo_job_hostname}" != "${worker_hostname}" ]] || ! jq -e \
  --argjson pv "${rwo_pv_json}" --arg worker "${worker_hostname}" '
    .status.location.mountScope == "SingleNode" and
    .status.location.volumeName == $pv.metadata.name and
    .status.location.volumeUID == $pv.metadata.uid and
    .status.location.nodeAffinity == $pv.spec.nodeAffinity and
    (.status.location.accessModes | index("ReadWriteOnce") != null) and
    (.status.location.nodeAffinity.required.nodeSelectorTerms[].matchExpressions[] |
      select(.key == "kubernetes.io/hostname") | .values | index($worker) != null)
  ' <<<"${rwo_json}" >/dev/null; then
  fail "Direct RWO artifact status, PV affinity, and importer scheduling do not agree"
fi
rwo_claim="$(jq -r '.status.location.claimName' <<<"${rwo_json}")"
rwo_subpath="$(jq -r '.status.location.subPath' <<<"${rwo_json}")"
start_reader e2e-rwo-reader "${worker_hostname}" "${rwo_claim}" "${rwo_subpath}"
wait_reader e2e-rwo-reader "${worker_hostname}"
rwo_digest="$(reader_result e2e-rwo-reader)"
"${kubectl_bin}" -n "${namespace}" delete pod e2e-rwo-reader \
  --wait --timeout=60s >/dev/null

sed \
  -e "s|E2E_NAMESPACE|${namespace}|g" \
  -e "s|E2E_WRONG_NODE|${control_hostname}|g" \
  -e "s|E2E_HELPER_IMAGE|${helper_image}|g" \
  -e "s|E2E_RWO_SUBPATH|${rwo_subpath}|g" \
  "${fixture_dir}/unschedulable-reader.yaml.tmpl" >"${tmp_dir}/rwo-wrong.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/rwo-wrong.yaml"
rwo_conflict_observed=0
for _ in $(seq 1 60); do
  scheduled="$("${kubectl_bin}" -n "${namespace}" get pod e2e-rwo-wrong-node \
    -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].status}' 2>/dev/null || true)"
  event_messages="$("${kubectl_bin}" -n "${namespace}" get events \
    --field-selector involvedObject.kind=Pod,involvedObject.name=e2e-rwo-wrong-node \
    -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null || true)"
  # Kubernetes 1.35 and earlier commonly reported a "volume node affinity
  # conflict" here. Kubernetes 1.36 reports that the node "didn't match
  # PersistentVolume's node affinity" instead. Both prove the same RWO
  # placement guarantee, so keep the assertion compatible with either form.
  if [[ "${scheduled}" == "False" ]] && \
    grep -Eiq 'volume node affinity conflict|PersistentVolume.*node affinity' <<<"${event_messages}"; then
    rwo_conflict_observed=1
    printf '%s\n' "${event_messages}" >"${evidence_dir}/rwo-wrong-node-events.txt"
    break
  fi
  sleep 2
done
if [[ ${rwo_conflict_observed} -ne 1 ]]; then
  fail "the wrong-node RWO consumer did not report a volume node-affinity conflict"
fi
"${kubectl_bin}" -n "${namespace}" delete pod e2e-rwo-wrong-node \
  --wait --timeout=60s >/dev/null

log "exercising checksum, malformed, truncated, incomplete, and unauthorized failures"
sed -e "s|E2E_NAMESPACE|${namespace}|g" \
  "${fixture_dir}/failures.yaml.tmpl" >"${tmp_dir}/failures.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/failures.yaml"
declare -A expected_conditions=(
  [e2e-checksum-mismatch]=ChecksumMismatch
  [e2e-malformed]=InvalidGGUF
  [e2e-truncated]=InvalidGGUF
  [e2e-incomplete]=MissingShard
  [e2e-unauthorized]=SourceUnavailable
)
for artifact in "${!expected_conditions[@]}"; do
  condition="${expected_conditions[${artifact}]}"
  "${kubectl_bin}" -n "${namespace}" wait --for="condition=${condition}=True" \
    "modelartifact/${artifact}" --timeout=5m
  ready_status="$("${kubectl_bin}" -n "${namespace}" get modelartifact "${artifact}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  if [[ "${ready_status}" == "True" ]]; then
    fail "failure artifact ${artifact} incorrectly became Ready"
  fi
done
if [[ "$(artifact_snapshot e2e-rwx-baseline)" != "${rwx_before_status}" ]]; then
  fail "failure scenarios changed the pre-existing RWX baseline status"
fi
start_reader e2e-rwx-reader-after "${control_hostname}" "${rwx_claim}" "${rwx_subpath}"
wait_reader e2e-rwx-reader-after "${control_hostname}"
rwx_after_digest="$(reader_result e2e-rwx-reader-after)"
"${kubectl_bin}" -n "${namespace}" delete pod e2e-rwx-reader-after \
  --wait --timeout=60s >/dev/null
if [[ "${rwx_after_digest}" != "${rwx_worker_digest}" ]]; then
  fail "failure scenarios changed bytes in the pre-existing RWX publication"
fi

log "forcing and observing genuine ENOSPC without replacing the fixture with a capacity declaration"
full_before_status="$(artifact_snapshot e2e-full-baseline)"
full_claim="$(artifact_location e2e-full-baseline claimName)"
full_subpath="$(artifact_location e2e-full-baseline subPath)"
start_reader e2e-full-reader-before "${worker_hostname}" "${full_claim}" "${full_subpath}"
wait_reader e2e-full-reader-before "${worker_hostname}"
full_before_digest="$(reader_result e2e-full-reader-before)"
"${kubectl_bin}" -n "${namespace}" delete pod e2e-full-reader-before \
  --wait --timeout=60s >/dev/null

sed \
  -e "s|E2E_NAMESPACE|${namespace}|g" \
  -e "s|E2E_WORKER_NODE|${worker_hostname}|g" \
  -e "s|E2E_HELPER_IMAGE|${helper_image}|g" \
  "${fixture_dir}/fill-job.yaml.tmpl" >"${tmp_dir}/fill-job.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/fill-job.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=Complete=True \
  job/e2e-storage-fill --timeout=2m
fill_log="$("${kubectl_bin}" -n "${namespace}" logs job/e2e-storage-fill)"
printf '%s\n' "${fill_log}" >"${evidence_dir}/enospc-fill.log"
if ! grep -Fq 'E2E_ENOSPC_OBSERVED=true' <<<"${fill_log}"; then
  fail "the bounded tmpfs fixture did not report a real ENOSPC write failure"
fi

sed -e "s|E2E_NAMESPACE|${namespace}|g" \
  "${fixture_dir}/full-failure.yaml.tmpl" >"${tmp_dir}/full-failure.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/full-failure.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=InsufficientStorage=True \
  modelartifact/e2e-storage-full --timeout=5m
if [[ "$("${kubectl_bin}" -n "${namespace}" get modelartifact e2e-storage-full \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == "True" ]]; then
  fail "storage-full artifact incorrectly became Ready"
fi
if [[ "$(artifact_snapshot e2e-full-baseline)" != "${full_before_status}" ]]; then
  fail "the storage-full scenario changed the pre-existing full-volume baseline status"
fi
start_reader e2e-full-reader-after "${worker_hostname}" "${full_claim}" "${full_subpath}"
wait_reader e2e-full-reader-after "${worker_hostname}"
full_after_digest="$(reader_result e2e-full-reader-after)"
"${kubectl_bin}" -n "${namespace}" delete pod e2e-full-reader-after \
  --wait --timeout=60s >/dev/null
if [[ "${full_after_digest}" != "${full_before_digest}" ]]; then
  fail "the storage-full scenario changed bytes in the pre-existing publication"
fi

mkdir -p "${evidence_dir}"
jq -n \
  --arg namespace "${namespace}" \
  --arg cluster "${cluster_name}" \
  --arg workerNode "${worker_hostname}" \
  --arg controlPlaneNode "${control_hostname}" \
  --arg rwxStorageClass "${rwx_storage_class}" \
  --arg rwxPV "${rwx_pv}" \
  --arg rwxCSIDriver "${rwx_driver}" \
  --arg rwxPublicationDigest "${rwx_worker_digest}" \
  --arg rwoPublicationDigest "${rwo_digest}" \
  --arg fullPublicationDigest "${full_before_digest}" \
  --arg validFileSHA256 "${valid_sha256}" \
  '{
    schemaVersion: 1,
    suite: "artifact-plane/storage",
    outcome: "passed",
    namespace: $namespace,
    cluster: $cluster,
    nodes: {worker: $workerNode, controlPlane: $controlPlaneNode},
    rwx: {
      storageClass: $rwxStorageClass,
      persistentVolume: $rwxPV,
      csiDriver: $rwxCSIDriver,
      concurrentReadOnlyMmap: true,
      publicationDigest: $rwxPublicationDigest
    },
    rwo: {
      matchingReaderScheduled: true,
      conflictingReaderUnschedulable: true,
      publicationDigest: $rwoPublicationDigest
    },
    failures: {
      checksumMismatch: true,
      malformedGGUF: true,
      truncatedGGUF: true,
      incompleteShardSet: true,
      unauthorizedHub: true,
      genuineENOSPC: true,
      readyPublicationUnchanged: true
    },
    fullVolumePublicationDigest: $fullPublicationDigest,
    validFileSHA256: $validFileSHA256
  }' >"${evidence_dir}/summary.json"
summary_written=1
collect_evidence
log "artifact-plane storage/failure suite passed; evidence: ${evidence_dir}"
