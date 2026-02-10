# Run the MCP server

This guide shows how to run the `coder-k8s` **MCP server** for local development and in-cluster access.

The MCP server supports two app modes:

- `mcp-stdio` for stdio-based MCP clients (for example, Mux).
- `mcp-http` for HTTP-based MCP clients.

## 1. Overview

The MCP server provides tools for inspecting Kubernetes resources managed by `coder-k8s`, including:

- `CoderControlPlane` resources
- `CoderWorkspace` resources
- `CoderTemplate` resources
- Namespace events
- Pod logs

## 2. Stdio mode (Mux / local development)

Run stdio mode locally from this repository:

```bash
GOFLAGS=-mod=vendor go run . --app=mcp-stdio
```

Run stdio mode in-cluster via `kubectl exec`:

```bash
kubectl exec -i -n coder-system deploy/coder-k8s-mcp -- /coder-k8s --app=mcp-stdio
```

Example Mux MCP configuration (`~/.mux/mcp.jsonc`):

```jsonc
{
  "mcpServers": {
    "coder-k8s": {
      "command": "kubectl",
      "args": [
        "exec",
        "-i",
        "-n",
        "coder-system",
        "deploy/coder-k8s-mcp",
        "--",
        "/coder-k8s",
        "--app=mcp-stdio"
      ]
    }
  }
}
```

## 3. HTTP mode (port-forward / remote clients)

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

## 4. Available tools

The server exposes MCP tools for:

- Reading `CoderControlPlane` resources and status
- Listing `CoderWorkspace` and `CoderTemplate` resources
- Listing namespace events for troubleshooting
- Reading pod logs for debugging

## 5. Health checks

The HTTP server exposes standard health endpoints:

- `/healthz`
- `/readyz`

Example checks:

```bash
curl -fsS http://127.0.0.1:8090/healthz
curl -fsS http://127.0.0.1:8090/readyz
```
