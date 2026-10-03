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
raw="" file="" patch="" all=$* pos=()
while (($#)); do
  case $1 in
    --raw) raw=$2; shift 2 ;;
    -f) file=$2; shift 2 ;;
    -p) patch=$2; shift 2 ;;
    -n | -o | -l | -c) shift 2 ;;
    -v=* | --request-timeout=* | --type=* | --wait=*) shift ;;
    *) pos+=("$1"); shift ;;
  esac
done
err() { echo "Error from server ($1): $2" >&2; exit 1; }
wsfile() { echo "$S/ws/${1##*/}.json"; }
next() { local n; n=$(($(cat "$S/counter") + 1)); echo "$n" >"$S/counter"; echo "$n"; }
ws_create() {
  local n f st=running
  n=$(next); f=$(wsfile "$(jq -r .metadata.name "$file")"); [[ ! -f $f ]] || err AlreadyExists exists
  [[ $SCENARIO != build-failed ]] || st=failed
  jq --arg n "$n" --arg st "$st" '.metadata += {uid: ("uid-" + $n), resourceVersion: $n} |
    .status = {latestBuildID: ("build-" + $n), latestBuildStatus: $st}' "$file" >"$f"
  echo "build-$n" >>"$S/builds-uid-$n"
}
side_effect() { # stale-side-effect-* scenarios: a rejected stale DELETE still changes one backend observable
  case $SCENARIO in
    stale-side-effect-count) echo build-extra >>"$S/builds-$(jq -r .metadata.uid "$1")" ;;
    stale-side-effect-latest) echo build-extra >"$S/latest-$(jq -r .metadata.uid "$1")" ;;
    stale-side-effect-object) jq '.status.lastUsedAt = "2026-09-23T00:00:00Z"' "$1" >"$1.tmp" && mv "$1.tmp" "$1" ;;
  esac
}
tt_json() { # <phase> <reason> [conditions]: a CoderTemplateTest object for the TemplateTest phases
  jq -n --arg n "${pos[2]}" --arg p "$1" --arg r "$2" --argjson c "${3:-[]}" \
    '{metadata: {name: $n}, status: {phase: $p, reason: $r, message: "stub", workspaceName: ("ktt-" + $n), workspaceID: ("ttws-" + $n), conditions: $c}}'
}
case "${pos[0]}:${pos[1]:-}" in
  get:pods) cat "$S/pods.json" ;;
  patch:codercontrolplane) # templateTests.ownerUserID, then spec.replicas=0 in the namespace phase
    if [[ $patch == *ownerUserID* ]]; then [[ $SCENARIO != owner-patch-fails ]] || err Invalid "bad ownerUserID"; touch "$S/owner"
    else touch "$S/replicas0"; fi; echo "codercontrolplane.coder.com/coder patched" ;;
  get:deploy) # coder-rolls: the owner patch changes the pod template; coder-stays-up: replicas=0 never removes the pod
    r=1 g=1 && [[ ! -f $S/replicas0 ]] || r=0 && [[ $SCENARIO != coder-rolls || ! -f $S/owner ]] || g=2
    jq -n --argjson r "$r" --argjson g "$g" --arg sc "$SCENARIO" '{metadata: {generation: $g}, spec: {replicas: $r},
      status: {replicas: (if $sc == "coder-stays-up" then 1 else $r end)}}' ;;
  get:cluster) echo '{"status":{"currentPrimary":"coder-db-1"}}' ;;
  exec:coder-db-1) # psql counts: api_keys 2 then 5; workspaces 1 (ws-rows-2: 2)
    case $all in
      *"FROM api_keys"*"user-operator"*) [[ $SCENARIO == operator-no-keys ]] && echo 0 || echo 1 ;;
      *"FROM api_keys"*)
        n=$(($(cat "$S/key-reads" 2>/dev/null || echo 0) + 1)) && echo "$n" >"$S/key-reads"
        [[ $SCENARIO != keys-count-fails || $n != 1 ]] && [[ $SCENARIO != keys-after-fails || $n != 2 ]] || err InternalError "psql failed"
        [[ $n == 1 ]] && echo 2 || echo 5 ;;
      *"FROM workspaces"*"ktt-e2e-bad-param"*) [[ $SCENARIO == badparam-created ]] && echo 1 || echo 0 ;;
      *"FROM workspaces"*"ktt-d0d0"*) echo 0 ;;
      *"FROM workspaces"*) [[ $SCENARIO == ws-rows-2 ]] && echo 2 || echo 1 ;;
      *) echo "stub kubectl: unexpected SQL: $all" >&2; exit 97 ;;
    esac ;;
  get:codertemplatetest) # e2e-pass-*: Pending, Running, then final; e2e-ns-delete: Running until Coder and the namespace go
    f=$S/tt-${pos[2]} && [[ -f $f ]] || err NotFound "codertemplatetests \"${pos[2]}\" not found"
    n=$(($(cat "$f") + 1)) && echo "$n" >"$f"
    if [[ ${pos[2]} == e2e-ns-delete ]]; then
      if [[ -f $S/ns-deleted ]]; then rm "$f"; tt_json Failed ControlPlaneGone '[{"type":"WorkspaceDeleted","status":"Unknown","reason":"ControlPlaneGone"}]'
      elif [[ -f $S/replicas0 && $n -gt 2 ]]; then tt_json Running CoderUnavailable
      elif ((n > 1)); then tt_json Running WaitingForAgents
      else tt_json Pending Creating; fi
    elif ((n == 1)); then tt_json Pending Creating
    elif [[ ${pos[2]} == e2e-bad-param ]]; then tt_json Failed CreateRejected '[{"type":"WorkspaceDeleted","status":"True","reason":"NotCreated"}]'
    elif ((n == 2)) || [[ ${pos[2]} == e2e-midrun ]]; then tt_json Running WaitingForAgents
    elif [[ ${pos[2]} == e2e-ttl ]]; then rm "$f"; err NotFound "codertemplatetests \"e2e-ttl\" not found" # expired
    elif [[ $SCENARIO:${pos[2]} == tt-failed:e2e-pass-1 || $SCENARIO:${pos[2]} == restart-failed:e2e-restart ||
      ($SCENARIO != fail-ignored && ${pos[2]} == e2e-agent-fail) ]]; then
      tt_json Failed AgentStartError '[{"type":"WorkspaceDeleted","status":"True","reason":"Deleted"}]'
    else tt_json Succeeded Succeeded '[{"type":"Ready","status":"True","reason":"Succeeded"},{"type":"WorkspaceDeleted","status":"True","reason":"Deleted"}]'
    fi ;;
  get:namespace) # content gone, but the namespace stays Terminating (the aggregated API LIST answers 503)
    jq -n '{status: {phase: "Terminating", conditions: [{type: "NamespaceDeletionContentFailure", status: "True", message: "no eligible CoderControlPlane"},
      {type: "NamespaceContentRemaining", status: "False"}, {type: "NamespaceFinalizersRemaining", status: "False"}]}}' ;;
  delete:namespace) touch "$S/ns-deleted"; echo 'namespace "coder" deleted' ;;
  delete:pod) echo "pod \"${pos[2]}\" deleted" ;;
  delete:codertemplatetest) # the controller deletes the workspace, then releases the test
    rm "$S/tt-${pos[2]}"; st=succeeded && [[ $SCENARIO != midrun-delete-failed ]] || st=failed
    echo "$st" >"$S/deleted-ttws-${pos[2]}"; echo deleted ;;
  patch:codertemplatetest) [[ $SCENARIO == immutable-accepted ]] && echo patched ||
    err Invalid 'CoderTemplateTest.coder.com "e2e-agent-fail" is invalid: spec: Invalid value: "object": spec is immutable, create a new CoderTemplateTest' ;;
  create:--dry-run=server) # dryrun-persists: the dry run stores the object anyway
    [[ $SCENARIO != dryrun-persists ]] || echo 0 >"$S/tt-$(jq -r .metadata.name "$file")"
    jq '.metadata.uid = "d0d0d0d0-d0d0-d0d0-d0d0-d0d0d0d0d0d0"' "$file" ;;
  get:endpointslices) cat "$S/endpoints.json" ;;
  get:codercontrolplane) [[ ! -f $S/ns-deleted || $SCENARIO == ns-delete-stuck ]] || err NotFound 'codercontrolplanes "coder" not found'
    echo '{"status":{"operatorTokenSecretRef":{"name":"op-token","key":"token"}}}' ;;
  get:secret) printf '{"data":{"token":"%s"}}\n' "$(printf secret-token-value | base64)" ;;
  port-forward:*) echo $$ >"$S/pf.pid"; exec sleep 300 ;;
  logs:*) echo "I1002 server started"; [[ $SCENARIO != log-leak ]] || echo "2026-10-02T10:00:00Z [info] [provisioner|Planning infrastructure] Terraform 1.14.0 ok" ;;
  get:)
    if [[ $raw == */codertemplatetests?watch=1* ]]; then # the e2e-ttl watch: passed (ttl-failed: failed), then deleted
      echo "I1003 round_trippers.go:553] GET https://127.0.0.1:6443$raw 200 OK in 2 milliseconds" >&2
      p=Succeeded && [[ $SCENARIO != ttl-failed ]] || p=Failed
      jq -nc --arg p "$p" '{type: "MODIFIED", object: {status: {phase: $p, conditions: [{type: "WorkspaceDeleted", status: "True"}]}}}, {type: "DELETED"}'
      exit 0
    fi
    if [[ $raw == *watch=1* ]]; then
      echo "$raw" >"$S/watch_url"; echo $$ >"$S/watch.pid"
      off=$(wc -c <"$S/events")
      [[ $SCENARIO == watch-unregistered ]] || echo "I0923 round_trippers.go:553] GET https://127.0.0.1:6443$raw 200 OK in 2 milliseconds" >&2
      [[ $SCENARIO != watch-exits ]] || exit 0
      exec timeout 60 tail -c "+$((off + 1))" -f "$S/events"
    fi
    if [[ $raw == */log* ]]; then # coderworkspaces/log snapshot; log-* scenarios break it
      [[ -f $(wsfile "${raw%/log*}") ]] || err NotFound missing
      [[ $SCENARIO != log-empty ]] || exit 0
      [[ $SCENARIO != follow-fails || $raw != *follow=true* ]] || err InternalError "stream failed"
      line='2026-10-02T10:00:00Z [info] [provisioner|Planning infrastructure] Terraform 1.14.0 ok'
      if [[ $raw == *limitBytes=64 ]]; then printf '%s\n%s\n' "$line" "$line" | head -c 64; else printf '%s\n%s\n' "$line" "$line"; fi
      exit 0
    fi
    if [[ $raw == */coderworkspaces ]]; then # LIST; list-* scenarios drop the item or skew its token
      exec jq -s --arg sc "$SCENARIO" '{items: map(select($sc != "list-missing") |
        if $sc == "list-token-differs" then .metadata.resourceVersion = "skewed" else . end)}' "$S"/ws/*.json
    fi
    f=$(wsfile "$raw"); [[ -f $f ]] || err NotFound "coderworkspaces \"${raw##*/}\" not found"; cat "$f" ;;
  create:--dry-run=client) # client-side render of the CoderTemplate manifest
    jq -n '{apiVersion: "aggregation.coder.com/v1alpha1", kind: "CoderTemplate", metadata: {name: "coder.e2e-template", namespace: "coder"},
      spec: {organization: "coder", files: {"main.tf": "# e2e"}}}' ;;
  get:codertemplateversions) # $S/template holds "id active-version version-count updated-stamp"
    read -r _ av n _ <"$S/template"
    jq -n --arg av "$av" --argjson n "$n" '{items: [range(1; $n + 1) | ("tv-\(.)") as $id | {status: {id: $id, active: ($id == $av)}}]}' ;;
  create:)
    if [[ ${file##*/} == tt-*.json ]]; then echo 0 >"$S/tt-$(jq -r .metadata.name "$file")"; echo created; exit 0; fi
    if [[ $raw == */codertemplates/*/promote* ]]; then # promote-* scenarios break the activation
      read -r id av n st <"$S/template"; v=$(jq -r .spec.versionID "$file")
      if [[ $av == "$v" ]]; then r=AlreadyActive; [[ $SCENARIO != promote-repeat-writes ]] || st=$((st + 1))
      elif [[ $raw == *dryRun=All ]]; then r=WouldPromote
      else r=Promoted; [[ $SCENARIO == promote-not-applied ]] || { av=$v; st=$((st + 1)); }; fi
      echo "$id $av $n $st" >"$S/template"; jq -n --arg r "$r" '{status: {result: $r}}'; exit 0
    fi
    if [[ $raw == */start* || $raw == */stop* ]]; then # #148 start/stop; builds use their own counter
      sub=${raw##*/} && sub=${sub%%\?*} && f=$(wsfile "${raw%/*}"); [[ -f $f ]] || err NotFound missing
      want=running && [[ $sub == start ]] || want=stopped
      dry=false && [[ $raw != *dryRun=All* ]] || dry=true
      b=$(jq -r .status.latestBuildID "$f")
      if [[ $(jq -r .status.latestBuildStatus "$f") == "$want" && $SCENARIO != repeat-start-queues ]]; then o=Unchanged
      elif [[ $dry == true && $SCENARIO != dryrun-queues ]]; then o=WouldQueue b=""
      else
        n=$(($(cat "$S/tcounter" 2>/dev/null || echo 0) + 1)) && echo "$n" >"$S/tcounter" && b=tbuild-$n o=Queued
        echo "$b" >>"$S/builds-$(jq -r .metadata.uid "$f")"
        jq --arg b "$b" --arg st "$want" '.status = {latestBuildID: $b, latestBuildStatus: $st}' "$f" >"$f.tmp" && mv "$f.tmp" "$f"
      fi
      exec jq -n --arg t "$sub" --arg o "$o" --argjson d "$dry" --arg b "$b" '{status: {transition: $t, outcome: $o, dryRun: $d, buildID: $b}}'
    fi
    ws_create && cat "$(wsfile "$(jq -r .metadata.name "$file")")" ;;
  apply:)
    if [[ $file == *codertemplate-agent.yaml ]]; then echo "codertemplate/coder.e2e-agent created"
    elif [[ $file == *.yaml ]]; then # the CoderTemplate manifest; $S/template holds "id active-version version-count"
      [[ $SCENARIO != template-apply-error ]] || err InternalError "an error on the server has prevented the request from succeeding"
      [[ -f $S/template ]] || { echo "tpl-1 tv-1 1 0" >"$S/template"; echo "codertemplate/coder.e2e-template created"; exit 0; }
      read -r id av n st <"$S/template"
      [[ $SCENARIO != reapply-new-version ]] || echo "$id $av $((n + 1)) $st" >"$S/template"
      echo "codertemplate/coder.e2e-template configured"
    elif [[ $(jq -r .kind "$file") == CoderTemplate ]]; then # changed files: a new active version
      read -r id _ n st <"$S/template"; echo "$id tv-$((n + 1)) $((n + 1)) $((st + 1))" >"$S/template"
      echo "codertemplate/coder.e2e-template configured"
    else
      f=$(wsfile "$(jq -r .metadata.name "$file")")
      [[ -f $f ]] || { ws_create; echo "coderworkspace created"; exit 0; }
      [[ $SCENARIO != reapply-error ]] || err Conflict "Operation cannot be fulfilled on coderworkspaces: precondition failed"
      [[ $SCENARIO != reapply-new-build ]] || echo build-reapply >>"$S/builds-$(jq -r .metadata.uid "$f")"
      echo "coderworkspace configured"
    fi ;;
  replace:)
    f=$(wsfile "$raw"); [[ -f $f ]] || err NotFound missing; cp -n "$file" "$S/first_update.json"
    [[ $SCENARIO == stale-update-accepted || $(jq -r .metadata.resourceVersion "$file") == "$(jq -r .metadata.resourceVersion "$f")" ]] ||
      err Conflict "rv mismatch"
    n=$(next) && echo "build-$n" >>"$S/builds-$(jq -r .metadata.uid "$f")"
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
      if [[ $SCENARIO != wrong-uid-ignored && $(jq -r .preconditions.uid "$file") != "$(jq -r .metadata.uid "$f")" ]]; then
        [[ $SCENARIO != recreate-build-drift || $(jq -r .metadata.uid "$f") == uid-1 ]] || { jq '.status.latestBuildStatus = "stopping"' "$f" >"$f.tmp"; mv "$f.tmp" "$f"; }
        err Conflict "Precondition failed: UID"
      fi
      rv=$(jq -r '.preconditions.resourceVersion // empty' "$file")
      [[ -z $rv || $rv == "$(jq -r .metadata.resourceVersion "$f")" ]] || { side_effect "$f"; err Conflict "Precondition failed: ResourceVersion"; }
    fi
    st=${DELETE_JOB_STATUS:-succeeded} && [[ $SCENARIO != agent-delete-failed || $raw != *.e2e-tester.* ]] || st=failed
    echo "$st" >"$S/deleted-$(jq -r .metadata.uid "$f")"
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
    -H) [[ $2 != @* ]] || stat -c %a "${2#@}" >>"$S/file-modes"; shift 2 ;;
    --max-time) shift 2 ;;
    --data) data=$2; [[ $data != @* ]] || { stat -c %a "${data#@}" >>"$S/file-modes"; data=$(<"${data#@}"); }; shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
