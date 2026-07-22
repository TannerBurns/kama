#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
service_contract="$(<"${repo_root}/hack/m2-nvidia-service-contract.jq")"
storage_contract="$(<"${repo_root}/hack/m2-nvidia-storage-contract.jq")"
runtime_class_contract='
  if $runtimeClass == "" then
    .runtimeClassName == null
  else
    .runtimeClassName == $runtimeClass
  end
'
fixture_dir="${repo_root}/test/e2e/serving/evidence"
service_name="e2e-serving-nvidia-abc123"
service_uid="11111111-2222-3333-4444-555555555555"

assert_service_passes() {
  local fixture=$1
  if ! jq -e --arg name "${service_name}" --arg uid "${service_uid}" \
    "${service_contract}" "${fixture_dir}/${fixture}" >/dev/null; then
    echo "expected NVIDIA Service evidence fixture to pass: ${fixture}" >&2
    exit 1
  fi
}

assert_service_fails() {
  local fixture=$1
  if jq -e --arg name "${service_name}" --arg uid "${service_uid}" \
    "${service_contract}" "${fixture_dir}/${fixture}" >/dev/null; then
    echo "expected NVIDIA Service evidence fixture to fail: ${fixture}" >&2
    exit 1
  fi
}

assert_service_passes nvidia-service-named-target-port.json
assert_service_fails nvidia-service-numeric-target-port.json
assert_service_fails nvidia-service-wrong-target-port.json

if ! jq -e --arg runtimeClass vendor-gpu.example "${runtime_class_contract}" \
  "${fixture_dir}/nvidia-pod-spec-runtime-class.json" >/dev/null ||
  ! jq -e --arg runtimeClass '' "${runtime_class_contract}" \
    "${fixture_dir}/nvidia-pod-spec-no-runtime-class.json" >/dev/null; then
  echo "expected NVIDIA RuntimeClass evidence fixtures to pass" >&2
  exit 1
fi
if jq -e --arg runtimeClass vendor-gpu.example "${runtime_class_contract}" \
  "${fixture_dir}/nvidia-pod-spec-no-runtime-class.json" >/dev/null ||
  jq -e --arg runtimeClass vendor-gpu.example "${runtime_class_contract}" \
    "${fixture_dir}/nvidia-pod-spec-wrong-runtime-class.json" >/dev/null ||
  jq -e --arg runtimeClass '' "${runtime_class_contract}" \
    "${fixture_dir}/nvidia-pod-spec-runtime-class.json" >/dev/null; then
  echo "expected NVIDIA RuntimeClass evidence fixtures to fail" >&2
  exit 1
fi

model_digest="48ab3034d0dd401fbc721eb1df3217902fee7dab9078992d66431f09b7750201"
model_revision="593b5a2e04c8f3e4ee880263f93e0bd2901ad47f"
model_size=386404992
storage_class="test-csi"
namespace="kama-qualification"
adopted_claim="existing-cache"
adopted_claim_uid="claim-uid"
adopted_volume="existing-pv"
adopted_volume_uid="volume-uid"
existing_storage_fixture="$(jq -cn \
  --arg digest "${model_digest}" \
  --arg revision "${model_revision}" \
  --argjson size "${model_size}" \
  --arg storageClass "${storage_class}" \
  --arg namespace "${namespace}" \
  --arg claim "${adopted_claim}" \
  --arg claimUID "${adopted_claim_uid}" \
  --arg volume "${adopted_volume}" \
  --arg volumeUID "${adopted_volume_uid}" '{
    schemaVersion: 1,
    mode: "existingClaim",
    storageClass: $storageClass,
    modelCache: {
      metadata: {name: "e2e-serving-nvidia-cache", uid: "cache-uid", generation: 1},
      spec: {retentionPolicy: "Retain", storage: {existingClaim: {name: $claim}}},
      status: {
        observedGeneration: 1,
        claimName: $claim,
        claimUID: $claimUID,
        volumeName: $volume,
        volumeUID: $volumeUID,
        storageClassName: $storageClass,
        conditions: [{type: "Ready", status: "True", reason: "Ready"}]
      }
    },
    modelArtifact: {
      metadata: {name: "e2e-serving-nvidia-model", uid: "artifact-uid", generation: 1},
      spec: {
        format: "GGUF",
        entrypoint: "smollm2-360m-instruct-q8_0.gguf",
        cacheRef: {name: "e2e-serving-nvidia-cache"},
        verification: {expectedSHA256: $digest, expectedSize: $size},
        source: {huggingFace: {
          repository: "HuggingFaceTB/SmolLM2-360M-Instruct-GGUF",
          revision: $revision,
          files: ["smollm2-360m-instruct-q8_0.gguf"]
        }}
      },
      status: {
        observedGeneration: 1,
        artifactDigest: $digest,
        location: {
          claimName: $claim,
          claimUID: $claimUID,
          volumeName: $volume,
          volumeUID: $volumeUID,
          readOnly: true
        },
        conditions: [{type: "Ready", status: "True", reason: "Ready"}]
      }
    },
    persistentVolumeClaim: {
      metadata: {namespace: $namespace, name: $claim, uid: $claimUID},
      spec: {
        storageClassName: $storageClass,
        accessModes: ["ReadWriteOnce"],
        volumeMode: "Filesystem",
        volumeName: $volume
      },
      status: {phase: "Bound", accessModes: ["ReadWriteOnce"]}
    },
    persistentVolume: {
      metadata: {name: $volume, uid: $volumeUID},
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
    }
  }')"

