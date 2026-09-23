#!/usr/bin/env bash
# CoderWorkspace lifecycle E2E driver for the aggregated API (#113), run after the Kind E2E bootstrap
# creates the CoderTemplate. Every wait is bounded; any failure exits before later mutations.
# Offline tests with stubbed tools: hack/e2e-workspace-lifecycle_test.sh.
set -euo pipefail
shopt -s inherit_errexit

NS=${E2E_NAMESPACE:-coder}
OP_NS=${E2E_OPERATOR_NAMESPACE:-coder-system}
OP_SELECTOR=${E2E_OPERATOR_SELECTOR:-app=coder-k8s}
APISERVER_SVC=${E2E_APISERVER_SERVICE:-coder-k8s-apiserver}
ORG=${E2E_ORG:-coder}
OWNER=${E2E_OWNER:-coder-k8s-operator}
TEMPLATE=${E2E_TEMPLATE:-e2e-template}
WS_NAME=${E2E_WORKSPACE:-e2e-lifecycle}
RENAMED=${E2E_RENAMED_WORKSPACE:-e2e-lifecycle-renamed}
TIMEOUT=${E2E_TIMEOUT_SECONDS:-300}
EVENT_TIMEOUT=${E2E_EVENT_TIMEOUT_SECONDS:-60}
POLL=${E2E_POLL_SECONDS:-3}
PORT=${E2E_CODER_LOCAL_PORT:-13000}
KIND_NODE=${E2E_KIND_NODE:-e2e-control-plane}
WORK=${E2E_WORKDIR:-$(mktemp -d)}
mkdir -p "$WORK"
: "${BUILT_IMAGE_ID:?BUILT_IMAGE_ID must be the image ID recorded at build time}"
EXPECT_VERSION=${E2E_EXPECT_CODER_VERSION:?E2E_EXPECT_CODER_VERSION must name the pinned Coder version}
SOURCE_SHA=${E2E_SOURCE_SHA:?E2E_SOURCE_SHA must be the checked-out source commit}

API=/apis/aggregation.coder.com/v1alpha1/namespaces/${NS}/coderworkspaces
CODER_URL=http://127.0.0.1:${PORT}
BG_PIDS=() CASES=() CURRENT="" RESULT=FAIL TOKEN=""

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "FAIL: $*" >&2; exit 1; }
step() { [[ -z $CURRENT ]] || CASES+=("case: $CURRENT = passed"); CURRENT=$*; log "=== STEP: $*"; }
k() { kubectl --request-timeout=30s "$@"; } # every non-streaming kubectl request is bounded
cleanup() {
  local p
  for p in "${BG_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  [[ $RESULT == PASS || -z $CURRENT ]] || CASES+=("case: $CURRENT = FAILED")
  # Sanitized evidence receipt: identifiers only, never the token.
  printf '%s\n' "=== RECEIPT ($RESULT) ===" "source_sha=$SOURCE_SHA" "run_id=${GITHUB_RUN_ID:-unset}" \
    "run_attempt=${GITHUB_RUN_ATTEMPT:-unset}" "coder_version=${CODER_VERSION:-unknown}" "built_image_id=$BUILT_IMAGE_ID" \
    "serving_image_id=${SERVING_IMAGE_ID:-unknown}" "pod=${POD_NAME:-unknown}" "uid0=${UID0:-unset}" "uid1=${UID1:-unset}" \
    "${CASES[@]}" | tee "$WORK/receipt.txt"
}
trap cleanup EXIT

for tool in kubectl curl jq base64 docker; do command -v "$tool" >/dev/null || fail "missing tool: $tool"; done
[[ $BUILT_IMAGE_ID =~ ^sha256:[0-9a-f]{64}$ ]] || fail "BUILT_IMAGE_ID is not a sha256 image ID: '$BUILT_IMAGE_ID'"

# wait_until <description> <command...>: retry until success, bounded by TIMEOUT.
wait_until() {
  local desc=$1 start=$SECONDS && shift
  until "$@"; do
    ((SECONDS - start < TIMEOUT)) || fail "timed out after ${TIMEOUT}s waiting for: $desc"
    sleep "$POLL"
  done
}

# ws_get <name>: prints the object. Returns 0 if found, 4 on NotFound; any other error fails.
ws_get() {
  k get --raw "$API/$1" >"$WORK/get.json" 2>"$WORK/get.err" && { cat "$WORK/get.json"; return 0; }
  grep -q '(NotFound)' "$WORK/get.err" && return 4
  fail "GET $1: $(cat "$WORK/get.err")"
}

