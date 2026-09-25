# Troubleshooting

Start with the operator logs; most problems show up there:

```bash
kubectl logs -n coder-system deploy/coder-k8s
```

## A component I didn't expect is running

`--app` is optional and defaults to `all` (controller and aggregated API server; the MCP server never runs in `all`). To isolate one component, set it explicitly:

```bash
GOFLAGS=-mod=vendor go run . --app=controller   # or aggregated-apiserver, or mcp-http --mcp-token-file=<file>
```

An unknown value fails at startup with `assertion failed: unsupported --app value ...`.

## `no matches for kind` when applying a `CoderControlPlane`

The CRDs are missing. Install them:

```bash
kubectl apply -f config/crd/bases/
kubectl get crd | grep coder.com
```

## The controller runs but nothing reconciles

Check that the shipped Deployment and RBAC exist:

```bash
kubectl get deploy coder-k8s -n coder-system
kubectl get clusterrole manager-role
kubectl get clusterrolebinding coder-k8s
```

Then inspect the object's events and status:

```bash
kubectl describe codercontrolplane <name> -n <namespace>
```

## `CoderControlPlane` stays `Pending`

Common causes:

1. The control plane Deployment has no ready pods.
2. The operator bootstrap token is not ready yet.
3. `spec.licenseSecretRef` points to a missing or invalid license Secret.
4. `spec.database.connectionSecretRef` cannot be resolved. Check the `DatabaseSecretResolved` condition reason; see [Connect an external PostgreSQL database](deploy-controller.md#connect-an-external-postgresql-database).

```bash
kubectl get codercontrolplane <name> -n <namespace> -o yaml
kubectl get deploy,svc -n <namespace>
```

## APIService is `False` / `Unavailable`

```bash
kubectl get svc coder-k8s-apiserver -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml
```

Do not install CRDs for the same resources (`coderworkspaces.aggregation.coder.com`, `codertemplates.aggregation.coder.com`); they conflict with the aggregated API.

## Proxied requests fail with 503 and an x509 error

Symptoms: `kubectl get` on `codertemplates` or `coderworkspaces`, or `kubectl get --raw /apis/aggregation.coder.com/v1alpha1`, fails with `Error from server (ServiceUnavailable): the server is currently unable to handle the request`. The kube-apiserver log shows `error trying to reach service: tls: failed to verify certificate: x509: certificate signed by unknown authority` for `v1alpha1.aggregation.coder.com`.

kube-apiserver cannot verify the aggregated API server's certificate against the APIService `caBundle`. The APIService can still report `Available=True`: kube-apiserver's availability check does not verify the certificate, so it does not rule this out. For how the `caBundle` is managed, see [How kube-apiserver trusts the server](deploy-aggregated-apiserver.md#how-kube-apiserver-trusts-the-server).

Confirm the error. On kubeadm-based clusters (including Kind), kube-apiserver runs as a Pod; on managed clusters, look in your provider's control-plane logs instead.

```bash
kubectl get --raw /apis/aggregation.coder.com/v1alpha1
kubectl -n kube-system logs -l component=kube-apiserver --since=10m --tail=-1 |
  grep v1alpha1.aggregation.coder.com | grep x509
```

Then compare the APIService `caBundle` with the CA the server uses. The two commands must print the same value:

```bash
kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}'; echo
kubectl get apiservice v1alpha1.aggregation.coder.com -o jsonpath='{.spec.caBundle}'; echo
```

If they differ, find the cause:

- **The APIService is opted out** and its `caBundle` is stale or wrong. `kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml` shows `coder.com/manage-ca-bundle: "false"` under `metadata.annotations`. Either set the `caBundle` to the Secret's `ca.crt` yourself, or hand the field back to coder-k8s:

    ```bash
    kubectl annotate apiservice v1alpha1.aggregation.coder.com coder.com/manage-ca-bundle-
    ```

- **The server may not update the APIService.** The server log names the missing permission:

    ```bash
    kubectl -n coder-system logs deploy/coder-k8s | grep 'missing permission'
    kubectl auth can-i patch apiservices.apiregistration.k8s.io/v1alpha1.aggregation.coder.com \
      --as=system:serviceaccount:coder-system:coder-k8s
    ```

    In standalone mode, use `--as=system:serviceaccount:coder-system:coder-k8s-apiserver`. Apply the RBAC (it must name the ServiceAccount the pod runs as; standalone mode also needs `config/apiserver-standalone/apiservice-cabundle-binding.yaml`); the server retries within about a minute:

    ```bash
    kubectl apply -f config/rbac/apiservice-cabundle-role.yaml
    ```

- **Another tool writes the `caBundle`,** for example cert-manager's CA injector (a `cert-manager.io/inject-ca-from` annotation) or a GitOps tool that sets `caBundle` from Git. The value keeps changing back, and the server logs `Set the APIService caBundle` again each time. Look for the other writer in the managed fields; coder-k8s writes as `coder-k8s-apiservice-cabundle`:

    ```bash
    kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml --show-managed-fields
    kubectl -n coder-system logs deploy/coder-k8s | grep -c 'Set the APIService caBundle'
    ```

    Keep one owner. Remove the other tool's `caBundle` injection, or [opt out](deploy-aggregated-apiserver.md#opt-out) and make that tool inject the `ca.crt` from `coder-k8s-apiserver-tls` (the certificate the server actually serves).

If they match, you are most likely inside a **CA rotation window**: old replicas still serve a certificate from the previous CA. Wait for the rollout to finish and restart any replica that still runs from before the Secret changed:

```bash
kubectl -n coder-system rollout status deployment/coder-k8s
kubectl -n coder-system rollout restart deployment/coder-k8s
```

See [Replace the CA](deploy-aggregated-apiserver.md#replace-the-ca) for the full procedure and the expected window.

## The pod exits with `configure delegated authentication`

The aggregated API server checks every caller with the Kubernetes API and refuses to start without it.

- `... configmaps "extension-apiserver-authentication" is forbidden`: apply `config/rbac/authentication-reader-binding.yaml` (`config/apiserver-standalone/authentication-reader-binding.yaml` in standalone mode). It must name the ServiceAccount the pod runs as (for example after installing into another namespace).
- `no Kubernetes configuration for delegated authentication and authorization` (outside a cluster): set `KUBECONFIG` to one kubeconfig file, or create `~/.kube/config`.
- `load kubeconfig ...` or `invalid kubeconfig ...`: the file named by `KUBECONFIG` is missing or incomplete. The server does not fall back to another configuration.

## The pod exits with `configure aggregated API server serving certificate`

The Secret `coder-k8s-apiserver-tls` exists but cannot be used; the message names the field (for example `data["ca.key"] is missing or empty`). Fix the Secret, or delete it so the server generates a new CA on its next start:

```bash
kubectl -n coder-system delete secret coder-k8s-apiserver-tls
kubectl -n coder-system rollout restart deployment/coder-k8s
```

In standalone mode the server may not create the Secret: after deleting it, re-apply the placeholder with `kubectl apply -f config/apiserver-standalone/serving-ca-secret.yaml` before the restart.

An error without a field name means the ServiceAccount is missing a permission on this Secret. The message says which:

- `get secret …`: it may not read the Secret. Grant `get` on `coder-k8s-apiserver-tls`.
- `create secret …`: the Secret does not exist and the ServiceAccount may not create Secrets. In standalone mode with `coder-k8s-apiserver`, this is expected until the placeholder exists: apply `config/apiserver-standalone/serving-ca-secret.yaml`, and the pod recovers on its next restart. Otherwise grant `create`, or create the empty placeholder that the message describes (the server fills it).
- `fill placeholder secret …` or `update secret …`: the ServiceAccount may not update the Secret, to fill a placeholder or to renew the serving certificate. Grant `update` on `coder-k8s-apiserver-tls`.

## Aggregated requests fail with `401 Unauthorized` or `403 Forbidden`

- **`401`:** the request has no valid credential. Requests sent straight to port `6443` need a Kubernetes bearer token; anonymous requests only reach `/healthz`, `/livez`, and `/readyz`. Use `kubectl`, which goes through kube-apiserver.
- **`403`:** the caller lacks RBAC for `aggregation.coder.com` in that namespace. Check with `kubectl auth can-i list codertemplates.aggregation.coder.com -n <namespace> --as=<user>`. Before you grant it, note that this RBAC is owner-equivalent inside Coder (see [How callers are checked](deploy-aggregated-apiserver.md#how-callers-are-checked)).
- **`500` mentioning `subjectaccessreviews`:** the server's ServiceAccount cannot create SubjectAccessReviews. Apply `config/rbac/auth-delegator-binding.yaml` (`config/apiserver-standalone/auth-delegator-binding.yaml` in standalone mode).

## Aggregated reads return `ServiceUnavailable`

This section covers `ServiceUnavailable` errors that come from the aggregated API server itself. Their messages are specific, for example that no eligible `CoderControlPlane` was found, or that standalone mode is missing configuration. kube-apiserver instead returns the generic `the server is currently unable to handle the request`; for that error, see [Proxied requests fail with 503 and an x509 error](#proxied-requests-fail-with-503-and-an-x509-error).

- **`all` mode:** no eligible `CoderControlPlane` exists yet, or its operator access is not ready.
- **Standalone mode (`--app=aggregated-apiserver`):** set all three flags: `--coder-url`, `--coder-session-token`, and `--coder-namespace`.

The logs show which provider configuration was used.

## Aggregated reads return `multiple eligible CoderControlPlane ...`

The server expects one eligible control plane per request scope. With several ready control planes, either:

- Query one namespace (`-n <namespace>`), or
- Run a dedicated aggregated API server pinned with `--coder-namespace`.

## Aggregated requests return `400` or `409`

These often come from the naming or `resourceVersion` rules. See [Aggregated API behavior](../reference/aggregated-api-behavior.md).
