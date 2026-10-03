#!/usr/bin/env bash
# CoderWorkspace lifecycle E2E driver for the aggregated API (#113), run after the Kind E2E bootstrap
# creates the CoderTemplate. Every wait is bounded; any failure exits before later mutations.
# Offline tests with stubbed tools: hack/e2e-workspace-lifecycle_test.sh.
set -euo pipefail
umask 077 # $WORK holds the Coder token header and request bodies
shopt -s inherit_errexit

NS=${E2E_NAMESPACE:-coder}
OP_NS=${E2E_OPERATOR_NAMESPACE:-coder-system}
OP_SELECTOR=${E2E_OPERATOR_SELECTOR:-app=coder-k8s}
APISERVER_SVC=${E2E_APISERVER_SERVICE:-coder-k8s-apiserver}
ORG=${E2E_ORG:-coder}
OWNER=${E2E_OWNER:-coder-k8s-operator}
TEMPLATE=${E2E_TEMPLATE:-e2e-template}
TEMPLATE_MANIFEST=${E2E_TEMPLATE_MANIFEST:-config/e2e/codertemplate.yaml} # must define $ORG.$TEMPLATE
TEMPLATE_APPLY_TIMEOUT=${E2E_TEMPLATE_APPLY_TIMEOUT:-600s}                  # Create now waits for the import (#105)
WS_NAME=${E2E_WORKSPACE:-e2e-lifecycle}
RENAMED=${E2E_RENAMED_WORKSPACE:-e2e-lifecycle-renamed}
TESTER=${E2E_TESTER:-e2e-tester} # password user created by the driver; owns the agent workspace
AGENT_TEMPLATE=${E2E_AGENT_TEMPLATE:-e2e-agent}
AGENT_MANIFEST=${E2E_AGENT_TEMPLATE_MANIFEST:-config/e2e/codertemplate-agent.yaml} # must define $ORG.$AGENT_TEMPLATE
AGENT_WS=${E2E_AGENT_WORKSPACE:-e2e-agent}
AGENT_TIMEOUT=${E2E_AGENT_TIMEOUT_SECONDS:-300} # create until every agent is connected and ready
TT_TIMEOUT=${E2E_TEMPLATE_TEST_TIMEOUT_SECONDS:-300}      # create until a CoderTemplateTest is final
NS_DELETE_TIMEOUT=${E2E_NAMESPACE_DELETE_TIMEOUT_SECONDS:-300}
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
  rm -f "$WORK/coder.hdr" "$WORK/coder-body.json" "$WORK/tester.json"
  [[ $RESULT == PASS || -z $CURRENT ]] || CASES+=("case: $CURRENT = FAILED")
  # Sanitized evidence receipt: identifiers only, never the token.
  printf '%s\n' "=== RECEIPT ($RESULT) ===" "source_sha=$SOURCE_SHA" "run_id=${GITHUB_RUN_ID:-unset}" \
    "run_attempt=${GITHUB_RUN_ATTEMPT:-unset}" "coder_version=${CODER_VERSION:-unknown}" "built_image_id=$BUILT_IMAGE_ID" \
    "serving_image_id=${SERVING_IMAGE_ID:-unknown}" "pod=${POD_NAME:-unknown}" "uid0=${UID0:-unset}" "uid1=${UID1:-unset}" \
    "rv_pre_rename=${OLD_RV:-unset}" "rv_post_rename=${NEW_RV:-unset}" "template=${TPL_STATE:-unset}" \
    "workspace=${WS_STATE:-unset}" "apply_failure=${APPLY_FAILURE:-none}" \
    "tester=${TESTER_NAME:-unset}/${TESTER_ID:-unset}" "agent_ready_seconds=${AGENT_READY_SECONDS:-unset}" \
    "template_test_seconds=${TT_SECONDS[*]:-unset}" "tester_api_keys_before=${KEYS_BEFORE:-unset}" \
    "tester_api_keys_after=${KEYS_AFTER:-unset}" "namespace_delete_seconds=${NS_DELETE_SECONDS:-unset}" \
    "phase_seconds=${PHASES[*]:-unset}" \
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