# expect_error <Reason> <command...>: the command must fail with the given API reason.
expect_error() {
  local reason=$1 && shift
  if "$@" >"$WORK/expect.out" 2>"$WORK/expect.err"; then fail "expected ($reason) but request succeeded: $*"; fi
  grep -q "($reason)" "$WORK/expect.err" || fail "expected ($reason), got: $(cat "$WORK/expect.err")"
}

delete_body() { jq -n --arg uid "$1" --arg rv "${2:-}" \
  '{kind:"DeleteOptions",apiVersion:"v1",preconditions:({uid:$uid} + (if $rv == "" then {} else {resourceVersion:$rv} end))}'; }
# Rendering is guarded explicitly: callers such as expect_error run this with errexit off.
ws_delete() {
  delete_body "$2" "${3:-}" >"$WORK/delete.json" || return 1
  jq -e --arg uid "$2" '.preconditions.uid == $uid' "$WORK/delete.json" >/dev/null || return 1
  k delete --raw "$API/$1" -f "$WORK/delete.json"
}

coder_api() { # <method> <path> [json-body]
  local args=(-fsS --max-time 30 -X "$1" -H "Coder-Session-Token: $TOKEN" -H 'Content-Type: application/json')
  [[ $# -ge 3 ]] && args+=(--data "$3")
  curl "${args[@]}" "$CODER_URL$2"
}

# build_done <name> <buildID> <status>: true once the build reached status; fails on terminal failure.
build_done() {
  local obj rc=0 s
  obj=$(ws_get "$1") || rc=$?
  ((rc == 0)) || fail "workspace $1 disappeared while waiting for build $2 (rc=$rc)"
  s=$(jq -r '.status.latestBuildStatus' <<<"$obj")
  [[ $(jq -r '.status.latestBuildID' <<<"$obj") == "$2" ]] || fail "latest build of $1 changed away from $2"
  case $s in failed | canceled | canceling) fail "build $2 of $1 ended in status $s" ;; esac
  [[ $s == "$3" ]]
}
gone() { local rc=0; ws_get "$1" >"$WORK/gone.json" || rc=$?; ((rc == 4)) || { build_not_failed "$WORK/gone.json" && return 1; }; }
build_not_failed() { ! jq -e '.status.latestBuildStatus | test("^(failed|canceled)$")' "$1" >/dev/null || fail "delete build failed: $(cat "$1")"; }

step "image identity: built vs serving"
serving_pod() {
  k -n "$OP_NS" get pods -l "$OP_SELECTOR" -o json >"$WORK/pods.json" || return 1
  jq -e '[.items[] | select(.metadata.deletionTimestamp == null)] | length == 1 and
    (.[0].status.conditions // [] | any(.type == "Ready" and .status == "True"))' "$WORK/pods.json" >/dev/null
}
wait_until "exactly one Ready non-terminating operator pod" serving_pod
POD=$(jq -c '[.items[] | select(.metadata.deletionTimestamp == null)][0]' "$WORK/pods.json")
POD_IMAGE_REF=$(jq -er '.status.containerStatuses[0].imageID' <<<"$POD") || fail "serving pod has no imageID"
POD_IP=$(jq -er '.status.podIP' <<<"$POD") || fail "serving pod has no podIP"
POD_NAME=$(jq -r '.metadata.name' <<<"$POD")
POD_IMAGE=$(jq -er '.spec.containers[0].image' <<<"$POD") || fail "serving pod has no image"
[[ $POD_IMAGE_REF == *@sha256:* ]] || fail "serving pod imageID is not a digest reference: $POD_IMAGE_REF"
# kind load imports images as import-<date>@sha256:<digest>, which crictl cannot inspect. Inspect the pod's
# image tag on the node instead and require the pod's digest to be one of that image's repo digests.
docker exec "$KIND_NODE" crictl inspecti -o json "$POD_IMAGE" >"$WORK/node-image.json" ||
  fail "cannot inspect serving image $POD_IMAGE on node $KIND_NODE"
SERVING_IMAGE_ID=$(jq -er '.status.id' "$WORK/node-image.json") || fail "node image $POD_IMAGE has no id"
jq -e --arg d "${POD_IMAGE_REF##*@}" '[.status.repoDigests[]? | sub("^.*@"; "")] | any(. == $d)' "$WORK/node-image.json" >/dev/null ||
  fail "serving pod digest ${POD_IMAGE_REF##*@} is not a digest of $POD_IMAGE on node $KIND_NODE"
log "built=$BUILT_IMAGE_ID serving=$SERVING_IMAGE_ID podImageRef=$POD_IMAGE_REF pod=$POD_NAME podIP=$POD_IP"
printf 'built=%s\nserving=%s\npod_image_ref=%s\n' "$BUILT_IMAGE_ID" "$SERVING_IMAGE_ID" "$POD_IMAGE_REF" >"$WORK/image-identity.txt"
[[ $SERVING_IMAGE_ID == "$BUILT_IMAGE_ID" ]] || fail "image identity mismatch: built=$BUILT_IMAGE_ID serving=$SERVING_IMAGE_ID"
k -n "$OP_NS" get endpointslices -l "kubernetes.io/service-name=$APISERVER_SVC" -o json >"$WORK/endpoints.json"
jq -e --arg ip "$POD_IP" '[.items[].endpoints[]? | select(.conditions.ready == true) | .addresses[]] | unique == [$ip]' \
  "$WORK/endpoints.json" >/dev/null || fail "aggregated API service endpoints do not match serving pod $POD_IP"

step "Coder API access (port-forward, operator token)"
CP=$(k -n "$NS" get codercontrolplane coder -o json)
SECRET=$(jq -er '.status.operatorTokenSecretRef.name' <<<"$CP") || fail "no operator token secret ref"
KEY=$(jq -er '.status.operatorTokenSecretRef.key' <<<"$CP") || fail "no operator token secret key"
TOKEN=$(k -n "$NS" get secret "$SECRET" -o json | jq -er --arg k "$KEY" '.data[$k]' | base64 -d)
[[ -n $TOKEN ]] || fail "operator token is empty"
[[ ${GITHUB_ACTIONS:-} == true ]] && echo "::add-mask::$TOKEN"
kubectl -n "$NS" port-forward svc/coder "$PORT:80" >"$WORK/port-forward.log" 2>&1 &
BG_PIDS+=("$!")
coder_ready() { coder_api GET /api/v2/buildinfo >/dev/null; }
wait_until "Coder API via port-forward" coder_ready
CODER_VERSION=$(coder_api GET /api/v2/buildinfo | jq -er '.version') || fail "cannot read Coder buildinfo version"
[[ $CODER_VERSION == "$EXPECT_VERSION" || $CODER_VERSION == "$EXPECT_VERSION+"* ]] ||
  fail "Coder version mismatch: expected $EXPECT_VERSION, serving $CODER_VERSION"

step "wait for template import (#105: create does not wait)"
template_imported() {
  local vid st
  vid=$(coder_api GET "/api/v2/organizations/$ORG/templates/$TEMPLATE" | jq -er '.active_version_id') || return 1
  st=$(coder_api GET "/api/v2/templateversions/$vid" | jq -er '.job.status') || return 1
  case $st in failed | canceled | canceling) fail "template version $vid import ended in status $st" ;; esac
  [[ $st == succeeded ]]
}
wait_until "template $ORG/$TEMPLATE import to succeed" template_imported

create_ws() { # <name> -> created object on stdout
  jq -n --arg n "$ORG.$OWNER.$1" --arg ns "$NS" --arg org "$ORG" --arg t "$TEMPLATE" \
    '{apiVersion:"aggregation.coder.com/v1alpha1",kind:"CoderWorkspace",metadata:{name:$n,namespace:$ns},
      spec:{organization:$org,templateName:$t,running:true}}' >"$WORK/create.json" || return 1
  [[ -s $WORK/create.json ]] || return 1
  k create --raw "$API" -f "$WORK/create.json"
}

