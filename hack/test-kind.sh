#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(tr -d '\r\n' < "${repo_root}/VERSION")"
kind_bin="${KIND:-${repo_root}/bin/kind}"
kubectl_bin="${KUBECTL:-kubectl}"
helm_bin="${HELM:-${repo_root}/bin/helm}"
keda_version="${KEDA_VERSION:-2.20.0}"
cluster_name="${KIND_CLUSTER:-kama-m0}"
node_image="${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must be a digest-pinned Kind node image}"
namespace="kama-system"
manager_image="${IMG:-local/kama-manager:${version}}"
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
for tool in "${kind_bin}" "${kubectl_bin}" "${helm_bin}" docker curl; do
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
  --file "${repo_root}/Dockerfile.test-fixtures" \
  --build-arg "VERSION=${version}" \
  --build-arg "VCS_REF=${revision}" \
  --build-arg "CREATED=${created}" \
  --tag "${fixtures_image}" \
  "${repo_root}"

"${kind_bin}" create cluster --name "${cluster_name}" --image "${node_image}" --wait 5m
cluster_created=1
"${kind_bin}" load docker-image --name "${cluster_name}" "${manager_image}" "${fixtures_image}"

"${kubectl_bin}" create namespace "${namespace}"
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
  --set metrics.enabled=true \
  --wait \
  --timeout 2m
"${kubectl_bin}" -n "${namespace}" rollout status deployment/kama --timeout=2m
"${helm_bin}" test kama --namespace "${namespace}" --timeout 2m

"${kubectl_bin}" -n "${namespace}" port-forward service/kama-fake-llama 18080:8080 \
  >"${tmp_dir}/fake-llama-port-forward.log" 2>&1 &
port_forward_pids+=("$!")
wait_for_http http://127.0.0.1:18080/health
completion="$(curl --fail --silent --show-error \
  --request POST \
  --header 'Content-Type: application/json' \
  --data '{"model":"synthetic","messages":[{"role":"user","content":"M0 smoke"}]}' \
  http://127.0.0.1:18080/v1/chat/completions)"
if ! grep -Fq '"choices"' <<<"${completion}"; then
  echo "fake llama-server completion response did not contain choices" >&2
  exit 1
fi

"${helm_bin}" uninstall kama --namespace "${namespace}" --wait
remaining=""
cluster_remaining=""
for _ in $(seq 1 30); do
  remaining="$("${kubectl_bin}" -n "${namespace}" get \
    deployment,service,serviceaccount,role,rolebinding,pod \
    --selector app.kubernetes.io/instance=kama \
    --output name)"
  cluster_remaining="$("${kubectl_bin}" get clusterrole,clusterrolebinding \
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

echo "Kind/KEDA compatibility smoke passed for ${node_image}"
