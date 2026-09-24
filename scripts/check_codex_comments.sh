#!/usr/bin/env bash
set -euo pipefail

if [ $# -eq 0 ]; then
  echo "Usage: $0 <pr_number>"
  exit 1
fi

PR_NUMBER=$1
BOT_LOGIN_GRAPHQL="chatgpt-codex-connector"

if ! [[ "$PR_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "❌ PR number must be numeric. Got: '$PR_NUMBER'"
  exit 1
fi

echo "Checking for unresolved Codex comments in PR #${PR_NUMBER}..."

REPO_INFO=$(gh repo view --json owner,name --jq '{owner: .owner.login, name: .name}')
OWNER=$(echo "$REPO_INFO" | jq -r '.owner')
REPO=$(echo "$REPO_INFO" | jq -r '.name')

# Depot runners sometimes hit transient network timeouts to api.github.com.
# Retry the GraphQL request a few times before failing the required check.
MAX_ATTEMPTS=5
BACKOFF_SECS=2

# shellcheck disable=SC2016 # Single quotes are intentional - these are GraphQL queries.
COMMENTS_QUERY='query($owner: String!, $repo: String!, $pr: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      comments(first: 100, after: $cursor) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          author { login }
          body
          createdAt
          isMinimized
        }
      }
    }
  }
}'

# shellcheck disable=SC2016 # Single quotes are intentional - these are GraphQL queries.
THREADS_QUERY='query($owner: String!, $repo: String!, $pr: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          isResolved
          comments(first: 1) {
            nodes {
              id
              fullDatabaseId
              author { login }
              body
              createdAt
              path
              line
            }
          }
        }
      }
    }
  }
}'

fetch_graphql_with_retry() {
  local query="$1"
  shift

  local attempt
  local backoff
  backoff="$BACKOFF_SECS"

  for ((attempt = 1; attempt <= MAX_ATTEMPTS; attempt++)); do
    if gh api graphql \
      -f query="$query" \
      -F owner="$OWNER" \
      -F repo="$REPO" \
      -F pr="$PR_NUMBER" \
      "$@"; then
      return 0
    fi

    if [ "$attempt" -eq "$MAX_ATTEMPTS" ]; then
      echo "❌ GraphQL query failed after ${MAX_ATTEMPTS} attempts"
      return 1
    fi

    echo "⚠️ GraphQL query failed (attempt ${attempt}/${MAX_ATTEMPTS}); retrying in ${backoff}s..."
    sleep "$backoff"
    backoff=$((backoff * 2))
  done
}

COMMENTS_CURSOR=""
ALL_COMMENTS='[]'

while true; do
  if [ -n "$COMMENTS_CURSOR" ]; then
    COMMENTS_RESULT=$(fetch_graphql_with_retry "$COMMENTS_QUERY" -F cursor="$COMMENTS_CURSOR")
  else
    COMMENTS_RESULT=$(fetch_graphql_with_retry "$COMMENTS_QUERY")
  fi

  if [ "$(echo "$COMMENTS_RESULT" | jq -r '.data.repository.pullRequest == null')" = "true" ]; then
    echo "❌ PR #${PR_NUMBER} does not exist in ${OWNER}/${REPO}."
    exit 1
  fi

  PAGE_COMMENTS=$(echo "$COMMENTS_RESULT" | jq '.data.repository.pullRequest.comments.nodes')
  ALL_COMMENTS=$(jq -cn --argjson all "$ALL_COMMENTS" --argjson page "$PAGE_COMMENTS" '$all + $page')

  HAS_NEXT=$(echo "$COMMENTS_RESULT" | jq -r '.data.repository.pullRequest.comments.pageInfo.hasNextPage')
  if [ "$HAS_NEXT" != "true" ]; then
    break
  fi

  COMMENTS_CURSOR=$(echo "$COMMENTS_RESULT" | jq -r '.data.repository.pullRequest.comments.pageInfo.endCursor')
  if [ -z "$COMMENTS_CURSOR" ] || [ "$COMMENTS_CURSOR" = "null" ]; then
    echo "❌ Assertion failed: comments pagination cursor missing while hasNextPage=true"
    exit 1
  fi
done

THREADS_CURSOR=""
ALL_THREADS='[]'

