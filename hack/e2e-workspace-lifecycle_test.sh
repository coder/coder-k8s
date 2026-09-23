#!/usr/bin/env bash
# Offline tests for hack/e2e-workspace-lifecycle.sh (issue #113). kubectl, curl, and
# docker are stubs on PATH backed by a fake workspace store; nothing touches a cluster.
# shellcheck disable=SC2016 # check expressions are single-quoted on purpose and eval'd at check time.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DRIVER=$ROOT/hack/e2e-workspace-lifecycle.sh
BUILT=sha256:$(printf 'a%.0s' {1..64})
OTHER=sha256:$(printf 'b%.0s' {1..64})
OLD_DIGEST=sha256:$(printf 'c%.0s' {1..64})
NEW_DIGEST=sha256:$(printf 'd%.0s' {1..64})
BASE=$(mktemp -d)
trap 'rm -rf "$BASE"' EXIT
STUBS=$BASE/bin
mkdir -p "$STUBS"
FAILURES=0

cat >"$STUBS/kubectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
S=$STUB_STATE
echo "kubectl $*" >>"$S/calls.log"
raw="" file="" pos=()
while (($#)); do
  case $1 in
    --raw) raw=$2; shift 2 ;;
    -f) file=$2; shift 2 ;;
    -n | -o | -l) shift 2 ;;
    -v=* | --request-timeout=*) shift ;;
    *) pos+=("$1"); shift ;;
  esac
done
err() { echo "Error from server ($1): $2" >&2; exit 1; }
wsfile() { echo "$S/ws/${1##*/}.json"; }
next() { local n; n=$(($(cat "$S/counter") + 1)); echo "$n" >"$S/counter"; echo "$n"; }
case "${pos[0]}:${pos[1]:-}" in
  get:pods) cat "$S/pods.json" ;;
  get:endpointslices) cat "$S/endpoints.json" ;;
  get:codercontrolplane) echo '{"status":{"operatorTokenSecretRef":{"name":"op-token","key":"token"}}}' ;;
  get:secret) printf '{"data":{"token":"%s"}}\n' "$(printf secret-token-value | base64)" ;;
  port-forward:*) echo $$ >"$S/pf.pid"; exec sleep 300 ;;
  get:)
    if [[ $raw == *watch=1* ]]; then
      echo "$raw" >"$S/watch_url"; echo $$ >"$S/watch.pid"
      off=$(wc -c <"$S/events")
      [[ $SCENARIO == watch-unregistered ]] || echo "I0923 round_trippers.go:553] GET https://127.0.0.1:6443$raw 200 OK in 2 milliseconds" >&2
      [[ $SCENARIO != watch-exits ]] || exit 0
      exec timeout 60 tail -c "+$((off + 1))" -f "$S/events"
    fi
    f=$(wsfile "$raw"); [[ -f $f ]] || err NotFound "coderworkspaces \"${raw##*/}\" not found"; cat "$f" ;;
  create:)
    n=$(next); f=$(wsfile "$(jq -r .metadata.name "$file")"); [[ ! -f $f ]] || err AlreadyExists exists
    st=running; [[ $SCENARIO != build-failed ]] || st=failed
    jq --arg n "$n" --arg st "$st" '.metadata += {uid: ("uid-" + $n), resourceVersion: $n} |
      .status = {latestBuildID: ("build-" + $n), latestBuildStatus: $st}' "$file" | tee "$f" ;;
  replace:)
    f=$(wsfile "$raw"); [[ -f $f ]] || err NotFound missing; cp "$file" "$S/last_update.json"
    [[ $(jq -r .metadata.resourceVersion "$file") == "$(jq -r .metadata.resourceVersion "$f")" ]] || err Conflict "rv mismatch"
    n=$(next)
    jq --arg n "$n" --argjson run "$(jq .spec.running "$file")" '.metadata.resourceVersion = $n | .spec.running = $run |
      .status = {latestBuildID: ("build-" + $n), latestBuildStatus: (if $run then "running" else "stopped" end)}' "$f" >"$f.tmp"
    mv "$f.tmp" "$f"
    ev=$(jq -c '{type: "MODIFIED", object: .}' "$f")
    if [[ $SCENARIO == event-token-mismatch ]]; then ev=$(jq -c '.object.metadata.resourceVersion = "0"' <<<"$ev"); fi
    { echo 'not-json'; jq -c '.object.metadata.uid = "other"' <<<"$ev"; echo "$ev"; } >>"$S/events"
    cat "$f" ;;
  delete:)
    f=$(wsfile "$raw"); [[ -f $f ]] || err NotFound missing
    if [[ $SCENARIO != delete-ignores-preconditions ]]; then
      if [[ $(jq -r .preconditions.uid "$file") != "$(jq -r .metadata.uid "$f")" ]]; then
        [[ $SCENARIO != recreate-build-drift || $(jq -r .metadata.uid "$f") == uid-1 ]] || { jq '.status.latestBuildStatus = "stopping"' "$f" >"$f.tmp"; mv "$f.tmp" "$f"; }
        err Conflict "Precondition failed: UID"
      fi
      rv=$(jq -r '.preconditions.resourceVersion // empty' "$file")
      [[ -z $rv || $rv == "$(jq -r .metadata.resourceVersion "$f")" ]] || err Conflict "Precondition failed: ResourceVersion"
    fi
    echo "${DELETE_JOB_STATUS:-succeeded}" >"$S/deleted-$(jq -r .metadata.uid "$f")"
    rm "$f"; echo '{"kind":"Status","status":"Success"}' ;;
  *) echo "stub kubectl: unexpected call: ${pos[*]}" >&2; exit 97 ;;
