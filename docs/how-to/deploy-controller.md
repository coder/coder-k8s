# Deploy the controller

Run `coder-k8s` as an operator only (`--app=controller`). The operator reconciles `CoderControlPlane`, `CoderProvisioner`, `CoderWorkspaceProxy`, `CoderTemplateTest`, `CoderAIProvider`, `CoderAIGatewayKey`, and `CoderMCPServer` resources.

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
- When the Secret exists and another owner, or no owner, controls it, the controller does not read, change or adopt it. It also does not create a token in Coder. It sets `operatorAccessReady` to `false`, so that provisioners, template tests and the aggregated API server stop using operator access for this control plane. It removes `status.operatorTokenSecretRef` only after it deleted its earlier operator token in Coder, as the next paragraphs describe. The Coder Deployment does not use this Secret, so it does not change.

When `status.operatorTokenSecretRef` still names a Secret at that point, the controller provisioned an operator token for this control plane that can still work in Coder. The controller then deletes that token in Coder, so that a copy of the old Secret value stops working. The token has the same name in every database, so the controller deletes it only in the database where it created it. Before the controller creates or checks the token in PostgreSQL, it writes the record of the token to the status: `status.operatorTokenSecretRef`, `status.operatorTokenDatabaseHash`, the SHA-256 hash of the PostgreSQL URL without its password, and `status.operatorTokenID`, a key ID that the controller generates. It creates the token with that ID. So the record exists also when the controller stops right after it created the token. Because the create can also have failed, the controller then deletes only the key with that ID, never a token that another installation created with the same name. A record that an earlier version wrote has no `status.operatorTokenID`. That version wrote the record only after a successful create, so the controller deletes that token by name. After the upgrade, it replaces the token once with one that has a recorded ID. A PostgreSQL error, or a URL that the controller cannot resolve, sets `operatorAccessReady` to `false` and keeps the record. The status holds one record: when you point a control plane whose Secret it owns at another database, the controller records the new database before it provisions there, so it no longer deletes a token in the earlier database. Delete that token in Coder yourself, or it expires after 365 days. Until the delete succeeds, for example while the Coder PostgreSQL database is not reachable, `status.operatorTokenSecretRef` stays set as the record of the pending delete, `operatorAccessReady` stays `false`, and the condition message says that the delete is pending. When the status names no Secret, the controller deletes no token, because it cannot prove that the token in Coder is its own.

The controller also deletes no token, and keeps `status.operatorTokenSecretRef`, when the hash of the current PostgreSQL URL differs from the recorded hash. This happens when you point the control plane at another database, or change the user, host, port, database name or options of the URL. It also happens when the URL is neither a `postgres://` or `postgresql://` URL nor a `key=value` connection string, or when an earlier version of the controller created the token and recorded no hash. A `key=value` string, for example `host=db port=5432 user=coder dbname=coder`, has the hash of the `postgres://` URL with the same parts. It gets no hash when it leaves out `host`, `port` or `dbname`, because lib/pq then takes them from `PGHOST`, `PGPORT` or `PGDATABASE` of the controller, or when a value is empty, such as `host=''`. After an upgrade, the controller records the hash the next time it provisions or checks the token of a control plane whose Secret it owns. A new password alone keeps the hash. The hash describes the URL, not the server: when a DNS name in the URL moves to another PostgreSQL server, the hash stays the same, so the controller sends the delete to that server, which removes a token with the same name if one exists there. The condition message names the token and says that it can still work. Set the URL back to the database where the controller created the token, or delete that token in Coder yourself, then fix `TokenSecretNotOwned` as described below.

When you set `spec.operatorAccess.disabled`, the controller deletes the recorded token with the same rules: only with a record, and only in the recorded database. Without such a record, it deletes in the current database only the exact token that the Secret holds, because no other token has that value, and it keeps the Secret until that delete works. It also deletes the exact token of every other Secret in the namespace that the control plane controls, for example the Secret of an earlier `spec.operatorAccess.generatedTokenSecretName`, and leaves those Secrets in place. When that Secret is missing or holds no token, the controller has no proof of any token and deletes none in Coder. When the current database has no token with that value, the controller cannot confirm that the token is gone: it keeps the Secret, logs `Keeping the operator token Secret`, and checks again every 30 seconds. Delete the token in Coder yourself, then delete the Secret. Then it deletes the Secret if the control plane owns it. When the hash does not match, it keeps the record, logs `Not deleting the operator token`, and checks again every 30 seconds, until the URL matches again or you turn operator access back on.

When you delete a control plane with `kubectl delete --cascade=orphan`, Kubernetes removes the control plane as the owner of this Secret while the control plane still waits for the AI Gateway keys, AI providers and MCP servers that use it, so one that you delete in that window cannot use the token and leaves its resource in Coder, and you must delete those objects before the control plane.

Every component that sends the operator token to Coder reads the Secret in `status.operatorTokenSecretRef` only when the control plane is its controller owner: the license and entitlements calls of the controller, provisioners, template tests and the aggregated API server. So none of them uses a Secret that someone replaced after the controller last checked it.

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

The controller, provisioners, template tests, the AI resources and the aggregated API server send the operator token to Coder. The URL and the transport depend on `spec.tls`:

