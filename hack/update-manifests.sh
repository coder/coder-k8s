#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

write_kustomization_file() {
	local manifests_dir="${1:-}"

	if [[ -z "${manifests_dir}" ]]; then
		echo "assertion failed: expected manifest directory argument" >&2
		exit 1
	fi

	if [[ ! -d "${SCRIPT_ROOT}/${manifests_dir}" ]]; then
		echo "assertion failed: expected manifest directory at ${SCRIPT_ROOT}/${manifests_dir}" >&2
		exit 1
	fi

	local -a resource_files=()
	mapfile -t resource_files < <(
		find "${SCRIPT_ROOT}/${manifests_dir}" -maxdepth 1 -mindepth 1 -type f -name '*.yaml' ! -name 'kustomization.yaml' -printf '%f\n' | sort
	)

	if [[ "${#resource_files[@]}" -eq 0 ]]; then
		echo "assertion failed: expected at least one manifest in ${SCRIPT_ROOT}/${manifests_dir}" >&2
		exit 1
	fi

	{
		echo "apiVersion: kustomize.config.k8s.io/v1beta1"
		echo "kind: Kustomization"
		echo "resources:"
		for resource_file in "${resource_files[@]}"; do
			echo "  - ${resource_file}"
		done
	} > "${SCRIPT_ROOT}/${manifests_dir}/kustomization.yaml"
}

if [[ ! -d "${SCRIPT_ROOT}/api/v1alpha1" ]]; then
	echo "assertion failed: expected API package at ${SCRIPT_ROOT}/api/v1alpha1" >&2
	exit 1
fi

if [[ ! -d "${SCRIPT_ROOT}/internal/controller" ]]; then
	echo "assertion failed: expected controller package at ${SCRIPT_ROOT}/internal/controller" >&2
	exit 1
fi

cd "${SCRIPT_ROOT}"

# Generate CRDs for operator-owned coder.com APIs only.
GOFLAGS=-mod=vendor go run ./vendor/sigs.k8s.io/controller-tools/cmd/controller-gen \
	crd:crdVersions=v1 \
	paths=./api/v1alpha1 \
	output:crd:artifacts:config=config/crd/bases

# Generate RBAC across the repo.
GOFLAGS=-mod=vendor go run ./vendor/sigs.k8s.io/controller-tools/cmd/controller-gen \
	rbac:roleName=manager-role \
	paths=./... \
	output:rbac:artifacts:config=config/rbac

write_kustomization_file "config/crd/bases"
write_kustomization_file "config/rbac"