esac
STUB

cat >"$STUBS/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
S=$STUB_STATE
echo "curl $*" >>"$S/calls.log"
method=GET data="" url=""
while (($#)); do
  case $1 in
    -X) method=$2; shift 2 ;;
    -H | --max-time) shift 2 ;;
    --data) data=$2; shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
path=/${url#http://*/}
case "$method $path" in
  "GET /api/v2/buildinfo") printf '{"version":"%s"}\n' "${CODER_VERSION_REPORTED:-v2.37.2+eb69e27}" ;;
  "GET /api/v2/organizations/coder/templates/e2e-template") echo '{"active_version_id":"tv-1"}' ;;
  "GET /api/v2/templateversions/tv-1") printf '{"job":{"status":"%s"}}\n' "${TEMPLATE_JOB_STATUS:-succeeded}" ;;
  "GET /api/v2/workspaces/"*"?include_deleted=true")
    uid=${path##*/} && uid=${uid%%\?*} && [[ -f $S/deleted-$uid ]] || exit 22
    printf '{"latest_build":{"transition":"delete","job":{"status":"%s"}}}\n' "$(<"$S/deleted-$uid")" ;;
  "PATCH /api/v2/workspaces/"*)
    [[ $SCENARIO != rename-rejected ]] || { echo "curl: (22) The requested URL returned error: 400" >&2; exit 22; }
    for f in "$S"/ws/*.json; do
      [[ $(jq -r .metadata.uid "$f") == "${path##*/}" ]] || continue
      nn=$(jq -r --arg n "$(jq -r .name <<<"$data")" '.metadata.name | split(".")[0:2] + [$n] | join(".")' "$f")
      jq --arg nn "$nn" '.metadata.name = $nn' "$f" >"$S/ws/$nn.json"; rm "$f"; exit 0
    done
    exit 22 ;;
  *) echo "stub curl: unexpected $method $path" >&2; exit 97 ;;
esac
STUB

cat >"$STUBS/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "docker $*" >>"$STUB_STATE/calls.log"
[[ $1 == exec && $3 == crictl && $4 == inspecti ]] || { echo "stub docker: unexpected $*" >&2; exit 97; }
# Like kind: the pod's import-<date>@sha256 reference is not inspectable; the image tag is.
case ${7:-} in
  ghcr.io/coder/coder-k8s:e2e) jq -n --arg id "$SERVING_ID" --arg d "$REPO_DIGEST" \
    '{status: {id: $id, repoTags: ["ghcr.io/coder/coder-k8s:e2e"], repoDigests: [$d]}}' ;;
  *) echo "level=fatal msg=\"no such image \\\"${7:-}\\\" present\"" >&2; exit 1 ;;
