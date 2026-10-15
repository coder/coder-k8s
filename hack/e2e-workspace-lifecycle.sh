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
# "<source> v<version>" that terraform init must install from a filesystem mirror in the agent build (#369).
# Empty: no check, for runs without a mirror.
TF_MIRROR_PROVIDER=${E2E_EXPECT_TF_MIRROR_PROVIDER:-}
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
  rm -f "$WORK/coder.hdr" "$WORK/coder-body.json" "$WORK/tester.json" "$WORK"/ai-key-* "$WORK"/secret-*.json "$WORK/ai-leak.txt" "$WORK/coder-raw.json" "$WORK/member.hdr"
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
coder_status() { # <path>: a GET without -f that prints the HTTP status; the body goes to $WORK/coder-raw.json
  curl -sS --max-time 30 -o "$WORK/coder-raw.json" -w '%{http_code}' -H @"$WORK/coder.hdr" "$CODER_URL$1"
}
CODER_NOT_FOUND="Resource not found or you do not have access to this resource" # Coder's own 404 message (#325)

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
if [[ -n $TF_MIRROR_PROVIDER ]]; then
  # Terraform 1.15 reports a filesystem-mirror install as unauthenticated and a registry install as signed.
  # With a lock file it takes the provider from Coder's shared plugin cache, which only the mirror can fill.
  k get --raw "$API/$AGENT_NAME/log" >"$WORK/agent-build.log" || fail "cannot read coderworkspaces/log of $AGENT_NAME"
  ! grep -qF "Installed provider version: $TF_MIRROR_PROVIDER (signed" "$WORK/agent-build.log" ||
    fail "the agent build installed $TF_MIRROR_PROVIDER from the registry"
  grep -qF -e "Installed provider version: $TF_MIRROR_PROVIDER (unauthenticated)" \
    -e "$TF_MIRROR_PROVIDER: Using from the shared cache directory" "$WORK/agent-build.log" ||
    fail "the agent build did not install $TF_MIRROR_PROVIDER from the provider mirror (log contents not printed)"
  log "agent build: $TF_MIRROR_PROVIDER installed from the provider mirror or its cache"
fi
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

