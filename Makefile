# Copyright 2026 Kama Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

include hack/versions.mk

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := build

VERSION := $(shell tr -d '[:space:]' < VERSION)
MODULE := github.com/TannerBurns/kama
REGISTRY ?= ghcr.io/tannerburns
IMG ?= $(REGISTRY)/kama-manager:$(VERSION)
IMPORTER_IMG ?= $(REGISTRY)/kama-importer:$(VERSION)
FIXTURES_IMG ?= $(REGISTRY)/kama-test-fixtures:$(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64
CONTAINER_TOOL ?= docker
LOCALBIN ?= $(CURDIR)/bin
DIST ?= $(CURDIR)/dist
COPYRIGHT_YEAR ?= 2026
K8S_MINOR ?= 1.36
KIND_CLUSTER ?= kama-$(subst .,-,$(K8S_MINOR))
KIND_NODE_IMAGE ?= $(KIND_NODE_IMAGE_$(K8S_MINOR))

GO ?= go
GOFLAGS ?=
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

KUSTOMIZE := $(LOCALBIN)/kustomize
CONTROLLER_GEN := $(LOCALBIN)/controller-gen
KUBEBUILDER := $(LOCALBIN)/kubebuilder
ENVTEST := $(LOCALBIN)/setup-envtest
GOLANGCI_LINT := $(LOCALBIN)/golangci-lint
GOVULNCHECK := $(LOCALBIN)/govulncheck
GO_LICENSES := $(LOCALBIN)/go-licenses
ACTIONLINT := $(LOCALBIN)/actionlint
KIND := $(LOCALBIN)/kind
HELM := $(LOCALBIN)/helm
SYFT := $(LOCALBIN)/syft
COSIGN := $(LOCALBIN)/cosign

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display the available make targets.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: bootstrap
bootstrap: kubebuilder controller-gen envtest kustomize golangci-lint govulncheck go-licenses actionlint kind helm ## Install pinned core development and CI tools into bin/.

##@ Generation and quality

.PHONY: manifests
manifests: controller-gen ## Generate M1 RBAC, webhook, and CRD manifests.
	@mkdir -p config/crd/bases config/rbac config/webhook charts/kama/crds
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook \
		'paths=./api/...;./internal/controller/...' \
		output:rbac:artifacts:config=config/rbac \
		output:crd:artifacts:config=config/crd/bases \
		output:webhook:artifacts:config=config/webhook
	install -m 0644 config/crd/bases/kama.tannerburns.github.io_modelcaches.yaml \
		charts/kama/crds/kama.tannerburns.github.io_modelcaches.yaml
	install -m 0644 config/crd/bases/kama.tannerburns.github.io_modelartifacts.yaml \
		charts/kama/crds/kama.tannerburns.github.io_modelartifacts.yaml

.PHONY: generate
generate: controller-gen ## Generate Go API helpers.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(COPYRIGHT_YEAR) paths="./api/..."

.PHONY: fmt
fmt: ## Format Go source files.
	@files="$$(find . -type f -name '*.go' -not -path './bin/*' -not -path './dist/*')"; test -z "$$files" || gofmt -w $$files

.PHONY: fmt-check
fmt-check: ## Fail when Go source is not formatted.
	@files="$$(find . -type f -name '*.go' -not -path './bin/*' -not -path './dist/*' -print0 | xargs -0 -r gofmt -l)"; \
	if [[ -n "$$files" ]]; then printf 'Go files require formatting:\n%s\n' "$$files"; exit 1; fi

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Apply safe linter fixes.
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Validate the golangci-lint configuration.
	"$(GOLANGCI_LINT)" config verify

.PHONY: vuln-check
vuln-check: govulncheck ## Scan reachable Go code for known vulnerabilities.
	"$(GOVULNCHECK)" ./...

.PHONY: license-check
license-check: go-licenses ## Check dependency licenses and reject forbidden, restricted, or unknown licenses.
	"$(GO_LICENSES)" check ./... --disallowed_types=forbidden,restricted,unknown

.PHONY: workflow-check
workflow-check: actionlint ## Validate GitHub Actions workflows.
	"$(ACTIONLINT)" -color

.PHONY: chart-sync
chart-sync: helm ## Verify packaged chart metadata and its default image tag are synchronized with VERSION.
	VERSION="$(VERSION)" HELM="$(HELM)" DIST="$(DIST)" bash hack/release-check.sh

.PHONY: verify-generated
verify-generated: generate manifests chart-sync ## Regenerate artifacts, verify chart synchronization, and reject drift.
	@git diff --exit-code
	@test -z "$$(git status --porcelain --untracked-files=all)" || { git status --short; exit 1; }

##@ Tests

.PHONY: test
test: ## Run race-enabled unit tests and write coverage data.
	@mkdir -p "$(DIST)"
	$(GO) test -race ./... -coverprofile "$(DIST)/coverage.out"

.PHONY: test-envtest
test-envtest: build setup-envtest ## Start a real envtest control plane and verify manager lifecycle and probes.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use 1.36 --bin-dir "$(LOCALBIN)" -p path)" \
	KAMA_MANAGER_BINARY="$(LOCALBIN)/manager" \
	$(GO) test -race -tags=integration ./test/integration -v

.PHONY: test-kind
test-kind: kind helm ## Run the Helm, admission, KEDA, fixture, and uninstall smoke suite on Kind.
	@test -n "$(KIND_NODE_IMAGE)" || { echo "unsupported K8S_MINOR=$(K8S_MINOR)"; exit 2; }
	KIND="$(KIND)" HELM="$(HELM)" K8S_MINOR="$(K8S_MINOR)" KIND_CLUSTER="$(KIND_CLUSTER)" \
	KIND_NODE_IMAGE="$(KIND_NODE_IMAGE)" KEDA_VERSION="$(KEDA_VERSION)" VERSION="$(VERSION)" \
	IMG="$(IMG)" IMPORTER_IMG="$(IMPORTER_IMG)" FIXTURES_IMG="$(FIXTURES_IMG)" bash hack/test-kind.sh

##@ Build and packaging

.PHONY: build
build: ## Build the manager, importer, and test fixture binaries.
	@mkdir -p "$(LOCALBIN)"
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o "$(LOCALBIN)/manager" ./cmd
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o "$(LOCALBIN)/kama-importer" ./cmd/importer
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o "$(LOCALBIN)/fake-llama-server" ./cmd/fake-llama-server
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o "$(LOCALBIN)/fake-huggingface-server" ./cmd/fake-huggingface-server
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o "$(LOCALBIN)/external-scaler" ./cmd/external-scaler

.PHONY: run
run: ## Run the empty manager against the current kubeconfig.
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd

.PHONY: container
container: ## Build versioned manager, importer, and test-fixture container images.
	$(CONTAINER_TOOL) build --build-arg VERSION="$(VERSION)" -t "$(IMG)" -f Dockerfile .
	$(CONTAINER_TOOL) build --build-arg VERSION="$(VERSION)" -t "$(IMPORTER_IMG)" -f Dockerfile.importer .
	$(CONTAINER_TOOL) build --build-arg VERSION="$(VERSION)" -t "$(FIXTURES_IMG)" -f Dockerfile.test-fixtures .

.PHONY: container-multiarch
container-multiarch: ## Build and push multi-architecture images (release workflow only).
	$(CONTAINER_TOOL) buildx build --push --platform "$(PLATFORMS)" --build-arg VERSION="$(VERSION)" -t "$(IMG)" -f Dockerfile .
	$(CONTAINER_TOOL) buildx build --push --platform "$(PLATFORMS)" --build-arg VERSION="$(VERSION)" -t "$(IMPORTER_IMG)" -f Dockerfile.importer .
	$(CONTAINER_TOOL) buildx build --push --platform "$(PLATFORMS)" --build-arg VERSION="$(VERSION)" -t "$(FIXTURES_IMG)" -f Dockerfile.test-fixtures .

.PHONY: helm-validate
helm-validate: helm ## Lint and render the chart for every supported Kubernetes minor.
	"$(HELM)" lint charts/kama
	@for minor in 1.34 1.35 1.36; do "$(HELM)" template kama charts/kama --namespace kama-system --kube-version "$$minor.0" >/dev/null; done

.PHONY: helm-package
helm-package: helm helm-validate ## Package the chart with version and appVersion sourced from VERSION.
	VERSION="$(VERSION)" HELM="$(HELM)" DIST="$(DIST)" bash hack/helm-package.sh

.PHONY: supply-chain-tools
supply-chain-tools: syft cosign ## Install pinned SBOM and signing tools.

.PHONY: sbom
sbom: syft ## Generate SPDX JSON SBOMs for all local images.
	@mkdir -p "$(DIST)/sbom"
	"$(SYFT)" "docker:$(IMG)" -o "spdx-json=$(DIST)/sbom/kama-manager.spdx.json"
	"$(SYFT)" "docker:$(IMPORTER_IMG)" -o "spdx-json=$(DIST)/sbom/kama-importer.spdx.json"
	"$(SYFT)" "docker:$(FIXTURES_IMG)" -o "spdx-json=$(DIST)/sbom/kama-test-fixtures.spdx.json"

.PHONY: sign
sign: cosign ## Sign an immutable OCI reference supplied as IMAGE_DIGEST.
	@test -n "$${IMAGE_DIGEST:-}" || { echo "set IMAGE_DIGEST to an immutable image@sha256 reference"; exit 2; }
	"$(COSIGN)" sign --yes "$${IMAGE_DIGEST}"

.PHONY: release-check
release-check: build helm-package ## Verify VERSION, binaries, Dockerfile pins, and packaged chart metadata agree.
	CHECK_BINARY=1 VERSION="$(VERSION)" IMG="$(IMG)" IMPORTER_IMG="$(IMPORTER_IMG)" FIXTURES_IMG="$(FIXTURES_IMG)" HELM="$(HELM)" DIST="$(DIST)" bash hack/release-check.sh

.PHONY: verify
verify: fmt-check vet lint test test-envtest vuln-check license-check workflow-check helm-validate release-check verify-generated ## Run all non-container verification gates.

##@ Tool installation

$(LOCALBIN):
	@mkdir -p "$(LOCALBIN)"

.PHONY: kubebuilder
kubebuilder: $(KUBEBUILDER)
$(KUBEBUILDER): | $(LOCALBIN)
	$(call go-install-tool,$(KUBEBUILDER),sigs.k8s.io/kubebuilder/v4,$(KUBEBUILDER_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): | $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): | $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(CONTROLLER_RUNTIME_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest
	"$(ENVTEST)" use 1.36 --bin-dir "$(LOCALBIN)" >/dev/null

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): | $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): | $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK)
$(GOVULNCHECK): | $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

