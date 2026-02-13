# Deploy a Coder Control Plane

This tutorial deploys the `coder-k8s` operator and brings up one managed Coder instance from a `CoderControlPlane` resource.

Estimated time: **10–15 minutes**.

## Prerequisites

- A Kubernetes cluster
- `kubectl` configured to your target context
- Permissions to create namespaces, CRDs, RBAC resources, and Deployments

## 1) Install the operator

Create the operator namespace and apply CRDs/RBAC/deployment from GitHub:

```bash
kubectl create namespace coder-system
kubectl create namespace coder
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/crd/bases/coder.com_codercontrolplanes.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/crd/bases/coder.com_coderprovisioners.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/crd/bases/coder.com_coderworkspaceproxies.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/serviceaccount.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/role.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/clusterrolebinding.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/authentication-reader-binding.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/auth-delegator-binding.yaml"
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/deploy/deployment.yaml"
```

!!! tip
    For reproducible installs, replace `/main/` with a release tag (for example `/v0.1.0/`).

Wait for the operator pod:

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get pods -n coder-system
```

## 2) Create a `CoderControlPlane` instance

Apply the sample control plane resource:

```bash
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/samples/coder_v1alpha1_codercontrolplane.yaml"
```

## 3) Verify reconciliation

Check the CR status:

```bash
kubectl get codercontrolplane codercontrolplane-sample -n coder
kubectl get codercontrolplane codercontrolplane-sample -n coder -o jsonpath='{.status.phase}{"\n"}{.status.url}{"\n"}'
```

Verify the managed Coder Deployment and Service are present and ready:

```bash
kubectl rollout status deployment/codercontrolplane-sample -n coder
kubectl get deployment codercontrolplane-sample -n coder
kubectl get service codercontrolplane-sample -n coder
```

Expected result:

- `status.phase` becomes `Ready`
- `status.url` is populated (for example `http://codercontrolplane-sample.coder.svc.cluster.local:80`)
- A Coder Deployment and Service named `codercontrolplane-sample` exist in `coder`

## 4) Access the Coder instance (optional)

Port-forward the Service locally:

```bash
kubectl port-forward svc/codercontrolplane-sample -n coder 3000:80
```

Then open:

```text
http://127.0.0.1:3000
```

## 5) Clean up (optional)

```bash
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/samples/coder_v1alpha1_codercontrolplane.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/deploy/deployment.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/auth-delegator-binding.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/authentication-reader-binding.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/clusterrolebinding.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/role.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/rbac/serviceaccount.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/crd/bases/coder.com_coderworkspaceproxies.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/crd/bases/coder.com_coderprovisioners.yaml"
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/crd/bases/coder.com_codercontrolplanes.yaml"
kubectl delete namespace coder --ignore-not-found
kubectl delete namespace coder-system --ignore-not-found
```

## Next steps

- Run an external provisioner daemon: [Deploy an External Provisioner](deploy-coderprovisioner.md)
- GitOps path: [Deploy operator stack with Argo CD](deploy-with-argocd.md)
- Split deployment model: [Deploy aggregated API server](../how-to/deploy-aggregated-apiserver.md)
- MCP operations: [Run MCP server](../how-to/mcp-server.md)
