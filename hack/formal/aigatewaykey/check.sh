#!/usr/bin/env bash
# Model-checks the CoderAIGatewayKey key lifecycle (AIGatewayKey.tla) against
# facts read from the Go source. See .mux/skills/verify-aigatewaykey-model/SKILL.md.
#
# Usage: hack/formal/aigatewaykey/check.sh [--config quick|deep|stall] [--out DIR]
#                                          [--set NAME=VALUE]... [--update-fingerprints]
#
# Exit codes: 0 pass, and every covered function matches its fingerprint.
#             1 a rule is violated (the trace is in DIR/tlc.out).
#             2 the check could not run (a fact is unreadable, TLC failed).
#             3 the rules hold, but covered code changed since the last review (STALE).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../../.." && pwd)
config=quick
out=""
update=false
declare -a sets=()
while [[ $# -gt 0 ]]; do
	case $1 in
	--config) config=$2; shift 2 ;;
	--out) out=$2; shift 2 ;;
	--set) sets+=("$2"); shift 2 ;;
	--update-fingerprints) update=true; shift ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

ctl=internal/controller
f_ctl=$ctl/coderaigatewaykey_controller.go
f_issue=$ctl/coderaigatewaykey_issue.go
f_recover=$ctl/coderaigatewaykey_recover.go
f_retire=$ctl/coderaigatewaykey_retire.go
f_rotate=$ctl/coderaigatewaykey_rotate.go
f_delete=$ctl/coderaigatewaykey_delete.go
f_secret=$ctl/owned_secret.go

# body FILE SIG prints the numbered lines of the function whose `func` line
# contains SIG, through its closing brace at column 0.
body() {
	awk -v sig="$2" '
		!in_f && index($0, "func ") == 1 && index($0, sig) { in_f = 1 }
		in_f { print NR ": " $0 }
		in_f && $0 == "}" { exit }
	' "$root/$1"
}

# line FILE SIG REGEX prints the line number of the first match in the function, or nothing.
line() {
	local numbered start n
	numbered=$(body "$1" "$2")
	[[ -n $numbered ]] || return 0
	start=${numbered%%:*}
	n=$(printf '%s\n' "$numbered" | sed -E 's/^[0-9]+: //' | grep -n -E -m1 -- "$3" | cut -d: -f1 || true)
	[[ -z $n ]] || echo $((start + n - 1))
}

declare -A fact=()
declare -A evidence=()
declare -a order=()
set_fact() { fact[$1]=$2; evidence[$1]=$3; order+=("$1"); }

# has NAME FILE SIG REGEX...: TRUE when every REGEX matches in the function.
has() {
	local name=$1 file=$2 sig=$3 where="" n
	shift 3
	for re in "$@"; do
		n=$(line "$file" "$sig" "$re")
		if [[ -z $n ]]; then set_fact "$name" FALSE "$file $sig: no match for /$re/"; return; fi
		where+="$file:$n "
	done
	set_fact "$name" TRUE "$where"
}

# before NAME FILE SIG A B: TRUE when A matches on an earlier line than B.
before() {
	local a b
	a=$(line "$2" "$3" "$4")
	b=$(line "$2" "$3" "$5")
	if [[ -n $a && -n $b && $a -lt $b ]]; then
		set_fact "$1" TRUE "$2:$a before $2:$b"
	else
		set_fact "$1" FALSE "$2 $3: /$4/ (line ${a:-none}) is not before /$5/ (line ${b:-none})"
	fi
}

# number NAME FILE REGEX: the first capture group of REGEX in FILE.
number() {
	local v
	v=$(grep -E -m1 -- "$3" "$root/$2" | sed -E "s/.*$3.*/\\1/" || true)
	if [[ ! $v =~ ^[0-9]+$ ]]; then
		echo "fact $1 is unreadable: no match for /$3/ in $2. Update check.sh and the model." >&2
		exit 2
	fi
	set_fact "$1" "$v" "$2: $(grep -n -E -m1 -- "$3" "$root/$2" | cut -d: -f1)"
}

