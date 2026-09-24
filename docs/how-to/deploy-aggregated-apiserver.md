# Deploy the aggregated API server

Serve `CoderWorkspace` and `CoderTemplate` (`aggregation.coder.com/v1alpha1`) through the Kubernetes API, so you can manage Coder workspaces and templates with `kubectl`.

Commands run from a clone of this repository.

## 1. Apply RBAC and register the API

```bash
kubectl create namespace coder-system
kubectl apply -f config/rbac/
kubectl apply -f deploy/apiserver-service.yaml -f deploy/apiserver-apiservice.yaml
```

## 2. Deploy

### Option A: all-in-one (recommended)

The default `--app=all` already includes the aggregated API server. It finds its Coder backend automatically from an eligible `CoderControlPlane`.

```bash
kubectl apply -f deploy/deployment.yaml
```

### Option B: standalone

Run only the aggregated API server (`--app=aggregated-apiserver`) and point it at a Coder instance yourself.

Deploy, then set the backend:

```bash
kubectl apply -f deploy/deployment.yaml

kubectl -n coder-system patch deployment coder-k8s --type=json -p '[{
  "op": "add",
  "path": "/spec/template/spec/containers/0/args",
  "value": [
    "--app=aggregated-apiserver",
    "--coder-url=https://coder.example.com",
    "--coder-session-token=replace-me",
    "--coder-namespace=coder-system"
  ]
}]'
```

Standalone mode serves health checks over HTTPS on port `6443`. Update the probes:

```bash
kubectl -n coder-system patch deployment coder-k8s --type=strategic -p '{
  "spec": {"template": {"spec": {"containers": [{
    "name": "coder-k8s",
    "livenessProbe":  {"httpGet": {"scheme": "HTTPS", "path": "/healthz", "port": 6443}},
    "readinessProbe": {"httpGet": {"scheme": "HTTPS", "path": "/readyz",  "port": 6443}}
  }]}}}
}'
```

## 3. Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A
```

If something fails, check `kubectl logs -n coder-system deploy/coder-k8s` and [Troubleshooting](troubleshooting.md).

## Next

These resources are backed by Coder, not etcd, so some Kubernetes behavior differs. Read [Aggregated API behavior](../reference/aggregated-api-behavior.md) before you write manifests. The most important rule: object names must use Coder's canonical names.

!!! warning "TLS"
    `deploy/apiserver-apiservice.yaml` sets `insecureSkipTLSVerify: true` for development. Use CA-backed TLS in any real environment.
