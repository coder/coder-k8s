You are an experienced, pragmatic software engineering AI agent. Do not over-engineer a solution when a simple one is possible. Keep edits minimal. If you want an exception to ANY rule, you MUST stop and get permission first.

## Project Overview

`coder-k8s` is a Go-based Kubernetes control-plane project with two app modes: a controller-runtime operator for `CoderControlPlane` (`coder.com/v1alpha1`) and an aggregated API server for `CoderWorkspace`/`CoderTemplate` (`aggregation.coder.com/v1alpha1`).

**Tech stack**
- Go `1.25.7` (`go.mod`)
- Kubernetes libraries: `controller-runtime`, `client-go`, `apimachinery`, `apiserver`, `code-generator`
- Vendored dependencies committed under `vendor/`
- Tooling: `make`, `golangci-lint`/`gofumpt`, Bash scripts in `hack/` and `scripts/`, GitHub Actions, GoReleaser, optional Nix dev shell (`flake.nix`)

## Reference

### Key files
- `main.go`: process entrypoint; initializes logging and exits on application failure.
- `app_dispatch.go`: `--app` mode dispatch between `controller` and `aggregated-apiserver`.
- `main_test.go`: dispatch and defensive nil-check coverage.
- `internal/app/controllerapp/controllerapp.go`: controller mode bootstrap (scheme, manager, health/readiness checks).
- `internal/app/apiserverapp/apiserverapp.go`: aggregated API server bootstrap and API group installation.
- `api/v1alpha1/codercontrolplane_types.go`: CRD spec/status and list types for `CoderControlPlane`.
- `api/aggregation/v1alpha1/types.go`: aggregated API types for `CoderWorkspace` and `CoderTemplate`.
- `internal/controller/codercontrolplane_controller.go`: reconciler and `SetupWithManager` logic.
- `internal/aggregated/storage/workspace.go` + `template.go`: hardcoded in-memory aggregated API storage.
- `hack/update-manifests.sh`: CRD/RBAC generation entrypoint.
- `hack/update-codegen.sh`: deepcopy codegen entrypoint.
- `Makefile`: canonical build/test/lint/vendor/codegen/manifests commands.
- `.golangci.yml`: lint and formatting rules (including `gofumpt`).
- `.github/workflows/ci.yaml` and `.github/workflows/release.yaml`: CI and release pipelines.
- `.goreleaser.yaml` and `Dockerfile.goreleaser`: release packaging and container build configuration.

### Important directories
- `api/v1alpha1/`: CRD API group/version types and generated deepcopy code.
- `api/aggregation/v1alpha1/`: aggregated API group/version types and generated deepcopy code.
- `internal/app/`: application-mode bootstrap packages (`controllerapp`, `apiserverapp`).
- `internal/controller/`: controller reconciliation logic and envtest coverage.
- `internal/aggregated/`: aggregated API server storage implementation.
- `internal/deps/`: blank imports to keep Kubernetes tool deps pinned in `go.mod`/`vendor`.
- `config/`: generated CRDs, RBAC, and sample manifests.
- `deploy/`: deployment manifests for controller and aggregated API server components.
- `hack/`: maintenance scripts (codegen/manifests).
- `scripts/`: PR workflow automation and review/check helpers.
- `.github/workflows/`: CI and release automation.
- `vendor/`: checked-in module dependencies (required by project workflow).

### Architecture notes
- `main` delegates to `run(...)`, which requires `--app=<controller|aggregated-apiserver>`.
- `controller` mode registers core Kubernetes + `coder.com/v1alpha1` schemes, starts the controller-runtime manager, and wires health/readiness probes.
- `aggregated-apiserver` mode builds a generic API server for `aggregation.coder.com/v1alpha1` and installs `coderworkspaces`/`codertemplates` storage.
- Defensive checks are intentional (`assertion failed: ...`) and used to fail fast during development.

## Essential Commands

Run from repository root.