path=/${url#http://*/}
case "$method $path" in
  "GET /api/v2/buildinfo") printf '{"version":"%s"}\n' "${CODER_VERSION_REPORTED:-v2.37.2+eb69e27}" ;;
  "GET /api/v2/organizations/coder/templates/e2e-template")
    [[ -f $S/template ]] || exit 22; read -r id av _ st <"$S/template"
    printf '{"id":"%s","active_version_id":"%s","updated_at":"2026-10-02T00:00:%02dZ"}\n' "$id" "$av" "$st" ;;
  "GET /api/v2/templates/tpl-1/versions?limit=100") read -r _ _ n _ <"$S/template"; jq -n --argjson n "$n" '[range($n) | {}]' ;;
  "GET /api/v2/templateversions/tv-1") printf '{"job":{"status":"%s"}}\n' "${TEMPLATE_JOB_STATUS:-succeeded}" ;;
  "GET /api/v2/workspaces/"*"?include_deleted=true")
    uid=${path##*/} && uid=${uid%%\?*} && [[ -f $S/deleted-$uid ]] || exit 22
    printf '{"latest_build":{"transition":"delete","job":{"status":"%s"}}}\n' "$(<"$S/deleted-$uid")" ;;
  "GET /api/v2/workspaces/"*"/builds?limit=100") uid=${path#/api/v2/workspaces/} && jq -R '{id: .}' "$S/builds-${uid%%/*}" | jq -s . ;;
  "GET /api/v2/workspaces/"*)
    uid=${path##*/}; lb=$(cat "$S/latest-$uid" 2>/dev/null || jq -r --arg u "$uid" 'select(.metadata.uid == $u) | .status.latestBuildID' "$S"/ws/*.json)
    a=connected/ready && case $SCENARIO in agent-start-error) a=connected/start_error ;; agent-never-ready) a=connecting/created ;; esac
    jq -n --arg u "$uid" --arg lb "$lb" --arg a "$a" '{id: $u, latest_build: {id: $lb,
      resources: [{agents: [{name: "main", status: ($a | split("/")[0]), lifecycle_state: ($a | split("/")[1])}]}]}}' ;;
  "GET /api/v2/organizations") echo '[{"id":"org-2","is_default":false},{"id":"org-1","is_default":true}]' ;;
  "POST /api/v2/users")
    printf '%s' "$data" >"$S/tester-body.json"
    [[ $SCENARIO != tester-create-fails ]] || { echo "curl: (22) The requested URL returned error: 500" >&2; exit 22; }
    [[ $SCENARIO != tester-exists* ]] || { echo "curl: (22) The requested URL returned error: 409" >&2; exit 22; }
    jq '{id: "user-tester", username, login_type, organization_ids, status: "dormant"}' <<<"$data" ;;
  "GET /api/v2/users/me") echo '{"id":"user-operator"}' ;;
  "GET /api/v2/users/e2e-tester") # tester-exists-oidc: someone else's login under the tester's name
    lt=password && [[ $SCENARIO != tester-exists-oidc ]] || lt=oidc
    printf '{"id":"user-tester","username":"e2e-tester","login_type":"%s","organization_ids":["org-1"],"status":"active"}\n' "$lt" ;;
  "PUT /api/v2/users/user-tester/status/activate")
    st=active && [[ $SCENARIO != tester-stays-dormant ]] || st=dormant; printf '{"id":"user-tester","status":"%s"}\n' "$st" ;;
  "PATCH /api/v2/workspaces/"*)
    [[ $SCENARIO != rename-rejected ]] || { echo "curl: (22) The requested URL returned error: 400" >&2; exit 22; }
    for f in "$S"/ws/*.json; do
      [[ $(jq -r .metadata.uid "$f") == "${path##*/}" ]] || continue
      nn=$(jq -r --arg n "$(jq -r .name <<<"$data")" '.metadata.name | split(".")[0:2] + [$n] | join(".")' "$f")
      jq --arg nn "$nn" --arg sc "$SCENARIO" '.metadata.name = $nn | # the #109 fingerprint changes with the name
        if $sc == "old-timestamp-rename" then . else .metadata.resourceVersion += "-renamed" end' "$f" >"$S/ws/$nn.json"; rm "$f"; exit 0
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

