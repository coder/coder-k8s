# Deploy the aggregated API server (in-cluster)

This guide deploys the aggregated API server for:

- API group: `aggregation.coder.com`
- Version: `v1alpha1`
- Resources: `coderworkspaces`, `codertemplates`

## 1) Create namespace and RBAC

```bash
kubectl create namespace coder-system
kubectl apply -f config/rbac/
```

## 2) Apply service and APIService manifests

```bash
kubectl apply -f deploy/apiserver-service.yaml
kubectl apply -f deploy/apiserver-apiservice.yaml
```

## 3) Choose a deployment model

### Option A: all-in-one mode (recommended for most dev/test setups)

`deploy/deployment.yaml` defaults to `--app=all`, which includes the aggregated API server.

```bash
kubectl apply -f deploy/deployment.yaml
```

In this mode, backend Coder client configuration is discovered dynamically from eligible `CoderControlPlane` resources.

### Option B: standalone aggregated API mode (`--app=aggregated-apiserver`)

Use this when you want a split deployment and explicit backend configuration.

1. Apply deployment manifest:

```bash
kubectl apply -f deploy/deployment.yaml
```

1. Configure required args:

```bash
CODER_URL="https://coder.example.com"
CODER_SESSION_TOKEN="replace-me"
CODER_NAMESPACE="coder-system"

kubectl -n coder-system set args deployment/coder-k8s --containers=coder-k8s -- \
  --app=aggregated-apiserver \
  --coder-url="${CODER_URL}" \
  --coder-session-token="${CODER_SESSION_TOKEN}" \
  --coder-namespace="${CODER_NAMESPACE}"
```

1. Update probes to HTTPS on port `6443` for standalone mode:

```bash
kubectl -n coder-system patch deployment coder-k8s --type='merge' -p '{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "coder-k8s",
            "livenessProbe": {
              "httpGet": {"scheme": "HTTPS", "path": "/healthz", "port": 6443}
            },
            "readinessProbe": {
              "httpGet": {"scheme": "HTTPS", "path": "/readyz", "port": 6443}
            }
          }
        ]
      }
    }
  }
}'
```

## 4) Verify

```bash
kubectl rollout status deployment/coder-k8s -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com
kubectl get coderworkspaces.aggregation.coder.com -A
kubectl get codertemplates.aggregation.coder.com -A
kubectl logs -n coder-system deploy/coder-k8s
```

## Object names: canonical organization and owner names

Aggregated objects are addressed by dotted names built from Coder's **canonical** names:

- `CoderTemplate`: `<organization>.<template>`
- `CoderWorkspace`: `<organization>.<owner>.<workspace>`

Coder itself accepts aliases such as the `default` organization, the `me` user, or raw IDs,
but the aggregated API server rejects a request whose organization or owner segment resolves
to a differently named organization or user. Aliases would otherwise return an object under a
different `metadata.name`, which breaks `kubectl apply` (name precondition failures on repeated
applies, `AlreadyExists` after a `NotFound` GET). The rejection is a `400 BadRequest` that names
the canonical form, and it happens before any Coder mutation (no upload, template version,
workspace, or build is created). A literal organization or user that is really named `default`
or `me` stays valid.

Cross-organization requests stay opaque: a workspace that exists in another organization is
reported as `NotFound` without disclosing its canonical names.

Discover the canonical names before creating objects (this works before any template or
workspace exists). Use the operator token that the controller stores for the control plane,
or any Coder session token with access to the organization:

```bash
kubectl -n coder port-forward svc/coder 3000:80 &
TOKEN_SECRET=$(kubectl -n coder get codercontrolplane coder -o jsonpath='{.status.operatorTokenSecretRef.name}')
TOKEN=$(kubectl -n coder get secret "$TOKEN_SECRET" -o jsonpath='{.data.token}' | base64 -d)

# canonical organization name behind the "default" alias
curl -sS -H "Coder-Session-Token: $TOKEN" http://127.0.0.1:3000/api/v2/organizations/default | jq -r .name
# canonical username behind the "me" alias (the token's user)
curl -sS -H "Coder-Session-Token: $TOKEN" http://127.0.0.1:3000/api/v2/users/me | jq -r .username
```

Existing objects already list their canonical names:

```bash
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A
```

Migration: manifests that used alias segments (for example `default.my-template` or
`default.me.my-workspace`) must be renamed to the canonical form reported by the error message,
and `spec.organization` must carry the same canonical organization name.

## Server-Side Apply (SSA) behavior

`coder-k8s` now includes a compatibility fallback for SSA create-on-update requests
(for example, `kubectl apply --server-side` when the target resource does not exist yet).

- For missing `coderworkspaces` / `codertemplates`, the aggregated API server's `Update`
  path can delegate to `Create` when `forceAllowCreate=true`.
- This is intentionally **best-effort**: Coder resources do not currently provide a
  first-class metadata store for Kubernetes `metadata.managedFields`, so SSA field-owner
  conflict semantics are not durable.

Planned follow-up options (in order of preference):

1. **Preferred:** add first-class metadata support on template/workspace resources in
   Coder + `codersdk`, and round-trip Kubernetes-managed metadata there.
2. Persist Kubernetes-only metadata in a shadow Kubernetes resource (ConfigMap/CRD)
   managed by the aggregated API server.
3. Keep the compatibility fallback and continue documenting the limitations.

## Template build wait tuning

When updating `CoderTemplate.spec.files`, the aggregated API server now waits for
Coder to finish building the new template version before promoting it active.

The wait behavior is configurable via environment variables on the
`coder-k8s` deployment:

- `CODER_K8S_TEMPLATE_BUILD_WAIT_TIMEOUT` (default: `25m`)
- `CODER_K8S_TEMPLATE_BUILD_BACKOFF_AFTER` (default: `2m`)
- `CODER_K8S_TEMPLATE_BUILD_INITIAL_POLL_INTERVAL` (default: `2s`)
- `CODER_K8S_TEMPLATE_BUILD_MAX_POLL_INTERVAL` (default: `10s`)

- Aggregated API request timeout defaults to `30m`; keep it at or above the build wait timeout.

- `CODER_K8S_TEMPLATE_BUILD_WAIT_TIMEOUT` values above `30m` are rejected, because they cannot exceed the API request timeout.

Behavior:

- Polls at the initial interval for the first 2 minutes.
- After that, poll interval doubles up to the max poll interval.
- Fails if the version build ends in `failed`/`canceled` or the total wait
  timeout is exceeded.

## TLS note

`deploy/apiserver-apiservice.yaml` uses `insecureSkipTLSVerify: true` for development convenience.
Use proper CA-backed TLS wiring for production environments.
