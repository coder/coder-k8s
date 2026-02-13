# Getting started (local development)

This tutorial walks through running the `coder-k8s` **controller** locally against a Kubernetes cluster, then creating your first `CoderControlPlane` resource.

## Prerequisites

- Go 1.25+ (`go.mod` currently declares Go 1.25.7)
- A Kubernetes cluster (OrbStack, KIND, or any conformant cluster)
- `kubectl` configured to your target context

If you use the Nix devshell:

```bash
nix develop
```

Optional: create a disposable KIND cluster with project defaults:

```bash
make kind-dev-up
```

## 1) Generate and install CRDs

Generate manifests:

```bash
make manifests
```

Install CRDs into your cluster:

```bash
kubectl apply -f config/crd/bases/
```

Create the sample namespace used by shipped manifests:

```bash
kubectl create namespace coder
```

## 2) Run the controller locally

Start controller mode (terminal A):

```bash
GOFLAGS=-mod=vendor go run . --app=controller
```

Leave this terminal running so you can watch reconciliation logs.

## 3) Create a sample `CoderControlPlane`

In a second terminal (terminal B):

```bash
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml
```

## 4) Verify reconciliation

Check resource status:

```bash
kubectl get codercontrolplanes -A
kubectl describe codercontrolplane codercontrolplane-sample -n coder
```

The controller creates a Deployment + Service named after the control plane (`codercontrolplane-sample`) in the same namespace.

```bash
kubectl get deploy,svc -n coder
```

## 5) Clean up (optional)

```bash
kubectl delete codercontrolplane codercontrolplane-sample -n coder
```

If you used `kind-dev-up`, you can remove the cluster with:

```bash
make kind-dev-down
```

## Next steps

- Deploy in-cluster: [How-to → Deploy controller](../how-to/deploy-controller.md)
- Understand internals: [Explanation → Architecture](../explanation/architecture.md)
- Explore aggregated APIs: [How-to → Deploy aggregated API server](../how-to/deploy-aggregated-apiserver.md)
