#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kubectl_bin="${KUBECTL:-kubectl}"
kind_bin="${KIND:-${repo_root}/bin/kind}"
cluster_name="${KIND_CLUSTER:-kama}"
namespace="${M1_FUNCTIONAL_NAMESPACE:-kama-m1-functional}"
operator_namespace="${M1_OPERATOR_NAMESPACE:-kama-system}"
manager_deployment="${M1_MANAGER_DEPLOYMENT:-kama}"
rwx_storage_class="${M1_RWX_STORAGE_CLASS:?M1_RWX_STORAGE_CLASS must name a preinstalled RWX CSI StorageClass}"
expected_csi_driver="${M1_RWX_CSI_DRIVER:-nfs.csi.k8s.io}"
helper_image="${M1_FUNCTIONAL_HELPER_IMAGE:-local/kama-m1-functional-helper:dev}"
fake_hub_endpoint="${M1_FAKE_HUB_ENDPOINT:-http://kama-fake-huggingface.kama-system.svc.cluster.local:8083}"
evidence_dir="${M1_EVIDENCE_DIR:-${repo_root}/dist/m1-functional}"
fixture_dir="${repo_root}/test/m1-functional"
valid_sha256="0e1ec3e53960fdc1556515b1368222af8acbd0d346447b1a724bdaaeb3fa1e94"
tmp_dir="$(mktemp -d)"
worker_container=""
worker_hostname=""
control_hostname=""
rwx_pv=""
created_resources=0
evidence_collected=0

log() {
  printf '[m1-functional] %s\n' "$*"
}

