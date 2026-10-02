# Deploy the aggregated API server

Serve `CoderWorkspace` and `CoderTemplate` (`aggregation.coder.com/v1alpha1`) through the Kubernetes API. Then you can use `kubectl` to manage Coder workspaces and templates.

Run the commands from a clone of this repository.

## 1. Register the API

```bash
kubectl create namespace coder-system
kubectl apply -f deploy/apiserver-service.yaml -f deploy/apiserver-apiservice.yaml
```

Each option in step 2 applies its own RBAC. Both options include two bindings that the aggregated API server needs to check callers:

- `auth-delegator-binding.yaml` lets the server create TokenReviews and SubjectAccessReviews.
- `authentication-reader-binding.yaml` lets the server read `kube-system/extension-apiserver-authentication`.

The bindings name the ServiceAccount in `coder-system`. If you install into a different namespace, edit them.

Without these permissions, the server fails closed:

- Without read access to the `kube-system/extension-apiserver-authentication` ConfigMap, the server does not start.
- Without permission to create SubjectAccessReviews, the server answers each request with an error. Members of `system:masters` are the exception.

## 2. Deploy

### Option A: all-in-one (recommended)

The default `--app=all` already includes the aggregated API server. The server finds its Coder backend automatically from an eligible `CoderControlPlane`. It runs as the `coder-k8s` ServiceAccount with `manager-role`, because the controller needs that role.

```bash
kubectl apply -f config/rbac/
kubectl apply -f deploy/deployment.yaml
```

### Option B: standalone

Run only the aggregated API server (`--app=aggregated-apiserver`), and configure the Coder URL and session token yourself.

The standalone server runs as its own ServiceAccount, `coder-k8s-apiserver`. It needs no permission from `manager-role`. `config/apiserver-standalone/` gives it only these permissions:

- `get` and `update` on the `coder-k8s-apiserver-tls` Secret. The directory contains this Secret as an empty placeholder, and the server fills it. There is no `create` permission. With `create` on Secrets in the namespace, this identity can create token Secrets for other ServiceAccounts in the namespace.
- Create TokenReviews and SubjectAccessReviews, and read `kube-system/extension-apiserver-authentication`.
- Its own APIService, through the ClusterRole in `config/rbac/apiservice-cabundle-role.yaml`. The binding for `coder-k8s` in that file is not used in standalone mode.

Apply the placeholder and the RBAC before the Deployment:

```bash
kubectl apply -f config/apiserver-standalone/ -f config/rbac/apiservice-cabundle-role.yaml
```

If the pod starts before the placeholder exists, the pod stops, because it does not have permission to create the Secret. Apply the placeholder. The pod recovers on its next restart. Kubernetes restarts it with a back-off of up to 5 minutes.

Put the Coder session token in a file, for example `./coder-session-token`. Put the token in a Secret and deploy. Then set the ServiceAccount and the backend:

```bash
kubectl -n coder-system create secret generic coder-k8s-session-token \
  --from-file=token=./coder-session-token

kubectl apply -f deploy/deployment.yaml

kubectl -n coder-system patch deployment coder-k8s --type=json -p '[{
  "op": "add",
  "path": "/spec/template/spec/serviceAccountName",
  "value": "coder-k8s-apiserver"
}, {
  "op": "add",
  "path": "/spec/template/spec/containers/0/env",
  "value": [{
    "name": "CODER_SESSION_TOKEN",
    "valueFrom": {"secretKeyRef": {"name": "coder-k8s-session-token", "key": "token"}}
  }]
}, {
  "op": "add",
  "path": "/spec/template/spec/containers/0/args",
  "value": [
    "--app=aggregated-apiserver",
    "--coder-url=https://coder.example.com",
    "--coder-session-token=$(CODER_SESSION_TOKEN)",
    "--coder-namespace=coder-system"
  ]
}]'
```

