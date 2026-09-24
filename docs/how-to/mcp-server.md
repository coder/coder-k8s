# Run the MCP server

The MCP server gives MCP clients (such as AI agents) tools to inspect and operate `coder-k8s` resources over HTTP.

It runs by default: `--app=all` in `deploy/deployment.yaml` includes it. To run it alone, use `--app=mcp-http`.

!!! danger "No authentication"
    Any client that can reach the endpoint can call every tool with the server's Kubernetes permissions. RBAC limits what the server can do; it does not identify callers. Keep it on trusted networks. Never expose port `8090` to untrusted clients. Remote access needs its own authentication layer and network restrictions.

## 1. Deploy and connect

From a clone of this repository:

```bash
kubectl apply -f config/rbac/ -f deploy/deployment.yaml -f deploy/mcp-service.yaml
kubectl port-forward svc/coder-k8s -n coder-system 8090:8090
```

Point your MCP client at:

```text
http://127.0.0.1:8090/mcp
```

In-cluster clients use `coder-k8s.coder-system.svc` on port `8090`.

## 2. Check health

```bash
curl -fsS http://127.0.0.1:8090/healthz
curl -fsS http://127.0.0.1:8090/readyz
```

## Tools

| Area | Tools |
| --- | --- |
| Control planes | `list_control_planes`, `get_control_plane_status`, `list_control_plane_pods`, `get_control_plane_deployment_status`, `get_service_status` |
| Workspaces | `list_workspaces`, `get_workspace`, `set_workspace_running` |
| Templates | `list_templates`, `get_template`, `set_template_running` |
| Diagnostics | `get_events`, `get_pod_logs`, `check_health` |

## Request rules

The MCP Go SDK (1.4.1) enforces these transport checks. They are protections, not authentication, and they can cause surprising errors:

| Rule | Error when broken |
| --- | --- |
| Requests arriving on a loopback address must use a loopback `Host` (`localhost:8090`, `127.0.0.1:8090`). A reverse proxy that connects over loopback but keeps a Service or external hostname fails. | `403 Forbidden` |
| POST requests must have exactly `Content-Type: application/json`. A missing header, form types, and `application/json; charset=utf-8` all fail. | `415 Unsupported Media Type` |
| Cross-site POST requests (detected via `Origin` or `Sec-Fetch-Site`) are rejected. Clients that send neither header are allowed. | Rejected |

Also note:

- The loopback check uses the address the server sees. It is not a hostname allowlist for Pod or Service traffic, and not every port-forward or proxy path arrives from loopback.
- JSON field names are case-sensitive. A key with an appended null character does not alias the original key.
- Service names and client-supplied headers are not credentials.
- Do not disable the SDK's localhost or cross-origin protections to work around routing problems.
