# Run the MCP server

This guide shows how to run the `coder-k8s` **MCP server** for local development and in-cluster access.

The MCP server runs in HTTP mode (`--app=mcp-http`).

## 1. Overview

The MCP server provides tools for inspecting Kubernetes resources managed by `coder-k8s`, including:

- `CoderControlPlane` resources
- `CoderWorkspace` resources
- `CoderTemplate` resources
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

The server exposes MCP tools for:

- Reading `CoderControlPlane` resources and status
- Listing `CoderWorkspace` and `CoderTemplate` resources
- Listing namespace events for troubleshooting
- Reading pod logs for debugging

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