# db_count <table> [<column> <value>]: rows of a Coder table (deleted rows included) through psql in the CNPG
# primary. A read-only count: it never selects key values or hashes.
db_count() {
  local pod where=""
  if (($# > 1)); then
    [[ $3 =~ ^[A-Za-z0-9-]+$ ]] || fail "assertion failed: unsafe value for $1.$2: '$3'"
    where=" WHERE $2 = '$3'"
  fi
  pod=$(k -n "$NS" get cluster coder-db -o json | jq -er '.status.currentPrimary') || return 1
  k -n "$NS" exec "$pod" -c postgres -- psql -d coder -XtAc "SELECT count(*) FROM $1$where" | grep -Ex '[0-9]+'
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

# CoderAIProvider phases (#154, plan 8.2 P1-P4, P6). Keys are synthetic and stay in 0600 files: never argv or output.
AI=e2e-openai AI_SECRET=e2e-ai-key AI_ORIGIN=http://ai-upstream.invalid:8080
gone_obj() { ! k -n "$NS" get "$1" "$2" -o name >/dev/null 2>"$WORK/obj.err" && grep -q NotFound "$WORK/obj.err"; }
new_key() { # <file>: 48 random hex characters
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$WORK/$1" && [[ $(wc -c <"$WORK/$1") == 48 ]] || return 1
  [[ ${GITHUB_ACTIONS:-} != true ]] || echo "::add-mask::$(<"$WORK/$1")"
}
key_secret() { # <create|replace> <name> <key file> <labels JSON>: an Opaque Secret for $AI_ORIGIN that holds the key file
  jq -n --arg n "$2" --arg ns "$NS" --rawfile v "$WORK/$3" --argjson l "$4" --arg o "$AI_ORIGIN" '{apiVersion: "v1", kind: "Secret",
    metadata: {name: $n, namespace: $ns, labels: $l, annotations: {"coder.com/credential-destinations": $o}}, type: "Opaque",
    data: {"api-key": ($v | @base64)}}' >"$WORK/secret-$2.json" && k "$1" -f "$WORK/secret-$2.json" >/dev/null
}
ai_provider() { # <name> <Secret> <key> [baseURL]: kubectl apply of an openai-compat CoderAIProvider
  jq -n --arg n "$1" --arg ns "$NS" --arg s "$2" --arg k "$3" --arg u "${4:-$AI_ORIGIN/v1}" '{apiVersion: "coder.com/v1alpha1",
    kind: "CoderAIProvider", metadata: {name: $n, namespace: $ns}, spec: {controlPlaneRef: {name: "coder"}, type: "openai-compat",
    baseURL: $u, displayName: ("E2E " + $n), apiKeySecretRef: {name: $s, key: $k}}}' >"$WORK/ai-$1.json" || fail "cannot render CoderAIProvider $1"
  apply_manifest "$WORK/ai-$1.json"
}
AI_SEEN="" AI_S=""
ai_state() { # <name>: reads the object into $WORK/ai-<name>.state.json; sets AI_S to "<Ready status> <reason> stalled=<status>"
  k -n "$NS" get coderaiprovider "$1" -o json >"$WORK/ai-$1.state.json" 2>"$WORK/ai.err" || return 1
  AI_S=$(jq -r '.metadata.generation as $g | [.status.conditions[]? | select(.observedGeneration == $g)] as $c |
    "\([$c[] | select(.type == "Ready") | "\(.status) \(.reason)"][0] // "- -") stalled=\([$c[] | select(.type == "Stalled") | .status][0] // "-")"' \
    "$WORK/ai-$1.state.json") || return 1
  [[ "$1 $AI_S" == "$AI_SEEN" ]] || { AI_SEEN="$1 $AI_S" && log "AI provider $1: $AI_S"; }
}
ai_is() { # <name> <Ready status> <reason>: true once Ready has them for the current generation; another stall fails at once
  ai_state "$1" || return 1
  [[ $AI_S == "$2 $3 "* ]] && return 0
  [[ $AI_S != *stalled=True ]] || fail "AI provider $1 stalled with $AI_S, want Ready=$2 $3: $(jq -r '.status.conditions[0].message' "$WORK/ai-$1.state.json")"
  return 1
}
ai_matches() { # <key file> <display name>: Coder's provider $AI_ID is enabled with the spec and only that key; sets AI_KEY_ID
  coder_api GET "/api/v2/ai/providers/$AI_ID" >"$WORK/ai-coder.json" || return 1
  AI_KEY_ID=$(jq -er --arg n "$AI" --arg d "$2" --arg u "$AI_ORIGIN/v1" --rawfile v "$WORK/$1" 'select(.name == $n and
    .type == "openai-compat" and .enabled and .display_name == $d and .base_url == $u and (.api_keys | length) == 1 and
    .api_keys[0].masked == $v[0:4] + "..." + $v[-4:]) | .api_keys[0].id' "$WORK/ai-coder.json")
}
ai_coder_seen() { jq -c 'del(.api_keys[]?.masked)' "$WORK/ai-coder.json"; } # the answer without key masks, for messages
ai_list() { coder_api GET /api/v2/ai/providers | jq -ce 'map({id, name, enabled, display_name, base_url, updated_at,
  keys: [.api_keys[]?.id]}) | sort_by(.name)'; }

step "CoderAIProvider: a provider on an opted-in Secret is Ready; Coder holds the spec and the masked key (plan 8.2 P1)"
T0=$SECONDS
new_key ai-key-1 || fail "cannot generate the first AI provider key"
key_secret create "$AI_SECRET" ai-key-1 '{"coder.com/credential": "true"}' || fail "cannot create Secret $AI_SECRET"
ai_provider "$AI" "$AI_SECRET" api-key
wait_until "AI provider $AI Ready" ai_is "$AI" True Applied
AI_ID=$(jq -er '.status.providerID' "$WORK/ai-$AI.state.json") || fail "AI provider $AI is Ready without status.providerID"
ai_matches ai-key-1 "E2E $AI" || fail "Coder provider $AI_ID does not match the spec and the first key: $(ai_coder_seen)"
FIRST_KEY_ID=$AI_KEY_ID
{ cat "$WORK/ai-key-1" && echo && jq -r '.api_keys[0].masked' "$WORK/ai-coder.json"; } >"$WORK/ai-leak.txt"
! grep -qFf "$WORK/ai-leak.txt" "$WORK/ai-$AI.state.json" || fail "CoderAIProvider $AI holds its key or the key mask"
phase ai-provider

step "CoderAIProvider rotation: a new Secret value sends one new key; a metadata-only change sends none (plan 8.2 P2)"
T0=$SECONDS
new_key ai-key-2 || fail "cannot generate the second AI provider key"
key_secret replace "$AI_SECRET" ai-key-2 '{"coder.com/credential": "true"}' || fail "cannot write the second key to Secret $AI_SECRET"
rotated() { ai_matches ai-key-2 "E2E $AI" && [[ $AI_KEY_ID != "$FIRST_KEY_ID" ]]; }
wait_until "Coder to hold the second key of $AI" rotated
ROTATED_KEY_ID=$AI_KEY_ID
key_recorded() { ai_state "$AI" && jq -e --arg id "$ROTATED_KEY_ID" '.status.credential.keyID == $id' "$WORK/ai-$AI.state.json" >/dev/null; }
wait_until "AI provider $AI to record key $ROTATED_KEY_ID" key_recorded
SECRET_RV=$(k -n "$NS" annotate secret "$AI_SECRET" e2e.coder.com/touched=1 -o json | jq -er '.metadata.resourceVersion') ||
  fail "cannot annotate Secret $AI_SECRET"
rv_recorded() { ai_state "$AI" && jq -e --arg rv "$SECRET_RV" '.status.credential.secretResourceVersion == $rv' "$WORK/ai-$AI.state.json" >/dev/null; }
wait_until "AI provider $AI to record Secret resourceVersion $SECRET_RV" rv_recorded
ai_matches ai-key-2 "E2E $AI" || fail "Coder provider $AI_ID lost the second key: $(ai_coder_seen)"
[[ $AI_KEY_ID == "$ROTATED_KEY_ID" ]] || fail "a metadata-only Secret change sent a new key ($ROTATED_KEY_ID -> $AI_KEY_ID)"
k -n "$OP_NS" logs deploy/coder-k8s >"$WORK/operator-ai.txt" || fail "cannot read the operator log"
LEAKS=$({ cat "$WORK/ai-key-1" && echo && cat "$WORK/ai-key-2"; } | grep -cFf - "$WORK/operator-ai.txt" || true)
[[ $LEAKS == 0 ]] || fail "the operator log holds an AI provider key ($LEAKS lines)"
phase ai-rotation

step "CoderAIProvider refusals: an unlabeled Secret, the operator token, a foreign origin and a taken name write nothing to Coder (plan 8.2 P3)"
T0=$SECONDS
coder_api POST /api/v2/ai/providers "$(jq -nc --arg u "$AI_ORIGIN/v1" '{type: "openai-compat", name: "e2e-taken", base_url: $u, enabled: false}')" \
  >/dev/null || fail "cannot create provider e2e-taken through the Coder API"
new_key ai-key-3 || fail "cannot generate the third AI provider key"
key_secret create e2e-ai-key-unlabeled ai-key-3 '{}' || fail "cannot create Secret e2e-ai-key-unlabeled"
AI_BEFORE=$(ai_list) || fail "cannot list the Coder AI providers"
ai_provider e2e-p3-unlabeled e2e-ai-key-unlabeled api-key
ai_provider e2e-p3-refused "$SECRET" "$KEY"
ai_provider e2e-p3-origin "$AI_SECRET" api-key http://other.invalid:8080/v1
ai_provider e2e-taken "$AI_SECRET" api-key
for c in e2e-p3-unlabeled:CredentialNotOptedIn e2e-p3-refused:CredentialRefused e2e-p3-origin:CredentialDestinationNotAllowed e2e-taken:NameConflict; do
  wait_until "AI provider ${c%%:*} to stall with ${c#*:}" ai_is "${c%%:*}" False "${c#*:}"
  jq -e '(.status.providerID // "") == ""' "$WORK/ai-${c%%:*}.state.json" >/dev/null || fail "refused AI provider ${c%%:*} has a providerID"
done
AI_AFTER=$(ai_list) || fail "cannot list the Coder AI providers after the refusals"
[[ $AI_AFTER == "$AI_BEFORE" ]] || fail "a refused CoderAIProvider wrote to Coder: before=$AI_BEFORE after=$AI_AFTER"
k -n "$NS" delete coderaiprovider e2e-p3-unlabeled e2e-p3-refused e2e-p3-origin e2e-taken --wait=false >/dev/null ||
  fail "cannot delete the refused CoderAIProviders"
for c in e2e-p3-unlabeled e2e-p3-refused e2e-p3-origin e2e-taken; do wait_until "CoderAIProvider $c to disappear" gone_obj coderaiprovider "$c"; done
AI_AFTER=$(ai_list) || fail "cannot list the Coder AI providers after deleting the refused objects"
[[ $AI_AFTER == "$AI_BEFORE" ]] || fail "deleting a refused CoderAIProvider wrote to Coder: before=$AI_BEFORE after=$AI_AFTER"
phase ai-refusals

step "CoderAIProvider kill switch: a disable in Coder stalls the object; a spec edit enables the provider again (plan 8.2 P4)"
T0=$SECONDS
coder_api PATCH "/api/v2/ai/providers/$AI_ID" '{"enabled":false}' >/dev/null || fail "cannot disable provider $AI in Coder"
k -n "$NS" annotate coderaiprovider "$AI" e2e.coder.com/poke=1 >/dev/null || fail "cannot annotate CoderAIProvider $AI"
wait_until "AI provider $AI to stall with DisabledExternally" ai_is "$AI" False DisabledExternally
coder_api GET "/api/v2/ai/providers/$AI_ID" >"$WORK/ai-coder.json" || fail "cannot read provider $AI_ID"
jq -e '.enabled == false' "$WORK/ai-coder.json" >/dev/null || fail "the controller undid the disable of provider $AI: $(ai_coder_seen)"
k -n "$NS" patch coderaiprovider "$AI" --type=merge -p '{"spec":{"displayName":"E2E edited"}}' >/dev/null ||
  fail "cannot edit spec.displayName of CoderAIProvider $AI"
wait_until "AI provider $AI Ready after the spec edit" ai_is "$AI" True Applied
ai_matches ai-key-2 "E2E edited" && [[ $AI_KEY_ID == "$ROTATED_KEY_ID" ]] ||
  fail "Coder provider $AI_ID is not enabled with the edited spec and the second key: $(ai_coder_seen)"
phase ai-kill-switch

step "CoderAIProvider delete: deleting the object deletes the provider in Coder (plan 8.2 P6)"
T0=$SECONDS
k -n "$NS" delete coderaiprovider "$AI" --wait=false >/dev/null || fail "cannot delete CoderAIProvider $AI"
wait_until "CoderAIProvider $AI to disappear" gone_obj coderaiprovider "$AI"
CODE=$(coder_status "/api/v2/ai/providers/$AI_ID") || fail "cannot call GET provider $AI_ID after the delete"
[[ $CODE != 200 ]] || fail "provider $AI ($AI_ID) still exists in Coder after its object was deleted"
# Only Coder's own 404 proves the delete (#348): a proxy or a wrong route also answers 404.
jq -e --arg c "$CODE" --arg m "$CODER_NOT_FOUND" '$c == "404" and .message == $m' "$WORK/coder-raw.json" >/dev/null 2>&1 ||
  fail "GET provider $AI_ID after the delete answered HTTP $CODE without Coder's own 404 message: $(head -c 300 "$WORK/coder-raw.json")"
phase ai-delete

# CoderAIGatewayKey phases (#156, plan 9). Kind runs Coder without a license, so they test the unlicensed path only:
# the controller stalls and sends no key create. The synthetic Secret value stays in a 0600 file: never argv or output.
GW_SEEN="" GW_S=""
gw_key() { # <name> <Secret>: kubectl apply of a CoderAIGatewayKey of control plane coder
  jq -n --arg n "$1" --arg ns "$NS" --arg s "$2" '{apiVersion: "coder.com/v1alpha1", kind: "CoderAIGatewayKey",
    metadata: {name: $n, namespace: $ns}, spec: {controlPlaneRef: {name: "coder"}, secretName: $s}}' >"$WORK/gw-$1.json" ||
    fail "cannot render CoderAIGatewayKey $1"
  apply_manifest "$WORK/gw-$1.json"
}
gw_state() { # <name>: reads the object into $WORK/gw-<name>.state.json; sets GW_S to "<Stalled status> <reason>" of the current generation
  k -n "$NS" get coderaigatewaykey "$1" -o json >"$WORK/gw-$1.state.json" 2>"$WORK/gw.err" || return 1
  GW_S=$(jq -r '.metadata.generation as $g | if .status.observedGeneration == $g then [.status.conditions[]? |
    select(.type == "Stalled" and .observedGeneration == $g) | "\(.status) \(.reason)"][0] // "- -" else "- -" end' "$WORK/gw-$1.state.json") || return 1
  [[ "$1 $GW_S" == "$GW_SEEN" ]] || { GW_SEEN="$1 $GW_S" && log "AI Gateway key $1: $GW_S"; }
}
gw_is() { # <name> <reason> [earlier reason]: true once Stalled=True has reason for the current generation; another stall fails at once
  gw_state "$1" || return 1
  [[ $GW_S == "True $2" ]] && return 0
  [[ $GW_S != True* || $GW_S == "True ${3:-}" ]] ||
    fail "AI Gateway key $1 stalled with $GW_S, want $2: $(jq -r '[.status.conditions[] | select(.type == "Stalled")][0].message' "$WORK/gw-$1.state.json")"
  return 1
}
gw_rows() { # Coder holds no AI Gateway key: the ai_gateway_keys table has 0 rows
  local n
  n=$(db_count ai_gateway_keys) || fail "cannot count the ai_gateway_keys rows"
  [[ $n == 0 ]] || fail "Coder holds $n ai_gateway_keys rows, want 0"
}
gw_clean() { # <name>: the object sent no create (nextSerial 0, no current or pending key), and Coder holds no key
  jq -e '(.status.nextSerial // 0) == 0 and .status.currentKey == null and .status.pendingKey == null' "$WORK/gw-$1.state.json" >/dev/null ||
    fail "AI Gateway key $1 has key state: nextSerial=$(jq -r '.status.nextSerial // 0' "$WORK/gw-$1.state.json")"
  gw_rows
}
secret_seen() { # <name>: resourceVersion, ownerReferences and a SHA-256 of the data, never the data itself
  local s
  s=$(k -n "$NS" get secret "$1" -o json) || return 1
  printf '%s data=%s\n' "$(jq -r '"rv=\(.metadata.resourceVersion) owners=\(.metadata.ownerReferences // [] | tojson)"' <<<"$s")" \
    "$(jq -j '.data | tojson' <<<"$s" | sha256sum | cut -d' ' -f1)"
}

step "CoderAIGatewayKey gate: unlicensed Coder has no aibridge feature and answers the key LIST 403 Premium (plan 9.1)"
T0=$SECONDS
coder_api GET /api/v2/entitlements >"$WORK/entitlements.json" || fail "cannot read the Coder entitlements"
jq -e '(.features | type) == "object" and (.features.aibridge.enabled // false) == false' "$WORK/entitlements.json" >/dev/null ||
  fail "Coder has the aibridge feature enabled: the AI Gateway key phases test the unlicensed path only"
CODE=$(coder_status /api/v2/ai-gateway/keys) || fail "cannot call GET /api/v2/ai-gateway/keys"
jq -e --arg c "$CODE" '$c == "403" and (.message | endswith("is a Premium feature. Contact sales!"))' "$WORK/coder-raw.json" >/dev/null 2>&1 ||
  fail "GET /api/v2/ai-gateway/keys answered HTTP $CODE, want 403 with the Premium message: $(jq -r 'if type == "object" then .message // "no message" else "a body without a message" end' "$WORK/coder-raw.json" 2>/dev/null | head -c 300)"
gw_rows
phase gw-gate

step "CoderAIGatewayKey without the control plane opt-in stalls NotAllowed and creates no Secret (plan 9.2)"
T0=$SECONDS
gw_key e2e-gw-off e2e-gw-key
wait_until "AI Gateway key e2e-gw-off to stall with NotAllowed" gw_is e2e-gw-off NotAllowed
gone_obj secret e2e-gw-key || fail "Secret e2e-gw-key exists while AI Gateway key e2e-gw-off is NotAllowed"
gw_clean e2e-gw-off
phase gw-not-allowed

# The opt-in stays on: no CoderAIGatewayKey is left when the namespace phase runs.
step "CoderAIGatewayKey with the opt-in stalls LicenseRequired: the LIST fails before any Secret claim (plan 9.3)"
T0=$SECONDS
k -n "$NS" patch codercontrolplane coder --type=merge -p '{"spec":{"aiGateway":{"allowKeys":true}}}' >/dev/null ||
  fail "cannot set aiGateway.allowKeys on codercontrolplane coder"
wait_until "AI Gateway key e2e-gw-off to stall with LicenseRequired" gw_is e2e-gw-off LicenseRequired NotAllowed
gone_obj secret e2e-gw-key || fail "Secret e2e-gw-key exists while AI Gateway key e2e-gw-off is LicenseRequired"
gw_clean e2e-gw-off
phase gw-license-required

step "CoderAIGatewayKey refuses an existing Secret that it does not own: SecretNotOwned, the Secret is unchanged (plan 9.4)"
T0=$SECONDS
v=$(head -c 96 /dev/urandom | base64 -w0 | tr -dc 'A-Za-z0-9') && printf '%s' "${v:0:43}" >"$WORK/ai-key-gw" && unset v &&
  [[ $(wc -c <"$WORK/ai-key-gw") == 43 ]] || fail "cannot generate the synthetic AI Gateway key value"
[[ ${GITHUB_ACTIONS:-} != true ]] || echo "::add-mask::$(<"$WORK/ai-key-gw")"
key_secret create e2e-gw-hijack ai-key-gw '{}' || fail "cannot create Secret e2e-gw-hijack"
HIJACK_SEEN=$(secret_seen e2e-gw-hijack) || fail "cannot read Secret e2e-gw-hijack"
gw_key e2e-gw-hijack e2e-gw-hijack
wait_until "AI Gateway key e2e-gw-hijack to stall with SecretNotOwned" gw_is e2e-gw-hijack SecretNotOwned
[[ $(secret_seen e2e-gw-hijack) == "$HIJACK_SEEN" ]] || fail "Secret e2e-gw-hijack changed: before=[$HIJACK_SEEN] after=[$(secret_seen e2e-gw-hijack)]"
gw_clean e2e-gw-hijack
phase gw-secret-not-owned

step "CoderAIGatewayKey leak check: no key-shaped value or Secret value in the operator log or the objects (plan 9.5)"
T0=$SECONDS
k -n "$OP_NS" logs deploy/coder-k8s >"$WORK/operator-gw.txt" || fail "cannot read the operator log"
k -n "$NS" get coderaigatewaykey e2e-gw-off e2e-gw-hijack -o yaml >"$WORK/gw-objects.yaml" || fail "cannot read the AI Gateway key objects"
# A key is a run of exactly 43 letters and digits. Only the log lines of the key controller are searched for one.
LEAKS=$({ grep -i coderaigatewaykey "$WORK/operator-gw.txt" || true; cat "$WORK/gw-objects.yaml"; } |
  grep -cE '(^|[^A-Za-z0-9])[A-Za-z0-9]{43}([^A-Za-z0-9]|$)' || true)
LEAKS=$((LEAKS + $(cat "$WORK/operator-gw.txt" "$WORK/gw-objects.yaml" | grep -cFf "$WORK/ai-key-gw" || true)))
[[ $LEAKS == 0 ]] || fail "the operator log or an AI Gateway key object holds a key-shaped value or the Secret value ($LEAKS lines)"
phase gw-leak-check

step "CoderAIGatewayKey delete: both finalizers go within 30 s without a key call to Coder (plan 9.6)"
T0=$SECONDS
k -n "$NS" delete coderaigatewaykey e2e-gw-off e2e-gw-hijack --wait=false >/dev/null || fail "cannot delete the AI Gateway key objects"
gw_gone() { gone_obj coderaigatewaykey e2e-gw-off && gone_obj coderaigatewaykey e2e-gw-hijack; }
TIMEOUT=$((TIMEOUT < 30 ? TIMEOUT : 30)) wait_until "AI Gateway keys e2e-gw-off and e2e-gw-hijack to disappear" gw_gone
gw_rows
[[ $(secret_seen e2e-gw-hijack) == "$HIJACK_SEEN" ]] || fail "deleting AI Gateway key e2e-gw-hijack changed Secret e2e-gw-hijack"
gone_obj secret e2e-gw-key || fail "Secret e2e-gw-key exists after the delete"
phase gw-delete

# CoderMCPServer phases (#155, plan 7.3 P0-P7, P9). The stub (hack/e2e/mcpstub) is the MCP server and the LLM, and its /report
# holds only SHA-256 hashes of the headers. Keys are synthetic and stay in 0600 files: never argv or output.
MCP=e2e-mcp MCP_SECRET=e2e-mcp-key MCP_STUB_IMAGE=${E2E_MCP_STUB_IMAGE:-e2e.local/mcpstub:e2e}
MCP_HOST=mcp-stub.$NS.svc:8080 MCP_HOST2=mcp-stub-2.$NS.svc:8080 MCPS=/api/v2/organizations/$ORG/mcp-servers
MCP_SEEN="" MCP_S="" MCP_OFF=0 LLM_OFF=0
mcp_server() { # <name> <Secret> <key> [url] [spec JSON]: kubectl apply of an api_key CoderMCPServer (slug = name) that Everyone can read
  local x=${5:-}
  [[ -n $x ]] || x='{}'
  jq -n --arg n "$1" --arg ns "$NS" --arg o "$ORG" --arg s "$2" --arg k "$3" --arg u "${4:-http://$MCP_HOST/mcp}" --argjson x "$x" \
    '{apiVersion: "coder.com/v1alpha1", kind: "CoderMCPServer", metadata: {name: $n, namespace: $ns}, spec: ({controlPlaneRef: {name: "coder"},
    organization: $o, slug: $n, displayName: ("E2E " + $n), url: $u, access: {groups: ["Everyone"]},
    auth: {type: "api_key", apiKey: {header: "X-Api-Key", secretRef: {name: $s, key: $k}}}} + $x)}' >"$WORK/mcp-$1.json" ||
    fail "cannot render CoderMCPServer $1"
  apply_manifest "$WORK/mcp-$1.json"
}
mcp_state() { # <name>: reads the object into $WORK/mcp-<name>.state.json; sets MCP_S to "<Ready status> <reason> stalled=<status>"
  k -n "$NS" get codermcpserver "$1" -o json >"$WORK/mcp-$1.state.json" 2>"$WORK/mcp.err" || return 1
  MCP_S=$(jq -r '.metadata.generation as $g | [.status.conditions[]? | select(.observedGeneration == $g)] as $c |
    "\([$c[] | select(.type == "Ready") | "\(.status) \(.reason)"][0] // "- -") stalled=\([$c[] | select(.type == "Stalled") | .status][0] // "-")"' \
    "$WORK/mcp-$1.state.json") || return 1
  [[ "$1 $MCP_S" == "$MCP_SEEN" ]] || { MCP_SEEN="$1 $MCP_S" && log "MCP server $1: $MCP_S"; }
}
mcp_is() { # <name> <Ready status> <reason>: true once Ready has them for the current generation; another stall fails at once
  mcp_state "$1" || return 1
  [[ $MCP_S == "$2 $3 "* ]] && return 0
  [[ $MCP_S != *stalled=True ]] || fail "MCP server $1 stalled with $MCP_S, want Ready=$2 $3: $(jq -r '.status.conditions[0].message' "$WORK/mcp-$1.state.json")"
  return 1
}
mcp_coder() { coder_api GET "$MCPS/$MCP_ID" >"$WORK/mcp-coder.json"; } # Coder's config of $MCP
mcp_seen() { jq -c '{slug, url, enabled, display_name, has_api_key, updated_at}' "$WORK/mcp-coder.json"; }
mcp_list() { coder_api GET "$MCPS" | jq -ce 'map({id, slug, url, enabled, updated_at}) | sort_by(.slug)'; }
mcp_acl() { coder_api GET "$MCPS/$MCP_ID/acl" | jq -ce '{users: (.users // []), groups: [.groups[]? | {name, role}]}'; }
mcp_rv() { # <Secret>: true once $MCP records the Secret's current resourceVersion
  local rv
  rv=$(k -n "$NS" get secret "$1" -o json | jq -er '.metadata.resourceVersion') || return 1
  mcp_state "$MCP" && jq -e --arg rv "$rv" '.status.credentials[0].secretResourceVersion == $rv' "$WORK/mcp-$MCP.state.json" >/dev/null
}
mcp_report() { k get --raw "/api/v1/namespaces/$NS/services/mcp-stub:8080/proxy/report" >"$WORK/mcp-report.json"; } # API server service proxy
mcp_chat() { # [header file]: POST of a chat of the stub model that selects $MCP_ID; prints the HTTP status, the body goes to $WORK/coder-raw.json
  jq -nc --arg o "$ORG_ID" --arg m "$MODEL_ID" --arg id "$MCP_ID" '{organization_id: $o, model_config_id: $m, mcp_server_ids: [$id],
    content: [{type: "text", text: "E2E: list your tools."}]}' >"$WORK/mcp-chat.json" || return 1
  curl -sS --max-time 30 -o "$WORK/coder-raw.json" -w '%{http_code}' -X POST -H @"${1:-$WORK/coder.hdr}" -H 'Content-Type: application/json' \
    --data @"$WORK/mcp-chat.json" "$CODER_URL/api/v2/chats"
}
REPORT_SEEN=""
mcp_turn() { # true once an LLM request of the new turn lists the tools of $MCP; logs the request counts and methods on change
  local seen
  mcp_report || return 1
  seen=$(jq -c --argjson o "$MCP_OFF" --argjson l "$LLM_OFF" '{mcp: [.mcp[$o:][].method], llm: [.llm[$l:][] | {stream, tools: (.tools | length)}]}' \
    "$WORK/mcp-report.json") || return 1
  [[ $seen == "$REPORT_SEEN" ]] || { REPORT_SEEN=$seen && log "stub report since the chat: $seen"; }
  jq -e --argjson l "$LLM_OFF" --arg p "${MCP}__" 'any(.llm[$l:][].tools[]?; startswith($p))' "$WORK/mcp-report.json" >/dev/null
}
mcp_turn_ok() { # <key file> <host> [header file]: a new chat turn lists the tools at host with only that key; the LLM gets the three stub tools
  local code h
  mcp_report || fail "cannot read the stub report"
  MCP_OFF=$(jq -er '.mcp | length' "$WORK/mcp-report.json") || fail "the stub report has no MCP request list"
  LLM_OFF=$(jq -er '.llm | length' "$WORK/mcp-report.json") || fail "the stub report has no LLM request list"
  code=$(mcp_chat "${3:-}") || fail "cannot create a chat that selects MCP server $MCP"
  [[ $code == 201 ]] || fail "a chat that selects MCP server $MCP answered HTTP $code: $(jq -r '.message?' "$WORK/coder-raw.json" 2>/dev/null | head -c 300)"
  wait_until "the chat turn to list the tools of MCP server $MCP" mcp_turn
  h=$(sha256sum <"$WORK/$1" | cut -d' ' -f1) # only "match" or "no match" is printed
  jq -e --argjson o "$MCP_OFF" --arg h "$h" --arg host "$2" '[.mcp[$o:][] | select(.method == "tools/list")] |
    length > 0 and all(.host == $host and .apiKeyHash == $h)' "$WORK/mcp-report.json" >/dev/null ||
    fail "the tools/list requests of the chat turn do not all reach $2 with the key in $1: key hash no match"
  log "MCP tools/list of the chat turn at $2: key hash match"
  jq -e --argjson l "$LLM_OFF" --arg p "${MCP}__" '[.llm[$l:][].tools[]? | select(startswith($p))] | unique == [$p + "Gamma", $p + "alpha", $p + "beta"]' \
    "$WORK/mcp-report.json" >/dev/null || fail "the LLM did not get exactly the tools ${MCP}__alpha, ${MCP}__beta and ${MCP}__Gamma: $(jq -c \
    --argjson l "$LLM_OFF" '[.llm[$l:][].tools[]?] | unique' "$WORK/mcp-report.json")"
}

