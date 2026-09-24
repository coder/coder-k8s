#!/usr/bin/env bash
# Offline regression tests for scripts/check_codex_comments.sh.
#
# Runs the real helper end-to-end with a stub `gh` (and a no-op `sleep`) on
# PATH; `jq` is the real binary. Fixture bodies under testdata/ are byte-exact
# copies of comments the Codex GitHub App posted on coder/coder-k8s PR #99
# (human requester logins in the full-page fixture are replaced by "alice"),
# except summary-security-advisory-finding-pr89.txt: the PR #89 summary card
# (comment 5812405711) with its finding title replaced by "Example advisory
# finding". Its link, severity and layout are unchanged.
#
# Usage: ./scripts/check_codex_comments_test.sh
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
HELPER="${SCRIPT_DIR}/check_codex_comments.sh"
FIXTURES="${SCRIPT_DIR}/testdata/check_codex_comments"
BODIES="${FIXTURES}/bodies"
BOT="chatgpt-codex-connector"

# sha256 of the primary frozen summary body (PR #99 comment 5732628692).
PRIMARY_SUMMARY_SHA256="cb7624eb0869f631aca6e3efde64656369924c9e5741020e78fa11287a36af2e"
# sha256 of the PR #89 summary card with one advisory finding (see above).
FINDING_SUMMARY_SHA256="d46fcf1cc41042be58dd18b514e14e8e2fd6c6a57957d5b20ce2e90aac1733ee"
# Review comment ID the PR #89 card's finding links to (discussion_r<ID>).
FINDING_ID=4092909628

for tool in jq sha256sum mktemp; do
  command -v "$tool" >/dev/null || {
    echo "❌ ${tool} is required to run these tests"
    exit 1
  }
done
[ -x "$HELPER" ] || {
  echo "❌ helper not found or not executable: ${HELPER}"
  exit 1
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"

# Stub gh: answers `gh repo view` and `gh api graphql`, choosing the fixture
# page by the query text. Fails loudly on anything else.
cat >"$WORK/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  repo)
    echo '{"owner":"coder","name":"coder-k8s"}'
    ;;
  api)
    if [ "${STUB_GH_GRAPHQL_FAIL:-0}" = "1" ]; then
      echo "stub gh: simulated GraphQL failure" >&2
      exit 1
    fi
    query=""
    for arg in "$@"; do
      case "$arg" in
        query=*) query=${arg#query=} ;;
      esac
    done
    case "$query" in
      *'comments(first: 100'*) cat "$STUB_GH_COMMENTS_PAGE" ;;
      # Finding links carry the review comment's databaseId, so the threads query must request it.
      *'reviewThreads(first: 100'*'databaseId'*) cat "$STUB_GH_THREADS_PAGE" ;;
      *)
        echo "stub gh: unexpected GraphQL query" >&2
        exit 2
        ;;
    esac
    ;;
  *)
    echo "stub gh: unexpected invocation: $*" >&2
    exit 2
    ;;
esac
EOF
chmod +x "$WORK/bin/gh"

# No-op sleep keeps the helper's retry backoff from slowing the API-failure case.
printf '#!/usr/bin/env bash\nexit 0\n' >"$WORK/bin/sleep"
chmod +x "$WORK/bin/sleep"

# comment_node <login> <body-string> [isMinimized]
comment_node() {
  jq -cn --arg login "$1" --arg body "$2" --argjson minimized "${3:-false}" \
    '{id: "IC_test", author: {login: $login}, body: $body, createdAt: "2026-09-18T16:00:00Z", isMinimized: $minimized}'
}

# thread_node <login> <isResolved> [first comment databaseId]
thread_node() {
  jq -cn --arg login "$1" --argjson resolved "$2" --argjson database_id "${3:-1}" \
    '{id: "PRRT_test", isResolved: $resolved, comments: {nodes: [{id: "PRRC_test", databaseId: $database_id, author: {login: $login}, body: "Consider handling the nil case.", createdAt: "2026-09-18T16:00:00Z", path: "main.go", line: 1}]}}'
}

