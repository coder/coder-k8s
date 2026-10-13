# Deploy the controller

Run `coder-k8s` as an operator only (`--app=controller`). The operator reconciles `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy`, and `CoderTemplateTest` resources.

Run the commands from a clone of this repository.

## 1. Install CRDs and RBAC

```bash
kubectl create namespace coder-system
kubectl apply -f config/crd/bases/ -f config/rbac/
```

!!! warning "RBAC for CoderWorkspaceProxy, CoderProvisioner and CoderControlPlane"
    Grant `create`, `update` and `patch` on `coderworkspaceproxies`, `coderprovisioners` and `codercontrolplanes` only to principals that may already read Secrets and create Pods in that namespace. A custom `spec.image` runs with whatever the pod receives, including the proxy token or provisioner key and every `extraEnv` value. The operator uses a Secret for these kinds only when the Secret is marked as a credential. See [Configure credential Secrets for proxies and provisioners](workspace-proxy-credentials.md).

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

!!! warning "Upgrades: mark credential Secrets first"
    `CoderWorkspaceProxy` and `CoderProvisioner` use a Secret only when it has the label `coder.com/credential=true` and lists the destination in `coder.com/credential-destinations`. Before you upgrade from a version without this check, do the steps in [Upgrade from a version without these checks](workspace-proxy-credentials.md#upgrade-from-a-version-without-these-checks).

!!! warning "Upgrades: check the operator token Secrets"
    A `CoderControlPlane` uses its operator token Secret only when the control plane is the controller owner of that Secret. Earlier versions took over an existing Secret with that name. Before you upgrade, do the check in [Operator token Secret](#operator-token-secret).

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

## Operator token Secret

The controller stores the API token of the `coder-k8s-operator` user in a Secret: `spec.operatorAccess.generatedTokenSecretName`, or `<control plane>-operator-token` by default. `status.operatorTokenSecretRef` names that Secret.

- When the Secret does not exist, the controller creates it, with the control plane as the controller owner.
- When the control plane is the controller owner of the Secret (same UID), the controller reads and updates it.
- When the Secret exists and another owner, or no owner, controls it, the controller does not read, change or adopt it. It also does not create or change the token in Coder. It sets `operatorAccessReady` to `false` and removes `status.operatorTokenSecretRef`, so that provisioners, template tests and the aggregated API server stop using operator access for this control plane. The Coder Deployment does not use this Secret, so it does not change.

The controller reports the `OperatorTokenSecretOwned` condition while operator access is on:

| Status | Reason | Meaning |
|---|---|---|
| `True` | `TokenSecretOwned` | The control plane owns the Secret, or the Secret does not exist yet and the controller creates it. |
| `False` | `TokenSecretNotOwned` | The Secret exists and another owner, or no owner, controls it. The message names the Secret. |

```bash
kubectl -n coder get codercontrolplane coder \
  -o jsonpath='{range .status.conditions[?(@.type=="OperatorTokenSecretOwned")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

To fix `TokenSecretNotOwned`, do one of these steps. The controller checks again every 30 seconds.

- If the Secret is a leftover operator token Secret, for example from a control plane that you deleted with `--cascade=orphan` and created again, delete it. The controller creates a new Secret and a new operator token.
- If the Secret belongs to something else, set `spec.operatorAccess.generatedTokenSecretName` to a name that does not exist yet.

### Where the operator token goes

The controller, provisioners, template tests and the aggregated API server send the operator token to Coder. With built-in TLS (`spec.tls.secretNames`), `status.url` is `https://<control plane>.<namespace>.svc.cluster.local:443`. Certificates for an external hostname rarely cover that name. So these components do not use `status.url` for their Coder API calls. They use the plain HTTP URL of the Service instead: `http://<control plane>.<namespace>.svc.cluster.local:<port>`.

The operator token therefore crosses the cluster network without TLS. If you need encryption inside the cluster, use a service mesh. Use a `NetworkPolicy` to limit which pods can reach the control plane Service.

Provisioner pods are not affected. They still connect to `status.url`.

### Upgrade from a version that took over the Secret

Earlier versions set the control plane as the controller owner of this Secret on every reconcile. So a Secret that an earlier version used already has the right owner, and the control plane keeps working after the upgrade. A control plane fails the check only when its Secret lost that owner after the last reconcile of the earlier version.

Before you upgrade, list the operator token Secrets that the control plane does not own:

```bash
kubectl get codercontrolplanes -A -o json | jq -r '.items[] | select(.spec.operatorAccess.disabled != true) |
  "\(.metadata.namespace) \(.status.operatorTokenSecretRef.name // .spec.operatorAccess.generatedTokenSecretName // (.metadata.name + "-operator-token")) \(.metadata.uid)"' |
while read -r ns secret uid; do
  owner=$(kubectl -n "$ns" get secret "$secret" -o jsonpath='{.metadata.ownerReferences[?(@.controller==true)].uid}' 2>/dev/null) || continue
  [ "$owner" = "$uid" ] || echo "not owned: $ns/$secret"
done
```

The command prints nothing when every Secret is owned or does not exist. For each line that it prints, do one of the fix steps above.
