# Deploy a Coder Control Plane

Install the `coder-k8s` operator, then create one Coder instance from a `CoderControlPlane` resource.

**Time:** 10–15 minutes.

## Prerequisites

- A Kubernetes cluster and `kubectl` pointed at it.
- Permission to create namespaces, CRDs, RBAC, and Deployments.

## 1. Install the operator

Set the source once. Use a release tag instead of `main` (for example `v0.1.0`) for reproducible installs.

```bash
BASE="https://raw.githubusercontent.com/coder/coder-k8s/main"
```

Create the namespaces and apply the CRDs, RBAC, and operator Deployment:

```bash
kubectl create namespace coder-system
kubectl create namespace coder

kubectl apply \
  -f "$BASE/config/crd/bases/coder.com_codercontrolplanes.yaml" \
  -f "$BASE/config/crd/bases/coder.com_coderprovisioners.yaml" \
  -f "$BASE/config/crd/bases/coder.com_coderworkspaceproxies.yaml" \
  -f "$BASE/config/rbac/serviceaccount.yaml" \
  -f "$BASE/config/rbac/role.yaml" \
  -f "$BASE/config/rbac/clusterrolebinding.yaml" \
  -f "$BASE/config/rbac/authentication-reader-binding.yaml" \
  -f "$BASE/config/rbac/auth-delegator-binding.yaml" \
  -f "$BASE/deploy/deployment.yaml"

kubectl rollout status deployment/coder-k8s -n coder-system
```

## 2. Create a control plane

```bash
kubectl apply -f "$BASE/config/samples/coder_v1alpha1_codercontrolplane.yaml"
```

## 3. Verify

```bash
kubectl get codercontrolplane codercontrolplane-sample -n coder \
  -o jsonpath='{.status.phase}{"\n"}{.status.url}{"\n"}'
kubectl rollout status deployment/codercontrolplane-sample -n coder
kubectl get deployment,service codercontrolplane-sample -n coder
```

You should see:

- `status.phase` is `Ready`.
- `status.url` is set, for example `http://codercontrolplane-sample.coder.svc.cluster.local:80`.
- A Deployment and Service named `codercontrolplane-sample` exist in `coder`.

## 4. Open Coder (optional)

```bash
kubectl port-forward svc/codercontrolplane-sample -n coder 3000:80
```

Then browse to <http://127.0.0.1:3000>.

## 5. Clean up (optional)

Delete in reverse order:

```bash
kubectl delete \
  -f "$BASE/config/samples/coder_v1alpha1_codercontrolplane.yaml" \
  -f "$BASE/deploy/deployment.yaml" \
  -f "$BASE/config/rbac/auth-delegator-binding.yaml" \
  -f "$BASE/config/rbac/authentication-reader-binding.yaml" \
  -f "$BASE/config/rbac/clusterrolebinding.yaml" \
  -f "$BASE/config/rbac/role.yaml" \
  -f "$BASE/config/rbac/serviceaccount.yaml" \
  -f "$BASE/config/crd/bases/coder.com_coderworkspaceproxies.yaml" \
  -f "$BASE/config/crd/bases/coder.com_coderprovisioners.yaml" \
  -f "$BASE/config/crd/bases/coder.com_codercontrolplanes.yaml"
kubectl delete namespace coder coder-system --ignore-not-found
```

## Next steps

- [Deploy an External Provisioner](deploy-coderprovisioner.md)
- [Deploy with Argo CD](deploy-with-argocd.md)
- [Deploy the aggregated API server](../how-to/deploy-aggregated-apiserver.md)
- [Run the MCP server](../how-to/mcp-server.md)
