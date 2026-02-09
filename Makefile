GOFLAGS ?= -mod=vendor
VENDOR_STAMP := vendor/.modules.stamp
MODULE_FILES := go.mod $(wildcard go.sum)
ENVTEST_K8S_VERSION ?= 1.35.x
ENVTEST_ASSETS_DIR := $(shell pwd)/bin/envtest
SETUP_ENVTEST_VERSION ?= release-0.23

.PHONY: vendor test test-integration setup-envtest build lint vuln verify-vendor codegen manifests

$(VENDOR_STAMP): $(MODULE_FILES)
	go mod tidy
	go mod vendor
	@mkdir -p $(dir $@)
	@touch $@

vendor: $(VENDOR_STAMP)

setup-envtest:
	GOFLAGS= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path > /dev/null

test: $(VENDOR_STAMP) setup-envtest
	KUBEBUILDER_ASSETS="$$(GOFLAGS= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path)" \
	GOFLAGS=$(GOFLAGS) go test ./...

test-integration: $(VENDOR_STAMP) setup-envtest
	KUBEBUILDER_ASSETS="$$(GOFLAGS= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path)" \
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
