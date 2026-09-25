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

!!! warning "TLS"
    `deploy/apiserver-apiservice.yaml` sets `insecureSkipTLSVerify: true` for development. Use CA-backed TLS in any real environment.