step "create workspace through the aggregated API"
NAME=$ORG.$OWNER.$WS_NAME
OBJ=$(create_ws "$WS_NAME")
UID0=$(jq -er '.metadata.uid' <<<"$OBJ") || fail "create response lacks uid: $OBJ"
BUILD=$(jq -er '.status.latestBuildID' <<<"$OBJ") || fail "create response lacks latestBuildID: $OBJ"
wait_until "start build $BUILD" build_done "$NAME" "$BUILD" running

step "watch from current token, update spec.running, require matching MODIFIED"
OBJ=$(ws_get "$NAME")
RV1=$(jq -er '.metadata.resourceVersion' <<<"$OBJ")
WATCH_URL="$API?watch=1&resourceVersion=$(jq -rn --arg v "$RV1" '$v|@uri')&timeoutSeconds=$((EVENT_TIMEOUT + 30))"
kubectl get --raw "$WATCH_URL" -v=6 >"$WORK/watch.out" 2>"$WORK/watch.err" &
WATCH_PID=$!
BG_PIDS+=("$WATCH_PID")
watch_registered() { grep 'watch=1' "$WORK/watch.err" | grep -q '200 OK'; }
wait_until "watch registration (200 OK response headers)" watch_registered
jq '.spec.running = false' <<<"$OBJ" >"$WORK/update.json"
kill -0 "$WATCH_PID" 2>/dev/null || fail "watch process exited before the update"
UPD=$(k replace --raw "$API/$NAME" -f "$WORK/update.json")
RV2=$(jq -er '.metadata.resourceVersion' <<<"$UPD") || fail "update response lacks resourceVersion: $UPD"
BUILD=$(jq -er '.status.latestBuildID' <<<"$UPD") || fail "update response lacks latestBuildID: $UPD"
[[ $(jq -r '.metadata.uid' <<<"$UPD") == "$UID0" ]] || fail "update response UID changed"
matching_event() {
  jq -e -R --arg uid "$UID0" --arg rv "$RV2" 'fromjson? | select(.type == "MODIFIED" and
    .object.metadata.uid == $uid and .object.metadata.resourceVersion == $rv)' "$WORK/watch.out" >/dev/null
}
TIMEOUT=$EVENT_TIMEOUT wait_until "MODIFIED event with resourceVersion $RV2" matching_event
kill "$WATCH_PID" 2>/dev/null || true
wait_until "stop build $BUILD" build_done "$NAME" "$BUILD" stopped

