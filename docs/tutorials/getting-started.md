# Deploy a Coder Control Plane

Install the `coder-k8s` operator, then create one Coder instance from a `CoderControlPlane` resource.

**Time:** 10–15 minutes.

## Prerequisites

- A Kubernetes cluster and `kubectl` pointed at it.
- Permission to create what `dist/install.yaml` contains: a Namespace, CustomResourceDefinitions, a ServiceAccount, a ClusterRole, ClusterRoleBindings, a RoleBinding in `kube-system`, a Service, a Deployment, and an `apiregistration.k8s.io/v1` APIService. Step 1 also creates the `coder` namespace.

## 1. Install the operator

Set the source once. For reproducible installs, use a release tag that contains `dist/install.yaml` instead of `main`.

```bash
BASE="https://raw.githubusercontent.com/coder/coder-k8s/main"
```

Apply the install bundle, then create the namespace for the control plane:

```bash
kubectl apply -f "$BASE/dist/install.yaml"
kubectl rollout status deployment/coder-k8s -n coder-system

kubectl create namespace coder
```

`dist/install.yaml` creates the `coder-system` namespace, the `coder.com` CRDs, RBAC, the operator Deployment, and the aggregated API server's Service and APIService. It does not deploy Coder; step 2 does that. The bundle runs the `ghcr.io/coder/coder-k8s:latest` image, so pin the image too if you need a fixed operator version.

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

Then browse to `http://127.0.0.1:3000`.

## 5. Clean up (optional)

Delete the control plane first, while the operator is still running, so it can clean up and remove its finalizer. Then remove the bundle. The block sets `BASE` again in case you are in a new shell:

```bash
BASE="https://raw.githubusercontent.com/coder/coder-k8s/main"

kubectl delete -f "$BASE/config/samples/coder_v1alpha1_codercontrolplane.yaml" --ignore-not-found
kubectl wait --for=delete codercontrolplane/codercontrolplane-sample -n coder --timeout=120s
kubectl delete -f "$BASE/dist/install.yaml" --ignore-not-found
kubectl delete namespace coder --ignore-not-found
```

Deleting the bundle also deletes the `coder.com` CRDs, which removes every remaining `CoderControlPlane`, `CoderProvisioner`, and `CoderWorkspaceProxy` in the cluster; delete those first. Deleting the `coder` namespace removes everything else in it, such as Secrets and PersistentVolumeClaims.

## Next steps

- [Deploy an External Provisioner](deploy-coderprovisioner.md)
- [Deploy with Argo CD](deploy-with-argocd.md)
- [Deploy the aggregated API server](../how-to/deploy-aggregated-apiserver.md)
- [Run the MCP server](../how-to/mcp-server.md)
