# Deploy the controller (in-cluster)

This guide shows how to deploy the `coder-k8s` **controller** to a Kubernetes cluster using the manifests in `deploy/`.

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
kubectl apply -f deploy/rbac.yaml
```

## 4. Deploy the controller

```bash
kubectl apply -f deploy/controller-deployment.yaml
```

## 5. Verify

```bash
kubectl rollout status deployment/coder-k8s-controller -n coder-system
kubectl get pods -n coder-system
```

## Customizing the image

By default, `deploy/controller-deployment.yaml` uses `ghcr.io/coder/coder-k8s:latest`. For a different image tag, edit the deployment manifest before applying it.
