#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ ! -d "${SCRIPT_ROOT}/api/v1alpha1" ]]; then
	echo "assertion failed: expected API package at ${SCRIPT_ROOT}/api/v1alpha1" >&2
	exit 1
fi

if [[ ! -d "${SCRIPT_ROOT}/api/aggregation/v1alpha1" ]]; then
	echo "assertion failed: expected API package at ${SCRIPT_ROOT}/api/aggregation/v1alpha1" >&2
	exit 1
fi

INPUT_PKG="$(cd "${SCRIPT_ROOT}" && GOFLAGS=-mod=vendor go list ./api/v1alpha1)"
if [[ -z "${INPUT_PKG}" ]]; then
	echo "assertion failed: go list returned empty package path for ./api/v1alpha1" >&2
	exit 1
fi

AGGREGATION_INPUT_PKG="$(cd "${SCRIPT_ROOT}" && GOFLAGS=-mod=vendor go list ./api/aggregation/v1alpha1)"
if [[ -z "${AGGREGATION_INPUT_PKG}" ]]; then
	echo "assertion failed: go list returned empty package path for ./api/aggregation/v1alpha1" >&2
	exit 1
fi

cd "${SCRIPT_ROOT}"
GOFLAGS=-mod=vendor go run ./vendor/k8s.io/code-generator/cmd/deepcopy-gen \
	--output-file zz_generated.deepcopy.go \
	--go-header-file /dev/null \
	"${INPUT_PKG}" \
	"${AGGREGATION_INPUT_PKG}"