.PHONY: go-licenses
go-licenses: $(GO_LICENSES)
$(GO_LICENSES): | $(LOCALBIN)
	$(call go-install-tool,$(GO_LICENSES),github.com/google/go-licenses/v2,$(GO_LICENSES_VERSION))

.PHONY: actionlint
actionlint: $(ACTIONLINT)
$(ACTIONLINT): | $(LOCALBIN)
	$(call go-install-tool,$(ACTIONLINT),github.com/rhysd/actionlint/cmd/actionlint,$(ACTIONLINT_VERSION))

.PHONY: kind
kind: $(KIND)
$(KIND): | $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: helm
helm: $(HELM)
$(HELM): | $(LOCALBIN)
	$(call go-install-tool,$(HELM),helm.sh/helm/v4/cmd/helm,$(HELM_VERSION))

.PHONY: syft
syft: $(SYFT)
$(SYFT): | $(LOCALBIN)
	$(call go-install-tool,$(SYFT),github.com/anchore/syft/cmd/syft,$(SYFT_VERSION))

.PHONY: cosign
cosign: $(COSIGN)
$(COSIGN): | $(LOCALBIN)
	$(call go-install-tool,$(COSIGN),github.com/sigstore/cosign/v3/cmd/cosign,$(COSIGN_VERSION))

define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
	echo "Installing $(2)@$(3)"; \
	GOBIN="$(LOCALBIN)" $(GO) install "$(2)@$(3)"; \
	mv "$(1)" "$(1)-$(3)"; \
}; \
ln -sf "$$(basename "$(1)-$(3)")" "$(1)"
endef
