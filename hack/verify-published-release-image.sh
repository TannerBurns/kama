#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 IMAGE_REF EXPECTED_DIGEST EXPECTED_PLATFORMS_JSON" >&2
  exit 2
fi

image_ref=$1
expected_digest=$2
expected_platforms=$3

if [[ ! "${expected_digest}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
  echo "build output is not an immutable SHA-256 digest" >&2
  exit 1
fi
if ! expected_platforms="$(jq -cer '
  select(
    type == "array" and
    length > 0 and
    length == (unique | length) and
    all(.[]; test("^linux/(amd64|arm64)$"))
  ) |
  sort
' <<<"${expected_platforms}")"; then
  echo "expected platform set is invalid" >&2
  exit 1
fi

inspect_output="$(docker buildx imagetools inspect "${image_ref}")"
published_digest="$(awk '$1 == "Digest:" { print $2; exit }' <<<"${inspect_output}")"
if [[ ! "${published_digest}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
  echo "could not resolve the published digest for ${image_ref}" >&2
  exit 1
fi
if [[ "${published_digest}" != "${expected_digest}" ]]; then
  echo "published digest for ${image_ref} is ${published_digest}; expected ${expected_digest}" >&2
  exit 1
fi

image_repository="${image_ref%:*}"
if [[ "${image_repository}" == "${image_ref}" || "${image_repository}" != ghcr.io/* ]]; then
  echo "release image reference must be a tagged GHCR repository" >&2
  exit 1
fi
immutable_ref="${image_repository}@${expected_digest}"
manifest="$(docker buildx imagetools inspect --raw "${immutable_ref}")"
published_platforms="$(jq -cer '
  [.manifests[] |
    select(((.annotations // {})["vnd.docker.reference.type"] // "") != "attestation-manifest") |
    ((.platform.os // "") + "/" + (.platform.architecture // ""))] |
  sort
' <<<"${manifest}")"
if [[ "${published_platforms}" != "${expected_platforms}" ]]; then
  echo "published platforms for ${image_ref} are ${published_platforms}; expected ${expected_platforms}" >&2
  exit 1
fi

if ! jq -e '
  . as $index |
  [.manifests[] |
    select(((.annotations // {})["vnd.docker.reference.type"] // "") != "attestation-manifest")
  ] as $runnable |
  [$runnable[] as $platform |
    any($index.manifests[];
        .platform.os == "unknown" and
        .platform.architecture == "unknown" and
        ((.annotations // {})["vnd.docker.reference.type"] == "attestation-manifest") and
        ((.annotations // {})["vnd.docker.reference.digest"] == $platform.digest)
      )] |
  all
' >/dev/null <<<"${manifest}"; then
  echo "published index for ${image_ref} does not attest every platform manifest" >&2
  exit 1
fi