assert_storage_contract() {
  local fixture=$1
  local mode=$2
  local expected=$3
  local actual=""
  local claim="" claim_uid="" volume="" volume_uid=""
  if [[ "${mode}" == "existingClaim" ]]; then
    claim="${adopted_claim}"
    claim_uid="${adopted_claim_uid}"
    volume="${adopted_volume}"
    volume_uid="${adopted_volume_uid}"
  fi
  if jq -e \
    --arg mode "${mode}" \
    --arg storageClass "${storage_class}" \
    --arg namespace "${namespace}" \
    --arg adoptedClaim "${claim}" \
    --arg adoptedClaimUID "${claim_uid}" \
    --arg adoptedVolume "${volume}" \
    --arg adoptedVolumeUID "${volume_uid}" \
    --arg modelDigest "${model_digest}" \
    --arg modelRevision "${model_revision}" \
    --argjson modelSize "${model_size}" \
    "${storage_contract}" <<<"${fixture}" >/dev/null; then
    actual=pass
  else
    actual=fail
  fi
  if [[ "${actual}" != "${expected}" ]]; then
    echo "expected ${mode} NVIDIA storage contract to ${expected}, got ${actual}" >&2
    exit 1
  fi
}

assert_storage_contract "${existing_storage_fixture}" existingClaim pass
assert_storage_contract "$(jq -c '.modelCache.spec.retentionPolicy = "Delete"' \
  <<<"${existing_storage_fixture}")" existingClaim fail
assert_storage_contract "$(jq -c '.persistentVolumeClaim.metadata.uid = "replacement-uid"' \
  <<<"${existing_storage_fixture}")" existingClaim fail
assert_storage_contract "$(jq -c '.persistentVolume.metadata.deletionTimestamp = "2026-07-21T00:00:00Z"' \
  <<<"${existing_storage_fixture}")" existingClaim fail

managed_storage_fixture="$(jq -c \
  --arg storageClass "${storage_class}" '
    .mode = "claimTemplate" |
    .modelCache.spec = {
      retentionPolicy: "Delete",
      storage: {claimTemplate: {spec: {
        storageClassName: $storageClass,
        accessModes: ["ReadWriteOnce"],
        volumeMode: "Filesystem",
        resources: {requests: {storage: "2Gi"}}
      }}}
    }
  ' <<<"${existing_storage_fixture}")"
assert_storage_contract "${managed_storage_fixture}" claimTemplate pass
assert_storage_contract "$(jq -c '.modelCache.spec.storage.existingClaim = {name: "foreign"}' \
  <<<"${managed_storage_fixture}")" claimTemplate fail

echo "M2 NVIDIA Service, RuntimeClass, and storage evidence contract fixtures passed"