- **Build:** `make build`
- **Test:** `make test`
- **Integration tests (controller envtest):** `make test-integration`
- **Lint + format checks:** `make lint`
- **Format (apply):** `GOFLAGS=-mod=vendor golangci-lint fmt`
- **Format (check):** `GOFLAGS=-mod=vendor golangci-lint fmt --diff`
- **Vulnerability scan:** `make vuln`
- **Lint (workflows):** `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.10`
- **Development run (controller mode):** `GOFLAGS=-mod=vendor go run . --app=controller` (requires Kubernetes config via your env, e.g. `KUBECONFIG`)
- **Development run (aggregated API mode):** `GOFLAGS=-mod=vendor go run . --app=aggregated-apiserver`
- **Vendor consistency:** `make verify-vendor`
- **Manifest generation:** `make manifests` (or `bash ./hack/update-manifests.sh`)
- **Code generation:** `make codegen` (or `bash ./hack/update-codegen.sh`)
- **Docs (serve):** `make docs-serve`
- **Docs (strict build):** `make docs-check`
- **Clean:** `go clean -cache -testcache && rm -f ./coder-k8s && rm -rf ./dist`
- **Shell scripts:** `find . -type f -name '*.sh' -not -path './vendor/*'`

## Mux Tooling Helpers

- `.mux/tool_env` is sourced before every `bash` tool call (Mux docs: `/hooks/tools`).
- Use `run_and_report <step_name> <command...>` for multi-step validation in one bash invocation.
- The helper writes full logs to `/tmp/mux-<workspace>-<step>.log`, prints pass/fail markers, and tails failures.
- Example:
  - `run_and_report verify-vendor make verify-vendor`
  - `run_and_report test make test`

## Patterns

- **Do** preserve fail-fast assertions for impossible states (nil manager/client/scheme, mismatched fetched objects).
  **Don’t** silently ignore these paths or convert them to soft failures.
- **Do** keep vendoring in sync when dependencies change (`go mod tidy`, `go mod vendor`, then verify diff).
  **Don’t** submit dependency changes without updating `vendor/`.
  **Don’t** manually delete or edit `vendor/modules.txt`; refresh vendoring via `go mod tidy && go mod vendor` (or `make vendor`) instead.
- **Do** regenerate generated artifacts after API changes (`make codegen`, `make manifests`).
  **Don’t** hand-edit generated files like `zz_generated.deepcopy.go` or CRD/RBAC manifests.
- **Do** keep controller, aggregated API server, and storage changes paired with focused tests (`main_test.go`, `internal/controller/*_test.go`, and package tests under `internal/app/`/`internal/aggregated/`).
  **Don’t** add behavior without coverage for critical assumptions.

- **Do** update the docs in `docs/` when you change user-facing behavior (APIs, flags, manifests, deployment).
  **Don’t** let docs drift from the implementation.

## Anti-patterns

- Unpinned GitHub Action versions in workflow files (CI uses SHA-pinned actions).
- Running CI-sensitive commands without vendoring mode when behavior differs from CI.
- Removing assertion messages that start with `assertion failed:`; these are deliberate diagnostics.

## Code Style

- Follow idiomatic Go and the [Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md) as a baseline; project-specific rules in this file take precedence.
- Keep code `gofumpt`-formatted (enforced via `golangci-lint fmt`).
- Keep comments concise and purposeful (package docs, exported type/function docs).
- Match existing error style: contextual wrapping + explicit assertion messages for impossible conditions.

## Commit and Pull Request Guidelines

### Before committing
1. Run `make test`.
2. Run `make build`.
3. Run `make verify-vendor`.
4. Run `make lint` (or explain why it was skipped).
5. If API types changed, run `make codegen` and `make manifests`, then include generated updates.
6. If `.github/workflows/*` changed, run `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.10`.
7. If your change affects user-facing behavior (APIs, flags, manifests, deployment), update the documentation in `docs/` and run `make docs-check`.


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

> PR readiness is mandatory. You MUST keep iterating until the PR is fully ready.
> A PR is fully ready only when: (1) Codex explicitly approves, (2) all Codex review threads are resolved, and (3) all required CI checks pass.
> You MUST NOT report success or stop the loop before these conditions are met.

When a PR exists, you MUST remain in this loop until the PR is fully ready:
1. Push your latest fixes.
2. Run local validation (`make verify-vendor`, `make test`, `make build`, `make lint`).
3. Request review with `@codex review`.
4. Run `./scripts/wait_pr_ready.sh <pr_number>` (it polls Codex + required checks concurrently and fails fast).
5. If Codex leaves comments, address them, resolve threads with `./scripts/resolve_pr_comment.sh <thread_id>`, push, and repeat.
6. If checks/mergeability fail, fix issues locally, push, and repeat.

The only early-stop exception is when the reviewer is clearly misunderstanding the intended change and further churn would be counterproductive. In that case, leave a clarifying PR comment and pause for human direction.
