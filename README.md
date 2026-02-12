# coder-k8s

> [!WARNING]
> **Highly Experimental / Alpha Software**
> This repository is a **hackathon contribution** and remains a **highly experimental, alpha-stage** project.
> **Do not use this in production or expose it to end users.**
>
## Project description

`coder-k8s` is a Go-based Kubernetes control-plane project with two app modes:

- A `controller-runtime` operator for managing `CoderControlPlane` and
  `WorkspaceProxy` resources (`coder.com/v1alpha1`).
- An aggregated API server for `CoderWorkspace` and `CoderTemplate` resources
  (`aggregation.coder.com/v1alpha1`).

## Prerequisites

- Go 1.25+ (`go.mod` currently declares Go 1.25.7)
- A Kubernetes cluster (OrbStack is recommended for local development; any cluster works)
- `kubectl` configured to point at your cluster context

## Quick start / Local development (OrbStack)

```bash
# Generate CRD and RBAC manifests
make manifests

# Apply CRDs to your cluster
kubectl apply -f config/crd/bases/

# Run the controller locally (uses your kubeconfig context)
GOFLAGS=-mod=vendor go run . --app=controller

# In another terminal: apply the sample CR
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml

# Verify
kubectl get codercontrolplanes -A
```

## Examples

- [`examples/cloudnativepg/`](examples/cloudnativepg/) - Deploy a `CoderControlPlane` with a CloudNativePG-managed PostgreSQL backend.

## KIND development cluster (for k9s demos)

Bootstrap a KIND cluster and install CRDs/RBAC (**this also switches your current kubectl context**):

```bash
make kind-dev-up
```

> Tip: override defaults when needed:
>
> - Use `CLUSTER_NAME` to run multiple clusters in parallel.
> - Use `KIND_NODE_IMAGE` to pin the node image (default: `kindest/node:v1.34.0`).
>
> ```bash
> CLUSTER_NAME=my-cluster make kind-dev-up
> KIND_NODE_IMAGE=kindest/node:v1.32.0 make kind-dev-up
> ```

> If the cluster already exists and you change `KIND_NODE_IMAGE`, run `make kind-dev-down` first so the new image can be applied.
>
If you need to switch your kubectl context later:

```bash
make kind-dev-ctx
# or: CLUSTER_NAME=my-cluster make kind-dev-ctx
```

Start the controller (pick one):

- Out-of-cluster (fast iteration):

  ```bash
  GOFLAGS=-mod=vendor go run . --app=controller
  ```

- In-cluster (closer to CI):

  ```bash
  make kind-dev-load-image
  kubectl apply -f config/e2e/deployment.yaml
  kubectl -n coder-system wait --for=condition=Available deploy/coder-k8s --timeout=120s
  ```

Demo:

```bash
make kind-dev-k9s
```

Cleanup:

```bash
make kind-dev-down
```

Mux users: there is an optional agent skill (`kind-dev`) under `.mux/skills/` with agent-oriented instructions for running per-workspace KIND clusters.

## Essential commands

| Command | Description |
| --- | --- |
| `make build` | Build the project |
| `make test` | Run tests |
| `make manifests` | Generate CRD/RBAC manifests |
| `make codegen` | Run deepcopy code generation |
| `make verify-vendor` | Verify vendor consistency |
| `make lint` | Run linter (requires `golangci-lint`) |
| `make vuln` | Run vulnerability check (requires `govulncheck`) |
| `make docs-serve` | Serve the documentation site locally (requires `mkdocs`) |
| `make docs-check` | Build docs in strict mode (CI-equivalent) |

## Testing strategy

- **Unit tests**: `make test` runs all tests, including unit tests in `main_test.go`.
- **Integration tests**: Use `envtest` to exercise reconciliation against a lightweight API server (no real cluster needed). Run them via `make test` (included in the full suite) or `make test-integration` (focused on controller tests only).
- **E2E smoke tests**: Recommended CI smoke coverage uses a Kind-based flow that deploys the controller image and verifies pod health.

## Project structure

- `api/v1alpha1/` — CRD types and generated deepcopy code
- `internal/controller/` — Reconciliation logic
- `config/crd/bases/` — Generated CRD manifests
- `config/rbac/` — RBAC manifests (generated role + deployment bindings)
- `config/samples/` — Sample custom resources
- `hack/` — Code generation and maintenance scripts

## License

Apache-2.0. See [LICENSE](./LICENSE).