# page <field> <node...>  -> GraphQL response with a single page of nodes
page() {
  local field="$1"
  shift
  printf '%s\n' "$@" | jq -cs --arg field "$field" \
    '{data: {repository: {pullRequest: {($field): {pageInfo: {hasNextPage: false, endCursor: null}, nodes: .}}}}}'
}

body() {
  cat "${BODIES}/$1.txt"
}

PASS=0
FAIL=0

# run_case <name> <expected-exit> <expected-output-substring> <comments-page-json> <threads-page-json>
# The helper checks PR ${CASE_PR:-99}.
run_case() {
  local name="$1" expected_exit="$2" expected_text="$3" comments_json="$4" threads_json="$5"
  local comments_file="$WORK/${name}.comments.json" threads_file="$WORK/${name}.threads.json"
  local out_file="$WORK/${name}.out" rc

  printf '%s' "$comments_json" >"$comments_file"
  printf '%s' "$threads_json" >"$threads_file"

  if PATH="$WORK/bin:$PATH" \
    STUB_GH_COMMENTS_PAGE="$comments_file" \
    STUB_GH_THREADS_PAGE="$threads_file" \
    STUB_GH_GRAPHQL_FAIL="${STUB_GH_GRAPHQL_FAIL:-0}" \
    "$HELPER" "${CASE_PR:-99}" >"$out_file" 2>&1; then
    rc=0
  else
    rc=$?
  fi

  # The clean verdict must appear exactly when the helper exits 0.
  local clean_ok
  if grep -qF -- "$CLEAN_MSG" "$out_file"; then
    [ "$expected_exit" -eq 0 ] && clean_ok=1 || clean_ok=0
  else
    [ "$expected_exit" -ne 0 ] && clean_ok=1 || clean_ok=0
  fi

  if [ "$rc" -eq "$expected_exit" ] && [ "$clean_ok" -eq 1 ] && grep -qF -- "$expected_text" "$out_file"; then
    echo "PASS ${name} (exit ${rc})"
    PASS=$((PASS + 1))
  else
    echo "FAIL ${name}: expected exit ${expected_exit} containing '${expected_text}', got exit ${rc}"
    sed 's/^/    | /' "$out_file"
    FAIL=$((FAIL + 1))
  fi
}

NO_THREADS=$(page reviewThreads)
CLEAN_MSG="✅ No unresolved Codex comments found"
BLOCK_MSG="Please address or resolve all Codex comments before merging."

# Fixture integrity: the primary summary body must be the frozen PR #99 bytes.
actual_sha=$(sha256sum "${BODIES}/summary-completed-both.txt" | cut -d' ' -f1)
if [ "$actual_sha" != "$PRIMARY_SUMMARY_SHA256" ]; then
  echo "❌ Assertion failed: summary-completed-both.txt sha256 ${actual_sha} != ${PRIMARY_SUMMARY_SHA256}"
  exit 1
fi
actual_sha=$(sha256sum "${BODIES}/summary-security-advisory-finding-pr89.txt" | cut -d' ' -f1)
if [ "$actual_sha" != "$FINDING_SUMMARY_SHA256" ]; then
  echo "❌ Assertion failed: summary-security-advisory-finding-pr89.txt sha256 ${actual_sha} != ${FINDING_SUMMARY_SHA256}"
  exit 1
fi

# finding_card [sed-script]  -> comments page holding the PR #89 card, optionally edited
finding_card() {
  page comments "$(comment_node "$BOT" "$(body summary-security-advisory-finding-pr89 | sed -e "${1:-}")")"
}

# Second finding line and count, for the two-finding cases.
TWO_FINDINGS_SED="s/^#### Advisory findings (1)\$/#### Advisory findings (2)/;/discussion_r${FINDING_ID}/{p;s/discussion_r${FINDING_ID}/discussion_r$((FINDING_ID + 1))/}"