all_true() { # NAME A B: TRUE when both facts are TRUE.
	if [[ ${fact[$2]} == TRUE && ${fact[$3]} == TRUE ]]; then set_fact "$1" TRUE "${evidence[$2]}; ${evidence[$3]}"
	else set_fact "$1" FALSE "${evidence[$2]}; ${evidence[$3]}"; fi
}

# --- Facts. Each one is a line of Go that a rule of the model depends on. ---
number Settle "$f_recover" 'aiGatewayKeySettleWindow = ([0-9]+) \* time\.Minute'
number WriteAge "$f_issue" 'aiGatewayKeyMaxWriteAge += ([0-9]+) \* time\.Minute'
number MaxRetiring "$f_recover" 'aiGatewayKeyMaxRetiring = ([0-9]+)$'
number SecretTries "$f_issue" 'aiGatewayKeySecretWriteTries += ([0-9]+)$'
number RotateScheduledRoom "$f_rotate" '^[[:space:]]+limit := ([0-9]+)$'
number RotateRequestedRoom "$f_rotate" '^[[:space:]]+limit = ([0-9]+)$'

before _held_skip "$f_recover" 'deleteOwnKey(' '^[[:space:]]+if held == k\.ID \{$' 'sdk\.DeleteAIGatewayKey\(ctx, k\.ID\)'
before _live_call "$f_recover" 'deleteOwnKey(' '^[[:space:]]+held, err := r\.liveSecretHolds\(ctx, key, ' '^[[:space:]]+if held == k\.ID \{$'
has _live_read "$f_recover" 'liveSecretHolds(' '^[[:space:]]+err := r\.APIReader\.Get\(ctx, types\.NamespacedName\{Namespace: key\.Namespace, Name: key\.Spec\.SecretName\}, secret\)$'
all_true _held_call _held_skip _live_call
all_true GuardLiveReadBeforeDelete _held_call _live_read
has GuardOrphanSkipsHeld "$f_recover" 'aiGatewayKeyOrphan(' '\(holds != uuid\.Nil && k\.ID == holds\)'
has GuardRetireSkipsHeld "$f_retire" 'aiGatewayKeyRetireState(' '\(holds != uuid\.Nil && own\[i\]\.ID == holds\)'
has _gate_orphan "$f_recover" 'aiGatewayKeyOrphan(' '^[[:space:]]+if !ok \|\| serial >= status\.NextSerial \|\| '
has _gate_created "$f_recover" 'nextAIGatewayKeyAction(' '^[[:space:]]+return serial, ok && serial < status\.NextSerial$'
all_true GuardSerialGate _gate_orphan _gate_created
before _marker_serial "$f_issue" 'issueKey(' '^[[:space:]]+key\.Status\.NextSerial = serial \+ 1$' 'sdk\.CreateAIGatewayKey\(attemptCtx, '
before _marker_write "$f_issue" 'issueKey(' '^[[:space:]]+if err := r\.writeStatus\(ctx, key, before\); err != nil \{$' 'sdk\.CreateAIGatewayKey\(attemptCtx, '
all_true GuardMarkerBeforeCreate _marker_serial _marker_write
before GuardWriteAge "$f_issue" 'writeKeySecret(' '^[[:space:]]+if r\.Clock\.Since\(attemptStart\) >= aiGatewayKeyMaxWriteAge \{$' 'writeOwnedSecret\(ctx, r\.Client, key, aiGatewayKeyKind, secret, data, labels, annotations\)'
has GuardStatusCAS "$f_ctl" '(r *CoderAIGatewayKeyReconciler) writeStatus(' '^[[:space:]]+if err := r\.Status\(\)\.Update\(ctx, key\); err != nil \{$'
has _secret_update "$f_secret" 'writeOwnedSecret(' '^[[:space:]]+updated := secret\.DeepCopy\(\)$' '^[[:space:]]+if err := c\.Update\(ctx, updated, opts\.\.\.\); err != nil \{$'
if [[ -n $(line "$f_secret" 'writeOwnedSecret(' 'ResourceVersion = [^=]') ]]; then
	set_fact _secret_keeps_rv FALSE "$f_secret writeOwnedSecret sets ResourceVersion"
