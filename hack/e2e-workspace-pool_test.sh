#!/usr/bin/env bash
# Offline tests for hack/e2e-workspace-pool.sh (#153). kubectl and curl are stubs on PATH over one fake cluster and
# Coder in JSON files, with a fake pool controller that runs one pass per stub call. Nothing touches a cluster. The
# pass scenario runs phases P1 to P8. Each other scenario breaks one behavior, and the driver must fail with the
# check that covers it. No scenario may print a secret outside its mask command or pass one in argv.
# shellcheck disable=SC2016 # jq programs are single-quoted on purpose.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DRIVER=${DRIVER:-$ROOT/hack/e2e-workspace-pool.sh}
BASE=$(mktemp -d)
trap 'rm -rf "$BASE"' EXIT
STUBS=$BASE/bin
mkdir -p "$STUBS"
FAILURES=0

cat >"$STUBS/fake.sh" <<'STUB'
# shellcheck shell=bash
S=$STUB_STATE
shopt -s nullglob
mkdir -p "$S/m" "$S/ws"
OWNER_UUID=00000001-0000-4000-8000-000000000001 ORG_UUID=00000002-0000-4000-8000-000000000002
TPL_UUID=00000003-0000-4000-8000-000000000003 POOL_UID=00000004-0000-4000-8000-000000000004
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
edit() { local f=$1 && shift && jq "$@" "$f" >"$f.tmp" && mv "$f.tmp" "$f"; } # <file> <jq args...>
slurp() { local f=("$S/$1"/*.json); if ((${#f[@]})); then jq -s . "${f[@]}"; else echo '[]'; fi; } # <m|ws>: one array
members() { slurp m; }
unused() { jq -e '.spec.claimRef == null and (.status.conditions // [] | length == 0)' "$1" >/dev/null; }
ws_delete() { [[ $SCENARIO == scale-down-keeps-workspace ]] ||
  edit "$S/ws/$1.json" '.deleted = true | .latest_build = {id: "build-delete", transition: "delete", job: {status: "succeeded"}}'; }
member_delete() { # the member finalizer: a person's delete of a released used member deletes its workspace (D6)
  [[ $SCENARIO == used-member-kept ]] && ! unused "$1" && jq -e '.spec.claimRef == null' "$1" >/dev/null ||
    ws_delete "$(jq -r .status.workspaceID "$1")"
  rm -f "$1"
}
member_create() {
  local n id name t d
  n=$(($(cat "$S/counter" 2>/dev/null || echo 10) + 1)) && echo "$n" >"$S/counter"
  id=$(printf '%08x-0000-4000-8000-%012x' "$n" "$n") name=$(printf 'e2e-pool-%03d' "$n") t=$(date +%s)
  d=$(iso $((t + 3500)))
  [[ $SCENARIO != zero-deadline ]] || d=0001-01-01T00:00:00Z
  jq -n --arg n "$name" --arg id "$id" --arg ws "$(printf 'kwp-%028x' "$n")" --arg u "$POOL_UID" --arg t "$(iso "$t")" \
    '{metadata: {name: $n, labels: {"coder.com/workspace-pool-uid": $u}, ownerReferences: [{kind: "CoderWorkspacePool",
    controller: true}]}, spec: {poolName: "e2e-pool", claimRef: null}, status: {phase: "Creating", createAttemptTime: $t,
    workspaceID: $id, workspaceName: $ws, startBuildID: ("build-" + $id)}}' >"$S/m/$name.json"
  jq -n --arg id "$id" --arg ws "$(printf 'kwp-%028x' "$n")" --arg o "$OWNER_UUID" --arg t "$TPL_UUID" --arg d "$d" \
    '{id: $id, name: $ws, owner_id: $o, template_id: $t, deleted: false,
    latest_build: {id: ("build-" + $id), transition: "start", deadline: $d, job: {status: "succeeded"}}}' >"$S/ws/$id.json"
}
reconcile() { # one pass of the fake pool controller
  local m pool=$S/pool.json want unclaimed=() reason=Ready
  for m in "$S"/m/*.json; do # garbage collection after a claim or pool delete
    if jq -e '.spec.claimRef != null' "$m" >/dev/null; then [[ -f $S/claim.json ]] || member_delete "$m"
    elif [[ ! -f $pool ]] && { jq -e '.metadata.ownerReferences | length > 0' "$m" >/dev/null ||
      [[ $SCENARIO == pool-delete-takes-used ]]; }; then member_delete "$m"; fi
  done
  [[ -f $pool ]] || return 0
  if [[ ! -f $S/allowed ]]; then
    reason=OwnerNotAllowed
    [[ $SCENARIO == members-before-allow ]] || { edit "$pool" '.status = {reason: "OwnerNotAllowed"}' && return 0; }
  fi
  for m in "$S"/m/*.json; do
    edit "$m" --arg t "$(iso "$(date +%s)")" 'if .status.phase == "Creating" then .status += {phase: "Ready", readyTime: $t} end'
    if [[ -f $S/usage && $SCENARIO != use-not-detected ]] && unused "$m" && jq -e --arg w "$(<"$S/usage")" \
      --arg s "$SCENARIO" '.status.workspaceID == $w or $s == "false-use"' "$m" >/dev/null; then
      edit "$m" '.status.conditions = [{type: "Used", status: "True"}] | .metadata.ownerReferences = []'
    fi
  done
  rm -f "$S/usage"
  if [[ -f $S/claim.json ]] && jq -e '.status.phase != "Bound"' "$S/claim.json" >/dev/null; then
    for m in "$S"/m/*.json; do
      unused "$m" && jq -e '.status.phase == "Ready"' "$m" >/dev/null || continue
      edit "$m" '.spec.claimRef = {name: "e2e-claim", uid: "claim-uid"} | .status.bindExtended = true |
        .metadata.ownerReferences = [{kind: "CoderWorkspaceClaim", controller: true}]'
      edit "$S/ws/$(jq -r .status.workspaceID "$m").json" --arg d "$(iso $(($(date +%s) + 3600)))" --arg s "$SCENARIO" \
        '.latest_build.deadline = $d | if $s == "bind-builds" then .latest_build.id = "build-claim" end'
      edit "$S/claim.json" --slurpfile m "$m" '.status = ($m[0].status | {phase: "Bound", conditions: [{type: "Ready",
        status: "True"}], workspaceID, workspaceName, ownerName: "e2e-pool-owner"}) + {memberName: $m[0].metadata.name}'
      break
    done
  fi
  for m in "$S"/m/*.json; do ! unused "$m" || unclaimed+=("$m"); done
  want=$(jq .spec.replicas "$pool")
  while ((${#unclaimed[@]} > want)); do member_delete "${unclaimed[0]}" && unclaimed=("${unclaimed[@]:1}"); done
  for ((m = ${#unclaimed[@]}; m < want; m++)); do member_create; done
  edit "$pool" --argjson ms "$(members)" --arg r "$reason" '.status = {observedGeneration: .metadata.generation, reason: $r,
    conditions: [{type: "Ready", status: "True"}], boundReplicas: ([$ms[] | select(.spec.claimRef != null)] | length),
    usedReplicas: ([$ms[] | select(.status.conditions // [] | length > 0)] | length)} | .status += ([$ms[] |
    select(.spec.claimRef == null and (.status.conditions // [] | length == 0))] | {replicas: length,
    readyReplicas: (map(select(.status.phase == "Ready")) | length)})'
}
reconcile
STUB

cat >"$STUBS/kubectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "kubectl $*" >>"$STUB_STATE/calls.log"
# shellcheck source=/dev/null
source "$(dirname "$0")/fake.sh"
out="" file="" patch="" replicas="" dry="" raw="" pos=() exec_args=()
while (($#)); do
  case $1 in
    -o) out=$2 && shift 2 ;;
    -f) file=$2 && shift 2 ;;
    -p) patch=$2 && shift 2 ;;
    --raw) raw=$2 && shift 2 ;;
    -n | -l | -c) shift 2 ;;
    --replicas=*) replicas=${1#*=} && shift ;;
    --dry-run=*) dry=${1#*=} && shift ;;
    --) shift && exec_args=("$@") && break ;;
    -*) shift ;;
    *) pos+=("$1") && shift ;;
  esac
done
notfound() { echo "Error from server (NotFound): $1 \"$2\" not found" >&2; exit 1; }
show() { # <file> <kind> <name>: the object in the -o format
  [[ -f $1 ]] || notfound "$2" "$3"
  case $out in jsonpath=*) out=${out#jsonpath=\{} && jq -r "${out%\}}" "$1" ;; name) echo "$2/$3" ;; *) cat "$1" ;; esac
}
[[ -z $raw ]] || pos=(get raw)
case "${pos[0]:-} ${pos[1]:-}" in
  "port-forward "*) echo "$$" >"$S/pf.pid" && exec sleep 60 ;;
  "get codercontrolplane") jq -n '{spec: {}, status: {operatorTokenSecretRef: {name: "tok", key: "token"}}} |
    if env.SCENARIO == "no-token-ref" then .status = {} end' ;;
  "get secret") printf '{"data": {"token": "%s"}}\n' "$(printf stub-token | base64)" ;;
  "get coderworkspacepool") show "$S/pool.json" coderworkspacepool "${pos[2]}" ;;
  "get coderworkspaceclaim") show "$S/claim.json" coderworkspaceclaim "${pos[2]}" ;;
  "get coderworkspacepoolmember") show "$S/m/${pos[2]}.json" coderworkspacepoolmember "${pos[2]}" ;;
  "get coderworkspacepoolmembers") members | jq '{items: .}' ;;
  "get pods") jq -n --arg p "$(cat "$S/pod" 2>/dev/null || echo coder-k8s-1)" '{items: [{metadata: {name: $p},
    status: {conditions: [{type: "Ready", status: "True"}]}}]}' ;;
  "get cluster") echo '{"status": {"currentPrimary": "coder-db-1"}}' ;;
  "get raw")
    case $raw in
      */scale) jq '{spec: {replicas: .spec.replicas}, status: {replicas: .status.replicas,
        selector: ("coder.com/workspace-pool-uid=" + .metadata.uid)}}' "$S/pool.json" ;;
      *) echo '{}' ;;
    esac ;;
  "create "*)
    if [[ $dry == client ]]; then
      case $file in
        *coderworkspacepool.yaml) echo '{"kind": "CoderWorkspacePool", "metadata": {"name": "x"}, "spec": {"controlPlaneRef": {}}}' ;;
        *coderworkspaceclaim.yaml) echo '{"kind": "CoderWorkspaceClaim", "metadata": {"name": "x"}, "spec": {}}' ;;
        *) echo '{"kind": "CoderTemplate", "metadata": {"name": "x"}, "spec": {}}' ;;
      esac
    elif jq -e '.kind == "CoderWorkspacePool"' "$file" >/dev/null; then
      jq --arg u "$POOL_UID" '.metadata += {uid: $u, generation: 1} | .status = {}' "$file" >"$S/pool.json"
    else jq '.metadata.uid = "claim-uid" | .status = {}' "$file" >"$S/claim.json"; fi ;;
  "apply ")
    [[ $dry != server ]] || { echo "coderworkspacepool.coder.com/e2e-pool created (server dry run)"; [[ $SCENARIO == dry-run-creates ]] ||
      exit 0; jq --arg u "$POOL_UID" '.metadata += {uid: $u, generation: 1} | .status = {}' "$file" >"$S/pool.json"; exit 0; }
    echo "codertemplate.aggregation.coder.com/coder.e2e-pool-agent created" ;;
  "patch codercontrolplane")
    if jq -e '.spec.workspacePools.owners // [] | length > 0' <<<"$patch" >/dev/null; then touch "$S/allowed"; else rm -f "$S/allowed"; fi ;;
  "scale coderworkspacepool")
    if ((replicas > 100)) && [[ $SCENARIO != scale-101-accepted ]]; then
      echo "The CoderWorkspacePool \"e2e-pool\" is invalid: spec.replicas: Invalid value: $replicas: spec.replicas in body should be less than or equal to 100" >&2
      exit 1
    fi
    edit "$S/pool.json" --argjson r "$replicas" '.spec.replicas = $r | .metadata.generation += 1' ;;
  "delete coderworkspaceclaim") rm -f "$S/claim.json" ;;
  "delete coderworkspacepool") rm -f "$S/pool.json" ;;
  "delete coderworkspacepoolmember") member_delete "$S/m/${pos[2]}.json" ;;
  "delete pod")
    echo coder-k8s-2 >"$S/pod"
    if [[ $SCENARIO == restart-duplicates ]]; then # a second create of the creating member, which Coder then deleted
      members | jq '.[-1].status.workspaceName as $n | {id: "dup", name: $n, deleted: true}' >"$S/ws/dup.json"
    fi ;;
  "exec "*)
    mapfile -t names < <(grep -o 'kwp-[0-9a-f]\{28\}' <<<"${exec_args[-1]}")
    slurp ws | jq '[.[] | select(.name as $n | $ARGS.positional | index($n))] | length' --args "${names[@]}" ;;
  "wait "*) ;;
  *) echo "stub kubectl: unexpected call" >&2 && exit 1 ;;
