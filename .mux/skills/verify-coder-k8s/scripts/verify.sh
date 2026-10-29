#!/usr/bin/env bash
# verify-coder-k8s: launch the real coder-k8s operator on a disposable KIND cluster with a real
# Coder, drive it through kubectl, capture evidence, and tear down only what this run created.
# Usage: see .mux/skills/verify-coder-k8s/SKILL.md, or run with no arguments.
set -euo pipefail

SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SELF="${SKILL_DIR}/scripts/verify.sh"
ROOT="$(cd "${SKILL_DIR}/../../.." && pwd)"
cd "${ROOT}"

KIND_VERSION=v0.26.0               # = helm/kind-action v1.12.0 in the CI job "E2E (Kind + CNPG + Templates)"
KUBECTL_VERSION=v1.31.4            # = the kubectl of helm/kind-action v1.12.0
KIND_NODE_IMAGE=kindest/node:v1.32.0 # kind v0.26.0 default; `kind load` fails on v1.34.0 (containerd 2.1)
E2E_IMAGE=ghcr.io/coder/coder-k8s:e2e # config/e2e/deployment.yaml (imagePullPolicy: Never)
MARKER_NS=kube-system
MARKER_CM=verify-coder-k8s-run
NS=coder
OP_NS=coder-system
CCP=coder
APISERVICE=v1alpha1.aggregation.coder.com