# --- Known non-finding shapes (must pass) ---------------------------------

run_case summary_running_pr_opened 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-running-pr-opened)")")" "$NO_THREADS"

run_case summary_completed_pr_opened 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-pr-opened)")")" "$NO_THREADS"

run_case summary_completed_security_running 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-security-running)")")" "$NO_THREADS"

run_case summary_completed_both 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both)")")" "$NO_THREADS"

run_case summary_trailing_newline 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both)"$'\n')")" "$NO_THREADS"

run_case normal_clean_verdict 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body normal-clean)")")" "$NO_THREADS"

run_case security_clean_verdict 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body security-clean)")")" "$NO_THREADS"

run_case usage_limits_notice 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "Codex usage limits have been reached for this organization.")")" "$NO_THREADS"

run_case minimized_finding_ignored 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "P1: nil dereference in reconcile loop." true)")" "$NO_THREADS"

run_case non_bot_comment_ignored 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node alice "<!-- codex-pull-request-review-summary --> looks wrong to me")")" "$NO_THREADS"

run_case summary_with_resolved_thread 0 "$CLEAN_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both)")")" \
  "$(page reviewThreads "$(thread_node "$BOT" true)")"

run_case pr99_full_payload 0 "Found 0 unminimized regular comment(s) from bot" \
  "$(cat "${FIXTURES}/pr99-comments-page.json")" "$NO_THREADS"

CASE_PR=89 run_case summary_finding_thread_resolved 0 "$CLEAN_MSG" \
  "$(finding_card)" "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

CASE_PR=89 run_case summary_finding_title_with_brackets_resolved 0 "$CLEAN_MSG" \
  "$(finding_card 's/\[Example advisory finding\]/[Check args[0] and \\[escaped\\] bounds]/')" \
  "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

CASE_PR=89 run_case summary_finding_title_with_brackets_unresolved 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card 's/\[Example advisory finding\]/[Check args[0] and \\[escaped\\] bounds]/')" \
  "$(page reviewThreads "$(thread_node "$BOT" false "$FINDING_ID")")"

CASE_PR=89 run_case summary_two_findings_threads_resolved 0 "$CLEAN_MSG" \
  "$(finding_card "$TWO_FINDINGS_SED")" \
  "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")" "$(thread_node "$BOT" true $((FINDING_ID + 1)))")"

# --- Findings and lookalikes (must stay blocking) --------------------------

run_case real_finding_comment 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "**P1** The reconciler ignores the returned error.")")" "$NO_THREADS"

run_case quoted_marker_in_finding 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "> <!-- codex-pull-request-review-summary -->"$'\n\n'"The helper mis-parses this marker.")")" "$NO_THREADS"

run_case marker_alone 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "<!-- codex-pull-request-review-summary -->")")" "$NO_THREADS"

run_case marker_then_finding 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "<!-- codex-pull-request-review-summary -->"$'\n\n'"**P0** Secrets are logged in plaintext.")")" "$NO_THREADS"

run_case marker_with_indent 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" " $(body summary-completed-both)")")" "$NO_THREADS"

run_case marker_on_second_line 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" $'\n'"$(body summary-completed-both)")")" "$NO_THREADS"

run_case summary_crlf_line_endings 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both | sed 's/$/\r/')")")" "$NO_THREADS"

run_case marker_lookalike_spacing 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-pr-opened | sed '1s/ -->$/-->/')")")" "$NO_THREADS"

run_case summary_appended_finding 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both)"$'\n\n'"**P1** Missing bounds check.")")" "$NO_THREADS"

run_case summary_finding_inside_details 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both | sed 's|^</details>$|**P1** Missing bounds check.\n</details>|')")")" "$NO_THREADS"

