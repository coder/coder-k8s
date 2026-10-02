# Troubleshooting

Start with the operator logs. Most problems show there:

```bash
kubectl logs -n coder-system deploy/coder-k8s
```

## A component that you did not expect is running

`--app` is optional. Its default is `all`, which runs the controller and the aggregated API server. The MCP server never runs in `all` mode. To run only one component, set `--app` explicitly:

```bash
GOFLAGS=-mod=vendor go run . --app=controller   # or aggregated-apiserver, or mcp-http --mcp-token-file=<file>
```

If the value is unknown, the process stops at startup with `assertion failed: unsupported --app value ...`.

## `no matches for kind` when applying a `CoderControlPlane`

The CRDs are missing. Install them:

```bash
kubectl apply -f config/crd/bases/
kubectl get crd | grep coder.com
```

## The controller runs but nothing reconciles

1. Make sure that the Deployment and the RBAC from this repository exist:

    ```bash
    kubectl get deploy coder-k8s -n coder-system
    kubectl get clusterrole manager-role
    kubectl get clusterrolebinding coder-k8s
    ```

2. Examine the events and the status of the object:

    ```bash
    kubectl describe codercontrolplane <name> -n <namespace>
    ```

## `CoderControlPlane` stays `Pending`

Usual causes:

1. The control plane Deployment has no ready pods.
2. The operator bootstrap token is not ready yet.
3. `spec.licenseSecretRef` points to a license Secret that is missing or not valid.
4. The controller cannot resolve `spec.database.connectionSecretRef`. Look at the reason of the `DatabaseSecretResolved` condition. See [Connect an external PostgreSQL database](deploy-controller.md#connect-an-external-postgresql-database).

```bash
kubectl get codercontrolplane <name> -n <namespace> -o yaml
kubectl get deploy,svc -n <namespace>
```

## APIService is `False` / `Unavailable`

```bash
kubectl get svc coder-k8s-apiserver -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml
```

Do not install CRDs for the same resources (`coderworkspaces.aggregation.coder.com`, `codertemplates.aggregation.coder.com`). They conflict with the aggregated API.

## Proxied requests fail with 503 and an x509 error

Symptoms:

- `kubectl get` on `codertemplates` or `coderworkspaces`, or `kubectl get --raw /apis/aggregation.coder.com/v1alpha1`, fails with `Error from server (ServiceUnavailable): the server is currently unable to handle the request`.
- The kube-apiserver log shows `error trying to reach service: tls: failed to verify certificate: x509: certificate signed by unknown authority` for `v1alpha1.aggregation.coder.com`.

Cause: kube-apiserver cannot verify the certificate of the aggregated API server against the APIService `caBundle`. The APIService can still show `Available=True`, because the availability check of kube-apiserver does not verify the certificate. Thus `Available=True` does not exclude this cause. For how the `caBundle` is managed, see [How kube-apiserver trusts the server](deploy-aggregated-apiserver.md#how-kube-apiserver-trusts-the-server).

Make sure that this is the error. On kubeadm-based clusters (including Kind), kube-apiserver runs as a Pod. On managed clusters, look in the control-plane logs of your provider instead.

```bash
kubectl get --raw /apis/aggregation.coder.com/v1alpha1
kubectl -n kube-system logs -l component=kube-apiserver --since=10m --tail=-1 |
  grep v1alpha1.aggregation.coder.com | grep x509
```

Then compare the APIService `caBundle` with the CA that the server uses. The two commands must show the same value:

```bash
kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}'; echo
kubectl get apiservice v1alpha1.aggregation.coder.com -o jsonpath='{.spec.caBundle}'; echo
```

If the values are different, find the cause:

- The APIService is opted out, and its `caBundle` is old or wrong. In the output of `kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml`, `metadata.annotations` contains `coder.com/manage-ca-bundle: "false"`. Set the `caBundle` to the `ca.crt` of the Secret yourself, or give the field back to coder-k8s:

    ```bash
    kubectl annotate apiservice v1alpha1.aggregation.coder.com coder.com/manage-ca-bundle-
    ```

- The server does not have permission to update the APIService. The server log gives the name of the missing permission:

    ```bash
    kubectl -n coder-system logs deploy/coder-k8s | grep 'missing permission'
    kubectl auth can-i patch apiservices.apiregistration.k8s.io/v1alpha1.aggregation.coder.com \
      --as=system:serviceaccount:coder-system:coder-k8s
    ```

    In standalone mode, use `--as=system:serviceaccount:coder-system:coder-k8s-apiserver`. Apply the RBAC. The RBAC must give the permission to the ServiceAccount that the pod runs as. Standalone mode also needs `config/apiserver-standalone/apiservice-cabundle-binding.yaml`. The server tries again within about one minute:

    ```bash
    kubectl apply -f config/rbac/apiservice-cabundle-role.yaml
    ```

- A different tool writes the `caBundle`. Examples are the CA injector of cert-manager (a `cert-manager.io/inject-ca-from` annotation), or a GitOps tool that sets `caBundle` from Git. The value changes back again and again, and each time the server logs `Set the APIService caBundle` again. To find the other writer, look in the managed fields. coder-k8s writes as `coder-k8s-apiservice-cabundle`:

    ```bash
    kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml --show-managed-fields
    kubectl -n coder-system logs deploy/coder-k8s | grep -c 'Set the APIService caBundle'
    ```

    Keep one owner. Remove the `caBundle` injection of the other tool. Or [opt out](deploy-aggregated-apiserver.md#opt-out), and make that tool inject the `ca.crt` from `coder-k8s-apiserver-tls`. That is the CA of the certificate that the server serves.

If the values are the same, the most likely cause is a CA rotation that is not complete: old replicas still serve a certificate from the previous CA. Wait until the rollout is complete. Then restart all replicas that started before the Secret changed:

```bash
kubectl -n coder-system rollout status deployment/coder-k8s
kubectl -n coder-system rollout restart deployment/coder-k8s
```

For the full procedure and how long the failures usually last, see [Replace the CA](deploy-aggregated-apiserver.md#replace-the-ca).

## The pod exits with `configure delegated authentication`

The aggregated API server uses the Kubernetes API to check each caller. Without this access, it does not start.

- `... configmaps "extension-apiserver-authentication" is forbidden`: apply `config/rbac/authentication-reader-binding.yaml`. In standalone mode, apply `config/apiserver-standalone/authentication-reader-binding.yaml` instead. The binding must name the ServiceAccount that the pod runs as, for example after you install into a different namespace.
- `no Kubernetes configuration for delegated authentication and authorization` (outside a cluster): set `KUBECONFIG` to one kubeconfig file, or create `~/.kube/config`.
- `load kubeconfig ...` or `invalid kubeconfig ...`: the file in `KUBECONFIG` is missing or not complete. The server does not use a different configuration instead.

## The pod exits with `configure aggregated API server serving certificate`

The Secret `coder-k8s-apiserver-tls` exists, but the server cannot use it. The message gives the name of the field, for example `data["ca.key"] is missing or empty`. Correct the Secret. Or delete it, and the server generates a new CA when it starts again:

```bash
kubectl -n coder-system delete secret coder-k8s-apiserver-tls
kubectl -n coder-system rollout restart deployment/coder-k8s
```

In standalone mode, the server does not have permission to create the Secret. Thus, after you delete it, apply the placeholder again with `kubectl apply -f config/apiserver-standalone/serving-ca-secret.yaml` before the restart.

If the error does not name a field, the ServiceAccount does not have a permission on this Secret. The message tells you which permission:

- `get secret …`: the ServiceAccount cannot read the Secret. Give it `get` on `coder-k8s-apiserver-tls`.
- `create secret …`: the Secret does not exist, and the ServiceAccount cannot create Secrets. In standalone mode with `coder-k8s-apiserver`, this is expected until the placeholder exists. Apply `config/apiserver-standalone/serving-ca-secret.yaml`, and the pod recovers on its next restart. In other cases, give the ServiceAccount `create`, or create the empty placeholder that the message describes. The server fills the placeholder.
- `fill placeholder secret …` or `update secret …`: the ServiceAccount cannot update the Secret. It needs this permission to fill a placeholder or to renew the serving certificate. Give it `update` on `coder-k8s-apiserver-tls`.

## Aggregated requests fail with `401 Unauthorized` or `403 Forbidden`

- `401`: the request has no valid credential. A request that you send directly to port `6443` needs a Kubernetes bearer token. Requests without credentials can reach only `/healthz`, `/livez`, and `/readyz`. Use `kubectl`, which sends requests through kube-apiserver.
- `403`: the caller does not have RBAC for `aggregation.coder.com` in that namespace. To find out, run `kubectl auth can-i list codertemplates.aggregation.coder.com -n <namespace> --as=<user>`. Before you give this RBAC, know that it gives owner-equivalent access in Coder. See [How callers are checked](deploy-aggregated-apiserver.md#how-callers-are-checked).
- `500` that mentions `subjectaccessreviews`: the ServiceAccount of the server cannot create SubjectAccessReviews. Apply `config/rbac/auth-delegator-binding.yaml`. In standalone mode, apply `config/apiserver-standalone/auth-delegator-binding.yaml` instead.

## Aggregated reads return `ServiceUnavailable`

This section is about `ServiceUnavailable` errors from the aggregated API server itself. Their messages are specific. For example, they say that the server found no eligible `CoderControlPlane`, or that the configuration for standalone mode is missing. kube-apiserver returns the general message `the server is currently unable to handle the request` instead. For that error, see [Proxied requests fail with 503 and an x509 error](#proxied-requests-fail-with-503-and-an-x509-error).

- `all` mode: no eligible `CoderControlPlane` exists yet. A control plane is eligible when its operator access is enabled and ready, its status has an operator token reference and a URL, and its name does not contain a `.` character.
- Standalone mode (`--app=aggregated-apiserver`): set all three flags `--coder-url`, `--coder-session-token`, and `--coder-namespace`.

The logs show which provider configuration the server used.

## Aggregated reads return `multiple eligible CoderControlPlane ...`

The server expects one eligible control plane for each request scope. If more than one control plane is eligible, do one of these steps:

- Send the request to one namespace (`-n <namespace>`).
- Run a dedicated aggregated API server in standalone mode, pinned to one namespace with `--coder-namespace`. Standalone mode needs all three flags `--coder-url`, `--coder-session-token`, and `--coder-namespace`.

If that namespace has more than one eligible control plane, the first step does not help. Keep only one eligible control plane in that namespace, or use a dedicated aggregated API server.

## Aggregated requests return `400` or `409`

These errors often come from the rules for names or for `resourceVersion`. See [Aggregated API behavior](../reference/aggregated-api-behavior.md).