step "CoderMCPServer setup: the stub, a stub LLM provider and model, and the opt-in; an unlisted organization is refused (plan 7.3 P0, P4e)"
T0=$SECONDS
coder_api GET /api/v2/entitlements | jq -e '.has_license == false' >/dev/null || fail "Coder has a license: the MCP phases test unlicensed Coder"
jq -n --arg ns "$NS" --arg i "$MCP_STUB_IMAGE" '{apiVersion: "v1", kind: "List", items: ([{apiVersion: "apps/v1", kind: "Deployment",
  metadata: {name: "mcp-stub", namespace: $ns}, spec: {replicas: 1, selector: {matchLabels: {app: "mcp-stub"}}, template: {metadata: {labels: {app: "mcp-stub"}},
  spec: {containers: [{name: "stub", image: $i, imagePullPolicy: "Never", ports: [{containerPort: 8080}],
  readinessProbe: {httpGet: {path: "/healthz", port: 8080}}}]}}}}] + (["mcp-stub", "mcp-stub-2"] | map({apiVersion: "v1", kind: "Service",
  metadata: {name: (.), namespace: $ns}, spec: {selector: {app: "mcp-stub"}, ports: [{port: 8080, targetPort: 8080}]}})))}' >"$WORK/stub-mcp.json" ||
  fail "cannot render the MCP stub manifest"
