#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
suite="${1:-}"
evidence_dir="${2:-}"
expected_commit="${3:-${E2E_EXPECTED_COMMIT:-}}"
expected_kubernetes_minor="${4:-${E2E_EXPECTED_KUBERNETES_MINOR:-}}"
require_qualifying="${E2E_REQUIRE_QUALIFYING:-1}"

usage() {
  echo "usage: $0 cpu|nvidia EVIDENCE_DIR EXPECTED_COMMIT [EXPECTED_KUBERNETES_MINOR]" >&2
  exit 2
}

[[ "${suite}" == "cpu" || "${suite}" == "nvidia" ]] || usage
[[ -n "${evidence_dir}" && -d "${evidence_dir}" ]] || {
  echo "M2 ${suite} evidence directory is unavailable: ${evidence_dir:-unset}" >&2
  exit 2
}
[[ "${expected_commit}" =~ ^[a-f0-9]{40}$ ]] || {
  echo "expected commit must be a full lowercase Git SHA" >&2
  exit 2
}
[[ "${require_qualifying}" == "0" || "${require_qualifying}" == "1" ]] || {
  echo "E2E_REQUIRE_QUALIFYING must be 0 or 1" >&2
  exit 2
}
if [[ "${suite}" == "cpu" && ! "${expected_kubernetes_minor}" =~ ^1\.(34|35|36)$ ]]; then
  echo "CPU evidence requires an expected Kubernetes minor in {1.34,1.35,1.36}" >&2
  exit 2
fi

version_value() {
  local name=$1
  awk -v name="${name}" '$1 == name {print $3}' "${repo_root}/hack/versions.mk"
}

llama_commit="$(version_value LLAMA_CPP_COMMIT)"
llama_build_number="$(version_value LLAMA_CPP_BUILD_NUMBER)"
llama_source_sha256="$(version_value LLAMA_CPP_SOURCE_SHA256)"
cuda_version="$(version_value CUDA_VERSION)"
model_digest="48ab3034d0dd401fbc721eb1df3217902fee7dab9078992d66431f09b7750201"
model_revision="593b5a2e04c8f3e4ee880263f93e0bd2901ad47f"
model_size=386404992

require_file() {
  local path="${evidence_dir}/$1"
  if [[ ! -s "${path}" ]]; then
    echo "M2 ${suite} evidence is missing or empty: $1" >&2
    exit 1
  fi
}

require_json() {
  require_file "$1"
  if ! jq -e . "${evidence_dir}/$1" >/dev/null 2>&1; then
    echo "M2 ${suite} evidence is not valid JSON: $1" >&2
    exit 1
  fi
}

assert_json() {
  local file=$1
  local description=$2
  local filter=$3
  shift 3
  if ! jq -e "$@" "${filter}" "${evidence_dir}/${file}" >/dev/null; then
    echo "M2 ${suite} evidence failed: ${description} (${file})" >&2
    exit 1
  fi
}

verify_common_qualification() {
  local expected_suite="baseline-serving/${suite}"
  require_json qualification.json
  assert_json qualification.json "qualification schema and PASS outcome" '
    .schemaVersion == 1 and .suite == $suite and
    (.outcome | type == "string" and startswith("PASS:"))
  ' --arg suite "${expected_suite}"
  if [[ "${require_qualifying}" == "1" ]]; then
    assert_json qualification.json "strict qualification and complete evidence" '
      .qualifying == true and .imageProvenanceVerified == true and
      .evidenceComplete == true and
      (.redactionVerified == true or .credentialSafe == true)
    '
  fi
  require_file outcome.txt
  if ! grep -Fq 'PASS:' "${evidence_dir}/outcome.txt"; then
    echo "M2 ${suite} outcome.txt does not record PASS" >&2
    exit 1
  fi
}