while true; do
  if [ -n "$THREADS_CURSOR" ]; then
    THREADS_RESULT=$(fetch_graphql_with_retry "$THREADS_QUERY" -F cursor="$THREADS_CURSOR")
  else
    THREADS_RESULT=$(fetch_graphql_with_retry "$THREADS_QUERY")
  fi

  if [ "$(echo "$THREADS_RESULT" | jq -r '.data.repository.pullRequest == null')" = "true" ]; then
    echo "❌ PR #${PR_NUMBER} does not exist in ${OWNER}/${REPO}."
    exit 1
  fi

  PAGE_THREADS=$(echo "$THREADS_RESULT" | jq '.data.repository.pullRequest.reviewThreads.nodes')
  ALL_THREADS=$(jq -cn --argjson all "$ALL_THREADS" --argjson page "$PAGE_THREADS" '$all + $page')

  HAS_NEXT=$(echo "$THREADS_RESULT" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage')
  if [ "$HAS_NEXT" != "true" ]; then
    break
  fi

  THREADS_CURSOR=$(echo "$THREADS_RESULT" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.endCursor')
  if [ -z "$THREADS_CURSOR" ] || [ "$THREADS_CURSOR" = "null" ]; then
    echo "❌ Assertion failed: review thread pagination cursor missing while hasNextPage=true"
    exit 1
  fi
done

# Filter regular comments from bot that aren't minimized, excluding the known
# shapes that carry no review finding:
# - "Didn't find any major issues" (no issues found)
# - "usage limits have been reached" (rate limit error, not a real review)
# - the "Codex Review Summary" status table the Codex app keeps editing in
#   place (marker on line 1, optional metadata marker, fixed header, one or
#   more Running/Completed rows, fixed "About Codex" footer). The card may also
#   carry one "### Security findings" section with one "#### Advisory findings
#   (N)" list of exactly N links to review threads on this PR; it only counts
#   as a non-finding when every linked thread is a resolved thread that the bot
#   started. An unresolved or missing thread keeps the card blocking.
# - the explicit clean security verdict ("No security issues were found")
# Recognition is structural and line-anchored: the summary and security shapes
# must match line for line, so a marker alone, a quoted marker, or a summary
# with any appended or inserted text stays blocking. Passing this filter means
# "no finding recorded here", not that a review approved the PR.
NON_FINDING_JQ=$(cat <<'EOF'
def body_lines: split("\n") | until(length == 0 or .[-1] != ""; .[:-1]);

def summary_marker: "<!-- codex-pull-request-review-summary -->";

def is_summary_metadata_marker:
  (capture("^<!-- codex-security-review:v1 (?<json>\\{.*\\}) -->$")
    | .json | try (fromjson | type == "object") catch false) // false;

def summary_head: [
  "## Codex Review Summary",
  "",
  "This comment shows the latest Codex review activity on this pull request.",
  "",
  "| Review | Status | Commit | Review trigger |",
  "| --- | --- | --- | --- |"
];

def summary_row_regex:
  "^\\| [^| ]{1,4} \\*\\*(Code|Security) Review\\*\\* \\| [^| ]{1,4} \\*\\*(Running\\*\\* since|Completed\\*\\*) "
  + "<relative-time datetime=\"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?Z\">"
  + "[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?Z</relative-time> "
  + "\\| `[0-9a-f]{7,40}` \\| [A-Za-z][A-Za-z ]{0,39} \\|$";

def summary_about: [
  "<details> <summary>ℹ️ About Codex in GitHub</summary>",
  "<br/>",
  "",
  "[Your team has set up Codex to review pull requests in this repo](https://chatgpt.com/codex/cloud/settings/general). Reviews are triggered when you",
  "- Open a pull request for review",
  "- Mark a draft as ready",
  "- Comment \"@codex review\" or \"@codex security review\".",
  "",
  "Codex reacts with 👀 while any review is running, comments if it has suggestions, and reacts with 👍 once all reviews finish with no findings.",
  "",
  "</details>"
];

# Count of leading elements satisfying f.
def leading_count(f): (map(f) | index(false)) // length;

def advisory_heading_regex: "^#### Advisory findings \\((?<n>[1-9][0-9]{0,2})\\)$";

# The link label may contain brackets (e.g. `args[0]` or escaped `\[`); only "](" ends it.
def finding_regex:
  "^- [^ ]{1,4} \\[(?:[^\\]]|\\](?!\\())+\\]\\(https://github\\.com/(?<owner>[^/()]+)/(?<repo>[^/()]+)/pull/(?<pr>[0-9]+)"
  + "#discussion_r(?<id>[0-9]+)\\) · \\*\\*(Critical|High|Medium|Low)\\*\\*$";

# True when the finding line links to a resolved, bot-started thread on this PR.
def is_resolved_finding:
  (capture(finding_regex) // null) as $m
  | $m != null
    and $m.owner == $owner and $m.repo == $repo and $m.pr == $pr
    and ($resolved | any(. == $m.id));

# Input: the card lines between the status table's blank lines and the About
# footer. Valid when empty, or when it is exactly one security findings section
# whose findings all link to resolved threads.
def findings_all_resolved:
  . as $s
  | if length == 0 then true
    elif length < 6
      or $s[0] != "### Security findings" or $s[1] != "" or $s[3] != "" or $s[-1] != ""
      or ($s[2] | test(advisory_heading_regex) | not)
    then false
    else
      ($s[2] | capture(advisory_heading_regex).n | tonumber) as $n
      | length == $n + 5 and ($s[4:4 + $n] | all(is_resolved_finding))
    end;

def is_codex_review_summary:
  body_lines as $l
  | (summary_about | length) as $about_len
  | ($l | length) > 8
    and $l[0] == summary_marker
    and ($l[1] == "" or ($l[1] | is_summary_metadata_marker))
    and $l[2:8] == summary_head
    and ($l[8:] as $rest
      | ($rest | leading_count(test(summary_row_regex))) as $rows
      | $rows >= 1
        and ($rest[$rows:] as $tail
          | ($tail | leading_count(. == "")) as $blanks
          | $blanks >= 1
            and ($tail | length) >= $blanks + $about_len
            and $tail[($tail | length) - $about_len:] == summary_about
            and ($tail[$blanks:($tail | length) - $about_len] | findings_all_resolved)));

def security_about: [
  "<details> <summary>ℹ️ About Codex security reviews in GitHub</summary>",
  "<br/>",
  "",
  "This is an experimental Codex feature. Security reviews are triggered when:",
  "- You comment \"@codex security review\"",
  "- A regular code review gets triggered (for example, \"@codex review\" or when a PR is opened), and you’re opted in so security review runs alongside code review",
  "",
  "Once complete, Codex will leave suggestions, or a comment if no findings are found.",
  "",
  "",
  "</details>"
];

def is_codex_security_clean:
  body_lines as $l
  | ($l | length) == 21
    and $l[0] == "### 🛡️ Codex Security Review"
    and $l[1] == ""
    and $l[2] == "Security review completed. No security issues were found in this pull request."
    and $l[3] == ""
    and ($l[4] | test("^\\*\\*Reviewed commit:\\*\\* `[0-9a-f]{7,40}`$"))
    and $l[5] == ""
    and ($l[6] | test("^\\[View security finding report\\]\\(https://chatgpt\\.com/codex/cloud/tasks/[A-Za-z0-9_-]+\\)$"))
    and $l[7] == ""
    and $l[8] == "_Only the user who started this review can view the report in Codex._"
    and $l[9] == ""
    and $l[10:] == security_about;

def is_known_non_finding:
  test("Didn't find any major issues|usage limits have been reached")
  or is_codex_review_summary
  or is_codex_security_clean;

[.[] | select(.author.login == $bot and .isMinimized == false and (.body | is_known_non_finding | not))]
EOF
)
# Review comment IDs (the discussion_r<ID> in finding links) of resolved threads
# the bot started, as decimal text. A summary card's findings must all appear
# here. fullDatabaseId is a BigInt that GitHub sends as a string; a number is
# accepted only while it is an exact integer (at most 2^53 - 1). A null,
# missing or malformed ID leaves its thread out, so a card linking it stays
# blocking.
RESOLVED_FINDING_IDS=$(echo "$ALL_THREADS" | jq -c --arg bot "$BOT_LOGIN_GRAPHQL" '
  [.[]
    | select(.isResolved == true and .comments.nodes[0].author.login == $bot)
    | .comments.nodes[0].fullDatabaseId
    | if type == "number" and . >= 1 and . <= 9007199254740991 and . == floor then tostring else . end
    | select(type == "string" and test("^[1-9][0-9]*$"))]')
REGULAR_COMMENTS=$(echo "$ALL_COMMENTS" | jq --arg bot "$BOT_LOGIN_GRAPHQL" \
  --arg owner "$OWNER" --arg repo "$REPO" --arg pr "$PR_NUMBER" \
  --argjson resolved "$RESOLVED_FINDING_IDS" "$NON_FINDING_JQ")
REGULAR_COUNT=$(echo "$REGULAR_COMMENTS" | jq 'length')

# Filter unresolved review threads from bot
UNRESOLVED_THREADS=$(echo "$ALL_THREADS" | jq "[.[] | select(.isResolved == false and .comments.nodes[0].author.login == \"${BOT_LOGIN_GRAPHQL}\")]")
UNRESOLVED_COUNT=$(echo "$UNRESOLVED_THREADS" | jq 'length')

TOTAL_UNRESOLVED=$((REGULAR_COUNT + UNRESOLVED_COUNT))

echo "Found ${REGULAR_COUNT} unminimized regular comment(s) from bot"
echo "Found ${UNRESOLVED_COUNT} unresolved review thread(s) from bot"

if [ "$TOTAL_UNRESOLVED" -gt 0 ]; then
  echo ""
  echo "❌ Found ${TOTAL_UNRESOLVED} unresolved comment(s) from Codex in PR #${PR_NUMBER}"
  echo ""
  echo "Codex comments:"

  if [ "$REGULAR_COUNT" -gt 0 ]; then
    echo "$REGULAR_COMMENTS" | jq -r '.[] | "  - [\(.createdAt)]\n\(.body)\n"'
  fi

  if [ "$UNRESOLVED_COUNT" -gt 0 ]; then
    echo "$UNRESOLVED_THREADS" | jq -r '.[] | "  - [\(.comments.nodes[0].createdAt)] thread=\(.id) \(.comments.nodes[0].path // "comment"):\(.comments.nodes[0].line // "")\n\(.comments.nodes[0].body)\n"'
    echo ""
    echo "Resolve review threads with: ./scripts/resolve_pr_comment.sh <thread_id>"
  fi

  echo ""
  echo "Please address or resolve all Codex comments before merging."
  exit 1
fi

echo "✅ No unresolved Codex comments found"
exit 0
