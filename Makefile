GOFLAGS ?= -mod=vendor
VENDOR_STAMP := vendor/.modules.stamp
MODULE_FILES := go.mod $(wildcard go.sum)

.PHONY: vendor test build verify-vendor codegen

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

verify-vendor:
	go mod tidy
	go mod vendor
	git diff --exit-code -- go.mod go.sum vendor/

codegen: $(VENDOR_STAMP)
	bash ./hack/update-codegen.sh
