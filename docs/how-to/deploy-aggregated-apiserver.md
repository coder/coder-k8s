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

## Object names: canonical organization, owner, template, and workspace names

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

The final segment is checked the same way for lookups of existing objects: Coder resolves
template and workspace names case-insensitively, so a `GET`, update, patch, delete, or
create-on-update of `acme.Starter-Template` finds the template named `starter-template`. The
aggregated API server rejects such a request with a `400 BadRequest` before returning the object,
evaluating delete preconditions, or mutating the backend. When the organization or owner segment
is also an alias, a workspace rejection names the fully canonical form taken from the fetched
workspace (`acme.alice.dev-workspace` for `default.me.Dev-Workspace`); a template rejection is
issued before the template lookup, so it corrects only the organization segment and states that
the template segment is not checked yet — retry with the canonical organization and, if needed,
the canonical template name. Names that are genuinely mixed-case in Coder stay valid when
requested exactly; only a differently cased request is rejected.

This no-mutation guarantee covers lookups of existing objects only. A direct create of a new
`CoderTemplate` whose name differs only in casing from an existing template is a genuine
creation attempt: with `spec.files` set, the source archive is uploaded and a template version is
created before Coder reports the name collision, so the failed request can leave those artifacts
behind. Create requests are not checked against existing names first.

Kubernetes authorization uses the requested URL name, so a `resourceNames` grant for the
canonical name does not cover other casings, and a grant for another casing reaches the
aggregated server only to be rejected.

Cross-organization requests stay opaque: a workspace that exists in another organization is
reported as `NotFound` without disclosing its canonical names, and a request naming an
organization the caller is not allowed to read is also reported as `NotFound`, so it cannot
be used to probe whether such a workspace exists.

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

Migration: manifests that used alias segments (for example `default.my-template`,
`default.me.my-workspace`, or `acme.My-Template` for a template named `my-template`) must be
renamed to the canonical form reported by the error message, and `spec.organization` must carry
the same canonical organization name.

## Delete preconditions

`DELETE` requests may carry `preconditions.uid` and/or `preconditions.resourceVersion` in an
explicit `DeleteOptions` body (`kubectl delete -f` does not send them, even when the manifest
includes `metadata.uid`). Both resources compare each supplied value with the object fetched for
that request and answer `409 Conflict` on a mismatch without touching Coder; matching values
proceed as an ordinary delete. Absent preconditions skip the check; an explicitly supplied empty
`uid` or `resourceVersion` is compared like any other value. Other `DeleteOptions` handling is
unchanged by this check.

- `uid` is the Coder template or workspace ID exposed as `metadata.uid`; after a match the
  deletion targets that same ID. It guards against deleting a different object that now carries
  the same name, not against changes to the same object.
- `resourceVersion`, when supplied, is compared with the value the server computes for the
  object fetched for that request. A mismatch is reported only when the two values differ; the
  comparison is a snapshot, not a compare-and-swap, so a backend change between the fetch and the
  delete is not detected. Template `resourceVersion` is still derived from the backend
  `updated_at`, which template metadata updates change. Workspace `resourceVersion` is the
  representation fingerprint described below, so builds, rename, TTL and autostart changes are
  detected even though those operations did not advance the workspace `updated_at` on Coder
  2.37.2.

## Workspace resourceVersion

`CoderWorkspace.metadata.resourceVersion` is an opaque fingerprint: the full hex SHA-256 of the
object the converter produces from the Coder workspace, serialized with `resourceVersion` unset.
The fingerprint covers the converter's metadata, spec and status fields, including
`status.lastUsedAt` and `status.autoShutdown`; two identical converted representations carry the
same token. Consequences:

- It is not a monotonic revision or a history cursor. Returning to an identical representation
  (for example TTL A → B → A) returns the same token again, and a change that was reverted before
  the fetch is not detected. Do not parse, order or compare tokens other than for equality.
- Activity is part of the representation. A workspace whose `status.lastUsedAt` or build status
  moved between a read and an update or delete produces a `409 Conflict` even without a user
  edit; re-read and retry with the fresh token.
- `GET`, `LIST`, mutation responses and local watch events use the same conversion, so a mutation
  response and the following `GET` agree only while the backend representation stays the same
  (a progressing build can legitimately change it in between).
- `UPDATE` (always) and `DELETE` (when `preconditions.resourceVersion` is supplied) compare the
  token with the freshly fetched object and return `409 Conflict` before any Coder mutation when
  they differ. A supplied `DELETE` `preconditions.uid` still guards recreation identity: a
  same-named workspace created later has a different `uid`.
- Tokens from releases that exposed the numeric `updated_at` value no longer match; clients must
  re-read before retrying an update or delete after the upgrade.
- Watch behavior is unchanged: events are emitted only for writes made through this server,
  `resourceVersion` on watch requests is ignored, and `resourceVersionMatch` is rejected. There is
  no replay and no notification for out-of-band Coder changes.

Workspace deletion stays asynchronous (a delete build is requested).

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