fail() {
  printf '[m1-functional] ERROR: %s\n' "$*" >&2
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
  "${kubectl_bin}" get pv m1-functional-source m1-functional-full \
    -o yaml >"${evidence_dir}/local-pvs.yaml" 2>&1 || true
  "${kubectl_bin}" get storageclass "${rwx_storage_class}" \
    -o yaml >"${evidence_dir}/rwx-storage-class.yaml" 2>&1 || true
  if [[ -z "${rwx_pv}" ]]; then
    rwx_pv="$("${kubectl_bin}" -n "${namespace}" get pvc m1-functional-rwx \
      -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)"
  fi
  if [[ -n "${rwx_pv}" ]]; then
    "${kubectl_bin}" get pv "${rwx_pv}" -o yaml \
      >"${evidence_dir}/rwx-pv.yaml" 2>&1 || true
  fi
  "${kubectl_bin}" -n "${operator_namespace}" get deployment/m1-functional-nfs \
    -o yaml >"${evidence_dir}/nfs-server-deployment.yaml" 2>&1 || true
  "${kubectl_bin}" -n "${operator_namespace}" logs deployment/m1-functional-nfs \
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
  collect_evidence
  if [[ ${created_resources} -eq 1 && "${M1_FUNCTIONAL_KEEP:-0}" != "1" ]]; then
    "${kubectl_bin}" delete namespace "${namespace}" --ignore-not-found \
      --wait --timeout=2m >/dev/null 2>&1 || true
    "${kubectl_bin}" delete pv m1-functional-source m1-functional-full \
      --ignore-not-found --wait --timeout=60s >/dev/null 2>&1 || true
    "${kubectl_bin}" delete storageclass m1-functional-source-local m1-functional-full-local \
      --ignore-not-found >/dev/null 2>&1 || true
    if [[ -n "${worker_container}" ]]; then
      docker exec "${worker_container}" umount /var/local/kama-m1-functional-full \
        >/dev/null 2>&1 || true
      docker exec "${worker_container}" rm -rf -- \
        /var/local/kama-m1-functional-source /var/local/kama-m1-functional-full \
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
require_dns_name "${namespace}" "M1 functional namespace"
require_dns_name "${operator_namespace}" "operator namespace"
require_dns_name "${manager_deployment}" "manager Deployment"
require_dns_name "${rwx_storage_class}" "RWX StorageClass"
if [[ ! "${expected_csi_driver}" =~ ^[A-Za-z0-9._/-]+$ ]]; then
  fail "M1_RWX_CSI_DRIVER contains unsafe characters: ${expected_csi_driver}"
fi
if [[ ! "${helper_image}" =~ ^[A-Za-z0-9./_:@-]+$ ]]; then
  fail "M1_FUNCTIONAL_HELPER_IMAGE contains unsafe characters"
fi

if ! "${kind_bin}" get clusters | grep -Fxq "${cluster_name}"; then
  fail "Kind cluster ${cluster_name} does not exist"
fi
if "${kubectl_bin}" get namespace "${namespace}" >/dev/null 2>&1; then
  fail "namespace ${namespace} already exists; refusing to reuse or delete it"
fi
for cluster_resource in \
  pv/m1-functional-source pv/m1-functional-full \
  storageclass/m1-functional-source-local storageclass/m1-functional-full-local; do
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
  'test ! -e /var/local/kama-m1-functional-source && test ! -e /var/local/kama-m1-functional-full'; then
  fail "M1 functional host paths already exist on ${worker_container}"
fi
docker exec "${worker_container}" mkdir -p \
  /var/local/kama-m1-functional-source/models /var/local/kama-m1-functional-full
created_resources=1
docker exec "${worker_container}" mount -t tmpfs \
  -o size=8m,mode=0777,nosuid,nodev tmpfs /var/local/kama-m1-functional-full

base64 --decode "${repo_root}/internal/testfixtures/gguf/testdata/valid-minimal.gguf.b64" \
  >"${tmp_dir}/model.gguf"
base64 --decode "${repo_root}/internal/testfixtures/gguf/testdata/malformed-magic.gguf.b64" \
  >"${tmp_dir}/malformed.gguf"
base64 --decode "${repo_root}/internal/testfixtures/gguf/testdata/truncated-metadata.gguf.b64" \
  >"${tmp_dir}/truncated.gguf"
cp "${tmp_dir}/model.gguf" "${tmp_dir}/model-00001-of-00002.gguf"
for fixture in model.gguf malformed.gguf truncated.gguf model-00001-of-00002.gguf; do
  docker cp "${tmp_dir}/${fixture}" \
    "${worker_container}:/var/local/kama-m1-functional-source/models/${fixture}"
done
docker exec "${worker_container}" chmod -R a+rX /var/local/kama-m1-functional-source

sed \
  -e "s|M1_NAMESPACE|${namespace}|g" \
  -e "s|M1_WORKER_NODE|${worker_hostname}|g" \
  -e "s|M1_RWX_STORAGE_CLASS|${rwx_storage_class}|g" \
  "${fixture_dir}/storage.yaml.tmpl" >"${tmp_dir}/storage.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/storage.yaml"
for claim in m1-functional-source m1-functional-full m1-functional-rwx; do
  "${kubectl_bin}" -n "${namespace}" wait \
    --for=jsonpath='{.status.phase}'=Bound "pvc/${claim}" --timeout=3m
done

rwx_pv="$("${kubectl_bin}" -n "${namespace}" get pvc m1-functional-rwx \
  -o jsonpath='{.spec.volumeName}')"
rwx_driver="$("${kubectl_bin}" get pv "${rwx_pv}" -o jsonpath='{.spec.csi.driver}')"
if [[ -z "${rwx_pv}" || "${rwx_driver}" != "${expected_csi_driver}" ]]; then
  fail "RWX PVC resolved CSI driver ${rwx_driver:-none}, want ${expected_csi_driver}"
fi
for cache in m1-functional-rwx m1-functional-full; do
  "${kubectl_bin}" -n "${namespace}" wait \
    --for=condition=Ready=True "modelcache/${cache}" --timeout=5m
done

sed -e "s|M1_NAMESPACE|${namespace}|g" \
  "${fixture_dir}/baselines.yaml.tmpl" >"${tmp_dir}/baselines.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/baselines.yaml"
for artifact in m1-rwx-baseline m1-rwo-direct m1-full-baseline; do
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
    -e "s|M1_READER_NAME|${reader_name}|g" \
    -e "s|M1_NAMESPACE|${namespace}|g" \
    -e "s|M1_READER_NODE|${node}|g" \
    -e "s|M1_HELPER_IMAGE|${helper_image}|g" \
    -e "s|M1_ARTIFACT_CLAIM|${claim}|g" \
    -e "s|M1_ARTIFACT_SUBPATH|${subpath}|g" \
    "${fixture_dir}/reader-pod.yaml.tmpl" >"${output}"
}