verify_cpu() {
  local required_json=(
    identities.json
    kubernetes-version.json
    artifact-gating.json
    serving-contract.json
    direct-request.json
    direct-request-job.json
    direct-request.log
    delayed-loading.json
    drain.json
    load-failure.json
    supervisor-state.json
    modeldeployments.json
    runtime-pods.json
    endpointslices.json
  )
  local file
  verify_common_qualification
  for file in "${required_json[@]}"; do
    require_json "${file}"
  done
  require_file resources.txt
  require_file events.txt

  assert_json qualification.json "immutable CPU image digests" '
    .immutableManifestDigestsPresent == true and .kubernetesMinorVerified == true
  '
  assert_json identities.json "commit, runtime pins, model, and immutable image identities" '
    .commit == $commit and
    .llamaCommit == $llamaCommit and
    .llamaBuildNumber == $llamaBuild and
    .llamaSourceSHA256 == $llamaSource and
    .modelDigest == $modelDigest and
    ([
      .images.manager.manifestDigest,
      .images.importer.manifestDigest,
      .images.servingClient.manifestDigest,
      .images.runtimeCPU.manifestDigest
    ] | all(type == "string" and test("^sha256:[a-f0-9]{64}$")))
  ' --arg commit "${expected_commit}" \
    --arg llamaCommit "${llama_commit}" \
    --arg llamaBuild "${llama_build_number}" \
    --arg llamaSource "${llama_source_sha256}" \
    --arg modelDigest "${model_digest}"
  if [[ "${require_qualifying}" == "1" ]]; then
    assert_json identities.json "clean, provenance-verified CPU runtime" '
      .images.runtimeCPU.provenanceVerified == true and
      .images.runtimeCPU.sourceCheckoutClean == true
    '
  fi

  assert_json kubernetes-version.json "matching Kubernetes client and API server minor" '
    .clientVersion.major == "1" and
    (.clientVersion.minor | sub("[+]$"; "")) == $minor and
    .serverVersion.major == "1" and
    (.serverVersion.minor | sub("[+]$"; "")) == $minor
  ' --arg minor "${expected_kubernetes_minor#1.}"

  assert_json artifact-gating.json "never-loaded artifact dependency gate" '
    .serviceCreated == true and .artifactReady == false and
    .desiredReplicas == 0 and .readyReplicas == 0 and
    .deploymentCreated == false and .podCreated == false and
    .runtimeConfigCreated == false and .readyEndpoint == false
  '
  assert_json serving-contract.json "generated workload, Service, placement, and status identity" '
    .validated == true and .artifactIdentityMatched == true and
    .runtimeIdentityMatched == true and .restrictedWorkload == true and
    .servicePortContract == true and .rwoPlacementMatched == true and
    (.artifactUID | type == "string" and length > 0) and
    (.deployment.name | type == "string" and length > 0) and
    (.deployment.uid | type == "string" and length > 0) and
    (.service.name | type == "string" and length > 0) and
    (.service.uid | type == "string" and length > 0) and
    (.pod.uid | type == "string" and length > 0) and
    (.runtime.fingerprint | type == "string" and length == 20) and
    .runtime.desiredImage == $image
  ' --arg image "$(jq -r '.images.runtimeCPU.reference' "${evidence_dir}/identities.json")"
  assert_json direct-request.log "complete CPU SSE response" '
    .schemaVersion == 1 and .sseDataEvents > 0 and
    (.generatedContentFragments | type) == "number" and .generatedContentFragments > 0 and
    (.generatedContentBytes | type) == "number" and .generatedContentBytes > 0 and .done == true
  '
  assert_json direct-request.json "in-cluster CPU Service request evidence" '
    .transport == "in-cluster ClusterIP Service DNS" and
    .route == "/v1/chat/completions" and .stream == true and .completed == true and
    .generatedContentObserved == true and
    (.generatedContentFragments | type) == "number" and .generatedContentFragments > 0 and
    (.generatedContentBytes | type) == "number" and .generatedContentBytes > 0 and
    (.serviceDNS | type == "string" and endswith(".kama-system.svc")) and
    .clientImageManifestDigest == $digest
  ' --arg digest "$(jq -r '.images.servingClient.manifestDigest' "${evidence_dir}/identities.json")"
  assert_json delayed-loading.json "delayed readiness without endpoint publication or restart" '
    .childTemporarilyStopped == true and .supervisorAliveWhileLoading == true and
    .runtimeReadyWhileLoading == false and .readyEndpointWhileLoading == false and
    .eventuallyRuntimeReady == true and .eventuallyReadyEndpoint == true and
    .restartCount == 0 and .loadingState.phase == "Loading" and
    .loadingState.ready == false and .loadingState.runtime.mode == "CPU" and
    .loadingState.runtime.acceleratorDetected == false
  '
  assert_json drain.json "bounded drain-first active stream completion" '
    .firstEventObservedBeforeUpdate == true and .streamActiveBeforeUpdate == true and
    .completionAbsentBeforeUpdate == true and .readinessRemovedWhileActive == true and
    .activeStreamCompleted == true and .withinDrainTimeout == true and
    (.elapsedSeconds | type == "number" and . >= 0 and . <= 120) and
    (.oldPodUID | type == "string" and length > 0) and
    (.newPodUID | type == "string" and length > 0) and .oldPodUID != .newPodUID
  '
  assert_json load-failure.json "stable terminal failure without restart or endpoint" '
    .supervisorRunning == true and .restartCount == 0 and .readyEndpoint == false and
    .stableObservation == true and (.pod | type == "string" and length > 0)
  '
  assert_json supervisor-state.json "sanitized CPU runtime state" '
    .schemaVersion == "kama.runtime/v1alpha1" and .phase == "Ready" and .ready == true and
    .runtime.mode == "CPU" and .runtime.effectiveContextTokens == 2048 and
    .runtime.desiredConcurrency == 1 and .runtime.llamaCPPCommit == $llamaCommit and
    .runtime.llamaCPPBuildNumber == $llamaBuild and
    .runtime.acceleratorDetected == false and
    (.deployment.fingerprint | type == "string" and length == 20) and
    (.artifact.uid | type == "string" and length > 0) and .artifact.digest == $modelDigest
  ' --arg llamaCommit "${llama_commit}" \
    --arg llamaBuild "${llama_build_number}" \
    --arg modelDigest "${model_digest}"
  assert_json modeldeployments.json "current CPU status identity and terminal failure status" '
    ([.items[] | select(.metadata.name == "e2e-serving-cpu")] | length) == 1 and
    ([.items[] | select(.metadata.name == "e2e-serving-load-failure")] | length) == 1 and
    ([.items[] | select(.metadata.name == "e2e-serving-cpu")][0] as $ready |
      $ready.status.observedGeneration == $ready.metadata.generation and
      $ready.status.desiredReplicas == 1 and $ready.status.readyReplicas == 1 and
      $ready.status.artifact.name == "e2e-serving-model" and
      ($ready.status.artifact.uid | type == "string" and length > 0) and
      $ready.status.artifact.digest == $modelDigest and
      ($ready.status.deploymentRef.uid | type == "string" and length > 0) and
      ($ready.status.serviceRef.uid | type == "string" and length > 0) and
      $ready.status.runtime.state == "Ready" and
      $ready.status.runtime.desiredFingerprint == $ready.status.runtime.observedFingerprint and
      $ready.status.runtime.desiredFingerprint == $ready.status.runtime.loadedFingerprint and
      $ready.status.runtime.effectiveContextTokens == 2048 and
      $ready.status.runtime.effectiveConcurrency == 1 and
      $ready.status.runtime.acceleratorDetected == false and
      any($ready.status.conditions[]; .type == "Serving" and .status == "True") and
      any($ready.status.conditions[]; .type == "Degraded" and .status == "True" and .reason == "CPUOnlyRequested")) and
    ([.items[] | select(.metadata.name == "e2e-serving-load-failure")][0] as $failed |
      $failed.status.runtime.state == "LoadFailed" and
      ($failed.status.readyReplicas // 0) == 0 and
      any($failed.status.conditions[]; .type == "RuntimeReady" and .status == "False") and
      any($failed.status.conditions[]; .type == "Serving" and .status == "False"))
  ' --arg modelDigest "${model_digest}"
  assert_json runtime-pods.json "CPU runtime Pod security, readiness, and zero restarts" '
    ([.items[] | select(.metadata.labels["kama.tannerburns.github.io/model-deployment"] == "e2e-serving-cpu")] | length) == 1 and
    ([.items[] | select(.metadata.labels["kama.tannerburns.github.io/model-deployment"] == "e2e-serving-load-failure")] | length) == 1 and
    all(.items[];
      .spec.securityContext.runAsUser == 65532 and
      .spec.securityContext.runAsGroup == 65532 and
      .spec.securityContext.fsGroup == 65532 and
      .spec.securityContext.runAsNonRoot == true and
      .spec.securityContext.seccompProfile.type == "RuntimeDefault" and
      any(.spec.containers[];
        .name == "runtime" and .securityContext.runAsUser == 65532 and
        .securityContext.runAsGroup == 65532 and .securityContext.runAsNonRoot == true and
        .securityContext.readOnlyRootFilesystem == true and
        .securityContext.allowPrivilegeEscalation == false and
        (.securityContext.capabilities.drop | index("ALL") != null) and
        any(.volumeMounts[]; .mountPath == "/models" and .readOnly == true) and
        any(.volumeMounts[]; .mountPath == "/tmp"))) and
    all(.items[].status.containerStatuses[]?; .name != "runtime" or .restartCount == 0)
  '
}

verify_nvidia() {
  local required_json=(
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
  local file
  verify_common_qualification
  for file in "${required_json[@]}"; do
    require_json "${file}"
  done
  require_file resources.txt
  require_file events.txt

  assert_json identities.json "trusted commit, canonical immutable images, model, and observed GPU facts" '
    .commit == $commit and .llamaCommit == $llamaCommit and
    .llamaBuildNumber == $llamaBuild and
    .model.revision == $modelRevision and .model.sha256 == $modelDigest and
    .model.size == $modelSize and
    (.images.manager | test("^ghcr\\.io/tannerburns/kama-manager@sha256:[a-f0-9]{64}$"))
  ' --arg commit "${expected_commit}" \
    --arg llamaCommit "${llama_commit}" \
    --arg llamaBuild "${llama_build_number}" \
    --arg modelRevision "${model_revision}" \
    --arg modelDigest "${model_digest}" \
    --argjson modelSize "${model_size}"
  assert_json identities.json "all canonical image roles and verified CUDA runtime" '
    (.images.importer | test("^ghcr\\.io/tannerburns/kama-importer@sha256:[a-f0-9]{64}$")) and
    (.images.servingClient | test("^ghcr\\.io/tannerburns/kama-test-fixtures@sha256:[a-f0-9]{64}$")) and
    (.images.runtimeCPU | test("^ghcr\\.io/tannerburns/kama-runtime-cpu@sha256:[a-f0-9]{64}$")) and
    (.images.runtimeCUDA | test("^ghcr\\.io/tannerburns/kama-runtime-cuda@sha256:[a-f0-9]{64}$")) and
    .nvidia.expectedDriverVersion == .nvidia.observedDriverVersion and
    .nvidia.expectedCUDAVersion == $cuda and .nvidia.observedCUDAVersion == $cuda and
    (.nvidia.observedDevice | type == "string" and length > 0) and
    (.nvidia.observedDeviceUUID | type == "string" and length > 0) and
    .runtimeImageProvenance.verified == true and
    .runtimeImageProvenance.sourceCheckoutClean == true and
    .runtimeImageProvenance.revision == $commit and
    .runtimeImageProvenance.llamaCommit == $llamaCommit and
    .runtimeImageProvenance.cudaVersion == $cuda and
    (.runtimeImageProvenance.expectedObservedManifestDigest | test("^sha256:[a-f0-9]{64}$"))
  ' --arg commit "${expected_commit}" --arg llamaCommit "${llama_commit}" --arg cuda "${cuda_version}"
  assert_json image-provenance.json "five provenance-bound production images" '
    .sourceCheckoutClean == true and (.images | length) == 5 and
    ([.images[].role] | unique | sort) == ["importer","manager","runtimeCPU","runtimeCUDA","servingClient"] and
    all(.images[]; .verified == true and .revision == $commit and
      .source == "https://github.com/TannerBurns/kama") and
    all(.images[] | select(.role == "runtimeCPU" or .role == "runtimeCUDA");
      .llamaCommit == $llamaCommit and .llamaBuildNumber == $llamaBuild and
      .llamaSourceSHA256 == $llamaSource) and
    all(.images[] | select(.role == "runtimeCUDA"); .cudaVersion == $cuda)
  ' --arg commit "${expected_commit}" \
    --arg llamaCommit "${llama_commit}" \
    --arg llamaBuild "${llama_build_number}" \
    --arg llamaSource "${llama_source_sha256}" \
    --arg cuda "${cuda_version}"
  assert_json supply-chain.json "cosign signatures and SPDX attestations for every image" '
    .verified == true and (.images | length) == 5 and
    ([.images[].role] | unique | length) == 5 and
    all(.images[]; .signatureVerified == true and .sbomAttestationVerified == true)
  '
  assert_json qualification.json "NVIDIA supply chain and supported Kubernetes minor" '
    .supplyChainVerified == true and .kubernetesMinorVerified == true
  '
  assert_json kubernetes-version.json "supported Kubernetes API server minor" '
    .serverVersion.major == "1" and
    ((.serverVersion.minor | sub("[+]$"; "")) | IN("34", "35", "36"))
  '
  assert_json direct-request.log "complete NVIDIA SSE response" '
    .schemaVersion == 1 and .sseDataEvents > 0 and
    (.generatedContentFragments | type) == "number" and .generatedContentFragments > 0 and
    (.generatedContentBytes | type) == "number" and .generatedContentBytes > 0 and .done == true
  '
  assert_json direct-request.json "one-GPU in-cluster Service request" '
    .transport == "in-cluster ClusterIP Service DNS" and
    .route == "/v1/chat/completions" and .stream == true and .completed == true and
    .generatedContentObserved == true and
    (.generatedContentFragments | type) == "number" and .generatedContentFragments > 0 and
    (.generatedContentBytes | type) == "number" and .generatedContentBytes > 0 and
    (.serviceDNS | type == "string" and length > 0) and
    (.gpuDevice | type == "string" and length > 0) and
    (.gpuUUID | type == "string" and length > 0) and
    .cudaVersion == $cuda and .restartCount == 0
  ' --arg cuda "${cuda_version}"
  assert_json client-pod.json "immutable restricted serving client Pod" '
    .requestedImage == $image and
    (.resolvedImage | type == "string" and endswith("@" + $resolvedDigest)) and
    .ready == false and .succeeded == true and .restartCount == 0 and
    .automountServiceAccountToken == false and .runAsNonRoot == true and
    .runAsUser == 65532 and .runAsGroup == 65532 and .seccompProfile == "RuntimeDefault" and
    .allowPrivilegeEscalation == false and .readOnlyRootFilesystem == true and
    .capabilitiesDropAll == true
  ' --arg image "$(jq -r '.images.servingClient' "${evidence_dir}/identities.json")" \
    --arg resolvedDigest "$(jq -r '.images[] | select(.role == "servingClient").resolvedDigest' \
      "${evidence_dir}/image-provenance.json")"
  assert_json cuda-runtime.json "installed and linked CUDA 12.4.1 runtime" '
    .expectedVersion == $cuda and .declaredVersion == $cuda and .observedVersion == $cuda and
    .versionMetadata.cuda.version == $cuda and
    any(.packages[]; test("^cuda-cudart-12-4(:amd64)?[\\t ]+12\\.4\\.")) and
    any(.linkedLibraries[]; test("libcudart\\.so\\.12[[:space:]]+=>[[:space:]]+/")) and
    any(.linkedLibraries[]; test("libcublas\\.so\\.12[[:space:]]+=>[[:space:]]+/"))
  ' --arg cuda "${cuda_version}"
  assert_json modeldeployment.json "current artifact, object, runtime, and fingerprint status" '
    .status.observedGeneration == .metadata.generation and
    .status.desiredReplicas == 1 and .status.readyReplicas == 1 and
    .status.artifact.name == "e2e-serving-nvidia-model" and
    (.status.artifact.uid | type == "string" and length > 0) and
    .status.artifact.digest == $modelDigest and
    (.status.deploymentRef.uid | type == "string" and length > 0) and
    (.status.serviceRef.uid | type == "string" and length > 0) and
    .status.runtime.state == "Ready" and .status.runtime.desiredImage == $image and
    (.status.runtime.observedImage | test("@sha256:[a-f0-9]{64}$")) and
    .status.runtime.desiredFingerprint == .status.runtime.observedFingerprint and
    .status.runtime.desiredFingerprint == .status.runtime.loadedFingerprint and
    .status.runtime.effectiveContextTokens == 2048 and
    .status.runtime.effectiveConcurrency == 1 and .status.runtime.acceleratorDetected == true and
    .status.runtime.offloadedLayers > 0 and
    any(.status.conditions[]; .type == "RuntimeReady" and .status == "True") and
    any(.status.conditions[]; .type == "Serving" and .status == "True") and
    any(.status.conditions[]; .type == "Degraded" and .status == "False")
  ' --arg modelDigest "${model_digest}" \
    --arg image "$(jq -r '.images.runtimeCUDA' "${evidence_dir}/identities.json")"
  assert_json workload.json "immutable one-replica restricted CUDA workload" '
    .metadata.name == $name and .metadata.uid == $uid and
    .spec.replicas == 1 and .spec.strategy.type == "Recreate" and
    .spec.template.metadata.annotations["kama.tannerburns.github.io/runtime-fingerprint-full"] == $fingerprint and
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.securityContext.runAsUser == 65532 and
    .spec.template.spec.securityContext.runAsGroup == 65532 and
    .spec.template.spec.securityContext.fsGroup == 65532 and
    .spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
    ([.spec.template.spec.containers[] | select(.name == "runtime" and
      .image == $image and .resources.requests["nvidia.com/gpu"] == "1" and
      .resources.limits["nvidia.com/gpu"] == "1" and
      .securityContext.readOnlyRootFilesystem == true and
      .securityContext.allowPrivilegeEscalation == false and
      (.securityContext.capabilities.drop | index("ALL") != null))] | length) == 1
  ' --arg name "$(jq -r '.status.deploymentRef.name' "${evidence_dir}/modeldeployment.json")" \
    --arg uid "$(jq -r '.status.deploymentRef.uid' "${evidence_dir}/modeldeployment.json")" \
    --arg fingerprint "$(jq -r '.status.runtime.desiredFingerprint' "${evidence_dir}/modeldeployment.json")" \
    --arg image "$(jq -r '.images.runtimeCUDA' "${evidence_dir}/identities.json")"
  assert_json pods.json "single ready current NVIDIA Pod with no restart" '
    (.items | length) == 1 and .items[0] as $pod |
    ($pod.metadata.uid | type == "string" and length > 0) and
    ($pod.spec.nodeName | type == "string" and length > 0) and
    any($pod.status.conditions[]; .type == "Ready" and .status == "True") and
    any($pod.status.containerStatuses[]; .name == "runtime" and .ready == true and
      .restartCount == 0 and .imageID == $image)
  ' --arg image "$(jq -r '.status.runtime.observedImage' "${evidence_dir}/modeldeployment.json")"
  assert_json nodes.json "runtime Pod scheduled to the recorded allocatable GPU node" '
    any(.items[]; .name == $node and ((.gpuCount // "0") | tonumber) > 0 and
      (.gpuProduct | type == "string" and length > 0))
  ' --arg node "$(jq -r '.items[0].spec.nodeName' "${evidence_dir}/pods.json")"
  assert_json service.json "stable ClusterIP serving Service" '
    .metadata.name == $name and .metadata.uid == $uid and .spec.type == "ClusterIP" and
    ([.spec.ports[] | select(.port == 8080 and .targetPort == 8080)] | length) == 1 and
    ([.spec.ports[] | select(.port == 8081 or .targetPort == 8081)] | length) == 0
  ' --arg name "$(jq -r '.status.serviceRef.name' "${evidence_dir}/modeldeployment.json")" \
    --arg uid "$(jq -r '.status.serviceRef.uid' "${evidence_dir}/modeldeployment.json")"
  assert_json endpointslices.json "one ready endpoint for the current NVIDIA Pod" '
    ([.items[].endpoints[]? | select(.conditions.ready == true and .targetRef.kind == "Pod" and
      .targetRef.uid == $podUID)] | length) == 1
  ' --arg podUID "$(jq -r '.items[0].metadata.uid' "${evidence_dir}/pods.json")"
  assert_json supervisor-state.json "GPU offload bound to current artifact and fingerprint" '
    .schemaVersion == "kama.runtime/v1alpha1" and .phase == "Ready" and .ready == true and
    .deployment.fingerprint == $fingerprint and .artifact.uid == $artifactUID and
    .artifact.digest == $modelDigest and .runtime.mode == "Accelerator" and
    .runtime.effectiveContextTokens == 2048 and .runtime.desiredConcurrency == 1 and
    .runtime.llamaCPPCommit == $llamaCommit and .runtime.llamaCPPBuildNumber == $llamaBuild and
    .runtime.acceleratorDetected == true and .runtime.visibleAccelerators == 1 and
    .runtime.offloadedLayers > 0 and .runtime.totalLayers == .runtime.offloadedLayers and
    (.runtime.acceleratorDevice | type == "string" and length > 0)
  ' --arg fingerprint "$(jq -r '.status.runtime.desiredFingerprint' "${evidence_dir}/modeldeployment.json")" \
    --arg artifactUID "$(jq -r '.status.artifact.uid' "${evidence_dir}/modeldeployment.json")" \
    --arg modelDigest "${model_digest}" \
    --arg llamaCommit "${llama_commit}" \
    --arg llamaBuild "${llama_build_number}"
}

if [[ "${suite}" == "cpu" ]]; then
  verify_cpu
else
  verify_nvidia
fi

echo "M2 ${suite} acceptance evidence is complete and internally consistent"
