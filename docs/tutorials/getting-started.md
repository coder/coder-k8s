# Deploy a Coder Control Plane

In this tutorial, you install the `coder-k8s` operator. Then you create one Coder instance from a `CoderControlPlane` resource.

Time: 10–15 minutes.

## Prerequisites

- A Kubernetes cluster, and `kubectl` configured to use it.
- Permission to create the objects in `dist/install.yaml`: a Namespace, CustomResourceDefinitions, a ServiceAccount, a ClusterRole, a ClusterRoleBinding, and a Deployment. Step 1 also creates the `coder` namespace.

## 1. Install the operator

Set the source one time. For an install that you can repeat with the same result, use a release tag that contains `dist/install.yaml`, not `main`.

```bash
BASE="https://raw.githubusercontent.com/coder/coder-k8s/main"
```

Apply the install bundle. Then create the namespace for the control plane:

```bash
kubectl apply -f "$BASE/dist/install.yaml"
kubectl rollout status deployment/coder-k8s -n coder-system

kubectl create namespace coder
```

`dist/install.yaml` installs the operator in controller mode. It contains the `coder-system` namespace, the `coder.com` CRDs, the RBAC, and the operator Deployment.

- The bundle does not deploy Coder. Step 2 does that.
- The bundle does not include the aggregated API server. To add it, see [Deploy the aggregated API server](../how-to/deploy-aggregated-apiserver.md).
- The bundle uses the `ghcr.io/coder/coder-k8s:latest` image. If you need a fixed operator version, pin the image too.

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

Make sure that:

- `status.phase` is `Ready`.
- `status.url` has a value, for example `http://codercontrolplane-sample.coder.svc.cluster.local:80`.
- A Deployment and a Service with the name `codercontrolplane-sample` exist in the `coder` namespace.

## 4. Open Coder (optional)

```bash
kubectl port-forward svc/codercontrolplane-sample -n coder 3000:80
```

Then open `http://127.0.0.1:3000` in a browser.

## 5. Clean up (optional)

Delete the control plane first, while the operator still runs. The operator then cleans up and removes its finalizer. After that, remove the bundle. The commands set `BASE` again, because you can be in a new shell.

!!! warning
    Delete all other `CoderControlPlane`, `CoderProvisioner`, and `CoderWorkspaceProxy` resources before you delete the bundle. When you delete the bundle, you also delete the `coder.com` CRDs, and Kubernetes then removes all remaining resources of these kinds in the cluster. When you delete the `coder` namespace, you also delete all other objects in it, for example Secrets and PersistentVolumeClaims.

```bash
BASE="https://raw.githubusercontent.com/coder/coder-k8s/main"

kubectl delete -f "$BASE/config/samples/coder_v1alpha1_codercontrolplane.yaml" --ignore-not-found
kubectl wait --for=delete codercontrolplane/codercontrolplane-sample -n coder --timeout=120s
kubectl delete -f "$BASE/dist/install.yaml" --ignore-not-found
kubectl delete namespace coder --ignore-not-found
```

## Next steps

- [Deploy an External Provisioner](deploy-coderprovisioner.md)
- [Deploy with Argo CD](deploy-with-argocd.md)
- [Deploy the aggregated API server](../how-to/deploy-aggregated-apiserver.md)
- [Run the MCP server](../how-to/mcp-server.md)
