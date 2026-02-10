# Getting started (local development)

This tutorial walks through running the `coder-k8s` **controller** locally against a Kubernetes cluster.

## Prerequisites

- Go 1.25+ (`go.mod` currently declares Go 1.25.7)
- A Kubernetes cluster (OrbStack is recommended for local development; any cluster works)
- `kubectl` configured to point at your cluster context

If you use the Nix devshell, run:

```bash
nix develop
```

## 1. Generate and install CRDs

Generate the CRD and RBAC manifests:

```bash
make manifests
```

Install the CRDs into your cluster:

```bash
kubectl apply -f config/crd/bases/
```

## 2. Run the controller locally

Run the controller in **controller** mode (uses your kubeconfig context):

```bash
GOFLAGS=-mod=vendor go run . --app=controller
```

## 3. Create a sample `CoderControlPlane`

In another terminal:

```bash
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml
```

## 4. Verify

```bash
kubectl get codercontrolplanes -A
```

## Next steps

- Learn how to deploy the controller in-cluster: **How-to guides → Deploy controller**.
- Learn how the project is structured: **Explanation → Architecture**.
