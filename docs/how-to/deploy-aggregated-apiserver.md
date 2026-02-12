# Deploy the aggregated API server (in-cluster)

This guide shows how to deploy the `coder-k8s` **aggregated API server** and register it with the Kubernetes API aggregation layer.

The aggregated API server serves:

- API group: `aggregation.coder.com`
- Version: `v1alpha1`
- Resources: `coderworkspaces`, `codertemplates`

## 1. Create the namespace

```bash
kubectl create namespace coder-system
```

## 2. Apply RBAC

Apply generated RBAC manifests (including the shared `coder-k8s` ServiceAccount and bindings):

```bash
kubectl apply -f config/rbac/
```

## 3. Deploy the service and deployment

```bash
kubectl apply -f deploy/apiserver-service.yaml
kubectl apply -f deploy/deployment.yaml
```

`deploy/deployment.yaml` defaults to `--app=all`, which runs the controller, aggregated API server, and MCP server in a single pod.

For split deployments, you can still run individual components by setting `--app=controller`, `--app=aggregated-apiserver`, or `--app=mcp-http` in the Deployment args.

## 4. Register the APIService

```bash
kubectl apply -f deploy/apiserver-apiservice.yaml
```

## 5. Verify

Wait for the deployment:

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
```

Check the APIService:

```bash
kubectl get apiservice v1alpha1.aggregation.coder.com
```

List resources served by the aggregated API server:

```bash
kubectl get coderworkspaces.aggregation.coder.com -A
kubectl get codertemplates.aggregation.coder.com -A
```

## TLS note

`deploy/apiserver-apiservice.yaml` currently sets `insecureSkipTLSVerify: true`, which is convenient for development but not appropriate for production.
