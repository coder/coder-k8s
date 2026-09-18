# Run the MCP server

This guide shows how to run the `coder-k8s` **MCP server** for local development and in-cluster access.

`deploy/deployment.yaml` defaults to `--app=all`, which runs the controller, aggregated API server, and MCP server in a single pod. For split deployments, set `--app=mcp-http` (or `--app=controller` / `--app=aggregated-apiserver`) in the Deployment args.

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
kubectl apply -f config/rbac/
kubectl apply -f deploy/deployment.yaml
kubectl apply -f deploy/mcp-service.yaml
```

The RBAC manifests create the shared `coder-k8s` ServiceAccount and bindings used by the Deployment.

Port-forward the MCP service:

```bash
kubectl port-forward svc/coder-k8s -n coder-system 8090:8090
```

Connect MCP clients to:

```text
http://127.0.0.1:8090/mcp
```

### Access and transport limits

The MCP HTTP endpoint has **no application authentication**. Any client that can reach it can use the registered tools with the server's Kubernetes permissions. Kubernetes RBAC limits the server's permissions; it does not authenticate MCP callers. Keep this alpha service on trusted networks. Do not expose port 8090 to untrusted clients. Remote access needs a separate authentication boundary and network restrictions.

MCP Go SDK 1.4.1 adds transport protections, not authentication:

- Requests received on a loopback address must use a loopback `Host`, such as `localhost:8090` or `127.0.0.1:8090`. A reverse proxy that connects over loopback but preserves a service or external hostname will receive `403 Forbidden`.
- The loopback check uses the address seen by the MCP server. It does not provide a general hostname allowlist for Pod or Service traffic. Do not assume every port-forward or proxy path presents a loopback address to the server.
- Cross-site POST requests identified by `Origin` or `Sec-Fetch-Site` are rejected. Direct clients without these headers remain allowed.
- POST requests must use exactly `Content-Type: application/json`. Missing headers, browser form content types, and `application/json; charset=utf-8` receive `415 Unsupported Media Type`.
- JSON field names are case-sensitive. A key with an appended null character does not alias the original key.

Use the loopback URL above for local clients. For in-cluster clients, the service is `coder-k8s.coder-system.svc` on port 8090. Service names and client-supplied headers are not credentials. Do not disable the SDK's localhost or cross-origin protections to work around routing problems.

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