coder_api() { # <method> <path> [json-body]: the token and the body reach curl through 0600 files, never argv
  local args=(-fsS --max-time 30 -X "$1" -H @"$WORK/coder.hdr" -H 'Content-Type: application/json')
  [[ $# -lt 3 ]] || { printf '%s' "$3" >"$WORK/coder-body.json" && args+=(--data @"$WORK/coder-body.json"); }
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
printf 'Coder-Session-Token: %s\n' "$TOKEN" >"$WORK/coder.hdr"
kubectl -n "$NS" port-forward svc/coder "$PORT:80" >"$WORK/port-forward.log" 2>&1 &
BG_PIDS+=("$!")
coder_ready() { coder_api GET /api/v2/buildinfo >/dev/null; }
wait_until "Coder API via port-forward" coder_ready
CODER_VERSION=$(coder_api GET /api/v2/buildinfo | jq -er '.version') || fail "cannot read Coder buildinfo version"
[[ $CODER_VERSION == "$EXPECT_VERSION" || $CODER_VERSION == "$EXPECT_VERSION+"* ]] ||
  fail "Coder version mismatch: expected $EXPECT_VERSION, serving $CODER_VERSION"

# apply_manifest <file> [request-timeout]: client-side kubectl apply; a failure is recorded in the receipt and stops.
apply_manifest() {
  kubectl --request-timeout="${2:-30s}" apply -f "$1" >"$WORK/apply.out" 2>&1 && return 0
  APPLY_FAILURE="apply $1: $(tr -s '\n' ' ' <"$WORK/apply.out" | cut -c1-400)"
  fail "$APPLY_FAILURE"
}
template_state() { # Coder template id, active version and version count
  local t id active n
  t=$(coder_api GET "/api/v2/organizations/$ORG/templates/$TEMPLATE") || return 1
  id=$(jq -er '.id' <<<"$t") || return 1
  active=$(jq -er '.active_version_id' <<<"$t") || return 1
  n=$(coder_api GET "/api/v2/templates/$id/versions?limit=100" | jq -er 'length') || return 1
  echo "id=$id active=$active versions=$n"
}
latest_build() { coder_api GET "/api/v2/workspaces/$UID0" | jq -er '.latest_build.id'; }
build_count() { coder_api GET "/api/v2/workspaces/$UID0/builds?limit=100" | jq -er 'length'; }
ws_state() { # UID, latest build (aggregated/Coder), build count and status of the first workspace
  local o latest n
  o=$(ws_get "$NAME") || return 1
  latest=$(latest_build) || return 1
  n=$(build_count) || return 1
  jq -er --arg l "$latest" --arg n "$n" '"uid=\(.metadata.uid) latest=\(.status.latestBuildID)/\($l) builds=\($n) status=\(.status.latestBuildStatus)"' <<<"$o"
}
ws_manifest() { # <leaf name> [owner] [template]: writes $WORK/ws-<leaf>.json, a canonical CoderWorkspace manifest with running=true
  jq -n --arg n "$ORG.${2:-$OWNER}.$1" --arg ns "$NS" --arg org "$ORG" --arg t "${3:-$TEMPLATE}" \
    '{apiVersion:"aggregation.coder.com/v1alpha1",kind:"CoderWorkspace",metadata:{name:$n,namespace:$ns},
      spec:{organization:$org,templateName:$t,running:true}}' >"$WORK/ws-$1.json" || return 1
  [[ -s $WORK/ws-$1.json ]] || return 1
}
create_ws() { # <leaf name> -> created object on stdout
  ws_manifest "$1" || return 1
  k create --raw "$API" -f "$WORK/ws-$1.json"
}

step "kubectl apply creates the template; its import is ready immediately (#105)"
apply_manifest "$TEMPLATE_MANIFEST" "$TEMPLATE_APPLY_TIMEOUT"
TPL_STATE=$(template_state) || fail "template $ORG/$TEMPLATE unreadable right after apply"
TPL_VERSION=$(sed -E 's/.* active=([^ ]+) .*/\1/' <<<"$TPL_STATE")
IMPORT=$(coder_api GET "/api/v2/templateversions/$TPL_VERSION" | jq -er '.job.status') || fail "cannot read import of $TPL_VERSION"
[[ $IMPORT == succeeded ]] || fail "template import not ready right after apply: version $TPL_VERSION job status $IMPORT"

step "template versions: a files change adds a version; promote rolls back, repeats as a no-op and previews (#149)"
GROUP=/apis/aggregation.coder.com/v1alpha1/namespaces/$NS
template_updated_at() { coder_api GET "/api/v2/organizations/$ORG/templates/$TEMPLATE" | jq -er '.updated_at'; }
promote() { # <version id> [query] -> status.result of codertemplates/promote
  jq -n --arg id "$1" '{apiVersion: "aggregation.coder.com/v1alpha1", kind: "CoderTemplateVersionPromotion", spec: {versionID: $id}}' \
    >"$WORK/promote.json" || return 1
  k create --raw "$GROUP/codertemplates/$ORG.$TEMPLATE/promote${2:-}" -f "$WORK/promote.json" | jq -er '.status.result'
}
k create --dry-run=client -o json -f "$TEMPLATE_MANIFEST" | jq '.spec.files["main.tf"] += "\n# e2e: second version\n"' \
  >"$WORK/template-v2.json" || fail "cannot render the second template version"
apply_manifest "$WORK/template-v2.json" "$TEMPLATE_APPLY_TIMEOUT"
k get codertemplateversions -n "$NS" -l "aggregation.coder.com/template=$TEMPLATE" -o json >"$WORK/versions.json" ||
  fail "cannot list template versions"
V2=$(jq -er --arg v1 "$TPL_VERSION" '.items | if length == 2 and any(.status.id == $v1 and (.status.active | not))
  then map(select(.status.active))[0].status.id else error("unexpected versions") end' "$WORK/versions.json") ||
  fail "expected v1 inactive and one new active version: $(jq -c '[.items[] | {id: .status.id, active: .status.active}]' "$WORK/versions.json")"
RESULT_V1=$(promote "$TPL_VERSION") || fail "promote v1 failed"
[[ $RESULT_V1 == Promoted ]] || fail "promote v1: result $RESULT_V1, want Promoted"
TPL_STATE=$(template_state) || fail "template unreadable after the rollback"
[[ $TPL_STATE == *" active=$TPL_VERSION "* ]] || fail "Coder does not show v1 active after the rollback: $TPL_STATE"
UPDATED_AT=$(template_updated_at) || fail "cannot read the template updated_at"
RESULT_V1=$(promote "$TPL_VERSION") || fail "repeated promote v1 failed"
[[ $RESULT_V1 == AlreadyActive ]] || fail "repeated promote v1: result $RESULT_V1, want AlreadyActive"
[[ $(template_updated_at) == "$UPDATED_AT" ]] || fail "repeated promote v1 wrote to Coder (template updated_at changed)"
RESULT_V2=$(promote "$V2" "?dryRun=All") || fail "dry-run promote v2 failed"
[[ $RESULT_V2 == WouldPromote ]] || fail "dry-run promote v2: result $RESULT_V2, want WouldPromote"
AFTER=$(template_state) || fail "template unreadable after the dry-run"
[[ $AFTER == "$TPL_STATE" && $(template_updated_at) == "$UPDATED_AT" ]] || fail "dry-run promote changed Coder: before=[$TPL_STATE] after=[$AFTER]"

step "create workspace through the aggregated API"
NAME=$ORG.$OWNER.$WS_NAME
ws_manifest "$WS_NAME" || fail "cannot render workspace manifest"
apply_manifest "$WORK/ws-$WS_NAME.json"
OBJ=$(ws_get "$NAME")
UID0=$(jq -er '.metadata.uid' <<<"$OBJ") || fail "created workspace lacks uid: $OBJ"
BUILD=$(jq -er '.status.latestBuildID' <<<"$OBJ") || fail "created workspace lacks latestBuildID: $OBJ"
wait_until "start build $BUILD" build_done "$NAME" "$BUILD" running

step "repeated kubectl apply of identical template and workspace manifests changes nothing (#105)"
WS_STATE=$(ws_state) || fail "cannot read workspace state before re-apply"
apply_manifest "$TEMPLATE_MANIFEST" "$TEMPLATE_APPLY_TIMEOUT"
apply_manifest "$WORK/ws-$WS_NAME.json"
AFTER=$(template_state) || fail "template unreadable after re-apply"
[[ $AFTER == "$TPL_STATE" ]] || fail "template changed after identical re-apply: before=[$TPL_STATE] after=[$AFTER]"
AFTER=$(ws_state) || fail "workspace unreadable after re-apply"
[[ $AFTER == "$WS_STATE" ]] || fail "workspace changed after identical re-apply: before=[$WS_STATE] after=[$AFTER]"

step "coderworkspaces/log returns the latest build log; the server log does not contain it (#148)"
k get --raw "$API/$NAME/log" >"$WORK/build.log" || fail "cannot read coderworkspaces/log of $NAME"
LOG_BYTES=$(wc -c <"$WORK/build.log")
((LOG_BYTES > 0)) || fail "build log of $NAME is empty"
grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[^ ]+ \[[a-z]+\] \[provisioner\|' "$WORK/build.log" ||
  fail "unexpected build log line format ($LOG_BYTES bytes; contents not printed because build logs can contain secrets)"
k get --raw "$API/$NAME/log?limitBytes=64" >"$WORK/build-64.log" || fail "cannot read coderworkspaces/log?limitBytes=64"
LIMITED=$(wc -c <"$WORK/build-64.log")
((LIMITED > 0 && LIMITED <= 64)) || fail "limitBytes=64 returned $LIMITED bytes"
log "build log: $LOG_BYTES bytes, $(wc -l <"$WORK/build.log") lines"
k -n "$OP_NS" logs "$POD_NAME" >"$WORK/server.log" || fail "cannot read the server log of $POD_NAME"
! grep -qF -- "$(tail -n 1 "$WORK/build.log")" "$WORK/server.log" || fail "the server log contains a build log line"

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
# #148: follow the log of the stop build; the stream must end on its own when the build ends.
kubectl --request-timeout="${TIMEOUT}s" get --raw "$API/$NAME/log?follow=true" >"$WORK/follow.log" 2>"$WORK/follow.err" &
FOLLOW_PID=$!
BG_PIDS+=("$FOLLOW_PID")
matching_event() {
  jq -e -R --arg uid "$UID0" --arg rv "$RV2" 'fromjson? | select(.type == "MODIFIED" and
    .object.metadata.uid == $uid and .object.metadata.resourceVersion == $rv)' "$WORK/watch.out" >/dev/null
}
TIMEOUT=$EVENT_TIMEOUT wait_until "MODIFIED event with resourceVersion $RV2" matching_event
kill "$WATCH_PID" 2>/dev/null || true
wait_until "stop build $BUILD" build_done "$NAME" "$BUILD" stopped
follow_ended() { ! kill -0 "$FOLLOW_PID" 2>/dev/null; }
TIMEOUT=$EVENT_TIMEOUT wait_until "the log follow of stop build $BUILD to end" follow_ended
wait "$FOLLOW_PID" || fail "log follow failed: $(head -c 300 "$WORK/follow.err")"
[[ -s $WORK/follow.log ]] || fail "log follow of stop build $BUILD printed nothing"
log "log follow: $(wc -l <"$WORK/follow.log") lines"

step "coderworkspaces/start and /stop round trip; a repeated start and a dry-run stop queue nothing (#148)"
transition() { k create --raw "$API/$NAME/$1${2:-}" -f - <<<'{}'; } # <start|stop> [query]
queued_build() { jq -er --arg t "$1" 'select(.status.outcome == "Queued" and .status.transition == $t) | .status.buildID'; }
OUT=$(transition start) || fail "POST $NAME/start failed"
BUILD=$(queued_build start <<<"$OUT") || fail "start of the stopped workspace did not queue a build: $OUT"
wait_until "start build $BUILD" build_done "$NAME" "$BUILD" running
COUNT=$(build_count) || fail "cannot list backend builds of $UID0"
OUT=$(transition start) || fail "repeated POST $NAME/start failed"
jq -e --arg b "$BUILD" '.status.outcome == "Unchanged" and .status.buildID == $b' <<<"$OUT" >/dev/null ||
  fail "repeated start did not answer Unchanged with build $BUILD: $OUT"
OUT=$(transition stop '?dryRun=All') || fail "dry-run POST $NAME/stop failed"
jq -e '.status.outcome == "WouldQueue" and .status.dryRun == true and (.status.buildID // "") == ""' <<<"$OUT" >/dev/null ||
  fail "dry-run stop did not answer WouldQueue without a build: $OUT"
[[ $(build_count) == "$COUNT" ]] || fail "a repeated start or a dry-run stop queued a build (count was $COUNT)"
OUT=$(transition stop) || fail "POST $NAME/stop failed"
BUILD=$(queued_build stop <<<"$OUT") || fail "stop of the running workspace did not queue a build: $OUT"
wait_until "stop build $BUILD" build_done "$NAME" "$BUILD" stopped

step "out-of-band Coder rename keeps UID; old name is 404"
PRE=$(ws_get "$NAME") # genuine pre-rename object of the stopped workspace
OLD_RV=$(jq -er '.metadata.resourceVersion' <<<"$PRE") || fail "pre-rename object lacks resourceVersion"
coder_api PATCH "/api/v2/workspaces/$UID0" "$(jq -nc --arg n "$RENAMED" '{name:$n}')" >/dev/null
NEW_NAME=$ORG.$OWNER.$RENAMED
renamed() { local rc=0; ws_get "$NEW_NAME" >"$WORK/renamed.json" || rc=$?; ((rc == 0)); }
wait_until "renamed workspace $NEW_NAME" renamed
[[ $(jq -r '.metadata.uid' "$WORK/renamed.json") == "$UID0" ]] || fail "renamed object has a different UID"
[[ $(jq -r '.metadata.name' "$WORK/renamed.json") == "$NEW_NAME" ]] || fail "renamed object has a non-canonical name"
RC=0 && ws_get "$NAME" >/dev/null || RC=$?
((RC == 4)) || fail "old name $NAME is still served after rename (rc=$RC)"

step "#109 rename changes the token; stale UPDATE and DELETE return 409 and change nothing"
POST=$(jq -cS . "$WORK/renamed.json")
NEW_RV=$(jq -er '.metadata.resourceVersion' <<<"$POST") || fail "renamed object lacks resourceVersion"
[[ $(jq -r .metadata.name <<<"$PRE") != "$(jq -r .metadata.name <<<"$POST")" ]] || fail "canonical name did not change"
[[ $NEW_RV != "$OLD_RV" ]] || fail "rename did not change resourceVersion (still $OLD_RV)"
LATEST=$(latest_build) || fail "cannot read backend latest build of $UID0"
COUNT=$(build_count) || fail "cannot list backend builds of $UID0"
[[ $LATEST == "$BUILD" ]] || fail "backend latest build $LATEST is not the stop build $BUILD"
jq --arg rv "$OLD_RV" '.metadata.resourceVersion = $rv' <<<"$POST" >"$WORK/stale-update.json"
expect_error Conflict k replace --raw "$API/$NEW_NAME" -f "$WORK/stale-update.json"
expect_error Conflict ws_delete "$NEW_NAME" "$UID0" "$OLD_RV"
[[ $(latest_build) == "$LATEST" ]] || fail "backend latest build changed after stale requests"
[[ $(build_count) == "$COUNT" ]] || fail "backend build count changed after stale requests (was $COUNT)"
[[ $(ws_get "$NEW_NAME" | jq -cS .) == "$POST" ]] || fail "object changed after stale requests"

step "#109 GET and LIST agree on the renamed object and its token"
GOT=$(ws_get "$NEW_NAME")
k get --raw "$API" >"$WORK/list.json"
ITEM=$(jq -c --arg uid "$UID0" '[.items[] | select(.metadata.uid == $uid)]' "$WORK/list.json")
jq -e --arg n "$NEW_NAME" 'length == 1 and .[0].metadata.name == $n' <<<"$ITEM" >/dev/null || fail "renamed $NEW_NAME missing from LIST"
[[ $(jq -r '.[0].metadata.resourceVersion' <<<"$ITEM") == "$(jq -r .metadata.resourceVersion <<<"$GOT")" ]] ||
  fail "LIST token differs from GET token for $NEW_NAME"

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
delete_succeeded() { # <uid>: a 404 proves disappearance; the delete job must also have succeeded
  local st
  st=$(coder_api GET "/api/v2/workspaces/$1?include_deleted=true" |
    jq -er 'select(.latest_build.transition == "delete") | .latest_build.job.status') || return 1
  case $st in failed | canceled | canceling) fail "delete job of $1 ended in status $st" ;; esac
  [[ $st == succeeded ]]
}
wait_until "delete job of $UID0 to succeed" delete_succeeded "$UID0"

step "recreate the same name: new UID; prior-UID delete returns 409"
OBJ=$(create_ws "$RENAMED")
UID1=$(jq -er '.metadata.uid' <<<"$OBJ") || fail "recreate response lacks uid: $OBJ"
BUILD=$(jq -er '.status.latestBuildID' <<<"$OBJ") || fail "recreate response lacks latestBuildID: $OBJ"
[[ $UID1 != "$UID0" ]] || fail "recreated workspace reused prior UID $UID0"
wait_until "start build $BUILD" build_done "$NEW_NAME" "$BUILD" running
expect_error Conflict ws_delete "$NEW_NAME" "$UID0"
jq -e --arg uid "$UID1" --arg b "$BUILD" '.metadata.uid == $uid and .status.latestBuildID == $b and
  .status.latestBuildStatus == "running"' <<<"$(ws_get "$NEW_NAME")" >/dev/null || fail "recreated object changed after prior-UID delete"

step "create the tester user through the Coder API (password login, default organization)"
ORG_ID=$(coder_api GET /api/v2/organizations | jq -er 'map(select(.is_default)) | if length == 1 then .[0].id else error end') ||
  fail "cannot read the single default organization"
TESTER_PASSWORD=$(head -c 24 /dev/urandom | base64) # never logged; only the tester's id and username are recorded
[[ ${GITHUB_ACTIONS:-} == true ]] && echo "::add-mask::$TESTER_PASSWORD"
jq -nc --arg u "$TESTER" --arg p "$TESTER_PASSWORD" --arg o "$ORG_ID" '{username: $u, name: "E2E tester", email: ($u + "@e2e.example.com"),
  password: $p, login_type: "password", organization_ids: [$o]}' >"$WORK/tester.json" || fail "cannot render the tester user request"
if USER_JSON=$(coder_api POST /api/v2/users "$(<"$WORK/tester.json")" 2>"$WORK/tester.err"); then :
elif grep -q 'error: 409' "$WORK/tester.err"; then # a local rerun: reuse the tester after the same checks
  log "tester user $TESTER exists (409): reusing it"
  USER_JSON=$(coder_api GET "/api/v2/users/$TESTER") || fail "cannot read the existing tester user $TESTER"
else fail "cannot create the tester user $TESTER: $(head -c 300 "$WORK/tester.err")"; fi
rm -f "$WORK/tester.json" "$WORK/coder-body.json"
TESTER_ID=$(jq -er --arg u "$TESTER" --arg o "$ORG_ID" 'select(.username == $u and .login_type == "password" and
  (.organization_ids | index($o))) | .id' <<<"$USER_JSON") || fail "tester user $TESTER has an unexpected shape or organization"
# New API users start dormant, and Coder refuses agents of workspaces whose owner is not active (401).
coder_api PUT "/api/v2/users/$TESTER_ID/status/activate" | jq -e '.status == "active"' >/dev/null ||
  fail "cannot activate the tester user $TESTER"
TESTER_NAME=$TESTER
log "tester user: $TESTER_NAME ($TESTER_ID), active"

step "a tester-owned workspace from the agent template gets a connected, ready agent; its delete build succeeds"
apply_manifest "$AGENT_MANIFEST" "$TEMPLATE_APPLY_TIMEOUT"
AGENT_NAME=$ORG.$TESTER_NAME.$AGENT_WS
ws_manifest "$AGENT_WS" "$TESTER_NAME" "$AGENT_TEMPLATE" || fail "cannot render the agent workspace manifest"
AGENT_START=$SECONDS
OBJ=$(k create --raw "$API" -f "$WORK/ws-$AGENT_WS.json") || fail "cannot create the agent workspace $AGENT_NAME"
AGENT_UID=$(jq -er '.metadata.uid' <<<"$OBJ") || fail "agent workspace lacks uid: $OBJ"
BUILD=$(jq -er '.status.latestBuildID' <<<"$OBJ") || fail "agent workspace lacks latestBuildID: $OBJ"
wait_until "start build $BUILD" build_done "$AGENT_NAME" "$BUILD" running
AGENTS_SEEN=""
agents_ready() { # every agent of build $BUILD is connected and ready; a failed lifecycle fails at once
  local w seen
  w=$(coder_api GET "/api/v2/workspaces/$AGENT_UID") || return 1
  [[ $(jq -r '.latest_build.id' <<<"$w") == "$BUILD" ]] || fail "latest build of $AGENT_NAME changed away from $BUILD"
  seen=$(jq -r '[.latest_build.resources[]?.agents[]? | "\(.name)=\(.status)/\(.lifecycle_state)"] | join(" ")' <<<"$w") || return 1
  [[ $seen == "$AGENTS_SEEN" ]] || { AGENTS_SEEN=$seen && log "agents: ${seen:-none}"; }
  [[ $seen != *"/start_error"* && $seen != *"/start_timeout"* ]] || fail "agent of $AGENT_NAME failed to start: $seen"
  jq -e '[.latest_build.resources[]?.agents[]?] | length > 0 and all(.status == "connected" and .lifecycle_state == "ready")' \
    <<<"$w" >/dev/null
}
TIMEOUT=$AGENT_TIMEOUT wait_until "agents of $AGENT_NAME connected and ready" agents_ready
AGENT_READY_SECONDS=$((SECONDS - AGENT_START))
log "agent time-to-ready: ${AGENT_READY_SECONDS}s from create"
ws_delete "$AGENT_NAME" "$AGENT_UID" >/dev/null || fail "cannot delete the agent workspace $AGENT_NAME"
wait_until "delete build of $AGENT_NAME (404)" gone "$AGENT_NAME"
wait_until "delete job of $AGENT_UID to succeed" delete_succeeded "$AGENT_UID"

# db_count <table> <column> <value>: rows of a Coder table (deleted rows included) through psql in the CNPG
# primary. A read-only count: it never selects key values or hashes.
db_count() {
  local pod
  [[ $3 =~ ^[A-Za-z0-9-]+$ ]] || fail "assertion failed: unsafe value for $1.$2: '$3'"
  pod=$(k -n "$NS" get cluster coder-db -o json | jq -er '.status.currentPrimary') || return 1
  k -n "$NS" exec "$pod" -c postgres -- psql -d coder -XtAc "SELECT count(*) FROM $1 WHERE $2 = '$3'" | grep -Ex '[0-9]+'
}
tt_create() { # <name> [parameters JSON] [spec JSON] [create flags]: a test of the agent template's active version
  jq -n --arg n "$1" --arg ns "$NS" --arg t "$ORG.$AGENT_TEMPLATE" --argjson p "${2:-[]}" --argjson x "${3:-"{}"}" '{apiVersion: "coder.com/v1alpha1",
    kind: "CoderTemplateTest", metadata: {name: $n, namespace: $ns}, spec: ({controlPlaneRef: {name: "coder"}, template: $t,
    version: {active: true}} + (if $p == [] then {} else {parameters: $p} end) + $x)}' >"$WORK/tt-$1.json" &&
    k create "${@:4}" -f "$WORK/tt-$1.json" -o json >"$WORK/tt-$1.created.json"
}
param() { jq -nc --arg n "$1" --arg v "$2" '[{name: $n, value: $v}]'; } # <name> <value>: a parameters JSON list
TT_SEEN="" TT_S=""
tt_state() { # <name>: reads the test into $WORK/tt-<name>.state.json; sets TT_S to "phase reason deleted=status/reason"
  k -n "$NS" get codertemplatetest "$1" -o json >"$WORK/tt-$1.state.json" 2>"$WORK/tt.err" || return 1
  TT_S=$(jq -r '"\(.status.phase // "-") \(.status.reason // "-") deleted=\([.status.conditions[]? |
    select(.type == "WorkspaceDeleted") | "\(.status)/\(.reason)"][0] // "-")"' "$WORK/tt-$1.state.json") || return 1
  [[ "$1 $TT_S" == "$TT_SEEN" ]] || { TT_SEEN="$1 $TT_S" && log "template test $1: $TT_S"; }
}
tt_passed() { # Succeeded, Ready=True and WorkspaceDeleted=True (Deleted); Failed fails at once
  tt_state "$1" || return 1
  [[ $TT_S != Failed* ]] || fail "template test $1 failed: $TT_S: $(jq -r '.status.message' "$WORK/tt-$1.state.json")"
  jq -e '.status.phase == "Succeeded" and (.status.conditions | any(.type == "Ready" and .status == "True") and
    any(.type == "WorkspaceDeleted" and .status == "True" and .reason == "Deleted"))' "$WORK/tt-$1.state.json" >/dev/null
}
CP_GEN=$(k -n "$NS" get deploy coder -o json | jq -er '.metadata.generation') || fail "cannot read deploy/coder"

