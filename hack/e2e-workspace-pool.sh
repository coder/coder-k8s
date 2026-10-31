#!/usr/bin/env bash
# CoderWorkspacePool E2E driver (#153 plan 10.4, phases P1 to P9). The Kind job runs it before the lifecycle driver,
# which deletes namespace coder at its end. It imports its own template from the agent fixture. P8 and P9 delete
# their claims, pools, and members and check that their workspaces are deleted in Coder. Once every phase passed, it
# restores the control plane's spec.workspacePools and the organization's sharing setting. It keeps its two Coder
# users, the pool owner and the Share user, for a rerun. Every wait is bounded. Offline tests with stubbed tools:
# hack/e2e-workspace-pool_test.sh.
set -euo pipefail
umask 077 # $WORK holds the Coder token header and request bodies
shopt -s inherit_errexit

NS=${E2E_NAMESPACE:-coder}
OP_NS=${E2E_OPERATOR_NAMESPACE:-coder-system}
OP_SELECTOR=${E2E_OPERATOR_SELECTOR:-app=coder-k8s}
CP=${E2E_CONTROL_PLANE:-coder}
ORG=${E2E_ORG:-coder}
OWNER=${E2E_POOL_OWNER:-e2e-pool-owner} # password user created by the driver; owns every pool workspace
TEMPLATE=${E2E_POOL_TEMPLATE:-e2e-pool-agent}
AGENT_MANIFEST=${E2E_AGENT_TEMPLATE_MANIFEST:-config/e2e/codertemplate-agent.yaml}
POOL_SAMPLE=config/samples/coder_v1alpha1_coderworkspacepool.yaml
CLAIM_SAMPLE=config/samples/coder_v1alpha1_coderworkspaceclaim.yaml
POOL=${E2E_POOL:-e2e-pool}
CLAIM=${E2E_CLAIM:-e2e-claim}
SHARE_USER=${E2E_SHARE_USER:-e2e-share-user} # second password user created by the driver; P9 shares a workspace with it
SHARE_POOL=${E2E_SHARE_POOL:-e2e-share-pool}
SHARE_CLAIM=${E2E_SHARE_CLAIM:-e2e-share-claim}
NO_GROUP=00000000-0000-4000-8000-00000000e2e9 # a group ID that is not the organization's Everyone group
TEMPLATE_APPLY_TIMEOUT=${E2E_TEMPLATE_APPLY_TIMEOUT:-600s}
CLAIM_READY_SECONDS=${E2E_CLAIM_READY_SECONDS:-10}
USED_SECONDS=${E2E_USED_SECONDS:-180} # Coder flushes usage once a minute, and a member reads Coder once a minute
DRY_RUN_SETTLE_SECONDS=${E2E_DRY_RUN_SETTLE_SECONDS:-10}
TIMEOUT=${E2E_TIMEOUT_SECONDS:-300}
POLL=${E2E_POLL_SECONDS:-2}
PORT=${E2E_CODER_LOCAL_PORT:-13001}
WORK=${E2E_WORKDIR:-$(mktemp -d)}
mkdir -p "$WORK"

CODER_URL=http://127.0.0.1:${PORT}
SELECTOR_LABEL=coder.com/workspace-pool-uid
BG_PIDS=() PHASES=() RESULT=FAIL T0=$SECONDS

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "FAIL: $*" >&2; exit 1; }
k() { kubectl --request-timeout=30s "$@"; } # every non-streaming kubectl request is bounded
phase() { PHASES+=("$1=$((SECONDS - T0))s") && log "=== $1 passed in $((SECONDS - T0))s" && T0=$SECONDS; }
cleanup() {
  local p
  for p in "${BG_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  rm -f "$WORK/coder.hdr" "$WORK/coder-body.json" "$WORK"/{owner,share-user}{.json,-login.json,.hdr}
  printf '%s\n' "=== RECEIPT ($RESULT) ===" "owner=$OWNER/${OWNER_ID:-unset}" "pool_uid=${POOL_UID:-unset}" \
    "claim_ready_ms=${CLAIM_MS:-unset}" "bind_deadline=${BIND_DEADLINE:-unset}" "restart_member=${RESTART_MEMBER:-unset}" \
    "live_workspaces_after_restart=${LIVE:-unset}" "member_workspace_rows=${ROWS:-unset}" "used_member=${USED:-unset}" \
    "used_seconds=${USED_S:-unset}" "scaled_down_workspaces=${SCALED_DOWN:-unset}" "share_user=$SHARE_USER/${SHARE_USER_ID:-unset}" \
    "share_group_refusal=${GROUP_REFUSAL:-unset}" "share_claim_ready_ms=${SHARE_MS:-unset}" "phase_seconds=${PHASES[*]:-unset}" |
    tee "$WORK/receipt.txt"
}
trap cleanup EXIT

for tool in kubectl curl jq base64 date; do command -v "$tool" >/dev/null || fail "missing tool: $tool"; done