else
	set_fact _secret_keeps_rv TRUE "$f_secret writeOwnedSecret keeps the read ResourceVersion"
fi
all_true GuardSecretCAS _secret_update _secret_keeps_rv
before GuardDeleteSettle "$f_delete" 'deleteStep(' '^[[:space:]]+if settleAt := key\.Status\.LastCreateAttemptTime\.Add\(aiGatewayKeySettleWindow\); now\.Before\(settleAt\) \{$' 'deleted, _, failure, err := r\.deleteKeys\(ctx, sdk, key\)'
before GuardFinalizerFirst "$f_ctl" '(r *CoderAIGatewayKeyReconciler) Reconcile(' '^[[:space:]]+if err := r\.patchFinalizer\(ctx, key, true\); err != nil \{$' 'if step, err = r\.observe\(ctx, key, before\); err != nil'

echo "Facts read from the Go source:"
for k in "${order[@]}"; do
	[[ $k == _* ]] && continue
	printf '  %-26s %-5s %s\n' "$k" "${fact[$k]}" "${evidence[$k]}"
done

# --- Fingerprints of the covered functions. ---
fingerprints=$here/covered.sha256
covered=(
	"$f_ctl|(r *CoderAIGatewayKeyReconciler) Reconcile("
	"$f_ctl|(r *CoderAIGatewayKeyReconciler) observe("
	"$f_ctl|func aiGatewayKeySecretHolds("
	"$f_ctl|func ownAIGatewayKeys("
	"$f_ctl|func aiGatewayKeySerial("
	"$f_ctl|(r *CoderAIGatewayKeyReconciler) patchFinalizer("
	"$f_ctl|(r *CoderAIGatewayKeyReconciler) writeStatus("
	"$f_issue|(r *CoderAIGatewayKeyReconciler) issueKey("
	"$f_issue|(r *CoderAIGatewayKeyReconciler) writeKeySecret("
	"$f_issue|func setAIGatewayKeyCurrent("
	"$f_issue|func aiGatewayKeyCreateFailure("
	"$f_recover|func nextAIGatewayKeyAction("
	"$f_recover|func aiGatewayKeyOrphan("
	"$f_recover|func aiGatewayKeyDriftBlocked("
	"$f_recover|func aiGatewayKeyRearmed("
	"$f_recover|func aiGatewayKeyRetiring("
	"$f_recover|func adoptAIGatewayKey("
	"$f_recover|func retireAIGatewayKey("
	"$f_recover|(r *CoderAIGatewayKeyReconciler) sweepOrphan("
	"$f_recover|(r *CoderAIGatewayKeyReconciler) deleteOwnKey("
	"$f_recover|(r *CoderAIGatewayKeyReconciler) liveSecretHolds("
	"$f_recover|(r *CoderAIGatewayKeyReconciler) deleteOnly("
	"$f_retire|func aiGatewayKeyRetireState("
	"$f_retire|(r *CoderAIGatewayKeyReconciler) retireKey("
	"$f_retire|func dropAIGatewayKeyRetiring("
	"$f_rotate|func aiGatewayKeyRotationDue("
	"$f_delete|(r *CoderAIGatewayKeyReconciler) deleteStep("
	"$f_delete|(r *CoderAIGatewayKeyReconciler) deleteKeys("
	"$f_secret|func writeOwnedSecret("
	"$f_secret|func getOwnedSecret("
)
current=$(for c in "${covered[@]}"; do
	file=${c%%|*} sig=${c#*|}
	text=$(body "$file" "$sig" | sed -E 's/^[0-9]+: //')
	[[ -n $text ]] || { echo "covered function not found: $file $sig" >&2; exit 2; }
	printf '%s  %s  %s\n' "$(printf '%s\n' "$text" | sha256sum | cut -c1-16)" "$file" "$sig"
done)
# A changed body shows on both sides. A function that left the covered list
# shows only as "<", and a new one only as ">": both are stale.
stale=$(diff <(cat "$fingerprints" 2>/dev/null) <(printf '%s\n' "$current") | sed -n -e 's/^> /  now:      /p' -e 's/^< /  reviewed: /p' || true)

# --- The model configuration. ---
declare -A c=(
	[MaxSerial]=2 [MaxTime]=8 [MaxUserActs]=1
	[CommitMax]=2 [ReqMax]=1 [NoStall]=TRUE
)
case $config in
quick) ;;
deep) c[MaxSerial]=3 ;;
stall) c[NoStall]=FALSE ;;
*) echo "unknown config: $config" >&2; exit 2 ;;
esac
for k in "${order[@]}"; do [[ $k == _* ]] || c[$k]=${fact[$k]}; done
for kv in "${sets[@]}"; do
	c[${kv%%=*}]=${kv#*=}
	echo "Override: ${kv%%=*} = ${kv#*=} (not the value from the code or the config)"
done

out=${out:-$(mktemp -d "${TMPDIR:-/tmp}/aigatewaykey-tlc.XXXXXX")}
mkdir -p "$out"
out=$(cd "$out" && pwd)
cp "$here/AIGatewayKey.tla" "$out/"
{
	echo "SPECIFICATION Spec"
	echo "CONSTANTS"
	for k in $(printf '%s\n' "${!c[@]}" | sort); do echo "  $k = ${c[$k]}"; done
	echo "INVARIANT SecretKeyAlive"
	echo "INVARIANT NoLeakAfterRelease"
	echo "CONSTRAINT StateBound"
	echo "CHECK_DEADLOCK FALSE"
} >"$out/AIGatewayKey.cfg"

# --- TLC 1.7.4, pinned by checksum. ---
jar_dir=${TLA2TOOLS_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/coder-k8s-tla}
jar=$jar_dir/tla2tools-1.7.4.jar
jar_sha=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88
if [[ ! -f $jar ]] || ! echo "$jar_sha  $jar" | sha256sum -c --quiet - 2>/dev/null; then
	mkdir -p "$jar_dir"
	curl -fsSL -o "$jar.tmp" https://github.com/tlaplus/tlaplus/releases/download/v1.7.4/tla2tools.jar ||
		{ echo "downloading tla2tools.jar failed" >&2; rm -f "$jar.tmp"; exit 2; }
	echo "$jar_sha  $jar.tmp" | sha256sum -c --quiet - || { echo "tla2tools.jar checksum mismatch" >&2; exit 2; }
	mv "$jar.tmp" "$jar"
fi
java=${JAVA:-java}
workers=${TLC_WORKERS:-$(($(nproc) < 8 ? $(nproc) : 8))}

echo "Running TLC ($config, $workers workers) in $out"
rc=0
(cd "$out" && "$java" -XX:+UseParallelGC "-Xmx${TLC_HEAP:-8g}" -cp "$jar" tlc2.TLC \
	-workers "$workers" -cleanup -metadir "$out/meta" -config AIGatewayKey.cfg AIGatewayKey.tla) >"$out/tlc.out" 2>&1 || rc=$?
grep -E 'distinct states found|Finished in' "$out/tlc.out" | tail -2 | sed 's/^/  /' || true

case $rc in
0) ;;
12)
	echo "VIOLATION: $(grep -m1 -E 'Invariant .* is violated' "$out/tlc.out")"
	echo "  Trace: $out/tlc.out. Read it with /statechart on $out/AIGatewayKey.tla and $out/AIGatewayKey.cfg."
	exit 1
	;;
*)
	echo "ERROR: TLC exited $rc. See $out/tlc.out." >&2
	exit 2
	;;
esac
if $update; then
	if ((${#sets[@]} > 0)); then
		echo "Refusing --update-fingerprints with --set overrides: the pass must use the values from the code." >&2
		exit 2
	fi
	printf '%s\n' "$current" >"$fingerprints"
	echo "Wrote $fingerprints. Move the sync commit in the skill only after you reviewed the model against this code."
	stale=""
fi
if [[ -n $stale ]]; then
	echo "PASS on the model, but STALE: these covered functions changed since the last review:"
	echo "$stale"
	echo "Review the model against them, then run with --update-fingerprints."
	exit 3
fi
echo "PASS: SecretKeyAlive, NoLeakAfterRelease hold ($config bounds)."
