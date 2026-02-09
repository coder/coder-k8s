You are an experienced, pragmatic software engineering AI agent. Do not over-engineer a solution when a simple one is possible. Keep edits minimal. If you want an exception to ANY rule, you MUST stop and get permission first.

## Project Overview

`coder-k8s` is a Go-based Kubernetes operator scaffold for managing a custom resource named `CoderControlPlane` (`coder.com/v1alpha1`). The current codebase focuses on baseline wiring: CRD types, scheme registration, controller startup, and a placeholder reconciliation loop.

**Tech stack**
- Go `1.25.6` (`go.mod`)
- Kubernetes libraries: `controller-runtime`, `client-go`, `apimachinery`, `code-generator`
- Vendored dependencies committed under `vendor/`
- Tooling: `make`, Bash scripts in `hack/`, GitHub Actions, GoReleaser, optional Nix dev shell (`flake.nix`)

## Reference

### Key files
- `main.go`: manager bootstrap, scheme registration, health/readiness endpoints, controller wiring.
- `main_test.go`: baseline tests for scheme registration and defensive nil-check behavior.
- `api/v1alpha1/codercontrolplane_types.go`: CRD spec/status and list types.
- `internal/controller/codercontrolplane_controller.go`: reconciler and `SetupWithManager` logic.
- `hack/update-codegen.sh`: deepcopy codegen entrypoint.
- `Makefile`: canonical build/test/vendor/codegen commands.
- `.github/workflows/ci.yaml`: CI checks, workflow linting, and `:main` container publish.

### Important directories
- `api/v1alpha1/`: API group/version types and generated deepcopy code.
- `internal/controller/`: reconciliation logic.
- `internal/deps/`: blank imports to keep baseline Kubernetes tool deps in `go.mod`/`vendor`.
- `hack/`: maintenance scripts.
- `.github/workflows/`: CI and release automation.
- `vendor/`: checked-in module dependencies (required by project workflow).

### Architecture notes
- `main` registers core Kubernetes + project schemes, constructs a controller-runtime manager, and starts it.
- Reconciliation is intentionally minimal: fetch resource, validate identity assumptions, then no-op with TODO markers.
- Defensive checks are intentional (`assertion failed: ...`) and used to fail fast during development.

## Essential Commands

Run from repository root.

- **Build:** `make build`
- **Format (apply):** `find . -type f -name '*.go' -not -path './vendor/*' -print0 | xargs -0 gofmt -w`
- **Format (check):** `find . -type f -name '*.go' -not -path './vendor/*' -print0 | xargs -0 gofmt -l`
- **Lint (workflows):** `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.10`
- **Test:** `make test`
- **Clean:** `go clean -cache -testcache && rm -f ./coder-k8s && rm -rf ./dist`
- **Development run:** `GOFLAGS=-mod=vendor go run .` (requires Kubernetes config via your env, e.g. `KUBECONFIG`)
- **Vendor consistency:** `make verify-vendor`
- **Code generation:** `make codegen` (or `bash ./hack/update-codegen.sh`)
- **Shell scripts:** `find . -type f -name '*.sh' -not -path './vendor/*'`

## Patterns

- **Do** preserve fail-fast assertions for impossible states (nil manager/client/scheme, mismatched fetched objects).
  **Don’t** silently ignore these paths or convert them to soft failures.
- **Do** keep vendoring in sync when dependencies change (`go mod tidy`, `go mod vendor`, then verify diff).
  **Don’t** submit dependency changes without updating `vendor/`.
- **Do** regenerate deepcopy code after API type changes (`make codegen`).
  **Don’t** hand-edit `api/v1alpha1/zz_generated.deepcopy.go`.
- **Do** keep controller and API changes paired with tests in `main_test.go` or focused package tests.
  **Don’t** add reconciliation behavior without coverage for critical assumptions.

## Anti-patterns

- Unpinned GitHub Action versions in workflow files (CI uses SHA-pinned actions).
- Running CI-sensitive commands without vendoring mode when behavior differs from CI.
- Removing assertion messages that start with `assertion failed:`; these are deliberate diagnostics.

## Code Style

- Follow idiomatic Go and keep code `gofmt`-formatted.
- Keep comments concise and purposeful (package docs, exported type/function docs).
- Match existing error style: contextual wrapping + explicit assertion messages for impossible conditions.

## Commit and Pull Request Guidelines

### Before committing
1. Run `make test`.
2. Run `make build`.
3. Run `make verify-vendor`.
4. If API types changed, run `make codegen` and include generated updates.
5. If `.github/workflows/*` changed, run `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.10`.

### Commit messages
- Match repository history style: short imperative summary, optionally prefixed by type (e.g., `chore: ...`).
- Prefer `type: message` if unsure.
- Include issue/PR reference when available (examples in history use `(#N)`).

### Pull request descriptions
- Include: what changed, why, validation commands run, and any follow-up work.
- For public mux-generated PRs/commits in this environment, include the attribution footer defined in `.mux/skills/pull-requests/SKILL.md`.

## PR Workflow (Codex)

- Before creating or updating any PR, commit, or public issue, read `.mux/skills/pull-requests/SKILL.md` and follow it.
- Use `./scripts/wait_pr_ready.sh <pr_number>` for a one-command wait flow after requesting review.
- Prefer `gh` CLI for GitHub interactions over manual web/curl flows.

When a PR exists, stay in this loop until ready:
1. Push your latest fixes.
2. Run local validation (`make verify-vendor`, `make test`, `make build`).
3. Request review with `@codex review`.
4. Run `./scripts/wait_pr_codex.sh <pr_number>` and wait for Codex.
5. If Codex leaves comments, address them, resolve threads with `./scripts/resolve_pr_comment.sh <thread_id>`, push, and repeat.
6. After explicit Codex approval, run `./scripts/wait_pr_checks.sh <pr_number>`.

Only stop the loop early if the reviewer is clearly misunderstanding the intended change and further churn would be counterproductive. In that case, leave a clarifying PR comment and wait for human direction.
