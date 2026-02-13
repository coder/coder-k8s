# Deploy the aggregated API server (in-cluster)

This guide deploys the aggregated API server for:

- API group: `aggregation.coder.com`
- Version: `v1alpha1`
- Resources: `coderworkspaces`, `codertemplates`

## 1) Create namespace and RBAC

```bash
kubectl create namespace coder-system
kubectl apply -f config/rbac/
```

## 2) Apply service and APIService manifests

```bash
kubectl apply -f deploy/apiserver-service.yaml
kubectl apply -f deploy/apiserver-apiservice.yaml
```

## 3) Choose a deployment model

### Option A: all-in-one mode (recommended for most dev/test setups)

`deploy/deployment.yaml` defaults to `--app=all`, which includes the aggregated API server.

```bash
kubectl apply -f deploy/deployment.yaml
```

In this mode, backend Coder client configuration is discovered dynamically from eligible `CoderControlPlane` resources.

### Option B: standalone aggregated API mode (`--app=aggregated-apiserver`)

Use this when you want a split deployment and explicit backend configuration.

1. Apply deployment manifest:

```bash
kubectl apply -f deploy/deployment.yaml
```

1. Configure required args:

```bash
CODER_URL="https://coder.example.com"
CODER_SESSION_TOKEN="replace-me"
CODER_NAMESPACE="coder-system"

kubectl -n coder-system set args deployment/coder-k8s --containers=coder-k8s -- \
  --app=aggregated-apiserver \
  --coder-url="${CODER_URL}" \
  --coder-session-token="${CODER_SESSION_TOKEN}" \
  --coder-namespace="${CODER_NAMESPACE}"
```

1. Update probes to HTTPS on port `6443` for standalone mode:

```bash
kubectl -n coder-system patch deployment coder-k8s --type='merge' -p '{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "coder-k8s",
            "livenessProbe": {
              "httpGet": {"scheme": "HTTPS", "path": "/healthz", "port": 6443}
            },
            "readinessProbe": {
              "httpGet": {"scheme": "HTTPS", "path": "/readyz", "port": 6443}
            }
          }
        ]
      }
    }
  }
}'
```

## 4) Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com
kubectl get coderworkspaces.aggregation.coder.com -A
kubectl get codertemplates.aggregation.coder.com -A
kubectl logs -n coder-system deploy/coder-k8s
```

## TLS note

`deploy/apiserver-apiservice.yaml` uses `insecureSkipTLSVerify: true` for development convenience.
Use proper CA-backed TLS wiring for production environments.
