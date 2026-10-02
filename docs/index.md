# coder-k8s

Use native Kubernetes APIs to run and manage [Coder](https://coder.com).

!!! warning "Alpha software"
    This project is an experimental prototype. Do not use it in production.

## What it does

`coder-k8s` is one binary with three components:

| Component | Resources |
| --- | --- |
| Operator | `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy` (`coder.com/v1alpha1`) |
| Aggregated API server | `CoderWorkspace`, `CoderTemplate`, `CoderTemplateVersion` (`aggregation.coder.com/v1alpha1`) |
| MCP server | Operational tools over HTTP |

The `--app` flag selects the components that run. The default, `--app=all`, runs the operator and the aggregated API server, but not the MCP server. For all values, and for how the components work together, see [Architecture](explanation/architecture.md).

## Where to start

| I want to… | Read |
| --- | --- |
| Get a Coder instance running | [Deploy a Coder Control Plane](tutorials/getting-started.md) |
| Add external provisioners | [Deploy an External Provisioner](tutorials/deploy-coderprovisioner.md) |
| Deploy with GitOps | [Deploy with Argo CD](tutorials/deploy-with-argocd.md) |
| Run only the operator | [Deploy the controller](how-to/deploy-controller.md) |
| Connect an external PostgreSQL database | [Connect an external PostgreSQL database](how-to/deploy-controller.md#connect-an-external-postgresql-database) |
| Manage workspaces and templates with `kubectl` | [Deploy the aggregated API server](how-to/deploy-aggregated-apiserver.md) |
| Connect an AI agent or another MCP client | [Run the MCP server](how-to/mcp-server.md) |
| Fix a problem | [Troubleshooting](how-to/troubleshooting.md) |
| Find a field | [API reference](reference/api/codercontrolplane.md) |