# wait_until <description> <command...>: retry until success, bounded by TIMEOUT.
wait_until() {
  local desc=$1 start=$SECONDS && shift
  until "$@"; do
    ((SECONDS - start < TIMEOUT)) || fail "timed out after ${TIMEOUT}s waiting for: $desc"
    sleep "$POLL"
  done
}
coder_api() { # <method> <path> [json-body]: the token and the body reach curl through 0600 files, never argv
  local args=(-fsS --max-time 30 -X "$1" -H @"$WORK/coder.hdr" -H 'Content-Type: application/json')
  [[ $# -lt 3 ]] || { printf '%s' "$3" >"$WORK/coder-body.json" && args+=(--data @"$WORK/coder-body.json"); }
  curl "${args[@]}" "$CODER_URL$2"
}
epoch() { date -u -d "$1" +%s; } # <RFC 3339 time>, fractions and offsets included
deadline_of() { # <Coder workspace JSON>: the latest build's deadline in epoch seconds; a zero deadline fails
  local d
  d=$(jq -r '.latest_build.deadline // ""' <<<"$1")
  [[ -n $d && $d != 0001-01-01T* ]] || fail "workspace $(jq -r .name <<<"$1") has a zero deadline"
  epoch "$d"
}
pool_json() { k -n "$NS" get coderworkspacepool "$POOL" -o json >"$WORK/pool.json"; }
POOL_SEEN=""
pool_ready() { # <ready> <bound>: the current generation reports them with Ready=True; Stalled fails at once
  local seen
  pool_json || return 1
  seen=$(jq -c '.status | {replicas, readyReplicas, boundReplicas, reason}' "$WORK/pool.json")
  [[ $seen == "$POOL_SEEN" ]] || { POOL_SEEN=$seen && log "pool: $seen"; }
  ! jq -e '.status.conditions // [] | any(.type == "Stalled" and .status == "True")' "$WORK/pool.json" >/dev/null ||
    fail "pool stalled: $(jq -c '.status | {reason, message, lastFailure}' "$WORK/pool.json")"
  jq -e --argjson r "$1" --argjson b "$2" '.status.observedGeneration == .metadata.generation and
    (.status.readyReplicas // 0) == $r and (.status.replicas // 0) == $r and (.status.boundReplicas // 0) == $b and
    (.status.conditions // [] | any(.type == "Ready" and .status == "True"))' "$WORK/pool.json" >/dev/null
}
pool_reason() { pool_json && jq -e --arg r "$1" '.status.reason == $r' "$WORK/pool.json" >/dev/null; }
members_json() { k -n "$NS" get coderworkspacepoolmembers -l "$SELECTOR_LABEL=$POOL_UID" -o json >"$WORK/members.json"; }
owner_workspaces() { coder_api GET "/api/v2/workspaces?q=owner:$OWNER" >"$WORK/owner-workspaces.json"; }
operator_pod() { # prints the name of the one Ready non-terminating operator pod
  k -n "$OP_NS" get pods -l "$OP_SELECTOR" -o json | jq -er '[.items[] | select(.metadata.deletionTimestamp == null)] |
    if length == 1 and (.[0].status.conditions // [] | any(.type == "Ready" and .status == "True")) then .[0].metadata.name
    else error("not exactly one Ready operator pod") end' 2>/dev/null
}
render() { # <sample> <jq filter> [jq args...]: the sample as JSON with the filter applied
  local sample=$1 filter=$2 && shift 2
  k create --dry-run=client -o json -f "$sample" | jq -e "$@" "$filter"
}
# password_user <username> <display name> <file prefix>: create the password user in the default organization, or
# reuse it with a new password, activate it, log in once, and keep the session in $WORK/<prefix>.hdr. Sets USER_ID.
password_user() {
  local u=$1 name=$2 f=$WORK/$3 user_json reused="" session
  PASSWORD=$(head -c 24 /dev/urandom | base64) # never logged; used once for the login below
  [[ ${GITHUB_ACTIONS:-} == true ]] && echo "::add-mask::$PASSWORD"
  # The password reaches jq through its environment, which other users cannot read, unlike argv.
  PASSWORD=$PASSWORD jq -nc --arg u "$u" --arg n "$name" --arg o "$ORG_ID" '{username: $u, name: $n,
    email: ($u + "@e2e.example.com"), password: env.PASSWORD, login_type: "password", organization_ids: [$o]}' >"$f.json"
  unset PASSWORD
  if user_json=$(coder_api POST /api/v2/users "$(<"$f.json")" 2>"$f.err"); then :
  elif grep -q 'error: 409' "$f.err"; then
    log "user $u exists (409): reusing it after the same checks"
    reused=1
    user_json=$(coder_api GET "/api/v2/users/$u") || fail "cannot read the existing user $u"
  else fail "cannot create the user $u: $(head -c 300 "$f.err")"; fi
  USER_ID=$(jq -er --arg u "$u" --arg o "$ORG_ID" 'select(.username == $u and .login_type == "password" and
    (.organization_ids | index($o))) | .id' <<<"$user_json") || fail "user $u has an unexpected shape or organization"
  [[ -z $reused ]] || coder_api PUT "/api/v2/users/$USER_ID/password" "$(jq -c '{password}' "$f.json")" \
    >/dev/null || fail "cannot reset the password of the reused user $u"
  # New API users start dormant, and the pool refuses an owner that is not active.
  coder_api PUT "/api/v2/users/$USER_ID/status/activate" | jq -e '.status == "active"' >/dev/null || fail "cannot activate $u"
  # Coder marks an active user dormant at its next check (every 15 min) once its last use is 90 days old, and a new
  # user was never used. One login sets the last use, as a person or the claim consumer would.
  jq -c '{email, password}' "$f.json" >"$f-login.json"
  session=$(curl -fsS --max-time 30 -X POST -H 'Content-Type: application/json' --data @"$f-login.json" \
    "$CODER_URL/api/v2/users/login" | jq -er '.session_token') || fail "cannot log in as $u"
  [[ ${GITHUB_ACTIONS:-} == true ]] && echo "::add-mask::$session"
  printf 'Coder-Session-Token: %s\n' "$session" >"$f.hdr"
  rm -f "$f.json" "$f-login.json" "$WORK/coder-body.json"
}
# allow_owner <shareGroupIDs JSON array>: the control plane allows the owner with the pool template, 10 workspaces,
# and these share groups, next to its original owners
allow_owner() {
  k -n "$NS" patch codercontrolplane "$CP" --type=merge -p "$(jq -nc --argjson orig "$ORIGINAL_POOLS" --arg o "$OWNER_ID" \
    --arg t "$ORG.$TEMPLATE" --argjson g "$1" '{spec: {workspacePools: (($orig // {}) | .owners = ([.owners[]? |
    select(.userID != $o)] + [{userID: $o, templates: [$t], maxWorkspaces: 10, shareGroupIDs: $g}]))}}')" >/dev/null ||
    fail "cannot allowlist $OWNER on $CP"
}