apply_manifest "$WORK/stub-mcp.json"
stub_ready() { k -n "$NS" get deploy mcp-stub -o json | jq -e '.status.readyReplicas == 1' >/dev/null; }
wait_until "deploy/mcp-stub Ready" stub_ready
PROVIDER_ID=$(coder_api POST /api/v2/ai/providers "$(jq -nc --arg u "http://$MCP_HOST/v1" '{type: "openai-compat", name: "e2e-mcp-llm",
  display_name: "E2E MCP stub", enabled: true, base_url: $u, api_keys: ["e2e-stub-llm-key"]}')" | jq -er '.id') ||
  fail "cannot create the stub LLM provider through the Coder API"
MODEL_ID=$(coder_api POST "/api/v2/organizations/$ORG/chats/models" "$(jq -nc --arg p "$PROVIDER_ID" '{ai_provider_id: $p, model: "stub-model",
  context_limit: 100000, display_name: "E2E stub model", enabled: true, is_default: true}')" | jq -er '.id') || fail "cannot create the stub chat model"
# The chats run as the tester, an organization member with a password login: the operator user does not run chats.
MEMBER_TOKEN=$(coder_api POST /api/v2/users/login "$(jq -nc --arg e "$TESTER@e2e.example.com" --arg p "$TESTER_PASSWORD" '{email: $e, password: $p}')" |
  jq -er '.session_token') || fail "cannot log in as the tester $TESTER"
