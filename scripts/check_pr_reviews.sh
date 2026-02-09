#!/usr/bin/env bash
# Check for unresolved PR review comments.
# Usage: ./scripts/check_pr_reviews.sh <pr_number>
# Exits 0 if all resolved, 1 if unresolved comments exist.

set -euo pipefail

if [ $# -eq 0 ]; then
  echo "Usage: $0 <pr_number>"
  exit 1
fi

PR_NUMBER="$1"
if ! [[ "$PR_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "❌ PR number must be numeric. Got: '$PR_NUMBER'"
  exit 1
fi

REPO_INFO=$(gh repo view --json owner,name --jq '{owner: .owner.login, name: .name}')
OWNER=$(echo "$REPO_INFO" | jq -r '.owner')
REPO=$(echo "$REPO_INFO" | jq -r '.name')

# shellcheck disable=SC2016 # Single quotes are intentional - this is a GraphQL query.
GRAPHQL_QUERY='query($owner: String!, $repo: String!, $pr: Int!, $cursor: String) {
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
              author { login }
              body
              diffHunk
              commit { oid }
            }
          }
        }
      }
    }
  }
}'

THREAD_CURSOR=""
ALL_THREADS='[]'

while true; do
  if [ -n "$THREAD_CURSOR" ]; then
    RESULT=$(gh api graphql \
      -f query="$GRAPHQL_QUERY" \
      -F owner="$OWNER" \
      -F repo="$REPO" \
      -F pr="$PR_NUMBER" \
      -F cursor="$THREAD_CURSOR")
  else
    RESULT=$(gh api graphql \
      -f query="$GRAPHQL_QUERY" \
      -F owner="$OWNER" \
      -F repo="$REPO" \
      -F pr="$PR_NUMBER")
  fi

  if [ "$(echo "$RESULT" | jq -r '.data.repository.pullRequest == null')" = "true" ]; then
    echo "❌ PR #$PR_NUMBER was not found in ${OWNER}/${REPO}."
    exit 1
  fi

  PAGE_THREADS=$(echo "$RESULT" | jq '.data.repository.pullRequest.reviewThreads.nodes')
  ALL_THREADS=$(jq -cn --argjson all "$ALL_THREADS" --argjson page "$PAGE_THREADS" '$all + $page')

  HAS_NEXT=$(echo "$RESULT" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage')
  if [ "$HAS_NEXT" != "true" ]; then
    break
  fi

  THREAD_CURSOR=$(echo "$RESULT" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.endCursor')
  if [ -z "$THREAD_CURSOR" ] || [ "$THREAD_CURSOR" = "null" ]; then
    echo "❌ Assertion failed: review thread cursor missing while hasNextPage=true"
    exit 1
  fi
done

UNRESOLVED=$(echo "$ALL_THREADS" | jq -c '.[] | select(.isResolved == false) | {thread_id: .id, user: (.comments.nodes[0].author.login // "unknown"), body: (.comments.nodes[0].body // ""), diff_hunk: (.comments.nodes[0].diffHunk // ""), commit_id: (.comments.nodes[0].commit.oid // "")}')

if [ -n "$UNRESOLVED" ]; then
  echo "❌ Unresolved review comments found:"
  echo "$UNRESOLVED" | jq -r '"  \(.user): \(.body)"'
  echo ""
  echo "To resolve a comment thread, use:"
  echo "$UNRESOLVED" | jq -r '"  ./scripts/resolve_pr_comment.sh \(.thread_id)"'
  echo ""
  echo "View PR: https://github.com/${OWNER}/${REPO}/pull/$PR_NUMBER"
  exit 1
fi

echo "✅ All review comments resolved"
exit 0
