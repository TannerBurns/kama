#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "${test_root}"' EXIT

fixture_root="${test_root}/repo"
mock_bin="${test_root}/bin"
mkdir -p \
  "${fixture_root}/hack" \
  "${fixture_root}/test/e2e/serving" \
  "${mock_bin}"
cp "${repo_root}/VERSION" "${fixture_root}/VERSION"
cp "${repo_root}/hack/versions.mk" "${fixture_root}/hack/versions.mk"
cp "${repo_root}/hack/test-e2e-serving-nvidia.sh" \
  "${fixture_root}/hack/test-e2e-serving-nvidia.sh"
if grep -Eq '^[[:space:]]*return[[:space:]]*$' \
  "${fixture_root}/hack/test-e2e-serving-nvidia.sh"; then
  echo "NVIDIA harness functions must use explicit return statuses for Bash 5.2 EXIT-trap safety" >&2
  exit 1
fi
cp "${repo_root}/test/e2e/serving/nvidia-storage.yaml.tmpl" \
  "${fixture_root}/test/e2e/serving/nvidia-storage.yaml.tmpl"
cp "${repo_root}/test/e2e/serving/nvidia-existing-cache.yaml.tmpl" \
  "${fixture_root}/test/e2e/serving/nvidia-existing-cache.yaml.tmpl"
cp "${repo_root}/test/e2e/serving/nvidia-deployment.yaml" \
  "${fixture_root}/test/e2e/serving/nvidia-deployment.yaml"
install -m 0755 /dev/stdin "${fixture_root}/hack/helm-package.sh" <<'HELM_PACKAGE'
#!/usr/bin/env bash

set -euo pipefail
printf 'invoked\n' >>"${MOCK_PACKAGE_LOG}"
if [[ "${MOCK_SCENARIO}" == suite-owned-* &&
  "${MOCK_SCENARIO}" != "suite-owned-existing-controller" ]]; then
  exit 0
fi
exit 95
HELM_PACKAGE

mock_commit="dddddddddddddddddddddddddddddddddddddddd"
mock_parent_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
mock_child_digest="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
mock_config_digest="sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
mock_unrelated_digest="sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
manager_image="ghcr.io/tannerburns/kama-manager@${mock_parent_digest}"
importer_image="ghcr.io/tannerburns/kama-importer@${mock_parent_digest}"
fixtures_image="ghcr.io/tannerburns/kama-test-fixtures@${mock_parent_digest}"
runtime_cpu_image="ghcr.io/tannerburns/kama-runtime-cpu@${mock_parent_digest}"
runtime_cuda_image="ghcr.io/tannerburns/kama-runtime-cuda@${mock_parent_digest}"
llama_commit="$(awk '$1 == "LLAMA_CPP_COMMIT" {print $3}' "${repo_root}/hack/versions.mk")"
llama_build_number="$(awk '$1 == "LLAMA_CPP_BUILD_NUMBER" {print $3}' "${repo_root}/hack/versions.mk")"
llama_source_sha256="$(awk '$1 == "LLAMA_CPP_SOURCE_SHA256" {print $3}' "${repo_root}/hack/versions.mk")"
runtime_class="vendor-gpu.example"

install -m 0755 /dev/stdin "${mock_bin}/mock-tool" <<'MOCK_TOOL'
#!/usr/bin/env bash

set -euo pipefail

tool="${0##*/}"
{
  printf '%s' "${tool}"
  for argument in "$@"; do
    printf '\t%s' "${argument}"
  done
  printf '\n'
} >>"${MOCK_CALL_LOG}"