esac
STUB

cat >"$STUBS/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "curl $*" >>"$STUB_STATE/calls.log"
# shellcheck source=/dev/null
source "$(dirname "$0")/fake.sh"
method=GET url="" data=""
while (($#)); do
  case $1 in
    -X) method=$2 && shift 2 ;;
    --data) data=${2#@} && shift 2 ;;
    -H | --max-time) shift 2 ;;
    http://*) url=$1 && shift ;;
    *) shift ;;
  esac
done
status() { echo "curl: (22) The requested URL returned error: $1" >&2 && exit 22; }
user() { jq -n --arg id "$OWNER_UUID" --arg o "$ORG_UUID" --arg t "${1:-password}" '{id: $id, username: "e2e-pool-owner",
  login_type: $t, organization_ids: [$o]}'; }
path=${url#http://127.0.0.1:*/}
id=${path#api/v2/workspaces/} && id=${id%%[/?]*}
case "$method $path" in
  "GET api/v2/buildinfo") echo '{"version": "v2.37.2"}' ;;
  "GET api/v2/organizations") jq -n --arg o "$ORG_UUID" '[{id: $o, is_default: true}]' ;;
  "POST api/v2/users") [[ $SCENARIO != owner-not-password ]] || status 409; jq -r .password "$data" >"$S/password" && user ;;
  "GET api/v2/users/e2e-pool-owner") if [[ $SCENARIO == owner-not-password ]]; then user oidc; else user; fi ;;
  "PUT api/v2/users/"*/status/activate) echo '{"status": "active"}' ;;
  "POST api/v2/users/login") echo '{"session_token": "stub-session"}' ;;
  "POST api/v2/users/logout" | "PATCH api/v2/templates/"*) echo '{}' ;;
  "GET api/v2/organizations/coder/templates/e2e-pool-agent") jq -n --arg t "$TPL_UUID" '{id: $t}' ;;
  "GET api/v2/templates/"*) echo '{"default_ttl_ms": 3600000}' ;;
  "GET api/v2/workspaces?q=owner:e2e-pool-owner") slurp ws | jq '{workspaces: map(select(.deleted | not))}' ;;
  "GET api/v2/workspaces/"*"?include_deleted=true") [[ -f $S/ws/$id.json ]] || status 404; cat "$S/ws/$id.json" ;;
  "GET api/v2/workspaces/"*) jq -e '.deleted | not' "$S/ws/$id.json" >/dev/null 2>&1 || status 410; cat "$S/ws/$id.json" ;;
  "POST api/v2/workspaces/"*/usage) echo "$id" >"$S/usage" ;;
  *) status 404 ;;