log "=== setup: Coder API access (port-forward, operator token)"
CP_JSON=$(k -n "$NS" get codercontrolplane "$CP" -o json) || fail "cannot read CoderControlPlane $NS/$CP"
SECRET=$(jq -er '.status.operatorTokenSecretRef.name' <<<"$CP_JSON") || fail "no operator token secret ref"
KEY=$(jq -er '.status.operatorTokenSecretRef.key' <<<"$CP_JSON") || fail "no operator token secret key"
ORIGINAL_POOLS=$(jq -c '.spec.workspacePools // null' <<<"$CP_JSON")
TOKEN=$(k -n "$NS" get secret "$SECRET" -o json | jq -er --arg k "$KEY" '.data[$k]' | base64 -d)
[[ -n $TOKEN ]] || fail "operator token is empty"
[[ ${GITHUB_ACTIONS:-} == true ]] && echo "::add-mask::$TOKEN"
printf 'Coder-Session-Token: %s\n' "$TOKEN" >"$WORK/coder.hdr"
unset TOKEN
kubectl -n "$NS" port-forward svc/coder "$PORT:80" >"$WORK/port-forward.log" 2>&1 &
BG_PIDS+=("$!")
coder_ready() { coder_api GET /api/v2/buildinfo >/dev/null 2>&1; }
wait_until "Coder API via port-forward" coder_ready

log "=== P1: pool owner, template with a 1 h TTL, pool refused until the owner is allowed"
ORG_ID=$(coder_api GET /api/v2/organizations | jq -er 'map(select(.is_default)) | if length == 1 then .[0].id else error end') ||
  fail "cannot read the single default organization"
password_user "$OWNER" "E2E pool owner" owner
OWNER_ID=$USER_ID
curl -fsS --max-time 30 -X POST -H @"$WORK/owner.hdr" "$CODER_URL/api/v2/users/logout" >/dev/null || fail "cannot log out $OWNER"
rm -f "$WORK/owner.hdr"
render "$AGENT_MANIFEST" '.metadata.name = $n | .spec.displayName = "E2E Pool Agent Template"' --arg n "$ORG.$TEMPLATE" \
  >"$WORK/template.json" || fail "cannot render the pool template from $AGENT_MANIFEST"
kubectl --request-timeout="$TEMPLATE_APPLY_TIMEOUT" apply -f "$WORK/template.json" >"$WORK/template-apply.out" 2>&1 ||
  fail "cannot apply template $ORG.$TEMPLATE: $(tr -s '\n' ' ' <"$WORK/template-apply.out" | cut -c1-400)"
TEMPLATE_ID=$(coder_api GET "/api/v2/organizations/$ORG/templates/$TEMPLATE" | jq -er '.id') || fail "no Coder template $TEMPLATE"
# Coder answers 304 without a body when the TTL is already 1 h (a rerun), so the GET checks the value.
coder_api PATCH "/api/v2/templates/$TEMPLATE_ID" '{"default_ttl_ms": 3600000}' >/dev/null || fail "cannot set the template TTL"
coder_api GET "/api/v2/templates/$TEMPLATE_ID" | jq -e '.default_ttl_ms == 3600000' >/dev/null || fail "template TTL is not 1 h"
render "$POOL_SAMPLE" '.metadata.name = $n | .metadata.namespace = $ns | .spec.controlPlaneRef.name = $cp |
  .spec.ownerUserID = $o | .spec.template = $t | .spec.replicas = 2' --arg n "$POOL" --arg ns "$NS" --arg cp "$CP" \
  --arg o "$OWNER_ID" --arg t "$ORG.$TEMPLATE" >"$WORK/pool-manifest.json" || fail "cannot render $POOL_SAMPLE"
