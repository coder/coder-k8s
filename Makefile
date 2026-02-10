GOFLAGS ?= -mod=vendor
VENDOR_STAMP := vendor/modules.txt
MODULE_FILES := go.mod $(wildcard go.sum)
ENVTEST_K8S_VERSION ?= 1.35.x
ENVTEST_ASSETS_DIR := $(shell pwd)/bin/envtest

.PHONY: vendor test test-integration setup-envtest build lint vuln verify-vendor codegen manifests docs-reference docs-reference-check docs-serve docs-build docs-check update-coder-docs-skill kind-dev-up kind-dev-ctx kind-dev-load-image kind-dev-status kind-dev-k9s kind-dev-down

$(VENDOR_STAMP): $(MODULE_FILES)
	go mod tidy
	go mod vendor

vendor: $(VENDOR_STAMP)

setup-envtest:
	GOFLAGS=-mod=vendor go run ./vendor/sigs.k8s.io/controller-runtime/tools/setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path > /dev/null

test: $(VENDOR_STAMP) setup-envtest
	KUBEBUILDER_ASSETS="$$(GOFLAGS=-mod=vendor go run ./vendor/sigs.k8s.io/controller-runtime/tools/setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path)" \
	GOFLAGS=$(GOFLAGS) go test ./...

test-integration: $(VENDOR_STAMP) setup-envtest
	KUBEBUILDER_ASSETS="$$(GOFLAGS=-mod=vendor go run ./vendor/sigs.k8s.io/controller-runtime/tools/setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path)" \
	GOFLAGS=$(GOFLAGS) go test ./internal/controller/... -count=1 -v

build: $(VENDOR_STAMP)
	GOFLAGS=$(GOFLAGS) go build ./...

lint: $(VENDOR_STAMP)
	@command -v golangci-lint >/dev/null || (echo "golangci-lint not found; use nix develop" && exit 1)
	GOFLAGS=$(GOFLAGS) golangci-lint run ./...
	GOFLAGS=$(GOFLAGS) golangci-lint fmt --diff

vuln: $(VENDOR_STAMP)
	@command -v govulncheck >/dev/null || (echo "govulncheck not found; use nix develop" && exit 1)
	GOFLAGS=$(GOFLAGS) govulncheck ./...

verify-vendor:
	go mod tidy
	go mod vendor
	git diff --exit-code -- go.mod go.sum vendor/

manifests: $(VENDOR_STAMP)
	bash ./hack/update-manifests.sh

codegen: $(VENDOR_STAMP)
	bash ./hack/update-codegen.sh


docs-reference: $(VENDOR_STAMP)
	bash ./hack/update-reference-docs.sh

docs-reference-check: docs-reference
	git diff --exit-code -- docs/reference/api/

docs-serve:
	@command -v mkdocs >/dev/null || (echo "mkdocs not found; use nix develop" && exit 1)
	mkdocs serve

docs-build:
	@command -v mkdocs >/dev/null || (echo "mkdocs not found; use nix develop" && exit 1)
	mkdocs build

docs-check: docs-reference-check
	@command -v mkdocs >/dev/null || (echo "mkdocs not found; use nix develop" && exit 1)
	mkdocs build --strict

update-coder-docs-skill:
	bash ./hack/update-coder-docs-skill.sh

kind-dev-up:
	./hack/kind-dev.sh up

kind-dev-ctx:
	./hack/kind-dev.sh ctx

kind-dev-load-image:
	./hack/kind-dev.sh load-image

kind-dev-status:
	./hack/kind-dev.sh status

kind-dev-k9s:
	./hack/kind-dev.sh k9s

kind-dev-down:
	./hack/kind-dev.sh down
