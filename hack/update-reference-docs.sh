#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRD_REF_DOCS_PKG="./vendor/github.com/elastic/crd-ref-docs"

assert_dir() {
	local path="$1"
	if [[ ! -d "${path}" ]]; then
		echo "assertion failed: expected directory at ${path}" >&2
		exit 1
	fi
}

assert_file() {
	local path="$1"
	if [[ ! -f "${path}" ]]; then
		echo "assertion failed: expected file at ${path}" >&2
		exit 1
	fi
}

assert_dir "${SCRIPT_ROOT}/api/v1alpha1"
assert_dir "${SCRIPT_ROOT}/api/aggregation/v1alpha1"
assert_dir "${SCRIPT_ROOT}/docs/reference/api"
assert_dir "${SCRIPT_ROOT}/hack/crd-ref-docs/templates/markdown"
assert_dir "${SCRIPT_ROOT}/vendor/github.com/elastic/crd-ref-docs"
assert_file "${SCRIPT_ROOT}/hack/crd-ref-docs/config.yaml"
assert_file "${SCRIPT_ROOT}/hack/crd-ref-docs/templates/markdown/gv_list.tpl"

generate_kind_doc() {
	local source_path="$1"
	local output_path="$2"
	local kind="$3"
	shift 3

	GOFLAGS=-mod=vendor go run "${CRD_REF_DOCS_PKG}" \
		--renderer=markdown \
		--config="${SCRIPT_ROOT}/hack/crd-ref-docs/config.yaml" \
		--templates-dir="${SCRIPT_ROOT}/hack/crd-ref-docs/templates/markdown" \
		--source-path="${SCRIPT_ROOT}/${source_path}" \
		--output-mode=single \
		--output-path="${SCRIPT_ROOT}/${output_path}" \
		--template-value="kind=${kind}" \
		--template-value="scope=namespaced" \
		"$@"
}

cd "${SCRIPT_ROOT}"

generate_kind_doc "api/v1alpha1" "docs/reference/api/codercontrolplane.md" "CoderControlPlane" \
	--template-value="goType=api/v1alpha1/codercontrolplane_types.go" \
	--template-value="generatedCRD=config/crd/bases/coder.com_codercontrolplanes.yaml"

generate_kind_doc "api/aggregation/v1alpha1" "docs/reference/api/coderworkspace.md" "CoderWorkspace" \
	--template-value="goType=api/aggregation/v1alpha1/types.go" \
	--template-value="storage=internal/aggregated/storage/workspace.go" \
	--template-value="apiServiceManifest=deploy/apiserver-apiservice.yaml"

generate_kind_doc "api/aggregation/v1alpha1" "docs/reference/api/codertemplate.md" "CoderTemplate" \
	--template-value="goType=api/aggregation/v1alpha1/types.go" \
	--template-value="storage=internal/aggregated/storage/template.go" \
	--template-value="apiServiceManifest=deploy/apiserver-apiservice.yaml"
