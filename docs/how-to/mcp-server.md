# Run the MCP server

The MCP (Model Context Protocol) server gives tools to MCP clients, for example AI agents. The clients use these tools over HTTP to inspect and operate `coder-k8s` resources.

The MCP server is off by default, and `--app=all` does not include it. It runs only with `--app=mcp-http`, and only with a bearer token file.

!!! danger "What the token gives"
    The MCP server does all tool calls with the permissions of its own Kubernetes ServiceAccount. In the default install, this is the ServiceAccount of the operator. The token is a shared administrative credential for all the tools. It does not identify callers, and there is no RBAC for each caller. Anyone who has the token can use all the tools.

The server listens on `127.0.0.1:8090` in the Pod only. To connect to it, a client needs the token and a path to the loopback interface of the Pod. An example of such a path is `kubectl port-forward`, which Kubernetes authorizes with `pods/portforward` on that Pod. Other containers in the same Pod can also connect to it. Do not add a proxy that makes it available on the Pod network.

## 1. Create the token

The token file must contain one token of 32 bytes or more, without whitespace. The server ignores a trailing newline.

The server reads the token file one time, at startup. To rotate the token, update the Secret. Then restart the pod, for example with `kubectl -n coder-system rollout restart deployment/coder-k8s`. Until the restart, the old token continues to work.

```bash
openssl rand -hex 32 > mcp-token
chmod 600 mcp-token
kubectl -n coder-system create secret generic coder-k8s-mcp-token --from-file=token=mcp-token
```

## 2. Run the MCP server next to the operator

Run these commands from a clone of this repository. Deploy the operator. Then add an `mcp` container that runs `--app=mcp-http`. The JSON patch adds the new container at the end, so the operator stays the first container. Thus other patches that use `containers/0` still change the operator container:

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

If `--mcp-token-file` is missing, cannot be read, is empty, is too short, or contains whitespace, the `mcp` container stops at startup. This does not affect the operator container.

## 3. Connect

```bash
kubectl -n coder-system port-forward deploy/coder-k8s 8090:8090
```

Set the URL of your MCP client to `http://127.0.0.1:8090/mcp`. Send `Authorization: Bearer <token>` with each request. A request without the correct token gets `401 Unauthorized`. This is also true for a request that has an existing `Mcp-Session-Id`.

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