case "${tool}" in
  git)
    arguments=" $* "
    if [[ "${arguments}" == *" rev-parse HEAD "* ]] ||
      [[ "${arguments}" == *" rev-parse --verify refs/remotes/origin/main^{commit} "* ]]; then
      printf '%s\n' "${MOCK_COMMIT}"
    elif [[ "${arguments}" == *" status --porcelain=v1 --untracked-files=all "* ]] ||
      [[ "${arguments}" == *" merge-base --is-ancestor "* ]]; then
      :
    else
      echo "unexpected git invocation: $*" >&2
      exit 90
    fi
    ;;
  cosign)
    printf '[]\n'
    ;;
  helm)
    if [[ "${1:-}" == "status" ]]; then
      # The release-status probe is read-only; status 1 means it is absent.
      exit 1
    elif [[ "${1:-}" == "show" && "${2:-}" == "crds" ]]; then
      printf '%s\n' 'apiVersion: v1' 'kind: List' 'items: []'
    elif [[ "${1:-}" == "upgrade" && "${2:-}" == "--install" ]]; then
      : >"${MOCK_STATE_DIR}/helm-release-present"
      if [[ "${MOCK_SCENARIO}" == "suite-owned-partial-install" ]]; then
        exit 44
      fi
    elif [[ "${1:-}" == "list" ]]; then
      if [[ "${MOCK_SCENARIO}" == "suite-owned-list-failure" ]]; then
        exit 45
      fi
      if [[ -f "${MOCK_STATE_DIR}/helm-release-present" ]]; then
        jq -cn --arg name "${MOCK_SUITE_RELEASE}" '[{name: $name}]'
      else
        printf '[]\n'
      fi
    elif [[ "${1:-}" == "uninstall" ]]; then
      if [[ "${MOCK_SCENARIO}" == "suite-owned-uninstall-failure" ]]; then
        exit 46
      fi
      rm -f -- "${MOCK_STATE_DIR}/helm-release-present"
    else
      echo "the NVIDIA harness invoked an unexpected Helm operation" >&2
      exit 91
    fi
    ;;
  curl)
    request=""
    payload=""
    url=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --request)
          request=$2
          shift 2
          ;;
        --data-binary)
          payload=$2
          shift 2
          ;;
        http://* | https://*)
          url=$1
          shift
          ;;
        *)
          shift
          ;;
      esac
    done
    if [[ "${request}" == "DELETE" ]]; then
      pending="$(<"${MOCK_EVIDENCE_DIR}/outcome.txt")"
      qualifying="$(jq -r '.qualifying' "${MOCK_EVIDENCE_DIR}/qualification.json")"
      jq -cn \
        --arg url "${url}" \
        --arg pending "${pending}" \
        --argjson qualifying "${qualifying}" \
        --argjson body "${payload}" \
        '{url: $url, pending: $pending, qualifying: $qualifying, body: $body}' \
        >>"${MOCK_DELETE_LOG}"
      if [[ "${url}" == */api/v1/namespaces/"${MOCK_TEST_NAMESPACE}" ]]; then
        : >"${MOCK_STATE_DIR}/namespace-deleted"
      fi
    elif [[ "${url}" == "https://ghcr.io/token" ]]; then
      printf '{"token":"mock-token"}\n'
    elif [[ "${url}" == *"/manifests/${MOCK_PARENT_DIGEST}" ]]; then
      jq -cn --arg digest "${MOCK_CHILD_DIGEST}" '{
        schemaVersion: 2,
        manifests: [{
          digest: $digest,
          platform: {os: "linux", architecture: "amd64"}
        }]
      }'
    elif [[ "${url}" == *"/manifests/${MOCK_CHILD_DIGEST}" ]]; then
      jq -cn --arg digest "${MOCK_CONFIG_DIGEST}" \
        '{schemaVersion: 2, config: {digest: $digest}}'
    elif [[ "${url}" == *"/blobs/${MOCK_CONFIG_DIGEST}" ]]; then
      jq -cn \
        --arg url "${url}" \
        --arg revision "${MOCK_COMMIT}" \
        --arg llamaCommit "${MOCK_LLAMA_COMMIT}" \
        --arg llamaBuildNumber "${MOCK_LLAMA_BUILD_NUMBER}" \
        --arg llamaSourceSHA256 "${MOCK_LLAMA_SOURCE_SHA256}" '
        {
          "org.opencontainers.image.source": "https://github.com/TannerBurns/kama",
          "org.opencontainers.image.revision": $revision
        } as $base |
        (if ($url | contains("kama-runtime-cpu")) or
            ($url | contains("kama-runtime-cuda")) then
          $base + {
            "io.kama.llama.cpp.commit": $llamaCommit,
            "io.kama.llama.cpp.build-number": $llamaBuildNumber,
            "io.kama.llama.cpp.source-sha256": $llamaSourceSHA256
          }
        else $base end) as $labels |
        {config: {Labels:
          (if ($url | contains("kama-runtime-cuda")) then
            $labels + {"io.kama.cuda.version": "12.4.1"}
          else $labels end)
        }}'
    else
      echo "unexpected curl URL: ${url:-unset}" >&2
      exit 92
    fi
    ;;
  kubectl)
    arguments=" $* "
    if [[ "${1:-}" == "proxy" ]]; then
      printf '%s\n' "$$" >"${MOCK_STATE_DIR}/proxy.pid"
      if [[ "${MOCK_SCENARIO}" == "proxy-start-timeout" ]]; then
        exec tail -f /dev/null
      fi
      printf 'Starting to serve on 127.0.0.1:43210\n'
      exec tail -f /dev/null
    elif [[ "${arguments}" == *" version -o json "* ]]; then
      printf '%s\n' '{
        "clientVersion":{"major":"1","minor":"36"},
        "serverVersion":{"major":"1","minor":"36"}
      }'
    elif [[ "${arguments}" == *" get namespace ${MOCK_TEST_NAMESPACE} "* ]]; then
      if [[ "${MOCK_SCENARIO}" == suite-owned-* ]]; then
        if [[ ! -f "${MOCK_STATE_DIR}/namespace-created" ||
          -f "${MOCK_STATE_DIR}/namespace-deleted" ]]; then
          if [[ "${arguments}" == *" --ignore-not-found "* ]]; then
            exit 0
          fi
          exit 1
        fi
        namespace_reads=0
        if [[ -f "${MOCK_STATE_DIR}/namespace-reads" ]]; then
          namespace_reads="$(<"${MOCK_STATE_DIR}/namespace-reads")"
        fi
        namespace_reads=$((namespace_reads + 1))
        printf '%s\n' "${namespace_reads}" >"${MOCK_STATE_DIR}/namespace-reads"
        namespace_uid="namespace-uid-original"
        namespace_run_id="$(<"${MOCK_STATE_DIR}/namespace-run-id")"
        if [[ "${MOCK_SCENARIO}" == "suite-owned-namespace-replacement" &&
          ${namespace_reads} -ge 2 ]]; then
          namespace_uid="namespace-uid-replacement"
          namespace_run_id="replacement-run"
        fi
        printf '%s\n' "${namespace_uid}" >"${MOCK_STATE_DIR}/namespace-last-uid"
        if [[ "${arguments}" == *" -o name "* ]]; then
          printf 'namespace/%s\n' "${MOCK_TEST_NAMESPACE}"
        else
          jq -cn \
            --arg name "${MOCK_TEST_NAMESPACE}" \
            --arg uid "${namespace_uid}" \
            --arg run "${namespace_run_id}" '{metadata: {
              name: $name,
              uid: $uid,
              labels: {"kama.tannerburns.github.io/e2e-run": $run}
            }}'
        fi
      else
        printf '{"metadata":{"name":"%s"}}\n' "${MOCK_TEST_NAMESPACE}"
      fi
    elif [[ -n "${MOCK_EXISTING_CLAIM}" &&
      "${arguments}" == *" -n ${MOCK_TEST_NAMESPACE} get pvc ${MOCK_EXISTING_CLAIM} -o json "* ]]; then
      owner_references='[]'
      claim_storage_class="${MOCK_STORAGE_CLASS}"
      if [[ "${MOCK_SCENARIO}" == "adopted-unsafe-owner" ]]; then
        owner_references='[{"apiVersion":"v1","kind":"Pod","name":"foreign-owner","uid":"owner-uid"}]'
      elif [[ "${MOCK_SCENARIO}" == "adopted-wrong-storage" ]]; then
        claim_storage_class="other-storage"
      fi
      jq -cn \
        --arg namespace "${MOCK_TEST_NAMESPACE}" \
        --arg name "${MOCK_EXISTING_CLAIM}" \
        --arg storageClass "${claim_storage_class}" \
        --argjson ownerReferences "${owner_references}" '{
          metadata: {
            namespace: $namespace,
            name: $name,
            uid: "existing-claim-uid",
            ownerReferences: $ownerReferences
          },
          spec: {
            storageClassName: $storageClass,
            accessModes: ["ReadWriteOnce"],
            volumeMode: "Filesystem",
            volumeName: "existing-pv"
          },
          status: {
            phase: "Bound",
            accessModes: ["ReadWriteOnce"],
            capacity: {storage: "2Gi"}
          }
        }'
    elif [[ -n "${MOCK_EXISTING_CLAIM}" &&
      "${arguments}" == *" get pv existing-pv -o json "* ]]; then
      claim_uid="existing-claim-uid"
      if [[ "${MOCK_SCENARIO}" == "adopted-pv-mismatch" ]]; then
        claim_uid="replacement-claim-uid"
      fi
      jq -cn \
        --arg namespace "${MOCK_TEST_NAMESPACE}" \
        --arg claim "${MOCK_EXISTING_CLAIM}" \
        --arg claimUID "${claim_uid}" \
        --arg storageClass "${MOCK_STORAGE_CLASS}" '{
          metadata: {name: "existing-pv", uid: "existing-pv-uid"},
          spec: {
            storageClassName: $storageClass,
            accessModes: ["ReadWriteOnce"],
            volumeMode: "Filesystem",
            claimRef: {
              apiVersion: "v1",
              kind: "PersistentVolumeClaim",
              namespace: $namespace,
              name: $claim,
              uid: $claimUID
            }
          },
          status: {phase: "Bound"}
        }'
    elif [[ "${arguments}" == *" get modelcaches,modelartifacts -o json "* ]]; then
      if [[ "${MOCK_SCENARIO}" == "adopted-existing-reference" ]]; then
        jq -cn --arg claim "${MOCK_EXISTING_CLAIM}" '{items: [{
          apiVersion: "kama.tannerburns.github.io/v1alpha1",
          kind: "ModelCache",
          metadata: {name: "foreign-cache"},
          spec: {storage: {existingClaim: {name: $claim}}}
        }]}'
      else
        printf '{"items":[]}\n'
      fi
    elif [[ "${arguments}" == *" get pods,jobs.batch -o json "* ]]; then
      if [[ "${MOCK_SCENARIO}" == "adopted-active-pod" ]]; then
        jq -cn --arg claim "${MOCK_EXISTING_CLAIM}" '{items: [{
          apiVersion: "v1",
          kind: "Pod",
          metadata: {name: "foreign-pod"},
          spec: {volumes: [{name: "cache", persistentVolumeClaim: {claimName: $claim}}]},
          status: {phase: "Running"}
        }]}'
      elif [[ "${MOCK_SCENARIO}" == "adopted-active-job" ]]; then
        jq -cn --arg claim "${MOCK_EXISTING_CLAIM}" '{items: [{
          apiVersion: "batch/v1",
          kind: "Job",
          metadata: {name: "foreign-job"},
          spec: {template: {spec: {volumes: [{
            name: "cache", persistentVolumeClaim: {claimName: $claim}
          }]}}},
          status: {active: 1}
        }]}'
      else
        printf '{"items":[]}\n'
      fi
    elif [[ "${arguments}" == *" get modeldeployments,modelartifacts,modelcaches,jobs -o json "* ]]; then
      printf '{"items":[]}\n'
    elif [[ "${arguments}" == *" get deployment,service,pod,pvc "* ]] ||
      [[ "${arguments}" == *" get pvc -l kama.tannerburns.github.io/model-cache="* ]]; then
      :
    elif [[ "${arguments}" == *" get storageclass ${MOCK_STORAGE_CLASS} "* ]]; then
      printf '{"metadata":{"name":"%s"}}\n' "${MOCK_STORAGE_CLASS}"
    elif [[ "${arguments}" == *" get runtimeclass ${MOCK_RUNTIME_CLASS} -o json "* ]]; then
      printf '{"metadata":{"name":"%s"}}\n' "${MOCK_RUNTIME_CLASS}"
    elif [[ "${arguments}" == *" get nodes -o json "* ]]; then
      jq -cn '{items: [{
        metadata: {
          name: "gpu-node",
          labels: {"nvidia.com/gpu.product": "Mock GPU"}
        },
        status: {
          allocatable: {"nvidia.com/gpu": "3"},
          nodeInfo: {
            architecture: "amd64",
            containerRuntimeVersion: "containerd://mock",
            kernelVersion: "mock",
            kubeletVersion: "v1.36.0",
            operatingSystem: "linux",
            osImage: "mock"
          }
        }
      }]}'
    elif [[ "${arguments}" == *" get deployments.apps --all-namespaces -o json "* ]]; then
      if [[ "${MOCK_SCENARIO}" == suite-owned-* &&
        "${MOCK_SCENARIO}" != "suite-owned-existing-controller" &&
        ! -f "${MOCK_STATE_DIR}/helm-release-present" ]]; then
        printf '{"items":[]}\n'
      else
        jq -cn \
        --arg namespace "${MOCK_CONTROLLER_NAMESPACE}" \
        --arg name "${MOCK_CONTROLLER_DEPLOYMENT}" \
        --arg manager "${MOCK_MANAGER_IMAGE}" \
        --arg importer "${MOCK_IMPORTER_IMAGE}" \
        --arg runtimeCPU "${MOCK_RUNTIME_CPU_IMAGE}" \
        --arg runtimeCUDA "${MOCK_RUNTIME_CUDA_IMAGE}" \
        --arg runtimeClass "${MOCK_OBSERVED_RUNTIME_CLASS}" \
        --arg llamaCommit "${MOCK_LLAMA_COMMIT}" '{items: [{
          metadata: {namespace: $namespace, name: $name, uid: "controller-deployment-uid", generation: 1},
          spec: {replicas: 1, template: {spec: {containers: [{
            name: "manager",
            image: $manager,
            args: ([
              "--importer-image=" + $importer,
              "--runtime-cpu-image=" + $runtimeCPU,
              "--runtime-cuda-image=" + $runtimeCUDA,
              "--llama-commit=" + $llamaCommit
            ] + (if $runtimeClass == "" then [] else
              ["--runtime-cuda-runtime-class=" + $runtimeClass] end))
          }]}}},
          status: {observedGeneration: 1, readyReplicas: 1, availableReplicas: 1}
        }]}'
      fi
    elif [[ "${arguments}" == *" -n ${MOCK_CONTROLLER_NAMESPACE} rollout status deployment/${MOCK_CONTROLLER_DEPLOYMENT} "* ]]; then
      :
    elif [[ "${arguments}" == *" -n ${MOCK_CONTROLLER_NAMESPACE} get deployment ${MOCK_CONTROLLER_DEPLOYMENT} -o json "* ]]; then
      controller_reads=0
      if [[ -f "${MOCK_STATE_DIR}/controller-reads" ]]; then
        controller_reads="$(<"${MOCK_STATE_DIR}/controller-reads")"
      fi
      controller_reads=$((controller_reads + 1))
      printf '%s\n' "${controller_reads}" >"${MOCK_STATE_DIR}/controller-reads"
      controller_deployment_uid="controller-deployment-uid"
      if [[ "${MOCK_SCENARIO}" == "controller-identity-change" && ${controller_reads} -ge 2 ]]; then
        controller_deployment_uid="controller-deployment-replacement"
      fi
      jq -cn \
        --arg namespace "${MOCK_CONTROLLER_NAMESPACE}" \
        --arg name "${MOCK_CONTROLLER_DEPLOYMENT}" \
        --arg deploymentUID "${controller_deployment_uid}" \
        --arg manager "${MOCK_MANAGER_IMAGE}" \
        --arg importer "${MOCK_IMPORTER_IMAGE}" \
        --arg runtimeCPU "${MOCK_RUNTIME_CPU_IMAGE}" \
        --arg runtimeCUDA "${MOCK_RUNTIME_CUDA_IMAGE}" \
        --arg runtimeClass "${MOCK_OBSERVED_RUNTIME_CLASS}" \
        --arg llamaCommit "${MOCK_LLAMA_COMMIT}" '{
          metadata: {namespace: $namespace, name: $name, uid: $deploymentUID, generation: 1},
          spec: {replicas: 1, template: {spec: {containers: [{
            name: "manager",
            image: $manager,
            args: ([
              "--importer-image=" + $importer,
              "--runtime-cpu-image=" + $runtimeCPU,
              "--runtime-cuda-image=" + $runtimeCUDA,
              "--llama-commit=" + $llamaCommit
            ] + (if $runtimeClass == "" then [] else
              ["--runtime-cuda-runtime-class=" + $runtimeClass] end))
          }]}}},
          status: {observedGeneration: 1, readyReplicas: 1, availableReplicas: 1}
        }'
    elif [[ "${arguments}" == *" get pods --all-namespaces -o json "* ]]; then
      if [[ "${MOCK_SCENARIO}" == suite-owned-* &&
        "${MOCK_SCENARIO}" != "suite-owned-existing-controller" &&
        ! -f "${MOCK_STATE_DIR}/helm-release-present" ]]; then
        printf '{"items":[]}\n'
      else
        manager_image_id_digest="${MOCK_PARENT_DIGEST}"
        if [[ "${MOCK_SCENARIO}" == "owned-default-runtime" ]]; then
          manager_image_id_digest="${MOCK_CHILD_DIGEST}"
        elif [[ "${MOCK_SCENARIO}" == "unrelated-manager-image-digest" ]]; then
          manager_image_id_digest="${MOCK_UNRELATED_DIGEST}"
        fi
        jq -cn \
          --arg namespace "${MOCK_CONTROLLER_NAMESPACE}" \
          --arg manager "${MOCK_MANAGER_IMAGE}" \
          --arg imageID "ghcr.io/tannerburns/kama-manager@${manager_image_id_digest}" '{items: [{
          metadata: {namespace: $namespace, name: "kama-manager-mock", uid: "controller-pod-uid"},
          spec: {containers: [{name: "manager", image: $manager}]},
          status: {
            phase: "Running",
            conditions: [{type: "Ready", status: "True"}],
            containerStatuses: [{
              name: "manager",
              ready: true,
              imageID: $imageID
            }]
          }
        }]}'
      fi
    elif [[ "${arguments}" == *" create -f - "* ]]; then
      namespace_manifest="${MOCK_STATE_DIR}/created-namespace.json"
      cat >"${namespace_manifest}"
      jq -er '.metadata.labels["kama.tannerburns.github.io/e2e-run"]' \
        "${namespace_manifest}" >"${MOCK_STATE_DIR}/namespace-run-id"
      : >"${MOCK_STATE_DIR}/namespace-created"
    elif [[ "${arguments}" == *" apply --server-side "* ]]; then
      cat >/dev/null
    elif [[ "${arguments}" == *" create -f "* ]]; then
      manifest="${@: -1}"
      cp "${manifest}" "${MOCK_STATE_DIR}/created-storage.yaml"
      awk '$1 == "kama.tannerburns.github.io/e2e-run:" {print $2; exit}' \
        "${manifest}" >"${MOCK_STATE_DIR}/run-id"
    elif [[ "${arguments}" == *" get modelcache e2e-serving-nvidia-cache "* ]]; then
      if [[ "${arguments}" == *" --ignore-not-found "* &&
        ( "${MOCK_SCENARIO}" == "runtime-class-mismatch" ||
          ! -f "${MOCK_STATE_DIR}/run-id" ) ]]; then
        exit 0
      fi
      run_id="$(<"${MOCK_STATE_DIR}/run-id")"
      jq -cn --arg run "${run_id}" '{metadata: {
        name: "e2e-serving-nvidia-cache",
        uid: "cache-uid-original",
        labels: {"kama.tannerburns.github.io/e2e-run": $run}
      }}'
    elif [[ "${arguments}" == *" get modelartifact e2e-serving-nvidia-model "* ]]; then
      if [[ "${arguments}" == *" --ignore-not-found "* &&
        ( "${MOCK_SCENARIO}" == "runtime-class-mismatch" ||
          ! -f "${MOCK_STATE_DIR}/run-id" ) ]]; then
        exit 0
      fi
      run_id="$(<"${MOCK_STATE_DIR}/run-id")"
      uid="artifact-uid-original"
      if [[ "${MOCK_SCENARIO}" == "replacement" &&
        "${arguments}" == *" --ignore-not-found "* ]]; then
        uid="artifact-uid-replacement"
      fi
      jq -cn --arg run "${run_id}" --arg uid "${uid}" '{metadata: {
        name: "e2e-serving-nvidia-model",
        uid: $uid,
        labels: {"kama.tannerburns.github.io/e2e-run": $run}
      }}'
    elif [[ "${arguments}" == *" get job e2e-serving-nvidia-client "* ]] ||
      [[ "${arguments}" == *" get modeldeployment e2e-serving-nvidia "* ]]; then
      if [[ "${arguments}" == *" --ignore-not-found "* ]]; then
        :
      else
        exit 1
      fi
    elif [[ "${arguments}" == *" get modelcache,modelartifact,modeldeployment,deploy,svc,job,pod -o wide "* ]]; then
      printf 'mock resources\n'
    elif [[ "${arguments}" == *" get deployments.apps,replicasets.apps,services,pods,persistentvolumeclaims,jobs.batch,configmaps,leases.coordination.k8s.io -l "* ]] ||
      [[ "${arguments}" == *" get endpointslices.discovery.k8s.io -l kubernetes.io/service-name="* ]]; then
      if [[ "${MOCK_SCENARIO}" == "suite-owned-residual" ]]; then
        printf 'deployment.apps/residual-owned-resource\n'
      fi
    elif [[ "${arguments}" == *" get pods -l kama.tannerburns.github.io/model-deployment=e2e-serving-nvidia -o json "* ]]; then
      printf '{"apiVersion":"v1","kind":"PodList","items":[]}\n'
    elif [[ "${arguments}" == *" get events --sort-by=.lastTimestamp "* ]]; then
      printf 'mock events\n'
    elif [[ "${arguments}" == *" logs deployment/${MOCK_CONTROLLER_DEPLOYMENT} "* ]]; then
      printf 'mock manager log\n'
    elif [[ "${arguments}" == *" wait --for=condition=Ready=True modelcache/e2e-serving-nvidia-cache "* ]]; then
      exit 23
    elif [[ "${arguments}" == *" wait "* ]]; then
      :
    else
      echo "unexpected kubectl invocation: $*" >&2
      exit 93
    fi
    ;;
  sleep)
    :
    ;;
  *)
    echo "unexpected mock tool: ${tool}" >&2
    exit 94
    ;;
