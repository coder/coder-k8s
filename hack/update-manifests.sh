#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ ! -d "${SCRIPT_ROOT}/api/v1alpha1" ]]; then
	echo "assertion failed: expected API package at ${SCRIPT_ROOT}/api/v1alpha1" >&2
	exit 1
fi

if [[ ! -d "${SCRIPT_ROOT}/internal/controller" ]]; then
	echo "assertion failed: expected controller package at ${SCRIPT_ROOT}/internal/controller" >&2
	exit 1
fi

cd "${SCRIPT_ROOT}"
GOFLAGS=-mod=vendor go run ./vendor/sigs.k8s.io/controller-tools/cmd/controller-gen \
	crd:crdVersions=v1 \
	rbac:roleName=manager-role \
	paths=./... \
	output:crd:artifacts:config=config/crd/bases \
	output:rbac:artifacts:config=config/rbac