reader_result() {
  local reader_name=$1
  local output_file="${evidence_dir}/reader-${reader_name}.log"
  mkdir -p "${evidence_dir}"
  "${kubectl_bin}" -n "${namespace}" logs "pod/${reader_name}" >"${output_file}"
  if ! grep -Fq 'M1_MMAP_HELD=true' "${output_file}"; then
    fail "reader ${reader_name} did not report mmap verification"
  fi
  sed -n 's/^M1_TREE_DIGEST=//p' "${output_file}" | tail -n 1
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
    "pod/${reader_name}" --timeout=2m
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

rwx_before_status="$(artifact_snapshot m1-rwx-baseline)"
rwx_claim="$(artifact_location m1-rwx-baseline claimName)"
rwx_subpath="$(artifact_location m1-rwx-baseline subPath)"
require_subpath "${rwx_subpath}"
if ! jq -e --arg digest "${valid_sha256}" '
  .status.location.mountScope == "MultiNode" and
  (.status.location.accessModes | index("ReadWriteMany") != null) and
  .status.files == [{"path":"model.gguf","sha256":$digest,"size":180}]
' <<<"$("${kubectl_bin}" -n "${namespace}" get modelartifact m1-rwx-baseline -o json)" >/dev/null; then
  fail "RWX baseline status does not report a MultiNode verified artifact"
fi

log "proving simultaneous read-only mmap and hash access on both Kind nodes"
start_reader m1-rwx-reader-worker "${worker_hostname}" "${rwx_claim}" "${rwx_subpath}"
start_reader m1-rwx-reader-control "${control_hostname}" "${rwx_claim}" "${rwx_subpath}"
wait_reader m1-rwx-reader-worker "${worker_hostname}"
wait_reader m1-rwx-reader-control "${control_hostname}"
rwx_worker_digest="$(reader_result m1-rwx-reader-worker)"
rwx_control_digest="$(reader_result m1-rwx-reader-control)"
if [[ -z "${rwx_worker_digest}" || "${rwx_worker_digest}" != "${rwx_control_digest}" ]]; then
  fail "concurrent RWX readers returned different publication digests"
fi
"${kubectl_bin}" -n "${namespace}" delete pod \
  m1-rwx-reader-worker m1-rwx-reader-control --wait --timeout=60s >/dev/null

log "strengthening RWO placement evidence with matching and conflicting consumers"
rwo_json="$("${kubectl_bin}" -n "${namespace}" get modelartifact m1-rwo-direct -o json)"
rwo_pv_json="$("${kubectl_bin}" get pv m1-functional-source -o json)"
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
start_reader m1-rwo-reader "${worker_hostname}" "${rwo_claim}" "${rwo_subpath}"
wait_reader m1-rwo-reader "${worker_hostname}"
rwo_digest="$(reader_result m1-rwo-reader)"
"${kubectl_bin}" -n "${namespace}" delete pod m1-rwo-reader \
  --wait --timeout=60s >/dev/null

sed \
  -e "s|M1_NAMESPACE|${namespace}|g" \
  -e "s|M1_WRONG_NODE|${control_hostname}|g" \
  -e "s|M1_HELPER_IMAGE|${helper_image}|g" \
  -e "s|M1_RWO_SUBPATH|${rwo_subpath}|g" \
  "${fixture_dir}/unschedulable-reader.yaml.tmpl" >"${tmp_dir}/rwo-wrong.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/rwo-wrong.yaml"
rwo_conflict_observed=0
for _ in $(seq 1 60); do
  scheduled="$("${kubectl_bin}" -n "${namespace}" get pod m1-rwo-wrong-node \
    -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].status}' 2>/dev/null || true)"
  event_messages="$("${kubectl_bin}" -n "${namespace}" get events \
    --field-selector involvedObject.kind=Pod,involvedObject.name=m1-rwo-wrong-node \
    -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null || true)"
  if [[ "${scheduled}" == "False" ]] && grep -Eiq 'volume node affinity conflict' <<<"${event_messages}"; then
    rwo_conflict_observed=1
    printf '%s\n' "${event_messages}" >"${evidence_dir}/rwo-wrong-node-events.txt"
    break
  fi
  sleep 2
done
if [[ ${rwo_conflict_observed} -ne 1 ]]; then
  fail "the wrong-node RWO consumer did not report a volume node-affinity conflict"
fi
"${kubectl_bin}" -n "${namespace}" delete pod m1-rwo-wrong-node \
  --wait --timeout=60s >/dev/null

log "exercising checksum, malformed, truncated, incomplete, and unauthorized failures"
sed -e "s|M1_NAMESPACE|${namespace}|g" \
  "${fixture_dir}/failures.yaml.tmpl" >"${tmp_dir}/failures.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/failures.yaml"
declare -A expected_conditions=(
  [m1-checksum-mismatch]=ChecksumMismatch
  [m1-malformed]=InvalidGGUF
  [m1-truncated]=InvalidGGUF
  [m1-incomplete]=MissingShard
  [m1-unauthorized]=SourceUnavailable
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
if [[ "$(artifact_snapshot m1-rwx-baseline)" != "${rwx_before_status}" ]]; then
  fail "failure scenarios changed the pre-existing RWX baseline status"
fi
start_reader m1-rwx-reader-after "${control_hostname}" "${rwx_claim}" "${rwx_subpath}"
wait_reader m1-rwx-reader-after "${control_hostname}"
rwx_after_digest="$(reader_result m1-rwx-reader-after)"
"${kubectl_bin}" -n "${namespace}" delete pod m1-rwx-reader-after \
  --wait --timeout=60s >/dev/null
if [[ "${rwx_after_digest}" != "${rwx_worker_digest}" ]]; then
  fail "failure scenarios changed bytes in the pre-existing RWX publication"
fi

log "forcing and observing genuine ENOSPC without replacing the fixture with a capacity declaration"
full_before_status="$(artifact_snapshot m1-full-baseline)"
full_claim="$(artifact_location m1-full-baseline claimName)"
full_subpath="$(artifact_location m1-full-baseline subPath)"
start_reader m1-full-reader-before "${worker_hostname}" "${full_claim}" "${full_subpath}"
wait_reader m1-full-reader-before "${worker_hostname}"
full_before_digest="$(reader_result m1-full-reader-before)"
"${kubectl_bin}" -n "${namespace}" delete pod m1-full-reader-before \
  --wait --timeout=60s >/dev/null

sed \
  -e "s|M1_NAMESPACE|${namespace}|g" \
  -e "s|M1_WORKER_NODE|${worker_hostname}|g" \
  -e "s|M1_HELPER_IMAGE|${helper_image}|g" \
  "${fixture_dir}/fill-job.yaml.tmpl" >"${tmp_dir}/fill-job.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/fill-job.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=Complete=True \
  job/m1-functional-fill --timeout=2m
fill_log="$("${kubectl_bin}" -n "${namespace}" logs job/m1-functional-fill)"
printf '%s\n' "${fill_log}" >"${evidence_dir}/enospc-fill.log"
if ! grep -Fq 'M1_ENOSPC_OBSERVED=true' <<<"${fill_log}"; then
  fail "the bounded tmpfs fixture did not report a real ENOSPC write failure"
fi

sed -e "s|M1_NAMESPACE|${namespace}|g" \
  "${fixture_dir}/full-failure.yaml.tmpl" >"${tmp_dir}/full-failure.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/full-failure.yaml"
"${kubectl_bin}" -n "${namespace}" wait --for=condition=InsufficientStorage=True \
  modelartifact/m1-storage-full --timeout=5m
if [[ "$("${kubectl_bin}" -n "${namespace}" get modelartifact m1-storage-full \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == "True" ]]; then
  fail "storage-full artifact incorrectly became Ready"
fi
if [[ "$(artifact_snapshot m1-full-baseline)" != "${full_before_status}" ]]; then
  fail "the storage-full scenario changed the pre-existing full-volume baseline status"
fi
start_reader m1-full-reader-after "${worker_hostname}" "${full_claim}" "${full_subpath}"
wait_reader m1-full-reader-after "${worker_hostname}"
full_after_digest="$(reader_result m1-full-reader-after)"
"${kubectl_bin}" -n "${namespace}" delete pod m1-full-reader-after \
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
    result: "pass",
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
collect_evidence
log "M1 functional storage/failure suite passed; evidence: ${evidence_dir}"
