#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
helper="${repo_root}/hack/verify-published-release-image.sh"
fixture="${repo_root}/hack/testdata/release-image-index.json"
parent_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
image_ref="ghcr.io/tannerburns/kama-manager:fixture"
immutable_ref="ghcr.io/tannerburns/kama-manager@${parent_digest}"

docker() {
  if [[ "${1:-}" != "buildx" || "${2:-}" != "imagetools" || "${3:-}" != "inspect" ]]; then
    echo "unexpected docker invocation: $*" >&2
    return 2
  fi
  if [[ "${4:-}" == "--raw" && "${5:-}" == "${immutable_ref}" ]]; then
    case "${MOCK_MANIFEST_MODE:-valid}" in
      valid)
        command cat "${fixture}"
        ;;
      missing-provenance)
        jq 'del(.manifests[-1])' "${fixture}"
        ;;
      duplicate-platform)
        jq '.manifests += [.manifests[0]]' "${fixture}"
        ;;
      extra-runnable)
        jq '.manifests += [{
          mediaType: "application/vnd.oci.image.manifest.v1+json",
          digest: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
          size: 1000,
          platform: {architecture: "amd64", os: "windows"}
        }]' "${fixture}"
        ;;
      *)
        echo "unexpected manifest mode: ${MOCK_MANIFEST_MODE}" >&2
        return 2
        ;;
    esac
  elif [[ "${4:-}" == "${image_ref}" && $# -eq 4 ]]; then
    printf 'Name: %s\nMediaType: application/vnd.oci.image.index.v1+json\nDigest: %s\n' \
      "${image_ref}" "${MOCK_PUBLISHED_DIGEST:-${parent_digest}}"
  else
    echo "unexpected docker invocation: $*" >&2
    return 2
  fi
}
export -f docker
export fixture image_ref immutable_ref parent_digest

bash "${helper}" "${image_ref}" "${parent_digest}" \
  '["linux/amd64","linux/arm64"]'

for mode in missing-provenance duplicate-platform extra-runnable; do
  if MOCK_MANIFEST_MODE="${mode}" bash "${helper}" "${image_ref}" "${parent_digest}" \
    '["linux/amd64","linux/arm64"]' >/dev/null 2>&1; then
    echo "${mode} release index was accepted" >&2
    exit 1
  fi
done

if MOCK_PUBLISHED_DIGEST="sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" \
  bash "${helper}" "${image_ref}" "${parent_digest}" \
    '["linux/amd64","linux/arm64"]' >/dev/null 2>&1; then
  echo "mismatched published digest was accepted" >&2
  exit 1
fi

if bash "${helper}" "${image_ref}" "not-a-digest" \
  '["linux/amd64","linux/arm64"]' >/dev/null 2>&1; then
  echo "invalid build output digest was accepted" >&2
  exit 1
fi

echo "published release image verification regression tests passed"
