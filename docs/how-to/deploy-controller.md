# Deploy the controller

Run `coder-k8s` as an operator only (`--app=controller`). It reconciles `CoderControlPlane`, `CoderProvisioner`, and `CoderWorkspaceProxy`.

Commands run from a clone of this repository.

## 1. Install CRDs and RBAC

```bash
kubectl create namespace coder-system
kubectl apply -f config/crd/bases/ -f config/rbac/
```

## 2. Deploy in controller-only mode

`deploy/deployment.yaml` defaults to `--app=all`. Switch it to the controller:

```bash
kubectl apply -f deploy/deployment.yaml
kubectl -n coder-system patch deployment/coder-k8s --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args","value":["--app=controller"]}]'
```

!!! tip "Pin the image"
    The manifest uses `ghcr.io/coder/coder-k8s:latest`. Edit the tag before applying to pin a version.

## 3. Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl logs -n coder-system deploy/coder-k8s
```

Optional smoke test:

```bash
kubectl create namespace coder
kubectl apply -f config/samples/coder_v1alpha1_codercontrolplane.yaml
kubectl get codercontrolplanes -A
```

## Want everything instead?

Skip the `kubectl patch` step to keep `--app=all` (operator plus aggregated API server), and register the aggregated API:

```bash
kubectl apply -f deploy/apiserver-service.yaml -f deploy/apiserver-apiservice.yaml
```

## Connect an external PostgreSQL database

Coder needs a PostgreSQL connection URL. Store it in a Secret in the same namespace as the `CoderControlPlane`, then reference the Secret key in `spec.database.connectionSecretRef`:

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderControlPlane
metadata:
  name: coder
  namespace: coder
spec:
  database:
    connectionSecretRef:
      name: coder-db-app
      key: uri
```

The value must be a `postgres://` or `postgresql://` URL. The controller then does two things:

1. It sets `CODER_PG_CONNECTION_URL` in the Coder container with `valueFrom.secretKeyRef`. The Deployment holds a reference to the Secret, not a copy of the URL.
2. It reads the URL from the Secret to create the `coder-k8s-operator` user and API token (operator access bootstrap).

Both `name` and `key` are required. Do not also set `CODER_PG_CONNECTION_URL` in `spec.extraEnv`: the API server rejects that combination. The controller does not check `spec.envFrom` for this variable, so do not provide it there either.

Without `spec.database`, the controller keeps the earlier behavior: set `CODER_PG_CONNECTION_URL` in `spec.extraEnv`, as a literal value or with `valueFrom.secretKeyRef`.

### Check the `DatabaseSecretResolved` condition

While `spec.database` is set, the controller reports the `DatabaseSecretResolved` condition. It re-checks the condition when the Secret is created, updated, or deleted. When you remove `spec.database`, the controller removes the condition.

| Status | Reason | Meaning |
|---|---|---|
| `True` | `Resolved` | The Secret key contains a `postgres://` or `postgresql://` URL. |
| `False` | `SecretNotFound` | The Secret does not exist in the namespace. |
| `False` | `KeyNotFound` | The Secret exists but does not contain the key. |
| `False` | `EmptyValue` | The value is empty or contains only whitespace. |
| `False` | `InvalidURL` | The value does not parse as a `postgres://` or `postgresql://` URL. The scheme must be exactly lowercase: for example, `Postgres://` is rejected. |
| `False` | `ConflictingConfiguration` | `spec.extraEnv` also sets `CODER_PG_CONNECTION_URL`. The controller leaves the Deployment unchanged. |

`Resolved` does not mean that the database is healthy or reachable. The controller does not connect to PostgreSQL to set this condition. Condition messages name the Secret and key, but never contain the URL or credentials.

```bash
kubectl -n coder get codercontrolplane coder \
  -o jsonpath='{range .status.conditions[?(@.type=="DatabaseSecretResolved")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

You can create the `CoderControlPlane` before its Secret. Until the Secret exists, the condition reason is `SecretNotFound`, the Coder pod cannot start, and the operator access bootstrap waits. After you create the Secret, the controller reconciles again and Kubernetes starts the pod.

### Rotate database credentials

Kubernetes does not update environment variables in running pods when a Secret changes. After you change the connection URL in the Secret (for example, a new password):

1. Confirm that the condition reason is `Resolved`.
2. Restart the Coder Deployment so new pods read the new value:

    ```bash
    kubectl -n coder rollout restart deployment/coder
    kubectl -n coder rollout status deployment/coder
    ```

The controller does not restart pods automatically. The operator access bootstrap reads the Secret again on each reconcile, so it uses the new URL without a restart.
