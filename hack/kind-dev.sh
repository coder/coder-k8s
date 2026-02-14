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

ensure_cluster_node_image_matches() {
	local control_plane_container expected_image actual_image expected_image_id actual_image_id

	require_cmd docker

	expected_image="${KIND_NODE_IMAGE}"
	if [[ -z "${expected_image}" ]]; then
		echo "assertion failed: KIND_NODE_IMAGE must not be empty" >&2
		exit 1
	fi

	control_plane_container="${CLUSTER_NAME}-control-plane"
	if ! docker inspect "${control_plane_container}" >/dev/null 2>&1; then
		echo "assertion failed: expected kind control-plane container ${control_plane_container} for existing cluster ${CLUSTER_NAME}" >&2
		exit 1
	fi

	actual_image="$(docker inspect --format '{{.Config.Image}}' "${control_plane_container}")"
	if [[ -z "${actual_image}" ]]; then
		echo "assertion failed: kind control-plane container ${control_plane_container} has an empty image value" >&2
		exit 1
	fi

	if [[ "${actual_image}" == "${expected_image}" ]]; then
		return
	fi

	expected_image_id="$(docker image inspect --format '{{.ID}}' "${expected_image}" 2>/dev/null || true)"
	actual_image_id="$(docker image inspect --format '{{.ID}}' "${actual_image}" 2>/dev/null || true)"
	if [[ -n "${expected_image_id}" && -n "${actual_image_id}" && "${expected_image_id}" == "${actual_image_id}" ]]; then
		return
	fi

	echo "assertion failed: existing cluster ${CLUSTER_NAME} uses node image ${actual_image}, but KIND_NODE_IMAGE=${expected_image}. Recreate the cluster to apply a different node image (run: ./hack/kind-dev.sh down && KIND_NODE_IMAGE=${expected_image} ./hack/kind-dev.sh up)" >&2
	exit 1
}

assert_no_aggregation_resource_conflict() {
	local has_apiservice="false"
	local has_template_crd="false"
	local has_workspace_crd="false"

	if kubectl_ctx get apiservice v1alpha1.aggregation.coder.com >/dev/null 2>&1; then
		has_apiservice="true"
	fi
	if kubectl_ctx get crd codertemplates.aggregation.coder.com >/dev/null 2>&1; then
		has_template_crd="true"
	fi
	if kubectl_ctx get crd coderworkspaces.aggregation.coder.com >/dev/null 2>&1; then
		has_workspace_crd="true"
	fi

	if [[ "${has_apiservice}" == "true" && ( "${has_template_crd}" == "true" || "${has_workspace_crd}" == "true" ) ]]; then
		echo "assertion failed: detected aggregation API conflict in ${KUBE_CONTEXT}: APIService v1alpha1.aggregation.coder.com and aggregation.coder.com CRDs are both installed." >&2
		echo "Delete conflicting CRDs before aggregated API demos:" >&2
		echo "  kubectl --context ${KUBE_CONTEXT} delete crd codertemplates.aggregation.coder.com coderworkspaces.aggregation.coder.com" >&2
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

	local resolved_goarm=""
	local target_platform="linux/${resolved_goarch}"
	if [[ "${resolved_goarch}" == "arm" ]]; then
		resolved_goarm="${GOARM:-}"
		if [[ -z "${resolved_goarm}" ]]; then
			resolved_goarm="$(go env GOARM)"
			if [[ -z "${resolved_goarm}" ]]; then
				echo "assertion failed: go env GOARM returned an empty value for GOARCH=arm" >&2
				exit 1
			fi
		fi
		target_platform="linux/arm/v${resolved_goarm}"
	fi

	mkdir -p "${target_platform}"
	GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=linux GOARCH="${resolved_goarch}" GOARM="${resolved_goarm}" go build -o "${target_platform}/coder-k8s" ./
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

	if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
		ensure_cluster_node_image_matches
		echo "Using existing KIND cluster ${CLUSTER_NAME} with node image: ${KIND_NODE_IMAGE}"
	else
		kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}"
		echo "Created KIND cluster ${CLUSTER_NAME} with node image: ${KIND_NODE_IMAGE}"
	fi

	kind export kubeconfig --name "${CLUSTER_NAME}" >/dev/null
	kubectl config use-context "${KUBE_CONTEXT}" >/dev/null

	kubectl_ctx wait --for=condition=Ready node --all --timeout="${NODE_READY_TIMEOUT}"
	assert_no_aggregation_resource_conflict

	kubectl_ctx apply -f config/e2e/namespace.yaml
	# config/crd/bases intentionally contains only operator-owned coder.com CRDs.
	kubectl_ctx apply -f config/crd/bases/
	kubectl_ctx apply -f config/rbac/

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
