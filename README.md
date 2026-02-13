# coder-k8s

[![CI](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/coder/coder-k8s/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go)](./go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](./LICENSE)

> [!WARNING]
> **Highly Experimental / Alpha Software**
> This repository is a **hackathon contribution** and remains a **highly experimental, alpha-stage** project.
> **Do not use this in production or expose it to end users.**

`coder-k8s` is a Kubernetes control-plane project for managing Coder-related resources with native Kubernetes APIs.

## Documentation

Start with the published docs: **https://coder.github.io/coder-k8s/**

Helpful entry points:

- Getting started: <https://coder.github.io/coder-k8s/tutorials/getting-started/>
- Deploy the controller: <https://coder.github.io/coder-k8s/how-to/deploy-controller/>
- Deploy the workspace/template API server: <https://coder.github.io/coder-k8s/how-to/deploy-aggregated-apiserver/>
- API reference: <https://coder.github.io/coder-k8s/reference/api/codercontrolplane/>

Prefer reading docs in-repo? See [`docs/`](docs/) and run `make docs-serve`.

## What this project provides

- A controller-runtime operator for `coder.com/v1alpha1` resources:
  - `CoderControlPlane`
  - `CoderProvisioner`
  - `CoderWorkspaceProxy`
- A workspace/template API server for `aggregation.coder.com/v1alpha1` resources:
  - `CoderWorkspace`
  - `CoderTemplate`
- An MCP HTTP server for operational tooling.
- A single binary that can run in all-in-one mode or split app modes.

## Application modes

| Mode | Description | Typical usage |
| --- | --- | --- |
| `all` (default) | Runs controller + workspace/template API + MCP HTTP together | Single deployment for evaluation environments |
| `controller` | Runs only Kubernetes reconcilers | Controller-only deployments |
| `aggregated-apiserver` | Runs only the workspace/template API server | Split deployments with dedicated API serving |
| `mcp-http` | Runs only MCP HTTP server | MCP-focused integrations |

## Quick start (brief, in-cluster)

For a full setup guide, use the docs links above. For a quick smoke deploy:

```bash
kubectl create namespace coder-system
kubectl apply -f config/crd/bases/
kubectl apply -f config/rbac/
kubectl apply -f deploy/deployment.yaml
kubectl apply -f deploy/apiserver-service.yaml
kubectl apply -f deploy/apiserver-apiservice.yaml
kubectl apply -f deploy/mcp-service.yaml
kubectl rollout status deployment/coder-k8s -n coder-system
```

## Examples

- [`examples/cloudnativepg/`](examples/cloudnativepg/) — Deploy a `CoderControlPlane` with a CloudNativePG-managed PostgreSQL backend.
- [`examples/argocd/`](examples/argocd/) — Bootstrap CloudNativePG + `coder-k8s` + PostgreSQL + `CoderControlPlane` from one Argo CD `ApplicationSet`.
- [`examples/coder-templates/`](examples/coder-templates/) — Reusable `CoderTemplate` manifests for workspace/template API testing.

## Contributing

For local development workflows, validation commands, and PR guidance, see [CONTRIBUTING.md](./CONTRIBUTING.md).

## License

Apache-2.0. See [LICENSE](./LICENSE).
