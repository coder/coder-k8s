# Deploy a Coder Control Plane

This tutorial deploys the `coder-k8s` operator and brings up one managed Coder instance from a `CoderControlPlane` resource.

Estimated time: **10–15 minutes**.

## Prerequisites

- A Kubernetes cluster
- `kubectl` configured to your target context
- Permissions to create namespaces, CRDs, RBAC resources, and Deployments

## 1) Install the operator (minimal installer)

Apply the minimal installer manifest from GitHub (operator namespace, CRDs, RBAC, deployment, and aggregated API registration):

```bash
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/dist/minimal-installer.yaml"
```

!!! tip
    For reproducible installs, replace `/main/` with a release tag (for example `/v0.1.0/`).

Wait for the operator pod:

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get pods -n coder-system
```

## 2) Create a `CoderControlPlane` instance

Create the control plane namespace and apply the sample resource:

```bash
kubectl create namespace coder --dry-run=client -o yaml | kubectl apply -f -
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

## 5) Seed quickstart template + workspace (optional)

After the aggregated API is available and your sample control plane is ready, apply the quickstart installer:

```bash
kubectl wait --for=condition=Available apiservice/v1alpha1.aggregation.coder.com --timeout=180s
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/dist/quickstart-installer.yaml"
```

If the first apply races aggregated API startup, wait a few seconds and re-run the same command.

Verify the quickstart resources:

```bash
kubectl -n coder get codertemplates.aggregation.coder.com default.quickstart-template
kubectl -n coder get coderworkspaces.aggregation.coder.com default.me.quickstart-workspace
```

## 6) Clean up (optional)

```bash
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/dist/quickstart-installer.yaml" --ignore-not-found
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/samples/coder_v1alpha1_codercontrolplane.yaml" --ignore-not-found
kubectl delete -f "https://raw.githubusercontent.com/coder/coder-k8s/main/dist/minimal-installer.yaml" --ignore-not-found
```

## Next steps

- Run an external provisioner daemon: [Deploy an External Provisioner](deploy-coderprovisioner.md)
- GitOps path: [Deploy operator stack with Argo CD](deploy-with-argocd.md)
- Split deployment model: [Deploy aggregated API server](../how-to/deploy-aggregated-apiserver.md)
- MCP operations: [Run MCP server](../how-to/mcp-server.md)
