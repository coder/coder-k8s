# Run the MCP server

This guide shows how to run the `coder-k8s` **MCP server** for local development and in-cluster access.

The MCP server runs in HTTP mode (`--app=mcp-http`).

## 1. Overview

The MCP server provides tools for inspecting and updating Kubernetes resources managed by `coder-k8s`, including:

- `CoderControlPlane` resources
- Control-plane Deployment, Service, and Pod status
- `CoderWorkspace` resources (including `spec.running` updates)
- `CoderTemplate` resources (including `spec.running` updates)
- Namespace events
- Pod logs

## 2. HTTP mode (port-forward / remote clients)

Apply RBAC, deployment, and service manifests:

```bash
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/mcp-deployment.yaml
kubectl apply -f deploy/mcp-service.yaml
```

Port-forward the MCP service:

```bash
kubectl port-forward svc/coder-k8s -n coder-system 8090:8090
```

Connect MCP clients to:

```text
http://127.0.0.1:8090/mcp
```

## 3. Available tools

The server exposes the following MCP tools:

- `list_control_planes`
- `get_control_plane_status`
- `list_control_plane_pods`
- `get_control_plane_deployment_status`
- `get_service_status`
- `list_workspaces`
- `get_workspace`
- `set_workspace_running`
- `list_templates`
- `get_template`
- `set_template_running`
- `get_events`
- `get_pod_logs`
- `check_health`

## 4. Health checks

<!-- cspell:ignore healthz readyz -->

The HTTP server exposes standard health endpoints:

- `/healthz`
- `/readyz`

Example checks:

```bash
curl -fsS http://127.0.0.1:8090/healthz
curl -fsS http://127.0.0.1:8090/readyz
```
