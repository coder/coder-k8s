# Deploy the aggregated API server

Serve `CoderWorkspace` and `CoderTemplate` (`aggregation.coder.com/v1alpha1`) through the Kubernetes API, so you can manage Coder workspaces and templates with `kubectl`.

Commands run from a clone of this repository.

## 1. Apply RBAC and register the API

```bash
kubectl create namespace coder-system
kubectl apply -f config/rbac/
kubectl apply -f deploy/apiserver-service.yaml -f deploy/apiserver-apiservice.yaml
```

`config/rbac/` includes two bindings the aggregated API server needs to check callers: `auth-delegator-binding.yaml` (create TokenReviews and SubjectAccessReviews) and `authentication-reader-binding.yaml` (read `kube-system/extension-apiserver-authentication`; the default `manager-role` also grants cluster-wide ConfigMap reads). Both name the `coder-k8s` ServiceAccount in `coder-system`; edit them if you install elsewhere. The server fails closed without these permissions: without read access to that ConfigMap it does not start, and without permission to create SubjectAccessReviews it answers every request with an error (members of `system:masters` excepted).

## 2. Deploy

### Option A: all-in-one (recommended)

The default `--app=all` already includes the aggregated API server. It finds its Coder backend automatically from an eligible `CoderControlPlane`.

```bash
kubectl apply -f deploy/deployment.yaml
```

### Option B: standalone

Run only the aggregated API server (`--app=aggregated-apiserver`) and point it at a Coder instance yourself.

Save the Coder session token in a file, for example `./coder-session-token`. Store it in a Secret, deploy, then set the backend:

```bash
kubectl -n coder-system create secret generic coder-k8s-session-token \
  --from-file=token=./coder-session-token

kubectl apply -f deploy/deployment.yaml

kubectl -n coder-system patch deployment coder-k8s --type=json -p '[{
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

Kubernetes replaces `$(CODER_SESSION_TOKEN)` with the value from the Secret when it starts the container, so the Deployment holds only a reference to the Secret, not the token. Keep the single quotes so that your shell does not expand it.

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

## How callers are checked

The aggregated API server authenticates and authorizes every request with the Kubernetes API:

| Caller | Authentication | Authorization |
| --- | --- | --- |
| `kubectl` and other clients through kube-apiserver (the normal path) | kube-apiserver's front-proxy client certificate, verified against `requestheader-client-ca-file` from `kube-system/extension-apiserver-authentication`; the user comes from the `X-Remote-*` headers | SubjectAccessReview for that user, verb, resource, and namespace |
| Direct requests to port `6443` with a bearer token | TokenReview | SubjectAccessReview |
| Direct requests with a client certificate signed by the cluster client CA | Certificate subject | SubjectAccessReview |
| Anything else | None | Rejected with `401`, except exact `/healthz`, `/livez`, `/readyz` |

`X-Remote-*` headers are trusted only on connections that present a valid front-proxy client certificate. Access to the resources is controlled with Kubernetes RBAC on `aggregation.coder.com` (`codertemplates`, `coderworkspaces`), but read the warning below before you grant it.

!!! warning "RBAC on these resources is owner access in Coder"
    Treat Kubernetes RBAC on `codertemplates` and `coderworkspaces` as owner-equivalent inside Coder. Grant it only to subjects you would trust as Coder owners, and prefer namespaced Roles over ClusterRoles.

The server does not map Kubernetes users to Coder users. After Kubernetes RBAC allows a request, the server calls Coder with the control plane's operator token, which has owner rights in Coder. Each request uses only the control plane that serves the request's namespace (in standalone mode, the server is pinned to `--coder-namespace`).

So a subject allowed to create, update, patch, or delete `codertemplates` or `coderworkspaces` in a namespace acts in that Coder deployment as an owner, and `get`, `list`, or `watch` shows that deployment's templates (including their source files) and workspaces as an owner sees them. A ClusterRole binding grants this for every namespace that has a control plane.

Kept Kubernetes defaults, so you know what to expect:

- Members of `system:masters` are authorized without a SubjectAccessReview, as in kube-apiserver.
- TokenReview results are cached for 10 seconds, allowed SubjectAccessReview results for 10 seconds, and denied results for 10 seconds. A permission change can take effect only after the matching cache entry expires.
- The server reads `extension-apiserver-authentication` at startup and keeps watching it. If the ConfigMap is deleted later, the server keeps trusting the CA it last loaded (retained trust) until it restarts or sees a new one. If the ConfigMap or its request-header CA is missing at startup, front-proxy requests fail with `401`.

The server uses the same Kubernetes API for all of these checks: `KUBECONFIG` if set (exactly one file, never a fallback), otherwise the in-cluster ServiceAccount, otherwise `~/.kube/config`. With none, it does not start.

## 3. Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A
```

