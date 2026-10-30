#!/usr/bin/env bash
# Promote a Coder template version only after its CoderTemplateTest passed.
#
#   promote.sh name <prefix> <version>  Print a Kubernetes object name for a
#                                       Coder template version name.
#   promote.sh promote <test.json>      Wait until the test in the cluster has
#                                       the spec of <test.json> and finished,
#                                       then promote the version it tested.
#
# See docs/how-to/gitops.md ("Gate before promotion").
# jq programs use $ for jq variables, so they stay in single quotes.
# shellcheck disable=SC2016
set -euo pipefail
export LC_ALL=C

readonly DEFAULT_TIMEOUT_SECONDS=900
# The controller starts the test clock only when it first handles the test.
# This grace covers that delay on top of spec.timeoutSeconds. The controller
# writes Succeeded only after the workspace delete, and Failed at the latest
# at the deadline, so the wait covers the delete too.
readonly START_GRACE_SECONDS=900
readonly POLL_SECONDS=30

die() {
  echo "promote: $*" >&2
  exit 1
}

cmd_name() {
  [[ $# -eq 2 ]] || die "usage: promote.sh name <prefix> <version>"
  local prefix=$1 version=$2 hash slug
  [[ $prefix =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#prefix} -le 40 ]] ||
    die "the prefix must be a DNS label of at most 40 characters: ${prefix}"
  [[ $version =~ ^[a-zA-Z0-9]+([_.-][a-zA-Z0-9]+)*$ && ${#version} -le 64 ]] ||
    die "not a valid Coder version name: ${version}"
  # The hash keeps names apart that differ only in case, '_' or '.'.
  if command -v sha256sum >/dev/null; then
    hash=$(printf '%s' "$version" | sha256sum | cut -c1-8)
  else
    hash=$(printf '%s' "$version" | shasum -a 256 | cut -c1-8)
  fi
  slug=$(printf '%s' "$version" | tr 'A-Z_.' 'a-z--')
  slug=${slug:0:$((63 - ${#prefix} - 10))}
  slug=${slug%-}
  printf '%s-%s-%s\n' "$prefix" "$slug" "$hash"
}

# Compares the whole spec, so a test that someone created first under this
# name with other parameters, a timeout or a version never counts. The API
# server defaults timeoutSeconds, and the order of parameters has no meaning.
readonly VERDICT='
def norm: .timeoutSeconds //= $defaultTimeout
  | .parameters = ((.parameters // []) | map(.value //= "") | sort_by(.name));
if (.spec | norm) != ($want[0].spec | norm) then "SpecMismatch"
elif .metadata.deletionTimestamp != null then "Deleting"
elif (.status.observedGeneration // 0) != .metadata.generation then "Pending"
elif .status.phase == "Failed" then "Failed"
elif .status.phase == "Succeeded" and any(.status.conditions[]?; .type == "Ready" and .status == "True") then "Passed"
else "phase \(.status.phase // "none"), Ready=\(first(.status.conditions[]? | select(.type == "Ready") | .status) // "none")"
end'

cmd_promote() {
  [[ $# -eq 1 ]] || die "usage: promote.sh promote <codertemplatetest.json>"
  local want name ns template timeout deadline obj verdict id answer
  want=$(jq -ce . "$1") || die "cannot read $1"
  jq -e '.apiVersion == "coder.com/v1alpha1" and .kind == "CoderTemplateTest"
    and (.metadata.name | type) == "string" and (.metadata.namespace | type) == "string"
    and (.spec.version.active | not)' <<<"$want" >/dev/null ||
    die "$1 must be a CoderTemplateTest with metadata.namespace and a fixed spec.version"
  name=$(jq -r .metadata.name <<<"$want")
  ns=$(jq -r .metadata.namespace <<<"$want")
  template=$(jq -r .spec.template <<<"$want")
  timeout=$(jq -r ".spec.timeoutSeconds // ${DEFAULT_TIMEOUT_SECONDS}" <<<"$want")
  deadline=$(($(date +%s) + timeout + START_GRACE_SECONDS))

  while :; do
    # One GET: the spec check and the result come from the same object.
    if obj=$(kubectl --request-timeout=30s -n "$ns" get codertemplatetest "$name" -o json); then
      # --slurpfile, not --argjson: a test can be larger than one argument may be.
      verdict=$(jq -r --slurpfile want "$1" --argjson defaultTimeout "$DEFAULT_TIMEOUT_SECONDS" "$VERDICT" <<<"$obj")
      case $verdict in
      SpecMismatch) die "refusing to promote: the spec of ${ns}/${name} differs from $1" ;;
      Failed) die "test ${ns}/${name} failed: $(jq -r '"\(.status.reason): \(.status.message)"' <<<"$obj")" ;;
      Passed) break ;;
      esac
      echo "promote: waiting for test ${ns}/${name}: ${verdict}" >&2
    fi
    (($(date +%s) < deadline)) ||
      die "test ${ns}/${name} did not finish within $((timeout + START_GRACE_SECONDS))s"
    sleep "$POLL_SECONDS"
  done

  id=$(jq -r '.status.templateVersionID // ""' <<<"$obj")
  [[ $id =~ ^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$ ]] ||
    die "assertion failed: test ${ns}/${name} succeeded without a valid status.templateVersionID: ${id}"
  # Never retry here: after a failed or unconfirmed promotion, another writer
  # can have activated a third version, and the last writer wins.
  answer=$(jq -cn --arg id "$id" '{spec: {versionID: $id}}' |
    kubectl --request-timeout=60s create --raw "/apis/aggregation.coder.com/v1alpha1/namespaces/${ns}/codertemplates/${template}/promote" -f -) ||
    die "the promotion of version ${id} of ${template} failed. Read the active version before you promote again."
  jq -e --arg id "$id" '(.status.result == "Promoted" or .status.result == "AlreadyActive")
    and .status.activeVersionID == $id' <<<"$answer" >/dev/null ||
    die "unexpected promotion answer: ${answer}"
  echo "promote: ${template}: $(jq -r .status.result <<<"$answer") version ${id} (test ${ns}/${name})"
}

case ${1:-} in
name)
  shift
  cmd_name "$@"
  ;;
promote)
  shift
  cmd_promote "$@"
  ;;
*) die "usage: promote.sh name <prefix> <version> | promote.sh promote <test.json>" ;;
esac