step "configure: CoderControlPlane coder names the active tester in spec.templateTests.ownerUserID"
k -n "$NS" patch codercontrolplane coder --type=merge -p "$(jq -nc --arg id "$TESTER_ID" '{spec: {templateTests: {ownerUserID: $id}}}')" \
  >/dev/null || fail "cannot set templateTests.ownerUserID on codercontrolplane coder"

step "tester API keys before the template tests (plan A4)"
OP_ID=$(coder_api GET /api/v2/users/me | jq -er '.id') || fail "cannot read the operator user"
OP_KEYS=$(db_count api_keys user_id "$OP_ID") || fail "cannot count the api_keys rows of the operator user"
((OP_KEYS > 0)) || fail "the operator user has no api_keys rows: the count query does not read Coder's keys"
KEYS_BEFORE=$(db_count api_keys user_id "$TESTER_ID") || fail "cannot count the api_keys rows of the tester"

step "CoderTemplateTest pass: three sequential tests of the active agent template version succeed and delete their workspaces"
TT_SECONDS=()
for i in 1 2 3; do
  T0=$SECONDS
  tt_create "e2e-pass-$i" || fail "cannot create template test e2e-pass-$i"
  TIMEOUT=$TT_TIMEOUT wait_until "template test e2e-pass-$i to succeed" tt_passed "e2e-pass-$i"
  TT_SECONDS+=("$((SECONDS - T0))s")
