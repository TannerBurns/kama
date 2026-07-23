def kama_nvidia_immutable_image_id($requestedParentDigest; $resolvedLinuxAMD64Digest):
  type == "string" and
  test("@sha256:[a-f0-9]{64}$") and
  ($requestedParentDigest | test("^sha256:[a-f0-9]{64}$")) and
  ($resolvedLinuxAMD64Digest | test("^sha256:[a-f0-9]{64}$")) and
  (endswith("@" + $requestedParentDigest) or
    endswith("@" + $resolvedLinuxAMD64Digest));
