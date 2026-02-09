#!/usr/bin/env bash
set -euo pipefail

# Wait for a PR to become merge-ready by enforcing the Codex + CI loop.
# Usage: ./scripts/wait_pr_ready.sh <pr_number>
#
# This script orchestrates:
#   1) wait_pr_codex.sh  - waits for an explicit Codex response/approval
#   2) wait_pr_checks.sh - waits for required CI checks and mergeability
#
# It cannot auto-fix feedback; if either phase fails, address feedback, push,
# re-request review (`@codex review`), then run this script again.

if [ $# -ne 1 ]; then
  echo "Usage: $0 <pr_number>" >&2
  exit 1
fi

PR_NUMBER="$1"
if ! [[ "$PR_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "❌ PR number must be numeric. Got: '$PR_NUMBER'" >&2
  exit 1
fi

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
WAIT_CODEX_SCRIPT="$SCRIPT_DIR/wait_pr_codex.sh"
WAIT_CHECKS_SCRIPT="$SCRIPT_DIR/wait_pr_checks.sh"

for required in "$WAIT_CODEX_SCRIPT" "$WAIT_CHECKS_SCRIPT"; do
  if [ ! -x "$required" ]; then
    echo "❌ Required executable script is missing or not executable: $required" >&2
    exit 1
  fi
done

for required_cmd in gh jq git; do
  if ! command -v "$required_cmd" >/dev/null 2>&1; then
    echo "❌ Missing required command: $required_cmd" >&2
    exit 1
  fi
done

echo "🚦 Waiting for PR #$PR_NUMBER to become ready (Codex + CI)..."
echo ""

echo "Step 1/2: Waiting for Codex review on latest @codex review request..."
if ! "$WAIT_CODEX_SCRIPT" "$PR_NUMBER"; then
  echo ""
  echo "❌ Codex phase did not pass."
  echo "   Address feedback (or retry if Codex was rate-limited), push, and request review again:"
  echo ""
  echo "   gh pr comment $PR_NUMBER --body-file - <<'EOF'"
  echo "   @codex review"
  echo ""
  echo "   Please take another look."
  echo "   EOF"
  echo ""
  exit 1
fi

echo ""
echo "✅ Codex approved the latest review request."
echo ""
echo "Step 2/2: Waiting for required checks and mergeability..."
if ! "$WAIT_CHECKS_SCRIPT" "$PR_NUMBER"; then
  echo ""
  echo "❌ CI/mergeability phase did not pass."
  echo "   Fix issues locally, push, and rerun this script."
  exit 1
fi

echo ""
echo "🎉 PR #$PR_NUMBER is ready: Codex approved and required checks passed."