done
log "template test durations: ${TT_SECONDS[*]}"
TT_WS=$(jq -er '.status.workspaceName' "$WORK/tt-e2e-pass-1.state.json") || fail "template test e2e-pass-1 has no workspaceName"
ROWS=$(db_count workspaces name "$TT_WS") || fail "cannot count the workspaces rows named $TT_WS"
[[ $ROWS == 1 ]] || fail "expected exactly one workspaces row named $TT_WS (deleted rows included), found $ROWS"
[[ $(k -n "$NS" get deploy coder -o json | jq -r '.metadata.generation') == "$CP_GEN" ]] ||
  fail "setting templateTests.ownerUserID rolled deploy/coder (generation was $CP_GEN)" # plan risk R5

PHASES=()
phase() { PHASES+=("$1=$((SECONDS - T0))s"); } # <label>: records the duration since T0 for the receipt
tt_final() { # <name> <phase> <reason> <WorkspaceDeleted reason>: true once the test ends so; another final state fails
  tt_state "$1" || return 1
  [[ $TT_S == "$2 $3 deleted=True/$4" ]] && return 0
  [[ $TT_S != Succeeded* && $TT_S != Failed* || $TT_S == "$2 $3 "* ]] ||
    fail "template test $1 ended $TT_S, want $2 $3 deleted=True/$4: $(jq -r '.status.message' "$WORK/tt-$1.state.json")"
  return 1
}
tt_gone() { ! k -n "$NS" get codertemplatetest "$1" -o name >/dev/null 2>"$WORK/tt.err" && grep -q NotFound "$WORK/tt.err"; }
ws_rows() { # <test name> <want>: the workspaces rows (deleted included) named by the test's status.workspaceName
  local ws n
  ws=$(jq -er '.status.workspaceName' "$WORK/tt-$1.state.json") || fail "template test $1 has no workspaceName"
  n=$(db_count workspaces name "$ws") || fail "cannot count the workspaces rows named $ws"
  [[ $n == "$2" ]] || fail "template test $1: expected $2 workspaces rows named $ws (deleted rows included), found $n"
}
wait_waiting() { tt_state "$1" && [[ $TT_S == "Running WaitingForAgents "* ]]; }