esac
STUB
cat >"$STUBS/jq" <<'STUB'
#!/usr/bin/env bash
# Real jq, except that render-fail-* scenarios break exactly one request-body render.
case "$SCENARIO:$*" in render-fail-delete:*DeleteOptions* | render-fail-create:*CoderWorkspace*) exit 5 ;; esac
exec "$REAL_JQ" "$@"
STUB
chmod +x "$STUBS"/*

# run_scenario <name> [VAR=value...]: runs the driver against the stubs; sets T, S, RC, SECS.
run_scenario() {
  local name=$1 endpoint_ip=${ENDPOINT_IP:-10.244.0.5} start=$SECONDS
  shift
  T=$BASE/$name S=$BASE/$name/state
  mkdir -p "$S/ws"
  : >"$S/calls.log" && : >"$S/events" && echo 0 >"$S/counter"
  jq -n '{items: [
    {metadata: {name: "coder-k8s-old", deletionTimestamp: "2026-09-23T00:00:00Z"}, spec: {containers: [{image: "ghcr.io/coder/coder-k8s:e2e"}]},
      status: {podIP: "10.244.0.4", conditions: [{type: "Ready", status: "True"}], containerStatuses: [{imageID: "docker.io/library/import-2026-09-22@\($old)"}]}},
    {metadata: {name: "coder-k8s-new"}, spec: {containers: [{image: "ghcr.io/coder/coder-k8s:e2e"}]},
      status: {podIP: "10.244.0.5", conditions: [{type: "Ready", status: "True"}], containerStatuses: [{imageID: "docker.io/library/import-2026-09-23@\($new)"}]}}]}' \
    --arg old "$OLD_DIGEST" --arg new "$NEW_DIGEST" >"$S/pods.json"
  jq -n --arg ip "$endpoint_ip" '{items: [{endpoints: [{addresses: [$ip], conditions: {ready: true}}]}]}' >"$S/endpoints.json"
  RC=0
  env PATH="$STUBS:$PATH" REAL_JQ="$(command -v jq)" STUB_STATE="$S" SCENARIO="$name" SERVING_ID="$BUILT" BUILT_IMAGE_ID="$BUILT" \
    REPO_DIGEST="docker.io/library/import-2026-09-23@$NEW_DIGEST" \
    E2E_EXPECT_CODER_VERSION=v2.37.2 E2E_SOURCE_SHA=0123abc GITHUB_RUN_ID=42 GITHUB_RUN_ATTEMPT=1 \
    E2E_WORKDIR="$T/work" E2E_POLL_SECONDS=0.1 E2E_TIMEOUT_SECONDS=3 E2E_EVENT_TIMEOUT_SECONDS=2 GITHUB_ACTIONS=false \
    "$@" timeout 60 bash "$DRIVER" >"$T/out" 2>&1 || RC=$?
  SECS=$((SECONDS - start))
}

mutations() { grep -E '^kubectl .*(create|replace|delete) --raw|^curl .*-X PATCH' "$S/calls.log" |
  awk '/^kubectl/ {for (i = 2; i <= NF; i++) if ($i ~ /^(create|replace|delete)$/) {print "kubectl " $i; next}} /^curl/ {print "curl PATCH"}' | paste -sd, -; }
check() { # <description> <command...>
  local desc=$1
  shift
  if "$@"; then echo "  ok   - $desc"; else echo "  FAIL - $desc"; FAILURES=$((FAILURES + 1)); fi
}
out_has() { grep -qF -- "$1" "$T/out"; }
no_mutations_after() { [[ $(mutations) == "$1" ]] || { echo "    mutations were: '$(mutations)' expected: '$1'" >&2; return 1; }; }
bg_stopped() { local f p; for f in "$S/pf.pid" "$S/watch.pid"; do
  [[ -f $f ]] || continue; p=$(<"$f"); for _ in 1 2 3 4 5 6 7 8 9 10; do
    [[ -r /proc/$p/stat && $(awk '{print $3}' "/proc/$p/stat") != Z ]] || continue 2; sleep 0.2; done; return 1; done; }
failed_with() { [[ $RC -ne 0 ]] && out_has "$1" && bg_stopped; }
summary() { echo "  (native driver exit=$RC, ${SECS}s)"; }

echo "TEST pass: full lifecycle against a well-behaved fake"
run_scenario pass; summary
check "driver exits 0 and prints PASS" eval '[[ $RC -eq 0 ]] && out_has "PASS: workspace lifecycle"'
# shellcheck disable=SC2034 # rv is used by the eval'd check below
rv=$(jq -r .metadata.resourceVersion "$S/last_update.json")
check "watch URL uses the current token and only watch/resourceVersion/timeoutSeconds" \
  eval '[[ $(<"$S/watch_url") =~ ^/apis/aggregation\.coder\.com/v1alpha1/namespaces/coder/coderworkspaces\?watch=1\&resourceVersion=${rv}\&timeoutSeconds=[0-9]+$ ]]'
check "watch URL omits sendInitialEvents and resourceVersionMatch" eval '! grep -qE "sendInitialEvents|resourceVersionMatch" "$S/watch_url"'
check "update was sent only after watch registration" eval '[[ $(grep -n "watch=1" "$S/calls.log" | cut -d: -f1) -lt $(grep -n "replace --raw" "$S/calls.log" | cut -d: -f1) ]]'
check "mutation order: create, update, rename, 409 delete, delete, recreate, 409 delete" \
  no_mutations_after "kubectl create,kubectl replace,curl PATCH,kubectl delete,kubectl delete,kubectl create,kubectl delete"
check "terminating pod ignored; serving pod image tag inspected on node" eval 'grep -q "crictl inspecti -o json ghcr.io/coder/coder-k8s:e2e" "$S/calls.log"'
check "serving pod digest recorded" eval 'grep -qx "pod_image_ref=docker.io/library/import-2026-09-23@$NEW_DIGEST" "$T/work/image-identity.txt"'
check "image identity recorded" eval 'grep -qx "built=$BUILT" "$T/work/image-identity.txt" && grep -qx "serving=$BUILT" "$T/work/image-identity.txt"'
check "operator token never printed" eval '! out_has secret-token-value'
check "background port-forward and watch stopped" bg_stopped
check "every non-streaming kubectl request carries --request-timeout=30s" \
  eval '! grep "^kubectl" "$S/calls.log" | grep -v -e "watch=1" -e port-forward | grep -qv -- "--request-timeout=30s "'
check "recreate only after the delete job succeeded" eval '[[ $(grep -n include_deleted "$S/calls.log" | tail -1 | cut -d: -f1) -lt $(grep -n "^kubectl --request-timeout=30s create" "$S/calls.log" | tail -1 | cut -d: -f1) ]]'
check "receipt: source, run, version, identity, UIDs, 9 passed cases" eval 'grep -q "=== RECEIPT (PASS) ===" "$T/out" && grep -qx "source_sha=0123abc" "$T/work/receipt.txt" &&
  grep -qx "run_id=42" "$T/work/receipt.txt" && grep -qx "coder_version=v2.37.2+eb69e27" "$T/work/receipt.txt" && grep -qx "uid1=uid-3" "$T/work/receipt.txt" && [[ $(grep -c "= passed$" "$T/work/receipt.txt") -eq 9 ]]'

echo "TEST image-mismatch: serving image differs from built image"
run_scenario image-mismatch SERVING_ID="$OTHER"; summary
check "fails with identity mismatch" failed_with "image identity mismatch"
check "no mutation and no port-forward" eval 'no_mutations_after "" && ! grep -q port-forward "$S/calls.log"'

echo "TEST digest-mismatch: the node image tag does not carry the serving pod's digest"
run_scenario digest-mismatch REPO_DIGEST="docker.io/library/import-2026-09-22@$OLD_DIGEST"; summary
check "fails with digest mismatch" failed_with "is not a digest of"
check "no mutation and no port-forward" eval 'no_mutations_after "" && ! grep -q port-forward "$S/calls.log"'

echo "TEST version-mismatch: Coder serves v2.37.20, a prefix of the pin without '+'"
run_scenario version-mismatch CODER_VERSION_REPORTED=v2.37.20; summary
check "fails on version before any mutation; receipt marks the failing case" \
  eval 'failed_with "Coder version mismatch" && no_mutations_after "" && grep -q "Coder API access.* = FAILED" "$T/work/receipt.txt"'

echo "TEST render-fail-create: create body render fails"
run_scenario render-fail-create; summary
check "fails at create with zero kubectl create calls" eval '[[ $RC -ne 0 ]] && bg_stopped && no_mutations_after "" &&
  grep -q "create workspace through the aggregated API = FAILED" "$T/work/receipt.txt"'

echo "TEST endpoint-mismatch: aggregated API service points at the terminating pod"
ENDPOINT_IP=10.244.0.4 run_scenario endpoint-mismatch; summary
check "fails before any mutation" eval 'failed_with "endpoints do not match serving pod" && no_mutations_after ""'

echo "TEST template-import-failed: setup failure"
run_scenario template-import-failed TEMPLATE_JOB_STATUS=failed; summary
check "fails on import status and never creates a workspace" eval 'failed_with "import ended in status failed" && no_mutations_after ""'

echo "TEST build-failed: create build fails"
run_scenario build-failed; summary
check "fails on build status; no update" eval 'failed_with "ended in status failed" && no_mutations_after "kubectl create"'

echo "TEST watch-unregistered: watch never reports 200 OK"
run_scenario watch-unregistered; summary
check "bounded failure before the update" eval 'failed_with "timed out after 3s waiting for: watch registration" && no_mutations_after "kubectl create" && ((SECS < 30))'

echo "TEST event-token-mismatch: MODIFIED event token differs from update response"
run_scenario event-token-mismatch; summary
check "bounded event match fails; no rename or delete" \
  eval 'failed_with "timed out after 2s waiting for: MODIFIED event" && no_mutations_after "kubectl create,kubectl replace" && ((SECS < 30))'

echo "TEST rename-rejected: Coder rejects the out-of-band rename"
run_scenario rename-rejected; summary
check "fails at rename; no delete" eval '[[ $RC -ne 0 ]] && bg_stopped && no_mutations_after "kubectl create,kubectl replace,curl PATCH"'

echo "TEST delete-ignores-preconditions: wrong-UID delete succeeds"
run_scenario delete-ignores-preconditions; summary
check "fails on missing 409; no live delete or recreate" \
  eval 'failed_with "expected (Conflict) but request succeeded" && no_mutations_after "kubectl create,kubectl replace,curl PATCH,kubectl delete"'

echo "TEST render-fail-delete: DeleteOptions render fails inside expect_error (errexit off)"
run_scenario render-fail-delete; summary
check "fails with zero kubectl delete calls and no recreate" eval 'failed_with "expected (Conflict)" && no_mutations_after "kubectl create,kubectl replace,curl PATCH"'

echo "TEST delete-job-failed: workspace is 404 but its delete job failed"
run_scenario delete-job-failed DELETE_JOB_STATUS=failed; summary
check "fails on delete job status; no recreate" eval 'failed_with "delete job of uid-1 ended in status failed" && no_mutations_after "kubectl create,kubectl replace,curl PATCH,kubectl delete,kubectl delete"'

echo "TEST watch-exits: watch registers then exits before the update"
run_scenario watch-exits; summary
check "fails before the update" eval 'failed_with "watch process exited before the update" && no_mutations_after "kubectl create"'

echo "TEST recreate-build-drift: prior-UID delete returns 409 but the recreated build changes"
run_scenario recreate-build-drift; summary
check "fails on recreated latest build status" eval 'failed_with "recreated object changed after prior-UID delete"'

echo "TEST missing-built-id: BUILT_IMAGE_ID is not a sha256 ID"
run_scenario missing-built-id BUILT_IMAGE_ID=e2e; summary
check "fails before any tool call" eval '[[ $RC -ne 0 ]] && out_has "not a sha256 image ID" && [[ ! -s $S/calls.log ]]'

((FAILURES == 0)) || { echo "FAILED: $FAILURES check(s)"; exit 1; }
echo "ALL OFFLINE TESTS PASSED"