run_case summary_unknown_status_row 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both | sed 's/\*\*Completed\*\*/**Failed**/')")")" "$NO_THREADS"

run_case summary_missing_table_rows 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both | sed '/^| .* Review\*\* |/d')")")" "$NO_THREADS"

run_case metadata_marker_unclosed 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both | sed '2s/ -->$//')")")" "$NO_THREADS"

run_case metadata_marker_not_json 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both | sed '2s/^.*$/<!-- codex-security-review:v1 {not json} -->/')")")" "$NO_THREADS"

run_case security_review_with_finding 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body security-clean | sed 's/^Security review completed\. No security issues were found in this pull request\.$/Security review completed. 1 finding requires attention./')")")" "$NO_THREADS"

run_case security_clean_then_finding 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "$(body security-clean)"$'\n\n'"**P0** Token leaks via debug endpoint.")")" "$NO_THREADS"

run_case security_clean_sentence_only 1 "$BLOCK_MSG" \
  "$(page comments "$(comment_node "$BOT" "No security issues were found in this pull request.")")" "$NO_THREADS"

run_case summary_with_unresolved_thread 1 "Found 1 unresolved review thread(s) from bot" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both)")")" \
  "$(page reviewThreads "$(thread_node "$BOT" false)")"

# A card listing findings counts as a comment unless each linked thread is resolved.
CASE_PR=89 run_case summary_finding_thread_unresolved 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card)" "$(page reviewThreads "$(thread_node "$BOT" false "$FINDING_ID")")"

CASE_PR=89 run_case summary_finding_thread_missing 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card)" "$NO_THREADS"

CASE_PR=89 run_case summary_finding_other_thread_resolved 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card)" "$(page reviewThreads "$(thread_node "$BOT" true $((FINDING_ID - 1)))")"

CASE_PR=89 run_case summary_finding_thread_started_by_human 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card)" "$(page reviewThreads "$(thread_node alice true "$FINDING_ID")")"

CASE_PR=89 run_case summary_one_of_two_findings_unresolved 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card "$TWO_FINDINGS_SED")" "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

run_case summary_finding_links_other_pr 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card)" "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

CASE_PR=89 run_case summary_finding_links_other_repo 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card 's|github\.com/coder/coder-k8s/|github.com/coder/other/|')" \
  "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

CASE_PR=89 run_case summary_finding_count_mismatch 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card 's/^#### Advisory findings (1)$/#### Advisory findings (2)/')" \
  "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

CASE_PR=89 run_case summary_unknown_findings_kind 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card 's/^#### Advisory findings/#### Blocking findings/')" \
  "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

CASE_PR=89 run_case summary_unknown_findings_section 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card 's/^### Security findings$/### Code findings/')" \
  "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

CASE_PR=89 run_case summary_finding_with_extra_text 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(finding_card "/discussion_r${FINDING_ID}/a **P1** Missing bounds check.")" \
  "$(page reviewThreads "$(thread_node "$BOT" true "$FINDING_ID")")"

run_case summary_plus_finding_comment 1 "Found 1 unminimized regular comment(s) from bot" \
  "$(page comments "$(comment_node "$BOT" "$(body summary-completed-both)")" "$(comment_node "$BOT" "**P2** Unused parameter.")")" "$NO_THREADS"

# --- Fail-fast API behaviour ------------------------------------------------

run_case pr_not_found 1 "does not exist in coder/coder-k8s" \
  '{"data":{"repository":{"pullRequest":null}}}' "$NO_THREADS"

# The helper retries MAX_ATTEMPTS times, then `set -e` aborts on the failed
# command substitution; its own "failed after N attempts" text is captured by
# that substitution, so the stub's stderr is the observable retry evidence.
STUB_GH_GRAPHQL_FAIL=1 run_case graphql_failure 1 "stub gh: simulated GraphQL failure" \
  "$NO_THREADS" "$NO_THREADS"

echo ""
echo "check_codex_comments tests: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