step "CoderTemplateTest agent failure: fail=true ends Failed/AgentStartError and deletes its workspace"
T0=$SECONDS
tt_create e2e-agent-fail "$(param fail true)" || fail "cannot create template test e2e-agent-fail"
TIMEOUT=$TT_TIMEOUT wait_until "template test e2e-agent-fail to end AgentStartError" tt_final e2e-agent-fail Failed AgentStartError Deleted
ws_rows e2e-agent-fail 1 && phase agent-failure

step "CoderTemplateTest bad parameter: fail=notabool ends Failed/CreateRejected without a workspace"
T0=$SECONDS
tt_create e2e-bad-param "$(param fail notabool)" || fail "cannot create template test e2e-bad-param"
TIMEOUT=$TT_TIMEOUT wait_until "template test e2e-bad-param to end CreateRejected" tt_final e2e-bad-param Failed CreateRejected NotCreated
ws_rows e2e-bad-param 0 && phase bad-parameter

step "CoderTemplateTest restart: the operator pod is deleted at WaitingForAgents and the test still succeeds"
T0=$SECONDS
tt_create e2e-restart "$(param startup_delay 60)" || fail "cannot create template test e2e-restart"
wait_until "template test e2e-restart at Running/WaitingForAgents" wait_waiting e2e-restart
k -n "$OP_NS" delete pod "$POD_NAME" --wait=false >/dev/null || fail "cannot delete operator pod $POD_NAME"
wait_until "a new Ready operator pod" serving_pod
TIMEOUT=$TT_TIMEOUT wait_until "template test e2e-restart to succeed after the restart" tt_passed e2e-restart
ws_rows e2e-restart 1 && phase restart

