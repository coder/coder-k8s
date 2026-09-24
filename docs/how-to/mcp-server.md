# Run the MCP server

The MCP server gives MCP clients (such as AI agents) tools to inspect and operate `coder-k8s` resources over HTTP.

It is off by default. `--app=all` does not include it. It runs only with `--app=mcp-http`, and only with a bearer token file.

!!! danger "What the token grants"
    The MCP server calls every tool with its own Kubernetes ServiceAccount's permissions (in the default install, the operator's). The token is a shared administrative credential for those tool powers. It does not identify callers and there is no per-caller RBAC: anyone who holds the token can use every tool.

The server listens on `127.0.0.1:8090` inside the Pod only. To reach it, a client needs the token **and** a way into the Pod's loopback interface, such as `kubectl port-forward` (which Kubernetes authorizes with `pods/portforward` on that Pod). Other containers in the same Pod can reach it too. Do not add a proxy that exposes it on the Pod network.

## 1. Create the token

The token file must hold one token of at least 32 bytes without whitespace (a trailing newline is ignored).

The server reads the token file once, at startup. To rotate the token, update the Secret, then restart the pod (for example, `kubectl -n coder-system rollout restart deployment/coder-k8s`). Until the restart, the old token keeps working.

```bash
openssl rand -hex 32 > mcp-token
chmod 600 mcp-token
kubectl -n coder-system create secret generic coder-k8s-mcp-token --from-file=token=mcp-token
```

## 2. Run the MCP server next to the operator

From a clone of this repository, deploy the operator, then append an `mcp` container that runs `--app=mcp-http`. The JSON patch keeps the operator as the first container, so other patches that address `containers/0` still target it:

```bash
kubectl apply -f config/rbac/ -f deploy/deployment.yaml

kubectl -n coder-system patch deployment coder-k8s --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/volumes", "value": [
    {"name": "mcp-token", "secret": {"secretName": "coder-k8s-mcp-token"}}
  ]},
  {"op": "add", "path": "/spec/template/spec/containers/-", "value": {
    "name": "mcp",
    "image": "ghcr.io/coder/coder-k8s:latest",
    "args": ["--app=mcp-http", "--mcp-token-file=/etc/coder-k8s-mcp/token"],
    "volumeMounts": [{"name": "mcp-token", "mountPath": "/etc/coder-k8s-mcp", "readOnly": true}]
  }}
]'
```

Use the same image as the operator container.

The `mcp` container exits at startup if `--mcp-token-file` is missing, unreadable, empty, or too short. The operator container is not affected.

## 3. Connect

```bash
kubectl -n coder-system port-forward deploy/coder-k8s 8090:8090
```

Point your MCP client at `http://127.0.0.1:8090/mcp` and send `Authorization: Bearer <token>` on every request. Requests without the correct token get `401 Unauthorized`, including requests that carry an existing `Mcp-Session-Id`.

## 4. Check health

Only these two paths answer without the token:

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

The bearer token check runs first. After it, the MCP Go SDK (1.4.1) enforces these transport checks. They are protections, not authentication, and they can cause surprising errors:

| Rule | Error when broken |
| --- | --- |
| Requests arriving on a loopback address must use a loopback `Host` (`localhost:8090`, `127.0.0.1:8090`). A reverse proxy that connects over loopback but keeps a Service or external hostname fails. | `403 Forbidden` |
| POST requests must have exactly `Content-Type: application/json`. A missing header, form types, and `application/json; charset=utf-8` all fail. | `415 Unsupported Media Type` |
| Cross-site POST requests (detected via `Origin` or `Sec-Fetch-Site`) are rejected. Clients that send neither header are allowed. | Rejected |

Also note:

- JSON field names are case-sensitive. A key with an appended null character does not alias the original key.
- Service names, session IDs, and other client-supplied headers are not credentials. Only the bearer token is.
- Do not disable the SDK's localhost or cross-origin protections to work around routing problems.
