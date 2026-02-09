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

# Filter regular comments from bot that aren't minimized, excluding:
# - "Didn't find any major issues" (no issues found)
# - "usage limits have been reached" (rate limit error, not a real review)
REGULAR_COMMENTS=$(echo "$ALL_COMMENTS" | jq "[.[] | select(.author.login == \"${BOT_LOGIN_GRAPHQL}\" and .isMinimized == false and (.body | test(\"Didn't find any major issues|usage limits have been reached\") | not))]")
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
