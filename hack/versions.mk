# Copyright 2026 Kama Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

GO_VERSION := 1.26.5
KUBEBUILDER_VERSION := v4.15.0
CONTROLLER_RUNTIME_VERSION := v0.24.1
KUBERNETES_LIB_VERSION := v0.36.0

KUSTOMIZE_VERSION := v5.8.1
CONTROLLER_TOOLS_VERSION := v0.21.0
GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION := v1.6.0
GO_LICENSES_VERSION := v2.0.1
ACTIONLINT_VERSION := v1.7.12
KIND_VERSION := v0.32.0
HELM_VERSION := v4.2.0
SYFT_VERSION := v1.44.0
COSIGN_VERSION := v3.0.6
KEDA_VERSION := 2.20.0
NFS_CSI_VERSION := 4.13.4
LLAMA_CPP_VERSION := b9445
LLAMA_CPP_BUILD_NUMBER := 9445
LLAMA_CPP_COMMIT := af6528e6df5d798f7f1363ec1141699be0f638e2
LLAMA_CPP_SOURCE_SHA256 := 8bb78d0331d7be27fae4321977eb5f3c686af85cae13c74e6a6b9150e90e4f18
CUDA_VERSION := 12.4.1
# Test-only mirror used by KubeVirt CDI. Pin the schema-v2 manifest digest;
# sha256:79f203... is this image's config digest and is not pullable directly.
NFS_SERVER_IMAGE := quay.io/awels/nfs-server-alpine:12@sha256:7fa99ae65c23c5af87dd4300e543a86b119ed15ba61422444207efc7abd0ba20

KIND_NODE_IMAGE_1.34 := kindest/node:v1.34.8@sha256:02722c2dedddcfc00febf5d27fbeb9b7b2c14294c82109ff4a85d89ac9ba3256
KIND_NODE_IMAGE_1.35 := kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95
KIND_NODE_IMAGE_1.36 := kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5