esac
MOCK_TOOL

for tool in git kubectl curl cosign helm sleep; do
  ln -s mock-tool "${mock_bin}/${tool}"
done

assert_no_kubernetes_ownership_mutation() {
  local call_log=$1
  if awk -F '\t' '
    $1 == "kubectl" {
      for (i = 2; i <= NF; i++) {
        if ($i == "apply" ||
            ($i == "create" && $(i + 1) == "namespace") ||
            ($i == "delete" && $(i + 1) == "namespace") ||
            ($i == "delete" && $(i + 1) ~ /^crd($|\/)/)) {
          found = 1
        }
      }
    }
    END {exit(found ? 0 : 1)}
  ' "${call_log}"; then
    echo "NVIDIA harness attempted a forbidden CRD/Namespace ownership mutation" >&2
    return 1
  fi
}

assert_preinstalled_controller_behavior() {
  local call_log=$1
  local expected_runtime_class=$2
  if grep -q '^helm' "${call_log}"; then
    echo "preinstalled-controller mode invoked Helm" >&2
    return 1
  fi
  assert_no_kubernetes_ownership_mutation "${call_log}"
  if ! grep -Fq $'kubectl\twait\t--for=condition=Established' "${call_log}" ||
    ! grep -Fq $'kubectl\t-n\tkama-system\trollout\tstatus\tdeployment/kama' "${call_log}"; then
    echo "preinstalled-controller mode did not verify the existing CRDs and controller" >&2
    return 1
  fi
  if [[ -n "${expected_runtime_class}" ]]; then
    if ! grep -Fq $'kubectl\tget\truntimeclass\t'"${expected_runtime_class}"$'\t-o\tjson' \
      "${call_log}"; then
      echo "preinstalled-controller mode did not verify the configured RuntimeClass" >&2
      return 1
    fi
  elif grep -Fq $'kubectl\tget\truntimeclass\t' "${call_log}"; then
    echo "preinstalled-controller mode looked up an unconfigured RuntimeClass" >&2
    return 1
  fi
}

