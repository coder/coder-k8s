# coder-k8s

Run and manage [Coder](https://coder.com) with native Kubernetes APIs.

!!! warning "Alpha software"
    This project is an experimental prototype. Do not use it in production.

## What it does

`coder-k8s` is one binary with three components. Choose which ones run with `--app`:

| `--app` | Runs | Resources |
| --- | --- | --- |
| `all` (default) | Operator and aggregated API server in one process (not the MCP server) | Everything below except MCP |
| `controller` | Operator | `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy` (`coder.com/v1alpha1`) |
| `aggregated-apiserver` | Aggregated API server | `CoderWorkspace`, `CoderTemplate` (`aggregation.coder.com/v1alpha1`) |
| `mcp-http` | MCP server | Operational tools over HTTP |

See [Architecture](explanation/architecture.md) for how the pieces fit.

## Where to start

| I want to… | Read |
| --- | --- |
| Get a Coder instance running | [Deploy a Coder Control Plane](tutorials/getting-started.md) |
| Add external provisioners | [Deploy an External Provisioner](tutorials/deploy-coderprovisioner.md) |
| Deploy with GitOps | [Deploy with Argo CD](tutorials/deploy-with-argocd.md) |
| Run only the operator | [Deploy the controller](how-to/deploy-controller.md) |
| Connect an external PostgreSQL database | [Connect an external PostgreSQL database](how-to/deploy-controller.md#connect-an-external-postgresql-database) |
| Manage workspaces and templates with `kubectl` | [Deploy the aggregated API server](how-to/deploy-aggregated-apiserver.md) |
| Connect an AI agent or other MCP client | [Run the MCP server](how-to/mcp-server.md) |
| Fix a problem | [Troubleshooting](how-to/troubleshooting.md) |
| Look up a field | [API reference](reference/api/codercontrolplane.md) |
