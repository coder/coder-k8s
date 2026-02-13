# Deploy the controller (in-cluster)

This guide deploys `coder-k8s` in **controller-only mode** (`--app=controller`).

!!! note
    `deploy/deployment.yaml` defaults to `--app=all`. In this guide, we explicitly switch it to controller-only mode.

## 1) Create namespace

```bash
kubectl create namespace coder-system
```

## 2) Install CRDs

`config/crd/bases/` includes CRDs for:

- `CoderControlPlane`
- `CoderProvisioner`
- `CoderWorkspaceProxy`

Apply them:

```bash
kubectl apply -f config/crd/bases/
```

## 3) Apply RBAC

```bash
kubectl apply -f config/rbac/
```

## 4) Deploy and force controller-only mode

```bash
kubectl apply -f deploy/deployment.yaml
kubectl -n coder-system set args deployment/coder-k8s --containers=coder-k8s -- --app=controller
```

## 5) Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get pods -n coder-system
kubectl logs -n coder-system deploy/coder-k8s
```

Optional smoke check:

```bash
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml
kubectl get codercontrolplanes -A
```

## Customizing image

By default, `deploy/deployment.yaml` uses `ghcr.io/coder/coder-k8s:latest`.
Edit the image tag before applying if you need a pinned version.

## If you want all-in-one mode instead

Skip `kubectl set args ... --app=controller` and keep the default `--app=all`, then also apply:

```bash
kubectl apply -f deploy/apiserver-service.yaml
kubectl apply -f deploy/apiserver-apiservice.yaml
kubectl apply -f deploy/mcp-service.yaml
```
