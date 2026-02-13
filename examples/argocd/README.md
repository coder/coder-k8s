# Argo CD single-apply example (CloudNativePG + coder-k8s)

This example bootstraps the full stack from one `ApplicationSet`:

1. CloudNativePG operator
2. `coder-k8s` operator stack (CRDs + RBAC + deployment)
3. CloudNativePG PostgreSQL `Cluster`
4. `CoderControlPlane`

## Prerequisites

- A Kubernetes cluster
- `kubectl` configured for that cluster
- Argo CD installed (including the ApplicationSet controller)
- Argo CD v2.6+ (required for Application `sources` in `apps/01-coder-k8s-operator.yaml`)

## Apply one manifest

```bash
kubectl apply -f examples/argocd/applicationset.yaml
```

That creates:

- `ApplicationSet` `coder-k8s-stack`
- parent `Application` `coder-k8s-stack`
- child Applications in `examples/argocd/apps/`

### Ordering behavior

`apps/00-cnpg-operator.yaml` and `apps/01-coder-k8s-operator.yaml` are synced in wave `0`, and `apps/02-coder-cloudnativepg.yaml` in wave `1`.

The CloudNativePG example manifests (`examples/cloudnativepg/`) also include resource-level sync waves so PostgreSQL is applied before `CoderControlPlane`.

## Watch rollout

```bash
kubectl -n argocd get applications
kubectl -n coder wait --for=condition=Ready cluster/coder-db --timeout=10m
kubectl -n coder rollout status deployment/coder --timeout=10m
```

## Access Coder

```bash
kubectl -n coder port-forward svc/coder 3000:80
```

Open <http://localhost:3000/setup> and complete the setup flow.

## Customization

- This example tracks `https://github.com/coder/coder-k8s.git` at `main` for child apps.
  Update `repoURL` and `targetRevision` in `examples/argocd/apps/*.yaml` if you want to pin to a tag or use a fork.
- `apps/00-cnpg-operator.yaml` uses `targetRevision: "*"` for the CloudNativePG chart.
  Pin this to a specific chart version for reproducibility.

## Cleanup

```bash
kubectl -n argocd delete applicationset coder-k8s-stack
```

Depending on your storage class reclaim policy, PVCs from PostgreSQL may remain after cleanup.
