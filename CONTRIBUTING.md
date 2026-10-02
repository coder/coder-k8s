# Contributing to coder-k8s

Thank you for your contribution to `coder-k8s`.

> [!NOTE]
> The root [`README.md`](./README.md) is for end users. This guide tells you how to develop locally and how to contribute.

## Development prerequisites

- Go 1.26.8 or later (`go.mod` declares Go 1.26.8)
- A Kubernetes cluster (OrbStack, KIND, or another conformant cluster)
- `kubectl`, configured for your target cluster

## Local development quick start (controller mode)

```bash
# Generate and apply CRDs
make manifests
kubectl apply -f config/crd/bases/

# Run controller locally against your kubeconfig context
GOFLAGS=-mod=vendor go run . --app=controller

# In another terminal, create the sample namespace and apply a sample control plane
kubectl create namespace coder
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml

# Verify resource creation
kubectl get codercontrolplanes -A
```

## KIND development cluster (k9s demos)

This command creates a KIND cluster and installs the CRDs and RBAC. It also changes your current `kubectl` context.

```bash
make kind-dev-up
```

Other helper commands:

```bash
make kind-dev-status
make kind-dev-ctx
make kind-dev-load-image
make kind-dev-k9s
make kind-dev-down
```

## Essential development commands

| Command | Description |
| --- | --- |
| `make build` | Build all packages |
| `make test` | Run the unit and integration tests |
| `make test-integration` | Run only the tests in `internal/controller/` (envtest) |
| `make manifests` | Generate the CRD and RBAC manifests |
| `make codegen` | Generate the deepcopy code |
| `make docs-reference` | Generate the API reference docs again from the Go types |
| `make docs-check` | Build the docs in strict mode, as CI does |
| `make verify-vendor` | Make sure that the vendored dependencies are consistent |
| `make lint` | Run the linter and the formatting checks |
| `make vuln` | Run the vulnerability scan |

## Before opening a PR

Run these commands at a minimum:

```bash
make verify-vendor
make test
make build
make lint
make docs-check
```
