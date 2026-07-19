#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(tr -d '\r\n' < "${repo_root}/VERSION")"
kind_bin="${KIND:-${repo_root}/bin/kind}"
kubectl_bin="${KUBECTL:-kubectl}"
helm_bin="${HELM:-${repo_root}/bin/helm}"
keda_version="${KEDA_VERSION:-2.20.0}"
cluster_name="${KIND_CLUSTER:-kama}"
node_image="${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must be a digest-pinned Kind node image}"
namespace="kama-system"
manager_image="${IMG:-local/kama-manager:${version}}"
importer_image="${IMPORTER_IMG:-local/kama-importer:${version}}"
fixtures_image="${FIXTURES_IMG:-local/kama-test-fixtures:${version}}"
created="$(git -C "${repo_root}" show -s --format=%cI HEAD 2>/dev/null || printf '1970-01-01T00:00:00Z')"
revision="$(git -C "${repo_root}" rev-parse HEAD 2>/dev/null || printf 'unknown')"
tmp_dir="$(mktemp -d)"
cluster_created=0
port_forward_pids=()

if [[ ! -x "${kind_bin}" ]]; then
  kind_bin="$(command -v kind || true)"
fi
if [[ ! -x "${helm_bin}" ]]; then
  helm_bin="$(command -v helm || true)"
fi
for tool in "${kind_bin}" "${kubectl_bin}" "${helm_bin}" docker curl jq base64; do
  if [[ -z "${tool}" ]] || ! command -v "${tool}" >/dev/null 2>&1; then
    echo "required command is unavailable: ${tool:-unset}" >&2
    exit 1
  fi
done

cleanup() {
  exit_code=$?
  for pid in "${port_forward_pids[@]:-}"; do
    kill "${pid}" >/dev/null 2>&1 || true
  done
  if [[ ${exit_code} -ne 0 && ${cluster_created} -eq 1 ]]; then
    "${kubectl_bin}" get pods -A -o wide || true
    "${kubectl_bin}" -n "${namespace}" describe pods || true
    "${kubectl_bin}" -n "${namespace}" get service kama-webhook -o yaml || true
    "${kubectl_bin}" -n "${namespace}" get endpointslice \
      -l kubernetes.io/service-name=kama-webhook -o yaml || true
    "${kubectl_bin}" -n "${namespace}" logs deployment/kama \
      --all-containers=true --prefix=true --tail=200 || true
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
  for _ in $(seq 1 60); do
    if curl --fail --silent --show-error "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for ${url}" >&2
  return 1
}

