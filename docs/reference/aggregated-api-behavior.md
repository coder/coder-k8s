# Aggregated API behavior

`coderworkspaces` and `codertemplates` are served from a live Coder instance, not stored in etcd. This page lists where they behave differently from ordinary Kubernetes resources.

## Summary

| Topic | What to know |
| --- | --- |
| [Object names](#object-names) | Use Coder's canonical names. Aliases (`default`, `me`) and wrong casing return `400`. |
| [Delete preconditions](#delete-preconditions) | `uid` and `resourceVersion` are checked. A mismatch returns `409` and leaves Coder untouched. |
| [Workspace `resourceVersion`](#workspace-resourceversion) | An opaque fingerprint. Compare for equality only. Workspace activity alone can cause `409`. |
| [Watch](#watch) | Reports only writes made through this server. No replay, no initial events. |
| [Server-side apply](#server-side-apply) | Create-on-update works. Field ownership is not persisted. |
| [Template builds](#template-builds) | Create and update with `spec.files` wait for Coder to finish the import. |

## Object names

Names are built from Coder's **canonical** names:

| Resource | `metadata.name` |
| --- | --- |
| `CoderTemplate` | `<organization>.<template>` |
| `CoderWorkspace` | `<organization>.<owner>.<workspace>` |

### Aliases return `400`

Coder accepts aliases such as the `default` organization, the `me` user, and raw IDs. The aggregated API server rejects any organization or owner segment that resolves to a differently named organization or user.

- The `400 BadRequest` error names the canonical form.
- The check runs before any Coder change: no upload, template version, workspace, or build is created.
- An organization or user that is literally named `default` or `me` is still valid.

**Why:** an alias would return an object with a different `metadata.name` than requested. That breaks `kubectl apply`: repeated applies fail name preconditions, and a `NotFound` GET is followed by `AlreadyExists`.

### Casing must match exactly

Coder resolves template and workspace names case-insensitively. The aggregated API server does not. For example, if the template is named `starter-template`, requests for `acme.Starter-Template` return `400 BadRequest`.

- This applies to GET, update, patch, delete, and create-on-update of existing objects.
- The request is rejected before the object is returned, delete preconditions are checked, or Coder is changed.
- Names that really are mixed-case in Coder work when requested exactly.

When the name also contains an alias:

- **Workspaces:** the error names the fully canonical form, taken from the fetched workspace. For example, `default.me.Dev-Workspace` → `acme.alice.dev-workspace`.
- **Templates:** the request is rejected before the template lookup. The error corrects only the organization and says the template segment was not checked yet. Retry with the canonical organization and, if needed, the canonical template name.

!!! warning "New templates are not checked against existing names"
    The no-change guarantee covers requests for existing objects only. Creating a `CoderTemplate` whose name differs from an existing template only by case is a real create. With `spec.files`, the archive is uploaded and a template version is created before Coder reports the collision. Those artifacts stay in Coder.

### Authorization and privacy

- Kubernetes authorizes the name in the request URL. A `resourceNames` grant for the canonical name does not cover other casings. A grant for another casing lets the request through, and the aggregated server then rejects it.
- Cross-organization requests reveal nothing. A workspace in another organization returns `NotFound` without its canonical names. A request that names an organization the caller cannot read also returns `NotFound`, so it cannot be used to probe whether a workspace exists.

### Find canonical names

Existing objects already use canonical names:

```bash
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A
```

Before any object exists, ask Coder. Use the operator token the controller stores for the control plane, or any session token with access to the organization:

```bash
kubectl -n coder port-forward svc/coder 3000:80 &
TOKEN_SECRET=$(kubectl -n coder get codercontrolplane coder -o jsonpath='{.status.operatorTokenSecretRef.name}')
TOKEN=$(kubectl -n coder get secret "$TOKEN_SECRET" -o jsonpath='{.data.token}' | base64 -d)

# Organization behind "default"
curl -sS -H "Coder-Session-Token: $TOKEN" http://127.0.0.1:3000/api/v2/organizations/default | jq -r .name
# Username behind "me" (the token's user)
curl -sS -H "Coder-Session-Token: $TOKEN" http://127.0.0.1:3000/api/v2/users/me | jq -r .username
```

### Migrate old manifests

Rename objects that use aliases or wrong casing to the canonical form given in the error message. Examples: `default.my-template`, `default.me.my-workspace`, or `acme.My-Template` for a template named `my-template`. Set `spec.organization` to the same canonical organization name.

## Delete preconditions

`DELETE` accepts `preconditions.uid` and `preconditions.resourceVersion` in an explicit `DeleteOptions` body.

!!! note
    `kubectl delete -f` does not send preconditions, even when the manifest has `metadata.uid`.

Each supplied value is compared with the object fetched for that request:

| Precondition | Result |
| --- | --- |
| Omitted | Not checked. |
| Supplied and matches | Normal delete. |
| Supplied and does not match | `409 Conflict`. Coder is not touched. |

An explicitly empty `uid` or `resourceVersion` counts as supplied and is compared like any other value. Other `DeleteOptions` handling is unchanged.

What each precondition protects against:

- **`uid`** is the Coder template or workspace ID (`metadata.uid`). After a match, the delete targets that same ID. It stops you from deleting a *different* object that now has the same name. It does not detect changes to the same object.
- **`resourceVersion`** is a snapshot check, not compare-and-swap. A backend change between the fetch and the delete is not detected. For templates, the value derives from Coder's `updated_at`, which template metadata updates change. For workspaces, it is the [fingerprint](#workspace-resourceversion), so builds, rename, TTL, and autostart changes are detected. (On Coder 2.37.2 those operations do not advance the workspace's `updated_at`.)

Workspace deletion is asynchronous: it requests a delete build.

## Workspace `resourceVersion`

`CoderWorkspace.metadata.resourceVersion` is an opaque fingerprint: the full hex SHA-256 of the converted object, serialized with `resourceVersion` unset. It covers metadata, spec, and status, including `status.lastUsedAt` and `status.autoShutdown`. Identical representations get the same token.

What this means for clients:

- **Equality only.** It is not a revision counter or history cursor. Changing TTL A → B → A returns the original token, and a change reverted before the fetch goes unnoticed. Do not parse or order tokens.
- **Activity counts.** If `status.lastUsedAt` or the build status changes between your read and your update or delete, you get `409 Conflict` without anyone editing the workspace. Re-read and retry with the fresh token.
- **Same conversion everywhere.** GET, LIST, mutation responses, and local watch events share it. A mutation response and the next GET agree only while Coder's state is unchanged; a running build can change it in between.
- **Checked before any change.** UPDATE always compares the token with the freshly fetched object. DELETE does too when `preconditions.resourceVersion` is supplied. A mismatch returns `409 Conflict` before Coder is touched. For DELETE, `preconditions.uid` still guards identity: a later workspace with the same name has a different `uid`.
- **Upgrading:** tokens from releases that exposed the numeric `updated_at` no longer match. Re-read before retrying an update or delete.

## Watch

Applies to both resources.

- Events are sent only for writes made through this server. There is no replay, and changes made directly in Coder produce no events.
- To start a watch, pass the current `resourceVersion` and omit `sendInitialEvents` and `resourceVersionMatch`. The token is ignored once the watch starts; it is not a replay cursor.

These requests are rejected:

| Request | Result |
| --- | --- |
| `resourceVersion` omitted or `0` (the API server's WatchList defaulting treats this as a request for initial events) | `400 Bad Request` |
| `resourceVersionMatch` set | Rejected |
| `sendInitialEvents=false` without a matching option | `422 Invalid` (rejected upstream) |

## Server-side apply

`kubectl apply --server-side` can create a resource that does not exist yet. For a missing workspace or template, the update path falls back to create (`forceAllowCreate=true`).

This is **best-effort**. Coder has no place to store Kubernetes `metadata.managedFields`, so SSA field-ownership conflicts are not tracked durably.

??? info "Possible future fixes (in order of preference)"
    1. Add first-class metadata to Coder templates and workspaces (and `codersdk`), and round-trip Kubernetes metadata there.
    2. Store Kubernetes-only metadata in a shadow Kubernetes resource (ConfigMap or CRD) owned by the aggregated API server.
    3. Keep the fallback and document its limits.

## Template builds

For a `CoderTemplate` with `spec.files`, the server waits for Coder to finish importing (building) the uploaded template version before using it:

- **Create** uploads the files, creates the version, waits for the import, then creates the template. On success, workspaces can use the template right away.
- **Update** with changed files waits the same way before making the new version active.
- **Create without `spec.files`** does not wait.

If the import fails, times out, or the request is cancelled, Create creates no template and Update activates nothing. The uploaded file and template version stay in Coder; they are not deleted or cancelled.

!!! warning "Retries are not idempotent"
    Each retry uploads and imports again, so retries can leave extra template versions. If your client gave up before the server answered, re-read the template before retrying.

### Tuning

Set these environment variables on the `coder-k8s` Deployment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `CODER_K8S_TEMPLATE_BUILD_WAIT_TIMEOUT` | `25m` | Total wait. Values above `30m` are rejected. |
| `CODER_K8S_TEMPLATE_BUILD_BACKOFF_AFTER` | `2m` | Poll at the initial interval for this long, then back off. |
| `CODER_K8S_TEMPLATE_BUILD_INITIAL_POLL_INTERVAL` | `2s` | Poll interval before backoff. |
| `CODER_K8S_TEMPLATE_BUILD_MAX_POLL_INTERVAL` | `10s` | Backoff doubles the interval up to this value. |

The wait timeout cannot exceed the aggregated API request timeout, which defaults to `30m`. The wait fails if the version build ends `failed` or `canceled`, or the timeout passes.