When Kubernetes starts the container, it replaces `$(CODER_SESSION_TOKEN)` with the value from the Secret. Thus the Deployment holds only a reference to the Secret, not the token. Keep the single quotes, so that your shell does not expand the variable.

Standalone mode serves health checks over HTTPS on port `6443`. Update the probes:

```bash
kubectl -n coder-system patch deployment coder-k8s --type=strategic -p '{
  "spec": {"template": {"spec": {"containers": [{
    "name": "coder-k8s",
    "livenessProbe":  {"httpGet": {"scheme": "HTTPS", "path": "/healthz", "port": 6443}},
    "readinessProbe": {"httpGet": {"scheme": "HTTPS", "path": "/readyz",  "port": 6443}}
  }]}}}
}'
```

#### Move a standalone server from `coder-k8s` to its own ServiceAccount

Earlier versions of this guide ran the standalone server as `coder-k8s`. To move it to `coder-k8s-apiserver`:

1. Apply the standalone RBAC and the placeholder. When you apply the placeholder over the existing Secret, the Secret keeps its data and CA.

    ```bash
    kubectl apply -f config/apiserver-standalone/ -f config/rbac/apiservice-cabundle-role.yaml
    ```

2. Change the ServiceAccount. The new pods use the existing Secret and CA again, so the APIService `caBundle` does not change.

    ```bash
    kubectl -n coder-system patch deployment coder-k8s --type=json \
      -p '[{"op": "replace", "path": "/spec/template/spec/serviceAccountName", "value": "coder-k8s-apiserver"}]'
    kubectl -n coder-system rollout status deployment/coder-k8s
    ```

3. If nothing else in this cluster runs as `coder-k8s`, delete the `config/rbac/` bindings for it.

