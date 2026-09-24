# coder-k8s

[![CI](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/badge/go-1.26.8%2B-00ADD8?logo=go)](./go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](./LICENSE)

**Run and manage [Coder](https://coder.com) with native Kubernetes APIs.**

> [!WARNING]
> Experimental alpha software from a hackathon. Do not use it in production or expose it to end users.

## What you get

One binary, three components:

| Component | Manages | API group |
| --- | --- | --- |
| **Operator** | `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy` (CRDs) | `coder.com/v1alpha1` |
| **Aggregated API server** | `CoderWorkspace`, `CoderTemplate` (backed by a live Coder instance) | `aggregation.coder.com/v1alpha1` |
| **MCP server** | Tools for inspecting and operating the above over HTTP | — |

Pick what runs with `--app`:

| `--app` | Runs |
| --- | --- |
| `all` (default) | Everything in one process |
| `controller` | Operator only |
| `aggregated-apiserver` | Aggregated API server only |
| `mcp-http` | MCP server only |

## Quick start

From a clone of this repo:

```bash
kubectl create namespace coder-system
kubectl apply -f config/crd/bases/ -f config/rbac/
kubectl apply -f deploy/
kubectl rollout status deployment/coder-k8s -n coder-system
```

Then create a Coder instance:

```bash
kubectl create namespace coder
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml
```

Full walkthrough: [Deploy a Coder Control Plane](https://coder.github.io/coder-k8s/tutorials/getting-started/).

## Documentation

📖 **<https://coder.github.io/coder-k8s/>** (source in [`docs/`](docs/); preview with `make docs-serve`)

- [Getting started](https://coder.github.io/coder-k8s/tutorials/getting-started/)
- [Deploy the aggregated API server](https://coder.github.io/coder-k8s/how-to/deploy-aggregated-apiserver/)
- [Run the MCP server](https://coder.github.io/coder-k8s/how-to/mcp-server/)
- [API reference](https://coder.github.io/coder-k8s/reference/api/codercontrolplane/)

## Examples

| Example | Shows |
| --- | --- |
| [`examples/cloudnativepg/`](examples/cloudnativepg/) | `CoderControlPlane` backed by CloudNativePG PostgreSQL |
| [`examples/argocd/`](examples/argocd/) | The whole stack from one Argo CD `ApplicationSet` |
| [`examples/coder-templates/`](examples/coder-templates/) | Reusable `CoderTemplate` manifests |

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md). Licensed under [Apache-2.0](./LICENSE).
