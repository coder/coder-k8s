GOFLAGS ?= -mod=vendor
VENDOR_STAMP := vendor/.modules.stamp
MODULE_FILES := go.mod $(wildcard go.sum)

.PHONY: vendor test build lint vuln verify-vendor codegen

$(VENDOR_STAMP): $(MODULE_FILES)
	go mod tidy
	go mod vendor
	@mkdir -p $(dir $@)
	@touch $@

vendor: $(VENDOR_STAMP)

test: $(VENDOR_STAMP)
	GOFLAGS=$(GOFLAGS) go test ./...

build: $(VENDOR_STAMP)
	GOFLAGS=$(GOFLAGS) go build ./...

lint: $(VENDOR_STAMP)
	@command -v golangci-lint >/dev/null || (echo "golangci-lint not found; use nix develop" && exit 1)
	GOFLAGS=$(GOFLAGS) golangci-lint run ./...

vuln: $(VENDOR_STAMP)
	@command -v govulncheck >/dev/null || (echo "govulncheck not found; use nix develop" && exit 1)
	GOFLAGS=$(GOFLAGS) govulncheck ./...

verify-vendor:
	go mod tidy
	go mod vendor
	git diff --exit-code -- go.mod go.sum vendor/

codegen: $(VENDOR_STAMP)
	bash ./hack/update-codegen.sh
