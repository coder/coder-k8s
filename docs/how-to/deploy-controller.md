# Deploy the controller

Run `coder-k8s` as an operator only (`--app=controller`). The operator reconciles `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy`, and `CoderTemplateTest` resources.

Run the commands from a clone of this repository.

## 1. Install CRDs and RBAC

```bash
kubectl create namespace coder-system
kubectl apply -f config/crd/bases/ -f config/rbac/
```

## 2. Deploy in controller-only mode

The default of `deploy/deployment.yaml` is `--app=all`. Change it to the controller:

```bash
kubectl apply -f deploy/deployment.yaml
kubectl -n coder-system patch deployment/coder-k8s --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args","value":["--app=controller"]}]'
```

!!! tip "Pin the image"
    The manifest uses `ghcr.io/coder/coder-k8s:latest`. To pin a version, change the tag before you apply the manifest.

!!! warning "Upgrades: apply the CRDs and RBAC first"
    Apply `config/crd/bases/` and `config/rbac/` from the new version before or together with the new image. The operator watches every kind it reconciles. If a kind's CRD is missing, for example `CoderTemplateTest` after an upgrade from an earlier version, the manager cannot start.

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

## Run all components instead

To keep `--app=all` (the operator and the aggregated API server), do not do the `kubectl patch` step. Then register the aggregated API:

```bash
kubectl apply -f deploy/apiserver-service.yaml -f deploy/apiserver-apiservice.yaml
```

## Connect an external PostgreSQL database

Coder requires a PostgreSQL connection URL.

1. Put the URL in a Secret in the same namespace as the `CoderControlPlane`.
2. Set `spec.database.connectionSecretRef` to the name and key of that Secret:

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

Rules:

- You must set both `name` and `key`.
- Do not also set `CODER_PG_CONNECTION_URL` in `spec.extraEnv`. The API server rejects that combination.
- Do not set this variable in `spec.envFrom`. The controller does not examine `spec.envFrom` for this variable.

If you do not set `spec.database`, the controller keeps the earlier behavior. You set `CODER_PG_CONNECTION_URL` in `spec.extraEnv`, as a literal value or with `valueFrom.secretKeyRef`.

### Check the `DatabaseSecretResolved` condition

While `spec.database` has a value, the controller reports the `DatabaseSecretResolved` condition. The controller does a new check of the condition when the Secret is created, updated, or deleted. When you remove `spec.database`, the controller removes the condition.

| Status | Reason | Meaning |
|---|---|---|
| `True` | `Resolved` | The Secret key contains a `postgres://` or `postgresql://` URL. |
| `False` | `SecretNotFound` | The Secret does not exist in the namespace. |
| `False` | `KeyNotFound` | The Secret exists but does not contain the key. |
| `False` | `EmptyValue` | The value is empty or contains only whitespace. |
| `False` | `InvalidURL` | The value is not a valid `postgres://` or `postgresql://` URL. The scheme must be lowercase. For example, the controller rejects `Postgres://`. |
| `False` | `ConflictingConfiguration` | `spec.extraEnv` also sets `CODER_PG_CONNECTION_URL`. The controller does not change the Deployment. |

`Resolved` does not mean that the database is healthy or that the network can reach it. The controller does not connect to PostgreSQL to set this condition. The condition messages contain the names of the Secret and the key, but never the URL or credentials.

```bash
kubectl -n coder get codercontrolplane coder \
  -o jsonpath='{range .status.conditions[?(@.type=="DatabaseSecretResolved")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

You can create the `CoderControlPlane` before its Secret. Until the Secret exists:

- The condition reason is `SecretNotFound`.
- The Coder pod cannot start.
- The operator access bootstrap waits.

After you create the Secret, the controller reconciles again and Kubernetes starts the pod.

### Rotate database credentials

When a Secret changes, Kubernetes does not update the environment variables in running pods. After you change the connection URL in the Secret (for example, to set a new password), do these steps:

1. Make sure that the condition reason is `Resolved`.
2. Restart the Coder Deployment, so that the new pods read the new value:

    ```bash
    kubectl -n coder rollout restart deployment/coder
    kubectl -n coder rollout status deployment/coder
    ```

The controller does not restart pods automatically. The operator access bootstrap reads the Secret again on each reconcile. Thus it uses the new URL without a restart.
