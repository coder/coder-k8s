#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

DEFAULT_CLUSTER_NAME="coder-k8s-dev"
if [[ -n "${MUX_WORKSPACE_NAME:-}" ]]; then
	DEFAULT_CLUSTER_NAME="coder-k8s-${MUX_WORKSPACE_NAME}"
fi

CLUSTER_NAME="${CLUSTER_NAME:-${DEFAULT_CLUSTER_NAME}}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE="${NAMESPACE:-coder-system}"
DEPLOYMENT="${DEPLOYMENT:-coder-k8s}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.34.0}"
IMAGE="${IMAGE:-ghcr.io/coder/coder-k8s:e2e}"
GOARCH="${GOARCH:-}"
NODE_READY_TIMEOUT="${NODE_READY_TIMEOUT:-300s}"

require_cmd() {
	local cmd="${1}"
	if ! command -v "${cmd}" >/dev/null 2>&1; then
		echo "assertion failed: missing required command: ${cmd}" >&2
		exit 1
	fi
}

kubectl_ctx() {
	require_cmd kubectl
	kubectl --context "${KUBE_CONTEXT}" "$@"
}

ensure_cluster() {
	require_cmd kind
	if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
		echo "assertion failed: kind cluster ${CLUSTER_NAME} does not exist (run: ./hack/kind-dev.sh up)" >&2
		exit 1
	fi
}

build_binary() {
	local resolved_goarch="${GOARCH}"
	if [[ -z "${resolved_goarch}" ]]; then
		resolved_goarch="$(go env GOARCH)"
		if [[ -z "${resolved_goarch}" ]]; then
			echo "assertion failed: go env GOARCH returned an empty value" >&2
			exit 1
		fi
	fi

	GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=linux GOARCH="${resolved_goarch}" go build -o coder-k8s ./
}

build_and_load_image() {
	docker build -f Dockerfile.goreleaser -t "${IMAGE}" .
	kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"
}

cmd_up() {
	require_cmd kind
	require_cmd kubectl

	if [[ -z "${KIND_NODE_IMAGE}" ]]; then
		echo "assertion failed: KIND_NODE_IMAGE must not be empty" >&2
		exit 1
	fi

	if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
		kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}"
	fi

	echo "Using KIND node image: ${KIND_NODE_IMAGE}"

	kind export kubeconfig --name "${CLUSTER_NAME}" >/dev/null
	kubectl config use-context "${KUBE_CONTEXT}" >/dev/null

	kubectl_ctx wait --for=condition=Ready node --all --timeout="${NODE_READY_TIMEOUT}"

	kubectl_ctx apply -f config/crd/bases/
	kubectl_ctx apply -f config/rbac/

	kubectl_ctx apply -f config/e2e/namespace.yaml
	kubectl_ctx apply -f config/e2e/serviceaccount.yaml
	kubectl_ctx apply -f config/e2e/clusterrole-binding.yaml

	cmd_ctx

	echo
	echo "KIND cluster bootstrapped for coder-k8s."
	echo
	echo "Run controller locally (out-of-cluster):"
	echo "  GOFLAGS=-mod=vendor go run . --app=controller"
	echo
	echo "Or deploy controller in-cluster:"
	echo "  ./hack/kind-dev.sh load-image"
	echo "  kubectl apply -f config/e2e/deployment.yaml"
	echo "  kubectl wait --for=condition=Available deploy/${DEPLOYMENT} -n ${NAMESPACE} --timeout=120s"
	echo
	echo "Demo with k9s:"
	echo "  ./hack/kind-dev.sh k9s"
}

cmd_ctx() {
	require_cmd kind
	require_cmd kubectl

	ensure_cluster
	kind export kubeconfig --name "${CLUSTER_NAME}" >/dev/null
	kubectl config use-context "${KUBE_CONTEXT}" >/dev/null
	echo "Using kubectl context: ${KUBE_CONTEXT}"
}

cmd_load_image() {
	require_cmd kind
	require_cmd docker
	require_cmd go

	ensure_cluster
	build_binary
	build_and_load_image

	echo "Loaded ${IMAGE} into cluster ${CLUSTER_NAME}."
}

cmd_k9s() {
	require_cmd k9s
	ensure_cluster
	exec k9s --context "${KUBE_CONTEXT}"
}

cmd_status() {
	require_cmd kubectl
	ensure_cluster

	kubectl_ctx get nodes -o wide
	kubectl_ctx get codercontrolplanes -A || true
	kubectl_ctx -n "${NAMESPACE}" get deploy,pods -o wide || true
}

cmd_down() {
	require_cmd kind
	kind delete cluster --name "${CLUSTER_NAME}"
}

usage() {
	cat <<'EOF' >&2
usage: ./hack/kind-dev.sh {up|ctx|load-image|k9s|status|down}
EOF
	exit 2
}

case "${1:-}" in
up)
	cmd_up
	;;
ctx | context | use-context)
	cmd_ctx
	;;
load-image)
	cmd_load_image
	;;
k9s)
	cmd_k9s
	;;
status)
	cmd_status
	;;
down)
	cmd_down
	;;
*)
	usage
	;;
esac