wait_for_condition() {
  local resource=$1
  local name=$2
  local condition=$3
  local timeout=${4:-5m}
  "${kubectl_bin}" -n "${namespace}" wait \
    --for="condition=${condition}=True" "${resource}/${name}" --timeout="${timeout}"
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
docker build \
  --file "${repo_root}/Dockerfile.test-fixtures" \
  --build-arg "VERSION=${version}" \
  --build-arg "VCS_REF=${revision}" \
  --build-arg "CREATED=${created}" \
  --tag "${fixtures_image}" \
  "${repo_root}"

"${kind_bin}" create cluster --name "${cluster_name}" --image "${node_image}" \
  --config "${repo_root}/test/kind/cluster.yaml" --wait 5m
cluster_created=1
"${kind_bin}" load docker-image --name "${cluster_name}" \
  "${manager_image}" "${importer_image}" "${fixtures_image}"

"${kubectl_bin}" create namespace "${namespace}"
worker_node="${cluster_name}-worker"
base64 --decode "${repo_root}/internal/testfixtures/gguf/testdata/valid-minimal.gguf.b64" \
  >"${tmp_dir}/model.gguf"
docker exec "${worker_node}" mkdir -p /var/local/kama-manual/models
docker exec "${worker_node}" mkdir -p /var/local/kama-cache
docker cp "${tmp_dir}/model.gguf" "${worker_node}:/var/local/kama-manual/models/model.gguf"
docker cp "${tmp_dir}/model.gguf" \
  "${worker_node}:/var/local/kama-manual/models/model-00001-of-00002.gguf"
docker exec "${worker_node}" chmod -R a+rX /var/local/kama-manual
docker exec "${worker_node}" chmod 0777 /var/local/kama-cache
sed "s|KAMA_WORKER_NODE|${worker_node}|g" \
  "${repo_root}/test/kind/m1-storage.yaml" >"${tmp_dir}/m1-storage.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/m1-storage.yaml"
"${kubectl_bin}" -n "${namespace}" wait \
  --for=jsonpath='{.status.phase}'=Bound pvc/kama-manual-models --timeout=60s

"${helm_bin}" repo add keda https://kedacore.github.io/charts --force-update
"${helm_bin}" repo update keda
"${helm_bin}" upgrade --install keda keda/keda \
  --namespace keda \
  --create-namespace \
  --version "${keda_version}" \
  --wait \
  --timeout 5m

sed "s|KAMA_FIXTURES_IMAGE|${fixtures_image}|g" \
  "${repo_root}/test/kind/fixtures.yaml" > "${tmp_dir}/fixtures.yaml"
"${kubectl_bin}" apply -f "${tmp_dir}/fixtures.yaml"
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama-external-scaler --timeout=2m
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama-fake-llama --timeout=2m
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama-fake-huggingface --timeout=2m

"${kubectl_bin}" -n "${namespace}" port-forward service/kama-external-scaler 18082:8082 \
  >"${tmp_dir}/external-scaler-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18082/state

sleep 5
initial_replicas="$("${kubectl_bin}" -n "${namespace}" get deployment kama-activation-target \
  -o jsonpath='{.spec.replicas}')"
if [[ "${initial_replicas}" != "0" ]]; then
  echo "activation target did not remain at zero while the external metric was inactive" >&2
  exit 1
fi

curl --fail --silent --show-error \
  --request PUT \
  --header 'Content-Type: application/json' \
  --data '{"metric":1}' \
  http://127.0.0.1:18082/state >/dev/null

for _ in $(seq 1 90); do
  replicas="$("${kubectl_bin}" -n "${namespace}" get deployment kama-activation-target \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  if [[ "${replicas:-0}" == "1" ]]; then
    break
  fi
  sleep 2
done
replicas="$("${kubectl_bin}" -n "${namespace}" get deployment kama-activation-target \
  -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
if [[ "${replicas:-0}" != "1" ]]; then
  echo "KEDA external scaler did not activate the zero-replica deployment" >&2
  exit 1
fi

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
  --set-string importer.hubEndpoint=http://kama-fake-huggingface.kama-system.svc.cluster.local:8083 \
  --set metrics.enabled=true \
  --set metrics.secure=false \
  --wait \
  --timeout 2m
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama --timeout=2m
manager_rollout_json="$("${kubectl_bin}" -n "${namespace}" get deployment kama -o json)"
if ! jq -e '
  .spec.minReadySeconds == 10 and
  .spec.strategy.type == "RollingUpdate" and
  .spec.strategy.rollingUpdate.maxUnavailable == 0 and
  .spec.strategy.rollingUpdate.maxSurge == 1
' <<<"${manager_rollout_json}" >/dev/null; then
  echo "manager rollout policy does not preserve a ready admission endpoint" >&2
  exit 1
fi
"${helm_bin}" test kama --namespace "${namespace}" --timeout 2m

"${kubectl_bin}" wait --for=condition=Established \
  crd/modelcaches.kama.tannerburns.github.io \
  crd/modelartifacts.kama.tannerburns.github.io \
  --timeout=60s

manager_service_account="system:serviceaccount:${namespace}:kama"
for verb in patch update; do
  if [[ "$("${kubectl_bin}" auth can-i "${verb}" persistentvolumeclaims \
    --as="${manager_service_account}" --namespace "${namespace}")" != "yes" ]]; then
    echo "manager RBAC cannot ${verb} PVC deletion guards" >&2
    exit 1
  fi
done

for example in "${repo_root}"/examples/*.yaml; do
  "${kubectl_bin}" -n "${namespace}" create --dry-run=server -f "${example}" >/dev/null
done

default_retention="$("${kubectl_bin}" -n "${namespace}" create --dry-run=server \
  -f - -o jsonpath='{.spec.retentionPolicy}' <<'EOF'
apiVersion: kama.tannerburns.github.io/v1alpha1
kind: ModelCache
metadata:
  name: admission-default-check
spec:
  storage:
    existingClaim:
      name: does-not-need-to-exist-for-admission
EOF
)"
if [[ "${default_retention}" != "Retain" ]]; then
  echo "ModelCache admission did not default retentionPolicy to Retain" >&2
  exit 1
fi

default_import_policy="$("${kubectl_bin}" -n "${namespace}" create --dry-run=server \
  -f - -o jsonpath='{.spec.source.persistentVolumeClaim.importPolicy}' <<'EOF'
apiVersion: kama.tannerburns.github.io/v1alpha1
kind: ModelArtifact
metadata:
  name: admission-artifact-default-check
spec:
  format: GGUF
  entrypoint: model.gguf
  cacheRef:
    name: admission-default-check
  source:
    persistentVolumeClaim:
      claimName: does-not-need-to-exist-for-admission
      rootPath: models
EOF
)"
if [[ "${default_import_policy}" != "Copy" ]]; then
  echo "ModelArtifact admission did not default PVC importPolicy to Copy" >&2
  exit 1
fi

if "${kubectl_bin}" -n "${namespace}" create --dry-run=server \
  -f - >"${tmp_dir}/invalid-admission.log" 2>&1 <<'EOF'
apiVersion: kama.tannerburns.github.io/v1alpha1
kind: ModelArtifact
metadata:
  name: invalid-direct-cache-reference
spec:
  format: GGUF
  entrypoint: model.gguf
  cacheRef:
    name: forbidden-for-direct
  source:
    persistentVolumeClaim:
      claimName: manual-models
      rootPath: models
      importPolicy: Direct
EOF
then
  echo "validating admission accepted a Direct artifact with cacheRef" >&2
  exit 1
fi
if ! grep -Fq "cacheRef is forbidden for Direct import policy" \
  "${tmp_dir}/invalid-admission.log"; then
  echo "Direct/cacheRef admission was rejected for an unexpected reason:" >&2
  sed -n '1,20p' "${tmp_dir}/invalid-admission.log" >&2
  exit 1
fi

initial_webhook_ca="$("${kubectl_bin}" -n "${namespace}" get secret kama-webhook-tls \
  -o jsonpath='{.data.ca\.crt}')"
initial_webhook_leaf="$("${kubectl_bin}" -n "${namespace}" get secret kama-webhook-tls \
  -o jsonpath='{.data.tls\.crt}')"
"${helm_bin}" upgrade kama "${chart_package}" \
  --namespace "${namespace}" --reuse-values --wait --timeout 2m
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama --timeout=2m
upgraded_webhook_ca="$("${kubectl_bin}" -n "${namespace}" get secret kama-webhook-tls \
  -o jsonpath='{.data.ca\.crt}')"
upgraded_webhook_leaf="$("${kubectl_bin}" -n "${namespace}" get secret kama-webhook-tls \
  -o jsonpath='{.data.tls\.crt}')"
if [[ -z "${initial_webhook_ca}" ]] || [[ "${upgraded_webhook_ca}" != "${initial_webhook_ca}" ]]; then
  echo "Helm upgrade did not preserve the webhook CA" >&2
  exit 1
fi
if [[ -z "${initial_webhook_leaf}" ]] || [[ "${upgraded_webhook_leaf}" == "${initial_webhook_leaf}" ]]; then
  echo "Helm upgrade did not refresh the webhook serving certificate" >&2
  exit 1
fi
post_upgrade_admission_ready=0
post_upgrade_admission_log="${tmp_dir}/post-upgrade-admission.log"
# A failed request may consume the 10-second webhook timeout. Eight attempts
# keep the convergence window bounded below two minutes.
for _ in $(seq 1 8); do
  if "${kubectl_bin}" -n "${namespace}" create --dry-run=server -f - \
    >/dev/null 2>"${post_upgrade_admission_log}" <<'EOF'
apiVersion: kama.tannerburns.github.io/v1alpha1
kind: ModelCache
metadata:
  name: admission-after-certificate-upgrade
spec:
  storage:
    existingClaim:
      name: admission-only
EOF
  then
    post_upgrade_admission_ready=1
    break
  fi
  sleep 2
done
if [[ ${post_upgrade_admission_ready} -ne 1 ]]; then
  echo "admission did not recover after the webhook certificate-refresh rollout:" >&2
  sed -n '1,20p' "${post_upgrade_admission_log}" >&2
  exit 1
fi

"${kubectl_bin}" apply -f "${repo_root}/test/kind/m1-cache.yaml"
wait_for_condition modelcache kind-cache Ready 5m
managed_claim="$("${kubectl_bin}" -n "${namespace}" get modelcache kind-cache \
  -o jsonpath='{.status.claimName}')"
if [[ -z "${managed_claim}" ]]; then
  echo "managed ModelCache did not report its claim" >&2
  exit 1
fi

"${kubectl_bin}" -n "${namespace}" port-forward service/kama-fake-huggingface 18083:8083 \
  >"${tmp_dir}/fake-huggingface-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18083/health
curl --fail --silent --show-error --request PUT http://127.0.0.1:18083/state/reset >/dev/null

"${kubectl_bin}" apply -f "${repo_root}/test/kind/m1-artifact-public.yaml"
wait_for_condition modelartifact kind-public Ready 5m
"${kubectl_bin}" apply -f "${repo_root}/test/kind/m1-artifact-private.yaml"
wait_for_condition modelartifact kind-private Ready 5m

manager_json="$("${kubectl_bin}" -n "${namespace}" get deployment kama -o json)"
if ! jq -e '
  .spec.template.spec.enableServiceLinks == false and
  .spec.template.spec.securityContext.runAsNonRoot == true and
  .spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
  ([.spec.template.spec.containers[] | select(.name == "manager") |
    .securityContext.runAsNonRoot == true and
    .securityContext.runAsUser == 65532 and
    .securityContext.runAsGroup == 65532 and
    .securityContext.readOnlyRootFilesystem == true and
    .securityContext.allowPrivilegeEscalation == false and
    (.securityContext.capabilities.drop | index("ALL") != null)] | all)
' <<<"${manager_json}" >/dev/null; then
  echo "manager Pod security defaults are incomplete" >&2
  exit 1
fi
manager_image_ref="$(jq -r '.spec.template.spec.containers[] | select(.name == "manager") | .image' \
  <<<"${manager_json}")"
public_job="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-public \
  -o jsonpath='{.status.jobRef.name}')"
importer_job_json="$("${kubectl_bin}" -n "${namespace}" get job "${public_job}" -o json)"
if ! jq -e '
  .spec.template.spec.automountServiceAccountToken == false and
  .spec.template.spec.enableServiceLinks == false and
  .spec.template.spec.securityContext.runAsNonRoot == true and
  .spec.template.spec.securityContext.runAsUser == 65532 and
  .spec.template.spec.securityContext.runAsGroup == 65532 and
  .spec.template.spec.securityContext.fsGroup == 65532 and
  .spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" and
  .spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
  ([.spec.template.spec.containers[] | select(.name == "importer") |
    .securityContext.runAsNonRoot == true and
    .securityContext.runAsUser == 65532 and
    .securityContext.runAsGroup == 65532 and
    .securityContext.readOnlyRootFilesystem == true and
    .securityContext.allowPrivilegeEscalation == false and
    (.securityContext.capabilities.drop | index("ALL") != null)] | all)
' <<<"${importer_job_json}" >/dev/null; then
  echo "importer Job security defaults are incomplete" >&2
  exit 1
fi
importer_image_ref="$(jq -r '.spec.template.spec.containers[] | select(.name == "importer") | .image' \
  <<<"${importer_job_json}")"
if [[ "${manager_image_ref}" != "${manager_image}" || "${importer_image_ref}" != "${importer_image}" ]]; then
  echo "installed manager/importer image references do not match the requested release" >&2
  exit 1
fi

download_state="$(curl --fail --silent --show-error http://127.0.0.1:18083/state)"
if [[ "$(jq -r '.downloads["kama/public-gguf/model.gguf"] // 0' <<<"${download_state}")" != "1" ]] || \
  [[ "$(jq -r '.downloads["kama/private-gguf/model.gguf"] // 0' <<<"${download_state}")" != "1" ]]; then
  echo "public/private fixture artifacts were not downloaded exactly once: ${download_state}" >&2
  exit 1
fi

"${kubectl_bin}" -n "${namespace}" rollout restart deployment/kama
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama --timeout=2m
wait_for_condition modelartifact kind-public Ready 2m
wait_for_condition modelartifact kind-private Ready 2m
post_restart_state="$(curl --fail --silent --show-error http://127.0.0.1:18083/state)"
if [[ "$(jq -r '.downloads["kama/public-gguf/model.gguf"] // 0' <<<"${post_restart_state}")" != "1" ]] || \
  [[ "$(jq -r '.downloads["kama/private-gguf/model.gguf"] // 0' <<<"${post_restart_state}")" != "1" ]]; then
  echo "manager restart redownloaded a ready artifact: ${post_restart_state}" >&2
  exit 1
fi

resume_target="kama/public-gguf/model.gguf"
curl --fail --silent --show-error --request PUT http://127.0.0.1:18083/state/reset >/dev/null
curl --fail --silent --show-error \
  --request PUT \
  --header 'Content-Type: application/json' \
  --data '{"target":"kama/public-gguf/model.gguf","pauseAfterBytes":64}' \
  http://127.0.0.1:18083/state/fault >/dev/null
"${kubectl_bin}" apply -f "${repo_root}/test/kind/m1-artifact-resume.yaml"

resume_state=""
for _ in $(seq 1 120); do
  resume_state="$(curl --fail --silent --show-error http://127.0.0.1:18083/state)"
  if [[ "$(jq -r --arg target "${resume_target}" \
    '.transfers[$target].activePauses // 0' <<<"${resume_state}")" == "1" ]]; then
    break
  fi
  sleep 1
done
if [[ "$(jq -r --arg target "${resume_target}" \
  '.transfers[$target].activePauses // 0' <<<"${resume_state}")" != "1" ]]; then
  echo "resume fixture transfer did not reach its partial-write pause: ${resume_state}" >&2
  exit 1
fi
resume_job=""
resume_pod=""
for _ in $(seq 1 30); do
  resume_job="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-resume \
    -o jsonpath='{.status.jobRef.name}')"
  resume_pod="$("${kubectl_bin}" -n "${namespace}" get pods \
    --selector kama.tannerburns.github.io/model-artifact=kind-resume -o json | \
    jq -r '[.items[] | select(.status.phase == "Running") | .metadata.name][0] // ""')"
  if [[ -n "${resume_job}" && -n "${resume_pod}" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "${resume_job}" || -z "${resume_pod}" ]]; then
  echo "paused resume artifact did not expose its deterministic Job and running Pod" >&2
  exit 1
fi
resume_pod_uid="$("${kubectl_bin}" -n "${namespace}" get pod "${resume_pod}" \
  -o jsonpath='{.metadata.uid}')"
"${kubectl_bin}" -n "${namespace}" delete pod "${resume_pod}" --wait --timeout=60s
wait_for_condition modelartifact kind-resume Ready 5m
"${kubectl_bin}" -n "${namespace}" wait --for=condition=complete \
  "job/${resume_job}" --timeout=2m

resumed_job="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-resume \
  -o jsonpath='{.status.jobRef.name}')"
replacement_pod_count="$("${kubectl_bin}" -n "${namespace}" get pods \
  --selector job-name="${resume_job}" -o json | \
  jq -r --arg uid "${resume_pod_uid}" '[.items[] | select(.metadata.uid != $uid)] | length')"
if [[ "${resumed_job}" != "${resume_job}" || "${replacement_pod_count}" -lt 1 ]]; then
  echo "interrupted import did not retain its deterministic Job identity and replace the Pod" >&2
  exit 1
fi

resumed_state="$(curl --fail --silent --show-error http://127.0.0.1:18083/state)"
resume_attempts="$(jq -r --arg target "${resume_target}" \
  '.transfers[$target].attempts // 0' <<<"${resumed_state}")"
resume_ranges="$(jq -r --arg target "${resume_target}" \
  '.transfers[$target].ranges // 0' <<<"${resumed_state}")"
resume_pauses="$(jq -r --arg target "${resume_target}" \
  '.transfers[$target].pauses // 0' <<<"${resumed_state}")"
resume_completions="$(jq -r --arg target "${resume_target}" \
  '.transfers[$target].completions // 0' <<<"${resumed_state}")"
resume_downloads="$(jq -r --arg target "${resume_target}" \
  '.downloads[$target] // 0' <<<"${resumed_state}")"
if [[ "${resume_attempts}" != "2" || "${resume_ranges}" != "1" || \
  "${resume_pauses}" != "1" || "${resume_completions}" != "1" || \
  "${resume_downloads}" != "1" ]]; then
  echo "interrupted import did not perform one full attempt and one successful Range resume: ${resumed_state}" >&2
  exit 1
fi

completed_job_uid="$("${kubectl_bin}" -n "${namespace}" get job "${resume_job}" \
  -o jsonpath='{.metadata.uid}')"
state_before_job_recovery="$(jq -c --arg target "${resume_target}" \
  '{download: (.downloads[$target] // 0), transfer: (.transfers[$target] // {})}' \
  <<<"${resumed_state}")"
"${kubectl_bin}" -n "${namespace}" delete job "${resume_job}" --wait --timeout=60s
recreated_job_uid=""
for _ in $(seq 1 120); do
  recreated_job_uid="$("${kubectl_bin}" -n "${namespace}" get job "${resume_job}" \
    -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  if [[ -n "${recreated_job_uid}" && "${recreated_job_uid}" != "${completed_job_uid}" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "${recreated_job_uid}" || "${recreated_job_uid}" == "${completed_job_uid}" ]]; then
  echo "controller did not recreate the missing deterministic completed Job" >&2
  exit 1
fi
"${kubectl_bin}" -n "${namespace}" wait --for=condition=complete \
  "job/${resume_job}" --timeout=2m
for _ in $(seq 1 60); do
  recovered_job_uid="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-resume \
    -o jsonpath='{.status.jobRef.uid}')"
  recovered_ready="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-resume \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  if [[ "${recovered_job_uid}" == "${recreated_job_uid}" && "${recovered_ready}" == "True" ]]; then
    break
  fi
  sleep 1
done
if [[ "${recovered_job_uid}" != "${recreated_job_uid}" || "${recovered_ready}" != "True" ]]; then
  echo "artifact did not report the recreated recovery Job as Ready" >&2
  exit 1
fi
state_after_job_recovery="$(curl --fail --silent --show-error http://127.0.0.1:18083/state | \
  jq -c --arg target "${resume_target}" \
    '{download: (.downloads[$target] // 0), transfer: (.transfers[$target] // {})}')"
if [[ "${state_after_job_recovery}" != "${state_before_job_recovery}" ]]; then
  echo "recreating the retained completed Job contacted the Hub instead of validating cache state" >&2
  echo "before=${state_before_job_recovery} after=${state_after_job_recovery}" >&2
  exit 1
fi

"${kubectl_bin}" apply -f "${repo_root}/test/kind/m1-artifact-copy.yaml"
wait_for_condition modelartifact kind-copy Ready 5m
copy_claim="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-copy \
  -o jsonpath='{.status.location.claimName}')"
if [[ "${copy_claim}" != "${managed_claim}" ]]; then
  echo "PVC Copy artifact did not publish into the managed cache" >&2
  exit 1
fi

"${kubectl_bin}" apply -f "${repo_root}/test/kind/m1-artifact-direct.yaml"
wait_for_condition modelartifact kind-direct Ready 5m
direct_scope="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-direct \
  -o jsonpath='{.status.location.mountScope}')"
direct_status="$("${kubectl_bin}" -n "${namespace}" get modelartifact kind-direct -o json)"
if [[ "${direct_scope}" != "SingleNode" ]] || ! grep -Fq "${worker_node}" <<<"${direct_status}"; then
  echo "PVC Direct artifact did not report RWO node placement" >&2
  exit 1
fi

"${kubectl_bin}" apply -f "${repo_root}/test/kind/m1-artifact-failures.yaml"
wait_for_condition modelartifact kind-checksum-failure ChecksumMismatch 5m
wait_for_condition modelartifact kind-unauthorized SourceUnavailable 5m
wait_for_condition modelartifact kind-missing-shard MissingShard 5m
for failed_artifact in kind-checksum-failure kind-unauthorized kind-missing-shard; do
  if [[ "$("${kubectl_bin}" -n "${namespace}" get modelartifact "${failed_artifact}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == "True" ]]; then
    echo "failure artifact ${failed_artifact} incorrectly became Ready" >&2
    exit 1
  fi
done

"${kubectl_bin}" -n "${namespace}" port-forward service/kama 18084:8443 \
  >"${tmp_dir}/manager-metrics-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18084/metrics
metrics_payload="$(curl --fail --silent --show-error http://127.0.0.1:18084/metrics)"
metric_sample_has_labels() {
  local metric_name=$1
  shift
  local sample
  local expected_label
  local matched
  while IFS= read -r sample; do
    if [[ "${sample}" != "${metric_name}{"* ]]; then
      continue
    fi
    matched=1
    for expected_label in "$@"; do
      if [[ "${sample}" != *"${expected_label}"* ]]; then
        matched=0
        break
      fi
    done
    if [[ ${matched} -eq 1 ]]; then
      return 0
    fi
  done <<<"${metrics_payload}"
  return 1
}

if ! metric_sample_has_labels kama_model_cache_ready \
  'cache="kind-cache"' 'namespace="kama-system"'; then
  echo "manager metrics are missing the ready cache gauge with bounded object labels" >&2
  exit 1
fi
cache_ready_series="$(grep -c '^kama_model_cache_ready{' <<<"${metrics_payload}" || true)"
if [[ "${cache_ready_series}" != "1" ]]; then
  echo "cache-ready metric exposed ${cache_ready_series} series, want exactly one" >&2
  exit 1
fi
if ! metric_sample_has_labels kama_model_artifact_operations_total \
  'source="hugging_face"' 'result="success"' 'reason=""'; then
  echo "manager metrics are missing the bounded successful Hub operation labels" >&2
  exit 1
fi
if ! metric_sample_has_labels kama_model_artifact_operations_total \
  'source="hugging_face"' 'result="failure"' 'reason="ChecksumMismatch"'; then
  echo "manager metrics are missing the bounded checksum-failure labels" >&2
  exit 1
fi
if ! metric_sample_has_labels kama_model_artifact_validation_duration_seconds_count \
  'source="hugging_face"' 'result="success"'; then
  echo "manager metrics are missing successful Hub validation timing" >&2
  exit 1
fi

"${kubectl_bin}" -n "${namespace}" port-forward service/kama-fake-llama 18080:8080 \
  >"${tmp_dir}/fake-llama-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18080/health
completion="$(curl --fail --silent --show-error \
  --request POST \
  --header 'Content-Type: application/json' \
  --data '{"model":"synthetic","messages":[{"role":"user","content":"compatibility smoke"}]}' \
  http://127.0.0.1:18080/v1/chat/completions)"
if ! grep -Fq '"choices"' <<<"${completion}"; then
  echo "fake llama-server completion response did not contain choices" >&2
  exit 1
fi

"${helm_bin}" uninstall kama --namespace "${namespace}" --wait
if ! "${kubectl_bin}" -n "${namespace}" get pvc "${managed_claim}" >/dev/null; then
  echo "Helm uninstall deleted retained managed cache claim ${managed_claim}" >&2
  exit 1
fi
if ! "${kubectl_bin}" -n "${namespace}" get pvc kama-manual-models >/dev/null; then
  echo "Helm uninstall deleted adopted manual source claim" >&2
  exit 1
fi
remaining=""
cluster_remaining=""
for _ in $(seq 1 30); do
  remaining="$("${kubectl_bin}" -n "${namespace}" get \
    deployment,service,serviceaccount,secret,role,rolebinding,pod \
    --selector app.kubernetes.io/instance=kama \
    --output name)"
  cluster_remaining="$("${kubectl_bin}" get \
    clusterrole,clusterrolebinding,mutatingwebhookconfiguration,validatingwebhookconfiguration \
    --selector app.kubernetes.io/instance=kama,kama.tannerburns.github.io/release-namespace=${namespace} \
    --output name)"
  if [[ -z "${remaining}" && -z "${cluster_remaining}" ]]; then
    break
  fi
  sleep 2
done
if [[ -n "${remaining}" || -n "${cluster_remaining}" ]]; then
  echo "Helm uninstall left chart-owned resources:" >&2
  echo "${remaining}" >&2
  echo "${cluster_remaining}" >&2
  exit 1
fi

echo "Kind artifact-admission/KEDA compatibility smoke passed for ${node_image}"
