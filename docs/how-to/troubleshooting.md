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