step "CoderTemplateTest delete mid-run: deleting the test at WaitingForAgents deletes its workspace"
T0=$SECONDS
tt_create e2e-midrun "$(param startup_delay 60)" || fail "cannot create template test e2e-midrun"
wait_until "template test e2e-midrun at Running/WaitingForAgents" wait_waiting e2e-midrun
MIDRUN_WS=$(jq -er '.status.workspaceID' "$WORK/tt-e2e-midrun.state.json") || fail "template test e2e-midrun has no workspaceID"
k -n "$NS" delete codertemplatetest e2e-midrun --wait=false >/dev/null || fail "cannot delete template test e2e-midrun"
TIMEOUT=$TT_TIMEOUT wait_until "template test e2e-midrun to disappear" tt_gone e2e-midrun
wait_until "delete job of $MIDRUN_WS to succeed" delete_succeeded "$MIDRUN_WS"
ws_rows e2e-midrun 1 && phase delete-mid-run

step "CoderTemplateTest immutability and dry run: a spec patch is refused; a server dry run creates nothing"
T0=$SECONDS
if k -n "$NS" patch codertemplatetest e2e-agent-fail --type=merge -p '{"spec":{"timeoutSeconds":901}}' >/dev/null 2>"$WORK/patch.err"; then
  fail "a patch of spec.timeoutSeconds succeeded"