k create -f "$WORK/pool-manifest.json" >/dev/null || fail "cannot create pool $POOL"
POOL_UID=$(k -n "$NS" get coderworkspacepool "$POOL" -o jsonpath='{.metadata.uid}')
wait_until "pool reason OwnerNotAllowed before the allowlist" pool_reason OwnerNotAllowed
members_json && jq -e '.items | length == 0' "$WORK/members.json" >/dev/null || fail "a pool whose owner is not allowed made members"
allow_owner '[]'
phase P1

log "=== P2: 2 ready members, /scale, non-zero deadlines"
wait_until "pool with 2 ready members" pool_ready 2 0
k get --raw "/apis/coder.com/v1alpha1/namespaces/$NS/coderworkspacepools/$POOL/scale" >"$WORK/scale.json"
jq -e --arg s "$SELECTOR_LABEL=$POOL_UID" '.spec.replicas == 2 and .status.replicas == 2 and .status.selector == $s' \
  "$WORK/scale.json" >/dev/null || fail "/scale does not report 2 replicas and the pool selector: $(jq -c . "$WORK/scale.json")"
members_json
jq -e '[.items[] | select(.metadata.deletionTimestamp == null)] | length == 2' "$WORK/members.json" >/dev/null ||
  fail "want 2 members, got $(jq -c '[.items[].metadata.name]' "$WORK/members.json")"
declare -A PRE_DEADLINE
while read -r name id ws; do
  w=$(coder_api GET "/api/v2/workspaces/$id") || fail "cannot read workspace $id of member $name"
  jq -e --arg o "$OWNER_ID" --arg t "$TEMPLATE_ID" --arg n "$ws" '.owner_id == $o and .template_id == $t and .name == $n' \
    <<<"$w" >/dev/null || fail "workspace $id of member $name is not the pool owner's $ws"
  d=$(deadline_of "$w")
  ((d > $(date +%s))) || fail "member $name has a deadline in the past"
  PRE_DEADLINE[$name]=$d
  log "member $name: workspace $ws deadline in $((d - $(date +%s)))s"
done < <(jq -r '.items[] | "\(.metadata.name) \(.status.workspaceID) \(.status.workspaceName)"' "$WORK/members.json")
phase P2

log "=== P3: kubectl scale to 3, and 101 is refused"
k -n "$NS" scale coderworkspacepool "$POOL" --replicas=3 >/dev/null
wait_until "pool with 3 ready members" pool_ready 3 0
if k -n "$NS" scale coderworkspacepool "$POOL" --replicas=101 >"$WORK/scale-101.out" 2>&1; then
  fail "kubectl scale --replicas=101 succeeded"
fi
grep -q 'less than or equal to 100' "$WORK/scale-101.out" || fail "scale to 101 failed for another reason: $(cat "$WORK/scale-101.out")"
[[ $(k -n "$NS" get coderworkspacepool "$POOL" -o jsonpath='{.spec.replicas}') == 3 ]] || fail "spec.replicas changed after 101"
phase P3

log "=== P4: a Reserve claim binds a ready workspace without a build and gets one deadline extension"
members_json
render "$CLAIM_SAMPLE" '.metadata.name = $n | .metadata.namespace = $ns | .spec.poolName = $p' \
  --arg n "$CLAIM" --arg ns "$NS" --arg p "$POOL" >"$WORK/claim-manifest.json" || fail "cannot render $CLAIM_SAMPLE"
claim_ready() {
  k -n "$NS" get coderworkspaceclaim "$CLAIM" -o json >"$WORK/claim.json" || return 1
  jq -e '.status.phase == "Bound" and (.status.conditions // [] | any(.type == "Ready" and .status == "True"))' \
    "$WORK/claim.json" >/dev/null
}
CLAIM_T0=$(date +%s%N)
k create -f "$WORK/claim-manifest.json" >/dev/null || fail "cannot create claim $CLAIM"
POLL=0.5 TIMEOUT=$CLAIM_READY_SECONDS wait_until "claim Bound and Ready" claim_ready
CLAIM_MS=$((($(date +%s%N) - CLAIM_T0) / 1000000))
((CLAIM_MS <= CLAIM_READY_SECONDS * 1000)) || fail "claim took ${CLAIM_MS}ms to be Bound and Ready, want at most ${CLAIM_READY_SECONDS}s"
log "claim Bound and Ready ${CLAIM_MS}ms after create"
CLAIM_UID=$(jq -er '.metadata.uid' "$WORK/claim.json")
BOUND=$(jq -er '.status.memberName' "$WORK/claim.json")
BOUND_WS=$(jq -er '.status.workspaceID' "$WORK/claim.json")
jq -e --arg o "$OWNER" '.status.ownerName == $o and (.status.workspaceName | startswith("kwp-"))' "$WORK/claim.json" >/dev/null ||
  fail "claim status does not name the owner's kwp- workspace: $(jq -c .status "$WORK/claim.json")"
START_BUILD=$(jq -er --arg m "$BOUND" '.items[] | select(.metadata.name == $m and .status.phase == "Ready") | .status.startBuildID' \
  "$WORK/members.json") || fail "claim bound $BOUND, which was not a ready member before the claim"
