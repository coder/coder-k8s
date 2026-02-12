# Troubleshooting

## The binary exits immediately with "--app flag is required"

`coder-k8s` requires an explicit application mode.

For the controller:

```bash
GOFLAGS=-mod=vendor go run . --app=controller
```

For the aggregated API server:

```bash
GOFLAGS=-mod=vendor go run . --app=aggregated-apiserver
```

## "no matches for kind" when applying a `CoderControlPlane`

This usually means the CRD isn't installed in the cluster.

```bash
make manifests
kubectl apply -f config/crd/bases/
```

## The controller is running, but reconciliation doesn't happen

- Check controller logs:

  ```bash
  kubectl logs -n coder-system deploy/coder-k8s-controller
  ```

- Confirm RBAC is applied:

  ```bash
  kubectl get clusterrole coder-k8s-controller
  kubectl get clusterrolebinding coder-k8s-controller
  ```

## Aggregated `codertemplates` / `coderworkspaces` reads are empty or inconsistent

If `kubectl get codertemplates.aggregation.coder.com` or `kubectl get coderworkspaces.aggregation.coder.com` returns empty data unexpectedly, check for CRD/APIService conflicts.

When both of these exist at once:

- `APIService` `v1alpha1.aggregation.coder.com`
- CRDs `codertemplates.aggregation.coder.com` and/or `coderworkspaces.aggregation.coder.com`

Kubernetes may not route reads the way you expect for demos.

```bash
kubectl get apiservice v1alpha1.aggregation.coder.com
kubectl get crd codertemplates.aggregation.coder.com coderworkspaces.aggregation.coder.com
```

For APIService-based demos, remove the conflicting aggregation CRDs:

```bash
kubectl delete crd codertemplates.aggregation.coder.com coderworkspaces.aggregation.coder.com
kubectl apply -f deploy/apiserver-apiservice.yaml
kubectl wait --for=condition=Available apiservice/v1alpha1.aggregation.coder.com --timeout=120s
```

## Aggregated APIService shows `False` / `Unavailable`

- Ensure the deployment and service exist:

  ```bash
  kubectl get deploy,svc -n coder-system | grep coder-k8s-apiserver
  ```

- Inspect APIService status:

  ```bash
  kubectl describe apiservice v1alpha1.aggregation.coder.com
  ```

- Check the aggregated API server logs:

  ```bash
  kubectl logs -n coder-system deploy/coder-k8s-apiserver
  ```
