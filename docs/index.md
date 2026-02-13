# coder-k8s documentation

!!! warning
    **Highly experimental / alpha software**
    This project is an active prototype. Do not use it in production.

`coder-k8s` is a Kubernetes control-plane stack for running and managing Coder through Kubernetes APIs.

## What you can deploy

`coder-k8s` ships one binary with four app modes:

- **`all` (default)**: operator + workspace/template API server + MCP server in one process.
- **`controller`**: reconciles `CoderControlPlane`, `CoderProvisioner`, and `CoderWorkspaceProxy` (`coder.com/v1alpha1`).
- **`aggregated-apiserver`**: serves `CoderWorkspace` + `CoderTemplate` (`aggregation.coder.com/v1alpha1`).
- **`mcp-http`**: serves operational MCP tooling over HTTP.

## Quick start path

If you want to deploy quickly and validate the end-to-end flow:

1. Follow [Deploy a Coder Control Plane](tutorials/getting-started.md).
2. Verify the managed Coder Deployment and Service.
3. If you need external provisioners, continue with [Deploy an External Provisioner](tutorials/deploy-coderprovisioner.md).

## Learn more

- [Deploy an External Provisioner](tutorials/deploy-coderprovisioner.md)
- [Deploy controller](how-to/deploy-controller.md)
- [Deploy aggregated API server](how-to/deploy-aggregated-apiserver.md)
- [Run MCP server](how-to/mcp-server.md)
- [Troubleshooting](how-to/troubleshooting.md)
- [API reference](reference/api/codercontrolplane.md)
- [Architecture](explanation/architecture.md)

Start with [Deploy a Coder Control Plane](tutorials/getting-started.md).