[[ ${GITHUB_ACTIONS:-} != true ]] || echo "::add-mask::$MEMBER_TOKEN"
printf 'Coder-Session-Token: %s\n' "$MEMBER_TOKEN" >"$WORK/member.hdr" && unset MEMBER_TOKEN
# P4e runs here: unlicensed Coder has one organization, and a list without it later would disable the P1 server.
k -n "$NS" patch codercontrolplane coder --type=merge -p '{"spec":{"mcpServers":{"organizations":["e2e-no-such-org"]}}}' >/dev/null ||
  fail "cannot set mcpServers.organizations on codercontrolplane coder"
MCP_BEFORE=$(mcp_list) || fail "cannot list the Coder MCP server configs"
mcp_server e2e-mcp-unlisted e2e-no-such-secret api-key
wait_until "MCP server e2e-mcp-unlisted to stall with MCPServersNotAllowed" mcp_is e2e-mcp-unlisted False MCPServersNotAllowed
k -n "$NS" delete codermcpserver e2e-mcp-unlisted --wait=false >/dev/null || fail "cannot delete CoderMCPServer e2e-mcp-unlisted"
wait_until "CoderMCPServer e2e-mcp-unlisted to disappear" gone_obj codermcpserver e2e-mcp-unlisted
[[ $(mcp_list) == "$MCP_BEFORE" ]] || fail "a CoderMCPServer of an unlisted organization wrote to Coder"
k -n "$NS" patch codercontrolplane coder --type=merge -p "$(jq -nc --arg o "$ORG" '{spec: {mcpServers: {organizations: [$o]}}}')" >/dev/null ||
  fail "cannot set mcpServers.organizations on codercontrolplane coder"