esac
STUB
chmod +x "$STUBS/kubectl" "$STUBS/curl"

# leaked <dir>: a secret in the output outside its mask command, in argv, or in a file the driver left behind.
leaked() {
  local secret
  for secret in stub-token stub-session "$(cat "$1/password" 2>/dev/null)"; do
    [[ -n $secret ]] || continue
    if grep -F -- "$secret" "$1/out.log" | grep -vqxF -- "::add-mask::$secret" || grep -qF -- "$secret" "$1/calls.log" ||
      grep -rqF -- "$secret" "$1/work"; then return 0; fi
  done
  return 1
}

# run_scenario <name> [part of the FAIL message] [nowrite]: without a message the driver must pass P1 to P8 and leave
# nothing behind; with one it must fail with that message, and with nowrite before any write.
run_scenario() {
  local name=$1 want=${2:-} nowrite=${3:-} dir=$BASE/$1 rc=0 problem=""
  mkdir -p "$dir"
  : >"$dir/calls.log"
  (cd "$ROOT" && PATH="$STUBS:$PATH" STUB_STATE=$dir SCENARIO=$name E2E_WORKDIR=$dir/work E2E_POLL_SECONDS=0.1 \
    E2E_TIMEOUT_SECONDS=5 E2E_USED_SECONDS=3 E2E_DRY_RUN_SETTLE_SECONDS=1 GITHUB_ACTIONS=true timeout 120 bash "$DRIVER") \
    </dev/null >"$dir/out.log" 2>&1 || rc=$?
  if [[ -z $want ]]; then
    if ((rc != 0)); then problem="driver failed"
    elif ! grep -qE '^phase_seconds=P1=.* P6=.* P7=.* P8=.* cleanup=' "$dir/work/receipt.txt"; then problem="phases P1 to P8 did not all run"
    elif [[ -e $dir/pool.json || -e $dir/claim.json || -e $dir/allowed ]] || compgen -G "$dir/m/*.json" >/dev/null ||
      cat "$dir"/ws/*.json 2>/dev/null | jq -se 'any(.[]; .deleted | not)' >/dev/null; then problem="left objects or live workspaces behind"
    elif grep -E '^curl .*-X (DELETE|POST) .*/api/v2/workspaces' "$dir/calls.log" | grep -qv '/usage$'; then
      problem="the driver wrote to a workspace itself"
    fi
  elif ((rc == 0)); then problem="driver passed"
  elif ! grep -F 'FAIL: ' "$dir/out.log" | grep -qF -- "$want"; then problem="want a FAIL line with: $want"
  elif [[ -n $nowrite ]] && grep -E '^kubectl .*( create | apply | patch | delete | scale )' "$dir/calls.log" >/dev/null; then
    problem="sent a Kubernetes write"
  elif [[ -n $nowrite ]] && grep -E '^curl .*-X (PUT|PATCH|DELETE) ' "$dir/calls.log" >/dev/null; then problem="sent a Coder write"
  fi
  if [[ -z $problem ]] && leaked "$dir"; then problem="leaked a secret"; fi
  if [[ -z $problem && -f $dir/pf.pid ]] && kill -0 "$(cat "$dir/pf.pid")" 2>/dev/null; then problem="left the port-forward running"; fi
  if [[ -n $problem ]]; then
    echo "FAIL $name: $problem (exit $rc)"
    sed 's/^/    /' "$dir/out.log" | tail -n 40
    FAILURES=$((FAILURES + 1))
  else
    echo "PASS $name"
  fi
}

run_scenario pass
run_scenario no-token-ref "no operator token secret ref" nowrite
run_scenario owner-not-password "owner e2e-pool-owner has an unexpected shape or organization" nowrite
run_scenario members-before-allow "a pool whose owner is not allowed made members"
run_scenario zero-deadline "has a zero deadline"
run_scenario scale-101-accepted "kubectl scale --replicas=101 succeeded"
run_scenario bind-builds "the claim started a build"
run_scenario restart-duplicates "the 5 members' workspace names have 6 workspace rows"
run_scenario use-not-detected "Used=True and released by the pool"
run_scenario false-use "pool usedReplicas is not 1"
run_scenario scale-down-keeps-workspace "deleted in Coder"
run_scenario pool-delete-takes-used "the used member"
run_scenario used-member-kept "did not delete workspace"
run_scenario dry-run-creates "the server dry run created pool e2e-pool"

((FAILURES == 0)) || { echo "$FAILURES scenario(s) failed"; exit 1; }
echo "all workspace pool driver scenarios passed"
