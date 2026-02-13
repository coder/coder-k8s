# Troubleshooting

## I expected `--app` to be required, but behavior is different

`coder-k8s` defaults to `--app=all` when you do not pass `--app`.

If you want to isolate components for debugging, set one explicit mode:

```bash
GOFLAGS=-mod=vendor go run . --app=controller
GOFLAGS=-mod=vendor go run . --app=aggregated-apiserver
GOFLAGS=-mod=vendor go run . --app=mcp-http
```

If you pass an unsupported value, startup fails with an `assertion failed: unsupported --app value ...` error.

## `no matches for kind` when applying `CoderControlPlane`

Install (or reinstall) CRDs:

```bash
make manifests
kubectl apply -f config/crd/bases/
kubectl get crd | grep coder.com
```

## Controller deployment is running, but reconciliation does not happen

Use the resource names from the shipped manifests:

- Deployment: `coder-k8s`
- ClusterRole: `manager-role`
- ClusterRoleBinding: `coder-k8s`

Check status and logs:

```bash
kubectl get deploy -n coder-system coder-k8s
kubectl logs -n coder-system deploy/coder-k8s
kubectl get clusterrole manager-role
kubectl get clusterrolebinding coder-k8s
```

Then inspect a specific control plane object:

```bash
kubectl get codercontrolplane -A
kubectl describe codercontrolplane <name> -n <namespace>
```

## `CoderControlPlane` stays `Pending`

Typical causes:

1. Control-plane Deployment has no ready pods.
2. Operator bootstrap token is not ready yet.
3. Optional license Secret is missing or invalid when `spec.licenseSecretRef` is set.

Debug commands:

```bash
kubectl get codercontrolplane -n <namespace> <name> -o yaml
kubectl get deploy,svc -n <namespace>
kubectl logs -n coder-system deploy/coder-k8s
```

## Aggregated APIService is `False` / `Unavailable`

Verify required resources:

```bash
kubectl get deploy -n coder-system coder-k8s
kubectl get svc -n coder-system coder-k8s-apiserver
kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml
kubectl logs -n coder-system deploy/coder-k8s
```

If you use APIService aggregation demos, avoid installing conflicting CRDs for the same aggregated resources (`coderworkspaces.aggregation.coder.com`, `codertemplates.aggregation.coder.com`).

## Aggregated reads return `ServiceUnavailable`

In `all` mode, this usually means no eligible `CoderControlPlane` exists yet (or operator access is not ready).

In standalone `--app=aggregated-apiserver` mode, ensure all three are configured:

- `--coder-url`
- `--coder-session-token`
- `--coder-namespace`

Check logs for provider configuration messages:

```bash
kubectl logs -n coder-system deploy/coder-k8s
```

## Aggregated reads return `multiple eligible CoderControlPlane ...`

Current dynamic provider behavior expects a single eligible control plane per request scope.

If you have multiple ready control planes, narrow scope:

- Query a single namespace (`-n <namespace>`), and/or
- Run a dedicated aggregated API deployment pinned with `--coder-namespace`.
