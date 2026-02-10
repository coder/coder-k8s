# Architecture

`coder-k8s` builds a single binary (`coder-k8s`) that can run in one of two modes:

- `--app=controller`
- `--app=aggregated-apiserver`

The dispatch logic lives in `app_dispatch.go`. The `--app` flag is required, and the code intentionally fails fast with an `assertion failed:` error when it is missing or invalid.

## Controller mode

In controller mode, the binary runs a `controller-runtime` manager and registers the `CoderControlPlane` API types:

- API group: `coder.com/v1alpha1`
- Kind: `CoderControlPlane`

Key code paths:

- `internal/app/controllerapp/` — scheme construction and manager startup
- `internal/controller/` — reconciliation logic (`CoderControlPlaneReconciler`)

## Aggregated API server mode

In aggregated API server mode, the binary starts an aggregated API server that installs storage for:

- API group: `aggregation.coder.com/v1alpha1`
- Resources: `coderworkspaces`, `codertemplates`

Key code paths:

- `internal/app/apiserverapp/` — API server bootstrap and API group installation
- `internal/aggregated/storage/` — storage implementations (currently hardcoded in-memory objects)

## Manifests and generated assets

- `config/` — generated CRDs and RBAC (via `make manifests`)
- `deploy/` — example deployment manifests for controller and aggregated API server
- `vendor/` — vendored dependencies (required by the repo workflow)
