#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CODER_DIR="$SCRIPT_ROOT/tmpfork/coder"
SKILL_DIR="$SCRIPT_ROOT/.mux/skills/coder-docs"
DEST_DOCS_DIR="$SKILL_DIR/references/docs"

# Dependency checks (fail fast).
command -v git >/dev/null 2>&1 || { echo "assertion failed: git not found in PATH" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "assertion failed: go not found in PATH" >&2; exit 1; }

# Assertion: SKILL.md template must exist before we can inject into it.
if [[ ! -f "$SKILL_DIR/SKILL.md" ]]; then
    echo "assertion failed: $SKILL_DIR/SKILL.md not found; create the skill skeleton first" >&2
    exit 1
fi

# Clone or update coder/coder into tmpfork/.
if [[ ! -d "$CODER_DIR/.git" ]]; then
    echo "Cloning coder/coder into $CODER_DIR..."
    git clone --depth=1 https://github.com/coder/coder "$CODER_DIR"
else
    echo "Updating existing clone at $CODER_DIR..."
    git -C "$CODER_DIR" fetch origin main --depth=1
    git -C "$CODER_DIR" reset --hard origin/main
fi

CODER_SHA="$(git -C "$CODER_DIR" rev-parse HEAD)"

# Assertion: upstream docs directory must contain manifest.json.
if [[ ! -f "$CODER_DIR/docs/manifest.json" ]]; then
    echo "assertion failed: $CODER_DIR/docs/manifest.json not found" >&2
    exit 1
fi

echo "Generating coder-docs skill from coder/coder@${CODER_SHA:0:12}..."

cd "$SCRIPT_ROOT"
GOFLAGS=-mod=vendor go run ./hack/gen-coder-docs-skill \
    --source-docs-root "$CODER_DIR/docs" \
    --dest-docs-root "$DEST_DOCS_DIR" \
    --skill-md "$SKILL_DIR/SKILL.md" \
    --coder-sha "$CODER_SHA" \
    --snapshot-out "$DEST_DOCS_DIR/SNAPSHOT.json"

echo "Done. Snapshot generated from coder/coder@${CODER_SHA:0:12}"