mutations() { grep -E '^kubectl .*((create|replace|delete) --raw|apply -f|create -f|patch |delete (namespace|pod|codertemplatetest) )|^curl .*-X (PATCH|POST|PUT)' "$S/calls.log" |
  awk '/^kubectl/ {for (i = 2; i <= NF; i++) if ($i ~ /^(create|replace|delete|apply|patch)$/) {print "kubectl " $i; next}}
    /^curl/ {for (i = 2; i <= NF; i++) if ($i == "-X") {print "curl " $(i + 1); next}}' | paste -sd, -; }
VERSIONS="kubectl apply,kubectl apply,kubectl create,kubectl create,kubectl create" # template, second version, promote v1, repeat, dry-run v2
APPLIES="$VERSIONS,kubectl apply,kubectl apply,kubectl apply"                       # then workspace, identical template and workspace re-apply
TRANS="kubectl create,kubectl create,kubectl create,kubectl create" # #148 start, repeated start, dry-run stop, stop
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
rv=$(jq -r .metadata.resourceVersion "$S/first_update.json")
check "watch URL uses the current token and only watch/resourceVersion/timeoutSeconds" \
  eval '[[ $(<"$S/watch_url") =~ ^/apis/aggregation\.coder\.com/v1alpha1/namespaces/coder/coderworkspaces\?watch=1\&resourceVersion=${rv}\&timeoutSeconds=[0-9]+$ ]]'
check "watch URL omits sendInitialEvents and resourceVersionMatch" eval '! grep -qE "sendInitialEvents|resourceVersionMatch" "$S/watch_url"'
check "update was sent only after watch registration" eval '[[ $(grep -n "watch=1" "$S/calls.log" | head -1 | cut -d: -f1) -lt $(grep -n "replace --raw" "$S/calls.log" | head -1 | cut -d: -f1) ]]'
LIFECYCLE="$APPLIES,kubectl replace,$TRANS,curl PATCH,kubectl replace,kubectl delete,kubectl delete,kubectl delete,kubectl create,kubectl delete"
AGENT="$LIFECYCLE,curl POST,curl PUT,kubectl apply,kubectl create,kubectl delete" # then owner patch and three pass tests:
TESTS="$AGENT,kubectl patch,kubectl create,kubectl create,kubectl create"
# agent failure, bad parameter, restart (+ pod delete), mid-run (+ test delete), refused spec patch, TTL
PHASED="$TESTS,kubectl create,kubectl create,kubectl create,kubectl delete,kubectl create,kubectl delete,kubectl patch,kubectl create"
check "mutation order: lifecycle, tester, agent, owner, three pass tests, namespace test, Coder to zero, namespace delete" \
  no_mutations_after "$PHASED,kubectl create,kubectl patch,kubectl delete"
check "stale requests carry the genuine pre-rename token; rename changed it" eval 'grep -qx "rv_pre_rename=2" "$T/work/receipt.txt" &&
  grep -qx "rv_post_rename=2-renamed" "$T/work/receipt.txt" && jq -e ".metadata.resourceVersion == \"2\"" "$T/work/stale-update.json" >/dev/null'
check "template applied with the long request timeout, then re-applied before any lifecycle mutation" eval '[[ $(grep -c -- "--request-timeout=600s apply -f config/e2e/codertemplate.yaml" "$S/calls.log") -eq 2 ]]'
check "receipt: template and workspace state, no apply failure" eval 'grep -qx "template=id=tpl-1 active=tv-1 versions=2" "$T/work/receipt.txt" &&
  grep -qx "workspace=uid=uid-1 latest=build-1/build-1 builds=1 status=running" "$T/work/receipt.txt" && grep -qx "apply_failure=none" "$T/work/receipt.txt"'
check "LIST requested after the stale checks" eval 'grep -q "get --raw /apis/aggregation.coder.com/v1alpha1/namespaces/coder/coderworkspaces$" "$S/calls.log"'
check "terminating pod ignored; serving pod image tag inspected on node" eval 'grep -q "crictl inspecti -o json ghcr.io/coder/coder-k8s:e2e" "$S/calls.log"'
check "serving pod digest recorded" eval 'grep -qx "pod_image_ref=docker.io/library/import-2026-09-23@$NEW_DIGEST" "$T/work/image-identity.txt"'
check "image identity recorded" eval 'grep -qx "built=$BUILT" "$T/work/image-identity.txt" && grep -qx "serving=$BUILT" "$T/work/image-identity.txt"'
check "operator token never printed" eval '! out_has secret-token-value'
check "background port-forward and watch stopped" bg_stopped
check "every non-streaming kubectl request carries --request-timeout=30s" \
  eval '! grep "^kubectl" "$S/calls.log" | grep -v -e "watch=1" -e port-forward | grep -qvE -- "--request-timeout=[0-9]+s "'
check "recreate only after the delete job succeeded" eval '[[ $(grep -n include_deleted "$S/calls.log" | head -1 | cut -d: -f1) -lt $(grep -n "create --raw .*ws-e2e-lifecycle-renamed.json" "$S/calls.log" | cut -d: -f1) ]]'
check "receipt: source, run, version, identity, UIDs, 28 passed cases" eval 'grep -q "=== RECEIPT (PASS) ===" "$T/out" && grep -qx "source_sha=0123abc" "$T/work/receipt.txt" &&
  grep -qx "run_id=42" "$T/work/receipt.txt" && grep -qx "coder_version=v2.37.2+eb69e27" "$T/work/receipt.txt" && grep -qx "uid1=uid-3" "$T/work/receipt.txt" && [[ $(grep -c "= passed$" "$T/work/receipt.txt") -eq 28 ]]'
check "remaining phases: durations recorded; dry run, restart, mid-run, and TTL calls as planned" eval 'grep -Eqx "phase_seconds=agent-failure=[0-9]+s bad-parameter=[0-9]+s restart=[0-9]+s delete-mid-run=[0-9]+s immutability-and-dry-run=[0-9]+s ttl=[0-9]+s" "$T/work/receipt.txt" &&
  grep -q "create --dry-run=server -f .*tt-e2e-dry-run.json" "$S/calls.log" && grep -q "FROM workspaces WHERE name = .ktt-d0d0d0d0d0d0d0d0d0d0d0d0d0d0." "$S/calls.log" &&
  grep -q "coder-system delete pod coder-k8s-new" "$S/calls.log" && grep -q "workspaces/ttws-e2e-midrun?include_deleted=true" "$S/calls.log" &&
  jq -e ".spec.ttlSecondsAfterFinished == 0" "$T/work/tt-e2e-ttl.json" >/dev/null && jq -e ".spec.parameters == [{name: \"fail\", value: \"notabool\"}]" "$T/work/tt-e2e-bad-param.json" >/dev/null'
check "TemplateTest receipt: three durations, tester key counts, namespace deletion time" eval 'grep -Eqx "template_test_seconds=[0-9]+s [0-9]+s [0-9]+s" "$T/work/receipt.txt" &&
  grep -qx "tester_api_keys_before=2" "$T/work/receipt.txt" && grep -qx "tester_api_keys_after=5" "$T/work/receipt.txt" &&
  grep -Eqx "namespace_delete_seconds=[0-9]+" "$T/work/receipt.txt" && out_has "tester api_keys: before=2 after=5 delta=3"'
check "psql only counts rows: operator and tester api_keys, the first pass test's workspace name" eval '[[ $(grep -c "^kubectl .* exec " "$S/calls.log") -eq 9 ]] &&
  ! grep "^kubectl .* exec " "$S/calls.log" | grep -qv "SELECT count(\*) FROM" && grep -q "FROM api_keys WHERE user_id = .user-tester." "$S/calls.log" &&
  grep -q "FROM workspaces WHERE name = .ktt-e2e-pass-1." "$S/calls.log"'
check "owner patch names the tester; the namespace test waits 120 s for its agent" eval 'grep -qF "\"ownerUserID\":\"user-tester\"" "$S/calls.log" &&
  jq -e ".spec.parameters == [{name: \"startup_delay\", value: \"120\"}] and .spec.template == \"coder.e2e-agent\"" "$T/work/tt-e2e-ns-delete.json" >/dev/null &&
  jq -e ".spec.version.active and (.spec | has(\"parameters\") | not)" "$T/work/tt-e2e-pass-1.json" >/dev/null'
check "namespace deleted only after CoderUnavailable; the release went through ControlPlaneGone" eval '[[ $(grep -n "e2e-ns-delete: Running CoderUnavailable" "$T/out" | cut -d: -f1) -lt $(grep -n "content deleted in" "$T/out" | cut -d: -f1) ]] &&
  out_has "e2e-ns-delete: Failed ControlPlaneGone deleted=Unknown/ControlPlaneGone" && grep -q "replicas.:0" "$S/calls.log" &&
  out_has "namespace coder is Terminating, known issue #209: no eligible CoderControlPlane"'
check "tester: password user in the default organization; receipt records its username and id" eval 'jq -e ".login_type == \"password\" and .organization_ids == [\"org-1\"]" "$S/tester-body.json" >/dev/null &&
  grep -qx "tester=e2e-tester/user-tester" "$T/work/receipt.txt"'
# shellcheck disable=SC2034 # pw is used by the eval'd check below
pw=$(jq -r .password "$S/tester-body.json")
check "tester password is random, never printed, and in no argv" eval '[[ ${#pw} -ge 32 ]] && ! out_has "$pw" && ! grep -qF -- "$pw" "$T/work/receipt.txt" "$S/calls.log"'
check "token and bodies reach curl through 0600 files that the driver removes" eval '! grep -q secret-token-value "$S/calls.log" &&
  ! grep "^curl " "$S/calls.log" | grep -vqF -- "-H @$T/work/coder.hdr" && ! grep "^curl " "$S/calls.log" | grep -qE -- "--data [^@]" &&
  [[ $(sort -u "$S/file-modes") == 600 && ! -e $T/work/coder.hdr && ! -e $T/work/coder-body.json && ! -e $T/work/tester.json ]]'
check "agent workspace: tester-owned, from the agent template, ready time logged, delete job checked" eval 'grep -q "create --raw .*ws-e2e-agent.json" "$S/calls.log" &&
  jq -e ".metadata.name == \"coder.e2e-tester.e2e-agent\" and .spec.templateName == \"e2e-agent\"" "$T/work/ws-e2e-agent.json" >/dev/null &&
  out_has "agent time-to-ready: " && out_has "agents: main=connected/ready" && grep -q "^agent_ready_seconds=[0-9]" "$T/work/receipt.txt" &&
  grep -q "workspaces/uid-4?include_deleted=true" "$S/calls.log"'

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
check "fails at create with zero workspace create/apply calls" eval '[[ $RC -ne 0 ]] && bg_stopped && no_mutations_after "$VERSIONS" &&
  grep -q "create workspace through the aggregated API = FAILED" "$T/work/receipt.txt"'

echo "TEST endpoint-mismatch: aggregated API service points at the terminating pod"
ENDPOINT_IP=10.244.0.4 run_scenario endpoint-mismatch; summary
check "fails before any mutation" eval 'failed_with "endpoints do not match serving pod" && no_mutations_after ""'

echo "TEST template-import-failed: setup failure"
run_scenario template-import-failed TEMPLATE_JOB_STATUS=failed; summary
check "fails on import status and never creates a workspace" eval 'failed_with "job status failed" && no_mutations_after "kubectl apply"'

echo "TEST build-failed: create build fails"
run_scenario build-failed; summary
check "fails on build status; no update" eval 'failed_with "ended in status failed" && no_mutations_after "$VERSIONS,kubectl apply"'

echo "TEST watch-unregistered: watch never reports 200 OK"
run_scenario watch-unregistered; summary
check "bounded failure before the update" eval 'failed_with "timed out after 3s waiting for: watch registration" && no_mutations_after "$APPLIES" && ((SECS < 30))'

echo "TEST event-token-mismatch: MODIFIED event token differs from update response"
run_scenario event-token-mismatch; summary
check "bounded event match fails; no rename or delete" \
  eval 'failed_with "timed out after 2s waiting for: MODIFIED event" && no_mutations_after "$APPLIES,kubectl replace" && ((SECS < 30))'

echo "TEST rename-rejected: Coder rejects the out-of-band rename"
run_scenario rename-rejected; summary
check "fails at rename; no delete" eval '[[ $RC -ne 0 ]] && bg_stopped && no_mutations_after "$APPLIES,kubectl replace,$TRANS,curl PATCH"'

echo "TEST delete-ignores-preconditions: stale DELETE (old token, live UID) is accepted"
run_scenario delete-ignores-preconditions; summary
check "fails on missing 409; no later delete or recreate" \
  eval 'failed_with "expected (Conflict) but request succeeded" && no_mutations_after "$APPLIES,kubectl replace,$TRANS,curl PATCH,kubectl replace,kubectl delete"'

echo "TEST wrong-uid-ignored: wrong-UID delete is accepted"
run_scenario wrong-uid-ignored; summary
check "fails on missing 409; no live delete or recreate" \
  eval 'failed_with "expected (Conflict) but request succeeded" && no_mutations_after "$APPLIES,kubectl replace,$TRANS,curl PATCH,kubectl replace,kubectl delete,kubectl delete"'

echo "TEST render-fail-delete: DeleteOptions render fails inside expect_error (errexit off)"
run_scenario render-fail-delete; summary
check "fails with zero kubectl delete calls and no recreate" eval 'failed_with "expected (Conflict)" && no_mutations_after "$APPLIES,kubectl replace,$TRANS,curl PATCH,kubectl replace"'

echo "TEST delete-job-failed: workspace is 404 but its delete job failed"
run_scenario delete-job-failed DELETE_JOB_STATUS=failed; summary
check "fails on delete job status; no recreate" eval 'failed_with "delete job of uid-1 ended in status failed" && no_mutations_after "$APPLIES,kubectl replace,$TRANS,curl PATCH,kubectl replace,kubectl delete,kubectl delete,kubectl delete"'

echo "TEST watch-exits: watch registers then exits before the update"
run_scenario watch-exits; summary
check "fails before the update" eval 'failed_with "watch process exited before the update" && no_mutations_after "$APPLIES"'

echo "TEST recreate-build-drift: prior-UID delete returns 409 but the recreated build changes"
run_scenario recreate-build-drift; summary
check "fails on recreated latest build status" eval 'failed_with "recreated object changed after prior-UID delete"'

STALE="$APPLIES,kubectl replace,$TRANS,curl PATCH,kubectl replace,kubectl delete"
# name|expected message|mutations: each #109 negative must stop before the wrong-UID/live deletes and recreate.
while IFS='|' read -r name msg muts; do
  echo "TEST $name (#109)"
  run_scenario "$name" </dev/null; summary
  check "fails with '$msg'; mutations stop at: $muts" eval 'failed_with "$msg" && no_mutations_after "$muts"'
done <<CASES
old-timestamp-rename|rename did not change resourceVersion|$APPLIES,kubectl replace,$TRANS,curl PATCH
stale-update-accepted|expected (Conflict) but request succeeded|$APPLIES,kubectl replace,$TRANS,curl PATCH,kubectl replace
stale-side-effect-latest|backend latest build changed after stale requests|$STALE
stale-side-effect-count|backend build count changed after stale requests|$STALE
stale-side-effect-object|object changed after stale requests|$STALE
list-missing|missing from LIST|$STALE
list-token-differs|LIST token differs from GET token|$STALE
CASES

echo "TEST template-import-async (#105): old behavior, import still running right after apply"
run_scenario template-import-async TEMPLATE_JOB_STATUS=running; summary
check "readiness assertion fails before any workspace create" eval 'failed_with "template import not ready right after apply" && no_mutations_after "kubectl apply"'

echo "TEST template-apply-error (#105): kubectl apply of the template fails"
run_scenario template-apply-error; summary
check "fails; receipt records the apply output; no later mutation" eval 'failed_with "InternalError" && no_mutations_after "kubectl apply" &&
  grep -q "^apply_failure=apply config/e2e/codertemplate.yaml: Error from server (InternalError)" "$T/work/receipt.txt"'

echo "TEST reapply-error (#105): identical workspace re-apply fails with a precondition error"
run_scenario reapply-error; summary
check "fails; receipt records the apply output; no lifecycle mutation" eval 'failed_with "precondition failed" && no_mutations_after "$APPLIES" &&
  grep -q "^apply_failure=apply .*ws-e2e-lifecycle.json: Error from server (Conflict)" "$T/work/receipt.txt"'

echo "TEST reapply-new-version (#105): identical template re-apply creates a template version"
run_scenario reapply-new-version; summary
check "fails on template state; no lifecycle mutation" eval 'failed_with "template changed after identical re-apply" && no_mutations_after "$APPLIES"'

echo "TEST reapply-new-build (#105): identical workspace re-apply creates a build"
run_scenario reapply-new-build; summary
check "fails on workspace state; no lifecycle mutation" eval 'failed_with "workspace changed after identical re-apply" && no_mutations_after "$APPLIES"'

echo "TEST log-empty (#148): coderworkspaces/log returns no bytes"
run_scenario log-empty; summary
check "fails on the empty log; no lifecycle mutation" eval 'failed_with "build log of coder.coder-k8s-operator.e2e-lifecycle is empty" && no_mutations_after "$APPLIES"'

echo "TEST log-leak (#148): the server log contains a build log line"
run_scenario log-leak; summary
check "fails on the leak; no lifecycle mutation" eval 'failed_with "the server log contains a build log line" && no_mutations_after "$APPLIES"'

echo "TEST follow-fails (#148): the log follow of the stop build fails"
run_scenario follow-fails; summary
check "fails on the follow; no mutation after the update" eval 'failed_with "log follow failed" && no_mutations_after "$APPLIES,kubectl replace"'

echo "TEST promote-not-applied (#149): promote answers Promoted but Coder keeps the old version"
run_scenario promote-not-applied; summary
check "fails on the Coder cross-check; no workspace mutation" eval 'failed_with "Coder does not show v1 active after the rollback" &&
  no_mutations_after "kubectl apply,kubectl apply,kubectl create"'

echo "TEST promote-repeat-writes (#149): a repeated promote changes the template in Coder"
run_scenario promote-repeat-writes; summary
check "fails on updated_at; no workspace mutation" eval 'failed_with "repeated promote v1 wrote to Coder" &&
  no_mutations_after "kubectl apply,kubectl apply,kubectl create,kubectl create"'

echo "TEST repeat-start-queues (#148): a repeated start of a running workspace queues a build"
run_scenario repeat-start-queues; summary
check "fails on the repeated start; no rename" eval 'failed_with "repeated start did not answer Unchanged" &&
  no_mutations_after "$APPLIES,kubectl replace,kubectl create,kubectl create"'

echo "TEST dryrun-queues (#148): a dry-run stop queues a build"
run_scenario dryrun-queues; summary
check "fails on the dry-run stop; no real stop and no rename" eval 'failed_with "dry-run stop did not answer WouldQueue" &&
  no_mutations_after "$APPLIES,kubectl replace,kubectl create,kubectl create,kubectl create"'

echo "TEST tester-create-fails: Coder rejects the tester user"
run_scenario tester-create-fails; summary
check "fails at the tester; no agent template or workspace" eval 'failed_with "cannot create the tester user e2e-tester: curl: (22) The requested URL returned error: 500" &&
  no_mutations_after "$LIFECYCLE,curl POST"'

echo "TEST tester-exists: a local rerun finds the tester (409) and reuses it"
run_scenario tester-exists; summary
check "reuses the password tester, still activates it, and passes" eval '[[ $RC -eq 0 ]] && out_has "tester user e2e-tester exists (409): reusing it" &&
  grep -q "PUT .*/api/v2/users/user-tester/status/activate" "$S/calls.log" && grep -qx "tester=e2e-tester/user-tester" "$T/work/receipt.txt"'

echo "TEST tester-exists-oidc: the existing e2e-tester is not a password user"
run_scenario tester-exists-oidc; summary
check "fails before activation and the agent template" eval 'failed_with "tester user e2e-tester has an unexpected shape or organization" &&
  no_mutations_after "$LIFECYCLE,curl POST"'

echo "TEST tester-stays-dormant: activating the tester does not make it active"
run_scenario tester-stays-dormant; summary
check "fails at the tester; no agent template or workspace" eval 'failed_with "cannot activate the tester user e2e-tester" && no_mutations_after "$LIFECYCLE,curl POST,curl PUT"'

echo "TEST agent-start-error: the agent lifecycle ends in start_error"
run_scenario agent-start-error; summary
check "fails at once on the lifecycle; no delete" eval 'failed_with "failed to start: main=connected/start_error" &&
  no_mutations_after "$LIFECYCLE,curl POST,curl PUT,kubectl apply,kubectl create" && ((SECS < 30))'

echo "TEST agent-never-ready: the agent never connects"
run_scenario agent-never-ready E2E_AGENT_TIMEOUT_SECONDS=2; summary
check "bounded failure; no delete" eval 'failed_with "timed out after 2s waiting for: agents of coder.e2e-tester.e2e-agent connected and ready" &&
  no_mutations_after "$LIFECYCLE,curl POST,curl PUT,kubectl apply,kubectl create" && ((SECS < 30))'

echo "TEST agent-delete-failed: the agent workspace is 404 but its delete job failed"
run_scenario agent-delete-failed; summary
check "fails on the delete job status" eval 'failed_with "delete job of uid-4 ended in status failed" &&
  no_mutations_after "$LIFECYCLE,curl POST,curl PUT,kubectl apply,kubectl create,kubectl delete"'

# name|expected message|mutations: each TemplateTest phase failure stops before the next mutation.
while IFS='|' read -r name msg muts; do
  echo "TEST $name (CoderTemplateTest phases)"
  run_scenario "$name" E2E_NAMESPACE_DELETE_TIMEOUT_SECONDS=2 </dev/null; summary
  check "fails with '$msg'" eval 'failed_with "$msg" && no_mutations_after "$muts"'
done <<CASES
owner-patch-fails|cannot set templateTests.ownerUserID on codercontrolplane coder|$AGENT,kubectl patch
keys-count-fails|cannot count the api_keys rows of the tester|$AGENT,kubectl patch
operator-no-keys|the operator user has no api_keys rows|$AGENT,kubectl patch
tt-failed|template test e2e-pass-1 failed: Failed AgentStartError|$AGENT,kubectl patch,kubectl create
ws-rows-2|expected exactly one workspaces row named ktt-e2e-pass-1 (deleted rows included), found 2|$TESTS
coder-rolls|setting templateTests.ownerUserID rolled deploy/coder|$TESTS
keys-after-fails|cannot count the api_keys rows of the tester after the tests|$PHASED
coder-stays-up|timed out after 3s waiting for: deploy/coder without pods|$PHASED,kubectl create,kubectl patch
ns-delete-stuck|timed out after 2s waiting for: test, control plane, and content of namespace coder deleted|$PHASED,kubectl create,kubectl patch,kubectl delete
fail-ignored|template test e2e-agent-fail ended Succeeded Succeeded deleted=True/Deleted, want Failed AgentStartError|$TESTS,kubectl create
badparam-created|template test e2e-bad-param: expected 0 workspaces rows named ktt-e2e-bad-param (deleted rows included), found 1|$TESTS,kubectl create,kubectl create
restart-failed|template test e2e-restart failed: Failed AgentStartError|$TESTS,kubectl create,kubectl create,kubectl create,kubectl delete
midrun-delete-failed|delete job of ttws-e2e-midrun ended in status failed|$TESTS,kubectl create,kubectl create,kubectl create,kubectl delete,kubectl create,kubectl delete
immutable-accepted|a patch of spec.timeoutSeconds succeeded|$TESTS,kubectl create,kubectl create,kubectl create,kubectl delete,kubectl create,kubectl delete,kubectl patch
dryrun-persists|the server dry run created template test e2e-dry-run|$TESTS,kubectl create,kubectl create,kubectl create,kubectl delete,kubectl create,kubectl delete,kubectl patch
ttl-failed|template test e2e-ttl was removed before it passed|$PHASED
CASES

echo "TEST missing-built-id: BUILT_IMAGE_ID is not a sha256 ID"
run_scenario missing-built-id BUILT_IMAGE_ID=e2e; summary
check "fails before any tool call" eval '[[ $RC -ne 0 ]] && out_has "not a sha256 image ID" && [[ ! -s $S/calls.log ]]'

((FAILURES == 0)) || { echo "FAILED: $FAILURES check(s)"; exit 1; }
echo "ALL OFFLINE TESTS PASSED"
