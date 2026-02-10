# CloudNativePG-backed CoderControlPlane example

This example deploys a `CoderControlPlane` backed by a PostgreSQL cluster managed by [CloudNativePG](https://cloudnative-pg.io/).

It creates:

- a `coder` namespace
- a CloudNativePG `Cluster` named `coder-db`
- a `CoderControlPlane` named `coder` wired to the CloudNativePG app Secret (`coder-db-app`)

## Prerequisites

- A Kubernetes cluster
- `kubectl` configured for that cluster
- `helm`

## 1. Install the CloudNativePG operator

```bash
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm repo update
helm upgrade --install cnpg cnpg/cloudnative-pg \
  --namespace cnpg-system \
  --create-namespace
```

## 2. Install the coder-k8s controller

Follow [Deploy the controller (in-cluster)](../../docs/how-to/deploy-controller.md), or run:

```bash
kubectl create namespace coder-system
kubectl apply -f config/crd/bases/
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/controller-deployment.yaml
kubectl rollout status deployment/coder-k8s-controller -n coder-system
```

## 3. Deploy this example

```bash
kubectl apply -f examples/cloudnativepg/
```

The namespace manifest is prefixed (`00-namespace.yaml`) so `kubectl apply -f` creates `coder` before namespaced resources.

Wait for PostgreSQL and verify the generated Secret:

```bash
kubectl -n coder wait --for=condition=Ready cluster/coder-db --timeout=10m
kubectl -n coder get secret coder-db-app
```

Wait for the `CoderControlPlane` deployment:

```bash
kubectl -n coder rollout status deployment/coder --timeout=10m
kubectl -n coder get codercontrolplane coder
```

## 4. Access Coder

In one terminal:

```bash
kubectl -n coder port-forward svc/coder 8080:80
```

Then open:

- <http://localhost:8080/setup>

Use the setup flow to create the first admin user.

## Important limitation of this quickstart

This example sets:

- `CODER_ACCESS_URL=http://localhost:8080`

That value is convenient for UI smoke tests through `kubectl port-forward`, but it is not suitable for end-to-end workspace connectivity because in-cluster components cannot reach your local `localhost`.

For a full setup, expose Coder with an ingress/load balancer and set `CODER_ACCESS_URL` to a real external URL (for example, `https://coder.example.com`).