PRE=${PRE_DEADLINE[$BOUND]:-0}
bind_extended() {
  k -n "$NS" get coderworkspacepoolmember "$BOUND" -o json >"$WORK/bound.json" || return 1
  jq -e '.status.bindExtended == true' "$WORK/bound.json" >/dev/null
}
wait_until "the bind extension of $BOUND" bind_extended
jq -e --arg u "$CLAIM_UID" --arg w "$BOUND_WS" '.spec.claimRef.uid == $u and .status.workspaceID == $w and
  ([.metadata.ownerReferences[]? | select(.controller == true) | .kind] == ["CoderWorkspaceClaim"])' "$WORK/bound.json" >/dev/null ||
  fail "member $BOUND is not held by claim $CLAIM: $(jq -c '{spec: .spec.claimRef, owners: .metadata.ownerReferences}' "$WORK/bound.json")"
w=$(coder_api GET "/api/v2/workspaces/$BOUND_WS") || fail "cannot read the bound workspace"
[[ $(jq -r '.latest_build.id' <<<"$w") == "$START_BUILD" ]] || fail "the claim started a build: latest is $(jq -r '.latest_build.id' <<<"$w")"
BIND_DEADLINE=$(deadline_of "$w")
((BIND_DEADLINE - $(date +%s) >= 59 * 60)) || fail "bound deadline is $((BIND_DEADLINE - $(date +%s)))s ahead, want at least 59 min"
((PRE > 0 && BIND_DEADLINE > PRE)) || fail "the bind did not extend the deadline of $BOUND ($PRE -> $BIND_DEADLINE)"
log "bound deadline moved $((BIND_DEADLINE - PRE))s later, $((BIND_DEADLINE - $(date +%s)))s ahead"
wait_until "3 unclaimed ready members and 1 bound" pool_ready 3 1
phase P4

log "=== P5: operator restart while a member creates makes no second workspace"
k -n "$NS" scale coderworkspacepool "$POOL" --replicas=4 >/dev/null
creating() {
  members_json || return 1
  RESTART_MEMBER=$(jq -ec '[.items[] | select(.spec.claimRef == null and .metadata.deletionTimestamp == null and
    .status.createAttemptTime != null and .status.readyTime == null)] | select(length > 0) | .[0] |
    {name: .metadata.name, phase: .status.phase, reason: .status.reason, workspaceID: (.status.workspaceID // "")}' \
    "$WORK/members.json")
}
POLL=0.5 wait_until "a member that creates its workspace" creating
OLD_POD=$(operator_pod) || fail "no Ready operator pod to delete"
k -n "$OP_NS" delete pod "$OLD_POD" --wait=false >/dev/null
log "deleted operator pod $OLD_POD while member $RESTART_MEMBER"
new_operator() { local p && p=$(operator_pod) && [[ $p != "$OLD_POD" ]]; }
wait_until "a new Ready operator pod" new_operator
settled() { # 4 unclaimed ready and 1 bound, and the owner never has more than 5 live workspaces
  owner_workspaces || return 1
  LIVE=$(jq -er '.workspaces | length' "$WORK/owner-workspaces.json") || return 1
  ((LIVE <= 5)) || fail "the owner has $LIVE live workspaces, want at most 5: $(jq -c '[.workspaces[].name]' "$WORK/owner-workspaces.json")"
  pool_ready 4 1 && ((LIVE == 5))
}
wait_until "4 unclaimed ready members, 1 bound, and 5 live workspaces" settled
members_json
jq -e --slurpfile c "$WORK/owner-workspaces.json" '[.items[] | select(.metadata.deletionTimestamp == null) | .status.workspaceID] | sort
  == ([$c[0].workspaces[] | select(.name | startswith("kwp-")) | .id] | sort) and length == 5' "$WORK/members.json" >/dev/null ||
  fail "the owner's live kwp- workspaces are not exactly the 5 members' workspaces"