phase mcp-setup

step "CoderMCPServer: a server on an opted-in Secret is Ready; Coder holds the spec, the key and an Everyone read ACL (plan 7.3 P1)"
T0=$SECONDS
new_key ai-key-mcp-1 || fail "cannot generate the first MCP server key"
AI_ORIGIN=http://$MCP_HOST key_secret create "$MCP_SECRET" ai-key-mcp-1 '{"coder.com/credential": "true"}' || fail "cannot create Secret $MCP_SECRET"
mcp_server "$MCP" "$MCP_SECRET" api-key
wait_until "MCP server $MCP Ready" mcp_is "$MCP" True Applied
MCP_ID=$(jq -er '.status.configID' "$WORK/mcp-$MCP.state.json") || fail "MCP server $MCP is Ready without status.configID"
mcp_coder || fail "cannot read MCP server config $MCP_ID"
jq -e --arg s "$MCP" --arg u "http://$MCP_HOST/mcp" '.slug == $s and .url == $u and .enabled == true and .has_api_key == true' \
  "$WORK/mcp-coder.json" >/dev/null || fail "Coder config $MCP_ID does not match the spec of $MCP: $(mcp_seen)"
ACL=$(mcp_acl) || fail "cannot read the ACL of MCP server config $MCP_ID"
[[ $ACL == '{"users":[],"groups":[{"name":"Everyone","role":"read"}]}' ]] || fail "the ACL of $MCP is not Everyone read only: $ACL"
k -n "$OP_NS" logs deploy/coder-k8s >"$WORK/operator-mcp.txt" || fail "cannot read the operator log"
LEAKS=$(cat "$WORK/mcp-$MCP.state.json" "$WORK/operator-mcp.txt" | grep -cFf "$WORK/ai-key-mcp-1" || true)
[[ $LEAKS == 0 ]] || fail "CoderMCPServer $MCP or the operator log holds the MCP server key ($LEAKS lines)"
phase mcp-apply

step "CoderMCPServer chat: the tester's chat reaches the stub with the key; the LLM gets the stub tools (plan 7.3 P2)"
T0=$SECONDS
mcp_turn_ok ai-key-mcp-1 "$MCP_HOST" "$WORK/member.hdr"
phase mcp-chat

step "CoderMCPServer rotation: the next chat turn carries a new key; a metadata-only Secret change writes nothing to Coder (plan 7.3 P3)"
T0=$SECONDS
new_key ai-key-mcp-2 || fail "cannot generate the second MCP server key"
AI_ORIGIN=http://$MCP_HOST key_secret replace "$MCP_SECRET" ai-key-mcp-2 '{"coder.com/credential": "true"}' ||
  fail "cannot write the second key to Secret $MCP_SECRET"
