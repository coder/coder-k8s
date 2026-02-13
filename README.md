# coder-k8s

[![CI](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go)](./go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](./LICENSE)

> [!WARNING]
> **Highly Experimental / Alpha Software**
> This repository is a **hackathon contribution** and remains a **highly experimental, alpha-stage** project.
> **Do not use this in production or expose it to end users.**

`coder-k8s` is a Kubernetes control-plane project for managing Coder-related resources with native Kubernetes APIs.

## What this project provides

- A controller-runtime operator for `coder.com/v1alpha1` resources:
  - `CoderControlPlane`
  - `CoderProvisioner`
  - `CoderWorkspaceProxy`
- An aggregated API server for `aggregation.coder.com/v1alpha1` resources:
  - `CoderWorkspace`
  - `CoderTemplate`
- An MCP HTTP server for operational tooling.
- A single binary that can run in all-in-one mode or split app modes.

## Application modes

| Mode | Description | Typical usage |
| --- | --- | --- |
| `all` (default) | Runs controller + aggregated API + MCP HTTP together | Local dev, demos, simple cluster deployment |
| `controller` | Runs only Kubernetes reconcilers | Controller-focused debugging and e2e smoke flows |
| `aggregated-apiserver` | Runs only aggregated API server | Split deployments with explicit Coder backend flags |
| `mcp-http` | Runs only MCP HTTP server | MCP-focused integrations |

## Prerequisites

- Go 1.25+ (`go.mod` currently declares Go 1.25.7)
- A Kubernetes cluster (OrbStack, KIND, or any conformant cluster)
- `kubectl` configured for your target cluster

## Quick start (local controller run)

```bash
# Generate and apply CRDs
make manifests
kubectl apply -f config/crd/bases/

# Run controller locally against your kubeconfig context
GOFLAGS=-mod=vendor go run . --app=controller

# In another terminal, apply a sample control plane
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml

# Verify resource creation
kubectl get codercontrolplanes -A
```

## Documentation

The project docs follow Diátaxis and live in `docs/`.

- Home: [`docs/index.md`](docs/index.md)
- Tutorial: [`docs/tutorials/getting-started.md`](docs/tutorials/getting-started.md)
- How-to guides: [`docs/how-to/`](docs/how-to/)
- Architecture: [`docs/explanation/architecture.md`](docs/explanation/architecture.md)
- API reference: [`docs/reference/api/`](docs/reference/api/)

Serve docs locally:

```bash
make docs-serve
```

## Examples

- [`examples/cloudnativepg/`](examples/cloudnativepg/) — Deploy a `CoderControlPlane` with a CloudNativePG-managed PostgreSQL backend.

- [`examples/aggregated/`](examples/aggregated/) - Reusable `CoderTemplate` manifests for aggregated API testing.

## KIND development cluster (k9s demos)

Bootstrap a KIND cluster and install CRDs/RBAC (**this switches current kubectl context**):

```bash
make kind-dev-up
```

Useful helpers:

```bash
make kind-dev-status
make kind-dev-ctx
make kind-dev-load-image
make kind-dev-k9s
make kind-dev-down
```

## Essential commands

| Command | Description |
| --- | --- |
| `make build` | Build all packages |
| `make test` | Run unit + integration tests |
| `make test-integration` | Run focused controller integration tests |
| `make manifests` | Generate CRD and RBAC manifests |
| `make codegen` | Run deepcopy generation |
| `make docs-reference` | Regenerate API reference docs from Go types |
| `make docs-check` | Build docs in strict mode (CI-equivalent) |
| `make verify-vendor` | Verify vendored dependency consistency |
| `make lint` | Run linter + formatting checks |
| `make vuln` | Run vulnerability scan |

## Repository layout

- `api/v1alpha1/` — CRD API types (`coder.com/v1alpha1`)
- `api/aggregation/v1alpha1/` — aggregated API types (`aggregation.coder.com/v1alpha1`)
- `internal/app/` — app mode entrypoints (`allapp`, `controllerapp`, `apiserverapp`, `mcpapp`)
- `internal/controller/` — controller reconcilers
- `internal/aggregated/` — aggregated storage + Coder client/provider logic
- `config/` — generated CRDs, RBAC, samples
- `deploy/` — deployable manifests for all-in-one, APIService, and MCP service
- `docs/` — user-facing documentation site content
- `hack/` — code generation and maintenance scripts

## Contributing

Before opening a PR, run at least:

```bash
make verify-vendor
make test
make build
make lint
make docs-check
```

## License

Apache-2.0. See [LICENSE](./LICENSE).
