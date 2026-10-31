#!/usr/bin/env bash
# Offline tests for hack/e2e-workspace-pool.sh (#153). kubectl and curl are stubs on PATH; nothing touches a
# cluster. These scenarios check that the driver stops before any write when its inputs are wrong. PR 13 of the
# #153 plan adds a scenario for each phase.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DRIVER=$ROOT/hack/e2e-workspace-pool.sh
BASE=$(mktemp -d)
trap 'rm -rf "$BASE"' EXIT
STUBS=$BASE/bin
mkdir -p "$STUBS"
FAILURES=0

cat >"$STUBS/kubectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "kubectl $*" >>"$STUB_STATE/calls.log"
case " $* " in
  *" port-forward "*) exec sleep 60 ;;
  *" get codercontrolplane "*)
    if [[ $SCENARIO == no-token-ref ]]; then echo '{"spec": {}, "status": {}}'
    else echo '{"spec": {}, "status": {"operatorTokenSecretRef": {"name": "tok", "key": "token"}}}'; fi ;;
  *" get secret tok "*) printf '{"data": {"token": "%s"}}\n' "$(printf stub-token | base64)" ;;
  *) echo "stub kubectl: unexpected call" >&2; exit 1 ;;
esac
STUB

cat >"$STUBS/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "curl $*" >>"$STUB_STATE/calls.log"
method=GET url=""
while (($#)); do
  case $1 in
    -X) method=$2; shift 2 ;;
    -H | --data | --max-time) shift 2 ;;
    http://*) url=$1; shift ;;
    *) shift ;;
  esac
done
case "$method ${url#http://127.0.0.1:*/}" in
  "GET api/v2/buildinfo") echo '{"version": "v2.37.2"}' ;;
  "GET api/v2/organizations") echo '[{"id": "org-1", "is_default": true}]' ;;
  "POST api/v2/users") echo "curl: (22) The requested URL returned error: 409" >&2; exit 22 ;;
  "GET api/v2/users/e2e-pool-owner")
    echo '{"id": "user-1", "username": "e2e-pool-owner", "login_type": "oidc", "organization_ids": ["org-1"]}' ;;
  *) echo "curl: (22) The requested URL returned error: 404" >&2; exit 22 ;;
esac
STUB
chmod +x "$STUBS/kubectl" "$STUBS/curl"

# run_scenario <name> <expected FAIL message>: the driver must fail with the message and send no write.
run_scenario() {
  local name=$1 want=$2 dir=$BASE/$1 rc=0
  mkdir -p "$dir"
  : >"$dir/calls.log"
  (cd "$ROOT" && PATH="$STUBS:$PATH" STUB_STATE=$dir SCENARIO=$name E2E_WORKDIR=$dir/work E2E_POLL_SECONDS=0.1 \
    E2E_TIMEOUT_SECONDS=5 GITHUB_ACTIONS=true timeout 60 bash "$DRIVER") >"$dir/out.log" 2>&1 || rc=$?
  local problem=""
  if ((rc == 0)); then problem="driver passed"
  elif ! grep -qF "FAIL: $want" "$dir/out.log"; then problem="want FAIL: $want"
  elif grep -E '^kubectl .*( create | apply | patch | delete | scale )' "$dir/calls.log" >/dev/null; then problem="sent a Kubernetes write"
  elif grep -E '^curl .*-X (PUT|PATCH|DELETE) ' "$dir/calls.log" >/dev/null; then problem="sent a Coder write"
  elif grep -F stub-token "$dir/out.log" | grep -vqx '::add-mask::stub-token'; then problem="printed the token outside its mask command"
  fi
  if [[ -n $problem ]]; then
    echo "FAIL $name: $problem (exit $rc)"
    sed 's/^/    /' "$dir/out.log" "$dir/calls.log"
    FAILURES=$((FAILURES + 1))
  else
    echo "PASS $name"
  fi
}

run_scenario no-token-ref "no operator token secret ref"
run_scenario owner-not-password "owner e2e-pool-owner has an unexpected shape or organization"

((FAILURES == 0)) || { echo "$FAILURES scenario(s) failed"; exit 1; }
echo "all workspace pool driver scenarios passed"
