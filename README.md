# coder-k8s

[![CI](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/badge/go-1.26.8%2B-00ADD8?logo=go)](./go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](./LICENSE)

Use native Kubernetes APIs to run and manage [Coder](https://coder.com).

> [!WARNING]
> This is experimental alpha software from a hackathon. Do not use it in production. Do not make it available to end users.

## What you get

`coder-k8s` is one binary with three components:

| Component | Manages | API group |
| --- | --- | --- |
| Operator | `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy` (custom resources) | `coder.com/v1alpha1` |
| Aggregated API server | `CoderWorkspace`, `CoderTemplate`, `CoderTemplateVersion` (served from a live Coder instance) | `aggregation.coder.com/v1alpha1` |
| MCP server | Tools that inspect and operate these resources over HTTP | — |

The operator is a Kubernetes controller. It creates and updates the Coder control planes, provisioners, and workspace proxies that these custom resources describe. The aggregated API server adds Coder workspaces and templates to the Kubernetes API, so you can use `kubectl` with them. The MCP (Model Context Protocol) server gives tools to AI agents and other MCP clients.

The `--app` flag selects the components that run. The default, `--app=all`, runs the operator and the aggregated API server in one process. The MCP server runs only with `--app=mcp-http`. For all values, see [Architecture](https://coder.github.io/coder-k8s/explanation/architecture/).

## Quick start

Run these commands from a clone of this repository:

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

For all the steps, see [Deploy a Coder Control Plane](https://coder.github.io/coder-k8s/tutorials/getting-started/).

## Documentation

The documentation is at <https://coder.github.io/coder-k8s/>. The source is in [`docs/`](docs/). To show a local preview, run `make docs-serve`.

- [Getting started](https://coder.github.io/coder-k8s/tutorials/getting-started/)
- [Deploy the aggregated API server](https://coder.github.io/coder-k8s/how-to/deploy-aggregated-apiserver/)
- [Run the MCP server](https://coder.github.io/coder-k8s/how-to/mcp-server/)
- [API reference](https://coder.github.io/coder-k8s/reference/api/codercontrolplane/)

## Examples

| Example | Shows |
| --- | --- |
| [`examples/cloudnativepg/`](examples/cloudnativepg/) | A `CoderControlPlane` that uses a CloudNativePG PostgreSQL database |
| [`examples/argocd/`](examples/argocd/) | The full stack from one Argo CD `ApplicationSet` |
| [`examples/coder-templates/`](examples/coder-templates/) | `CoderTemplate` manifests that you can use again |

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md). The license is [Apache-2.0](./LICENSE).
