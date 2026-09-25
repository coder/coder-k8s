# Troubleshooting

Start with the operator logs; most problems show up there:

```bash
kubectl logs -n coder-system deploy/coder-k8s
```

## A component I didn't expect is running

`--app` is optional and defaults to `all` (controller and aggregated API server; the MCP server never runs in `all`). To isolate one component, set it explicitly:

```bash
GOFLAGS=-mod=vendor go run . --app=controller   # or aggregated-apiserver, or mcp-http --mcp-token-file=<file>
```

An unknown value fails at startup with `assertion failed: unsupported --app value ...`.

## `no matches for kind` when applying a `CoderControlPlane`

The CRDs are missing. Install them:

```bash
kubectl apply -f config/crd/bases/
kubectl get crd | grep coder.com
```

## The controller runs but nothing reconciles

Check that the shipped Deployment and RBAC exist:

```bash
kubectl get deploy coder-k8s -n coder-system
kubectl get clusterrole manager-role
kubectl get clusterrolebinding coder-k8s
```

Then inspect the object's events and status:

```bash
kubectl describe codercontrolplane <name> -n <namespace>
```

## `CoderControlPlane` stays `Pending`

Common causes:

1. The control plane Deployment has no ready pods.
2. The operator bootstrap token is not ready yet.
3. `spec.licenseSecretRef` points to a missing or invalid license Secret.
4. `spec.database.connectionSecretRef` cannot be resolved. Check the `DatabaseSecretResolved` condition reason; see [Connect an external PostgreSQL database](deploy-controller.md#connect-an-external-postgresql-database).

```bash
kubectl get codercontrolplane <name> -n <namespace> -o yaml
kubectl get deploy,svc -n <namespace>
```

## APIService is `False` / `Unavailable`

```bash
kubectl get svc coder-k8s-apiserver -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml
```

Do not install CRDs for the same resources (`coderworkspaces.aggregation.coder.com`, `codertemplates.aggregation.coder.com`); they conflict with the aggregated API.

## The pod exits with `configure delegated authentication`

The aggregated API server checks every caller with the Kubernetes API and refuses to start without it.

- `... configmaps "extension-apiserver-authentication" is forbidden`: apply `config/rbac/authentication-reader-binding.yaml`. It must name the ServiceAccount the pod runs as (for example after installing into another namespace).
- `no Kubernetes configuration for delegated authentication and authorization` (outside a cluster): set `KUBECONFIG` to one kubeconfig file, or create `~/.kube/config`.
- `load kubeconfig ...` or `invalid kubeconfig ...`: the file named by `KUBECONFIG` is missing or incomplete. The server does not fall back to another configuration.

## Aggregated requests fail with `401 Unauthorized` or `403 Forbidden`

- **`401`:** the request has no valid credential. Requests sent straight to port `6443` need a Kubernetes bearer token; anonymous requests only reach `/healthz`, `/livez`, and `/readyz`. Use `kubectl`, which goes through kube-apiserver.
- **`403`:** the caller lacks RBAC for `aggregation.coder.com` in that namespace. Check with `kubectl auth can-i list codertemplates.aggregation.coder.com -n <namespace> --as=<user>`. Before you grant it, note that this RBAC is owner-equivalent inside Coder (see [How callers are checked](deploy-aggregated-apiserver.md#how-callers-are-checked)).
- **`500` mentioning `subjectaccessreviews`:** the server's ServiceAccount cannot create SubjectAccessReviews. Apply `config/rbac/auth-delegator-binding.yaml`.

## Aggregated reads return `ServiceUnavailable`

- **`all` mode:** no eligible `CoderControlPlane` exists yet, or its operator access is not ready.
- **Standalone mode (`--app=aggregated-apiserver`):** set all three flags: `--coder-url`, `--coder-session-token`, and `--coder-namespace`.

The logs show which provider configuration was used.

## Aggregated reads return `multiple eligible CoderControlPlane ...`

The server expects one eligible control plane per request scope. With several ready control planes, either:

- Query one namespace (`-n <namespace>`), or
- Run a dedicated aggregated API server pinned with `--coder-namespace`.

## Aggregated requests return `400` or `409`

These often come from the naming or `resourceVersion` rules. See [Aggregated API behavior](../reference/aggregated-api-behavior.md).
