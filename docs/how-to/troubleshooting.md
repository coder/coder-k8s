# Troubleshooting

Start with the operator logs; most problems show up there:

```bash
kubectl logs -n coder-system deploy/coder-k8s
```

## A component I didn't expect is running

`--app` is optional and defaults to `all` (every component). To isolate one component, set it explicitly:

```bash
GOFLAGS=-mod=vendor go run . --app=controller   # or aggregated-apiserver, mcp-http
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
