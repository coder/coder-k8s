# coder-k8s documentation

!!! warning
    **Highly experimental / alpha software**
    This project is an active prototype. Do not use it in production.

`coder-k8s` is a Kubernetes control-plane project for running and managing Coder through Kubernetes APIs.

## What is in this repository?

`coder-k8s` ships one binary with four app modes:

- **`all` (default)**: controller + aggregated API server + MCP server in one process.
- **`controller`**: reconciles `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy` (`coder.com/v1alpha1`).
- **`aggregated-apiserver`**: serves `CoderWorkspace` + `CoderTemplate` (`aggregation.coder.com/v1alpha1`).
- **`mcp-http`**: serves MCP tooling over HTTP.

## Documentation map (Diátaxis)

- **Tutorials**: guided learning path
  - [Getting started](tutorials/getting-started.md)
- **How-to guides**: task-focused operations
  - [Deploy controller](how-to/deploy-controller.md)
  - [Deploy aggregated API server](how-to/deploy-aggregated-apiserver.md)
  - [Run MCP server](how-to/mcp-server.md)
  - [Troubleshooting](how-to/troubleshooting.md)
- **Reference**: generated API reference
  - [API reference](reference/api/codercontrolplane.md)
- **Explanation**: conceptual internals
  - [Architecture](explanation/architecture.md)

## Quick commands

```bash
make manifests
make test
make build
make docs-serve
```

New here? Start with the [Getting started tutorial](tutorials/getting-started.md).