wait_until "MCP server $MCP to record the second Secret version" mcp_rv "$MCP_SECRET"
wait_until "MCP server $MCP Ready after the rotation" mcp_is "$MCP" True Applied
mcp_turn_ok ai-key-mcp-2 "$MCP_HOST" "$WORK/member.hdr"
mcp_coder || fail "cannot read MCP server config $MCP_ID"
UPDATED=$(jq -er '.updated_at' "$WORK/mcp-coder.json") || fail "Coder config $MCP_ID has no updated_at"
k -n "$NS" annotate secret "$MCP_SECRET" e2e.coder.com/touched=1 >/dev/null || fail "cannot annotate Secret $MCP_SECRET"
wait_until "MCP server $MCP to record the annotated Secret version" mcp_rv "$MCP_SECRET"
mcp_coder || fail "cannot read MCP server config $MCP_ID"
[[ $(jq -r '.updated_at' "$WORK/mcp-coder.json") == "$UPDATED" ]] ||
  fail "a metadata-only Secret change wrote to Coder: updated_at $UPDATED -> $(jq -r '.updated_at' "$WORK/mcp-coder.json")"
phase mcp-rotation

step "CoderMCPServer refusals: an unlabeled Secret, the operator token, a foreign origin, a taken slug and force_on write nothing (plan 7.3 P4)"
T0=$SECONDS
coder_api POST "$MCPS" "$(jq -nc --arg u "http://$MCP_HOST/mcp" '{display_name: "Taken out of band", slug: "e2e-mcp-taken", url: $u,
  transport: "streamable_http", availability: "default_off", enabled: false, auth_type: "none"}')" >/dev/null ||
  fail "cannot create MCP server config e2e-mcp-taken through the Coder API"
new_key ai-key-mcp-3 || fail "cannot generate the third MCP server key"
AI_ORIGIN=http://$MCP_HOST key_secret create e2e-mcp-unlabeled ai-key-mcp-3 '{}' || fail "cannot create Secret e2e-mcp-unlabeled"
MCP_BEFORE=$(mcp_list) || fail "cannot list the Coder MCP server configs"
mcp_server e2e-mcp-unlabeled e2e-mcp-unlabeled api-key
mcp_server e2e-mcp-refused "$SECRET" "$KEY"
mcp_server e2e-mcp-origin "$MCP_SECRET" api-key http://other.invalid:8080/mcp
mcp_server e2e-mcp-taken "$MCP_SECRET" api-key
mcp_server e2e-mcp-forced "$MCP_SECRET" api-key "" '{"availability": "force_on"}'
MCP_REFUSED=(e2e-mcp-unlabeled:CredentialNotOptedIn e2e-mcp-refused:CredentialRefused e2e-mcp-origin:CredentialDestinationNotAllowed
  e2e-mcp-taken:SlugConflict e2e-mcp-forced:ForceOnNotAllowed)
for c in "${MCP_REFUSED[@]}"; do
  wait_until "MCP server ${c%%:*} to stall with ${c#*:}" mcp_is "${c%%:*}" False "${c#*:}"
  jq -e '(.status.configID // "") == ""' "$WORK/mcp-${c%%:*}.state.json" >/dev/null || fail "refused MCP server ${c%%:*} has a configID"
done
[[ $(mcp_list) == "$MCP_BEFORE" ]] || fail "a refused CoderMCPServer wrote to Coder: before=$MCP_BEFORE after=$(mcp_list)"
k -n "$NS" delete codermcpserver "${MCP_REFUSED[@]%%:*}" --wait=false >/dev/null || fail "cannot delete the refused CoderMCPServers"
for c in "${MCP_REFUSED[@]%%:*}"; do wait_until "CoderMCPServer $c to disappear" gone_obj codermcpserver "$c"; done
[[ $(mcp_list) == "$MCP_BEFORE" ]] || fail "deleting a refused CoderMCPServer wrote to Coder: before=$MCP_BEFORE after=$(mcp_list)"
phase mcp-refusals

step "CoderMCPServer URL change: Coder keeps the old URL until the Secret allows the new origin (plan 7.3 P5)"
T0=$SECONDS
k -n "$NS" patch codermcpserver "$MCP" --type=merge -p "{\"spec\":{\"url\":\"http://$MCP_HOST2/mcp\"}}" >/dev/null || fail "cannot change spec.url of $MCP"
wait_until "MCP server $MCP to stall with CredentialDestinationNotAllowed" mcp_is "$MCP" False CredentialDestinationNotAllowed
mcp_coder || fail "cannot read MCP server config $MCP_ID"
jq -e --arg u "http://$MCP_HOST/mcp" '.url == $u and .enabled == true' "$WORK/mcp-coder.json" >/dev/null ||
  fail "Coder config $MCP_ID changed before the Secret allowed the new origin: $(mcp_seen)"
k -n "$NS" annotate secret "$MCP_SECRET" "coder.com/credential-destinations=http://$MCP_HOST,http://$MCP_HOST2" --overwrite >/dev/null ||
  fail "cannot add the second origin to Secret $MCP_SECRET"
wait_until "MCP server $MCP Ready after the consent" mcp_is "$MCP" True Applied
mcp_coder || fail "cannot read MCP server config $MCP_ID"
jq -e --arg u "http://$MCP_HOST2/mcp" '.url == $u and .enabled == true' "$WORK/mcp-coder.json" >/dev/null ||
  fail "Coder config $MCP_ID does not hold the new URL: $(mcp_seen)"
mcp_turn_ok ai-key-mcp-2 "$MCP_HOST2" "$WORK/member.hdr"
phase mcp-url-change

step "CoderMCPServer access: with access.groups [] a member's chat cannot select the server; with [Everyone] it can (plan 7.3 P6)"
T0=$SECONDS
k -n "$NS" patch codermcpserver "$MCP" --type=merge -p '{"spec":{"access":{"groups":[]}}}' >/dev/null || fail "cannot empty access.groups of $MCP"
wait_until "MCP server $MCP Ready with no groups" mcp_is "$MCP" True Applied
CODE=$(mcp_chat "$WORK/member.hdr") || fail "cannot call POST /api/v2/chats as the tester"
jq -e --arg c "$CODE" '$c == "400" and .message == "One or more MCP server IDs are invalid or disabled."' "$WORK/coder-raw.json" >/dev/null 2>&1 ||
  fail "a tester chat that selects $MCP with access.groups [] answered HTTP $CODE, want 400 invalid or disabled"
ACL=$(mcp_acl) || fail "cannot read the ACL of MCP server config $MCP_ID"
[[ $ACL == '{"users":[],"groups":[]}' ]] || fail "the ACL of $MCP is not empty with access.groups []: $ACL"
k -n "$NS" patch codermcpserver "$MCP" --type=merge -p '{"spec":{"access":{"groups":["Everyone"]}}}' >/dev/null || fail "cannot set access.groups of $MCP"
wait_until "MCP server $MCP Ready with Everyone" mcp_is "$MCP" True Applied
mcp_turn_ok ai-key-mcp-2 "$MCP_HOST2" "$WORK/member.hdr"
phase mcp-access