fi
grep -qF "spec is immutable" "$WORK/patch.err" || fail "spec patch refused without 'spec is immutable': $(head -c 300 "$WORK/patch.err")"
tt_create e2e-dry-run "[]" "{}" --dry-run=server || fail "server dry run of template test e2e-dry-run failed"
# The workspace name the object would get: ktt- and the first 28 hex characters of its UID.
DRY_UID=$(jq -er '.metadata.uid' "$WORK/tt-e2e-dry-run.created.json") || fail "the dry run answered without a UID"
DRY_WS=ktt-$(tr -d - <<<"$DRY_UID" | cut -c1-28)
tt_gone e2e-dry-run || fail "the server dry run created template test e2e-dry-run"
phase immutability-and-dry-run

step "CoderTemplateTest TTL: ttlSecondsAfterFinished=0 removes a passed test"
T0=$SECONDS
TTL_URL="/apis/coder.com/v1alpha1/namespaces/$NS/codertemplatetests?watch=1&fieldSelector=metadata.name%3De2e-ttl&timeoutSeconds=$((TT_TIMEOUT + 30))"
kubectl get --raw "$TTL_URL" -v=6 >"$WORK/ttl-watch.out" 2>"$WORK/ttl-watch.err" &
BG_PIDS+=("$!")
ttl_watching() { grep 'watch=1' "$WORK/ttl-watch.err" | grep -q '200 OK'; }
wait_until "watch of template test e2e-ttl" ttl_watching
tt_create e2e-ttl "[]" '{"ttlSecondsAfterFinished":0}' || fail "cannot create template test e2e-ttl"
TIMEOUT=$TT_TIMEOUT wait_until "template test e2e-ttl to be removed" tt_gone e2e-ttl
ttl_seen() { jq -e -R 'fromjson? | select(.type == $t)' --arg t "$1" "$WORK/ttl-watch.out" >/dev/null; }
wait_until "the DELETED event of template test e2e-ttl" ttl_seen DELETED
jq -e -R 'fromjson? | select(.type == "MODIFIED" and .object.status.phase == "Succeeded" and (.object.status.conditions |
  any(.type == "WorkspaceDeleted" and .status == "True")))' "$WORK/ttl-watch.out" >/dev/null || fail "template test e2e-ttl was removed before it passed"