| `spec.tls` | URL of the operator's Coder API calls | Transport |
| --- | --- | --- |
| No `secretNames` | `status.url`, `http://<control plane>.<namespace>.svc.cluster.local:<port>` | Plain HTTP |
| `secretNames` only | `http://<control plane>.<namespace>.svc.cluster.local:<port>`, not `status.url` | Plain HTTP |
| `secretNames` and `operatorAccess` | `status.url`, `https://<control plane>.<namespace>.svc.cluster.local:443` | HTTPS, verified |

With built-in TLS but without `operatorAccess`, the operator does not call `status.url`, because certificates for an external hostname rarely cover the Service name. The operator token then crosses the cluster network without TLS. Set `spec.tls.operatorAccess`, or use a service mesh. Use a `NetworkPolicy` to limit which pods can reach the control plane Service.

Provisioner pods are not affected. They still connect to `status.url`.

#### Verify Coder over HTTPS

1. Make sure that the Coder serving certificate in `spec.tls.secretNames` holds the DNS name `<control plane>.<namespace>.svc.cluster.local`. If it holds only another name, for example `coder.example.com`, set `serverName` to that name in step 3.
2. Put the PEM CA certificates that sign the serving certificate into a ConfigMap in the namespace of the control plane. For a cert-manager CA issuer, the TLS Secret holds the CA as `ca.crt`:

    ```bash
    kubectl -n coder create configmap coder-ca \
      --from-literal=ca.crt="$(kubectl -n coder get secret coder-tls -o jsonpath='{.data.ca\.crt}' | base64 -d)"
    ```

    A trust-manager `Bundle` that writes a ConfigMap works too.

3. Set `spec.tls.operatorAccess`:

    ```yaml
    spec:
      tls:
        secretNames: [coder-tls]
        operatorAccess:
          caBundleRef:
            name: coder-ca
            key: ca.crt
          # serverName: coder.example.com
    ```

The operator trusts exactly one set of roots:

- With `caBundleRef`, it trusts only the CA certificates in the bundle. It never falls back to the public roots of its image, so a certificate from a public CA fails unless its CA is in the bundle.
- Without `caBundleRef`, it trusts only the public roots of its image. That works only with a public certificate and `serverName` set to its DNS name.

With `operatorAccess`, the operator never sends the operator token over plain HTTP:

- A missing ConfigMap or key, or a bundle with anything other than PEM certificates, stops every Coder call of the operator for this control plane. `CoderTemplateTest` and the AI resources report `ControlPlaneNotReady` with the error, `CoderProvisioner` reports `OperatorAccessReady=False` with reason `OperatorTLSUnavailable`, a control plane with `spec.licenseSecretRef` reports `LicenseApplied=False` with reason `Pending`, and the aggregated API answers `503`.
- A certificate that the bundle does not verify fails each call with a certificate error (`tls: failed to verify certificate: x509: ...`).
- The operator reads the ConfigMap from its cache each time it builds a Coder client, for example once per reconcile and once per aggregated API request. After you change the bundle, new clients use it as soon as the cache has the change. During a CA rotation, put the old and the new CA into the bundle until the serving certificate uses the new CA.
- An `https://` proxy in `HTTPS_PROXY` cannot carry these calls, and they fail with an error that says so. An `http://` proxy works. Better, exclude the Service host with `NO_PROXY`, for example `NO_PROXY=.svc,.cluster.local`.
- The operator already reads ConfigMaps in every namespace, so this needs no new RBAC.
- Existing `CoderProvisioner` keys stay. A key that a provisioner recorded at the plain HTTP URL before you set `operatorAccess` is deleted over HTTPS.

#### Redirects to another origin

The operator never sends the operator token to another origin. Every operator client that sends the token follows a redirect only when the target has the same scheme, host and port as the URL that the client called. A redirect to any other origin fails the call. This covers the license and entitlements calls of the control-plane controller, the provisioner key calls, and the aggregated API server. The aggregated API server's client also serves template tests and the AI resources. The check also covers the websocket that streams workspace build logs for `?follow=true`.

Coder's `CODER_REDIRECT_TO_ACCESS_URL=true` setting (`--redirect-to-access-url`) conflicts with this. With that setting, Coder answers every request whose host is not the host of the access URL with a `307` to the access URL. The operator calls Coder on the in-cluster Service host, which is never the access URL host. So every operator call gets that redirect and fails. Without this check, the calls would also fail, because the redirect drops the API path, and the token would also go to the access URL.

Do not set `CODER_REDIRECT_TO_ACCESS_URL` in `spec.extraEnv` of a `CoderControlPlane`. To redirect browsers from HTTP to HTTPS, do it in your Ingress, Gateway or load balancer instead. When the setting is on, the operator log shows `refusing redirect to a different origin than the coder URL`, and the license, provisioner and aggregated API calls fail.

### Upgrade from a version that took over the Secret

Earlier versions set the control plane as the controller owner of this Secret each time they wrote the token into it. So a Secret that an earlier version used already has the right owner, and the control plane keeps working after the upgrade. A control plane fails the check in two cases:

- Its Secret lost that owner after the last reconcile of the earlier version. For example, you deleted the control plane with `--cascade=orphan` and created it again.
- Someone created the Secret by hand before the earlier version wrote a token into it for the first time, for example while the PostgreSQL URL could not be resolved yet. That Secret never got an owner.

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
