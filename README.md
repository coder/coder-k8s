# coder-k8s

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
- `config/rbac/` — Generated RBAC manifests
- `config/samples/` — Sample custom resources
- `hack/` — Code generation and maintenance scripts