DRY_ROWS=$(db_count workspaces name "$DRY_WS") || fail "cannot count the workspaces rows named $DRY_WS"
[[ $DRY_ROWS == 0 ]] || fail "the server dry run created workspace $DRY_WS"
phase ttl

step "tester API keys after the template tests (plan A4: recorded, not enforced)"
KEYS_AFTER=$(db_count api_keys user_id "$TESTER_ID") || fail "cannot count the api_keys rows of the tester after the tests"
log "tester api_keys: before=$KEYS_BEFORE after=$KEYS_AFTER delta=$((KEYS_AFTER - KEYS_BEFORE))"

# Last: it scales Coder to zero and deletes namespace $NS. Later CI steps must not need either.
step "namespace deletion releases a running test while Coder is unreachable (plan A1, A13.2)"
tt_create e2e-ns-delete "$(param startup_delay 120)" || fail "cannot create template test e2e-ns-delete"
tt_waiting() { tt_state e2e-ns-delete && [[ $TT_S == "Running WaitingForAgents "* ]]; }
wait_until "template test e2e-ns-delete at Running/WaitingForAgents" tt_waiting
# spec.replicas is the operator's desired state for deploy/coder: the operator keeps it at zero instead of undoing it.
k -n "$NS" patch codercontrolplane coder --type=merge -p '{"spec":{"replicas":0}}' >/dev/null || fail "cannot scale Coder to zero"
coder_down() { k -n "$NS" get deploy coder -o json | jq -e '.spec.replicas == 0 and (.status.replicas // 0) == 0' >/dev/null; }
wait_until "deploy/coder without pods" coder_down
tt_unavailable() { tt_state e2e-ns-delete && [[ $TT_S == "Running CoderUnavailable "* ]]; }
wait_until "template test e2e-ns-delete to report CoderUnavailable" tt_unavailable
T0=$SECONDS
k delete namespace "$NS" --wait=false >/dev/null || fail "cannot delete namespace $NS"
gone_obj() { ! k -n "$NS" get "$1" "$2" -o name >/dev/null 2>"$WORK/obj.err" && grep -q NotFound "$WORK/obj.err"; }
NS_SEEN=""
ns_gone() { # the test and the control plane go first, then the namespace itself disappears (#209)
  tt_state e2e-ns-delete || true # logs the ControlPlaneGone release while the object still exists
  gone_obj codertemplatetest e2e-ns-delete && gone_obj codercontrolplane coder || return 1
  if k get namespace "$NS" -o json >"$WORK/ns.json" 2>"$WORK/ns.err"; then
    local seen # diagnostics: the phase and every True condition, logged when they change
    seen=$(jq -r '[.status.phase // "unknown"] + [.status.conditions[]? | select(.status == "True") |
      "\(.type): \(.message)"] | join("; ")' "$WORK/ns.json") || seen="unparsable namespace JSON"
    [[ $seen == "$NS_SEEN" ]] || log "namespace $NS still exists: $seen"
    NS_SEEN=$seen
    return 1
  fi
  grep -q NotFound "$WORK/ns.err"
}
TIMEOUT=$NS_DELETE_TIMEOUT wait_until "test, control plane, and namespace $NS deleted (ControlPlaneGone release)" ns_gone
NS_DELETE_SECONDS=$((SECONDS - T0))
log "namespace $NS deleted in ${NS_DELETE_SECONDS}s, after its test and control plane"
CASES+=("case: $CURRENT = passed") && CURRENT="" && RESULT=PASS
log "PASS: workspace lifecycle"