step "CoderMCPServer kill switch: a disable in Coder stalls the object; a spec edit enables the server again (plan 7.3 P7)"
T0=$SECONDS
coder_api PATCH "$MCPS/$MCP_ID" '{"enabled":false}' >/dev/null || fail "cannot disable MCP server config $MCP_ID in Coder"
k -n "$NS" annotate codermcpserver "$MCP" e2e.coder.com/poke=1 >/dev/null || fail "cannot annotate CoderMCPServer $MCP"
wait_until "MCP server $MCP to stall with DisabledExternally" mcp_is "$MCP" False DisabledExternally
mcp_coder || fail "cannot read MCP server config $MCP_ID"
jq -e '.enabled == false' "$WORK/mcp-coder.json" >/dev/null || fail "the controller undid the disable of MCP server $MCP: $(mcp_seen)"
k -n "$NS" patch codermcpserver "$MCP" --type=merge -p '{"spec":{"displayName":"E2E edited"}}' >/dev/null || fail "cannot edit spec.displayName of $MCP"
wait_until "MCP server $MCP Ready after the spec edit" mcp_is "$MCP" True Applied
mcp_coder || fail "cannot read MCP server config $MCP_ID"
jq -e '.enabled == true and .display_name == "E2E edited"' "$WORK/mcp-coder.json" >/dev/null || fail "Coder config $MCP_ID is not enabled with the edit: $(mcp_seen)"
phase mcp-kill-switch

step "CoderMCPServer delete: deleting the object deletes the config in Coder (plan 7.3 P9)"
T0=$SECONDS
k -n "$NS" delete codermcpserver "$MCP" --wait=false >/dev/null || fail "cannot delete CoderMCPServer $MCP"
wait_until "CoderMCPServer $MCP to disappear" gone_obj codermcpserver "$MCP"
CODE=$(coder_status "$MCPS/$MCP_ID") || fail "cannot call GET MCP server config $MCP_ID after the delete"
[[ $CODE != 200 ]] || fail "MCP server config $MCP ($MCP_ID) still exists in Coder after its object was deleted"
jq -e --arg c "$CODE" --arg m "$CODER_NOT_FOUND" '$c == "404" and .message == $m' "$WORK/coder-raw.json" >/dev/null 2>&1 ||
  fail "GET MCP server config $MCP_ID after the delete answered HTTP $CODE without Coder's own 404 message: $(head -c 300 "$WORK/coder-raw.json")"
phase mcp-delete

# Last: it scales Coder to zero and deletes namespace $NS. Later CI steps must not need either.
step "namespace deletion releases a running test while Coder is unreachable (plan A1, A13.2)"
tt_create e2e-ns-delete "$(param startup_delay 120)" || fail "cannot create template test e2e-ns-delete"
tt_waiting() { tt_state e2e-ns-delete && [[ $TT_S == "Running WaitingForAgents "* ]]; }
wait_until "template test e2e-ns-delete at Running/WaitingForAgents" tt_waiting
ai_provider e2e-ns-keep "$AI_SECRET" api-key # plan 8.2 P7: a Ready provider goes with the namespace
wait_until "AI provider e2e-ns-keep Ready" ai_is e2e-ns-keep True Applied
mcp_server e2e-mcp-keep "$MCP_SECRET" api-key # plan 7.3 P9: a Ready MCP server goes with the namespace
wait_until "MCP server e2e-mcp-keep Ready" mcp_is e2e-mcp-keep True Applied
# spec.replicas is the operator's desired state for deploy/coder: the operator keeps it at zero instead of undoing it.
k -n "$NS" patch codercontrolplane coder --type=merge -p '{"spec":{"replicas":0}}' >/dev/null || fail "cannot scale Coder to zero"
coder_down() { k -n "$NS" get deploy coder -o json | jq -e '.spec.replicas == 0 and (.status.replicas // 0) == 0' >/dev/null; }
wait_until "deploy/coder without pods" coder_down
tt_unavailable() { tt_state e2e-ns-delete && [[ $TT_S == "Running CoderUnavailable "* ]]; }
wait_until "template test e2e-ns-delete to report CoderUnavailable" tt_unavailable
T0=$SECONDS
k delete namespace "$NS" --wait=false >/dev/null || fail "cannot delete namespace $NS"
NS_SEEN=""
ns_gone() { # the namespace disappears (#209), and with it the test, the provider, the MCP server and the control plane
  tt_state e2e-ns-delete || true # logs the ControlPlaneGone release while the object still exists
  ai_state e2e-ns-keep || true
  mcp_state e2e-mcp-keep || true
  if k get namespace "$NS" -o json >"$WORK/ns.json" 2>"$WORK/ns.err"; then
    # Diagnostics from the first poll on, so a run shows NamespaceDeletionContentFailure (the aggregated
    # LIST errors while the control plane still exists): the phase and every True condition, on change.
    local seen
    seen=$(jq -r '[.status.phase // "unknown"] + [.status.conditions[]? | select(.status == "True") |
      "\(.type): \(.message)"] | join("; ")' "$WORK/ns.json") || seen="unparsable namespace JSON"
    [[ $seen == "$NS_SEEN" ]] || log "namespace $NS still exists: $seen"
    NS_SEEN=$seen
    return 1
  fi
  grep -q NotFound "$WORK/ns.err" && gone_obj codertemplatetest e2e-ns-delete && gone_obj coderaiprovider e2e-ns-keep &&
    gone_obj codermcpserver e2e-mcp-keep && gone_obj codercontrolplane coder
}
TIMEOUT=$NS_DELETE_TIMEOUT wait_until "test, provider, control plane, and namespace $NS deleted (ControlPlaneGone release)" ns_gone
NS_DELETE_SECONDS=$((SECONDS - T0))
k -n "$OP_NS" logs deploy/coder-k8s >"$WORK/operator-ns.txt" || fail "cannot read the operator log after the namespace deletion"
grep -F 'Releasing the finalizer without a delete in Coder' "$WORK/operator-ns.txt" | grep -F '"ControlPlaneGone"' | grep -qF '"e2e-ns-keep"' ||
  fail "the operator log has no ControlPlaneGone release of AI provider e2e-ns-keep (plan 8.2 P7)"
grep -F 'Releasing the finalizer without a delete in Coder: the MCP server config' "$WORK/operator-ns.txt" | grep -F '"ControlPlaneGone"' |
  grep -qF '"e2e-mcp-keep"' || fail "the operator log has no ControlPlaneGone release of MCP server e2e-mcp-keep (plan 7.3 P9)"
log "namespace $NS deleted in ${NS_DELETE_SECONDS}s, after its test, provider and control plane"
CASES+=("case: $CURRENT = passed") && CURRENT="" && RESULT=PASS
log "PASS: workspace lifecycle"