If something fails, check `kubectl logs -n coder-system deploy/coder-k8s` and [Troubleshooting](troubleshooting.md).

## Next

These resources are backed by Coder, not etcd, so some Kubernetes behavior differs. Read [Aggregated API behavior](../reference/aggregated-api-behavior.md) before you write manifests. The most important rule: object names must use Coder's canonical names.

## Serving certificate

In a cluster, the aggregated API server serves a certificate signed by its own CA. Both live in the Secret `coder-k8s-apiserver-tls` in the server's namespace (type `coder.com/aggregated-apiserver-serving-ca`, label `app.kubernetes.io/component: aggregated-apiserver-serving-ca`). The certificate is valid for `coder-k8s-apiserver`, `coder-k8s-apiserver.<namespace>`, `coder-k8s-apiserver.<namespace>.svc`, and `coder-k8s-apiserver.<namespace>.svc.cluster.local`.

- The server creates the Secret on first start and reuses it afterwards. With several replicas, they all use the same Secret.
- The serving certificate is valid for 1 year. The server checks it at startup and every 12 hours, and renews it with the same CA when less than a third of its lifetime is left. The new certificate is served without a restart.
- The CA is valid for 10 years. To replace it earlier (for example after the Secret was exposed), see [Replace the CA](#replace-the-ca).
- If the Secret exists but is unusable (a missing key, unparsable PEM, a key that does not match its certificate, a serving certificate not signed by the CA, or an expired CA), the server does not start and the log names the field. Fix the Secret or delete it.
- Outside a cluster (for example `go run`), the server serves a self-signed certificate for `localhost` instead.

!!! warning "The Secret holds the CA private key"
    Anyone who can read Secrets in the server's namespace can issue certificates that the aggregated API server's CA vouches for. Restrict Secret read access in `coder-system` accordingly.

### Replace the CA

Replacing the CA takes a restart of every replica, and requests through kube-apiserver fail with `503 ServiceUnavailable` for several seconds while it happens (about 10 seconds with two replicas in testing). Clients retry, so plan it like a short maintenance window.

1. Note the current CA fingerprint, so you can tell the new one apart:

    ```bash
    kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}' |
      base64 -d | openssl x509 -noout -fingerprint -sha256
    ```

2. Delete the Secret and restart every replica. Do both: a running replica keeps serving the old certificate until it restarts.

    ```bash
    kubectl -n coder-system delete secret coder-k8s-apiserver-tls
    kubectl -n coder-system rollout restart deployment/coder-k8s
    kubectl -n coder-system rollout status deployment/coder-k8s
    ```

    The first new replica generates a new CA and sets the APIService `caBundle` to it. Requests that kube-apiserver sends to an old replica fail verification until that replica is gone; that is the `503` window.

3. Check the result. The fingerprint differs from step 1, the APIService `caBundle` equals the Secret's `ca.crt` (the two commands print the same value), and a request through kube-apiserver succeeds:

    ```bash
    kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}' |
      base64 -d | openssl x509 -noout -fingerprint -sha256
    kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}'; echo
    kubectl get apiservice v1alpha1.aggregation.coder.com -o jsonpath='{.spec.caBundle}'; echo
    kubectl get --raw /apis/aggregation.coder.com/v1alpha1
    ```

If the APIService is [opted out](#opt-out), the server does not update the `caBundle`: set it to the new `ca.crt` yourself, or requests keep failing with `503`. Clients outside kube-apiserver that pinned the old CA must be given the new one. If requests still fail after the rollout, see [Proxied requests fail with 503 and an x509 error](troubleshooting.md#proxied-requests-fail-with-503-and-an-x509-error).

## How kube-apiserver trusts the server

kube-apiserver verifies the aggregated API server's certificate against the APIService `spec.caBundle`. The aggregated API server keeps that field set to the CA in `coder-k8s-apiserver-tls`, and keeps `insecureSkipTLSVerify` off:

- It patches only the APIService `v1alpha1.aggregation.coder.com`, and only when the `caBundle` differs from the Secret's CA or `insecureSkipTLSVerify` is set. It reacts to changes of that APIService and of its own CA; it does not poll.
- It writes only a CA that passed the same checks as at startup. If the Secret is missing or invalid, it leaves the APIService unchanged and logs why.
- It needs `config/rbac/apiservice-cabundle-role.yaml`: `get`, `list`, `watch`, and `patch` on that one APIService (`resourceNames`). `list` and `watch` are allowed only for requests that select it by name. The controller-only install bundle (`dist/install.yaml`) does not include this file, because it registers no APIService.
- Without that permission the aggregated API server keeps serving, logs the missing permission (at most every 5 minutes), and retries with backoff up to 60 seconds.
- On a fresh install, requests through kube-apiserver fail with `503 ServiceUnavailable` for a few seconds, until the first patch. `Available=True` alone does not show that verification works, because kube-apiserver's availability check does not verify the certificate. Check a real request instead: `kubectl get --raw /apis/aggregation.coder.com/v1alpha1`.
- Running `kubectl apply -f deploy/apiserver-apiservice.yaml` again keeps the injected `caBundle`. Replacing or re-creating the APIService clears it; the aggregated API server sets it again within seconds.

If requests through kube-apiserver fail with `503` while the APIService reports `Available=True`, see [Proxied requests fail with 503 and an x509 error](troubleshooting.md#proxied-requests-fail-with-503-and-an-x509-error).

### Upgrade from a version that used `insecureSkipTLSVerify`

Apply the changes in this order:

1. `kubectl apply -f config/rbac/`
2. Deploy the new image. It sets `caBundle` and turns `insecureSkipTLSVerify` off in one step.
3. `kubectl apply -f deploy/apiserver-apiservice.yaml` (the file no longer sets `insecureSkipTLSVerify`).

If the new image starts before step 1, it logs a missing-permission error until the RBAC exists, and the APIService keeps working without verification in the meantime. If you apply step 3 before the new image runs, requests through kube-apiserver fail with `503` until the new image starts. Do not re-apply an old copy of `deploy/apiserver-apiservice.yaml`: once a `caBundle` is set, the API rejects `insecureSkipTLSVerify: true`.

To roll back to a version without a managed serving certificate, restore the old registration before you change the image. The opt-out annotation stops the running server from setting the `caBundle` again:

```bash
kubectl annotate apiservice v1alpha1.aggregation.coder.com coder.com/manage-ca-bundle=false
kubectl patch apiservice v1alpha1.aggregation.coder.com --type=merge \
  -p '{"spec":{"caBundle":null,"insecureSkipTLSVerify":true}}'
```

Then deploy the old image and re-apply its `deploy/apiserver-apiservice.yaml`.

### Opt out

If another tool owns the APIService `caBundle` (for example cert-manager's CA injector, or a GitOps tool that sets it from Git), annotate the APIService so the aggregated API server leaves it alone:

```bash
kubectl annotate apiservice v1alpha1.aggregation.coder.com coder.com/manage-ca-bundle=false
```

That tool must then trust a CA that signed the certificate the server actually serves, which is the one in `coder-k8s-apiserver-tls`. Remove the annotation (or set any other value) to hand the field back.
