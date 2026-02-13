# Argo CD single-apply example (CloudNativePG + coder-k8s)

This example bootstraps the full stack from one `ApplicationSet`.

The generated Argo CD `Application` uses multiple sources to deploy:

1. CloudNativePG operator (Helm chart)
2. `coder-k8s` operator stack (`config/crd/bases`, `config/rbac`, `deploy`)
3. CloudNativePG PostgreSQL `Cluster`
4. `CoderControlPlane`

## Prerequisites

- A Kubernetes cluster
- `kubectl` configured for that cluster
- Argo CD installed (including the ApplicationSet controller)
- Argo CD v2.6+ (required for `spec.sources` in the generated Application)

## Apply one manifest

```bash
kubectl apply -f https://raw.githubusercontent.com/coder/coder-k8s/main/examples/argocd/applicationset.yaml
```

That creates:

- `ApplicationSet` `coder-k8s-stack`
- generated `Application` `coder-k8s-stack`

## Ordering behavior

This setup avoids app-of-apps ordering ambiguity by using a single generated `Application` with multiple sources.

Resource-level sync waves provide dependency ordering for workload resources:

- `examples/argocd/resources/00-coder-system-namespace.yaml` uses `wave -1`
- `examples/cloudnativepg/00-namespace.yaml` uses `wave 0`
- `examples/cloudnativepg/cnpg-cluster.yaml` uses `wave 1`
- `examples/cloudnativepg/codercontrolplane.yaml` uses `wave 2`

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

- This example tracks `https://github.com/coder/coder-k8s.git` at `main`.
  Update `repoURL` and `targetRevision` in `examples/argocd/applicationset.yaml` if you want to pin to a tag or use a fork.
- The CloudNativePG chart version is configurable with `cloudnativepgChartVersion`.
  Pin it to an explicit chart version for reproducible environments.

## Cleanup

```bash
kubectl -n argocd delete applicationset coder-k8s-stack
```

The generated `Application` includes `resources-finalizer.argocd.argoproj.io` so deleting the `ApplicationSet` cascades cleanup of managed resources. Depending on your storage class reclaim policy, PVCs from PostgreSQL may remain after cleanup.