Some tools replace the full Secret from the manifest, for example a GitOps sync that replaces objects instead of applying them. This clears the data of the Secret. The server then generates a new CA. Thus treat this as a [CA replacement](#replace-the-ca), and restart all replicas.

## How callers are checked

The aggregated API server uses the Kubernetes API to authenticate and authorize each request:

| Caller | Authentication | Authorization |
| --- | --- | --- |
| `kubectl` and other clients through kube-apiserver (the normal path) | The front-proxy client certificate of kube-apiserver, verified against `requestheader-client-ca-file` from `kube-system/extension-apiserver-authentication`. The user comes from the `X-Remote-*` headers. | SubjectAccessReview for that user, verb, resource, and namespace |
| Direct requests to port `6443` with a bearer token | TokenReview | SubjectAccessReview |
| Direct requests with a client certificate signed by the cluster client CA | Certificate subject | SubjectAccessReview |
| All other requests | None | Rejected with `401`, except exact `/healthz`, `/livez`, `/readyz` |

The server trusts `X-Remote-*` headers only on connections that present a valid front-proxy client certificate. Kubernetes RBAC on `aggregation.coder.com` (`codertemplates`, `coderworkspaces`) controls access to the resources. Read the warning that follows before you give this RBAC.

!!! warning "RBAC on these resources is owner access in Coder"
    Treat Kubernetes RBAC on `codertemplates` and `coderworkspaces` as owner-equivalent access in Coder. Give it only to subjects that you trust as Coder owners. Use namespaced Roles, not ClusterRoles, when you can.

The server does not map Kubernetes users to Coder users. After Kubernetes RBAC allows a request, the server calls Coder with the operator token of the control plane. This token has owner rights in Coder. Each request uses only the control plane that serves the namespace of the request. In standalone mode, the server is pinned to `--coder-namespace`.

Thus a subject with this RBAC in a namespace gets these rights in the Coder deployment of that namespace:

- A subject that can create, update, patch, or delete `codertemplates` or `coderworkspaces` acts as an owner.
- A subject that can `get`, `list`, or `watch` these resources sees the templates (including their source files) and workspaces as an owner sees them.

A ClusterRole binding gives this access for each namespace that has a control plane.

The server keeps these Kubernetes defaults:

- Members of `system:masters` are authorized without a SubjectAccessReview, as in kube-apiserver.
- The server caches TokenReview results for 10 seconds, allowed SubjectAccessReview results for 10 seconds, and denied results for 10 seconds. A permission change can take effect only after the related cache entry expires.
- The server reads `extension-apiserver-authentication` at startup, and continues to watch it. If the ConfigMap is deleted later, the server continues to trust the CA that it loaded last (retained trust), until it restarts or sees a new CA. If the ConfigMap or its request-header CA is missing at startup, front-proxy requests fail with `401`.

The server uses the same Kubernetes API for all of these checks. It uses `KUBECONFIG` if it is set (exactly one file, never a fallback). If not, it uses the in-cluster ServiceAccount. If there is none, it uses `~/.kube/config`. If none of these is available, the server does not start.

## 3. Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A
```

If a step fails, examine `kubectl logs -n coder-system deploy/coder-k8s` and see [Troubleshooting](troubleshooting.md).

## Next

Coder, not etcd, stores these resources. Thus some Kubernetes behavior is different. Read [Aggregated API behavior](../reference/aggregated-api-behavior.md) before you write manifests. The most important rule: object names must use the canonical names of Coder.

## Serving certificate

In a cluster, the aggregated API server serves a certificate that its own CA signed. The certificate and the CA are in the Secret `coder-k8s-apiserver-tls` in the namespace of the server. The Secret has the type `coder.com/aggregated-apiserver-serving-ca` and the label `app.kubernetes.io/component: aggregated-apiserver-serving-ca`. The certificate is valid for `coder-k8s-apiserver`, `coder-k8s-apiserver.<namespace>`, `coder-k8s-apiserver.<namespace>.svc`, and `coder-k8s-apiserver.<namespace>.svc.cluster.local`.

- With `--app=all`, the server creates the Secret when it starts for the first time. In standalone mode, `config/apiserver-standalone/` contains the Secret as an empty placeholder, and the server fills it. After that, the server uses the same Secret again. If there are several replicas, they all use the same Secret.
- An empty placeholder has the type `coder.com/aggregated-apiserver-serving-ca`, no `data` keys at all, and is not `immutable`. If the Secret already exists as an empty placeholder, the server fills it with a new CA. It does not create the Secret, so it does not need `create` on Secrets. If the Secret does not exist and the server does not have permission to create Secrets, the server does not start, and the log tells you to create the placeholder.
- The serving certificate is valid for 1 year. The server checks it at startup and every 12 hours. When less than one third of its lifetime is left, the server renews it with the same CA. The server serves the new certificate without a restart.
- The CA is valid for 10 years. To replace it earlier (for example, after the Secret was exposed), see [Replace the CA](#replace-the-ca).
- The Secret can exist but be unusable, and not be an empty placeholder. Examples are a missing key, PEM data that cannot be parsed, a key that does not match its certificate, a serving certificate that the CA did not sign, or an expired CA. Then the server does not start, and the log gives the name of the field. Correct the Secret or delete it. In standalone mode, apply the placeholder again after you delete the Secret.
- Outside a cluster (for example with `go run`), the server serves a self-signed certificate for `localhost` instead.

!!! warning "The Secret holds the CA private key"
    A subject that can read this Secret can issue certificates that kube-apiserver accepts for the aggregated API server. This includes readers outside `coder-system`. See [Who can read the CA key](#who-can-read-the-ca-key).

### Who can read the CA key

A subject that has the CA key, and that can also redirect the traffic of the `coder-k8s-apiserver` Service, can act as the aggregated API server. That subject gets the requests that kube-apiserver sends to the server, and can answer them.

These identities can read the key:

- All subjects that can `get`, `list`, or `watch` Secrets in the namespace of the server (`coder-system` by default).
- All subjects that can read Secrets in all namespaces. It is easy to forget these: cluster administrators, and GitOps, backup, or monitoring tools with access to Secrets in all namespaces.
- The `coder-k8s` ServiceAccount. Its `manager-role` allows all verbs on Secrets in all namespaces, because the controller manages Secrets for each `CoderControlPlane`.
- The `coder-k8s-apiserver` ServiceAccount, for this one Secret only.

For each type of deployment:

| Deployment | Runs as | Can read the CA key |
| --- | --- | --- |
| `--app=all` ([Option A](#option-a-all-in-one-recommended)) | `coder-k8s` | Yes. Also, the process keeps all Secrets in the cluster in its cache, because the controllers watch Secrets. |
| Standalone `--app=aggregated-apiserver` ([Option B](#option-b-standalone)) | `coder-k8s-apiserver`, with `config/apiserver-standalone/` | Yes, and no other Secret. It can `get` and `update` only `coder-k8s-apiserver-tls`. It cannot list, watch, or create Secrets. If `coder-k8s` is installed with its `manager-role` binding, `coder-k8s` can also read the key. |
| Controller only (`dist/install.yaml`) | `coder-k8s` | Only if an aggregated API server created the Secret in a namespace of the cluster. This bundle never creates it. |

For `--app=all`, a narrower Role does not change this. One process runs both the controller and the aggregated API server with one ServiceAccount, and the controller needs access to Secrets in all namespaces. The same access also covers the operator token Secrets. These give owner rights in Coder, and are more sensitive than the CA key. Limit who can read Secrets in `coder-system` and in all namespaces. If the Secret was possibly exposed, [replace the CA](#replace-the-ca).

### Replace the CA

To replace the CA, you must restart all replicas. While this occurs, requests through kube-apiserver fail with `503 ServiceUnavailable` for several seconds (about 10 seconds with two replicas in testing). Plan it as a short maintenance window.

1. Write down the fingerprint of the current CA, so that you can identify the new CA:

    ```bash
    kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}' |
      base64 -d | openssl x509 -noout -fingerprint -sha256
    ```

2. Remove the old CA, and restart all replicas. Do both: a running replica continues to serve the old certificate until it restarts.

    With `--app=all`, delete the Secret. The server creates a new one:

    ```bash
    kubectl -n coder-system delete secret coder-k8s-apiserver-tls
    kubectl -n coder-system rollout restart deployment/coder-k8s
    kubectl -n coder-system rollout status deployment/coder-k8s
    ```

    In standalone mode, the server does not have permission to create the Secret. Instead, clear its data, which makes it an empty placeholder again. Or delete it, and apply `config/apiserver-standalone/serving-ca-secret.yaml` again:

    ```bash
    kubectl -n coder-system patch secret coder-k8s-apiserver-tls --type=json -p '[{"op": "remove", "path": "/data"}]'
    kubectl -n coder-system rollout restart deployment/coder-k8s
    kubectl -n coder-system rollout status deployment/coder-k8s
    ```

    The first new replica generates a new CA and sets the APIService `caBundle` to it. When kube-apiserver sends a request to an old replica, the verification fails until that replica stops. This is the `503` window.

3. Examine the result. The fingerprint is different from step 1. The APIService `caBundle` is the same as the `ca.crt` of the Secret (the two commands show the same value). A request through kube-apiserver succeeds:

    ```bash
    kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}' |
      base64 -d | openssl x509 -noout -fingerprint -sha256
    kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}'; echo
    kubectl get apiservice v1alpha1.aggregation.coder.com -o jsonpath='{.spec.caBundle}'; echo
    kubectl get --raw /apis/aggregation.coder.com/v1alpha1
    ```

If the APIService is [opted out](#opt-out), the server does not update the `caBundle`. Set it to the new `ca.crt` yourself, or requests continue to fail with `503`. Give the new CA to clients outside kube-apiserver that pinned the old CA. If requests still fail after the rollout, see [Proxied requests fail with 503 and an x509 error](troubleshooting.md#proxied-requests-fail-with-503-and-an-x509-error).

## How kube-apiserver trusts the server

kube-apiserver verifies the certificate of the aggregated API server against the APIService `spec.caBundle`. The aggregated API server keeps that field set to the CA in `coder-k8s-apiserver-tls`, and keeps `insecureSkipTLSVerify` off:

- It patches only the APIService `v1alpha1.aggregation.coder.com`. It patches only when the `caBundle` is different from the CA of the Secret, or when `insecureSkipTLSVerify` is set. It reacts to changes of that APIService and of its own CA. It does not poll.
- It writes only a CA that passed the same checks as at startup. If the Secret is missing or not valid, it does not change the APIService, and it logs the cause.
- It needs `config/rbac/apiservice-cabundle-role.yaml`: `get`, `list`, `watch`, and `patch` on that one APIService (`resourceNames`). `list` and `watch` are allowed only for requests that select the APIService by name. The controller-only install bundle (`dist/install.yaml`) does not include this file, because the bundle registers no APIService.
- Without that permission, the aggregated API server continues to serve. It logs the missing permission (at most one time every 5 minutes), and tries again with a backoff of up to 60 seconds.
- On a new install, requests through kube-apiserver fail with `503 ServiceUnavailable` for a few seconds, until the first patch. `Available=True` alone does not show that verification works, because the availability check of kube-apiserver does not verify the certificate. Make a real request instead: `kubectl get --raw /apis/aggregation.coder.com/v1alpha1`.
- If you run `kubectl apply -f deploy/apiserver-apiservice.yaml` again, the injected `caBundle` stays. If you replace or create the APIService again, the `caBundle` is cleared. The aggregated API server sets it again within seconds.

If requests through kube-apiserver fail with `503` while the APIService shows `Available=True`, see [Proxied requests fail with 503 and an x509 error](troubleshooting.md#proxied-requests-fail-with-503-and-an-x509-error).

### Upgrade from a version that used `insecureSkipTLSVerify`

Apply the changes in this sequence:

1. `kubectl apply -f config/rbac/`
2. Deploy the new image. It sets `caBundle` and turns `insecureSkipTLSVerify` off in one step.
3. `kubectl apply -f deploy/apiserver-apiservice.yaml` (the file no longer sets `insecureSkipTLSVerify`).

If the new image starts before step 1, it logs a missing-permission error until the RBAC exists. During that time, the APIService continues to work without verification. If you apply step 3 before the new image runs, requests through kube-apiserver fail with `503` until the new image starts.

Do not apply an old copy of `deploy/apiserver-apiservice.yaml` again. After a `caBundle` is set, the API rejects `insecureSkipTLSVerify: true`.

To roll back to a version without a managed serving certificate, restore the old registration before you change the image. The opt-out annotation stops the running server from setting the `caBundle` again:

```bash
kubectl annotate apiservice v1alpha1.aggregation.coder.com coder.com/manage-ca-bundle=false
kubectl patch apiservice v1alpha1.aggregation.coder.com --type=merge \
  -p '{"spec":{"caBundle":null,"insecureSkipTLSVerify":true}}'
```

Then deploy the old image, and apply its `deploy/apiserver-apiservice.yaml` again.

### Opt out

A different tool can own the APIService `caBundle`, for example the CA injector of cert-manager, or a GitOps tool that sets it from Git. In that case, add this annotation to the APIService, so that the aggregated API server does not change the field:

```bash
kubectl annotate apiservice v1alpha1.aggregation.coder.com coder.com/manage-ca-bundle=false
```

That tool must then trust a CA that signed the certificate that the server serves. This is the CA in `coder-k8s-apiserver-tls`. To give the field back to the server, remove the annotation, or set it to a value other than `false`.
