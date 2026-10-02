# Aggregated API behavior

The aggregated API server serves `coderworkspaces`, `codertemplates` and `codertemplateversions` from a live Coder instance. Kubernetes does not store them in etcd. This page lists where they behave differently from usual Kubernetes resources.

## Summary

| Topic | What to know |
| --- | --- |
| [Object names](#object-names) | Use the canonical names of Coder. Aliases (`default`, `me`) and wrong casing return `400`. |
| [Delete preconditions](#delete-preconditions) | The server checks `uid` and `resourceVersion`. A mismatch returns `409` and does not change Coder. |
| [Workspace `resourceVersion`](#workspace-resourceversion) | An opaque fingerprint. Compare it only for equality. Workspace activity alone can cause `409`. |
| [Watch](#watch) | Shows only writes made through this server. No replay, no initial events. |
| [Server-side apply](#server-side-apply) | Create-on-update works. The server does not keep field ownership. |
| [Server-side dry-run](#server-side-dry-run) | Not supported for writes to `coderworkspaces` and `codertemplates`. `kubectl diff` and `--dry-run=server` return `400` and do not change Coder. A promotion with `dryRun=All` is a read-only preview. Start and stop accept dry-run too. |
| [Template versions](#template-versions) | Read-only: `get` and `list`, no watch. Reads never download template source. |
| [Promote a template version](#promote-a-template-version) | `codertemplates/<name>/promote` (`create`) makes a version active. A rollback promotes an older version. It needs its own RBAC grant. `dryRun=All` previews it. An unconfirmed result returns `503`, and a concurrent change returns `409`. |
| [Template builds](#template-builds) | Create and update with `spec.files` wait until Coder completes the import. The full request must complete within 34 seconds. |
| [Coder timeouts](#coder-timeouts) | A Coder call that runs out of time returns `504` on every resource. After a `504` on a write, re-read before you retry. |
| [Workspace build log](#workspace-build-log) | `coderworkspaces/log` returns the latest build log as text. It needs its own RBAC grant and has fixed size, time, and concurrency limits. |
| [Start and stop](#start-and-stop) | `coderworkspaces/start` and `coderworkspaces/stop` start or stop a workspace. Each needs its own RBAC grant. A repeated request does not queue a second build while one is active. |

## Object names

The server makes names from the canonical names in Coder:

| Resource | `metadata.name` |
| --- | --- |
| `CoderTemplate` | `<organization>.<template>` |
| `CoderWorkspace` | `<organization>.<owner>.<workspace>` |

### Aliases return `400`

Coder accepts aliases, for example the `default` organization, the `me` user, and raw IDs. The aggregated API server rejects an organization or owner segment that resolves to an organization or user with a different name.

- The `400 BadRequest` error gives the canonical form.
- The check runs before the server changes Coder. The server creates no upload, template version, workspace, or build.
- An organization or user with the real name `default` or `me` is still valid.

The reason: if the server accepted an alias, it would return an object with a `metadata.name` that is different from the requested name. That breaks `kubectl apply`. Repeated applies fail the name preconditions, and a GET that returns `NotFound` is followed by `AlreadyExists`.

### Casing must match exactly

Coder resolves template and workspace names without regard to case. The aggregated API server does not. For example, if the name of the template is `starter-template`, requests for `acme.Starter-Template` return `400 BadRequest`.

- This rule applies to GET, update, patch, delete, and create-on-update of existing objects.
- The server rejects the request before it returns the object, checks delete preconditions, or changes Coder.
- Names that are mixed-case in Coder work when you request them exactly.

When the name also contains an alias:

- Workspaces: the error gives the fully canonical form, taken from the fetched workspace. For example, `default.me.Dev-Workspace` → `acme.alice.dev-workspace`.
- Templates: the server rejects the request before the template lookup. The error corrects only the organization, and says that the server did not check the template segment yet. Try again with the canonical organization and, if necessary, the canonical template name.

!!! warning "The server does not compare new templates with existing names"
    The no-change guarantee applies only to requests for existing objects. A `CoderTemplate` create with a name that is different from an existing template only in case is a real create. With `spec.files`, the server uploads the archive and creates a template version before Coder reports the collision. Those artifacts stay in Coder.

### Authorization and privacy

- Kubernetes authorizes the name in the request URL. A `resourceNames` grant for the canonical name does not cover other casings. A grant for a different casing lets the request through, and then the aggregated server rejects it.
- Requests across organizations show nothing. A workspace in a different organization returns `NotFound` without its canonical names. A request that names an organization that the caller cannot read also returns `NotFound`. Thus a caller cannot use such a request to find out if a workspace exists.

### Find canonical names

Existing objects already use canonical names:

```bash
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A
```

If no object exists yet, ask Coder. Use the operator token that the controller stores for the control plane, or a session token with access to the organization:

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

Rename objects that use aliases or wrong casing to the canonical form that the error message gives. Examples are `default.my-template`, `default.me.my-workspace`, or `acme.My-Template` for a template with the name `my-template`. Set `spec.organization` to the same canonical organization name.

## Delete preconditions

`DELETE` accepts `preconditions.uid` and `preconditions.resourceVersion` in an explicit `DeleteOptions` body.

!!! note
    `kubectl delete -f` does not send preconditions. This is true also when the manifest has `metadata.uid`.

The server compares each supplied value with the object that it fetched for that request:

| Precondition | Result |
| --- | --- |
| Omitted | Not checked. |
| Supplied and matches | Normal delete. |
| Supplied and does not match | `409 Conflict`. Coder does not change. |

An explicitly empty `uid` or `resourceVersion` counts as supplied. The server compares it like all other values. The precondition checks do not change how the server handles other `DeleteOptions`.

What each precondition protects against:

- `uid` is the ID of the Coder template or workspace (`metadata.uid`). After a match, the delete targets that same ID. It prevents a delete of a different object that now has the same name. It does not find changes to the same object.
- `resourceVersion` is a snapshot check, not compare-and-swap. The server does not find a backend change between the fetch and the delete. For templates, the value comes from `updated_at` in Coder, which template metadata updates change. For workspaces, it is the [fingerprint](#workspace-resourceversion), so the server detects builds, renames, TTL changes, and autostart changes. (On Coder 2.37.2, those operations do not change the `updated_at` of the workspace.)

Workspace deletion is asynchronous: it requests a delete build.

## Workspace `resourceVersion`

`CoderWorkspace.metadata.resourceVersion` is an opaque fingerprint. It is the full hex SHA-256 of the converted object, serialized with `resourceVersion` unset. It covers metadata, spec, and status, including `status.lastUsedAt` and `status.autoShutdown`. Identical representations get the same token.

What this means for clients:

- Equality only. The token is not a revision counter or a history cursor. If you change the TTL from A to B and back to A, you get the original token again. A change that is reverted before the fetch is not found. Do not parse or sort tokens.
- Activity counts. If `status.lastUsedAt` or the build status changes between your read and your update or delete, you get `409 Conflict`, also when nobody edited the workspace. Read the workspace again and try again with the new token.
- Same conversion everywhere. GET, LIST, mutation responses, and local watch events use the same conversion. A mutation response and the next GET agree only while the state in Coder does not change. A running build can change it between the two.
- Checked before a change. UPDATE always compares the token with the object that it just fetched. DELETE does too when `preconditions.resourceVersion` is supplied. A mismatch returns `409 Conflict` before Coder changes. For DELETE, `preconditions.uid` still protects identity: a later workspace with the same name has a different `uid`.
- Upgrades: tokens from releases that showed the numeric `updated_at` no longer match. Read the object again before you try an update or delete again.

## Watch

This section applies to both resources.

- The server sends events only for writes made through this server. There is no replay. Changes made directly in Coder make no events.
- A promotion (`codertemplates/<name>/promote`) makes no `codertemplates` event. Events come only from create, update, and delete of the template.
- To start a watch, pass the current `resourceVersion`, and do not set `sendInitialEvents` or `resourceVersionMatch`. After the watch starts, the server ignores the token. It is not a replay cursor.

The server rejects these requests:

| Request | Result |
| --- | --- |
| `resourceVersion` omitted or `0` (the WatchList defaulting of the API server treats this as a request for initial events) | `400 Bad Request` |
| `resourceVersionMatch` set | Rejected |
| `sendInitialEvents=false` without a matching option | `422 Invalid` (rejected upstream) |

## Server-side apply

`kubectl apply --server-side` can create a resource that does not exist yet. For a missing workspace or template, the update path creates the object instead (`forceAllowCreate=true`).

This is best-effort only. Coder has no place to store Kubernetes `metadata.managedFields`. Thus the server does not keep a durable record of SSA field-ownership conflicts.

??? info "Possible future fixes (in order of preference)"
    1. Add first-class metadata to Coder templates and workspaces (and `codersdk`), and round-trip Kubernetes metadata there.
    2. Store Kubernetes-only metadata in a shadow Kubernetes resource (ConfigMap or CRD) owned by the aggregated API server.
    3. Keep the fallback and document its limits.

## Server-side dry-run

The server does not support server-side dry-run for `coderworkspaces` and `codertemplates`. Coder has no dry-run mode, so the server cannot preview a write without making it.

These requests send `dryRun=All`. The server rejects them with `400 BadRequest` ("server-side dry-run is not supported ...; nothing was changed"):

- `kubectl diff`
- `kubectl apply --dry-run=server`, `kubectl create --dry-run=server`, and `kubectl delete --dry-run=server`
- Argo CD with server-side diff turned on (`ServerSideDiff=true`)

The server rejects the request before it sends anything to Coder. Nothing is uploaded, built, or deleted.

The default Argo CD diff and sync do not send `dryRun=All`, and neither does `--dry-run=client`. They work as before. To preview a change, compare the output of `kubectl get -o yaml` with your manifest.

A promotion, a start, and a stop are different, because the server can evaluate them without a write:

| Request with `dryRun=All` | Result |
| --- | --- |
| Create, update, patch, or delete of `coderworkspaces` or `codertemplates` | `400 BadRequest`. Nothing is sent to Coder. |
| Create of `codertemplates/<name>/promote` | `201` with `WouldPromote` or `AlreadyActive`. The server only reads from Coder. See [Promote a template version](#promote-a-template-version). |
| Create of `coderworkspaces/<name>/start` or `/stop` | `201` with `status.dryRun: true` and `WouldQueue`, `InProgress`, or `Unchanged`. The server only reads from Coder. See [Dry-run of start and stop](#dry-run-of-start-and-stop). |

## Coder timeouts

A Coder call that runs out of time returns `504 Gateway Timeout` on every aggregated resource: workspaces, templates, template versions, workspace build logs, and workspace start and stop. A call runs out of time when:

- it takes longer than the Coder request timeout (`--coder-request-timeout`, 30 seconds by default),
- the request deadline ends first (see [The 34-second write budget](#the-34-second-write-budget), the [log limits](#workspace-build-log), and the [start and stop budget](#time-limit-and-retries)), or
- Coder itself answers `504`.

The message does not include the Coder URL.

- After a `504` on a read, try again later.
- After a `504` on a write (create, update, patch, or delete), the result is not certain. Coder can have applied the change before the call timed out. Re-read the object with `kubectl get` before you retry.
- A promotion handles this itself. A `504` before the activation means that nothing changed. When the activation itself times out or fails without an answer, the server re-reads the template and answers `Promoted`, `409`, or `503`. See [Promote a template version](#promote-a-template-version).
- After a `504` on start or stop, re-read the latest build of the workspace before you retry. See [Time limit and retries](#time-limit-and-retries).

## Template versions

`codertemplateversions` is a read-only view of the versions of each Coder template.

- **Names:** `<organization>.<template>.<version>`, for example `acme.docker.v1.2.3`. The version name can contain `.`. Version names are case-sensitive, so `V1` and `v1` are different objects, and a version name with the wrong casing returns `404`. An organization alias or a template name with the wrong casing returns `400`, as for templates.
- **Verbs:** `get` and `list` only. Writes return `405`. To make a new version, change `spec.files` of the `CoderTemplate`.
- **No watch:** `?watch=true` returns `405`. Coder changes versions outside this server, so a watch that showed only writes made through this server would miss most changes. Tools that need `watch` skip the resource. For example, Argo CD does not show or sync resources whose API has no `watch` verb.
- **Labels:** `aggregation.coder.com/organization` and `aggregation.coder.com/template`. For example: `kubectl get codertemplateversions -n coder -l aggregation.coder.com/template=docker`.
- **Fields:** the status shows the version ID, the template ID, the active and archived flags, the creator's username, and the import job status, error code and times. The server does not return the job error text, because Terraform output can contain secrets. It also does not return source files, logs, template variables or the README.
- **`resourceVersion`:** an opaque fingerprint of the object. Compare it only for equality. It changes when the version becomes active or inactive.
- **Lists:** include archived, failed and pending versions, sorted by organization, template and creation time. A list is always complete: the server ignores `limit` and never sends a `continue` token. It rejects `continue`, `resourceVersionMatch`, and a `resourceVersion` other than `0`, with `400` or `422`.

### Cost and limits of reads

The server sends every request to Coder with the one operator token of the control plane. All Kubernetes clients therefore share the Coder API rate limit of that one user. Coder counts the limit for each user and request path. The default is 512 requests per minute, and the Coder deployment can change it with `CODER_API_RATE_LIMIT`.

| Request | Coder requests | Time limit |
| --- | --- | --- |
| `get` | 3, one after another | 25 seconds in total, then `504 Timeout` |
| `list` in one namespace | 1, plus 1 for each template, one after another | 25 seconds in total, then `504 Timeout` and no partial list |
| `list` in all namespaces (`-A`) | For each control plane: 1, plus 1 for each of its templates, one after another | 25 seconds in total for all control planes, then `504 Timeout` and no partial list |

- **No paging:** the server ignores `limit`, and the `continue` token in a list is always empty. Every list returns all versions. A client that sends a `continue` token gets `400`.
- **No watch:** `?watch=true` returns `405`. Tools that need `watch` skip the resource. For example, Argo CD does not show or sync it.
- **No file downloads:** reads of template versions never download template source, so they do not use the file download limit of 12 per minute.
- **Tools that list everything:** tools that list every API resource also list all template versions. For example, a Velero backup that includes the `aggregation.coder.com` group sends one all-namespaces `list`, which costs 1 plus 1 for each template, for each control plane.

## Promote a template version

The `promote` subresource of `codertemplates` makes one version of a template the active version. A rollback is a promotion of an older version.

Send a `CoderTemplateVersionPromotion` with the ID of the version. `kubectl` has no promote command, so use `kubectl create --raw`:

```sh
ID=$(kubectl get codertemplateversion -n coder acme.docker.v1 -o jsonpath='{.status.id}')
echo '{"spec":{"versionID":"'"$ID"'"}}' | kubectl create --raw \
  "/apis/aggregation.coder.com/v1alpha1/namespaces/coder/codertemplates/acme.docker/promote" -f -
```

Add `?dryRun=All` to the path to preview the result without a change. To roll back, promote the ID of an older version the same way.

The server answers `201 Created` with the same kind. The status shows what the server observed:

| `status.result` | Meaning |
| --- | --- |
| `Promoted` | This request changed the active version. A re-read of the template confirmed it. |
| `WouldPromote` | Dry-run only. A real promotion would change the active version. |
| `AlreadyActive` | The version is already active. The server sends no write to Coder, with or without `dryRun`. |

`status.previousActiveVersionID` and `status.activeVersionID` show the active version before and after the request.

- **Version ID:** `spec.versionID` must be the UUID in `status.id` of a `codertemplateversion`. Other values return `422 Invalid`.
- **Membership:** the version must belong to the template in the path. An unknown version and a version of another template return the same `400` ("spec.versionID is not a version of template ..."), so the answer does not show whether a version of another template exists.
- **Only built versions:** an archived version, or a version whose import job did not succeed, returns `400`.
- **Names:** the template name in the path follows the [template rules](#object-names). Aliases and wrong casing return `400`. `metadata.name` in the body must be empty or equal that name.
- **Authorization:** promotion needs `create` on `codertemplates/promote`. No verb on `codertemplates` or `codertemplateversions` gives it. `resourceNames` limit the grant to some templates. See [How callers are checked](../how-to/deploy-aggregated-apiserver.md#how-callers-are-checked).
- **No file downloads:** a promotion reads the organization, the template and the version. It never downloads template source.

**Confirmation.** Coder can apply an activation and still fail to answer, for example after a timeout or a dropped connection. The server never retries the activation. It re-reads the template once and answers with what it sees:

| Activation answer | The re-read shows | Result |
| --- | --- | --- |
| Success, or no clear answer (timeout, dropped connection, `5xx`) | The requested version | `201 Promoted` |
| Success | Another version | `409 Conflict`: a concurrent change superseded the promotion |
| No clear answer | A third version | `409 Conflict` |
| No clear answer | The previous version | `503 ServiceUnavailable`: "could not confirm the promotion ..." |
| Any | The re-read fails | `503 ServiceUnavailable` |

- After a `409` or a `503`, check the active version (`kubectl get codertemplateversions -l aggregation.coder.com/template=<template>`) before you try again. The `503` has no `Retry-After`, so clients do not retry it on their own.
- If Coder rejects the activation, nothing changed. The server answers `400`, or `429` when Coder rate-limits the operator token. The messages do not pass on the Coder error text.
- Coder has no compare-and-swap for the active version, so the last writer wins. The re-read detects only a change that lands before it.
- **Template resourceVersion:** a promotion changes the template in Coder, so the `CoderTemplate` gets a new `resourceVersion`. A client that holds the old one gets `409` on its next update. `AlreadyActive` and dry-run change nothing.
- **GitOps:** an apply of a `CoderTemplate` manifest with other `spec.files` creates and activates a new version. That undoes a rollback. For a durable rollback, revert the manifest or pause syncing.
- **Audit:** the Kubernetes audit log records the caller and the template. The Coder audit log, where the deployment has one, records the operator account and the version IDs.

## Template builds

For a `CoderTemplate` with `spec.files`, the server waits for Coder to finish importing (building) the uploaded template version before using it:

- **Create** uploads the files, creates the version, waits for the import, then creates the template. On success, workspaces can use the template right away.
- **Update** with changed files waits the same way before making the new version active. Metadata changes in the same request (`displayName`, `description`, `icon`) are applied only after that. If the template changed in Coder during the wait, the Update returns `409 Conflict` and changes nothing.
- **Create without `spec.files`** does not wait.

If the import fails, times out, or the request is cancelled, Create creates no template and Update changes nothing (neither the source nor the metadata). The uploaded file and the template version stay in Coder. The server does not delete or cancel them.

### The 34-second write budget

Every create, update, and patch request must finish within **34 seconds**. The limit comes from the Kubernetes API server library that the aggregated API server is built on (`requestTimeoutUpperBound` in the vendored `k8s.io/apiserver`). A client timeout, such as `kubectl --request-timeout`, can only shorten it.

The upload, the template version creation, and the import wait all count against this budget. In practice, the import must finish in about 33 seconds.

When the budget runs out:

- The client gets `504 Gateway Timeout`. The message is usually `request did not complete within requested timeout - context deadline exceeded`, but it can also be the template import timeout message of the server.
- Usually, Create creates no template and Update does not activate the new version. But if the import finishes just before the deadline, Coder can still create the template or activate the version while the client gets the `504`. The final state after a `504` is not certain, so re-read the template with `kubectl get` before you retry.
- If the upload or the version creation had already finished, the file or the template version stays in Coder. If the request timed out while still waiting for the import, the import keeps running and can still succeed, but nothing uses it.

!!! warning "Retries are not idempotent"
    Each retry creates another template version and starts another import. Coder reuses an identical uploaded file, but not the version. If the import takes longer than the budget, every retry times out again, even after an earlier import has succeeded. An Update that timed out while waiting for the import never activates its version later. If your client gave up before the server answered, re-read the template before retrying.

!!! tip "Keep template imports fast"
    Imports that take longer than the budget cannot complete through this API today. Keep the import well under 34 seconds. Follow [issue #117](https://github.com/coder/coder-k8s/issues/117) for changes to this behavior.

### Tuning

Set these environment variables on the `coder-k8s` Deployment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `CODER_K8S_TEMPLATE_BUILD_WAIT_TIMEOUT` | `25m` | Upper limit for the import wait. Must be greater than `0`, at most `30m`, and at least `CODER_K8S_TEMPLATE_BUILD_BACKOFF_AFTER`. Values above the 34-second budget are allowed but do not extend the wait. |
| `CODER_K8S_TEMPLATE_BUILD_BACKOFF_AFTER` | `2m` | Poll at the initial interval for this long, then back off. `0` turns backoff off, so the interval never grows. Must be `0` or more and at most the wait timeout. |
| `CODER_K8S_TEMPLATE_BUILD_INITIAL_POLL_INTERVAL` | `2s` | Poll interval before backoff. Must be greater than `0`. |
| `CODER_K8S_TEMPLATE_BUILD_MAX_POLL_INTERVAL` | `10s` | Backoff doubles the interval up to this value. Must be at least the initial poll interval. |

The default request timeout of the aggregated API server is `30m`. Neither that timeout nor `CODER_K8S_TEMPLATE_BUILD_WAIT_TIMEOUT` can extend a write request beyond the 34-second budget. The wait fails if the version build ends `failed` or `canceled`, or if the budget or the wait timeout runs out.

The server checks these values on each create or update that uploads files, after it uploads them and creates the template version. If the values are invalid, the request fails before the wait starts, and the file and version stay in Coder. For example, `CODER_K8S_TEMPLATE_BUILD_WAIT_TIMEOUT=1m` with the default `2m` backoff makes every such request fail. When you lower the wait timeout below `2m`, lower `CODER_K8S_TEMPLATE_BUILD_BACKOFF_AFTER` too.

Keep the poll intervals well below 34 seconds. The wait sleeps a full interval between polls, so a long interval can miss an import that finishes within the budget, and the request then returns `504`. The maximum interval matters only when `CODER_K8S_TEMPLATE_BUILD_BACKOFF_AFTER` is greater than `0` and shorter than the budget.

## Workspace build log

`GET …/namespaces/<namespace>/coderworkspaces/<name>/log` returns the log of the latest build of the workspace as `text/plain`:

```bash
API=/apis/aggregation.coder.com/v1alpha1/namespaces/coder/coderworkspaces
kubectl get --raw "$API/acme.alice.dev/log"
kubectl get --raw "$API/acme.alice.dev/log?limitBytes=4096"
kubectl get --raw "$API/acme.alice.dev/log?follow=true"
```

Each line has the text format of Coder: `<RFC 3339 time> [<level>] [provisioner|<stage>] <output>`.

- `limitBytes=<n>` ends the response after at most `n` bytes. It can cut a line, but not a UTF-8 character. A value below 1 returns `422`. The server ignores unknown query parameters.
- `follow=true` (or `follow`) sends the existing entries, then new entries as Coder writes them. The response ends when the build ends, at the byte limit, or at the time limit below. There are no duplicate or missing entries between the two parts. To follow the next build, send a new request: the log is always the latest build at request time.
- The name must be the canonical name (see [Object names](#object-names)). The server checks the name before it reads the log. An alias returns `400`, and a workspace in another organization returns `404`.
- The `Accept` header must allow JSON or `*/*`. `Accept: text/plain` alone returns `406`. `kubectl get --raw` works.
- Websocket and other upgrade requests return `400`.
- Build logs can contain secrets that Terraform printed. Reading a workspace does not give access to its log. See [How callers are checked](../how-to/deploy-aggregated-apiserver.md#how-callers-are-checked).

The server limits each log request:

| Limit | Value | When the limit applies |
| --- | --- | --- |
| Response size | 4 MiB | The response ends. When the snapshot part is cut, a `Warning` header says so. A `follow=true` stream that reaches the limit during the live part ends without a warning, because the headers are already sent. If it ends before the build ends, the server cut it. |
| Data read from Coder | 4 MiB of JSON | The response holds the entries read until then, and a `Warning` header. A capped Coder build log is about 2.4 MB of JSON. |
| Time to read from Coder | 60 seconds in total, or 25 minutes with `follow=true`. Each Coder call before the live stream also ends after the Coder request timeout (30 seconds by default). | A snapshot returns `504`. A `follow=true` stream ends. |
| Time to write the response | 2 minutes after the request arrives, or 26 minutes with `follow=true` | If the client reads too slowly, the server closes the response. |
| Open log requests | 64 for each server, 4 for each user | The server returns `429` with `Retry-After: 5`. It makes no Coder call. |

Snapshots and `follow=true` streams use the same open-request slots. A user with 4 open `follow=true` streams gets `429` on a snapshot too.

## Start and stop

`POST …/namespaces/<namespace>/coderworkspaces/<name>/start` starts a workspace. `POST …/coderworkspaces/<name>/stop` stops it:

```bash
API=/apis/aggregation.coder.com/v1alpha1/namespaces/coder/coderworkspaces
kubectl create --raw "$API/acme.alice.dev/start" -f - <<<'{}'
kubectl create --raw "$API/acme.alice.dev/stop" -f - <<<'{}'
kubectl create --raw "$API/acme.alice.dev/stop?dryRun=All" -f - <<<'{}'
```

- Each subresource needs its own grant: `create` on `coderworkspaces/start` or `coderworkspaces/stop`. See [How callers are checked](../how-to/deploy-aggregated-apiserver.md#how-callers-are-checked).
- The body is a `CoderWorkspaceTransition`. `{}` is enough. If the body sets `metadata.name`, it must equal the name in the URL. Otherwise the server returns `400`. The server ignores a `status` in the body.
- The name must be the canonical name (see [Object names](#object-names)). An alias returns `400`, and a workspace in another organization returns `404`.
- A success always returns `201`. The response holds `metadata.name`, `metadata.namespace`, and `status`. It holds no workspace spec or status.

| Field | Meaning |
| --- | --- |
| `status.transition` | `start` or `stop`. |
| `status.outcome` | `Queued`, `InProgress`, `Unchanged`, or `WouldQueue`. See the next table. |
| `status.dryRun` | `true` for a dry-run. |
| `status.buildID`, `status.buildNumber`, `status.jobStatus` | The build that the outcome refers to. They are empty for `WouldQueue`. |

The server reads the latest build of the workspace and decides from its transition and job status:

| Latest build | `start` | `stop` |
| --- | --- | --- |
| start, job `pending` or `running` | `InProgress` | `409` |
| start, job `succeeded` | `Unchanged` | `Queued` |
| stop, job `pending` or `running` | `409` | `InProgress` |
| stop, job `succeeded` | `Queued` | `Unchanged` |
| delete, job `pending` or `running` | `409` | `409` |
| any, job `canceling` | `409` | `409` |
| any, job `failed` or `canceled` | `Queued` | `Queued` |

- `Queued`: the server queued a new build. The build fields name the new build.
- `InProgress`: a build of the requested kind is already pending or running. The server queued nothing. The build fields name that build.
- `Unchanged`: the workspace is already started or stopped. The server queued nothing.
- `409 Conflict`: another build is active. The message names that build. Retry after it ends. `kubectl get --raw "$API/acme.alice.dev/log?follow=true"` ends when the build ends.

Other status codes:

| Code | Cause |
| --- | --- |
| `400` | The name is not valid or not canonical, the body name does not match, or Coder rejected the build. For example, the template version needs parameter values that the workspace does not have. |
| `403` | The caller has no grant on the subresource. |
| `404` | The workspace does not exist, or it is in another organization. |
| `504` | Coder did not answer in time. See the next section. |

### Time limit and retries

One time budget of 25 seconds covers every Coder call of a request: the lookup, the build request, the re-read after a Coder `409`, and the confirming re-read after an uncertain build request. The calls before the confirming re-read must end within 20 seconds, so at least 5 seconds stay for it. Each call also ends after the Coder request timeout (`--coder-request-timeout`, 30 seconds by default).

- If Coder answers `409` to the build request, another build started after the lookup. The server re-reads the workspace one time. If the latest build already does what you asked, the outcome is `InProgress` or `Unchanged`. Otherwise the server returns `409` with the message from Coder.
- If the build request times out, fails on the network, or gets `502`, `503`, or `504` (for example from a proxy in front of Coder), the server cannot know whether Coder queued the build. It re-reads the workspace one time, in the rest of the budget. If the latest build is a new build of the requested kind, the outcome is `Queued`. Otherwise the server returns `504`, and the message says that the result is uncertain.
- After a `504`, re-read the latest build of the workspace (`status.latestBuildID` and `status.latestBuildStatus` of the `CoderWorkspace`) before you retry.
- The server never sends a second build request by itself.

A retry is safe while a build of the same kind is pending or running, because the outcome is then `InProgress`. The server decides from the current state, so a request is not exactly-once. For example, if someone stops the workspace between your start and your retry, the retry starts it again.

### Dry-run of start and stop

Unlike writes to `coderworkspaces` and `codertemplates` (see [Server-side dry-run](#server-side-dry-run)), the start and stop subresources accept `dryRun=All`:

- The server makes only read-only Coder calls. It never sends a build request.
- Admission runs as for a real request.
- The response is `201` with `status.dryRun: true`.
- Where a real request would queue a build, the outcome is `WouldQueue`, and the build fields are empty. `InProgress`, `Unchanged`, and the errors are the same as for a real request.

### Template version

- Start sends the active version of the template when the template requires the active version (`require_active_version`), or when the automatic updates of the workspace are `always`. This is what `coder start` does.
- Otherwise Coder builds the version of the previous build, also when the workspace is outdated.
- Stop never changes the template version.
- `spec.running: true` on a `CoderWorkspace` update follows the same rule as start.

### Other effects

- A start clears the dormant state of a dormant workspace in Coder.
- The Coder audit log shows the operator user of the control plane as the initiator of each build, not the Kubernetes user. The audit log of kube-apiserver records the Kubernetes user.
