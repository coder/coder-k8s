# Contributing to coder-k8s

Thanks for contributing to `coder-k8s`.

> [!NOTE]
> The root [`README.md`](./README.md) is end-user focused. This guide contains local development and contribution workflows.

## Development prerequisites

- Go 1.25+ (`go.mod` currently declares Go 1.25.7)
- A Kubernetes cluster (OrbStack, KIND, or any conformant cluster)
- `kubectl` configured for your target cluster

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

## Essential development commands

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

## Before opening a PR

Run at least:

```bash
make verify-vendor
make test
make build
make lint
make docs-check
```