# A second create of a member that Coder then deleted leaves a deleted row with that member's name. Coder's API does
# not list deleted workspaces, so count the rows in its database.
NAMES=$(jq -er '[.items[].status.workspaceName | select(test("^kwp-[0-9a-f]{28}$"))] |
  if length == 5 then map(@sh) | join(",") else error("want 5 member workspace names") end' "$WORK/members.json") ||
  fail "the members do not name 5 kwp- workspaces"
[[ $OWNER_ID =~ ^[0-9a-f-]{36}$ ]] || fail "assertion failed: owner ID '$OWNER_ID' is not a UUID"
DB_POD=$(k -n "$NS" get cluster coder-db -o json | jq -er '.status.currentPrimary') || fail "no CloudNativePG primary for coder-db"
ROWS=$(k -n "$NS" exec "$DB_POD" -c postgres -- psql -d coder -XtAc \
  "SELECT count(*) FROM workspaces WHERE owner_id = '$OWNER_ID' AND name IN ($NAMES)" | grep -Ex '[0-9]+') ||
  fail "cannot count the members' workspace rows"
((ROWS == 5)) || fail "the 5 members' workspace names have $ROWS workspace rows (deleted included), want 5"
w=$(coder_api GET "/api/v2/workspaces/$BOUND_WS") || fail "cannot read the bound workspace"
[[ $(deadline_of "$w") == "$BIND_DEADLINE" ]] || fail "the bound deadline changed after the bind extension"
phase P5

log "=== P6: usage on an unclaimed workspace makes its member Used, and the pool releases it"
members_json
USED=$(jq -er '[.items[] | select(.spec.claimRef == null and .status.phase == "Ready")] | .[0].metadata.name' "$WORK/members.json") ||
  fail "no unclaimed ready member to use"
USED_WS=$(jq -er --arg m "$USED" '.items[] | select(.metadata.name == $m) | .status.workspaceID' "$WORK/members.json")
coder_api POST "/api/v2/workspaces/$USED_WS/usage" >/dev/null || fail "cannot post usage for workspace $USED_WS"
released() {
  k -n "$NS" get coderworkspacepoolmember "$USED" -o json >"$WORK/used.json" || return 1
  jq -e '(.status.conditions // [] | any(.type == "Used" and .status == "True")) and
    ([.metadata.ownerReferences[]? | select(.controller == true)] | length == 0)' "$WORK/used.json" >/dev/null
}
USED_T0=$SECONDS
TIMEOUT=$USED_SECONDS wait_until "member $USED Used=True and released by the pool" released
USED_S=$((SECONDS - USED_T0))
log "member $USED Used=True and released ${USED_S}s after the usage request"
wait_until "4 unclaimed ready members and 1 bound after the release" pool_ready 4 1
jq -e '.status.usedReplicas == 1' "$WORK/pool.json" >/dev/null || fail "pool usedReplicas is not 1: $(jq -c .status "$WORK/pool.json")"
members_json
phase P6

log "=== P7: scale to 1 deletes the extra unclaimed workspaces in Coder; the bound and the used one stay"
UNCLAIMED=$(jq -c --arg m "$USED" '[.items[] | select(.spec.claimRef == null and .metadata.name != $m) | .status.workspaceID]' \
  "$WORK/members.json")
k -n "$NS" scale coderworkspacepool "$POOL" --replicas=1 >/dev/null
wait_until "1 unclaimed ready member and 1 bound" pool_ready 1 1
three_members() { members_json && jq -e '.items | length == 3' "$WORK/members.json" >/dev/null; }
wait_until "3 members: 1 unclaimed, 1 bound, 1 used" three_members
KEPT_WS=$(jq -er --arg m "$USED" '[.items[] | select(.spec.claimRef == null and .metadata.name != $m)] |
  if length == 1 then .[0].status.workspaceID else error end' "$WORK/members.json") || fail "want 1 unclaimed member after the scale to 1"
deleted_in_coder() { # <workspace ID>: an include_deleted read shows a succeeded delete build
  coder_api GET "/api/v2/workspaces/$1?include_deleted=true" |
    jq -e '.latest_build.transition == "delete" and .latest_build.job.status == "succeeded"' >/dev/null
}
live_in_coder() { coder_api GET "/api/v2/workspaces/$1" | jq -e '.latest_build.transition == "start"' >/dev/null; }
SCALED_DOWN=0
for ws in $(jq -r --arg kept "$KEPT_WS" '.[] | select(. != $kept)' <<<"$UNCLAIMED"); do
  wait_until "workspace $ws deleted in Coder" deleted_in_coder "$ws"
  SCALED_DOWN=$((SCALED_DOWN + 1))
done
((SCALED_DOWN == 3)) || fail "the scale to 1 deleted $SCALED_DOWN workspaces, want 3"
for ws in "$KEPT_WS" "$BOUND_WS" "$USED_WS"; do live_in_coder "$ws" || fail "workspace $ws is not live after the scale to 1"; done
phase P7

log "=== P8: claim, pool, and used member deletes; a server dry run creates nothing"
absent() { local out; ! out=$(k -n "$NS" get "$@" -o name 2>&1) && grep -q '(NotFound)' <<<"$out"; } # <kind> <name>
member_gone() { absent coderworkspacepoolmember "$1"; }
k -n "$NS" delete coderworkspaceclaim "$CLAIM" --wait=false >/dev/null
wait_until "member $BOUND of the deleted claim gone" member_gone "$BOUND"
deleted_in_coder "$BOUND_WS" || fail "the claim's workspace $BOUND_WS is not deleted in Coder"
log "claim delete: member $BOUND gone, workspace deleted in Coder"
KEPT=$(jq -er --arg w "$KEPT_WS" '.items[] | select(.status.workspaceID == $w) | .metadata.name' "$WORK/members.json")
k -n "$NS" delete coderworkspacepool "$POOL" --wait=false >/dev/null
wait_until "unclaimed member $KEPT of the deleted pool gone" member_gone "$KEPT"
deleted_in_coder "$KEPT_WS" || fail "the unclaimed workspace $KEPT_WS is not deleted in Coder"
k -n "$NS" get coderworkspacepoolmember "$USED" -o json >"$WORK/used.json" || fail "the used member $USED went with its pool"
jq -e '.metadata.deletionTimestamp == null' "$WORK/used.json" >/dev/null || fail "the pool delete deletes the used member $USED"
live_in_coder "$USED_WS" || fail "the used workspace $USED_WS is not live after the pool delete"
log "pool delete: member $KEPT gone, workspace deleted in Coder; used member $USED and its workspace stay"
k -n "$NS" delete coderworkspacepoolmember "$USED" --wait=false >/dev/null
wait_until "the used member $USED gone" member_gone "$USED"
deleted_in_coder "$USED_WS" || fail "deleting the used member $USED did not delete workspace $USED_WS"
log "used member delete: member $USED gone, workspace deleted in Coder"
k apply --dry-run=server -f "$WORK/pool-manifest.json" >"$WORK/dry-run.out" 2>&1 ||
  fail "server dry run of the pool failed: $(cat "$WORK/dry-run.out")"
grep -q 'server dry run' "$WORK/dry-run.out" || fail "kubectl did not report a server dry run: $(cat "$WORK/dry-run.out")"
nothing_made() { # the pool, a member of it, or a live workspace of the owner after the dry run fails at once
  absent coderworkspacepool "$POOL" || fail "the server dry run created pool $POOL"
  k -n "$NS" get coderworkspacepoolmembers -o json | jq -e --arg p "$POOL" '[.items[] | select(.spec.poolName == $p)] |
    length == 0' >/dev/null || fail "a member of $POOL exists after the server dry run"
  owner_workspaces && jq -e '.workspaces | length == 0' "$WORK/owner-workspaces.json" >/dev/null ||
    fail "the owner has live workspaces after the server dry run"
  ((SECONDS - DRY_T0 >= DRY_RUN_SETTLE_SECONDS))
}
DRY_T0=$SECONDS
wait_until "no pool, member, or workspace ${DRY_RUN_SETTLE_SECONDS}s after the server dry run" nothing_made
phase P8

log "=== P9: a Share claim gives a second user the use role on its workspace, within the organization's sharing policy"
password_user "$SHARE_USER" "E2E share user" share-user
SHARE_USER_ID=$USER_ID
sharing() { coder_api GET "/api/v2/organizations/$ORG_ID/settings/workspace-sharing" | jq -er '.shareable_workspace_owners'; }
set_sharing() { # <shareable_workspace_owners>: the admin's organization setting, which the operator never writes
  coder_api PATCH "/api/v2/organizations/$ORG_ID/settings/workspace-sharing" "$(jq -nc --arg v "$1" '{shareable_workspace_owners: $v}')" |
    jq -e --arg v "$1" '.shareable_workspace_owners == $v' >/dev/null || fail "cannot set the organization's workspace sharing to $1"
}
ORIGINAL_SHARING=$(sharing) || fail "cannot read the organization's workspace sharing setting"
[[ $ORIGINAL_SHARING == everyone ]] || set_sharing everyone
POOL=$SHARE_POOL CLAIM=$SHARE_CLAIM
render "$POOL_SAMPLE" '.metadata.name = $n | .metadata.namespace = $ns | .spec.controlPlaneRef.name = $cp |
  .spec.ownerUserID = $o | .spec.template = $t | .spec.replicas = 1 | .spec.claims.mode = "Share"' --arg n "$POOL" --arg ns "$NS" \
  --arg cp "$CP" --arg o "$OWNER_ID" --arg t "$ORG.$TEMPLATE" >"$WORK/share-pool.json" || fail "cannot render the Share pool"
k create -f "$WORK/share-pool.json" >/dev/null || fail "cannot create the Share pool $POOL"
POOL_UID=$(k -n "$NS" get coderworkspacepool "$POOL" -o jsonpath='{.metadata.uid}')
wait_until "Share pool reason ShareNotAllowed without shareGroupIDs" pool_reason ShareNotAllowed
members_json && jq -e '.items | length == 0' "$WORK/members.json" >/dev/null || fail "a Share pool without shareGroupIDs made members"
# Coder's group routes need a Premium license, so without one only the organization's Everyone group, whose ID is the
# organization ID, admits a user.
allow_owner "[\"$NO_GROUP\"]"
wait_until "Share pool with 1 ready member" pool_ready 1 0
render "$CLAIM_SAMPLE" '.metadata.name = $n | .metadata.namespace = $ns | .spec.poolName = $p | .spec.share.user = $u' \
  --arg n "$CLAIM" --arg ns "$NS" --arg p "$POOL" --arg u "$SHARE_USER" >"$WORK/share-claim.json" || fail "cannot render the Share claim"
k create -f "$WORK/share-claim.json" >/dev/null || fail "cannot create the Share claim $CLAIM"
claim_waits() { # <reason>: the claim is Pending with this reason
  k -n "$NS" get coderworkspaceclaim "$CLAIM" -o json >"$WORK/claim.json" || return 1
  jq -e --arg r "$1" '.status.phase == "Pending" and .status.reason == $r' "$WORK/claim.json" >/dev/null
}
wait_until "Share claim Pending with ShareUserNotEligible for a group that is not Everyone" claim_waits ShareUserNotEligible
GROUP_REFUSAL=$(jq -er '.status.message' "$WORK/claim.json")
grep -qF "$NO_GROUP" <<<"$GROUP_REFUSAL" || fail "the refusal does not name group $NO_GROUP: $GROUP_REFUSAL"
log "claim refused: $GROUP_REFUSAL"
set_sharing none
allow_owner "[\"$ORG_ID\"]"
wait_until "Share pool reason ShareNotAllowed while the organization's sharing is none" pool_reason ShareNotAllowed
jq -e '.status.message | contains("none")' "$WORK/pool.json" >/dev/null || fail "the pool refusal does not name the none setting"
k -n "$NS" get coderworkspaceclaim "$CLAIM" -o json | jq -e '.status.phase == "Pending"' >/dev/null ||
  fail "the Share claim bound while the organization's sharing is none"
SHARE_T0=$(date +%s%N)
set_sharing everyone
wait_until "Share claim Bound and Ready" claim_ready
SHARE_MS=$((($(date +%s%N) - SHARE_T0) / 1000000))
log "Share claim Bound and Ready ${SHARE_MS}ms after sharing was allowed (the pool reads the setting at least once a minute)"
SHARE_WS=$(jq -er '.status.workspaceID' "$WORK/claim.json")
SHARE_MEMBER=$(jq -er '.status.memberName' "$WORK/claim.json")
jq -e --arg u "$SHARE_USER_ID" '.status.sharedUserID == $u' "$WORK/claim.json" >/dev/null ||
  fail "claim sharedUserID is not $SHARE_USER: $(jq -c .status "$WORK/claim.json")"
coder_api GET "/api/v2/workspaces/$SHARE_WS/acl" >"$WORK/acl.json" || fail "cannot read the ACL of $SHARE_WS"
jq -e --arg u "$SHARE_USER_ID" '[.users[] | {id, role}] == [{id: $u, role: "use"}] and ((.group // []) | length == 0)' \
  "$WORK/acl.json" >/dev/null || fail "ACL of $SHARE_WS: $(jq -c . "$WORK/acl.json"), want only $SHARE_USER with use"
curl -fsS --max-time 30 -H @"$WORK/share-user.hdr" "$CODER_URL/api/v2/workspaces/$SHARE_WS" |
  jq -e --arg w "$SHARE_WS" '.id == $w' >/dev/null || fail "$SHARE_USER cannot read the shared workspace $SHARE_WS"
CODE=$(curl -sS -o "$WORK/share-delete.out" -w '%{http_code}' --max-time 30 -X POST -H @"$WORK/share-user.hdr" \
  -H 'Content-Type: application/json' --data '{"transition": "delete"}' "$CODER_URL/api/v2/workspaces/$SHARE_WS/builds")
[[ $CODE == 403 ]] || fail "a delete build with $SHARE_USER's token answered $CODE, want 403"
live_in_coder "$SHARE_WS" || fail "the shared workspace $SHARE_WS is not live after the refused delete"
log "$SHARE_USER has use on $SHARE_WS, reads it, and cannot delete it (403)"
wait_until "1 unclaimed ready member and 1 bound in the Share pool" pool_ready 1 1
members_json
SPARE_WS=$(jq -c --arg m "$SHARE_MEMBER" '[.items[] | select(.metadata.name != $m) | .status.workspaceID]' "$WORK/members.json")
k -n "$NS" delete coderworkspaceclaim "$CLAIM" --wait=false >/dev/null
wait_until "member $SHARE_MEMBER of the deleted Share claim gone" member_gone "$SHARE_MEMBER"
deleted_in_coder "$SHARE_WS" || fail "the Share claim's workspace $SHARE_WS is not deleted in Coder"
k -n "$NS" delete coderworkspacepool "$POOL" --wait=false >/dev/null
pool_members_gone() { members_json && jq -e '.items | length == 0' "$WORK/members.json" >/dev/null; }
wait_until "the members of the deleted Share pool gone" pool_members_gone
for ws in $(jq -r '.[]' <<<"$SPARE_WS"); do deleted_in_coder "$ws" || fail "the unclaimed workspace $ws is not deleted in Coder"; done
[[ $(sharing) == everyone ]] || fail "the organization's sharing setting changed during P9"
[[ $ORIGINAL_SHARING == everyone ]] || set_sharing "$ORIGINAL_SHARING"
curl -fsS --max-time 30 -X POST -H @"$WORK/share-user.hdr" "$CODER_URL/api/v2/users/logout" >/dev/null || fail "cannot log out $SHARE_USER"
rm -f "$WORK/share-user.hdr"
phase P9

log "=== cleanup: restore spec.workspacePools"
k -n "$NS" patch codercontrolplane "$CP" --type=merge -p "$(jq -nc --argjson orig "$ORIGINAL_POOLS" '{spec: {workspacePools: $orig}}')" \
  >/dev/null || fail "cannot restore spec.workspacePools of $CP"
kubectl -n "$OP_NS" wait --for=condition=Available deploy/coder-k8s --timeout=120s >/dev/null
kubectl wait --for=condition=Available apiservice/v1alpha1.aggregation.coder.com --timeout=180s >/dev/null
aggregated_served() { k get --raw /apis/aggregation.coder.com/v1alpha1 >/dev/null 2>&1; }
wait_until "the aggregated API" aggregated_served
phase cleanup
RESULT=PASS
log "PASS: workspace pool"