step "out-of-band Coder rename keeps UID; old name is 404"
coder_api PATCH "/api/v2/workspaces/$UID0" "$(jq -nc --arg n "$RENAMED" '{name:$n}')" >/dev/null
NEW_NAME=$ORG.$OWNER.$RENAMED
renamed() { local rc=0; ws_get "$NEW_NAME" >"$WORK/renamed.json" || rc=$?; ((rc == 0)); }
wait_until "renamed workspace $NEW_NAME" renamed
[[ $(jq -r '.metadata.uid' "$WORK/renamed.json") == "$UID0" ]] || fail "renamed object has a different UID"
[[ $(jq -r '.metadata.name' "$WORK/renamed.json") == "$NEW_NAME" ]] || fail "renamed object has a non-canonical name"
RC=0 && ws_get "$NAME" >/dev/null || RC=$?
((RC == 4)) || fail "old name $NAME is still served after rename (rc=$RC)"

step "wrong-UID delete returns 409 and leaves object and latest build intact"
WRONG_UID=00000000-0000-4000-8000-000000000000
[[ $WRONG_UID != "$UID0" ]] || fail "wrong UID collides with live UID"
expect_error Conflict ws_delete "$NEW_NAME" "$WRONG_UID"
OBJ=$(ws_get "$NEW_NAME")
jq -e --arg uid "$UID0" --arg b "$BUILD" '.metadata.uid == $uid and .status.latestBuildID == $b and
  .status.latestBuildStatus == "stopped"' <<<"$OBJ" >/dev/null || fail "object or latest build changed after rejected delete: $OBJ"

step "delete with live UID and token, wait for delete build"
ws_delete "$NEW_NAME" "$UID0" "$(jq -er '.metadata.resourceVersion' <<<"$OBJ")" >/dev/null
wait_until "delete build of $NEW_NAME (404)" gone "$NEW_NAME"
delete_succeeded() { # a 404 proves disappearance; the delete job must also have succeeded
  local st
  st=$(coder_api GET "/api/v2/workspaces/$UID0?include_deleted=true" |
    jq -er 'select(.latest_build.transition == "delete") | .latest_build.job.status') || return 1
  case $st in failed | canceled | canceling) fail "delete job of $UID0 ended in status $st" ;; esac
  [[ $st == succeeded ]]
}
wait_until "delete job of $UID0 to succeed" delete_succeeded

step "recreate the same name: new UID; prior-UID delete returns 409"
OBJ=$(create_ws "$RENAMED")
UID1=$(jq -er '.metadata.uid' <<<"$OBJ") || fail "recreate response lacks uid: $OBJ"
BUILD=$(jq -er '.status.latestBuildID' <<<"$OBJ") || fail "recreate response lacks latestBuildID: $OBJ"
[[ $UID1 != "$UID0" ]] || fail "recreated workspace reused prior UID $UID0"
wait_until "start build $BUILD" build_done "$NEW_NAME" "$BUILD" running
expect_error Conflict ws_delete "$NEW_NAME" "$UID0"
jq -e --arg uid "$UID1" --arg b "$BUILD" '.metadata.uid == $uid and .status.latestBuildID == $b and
  .status.latestBuildStatus == "running"' <<<"$(ws_get "$NEW_NAME")" >/dev/null || fail "recreated object changed after prior-UID delete"
CASES+=("case: $CURRENT = passed") && CURRENT="" && RESULT=PASS
log "PASS: workspace lifecycle"
