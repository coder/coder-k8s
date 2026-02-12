# Deploy the controller (in-cluster)

This guide shows how to deploy the `coder-k8s` **controller** to a Kubernetes cluster using manifests from `config/` and `deploy/`.

## 1. Create the namespace

The deployment manifests expect a `coder-system` namespace:

```bash
kubectl create namespace coder-system
```

## 2. Install the CRDs

Install the `CoderControlPlane` CRD:

```bash
kubectl apply -f config/crd/bases/
```

## 3. Apply RBAC

```bash
kubectl apply -f config/rbac/
```

## 4. Deploy `coder-k8s`

```bash
kubectl apply -f deploy/deployment.yaml
```

`deploy/deployment.yaml` defaults to `--app=all`, which runs the controller, aggregated API server, and MCP server in a single pod.

For split deployments, you can still run individual components by setting `--app=controller`, `--app=aggregated-apiserver`, or `--app=mcp-http` in the Deployment args.

## 5. Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get pods -n coder-system
```

## Customizing the image

By default, `deploy/deployment.yaml` uses `ghcr.io/coder/coder-k8s:latest`. For a different image tag, edit the deployment manifest before applying it.