# private_dir DIR creates DIR (mode 0700) if needed, then refuses it unless this
# user owns it and nobody else can write it: BIN goes first on PATH, and
# run.env is sourced, so a directory that another user can write would let
# that user run code as this one.
private_dir() {
	(umask 077 && mkdir -p -- "$1")
	safe_ancestors "$1"
	local owner mode
	read -r owner mode < <(stat -c '%u %a' -- "$1")
	if [[ ${owner} != "$(id -u)" ]] || (((8#${mode} & 8#022) != 0)); then
		printf 'verify-coder-k8s: refusing %s: owner uid %s, mode %s (need this user and no group or other write)\n' "$1" "${owner}" "${mode}" >&2
		exit 1
	fi
}

# safe_ancestors DIR refuses DIR when another user could rename or replace it:
# every ancestor, as spelled and after symlinks, must be owned by root or this
# user, and a directory that others can write must be sticky (like /tmp).
safe_ancestors() {
	local real p owner mode type
	real=$(realpath -e -- "$1")
	for p in "$1" "${real}"; do
		while :; do
			read -r owner mode type < <(stat -c '%u %a %F' -- "${p}")
			if [[ ${owner} != 0 && ${owner} != "$(id -u)" ]] ||
				{ [[ ${type} == directory ]] && (((8#${mode} & 8#022) != 0 && (8#${mode} & 8#1000) == 0)); }; then
				printf 'verify-coder-k8s: refusing %s: ancestor %s has owner uid %s, mode %s\n' "$1" "${p}" "${owner}" "${mode}" >&2
				exit 1
			fi
			[[ ${p} == / ]] && break
			p=$(dirname -- "${p}")
		done
	done
}

# Never a shared /tmp default: see private_dir.
VERIFY_BASE="${VERIFY_BASE:-${XUM_SCRATCH_DIR:-${XDG_CACHE_HOME:-${HOME}/.cache}}/verify-coder-k8s}"
BIN="${VERIFY_BASE}/bin"
private_dir "${VERIFY_BASE}"
private_dir "${BIN}"
export PATH="${BIN}:${PATH}"
# A private Go build cache: the shared ~/.cache/go-build on this host can be damaged ("no such file").
export GOCACHE="${GOCACHE:-${VERIFY_BASE}/gocache}"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() {
	printf 'verify-coder-k8s: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat >&2 <<'EOF'
usage: VERIFY_RUN_DIR=<new or existing run dir> verify.sh <command>

  launch                         empty state -> usable state (KIND, image, CRDs/RBAC, operator, Postgres, CoderControlPlane)
  doctor                         read-only health check of this run's cluster
  kubectl <args...>              kubectl against this run's cluster only (refuses any other cluster)
  node <cmd...>                  run a command in this run's KIND node container (docker exec), e.g. ctr or crictl
  coder-api <METHOD> <PATH> [BODY_FILE]
                                 Coder REST call as the operator user (port-forward per call; the token never prints)
  capture <label> -- <cmd...>    run a command and keep its output in $VERIFY_RUN_DIR/evidence/
  collect                        snapshot pods, events, CRs and logs into $VERIFY_RUN_DIR/evidence/
  cleanup                        delete only what this run created; evidence stays
EOF
	exit 2
}

require_run_dir() {
	[[ -n ${VERIFY_RUN_DIR:-} ]] || die "set VERIFY_RUN_DIR to this run's directory (absolute path)"
	[[ ${VERIFY_RUN_DIR} == /* ]] || die "VERIFY_RUN_DIR must be an absolute path: ${VERIFY_RUN_DIR}"
	RUN_DIR="${VERIFY_RUN_DIR}"
	EVIDENCE="${RUN_DIR}/evidence"
	KCFG="${RUN_DIR}/kubeconfig"
	SECRETS="${RUN_DIR}/secrets"
}

load_run() {
	require_run_dir
	[[ -f ${RUN_DIR}/run.env ]] || die "no run.env in ${RUN_DIR}: run launch first"
	private_dir "${RUN_DIR}"
	[[ -O ${RUN_DIR}/run.env ]] || die "refusing ${RUN_DIR}/run.env: another user owns it"
	# shellcheck disable=SC1091
	source "${RUN_DIR}/run.env"
	[[ -n ${RUN_ID:-} && -n ${CLUSTER:-} ]] || die "assertion failed: run.env lacks RUN_ID or CLUSTER"
	NODE="${CLUSTER}-control-plane"
	CTX="kind-${CLUSTER}"
	install_tools # The pinned kind and kubectl, never the host's.
}

record() { printf '%s=%q\n' "$1" "$2" >>"${RUN_DIR}/run.env"; }

k() { kubectl --kubeconfig "${KCFG}" --context "${CTX}" --request-timeout=60s "$@"; }
kw() { kubectl --kubeconfig "${KCFG}" --context "${CTX}" "$@"; } # waits and streams: no request timeout

# owned: true only for the KIND cluster that this run created and marked.
owned() {
	[[ ${CLUSTER_CREATED:-0} == 1 ]] || return 1
	kind get clusters 2>/dev/null | grep -qx "${CLUSTER}" || return 1
	[[ $(docker inspect -f '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "${NODE}" 2>/dev/null) == "${CLUSTER}" ]] || return 1
	[[ -n ${NODE_ID:-} && $(docker inspect -f '{{.Id}}' "${NODE}" 2>/dev/null) == "${NODE_ID}" ]] || return 1
	[[ $(k -n "${MARKER_NS}" get configmap "${MARKER_CM}" -o jsonpath='{.data.run}' 2>/dev/null) == "${RUN_ID}" ]]
}
guard() {
	load_run
	owned || die "refusing: cluster ${CLUSTER} is missing or was not created by run ${RUN_ID} (marker ${MARKER_NS}/${MARKER_CM})"
}

install_tools() {
	[[ $(uname -s)/$(uname -m) == Linux/x86_64 ]] || die "assertion failed: this skill downloads linux-amd64 tools, host is $(uname -s)/$(uname -m)"
	mkdir -p "${BIN}"
	if ! "${BIN}/kind" version 2>/dev/null | grep -q "kind ${KIND_VERSION} "; then
		log "downloading kind ${KIND_VERSION} into ${BIN}"
		local url="https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-amd64"
		curl -fsSL --retry 3 -o "${BIN}/kind.tmp" "${url}"
		echo "$(curl -fsSL --retry 3 "${url}.sha256sum" | awk '{print $1}')  ${BIN}/kind.tmp" | sha256sum -c - >/dev/null
		chmod +x "${BIN}/kind.tmp" && mv "${BIN}/kind.tmp" "${BIN}/kind"
	fi
	if ! "${BIN}/kubectl" version --client 2>/dev/null | grep -q "Client Version: ${KUBECTL_VERSION}$"; then
		log "downloading kubectl ${KUBECTL_VERSION} into ${BIN}"
		local url="https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
		curl -fsSL --retry 3 -o "${BIN}/kubectl.tmp" "${url}"
		echo "$(curl -fsSL --retry 3 "${url}.sha256")  ${BIN}/kubectl.tmp" | sha256sum -c - >/dev/null
		chmod +x "${BIN}/kubectl.tmp" && mv "${BIN}/kubectl.tmp" "${BIN}/kubectl"
	fi
	[[ $(command -v kind) == "${BIN}/kind" && $(command -v kubectl) == "${BIN}/kubectl" ]] ||
		die "assertion failed: private kind/kubectl are not first on PATH"
	for c in docker go jq curl git; do command -v "$c" >/dev/null || die "missing required command: $c"; done
}

# image_present <ref>: true when the host Docker has the image.
image_present() { docker image inspect "$1" >/dev/null 2>&1; }

STEP_NAME=""
STEP_T0=0
step() {
	if [[ -n ${STEP_NAME} ]]; then printf '%s\t%ss\n' "${STEP_NAME}" "$((SECONDS - STEP_T0))" >>"${RUN_DIR}/launch-steps.tsv"; fi
	STEP_NAME="$1"
	STEP_T0=${SECONDS}
	[[ $1 == finish ]] || log "=== launch: $1"
}

cmd_launch() {
	require_run_dir
	[[ ! -e ${RUN_DIR}/run.env ]] || die "refusing: ${RUN_DIR} already holds a run (run.env); use a new VERIFY_RUN_DIR"
	private_dir "${RUN_DIR}"
	mkdir -p "${EVIDENCE}"
	exec > >(tee -a "${RUN_DIR}/launch.log") 2>&1
	local t_all=${SECONDS}

	step "tools (kind ${KIND_VERSION}, kubectl ${KUBECTL_VERSION})"
	install_tools

	step "run identity"
	RUN_ID="vck-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
	CLUSTER="${RUN_ID}"
	NODE="${CLUSTER}-control-plane"
	CTX="kind-${CLUSTER}"
	IMAGE_TAG="coder-k8s-verify:${RUN_ID}"
	! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}" || die "assertion failed: cluster ${CLUSTER} already exists"
	CODER_IMAGE=$(awk '/^ *CODER_E2E_IMAGE:/ {print $2; exit}' .github/workflows/ci.yaml)
	CODER_VERSION=$(awk '/^ *E2E_EXPECT_CODER_VERSION:/ {print $2; exit}' .github/workflows/ci.yaml)
	[[ ${CODER_IMAGE} == ghcr.io/coder/coder:*@sha256:* && -n ${CODER_VERSION} ]] ||
		die "assertion failed: cannot read CODER_E2E_IMAGE/E2E_EXPECT_CODER_VERSION from .github/workflows/ci.yaml"
	: >"${RUN_DIR}/run.env"
	chmod 600 "${RUN_DIR}/run.env"
	record RUN_ID "${RUN_ID}"
	record CLUSTER "${CLUSTER}"
	record IMAGE_TAG "${IMAGE_TAG}"
	record CODER_IMAGE "${CODER_IMAGE}"
	record CODER_VERSION "${CODER_VERSION}"
	record SOURCE_SHA "$(git rev-parse HEAD)"
	record SOURCE_DIRTY "$(git status --porcelain | wc -l | tr -d ' ')" # untracked files too: go build ./ reads them
	record STARTED_AT "$(date -u +%FT%TZ)"
	local pulled=()
	image_present "${KIND_NODE_IMAGE}" || pulled+=("${KIND_NODE_IMAGE}")
	image_present gcr.io/distroless/static:nonroot || pulled+=(gcr.io/distroless/static:nonroot)
	record PULLED_IMAGES "${pulled[*]}"
	log "run ${RUN_ID}: cluster ${CLUSTER}, image ${IMAGE_TAG}, Coder ${CODER_IMAGE}"

	step "KIND cluster + namespace coder-system + CRDs + RBAC (hack/kind-dev.sh up)"
	record CLUSTER_CREATED 1 # from here on, cleanup owns the cluster name
	CLUSTER_CREATED=1
	local up_rc=0
	KUBECONFIG="${KCFG}" CLUSTER_NAME="${CLUSTER}" KIND_NODE_IMAGE="${KIND_NODE_IMAGE}" ./hack/kind-dev.sh up || up_rc=$?
	# The node container ID binds cleanup to this run's cluster, not to its name.
	record NODE_ID "$(docker inspect -f '{{.Id}}' "${NODE}" 2>/dev/null || true)"
	NODE_ID=$(docker inspect -f '{{.Id}}' "${NODE}" 2>/dev/null || true)
	((up_rc == 0)) || die "hack/kind-dev.sh up failed (exit ${up_rc})"
	chmod 600 "${KCFG}"
	k -n "${MARKER_NS}" create configmap "${MARKER_CM}" --from-literal=run="${RUN_ID}" \
		--from-literal=sourceSHA="$(git rev-parse HEAD)" >/dev/null
	owned || die "assertion failed: the new cluster ${CLUSTER} fails the ownership check"

	step "build the operator binary and image ${IMAGE_TAG}"
	local arch
	arch=$(go env GOARCH)
	mkdir -p "${RUN_DIR}/build/linux/${arch}"
	GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -o "${RUN_DIR}/build/linux/${arch}/coder-k8s" ./
	docker build -q -f Dockerfile.goreleaser -t "${IMAGE_TAG}" "${RUN_DIR}/build" >/dev/null
	BUILT_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "${IMAGE_TAG}")
	record BUILT_IMAGE_ID "${BUILT_IMAGE_ID}"
	log "built ${IMAGE_TAG} = ${BUILT_IMAGE_ID}"

	step "load the image into the node as ${E2E_IMAGE}"
	if ! kind load docker-image "${IMAGE_TAG}" --name "${CLUSTER}"; then
		log "kind load failed; importing with ctr instead"
		docker save "${IMAGE_TAG}" | docker exec -i "${NODE}" ctr -n k8s.io images import --digests -
	fi
	docker exec "${NODE}" ctr -n k8s.io images tag --force "docker.io/library/${IMAGE_TAG}" "${E2E_IMAGE}" >/dev/null
	[[ $(docker exec "${NODE}" crictl inspecti -o json "${E2E_IMAGE}" | jq -r .status.id) == "${BUILT_IMAGE_ID}" ]] ||
		die "assertion failed: ${E2E_IMAGE} on ${NODE} is not ${BUILT_IMAGE_ID}"

	step "operator Deployment (--app=all) + aggregated APIService"
	k apply -f deploy/apiserver-service.yaml -f deploy/apiserver-apiservice.yaml
	k apply -f config/e2e/deployment.yaml
	kw wait --for=condition=Available "deploy/coder-k8s" -n "${OP_NS}" --timeout=180s
	kw wait --for=condition=Available "apiservice/${APISERVICE}" --timeout=180s
	local want="" have="" insecure="" i
	for i in $(seq 1 60); do
		want=$(k -n "${OP_NS}" get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)
		have=$(k get apiservice "${APISERVICE}" -o jsonpath='{.spec.caBundle}')
		insecure=$(k get apiservice "${APISERVICE}" -o jsonpath='{.spec.insecureSkipTLSVerify}')
		[[ -n ${want} && ${have} == "${want}" && -z ${insecure} ]] && break
		sleep 2
	done
	[[ -n ${want} && ${have} == "${want}" && -z ${insecure} ]] || die "assertion failed: APIService caBundle never matched the serving CA (try ${i})"
	for i in $(seq 1 30); do
		if k get --raw /apis/aggregation.coder.com/v1alpha1 >/dev/null 2>&1; then break; fi
		sleep 2
	done
	k get --raw /apis/aggregation.coder.com/v1alpha1 >/dev/null

	step "PostgreSQL (emptyDir) in namespace coder"
	k apply -f examples/cloudnativepg/00-namespace.yaml
	k apply -f "${SKILL_DIR}/manifests/postgres.yaml"
	kw -n "${NS}" rollout status deployment/postgres --timeout=300s

	step "CoderControlPlane coder (examples/cloudnativepg/codercontrolplane.yaml, CI overrides)"
	# Same overrides as the CI step "Provision PostgreSQL and CoderControlPlane", minus the Terraform
	# mirror, plus Guaranteed QoS for Coder.
	k create --dry-run=client -o json -f examples/cloudnativepg/codercontrolplane.yaml |
		jq -e --arg image "${CODER_IMAGE}" '.spec.image = $image
			| if [.spec.extraEnv[] | select(.name == "CODER_ACCESS_URL")] | length == 1 then . else error("expected one CODER_ACCESS_URL") end
			| .spec.extraEnv |= map(if .name == "CODER_ACCESS_URL" then .value = "http://coder.coder.svc.cluster.local" else . end)
			| .spec.resources = {requests: {cpu: "1", memory: "2Gi"}, limits: {cpu: "1", memory: "2Gi"}}' \
			>"${EVIDENCE}/launch-codercontrolplane.json"
	k apply -f "${EVIDENCE}/launch-codercontrolplane.json"
	kw -n "${NS}" wait --for=condition=DatabaseSecretResolved "codercontrolplane/${CCP}" --timeout=180s
	kw -n "${NS}" rollout status deployment/coder --timeout=600s
	kw -n "${NS}" wait --for=jsonpath='{.status.phase}'=Ready "codercontrolplane/${CCP}" --timeout=600s
	kw -n "${NS}" wait --for=jsonpath='{.status.operatorAccessReady}'=true "codercontrolplane/${CCP}" --timeout=600s

	step "usable: Coder answers the operator token with version ${CODER_VERSION}"
	local version
	version=$("${SELF}" coder-api GET /api/v2/buildinfo | jq -er .version)
	[[ ${version} == "${CODER_VERSION}" || ${version} == "${CODER_VERSION}+"* ]] ||
		die "assertion failed: Coder reports ${version}, want ${CODER_VERSION}"

	step finish
	record READY_AT "$(date -u +%FT%TZ)"
	log "launch ready in $((SECONDS - t_all))s: export VERIFY_RUN_DIR=${RUN_DIR}"
	cat "${RUN_DIR}/launch-steps.tsv" >&2
}

DOCTOR_FAIL=0
check() { # <label> <command...>: prints PASS or FAIL; never stops early
	local label=$1
	shift
	local out
	if out=$("$@" 2>&1); then
		printf 'PASS  %s%s\n' "${label}" "${out:+ ($out)}"
	else
		printf 'FAIL  %s%s\n' "${label}" "${out:+ ($out)}"
		DOCTOR_FAIL=1
	fi
}
doc_owned() { owned && echo "run=${RUN_ID} node=${NODE}"; }
doc_image() {
	k -n "${OP_NS}" get pods -l app=coder-k8s -o json >"${RUN_DIR}/doctor-pods.json"
	jq -e '[.items[] | select(.metadata.deletionTimestamp == null)] | length == 1 and
		(.[0].status.conditions // [] | any(.type == "Ready" and .status == "True"))' "${RUN_DIR}/doctor-pods.json" >/dev/null ||
		{ echo "want exactly one Ready non-terminating operator pod"; return 1; }
	local ref digest id
	ref=$(jq -er '[.items[] | select(.metadata.deletionTimestamp == null)][0].status.containerStatuses[0].imageID' "${RUN_DIR}/doctor-pods.json")
	digest=${ref##*@}
	docker exec "${NODE}" crictl inspecti -o json "${E2E_IMAGE}" >"${RUN_DIR}/doctor-node-image.json"
	id=$(jq -er .status.id "${RUN_DIR}/doctor-node-image.json")
	[[ ${id} == "${BUILT_IMAGE_ID}" ]] || { echo "node ${E2E_IMAGE}=${id}, built ${BUILT_IMAGE_ID}"; return 1; }
	jq -e --arg d "${digest}" '[.status.repoDigests[]? | sub("^.*@"; "")] | any(. == $d)' "${RUN_DIR}/doctor-node-image.json" >/dev/null ||
		{ echo "pod digest ${digest} is not a digest of ${E2E_IMAGE}"; return 1; }
	echo "image ${BUILT_IMAGE_ID} pod ${digest}"
}
doc_crds() {
	local f name missing=() files=(config/crd/bases/*.yaml)
	for f in "${files[@]}"; do
		name=$(awk '/^  name:/ {print $2; exit}' "$f")
		[[ $(k get crd "${name}" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null) == True ]] || missing+=("${name}")
	done
	((${#missing[@]} == 0)) || { echo "not established: ${missing[*]}"; return 1; }
	echo "${#files[@]} established"
}
doc_apiservice() {
	[[ $(k get apiservice "${APISERVICE}" -o jsonpath='{.status.conditions[?(@.type=="Available")].status}') == True ]] || { echo "not Available"; return 1; }
	local want=(codertemplates codertemplates/promote codertemplateversions coderworkspaces coderworkspaces/log coderworkspaces/start coderworkspaces/stop) have missing=() r
	have=$(k get --raw /apis/aggregation.coder.com/v1alpha1 | jq -r '.resources[].name') || { echo "no discovery document"; return 1; }
	for r in "${want[@]}"; do grep -qx -- "${r}" <<<"${have}" || missing+=("${r}"); done
	((${#missing[@]} == 0)) || { echo "discovery lacks: ${missing[*]}"; return 1; }
	paste -sd, <<<"${have}"
}
doc_ccp() {
	k -n "${NS}" get codercontrolplane "${CCP}" -o jsonpath='{.status.phase} {.status.operatorAccessReady}' | grep -qx 'Ready true' ||
		{ k -n "${NS}" get codercontrolplane "${CCP}" -o jsonpath='phase={.status.phase} operatorAccessReady={.status.operatorAccessReady}'; return 1; }
	k -n "${NS}" get codercontrolplane "${CCP}" -o jsonpath='phase={.status.phase} operatorAccessReady={.status.operatorAccessReady} url={.status.url}'
}
doc_coder() {
	local v
	v=$("${SELF}" coder-api GET /api/v2/buildinfo 2>/dev/null | jq -er .version) || { echo "no buildinfo"; return 1; }
	[[ ${v} == "${CODER_VERSION}" || ${v} == "${CODER_VERSION}+"* ]] || { echo "version ${v}, want ${CODER_VERSION}"; return 1; }
	echo "version ${v}"
}
cmd_doctor() {
	load_run
	check "cluster ${CLUSTER} exists, is labeled, and carries this run's marker" doc_owned
	if ((DOCTOR_FAIL)); then
		echo "doctor: FAIL (not this run's cluster; nothing else checked)"
		return 1
	fi
	check "operator pod runs the built image" doc_image
	check "coder.com CRDs from config/crd/bases" doc_crds
	check "APIService ${APISERVICE} Available and serving" doc_apiservice
	check "CoderControlPlane ${NS}/${CCP} Ready with operator access" doc_ccp
	check "Coder API answers the operator token" doc_coder
	if ((DOCTOR_FAIL)); then
		echo "doctor: FAIL"
		return 1
	fi
	echo "doctor: PASS"
}

cmd_kubectl() {
	local arg
	for arg in "$@"; do
		# -s takes its value attached too (-shttps://...), as pflag allows.
		[[ ! $arg =~ ^(--?(context|cluster|user|kubeconfig|server)(=|$)|-s) ]] ||
			die "refusing ${arg}: this helper only talks to the run's cluster"
	done
	guard
	kw "$@"
}

cmd_node() {
	(($# >= 1)) || die "usage: node <cmd...>"
	guard
	docker exec -i "${NODE}" "$@"
}

cmd_coder_api() {
	(($# >= 2 && $# <= 3)) || die "usage: coder-api <METHOD> <PATH> [BODY_FILE]"
	guard
	local method=$1 path=$2 body=${3:-}
	[[ ${path} == /api/* ]] || die "assertion failed: path must start with /api/: ${path}"
	umask 077
	mkdir -p "${SECRETS}"
	if [[ ! -s ${SECRETS}/coder.hdr ]]; then
		local name key token
		name=$(k -n "${NS}" get codercontrolplane "${CCP}" -o jsonpath='{.status.operatorTokenSecretRef.name}')
		key=$(k -n "${NS}" get codercontrolplane "${CCP}" -o jsonpath='{.status.operatorTokenSecretRef.key}')
		[[ -n ${name} && -n ${key} ]] || die "CoderControlPlane ${NS}/${CCP} has no status.operatorTokenSecretRef yet"
		token=$(k -n "${NS}" get secret "${name}" -o json | jq -er --arg k "${key}" '.data[$k]' | base64 -d)
		[[ -n ${token} ]] || die "assertion failed: operator token is empty"
		printf 'Coder-Session-Token: %s\n' "${token}" >"${SECRETS}/coder.hdr"
	fi
	local pf_log port="" pid i
	pf_log=$(mktemp "${SECRETS}/port-forward.XXXXXX")
	kw -n "${NS}" port-forward --address 127.0.0.1 svc/coder :80 >"${pf_log}" 2>&1 &
	pid=$!
	# shellcheck disable=SC2064
	trap "kill ${pid} 2>/dev/null || true; rm -f '${pf_log}'" EXIT
	for i in $(seq 1 100); do
		port=$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) .*/\1/p' "${pf_log}" | head -1)
		[[ -n ${port} ]] && break
		kill -0 "${pid}" 2>/dev/null || die "port-forward to svc/coder exited: $(cat "${pf_log}")"
		sleep 0.1
	done
	[[ -n ${port} ]] || die "port-forward to svc/coder did not start in 10s (try ${i})"
	local resp code
	resp=$(mktemp "${SECRETS}/response.XXXXXX")
	# shellcheck disable=SC2064
	trap "kill ${pid} 2>/dev/null || true; rm -f '${pf_log}' '${resp}'" EXIT
	local args=(-sS --max-time 60 -o "${resp}" -w '%{http_code}' -X "${method}"
		-H @"${SECRETS}/coder.hdr" -H 'Accept: application/json')
	[[ -z ${body} ]] || args+=(-H 'Content-Type: application/json' --data @"${body}")
	code=$(curl "${args[@]}" "http://127.0.0.1:${port}${path}")
	cat "${resp}"
	[[ -s ${resp} && $(tail -c1 "${resp}" | od -An -c | tr -d ' ') != '\n' ]] && echo
	echo "HTTP ${code} ${method} ${path}" >&2
	[[ ${code} =~ ^2 ]]
}

cmd_capture() {
	require_run_dir
	(($# >= 3)) && [[ $2 == -- ]] || die "usage: capture <label> -- <cmd...>"
	local label=$1
	shift 2
	[[ ${label} =~ ^[a-z0-9][a-z0-9._-]*$ ]] || die "label must match ^[a-z0-9][a-z0-9._-]*$: ${label}"
	mkdir -p "${EVIDENCE}"
	local n f t0 rc
	n=$(find "${EVIDENCE}" -maxdepth 1 -name '[0-9][0-9][0-9]-*.txt' | wc -l)
	f=$(printf '%s/%03d-%s.txt' "${EVIDENCE}" "$((n + 1))" "${label}")
	{
		printf '$'
		printf ' %q' "$@"
		printf '\n# started %s\n' "$(date -u +%FT%TZ)"
	} >"${f}"
	t0=${SECONDS}
	set +e
	"$@" 2>&1 | tee -a "${f}"
	rc=${PIPESTATUS[0]}
	set -e
	[[ $(tail -c1 "${f}" | od -An -c | tr -d ' ') == '\n' ]] || echo >>"${f}"
	printf '# exit %s after %ss\n' "${rc}" "$((SECONDS - t0))" >>"${f}"
	printf '%s\texit %s\t%ss\t%s\n' "$(date -u +%FT%TZ)" "${rc}" "$((SECONDS - t0))" "${f##*/}" >>"${EVIDENCE}/index.tsv"
	log "evidence: ${f} (exit ${rc})"
	return "${rc}"
}

cmd_collect() {
	guard
	local d
	d="${EVIDENCE}/collect-$(date -u +%Y%m%dT%H%M%SZ)"
	mkdir -p "${d}"
	local kinds
	kinds=$(k get crd -o jsonpath='{range .items[?(@.spec.group=="coder.com")]}{.metadata.name}{","}{end}')
	k get nodes,pods -A -o wide >"${d}/pods.txt" 2>&1 || true
	k get events -A --sort-by=.lastTimestamp >"${d}/events.txt" 2>&1 || true
	k get "${kinds%,}" -A -o yaml >"${d}/coder-com-objects.yaml" 2>&1 || true
	k -n "${NS}" get codertemplates.aggregation.coder.com,coderworkspaces.aggregation.coder.com -o yaml >"${d}/aggregated-objects.yaml" 2>&1 || true
	k get apiservice "${APISERVICE}" -o yaml >"${d}/apiservice.yaml" 2>&1 || true
	k -n "${OP_NS}" logs deploy/coder-k8s --tail=-1 >"${d}/operator.log" 2>&1 || true
	k -n "${NS}" logs deploy/coder --tail=2000 >"${d}/coder.log" 2>&1 || true
	log "collected: ${d}"
	ls -1 "${d}"
}

cmd_cleanup() {
	load_run
	local out="${EVIDENCE}/cleanup.txt"
	mkdir -p "${EVIDENCE}"
	exec > >(tee -a "${out}") 2>&1
	log "cleanup of run ${RUN_ID} (cluster ${CLUSTER}, image ${IMAGE_TAG:-none})"
	if [[ ${CLUSTER_CREATED:-0} == 1 ]] && kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
		if owned; then
			"${SELF}" collect || log "collect failed; continuing"
		fi
		[[ $(docker inspect -f '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "${NODE}" 2>/dev/null) == "${CLUSTER}" ]] ||
			die "refusing: node ${NODE} does not carry the KIND label of ${CLUSTER}"
		[[ -n ${NODE_ID:-} && $(docker inspect -f '{{.Id}}' "${NODE}" 2>/dev/null) == "${NODE_ID}" ]] ||
			die "refusing: node ${NODE} is not the container this run created (NODE_ID ${NODE_ID:-unrecorded}). If launch stopped before it recorded NODE_ID, check the node's creation time and delete it by hand: kind delete cluster --name ${CLUSTER}"
		kind delete cluster --name "${CLUSTER}" --kubeconfig "${KCFG}"
	else
		log "no cluster ${CLUSTER} to delete"
	fi
	if [[ -n ${IMAGE_TAG:-} ]] && image_present "${IMAGE_TAG}"; then
		docker image rm "${IMAGE_TAG}" >/dev/null
		log "removed image tag ${IMAGE_TAG}"
	fi
	local img
	for img in ${PULLED_IMAGES:-}; do
		if image_present "${img}" && [[ -z $(docker ps -aq --filter "ancestor=${img}") ]]; then
			docker image rm "${img}" >/dev/null && log "removed image ${img} (this run pulled it)"
		fi
	done
	rm -rf "${RUN_DIR}/build" "${SECRETS}" "${KCFG}" "${RUN_DIR}"/doctor-*.json
	echo "--- after cleanup"
	echo "\$ kind get clusters | grep -x ${CLUSTER}"
	kind get clusters 2>/dev/null | grep -x "${CLUSTER}" || echo "(none)"
	echo "\$ docker ps -a --filter label=io.x-k8s.kind.cluster=${CLUSTER}"
	docker ps -a --filter "label=io.x-k8s.kind.cluster=${CLUSTER}" --format '{{.Names}}' | grep . || echo "(none)"
	echo "\$ docker images ${IMAGE_TAG:-none}"
	docker images --format '{{.Repository}}:{{.Tag}}' "${IMAGE_TAG:-none}" | grep . || echo "(none)"
	echo "evidence kept in ${EVIDENCE}:"
	ls -1 "${EVIDENCE}"
}

main() {
	(($# >= 1)) || usage
	local cmd=$1
	shift
	case ${cmd} in
	launch) cmd_launch "$@" ;;
	doctor) cmd_doctor "$@" ;;
	kubectl) cmd_kubectl "$@" ;;
	node) cmd_node "$@" ;;
	coder-api) cmd_coder_api "$@" ;;
	capture) cmd_capture "$@" ;;
	collect) cmd_collect "$@" ;;
	cleanup) cmd_cleanup "$@" ;;
	*) usage ;;
	esac
}

main "$@"