run_scenario() {
  local scenario=$1
  local expected_status=$2
  local state_dir="${test_root}/${scenario}"
  local status=0
  local preinstalled_controller=1
  local use_existing_namespace=1
  local controller_namespace="kama-system"
  local controller_deployment="kama"
  local suite_release="kama-e2e-serving-nvidia"
  local namespace_deleted=0
  local proxy_pid=""
  local configured_runtime_class="${runtime_class}"
  local observed_runtime_class="${runtime_class}"
  local existing_claim=""
  local keep_resources=0
  if [[ "${scenario}" == suite-owned-* ]]; then
    preinstalled_controller=0
    use_existing_namespace=0
    controller_namespace="kama-qualification"
    controller_deployment="${suite_release}"
  fi
  if [[ "${scenario}" == "suite-owned-existing-controller" ]]; then
    :
  elif [[ "${scenario}" == "suite-owned-existing-claim" ]]; then
    existing_claim="existing-cache"
  elif [[ "${scenario}" == "invalid-existing-claim-name" ]]; then
    existing_claim="Invalid_Claim"
  elif [[ "${scenario}" == "invalid-keep-resources" ]]; then
    keep_resources=invalid
  elif [[ "${scenario}" == "runtime-class-mismatch" ]]; then
    observed_runtime_class="different-runtime"
  elif [[ "${scenario}" == "keep-resources" ]]; then
    keep_resources=1
  elif [[ "${scenario}" == "owned-default-runtime" ]]; then
    configured_runtime_class=""
    observed_runtime_class=""
  elif [[ "${scenario}" == adopted* ]]; then
    existing_claim="existing-cache"
  fi
  mkdir -p "${state_dir}"
  : >"${state_dir}/calls.log"
  : >"${state_dir}/deletes.jsonl"
  : >"${state_dir}/kubeconfig"
  : >"${state_dir}/package.log"

  set +e
  PATH="${mock_bin}:${PATH}" \
  KUBECTL="${mock_bin}/kubectl" \
  COSIGN="${mock_bin}/cosign" \
  HELM="${mock_bin}/helm" \
  KUBECONFIG="${state_dir}/kubeconfig" \
  MOCK_CALL_LOG="${state_dir}/calls.log" \
  MOCK_DELETE_LOG="${state_dir}/deletes.jsonl" \
  MOCK_PACKAGE_LOG="${state_dir}/package.log" \
  MOCK_STATE_DIR="${state_dir}" \
  MOCK_SCENARIO="${scenario}" \
  MOCK_EVIDENCE_DIR="${fixture_root}/dist/e2e/serving-nvidia" \
  MOCK_COMMIT="${mock_commit}" \
  MOCK_PARENT_DIGEST="${mock_parent_digest}" \
  MOCK_CHILD_DIGEST="${mock_child_digest}" \
  MOCK_CONFIG_DIGEST="${mock_config_digest}" \
  MOCK_UNRELATED_DIGEST="${mock_unrelated_digest}" \
  MOCK_TEST_NAMESPACE="kama-qualification" \
  MOCK_CONTROLLER_NAMESPACE="${controller_namespace}" \
  MOCK_CONTROLLER_DEPLOYMENT="${controller_deployment}" \
  MOCK_SUITE_RELEASE="${suite_release}" \
  MOCK_STORAGE_CLASS="mock-csi" \
  MOCK_MANAGER_IMAGE="${manager_image}" \
  MOCK_IMPORTER_IMAGE="${importer_image}" \
  MOCK_RUNTIME_CPU_IMAGE="${runtime_cpu_image}" \
  MOCK_RUNTIME_CUDA_IMAGE="${runtime_cuda_image}" \
  MOCK_RUNTIME_CLASS="${configured_runtime_class}" \
  MOCK_OBSERVED_RUNTIME_CLASS="${observed_runtime_class}" \
  MOCK_EXISTING_CLAIM="${existing_claim}" \
  MOCK_LLAMA_COMMIT="${llama_commit}" \
  MOCK_LLAMA_BUILD_NUMBER="${llama_build_number}" \
  MOCK_LLAMA_SOURCE_SHA256="${llama_source_sha256}" \
  E2E_NVIDIA_PREINSTALLED_CONTROLLER="${preinstalled_controller}" \
  E2E_NVIDIA_USE_EXISTING_NAMESPACE="${use_existing_namespace}" \
  E2E_NVIDIA_NAMESPACE="kama-qualification" \
  E2E_NVIDIA_RELEASE="${suite_release}" \
  E2E_NVIDIA_CONTROLLER_NAMESPACE="${controller_namespace}" \
  E2E_NVIDIA_CONTROLLER_DEPLOYMENT="${controller_deployment}" \
  E2E_NVIDIA_MANAGER_IMAGE="${manager_image}" \
  E2E_NVIDIA_IMPORTER_IMAGE="${importer_image}" \
  E2E_NVIDIA_FIXTURES_IMAGE="${fixtures_image}" \
  E2E_NVIDIA_RUNTIME_CPU_IMAGE="${runtime_cpu_image}" \
  E2E_NVIDIA_RUNTIME_CUDA_IMAGE="${runtime_cuda_image}" \
  E2E_NVIDIA_RUNTIME_CLASS="${configured_runtime_class}" \
  E2E_NVIDIA_STORAGE_CLASS="mock-csi" \
  E2E_NVIDIA_EXISTING_CACHE_CLAIM="${existing_claim}" \
  E2E_NVIDIA_DRIVER_VERSION="550.54.15" \
  E2E_NVIDIA_CUDA_VERSION="12.4.1" \
  E2E_NVIDIA_EXPECTED_COMMIT="${mock_commit}" \
  E2E_NVIDIA_MODEL_REVISION="593b5a2e04c8f3e4ee880263f93e0bd2901ad47f" \
  E2E_NVIDIA_MODEL_SHA256="48ab3034d0dd401fbc721eb1df3217902fee7dab9078992d66431f09b7750201" \
  E2E_NVIDIA_MODEL_SIZE="386404992" \
  LLAMA_CPP_COMMIT="${llama_commit}" \
  LLAMA_CPP_BUILD_NUMBER="${llama_build_number}" \
  LLAMA_CPP_SOURCE_SHA256="${llama_source_sha256}" \
  KEEP_NVIDIA_RESOURCES="${keep_resources}" \
    bash "${fixture_root}/hack/test-e2e-serving-nvidia.sh" \
      >"${state_dir}/stdout.log" 2>"${state_dir}/stderr.log"
  status=$?
  set -e

  if [[ ${status} -ne ${expected_status} ]]; then
    echo "${scenario}: expected status ${expected_status}, got ${status}" >&2
    sed -n '1,200p' "${state_dir}/stderr.log" >&2
    return 1
  fi
  if [[ "${scenario}" == "suite-owned-existing-claim" ||
    "${scenario}" == "invalid-existing-claim-name" ||
    "${scenario}" == "invalid-keep-resources" ]]; then
    if grep -Eq '^(kubectl|helm|cosign|curl)[[:space:]]' "${state_dir}/calls.log"; then
      echo "${scenario}: invalid adopted-claim input reached cluster or registry tools" >&2
      return 1
    fi
    if [[ "${scenario}" == "suite-owned-existing-claim" ]] &&
      ! grep -Fq 'requires preinstalled-controller mode and an existing namespace' \
        "${state_dir}/stderr.log"; then
      echo "suite-owned existing-claim mode was not rejected" >&2
      return 1
    fi
    if [[ "${scenario}" == "invalid-existing-claim-name" ]] &&
      ! grep -Fq 'must be a valid DNS label' "${state_dir}/stderr.log"; then
      echo "invalid existing-claim name was not rejected" >&2
      return 1
    fi
    if [[ "${scenario}" == "invalid-keep-resources" ]] &&
      ! grep -Fq 'KEEP_NVIDIA_RESOURCES must be 0 or 1' "${state_dir}/stderr.log"; then
      echo "invalid keep-resources value was not rejected" >&2
      return 1
    fi
    return
  fi
  if [[ "${scenario}" == "suite-owned-existing-controller" ]]; then
    assert_no_kubernetes_ownership_mutation "${state_dir}/calls.log"
    if ! awk -F '\t' -v test_namespace="kama-qualification" '
      $1 == "helm" {
        count++
        if (NF == 5 && $2 == "status" &&
            $3 == "kama-e2e-serving-nvidia" &&
            $4 == "--namespace" && $5 == test_namespace) {
          allowed++
        }
      }
      END {exit(count == 1 && allowed == 1 ? 0 : 1)}
    ' "${state_dir}/calls.log"; then
      echo "suite-owned mode did not limit Helm to the release-status probe" >&2
      return 1
    fi
    if [[ -s "${state_dir}/package.log" ]] ||
      grep -Fq $'kubectl\twait\t--for=condition=Established' "${state_dir}/calls.log" ||
      grep -Fq $'kubectl\tproxy' "${state_dir}/calls.log"; then
      echo "suite-owned mode continued past the existing-controller guard" >&2
      return 1
    fi
    if ! grep -Fq $'kubectl\tget\tdeployments.apps\t--all-namespaces\t-o\tjson' \
      "${state_dir}/calls.log" ||
      ! grep -Fq $'kubectl\tget\tpods\t--all-namespaces\t-o\tjson' \
        "${state_dir}/calls.log" ||
      ! grep -Fq 'suite-owned mode refuses to install a second Kama controller' \
        "${state_dir}/stderr.log"; then
      echo "suite-owned mode did not exercise the existing-controller guard" >&2
      return 1
    fi
    if [[ -s "${state_dir}/deletes.jsonl" ]]; then
      echo "suite-owned existing-controller refusal attempted resource deletion" >&2
      return 1
    fi
    jq -e '
      .outcome == "FAIL (exit 1)" and .qualifying == false
    ' "${fixture_root}/dist/e2e/serving-nvidia/qualification.json" >/dev/null
    return
  fi

  if [[ "${scenario}" == "suite-owned-namespace-replacement" ||
    "${scenario}" == "suite-owned-residual" ||
    "${scenario}" == "suite-owned-partial-install" ||
    "${scenario}" == "suite-owned-uninstall-failure" ||
    "${scenario}" == "suite-owned-list-failure" ]]; then
    if ! grep -Fq $'helm\tupgrade\t--install\t'"${suite_release}" \
      "${state_dir}/calls.log" ||
      ! grep -Fq $'\t--atomic\t--wait\t--timeout\t5m' "${state_dir}/calls.log"; then
      echo "${scenario}: suite-owned mode did not attempt an atomic Helm install" >&2
      return 1
    fi
    if jq -se --arg namespace "kama-qualification" '
      any(.[]; .url | endswith("/api/v1/namespaces/" + $namespace))
    ' "${state_dir}/deletes.jsonl" >/dev/null; then
      namespace_deleted=1
    else
      namespace_deleted=0
    fi

    case "${scenario}" in
      suite-owned-partial-install)
        if [[ ${namespace_deleted} -ne 1 ||
          -f "${state_dir}/helm-release-present" ]]; then
          echo "partial Helm install was not safely uninstalled with its owned namespace" >&2
          return 1
        fi
        if [[ "$(grep -Fc $'helm\tlist\t--namespace\tkama-qualification\t--all\t-o\tjson' \
          "${state_dir}/calls.log")" -ne 2 ]] ||
          ! grep -Fq $'helm\tuninstall\t'"${suite_release}" \
            "${state_dir}/calls.log"; then
          echo "partial Helm install cleanup did not prove release removal" >&2
          return 1
        fi
        jq -se '
          ([.[] | select(.url | endswith("/api/v1/namespaces/kama-qualification"))] |
            length) == 1 and
          ([.[] | select(.url | endswith("/api/v1/namespaces/kama-qualification"))][0] |
            .body.preconditions.uid == "namespace-uid-original" and
            (.pending | startswith("PENDING: NVIDIA suite cleanup")) and
            .qualifying == false)
        ' "${state_dir}/deletes.jsonl" >/dev/null
        jq -e '
          .outcome == "FAIL (exit 44)" and .qualifying == false and
          .cleanupComplete == true
        ' "${fixture_root}/dist/e2e/serving-nvidia/qualification.json" >/dev/null
        ;;
      suite-owned-uninstall-failure)
        if [[ ${namespace_deleted} -ne 0 ||
          ! -f "${state_dir}/helm-release-present" ]] ||
          ! grep -Fq $'helm\tlist\t--namespace\tkama-qualification\t--all\t-o\tjson' \
            "${state_dir}/calls.log" ||
          ! grep -Fq $'helm\tuninstall\t'"${suite_release}" \
            "${state_dir}/calls.log"; then
          echo "failed Helm uninstall did not preserve the owned namespace and release" >&2
          return 1
        fi
        ;;
      suite-owned-list-failure)
        if [[ ${namespace_deleted} -ne 0 ||
          ! -f "${state_dir}/helm-release-present" ]] ||
          ! grep -Fq $'helm\tlist\t--namespace\tkama-qualification\t--all\t-o\tjson' \
            "${state_dir}/calls.log" ||
          grep -Fq $'helm\tuninstall\t' "${state_dir}/calls.log"; then
          echo "failed Helm release discovery did not preserve the owned namespace" >&2
          return 1
        fi
        ;;
      suite-owned-namespace-replacement)
        if [[ ${namespace_deleted} -ne 0 ||
          ! -f "${state_dir}/helm-release-present" ||
          "$(<"${state_dir}/namespace-last-uid")" != "namespace-uid-replacement" ]] ||
          grep -Fq $'helm\tlist\t' "${state_dir}/calls.log" ||
          grep -Fq $'helm\tuninstall\t' "${state_dir}/calls.log"; then
          echo "namespace replacement was not preserved ahead of release teardown" >&2
          return 1
        fi
        ;;
      suite-owned-residual)
        if [[ ${namespace_deleted} -ne 0 ||
          ! -f "${state_dir}/helm-release-present" ]] ||
          grep -Fq $'helm\tlist\t' "${state_dir}/calls.log" ||
          grep -Fq $'helm\tuninstall\t' "${state_dir}/calls.log" ||
          ! grep -Fq 'NVIDIA suite cleanup left resources matching' \
            "${state_dir}/stderr.log"; then
          echo "residual resources did not prevent release and namespace teardown" >&2
          return 1
        fi
        ;;
    esac

    if [[ "${scenario}" != "suite-owned-partial-install" ]]; then
      jq -e '
        .outcome == "FAIL: NVIDIA suite cleanup did not complete safely" and
        .qualifying == false and .cleanupComplete == false
      ' "${fixture_root}/dist/e2e/serving-nvidia/qualification.json" >/dev/null
    fi
    return
  fi

  assert_preinstalled_controller_behavior "${state_dir}/calls.log" "${configured_runtime_class}"

  if [[ "${scenario}" == "adopted-unsafe-owner" ||
    "${scenario}" == "adopted-wrong-storage" ||
    "${scenario}" == "adopted-pv-mismatch" ||
    "${scenario}" == "adopted-existing-reference" ||
    "${scenario}" == "adopted-active-pod" ||
    "${scenario}" == "adopted-active-job" ]]; then
    if grep -Fq $'kubectl\t-n\tkama-qualification\tcreate\t-f' "${state_dir}/calls.log" ||
      [[ -s "${state_dir}/deletes.jsonl" ]]; then
      echo "${scenario}: unsafe adopted claim continued into workload mutation or deletion" >&2
      return 1
    fi
    case "${scenario}" in
      adopted-unsafe-owner | adopted-wrong-storage)
        expected_error='must be an unowned, unguarded, Bound RWO Filesystem claim'
        ;;
      adopted-pv-mismatch)
        expected_error='claim and PersistentVolume identities are not coherent and Bound'
        ;;
      adopted-existing-reference)
        expected_error='is already referenced by Kama resources'
        ;;
      adopted-active-pod | adopted-active-job)
        expected_error='has active foreign Pod or Job consumers'
        ;;
    esac
    if ! grep -Fq "${expected_error}" "${state_dir}/stderr.log"; then
      echo "${scenario}: expected validation error was not reported" >&2
      sed -n '1,160p' "${state_dir}/stderr.log" >&2
      return 1
    fi
    return
  fi

  if [[ "${scenario}" == "runtime-class-mismatch" ]]; then
    if grep -Fq $'kubectl\t-n\tkama-qualification\tcreate\t-f' "${state_dir}/calls.log" ||
      [[ -s "${state_dir}/deletes.jsonl" ]]; then
      echo "runtime-class mismatch continued into workload mutation or cleanup deletion" >&2
      return 1
    fi
    if ! grep -Fq 'does not match the expected immutable images, CUDA RuntimeClass' \
      "${state_dir}/stderr.log"; then
      echo "preinstalled mode did not reject the mismatched CUDA RuntimeClass flag" >&2
      return 1
    fi
    jq -e --arg runtimeClass "${runtime_class}" '
      .nvidia.expectedRuntimeClassName == $runtimeClass
    ' "${fixture_root}/dist/e2e/serving-nvidia/identities.json" >/dev/null
    return
  fi

  if [[ "${scenario}" == "unrelated-manager-image-digest" ]]; then
    if grep -Fq $'kubectl\t-n\tkama-qualification\tcreate\t-f' "${state_dir}/calls.log" ||
      [[ -s "${state_dir}/deletes.jsonl" ]]; then
      echo "unrelated manager image digest continued into workload mutation or cleanup deletion" >&2
      return 1
    fi
    if ! grep -Fq 'requires exactly one ready Kama manager Pod running the expected immutable image' \
      "${state_dir}/stderr.log"; then
      echo "preinstalled mode did not reject the unrelated manager image digest" >&2
      return 1
    fi
    return
  fi

  jq -e --arg runtimeClass "${configured_runtime_class}" '
    .nvidia.expectedRuntimeClassName == $runtimeClass
  ' "${fixture_root}/dist/e2e/serving-nvidia/identities.json" >/dev/null

  if [[ "${scenario}" == "keep-resources" ]]; then
    if [[ -s "${state_dir}/deletes.jsonl" ]] ||
      grep -Fq $'kubectl\tproxy' "${state_dir}/calls.log"; then
      echo "keep-resources mode unexpectedly attempted cleanup" >&2
      return 1
    fi
    if ! grep -Fq 'retains diagnostic resources and cannot produce qualifying evidence' \
      "${state_dir}/stderr.log"; then
      echo "keep-resources mode did not explain its non-qualifying result" >&2
      return 1
    fi
    jq -e '
      .outcome == "FAIL: NVIDIA suite cleanup did not complete safely" and
      .qualifying == false
    ' "${fixture_root}/dist/e2e/serving-nvidia/qualification.json" >/dev/null
    return
  fi

  if [[ "${scenario}" == "proxy-start-timeout" ]]; then
    if ! grep -Fq $'kubectl\tproxy\t--address=127.0.0.1\t--port=0' \
      "${state_dir}/calls.log" || [[ ! -s "${state_dir}/proxy.pid" ]]; then
      echo "proxy timeout scenario did not start the cleanup proxy" >&2
      return 1
    fi
    proxy_pid="$(<"${state_dir}/proxy.pid")"
    if kill -0 "${proxy_pid}" >/dev/null 2>&1; then
      echo "cleanup proxy remained alive after its startup timeout" >&2
      return 1
    fi
    if [[ -s "${state_dir}/deletes.jsonl" ]]; then
      echo "proxy timeout scenario attempted deletion without a usable proxy" >&2
      return 1
    fi
    jq -e '
      .outcome == "FAIL: NVIDIA suite cleanup did not complete safely" and
      .qualifying == false and .cleanupComplete == false
    ' "${fixture_root}/dist/e2e/serving-nvidia/qualification.json" >/dev/null
    return
  fi

  if [[ "${scenario}" == "owned" || "${scenario}" == "owned-default-runtime" ||
    "${scenario}" == "adopted" ]]; then
    if ! jq -se '
      length == 2 and
      all(.[];
        (.pending | startswith("PENDING: NVIDIA suite cleanup")) and
        .qualifying == false and
        .body.apiVersion == "v1" and
        .body.kind == "DeleteOptions" and
        .body.propagationPolicy == "Foreground") and
      ([.[].body.preconditions.uid] | sort) ==
        (["artifact-uid-original", "cache-uid-original"] | sort)
    ' "${state_dir}/deletes.jsonl" >/dev/null; then
      echo "${scenario}: cleanup deletion evidence did not match the expected UID-safe contract" >&2
      sed -n '1,20p' "${state_dir}/deletes.jsonl" >&2
      sed -n '1,80p' "${state_dir}/stderr.log" >&2
      return 1
    fi
    jq -e '
      .outcome == "FAIL (exit 23)" and .qualifying == false
    ' "${fixture_root}/dist/e2e/serving-nvidia/qualification.json" >/dev/null
    if [[ "${scenario}" == "owned" ]]; then
      if ! grep -Fq 'retentionPolicy: Delete' "${state_dir}/created-storage.yaml" ||
        ! grep -Fq 'claimTemplate:' "${state_dir}/created-storage.yaml" ||
        ! grep -Fq 'storageClassName: mock-csi' "${state_dir}/created-storage.yaml" ||
        grep -Fq 'existingClaim:' "${state_dir}/created-storage.yaml"; then
        echo "default cache manifest no longer selects claimTemplate/Delete exclusively" >&2
        return 1
      fi
    elif [[ "${scenario}" == "adopted" ]]; then
      if ! grep -Fq 'retentionPolicy: Retain' "${state_dir}/created-storage.yaml" ||
        ! grep -Fq 'existingClaim:' "${state_dir}/created-storage.yaml" ||
        ! grep -Fq 'name: existing-cache' "${state_dir}/created-storage.yaml" ||
        ! grep -Fq 'kind: ModelArtifact' "${state_dir}/created-storage.yaml" ||
        grep -Fq 'claimTemplate:' "${state_dir}/created-storage.yaml" ||
        grep -Fq 'retentionPolicy: Delete' "${state_dir}/created-storage.yaml"; then
        echo "adopted cache manifest did not select existingClaim/Retain exclusively" >&2
        return 1
      fi
      jq -e '
        .storage.mode == "existingClaim" and .storage.storageClass == "mock-csi" and
        .storage.adoptedClaim == {
          name: "existing-cache",
          uid: "existing-claim-uid",
          volumeName: "existing-pv",
          volumeUID: "existing-pv-uid"
        }
      ' "${fixture_root}/dist/e2e/serving-nvidia/identities.json" >/dev/null
      if grep -Fq '/persistentvolumeclaims/' "${state_dir}/deletes.jsonl" ||
        grep -Fq '/persistentvolumes/' "${state_dir}/deletes.jsonl"; then
        echo "cleanup attempted to delete adopted storage" >&2
        return 1
      fi
      pvc_reads="$(grep -Fc $'kubectl\t-n\tkama-qualification\tget\tpvc\texisting-cache\t-o\tjson' \
        "${state_dir}/calls.log")"
      if [[ "${pvc_reads}" -lt 2 ]]; then
        echo "cleanup did not revalidate retained adopted storage" >&2
        return 1
      fi
    fi
  else
    jq -se '
      length == 1 and
      .[0].body.preconditions.uid == "cache-uid-original" and
      (.[0].pending | startswith("PENDING: NVIDIA suite cleanup")) and
      .[0].qualifying == false
    ' "${state_dir}/deletes.jsonl" >/dev/null
    if grep -Fq 'artifact-uid-replacement' "${state_dir}/deletes.jsonl"; then
      echo "cleanup attempted to delete the replacement ModelArtifact UID" >&2
      return 1
    fi
    jq -e '
      .outcome == "FAIL: NVIDIA suite cleanup did not complete safely" and
      .qualifying == false
    ' "${fixture_root}/dist/e2e/serving-nvidia/qualification.json" >/dev/null
  fi
}

test_controller_identity_change() {
  local state_dir="${test_root}/controller-identity-change"
  local functions_file="${state_dir}/controller-functions.sh"
  local status=0
  mkdir -p "${state_dir}"
  : >"${state_dir}/calls.log"
  : >"${state_dir}/deletes.jsonl"
  awk '
    /^signed_linux_manifest_digests\(\) \{/ {copy = 1}
    /^verify_existing_test_namespace_is_clean\(\) \{/ {exit}
    copy {print}
  ' "${fixture_root}/hack/test-e2e-serving-nvidia.sh" >"${functions_file}"

  set +e
  (
    set -euo pipefail
    PATH="${mock_bin}:${PATH}"
    export PATH
    export MOCK_CALL_LOG="${state_dir}/calls.log"
    export MOCK_DELETE_LOG="${state_dir}/deletes.jsonl"
    export MOCK_STATE_DIR="${state_dir}"
    export MOCK_SCENARIO="controller-identity-change"
    export MOCK_EVIDENCE_DIR="${fixture_root}/dist/e2e/serving-nvidia"
    export MOCK_COMMIT="${mock_commit}"
    export MOCK_PARENT_DIGEST="${mock_parent_digest}"
    export MOCK_CHILD_DIGEST="${mock_child_digest}"
    export MOCK_CONFIG_DIGEST="${mock_config_digest}"
    export MOCK_TEST_NAMESPACE="kama-qualification"
    export MOCK_CONTROLLER_NAMESPACE="kama-system"
    export MOCK_CONTROLLER_DEPLOYMENT="kama"
    export MOCK_SUITE_RELEASE="kama-e2e-serving-nvidia"
    export MOCK_STORAGE_CLASS="mock-csi"
    export MOCK_MANAGER_IMAGE="${manager_image}"
    export MOCK_IMPORTER_IMAGE="${importer_image}"
    export MOCK_RUNTIME_CPU_IMAGE="${runtime_cpu_image}"
    export MOCK_RUNTIME_CUDA_IMAGE="${runtime_cuda_image}"
    export MOCK_RUNTIME_CLASS="${runtime_class}"
    export MOCK_OBSERVED_RUNTIME_CLASS="${runtime_class}"
    export MOCK_EXISTING_CLAIM=""
    export MOCK_LLAMA_COMMIT="${llama_commit}"
    export MOCK_LLAMA_BUILD_NUMBER="${llama_build_number}"
    export MOCK_LLAMA_SOURCE_SHA256="${llama_source_sha256}"
    kubectl_bin="${mock_bin}/kubectl"
    controller_namespace="kama-system"
    controller_deployment="kama"
    manager_image="${manager_image}"
    importer_image="${importer_image}"
    runtime_cpu_image="${runtime_cpu_image}"
    runtime_cuda_image="${runtime_cuda_image}"
    runtime_class="${runtime_class}"
    llama_commit="${llama_commit}"
    controller_deployment_uid=""
    controller_observed_generation=""
    controller_pod_uid=""
    preinstalled_controller_verified=0
    # Source the exact production verification functions so this exercises their
    # stateful second-check behavior without mocking the entire GPU-serving path.
    # shellcheck source=/dev/null
    source "${functions_file}"
    verify_preinstalled_controller
    if verify_preinstalled_controller; then
      echo "replacement controller identity was accepted" >&2
      exit 99
    fi
  ) >"${state_dir}/stdout.log" 2>"${state_dir}/stderr.log"
  status=$?
  set -e

  if [[ ${status} -ne 0 ]]; then
    echo "controller identity-change regression failed with status ${status}" >&2
    sed -n '1,160p' "${state_dir}/stderr.log" >&2
    return 1
  fi
  if [[ "$(<"${state_dir}/controller-reads")" -ne 2 ]] ||
    ! grep -Fq 'controller identity changed during NVIDIA qualification' \
      "${state_dir}/stderr.log"; then
    echo "second controller verification did not reject an identity change" >&2
    return 1
  fi
}

# The owned case reports the signed parent-index digest, matching K3s/containerd;
# owned-default-runtime retains coverage for a resolved Linux child digest.
run_scenario owned 23
run_scenario owned-default-runtime 23
run_scenario adopted 23
run_scenario adopted-unsafe-owner 1
run_scenario adopted-wrong-storage 1
run_scenario adopted-pv-mismatch 1
run_scenario adopted-existing-reference 1
run_scenario adopted-active-pod 1
run_scenario adopted-active-job 1
run_scenario replacement 1
run_scenario runtime-class-mismatch 1
run_scenario unrelated-manager-image-digest 1
run_scenario keep-resources 1
run_scenario proxy-start-timeout 1
run_scenario suite-owned-existing-controller 1
run_scenario suite-owned-existing-claim 2
run_scenario suite-owned-namespace-replacement 1
run_scenario suite-owned-residual 1
run_scenario suite-owned-partial-install 44
run_scenario suite-owned-uninstall-failure 1
run_scenario suite-owned-list-failure 1
run_scenario invalid-existing-claim-name 2
run_scenario invalid-keep-resources 2
test_controller_identity_change

echo "M2 NVIDIA controller-mode lifecycle regression tests passed"
