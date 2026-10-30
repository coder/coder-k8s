# Manage MCP servers with CoderMCPServer

A `CoderMCPServer` manages one MCP server config in a Coder organization (Coder's **AI settings > MCP servers**). Coder chats then offer the server's tools to the model. The operator creates the config, keeps it equal to the spec, and deletes it when you delete the object.

!!! note "Not the coder-k8s MCP server"
    This page is about MCP servers that Coder chats use. To run the MCP server that coder-k8s itself ships (`--app=mcp-http`), see [Run the MCP server](mcp-server.md).

!!! warning "Who may write these resources"
    The operator calls Coder as a site owner. A subject who can create or update `CoderMCPServer` objects acts as an organization admin for MCP servers in every organization that the control plane allows. See [Grant access](#grant-access).

## Before you start

- A `CoderControlPlane` in the same namespace with `status.operatorAccessReady: true`. The operator user must be a site owner. Without that, a create cannot be confirmed, and the object waits with `ConfirmingCreate`.
- Coder v2.37.2 or later. The Kind end-to-end test runs Coder v2.37.2, and the operator is built against the v2.37.3 SDK.
- The control plane opt-in below.

## 1. Allow organizations on the control plane

The control plane admin decides where `CoderMCPServer` objects can reach. Without `spec.mcpServers`, every `CoderMCPServer` of the control plane stalls with `MCPServersNotAllowed`, and the operator sends no MCP request.

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderControlPlane
metadata:
  name: coder
  namespace: coder
spec:
  mcpServers:
    organizations: [coder]
    allowForceOn: false
```

- `organizations` lists 1 to 64 Coder organization names. The operator resolves each name to its Coder organization ID and compares IDs, so the letter case of a name does not matter. A name that does not resolve yet allows nothing. The operator checks again every 10 minutes.
- `allowForceOn` lets objects use `availability: force_on`. A `force_on` server joins every new chat of each user who can read it, without that user's choice. The default is `false`. Then a `force_on` object stalls with `ForceOnNotAllowed`.
- When you remove an organization, remove `spec.mcpServers`, or set `allowForceOn` back to `false`, the operator disables each affected server in Coder. It sends one update that holds only `enabled: false`, and it reads no credential. Then it changes nothing more. The object stalls with `MCPServersNotAllowed` or `ForceOnNotAllowed`. After you restore the opt-in, the object reports `DisabledExternally` until you edit its spec. Edit the spec (any change, for example `description`) to enable the server again.
- Deleting a `CoderMCPServer` still deletes its config when the opt-in is gone.

## 2. Prepare the credential Secret

An `auth.type` of `api_key`, `custom_headers` or `oauth2` takes its credential from a Secret. The operator reads a value only from a Secret that opted in to the credential contract:

- Type `Opaque`.
- The label `coder.com/credential: "true"`.
- The annotation `coder.com/credential-destinations` with the origin of every URL that receives the value. An origin is `scheme://host`, plus `:port` for a port that is not the default. Separate more than one origin with commas.

| `auth.type` | Value | Origins that the Secret must list |
| --- | --- | --- |
| `none` | no Secret | none |
| `api_key` | `apiKey.secretRef`, sent in the header `apiKey.header` | `spec.url` |
| `custom_headers` | one `secretRef` per header, up to 16 headers | `spec.url` |
| `oauth2` | `oauth2.clientSecretRef` | `spec.url`, `authURL`, `tokenURL`, and `revocationURL` when set |

```bash
kubectl -n coder create secret generic example-mcp-key --from-literal=api-key="$MCP_KEY"
kubectl -n coder label secret example-mcp-key coder.com/credential=true
kubectl -n coder annotate secret example-mcp-key coder.com/credential-destinations=https://mcp.example.com
```

The operator refuses a value that is empty, larger than 8 KiB, starts or ends with whitespace (a trailing newline counts), or is not valid UTF-8. A header value must also hold no control byte such as CR, LF or NUL. Coder sends an API key exactly as stored, so store a bearer token as `Bearer <token>`. [Configure credential Secrets](workspace-proxy-credentials.md#checks-the-operator-runs) lists every rule of the contract.

List only origins that you trust with the credential on every path. Coder sends the API key and the header values again on every request, also after a redirect to another origin. So a listed origin means "this origin and every place that it sends Coder". A plain `http://` URL is allowed for servers in the cluster, but Coder then sends the credential in clear text. The Secret must list the `http://` origin.

## 3. Create the server

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderMCPServer
metadata:
  name: example
  namespace: coder
spec:
  controlPlaneRef:
    name: coder
  organization: coder
  slug: example
  displayName: Example
  url: https://mcp.example.com/mcp
  auth:
    type: api_key
    apiKey:
      header: X-Api-Key
      secretRef:
        name: example-mcp-key
        key: api-key
  access:
    groups: [Everyone]
```

```bash
kubectl apply -f example-mcp-server.yaml
kubectl -n coder wait --for=condition=Ready codermcpserver/example --timeout=2m
```

The sample, with its Secret, is in `config/samples/coder_v1alpha1_codermcpserver.yaml`.

- `controlPlaneRef`, `organization` and `slug` cannot change. The model sees the tools as `<slug>__<tool>`. To rename a slug, delete the object and create a new one. Chats then lose the old server.
- The operator never takes over a config that it did not create. When the organization already has a config with the slug, the object stalls with `SlugConflict`, and the operator sends no write.
- The operator creates the config disabled, with a temporary display name that holds a secret nonce (see [Cleanup records](#cleanup-records)). It then applies the spec. Members do not see the config before that.
- The operator pins the organization ID at the first lookup. If the name later resolves to another ID, the object stalls with `OrganizationChanged`. Re-create the object to use the new organization.
- Coder's `user_oidc` auth type is not supported. If someone sets it in the dashboard, the operator sets the auth type back at the next sync.

## 4. Read the result

```bash
kubectl -n coder get codermcpservers
kubectl -n coder get codermcpserver example -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

The `ENABLED` column shows `status.enabled`: the enabled flag of the config in Coder at the operator's last read or update. It is empty while Coder holds no config of this object, for example when the credential Secret is refused before the create, and before the first read.

The object has the conditions `Ready`, `Reconciling` and `Stalled`. A stalled object waits for a change of its spec or of an input.

| Reason | Meaning |
| --- | --- |
| `Applied` | Coder matches the spec (`Ready=True`). |
| `ControlPlaneNotReady`, `OperatorAccessNotReady` | The control plane cannot serve Coder calls yet. |
| `CoderUnavailable` | A Coder call failed. The operator retries every 30 seconds. |
| `OrganizationNotFound`, `OrganizationChanged` | The organization does not exist, or its ID changed. |
| `MCPServersNotAllowed`, `ForceOnNotAllowed` | The control plane does not allow this object. See [step 1](#1-allow-organizations-on-the-control-plane). |
| `CredentialSecretNotFound`, `CredentialNotOptedIn`, `CredentialDestinationNotAllowed`, `CredentialRefused`, `CredentialKeyMissing`, `CredentialMalformed` | A credential Secret fails the contract. See [Credential kill switch](#credential-kill-switch). |
| `SlugConflict` | The organization has a config with this slug that the operator did not create. |
| `CoderDeploymentChanged` | The control plane now reaches another Coder deployment than the one that holds the config. See [Cleanup records](#cleanup-records). |
| `CreatingConfig`, `ConfirmingCreate`, `CreateRetrying` | A create is in progress, or the operator reads Coder to learn whether a create committed. |
| `GroupNotFound` | A name in `access.groups` is not a Coder group yet. See [Access](#access). |
| `CoderRejected` | Coder rejected the request. Fix the spec. |
| `CoderAnswerMismatch` | Coder's answer does not match the request, for example an answer for another config. The operator checks Coder again later. |
| `DisabledExternally`, `DeletedExternally`, `CredentialClearedExternally` | Someone disabled the server, deleted it, or cleared a credential. `DisabledExternally` also follows a disable by the operator itself, after the opt-in or the credential consent came back. See [Kill switches](#kill-switches). |

## Access

`spec.access` decides which Coder groups can read the server. Coder checks it when a chat selects the server.

- With `access` omitted, the operator never reads or writes the server's access list. Coder's default lets the organization's `Everyone` group read the server. So every member of the organization can use it. Removing `access` later leaves the last access list in place.
- With `access` set, the access list is exactly `access.groups`, each with read access. `spec.access` lists groups only. So every sync removes user grants that someone added in the Coder dashboard, and every group that the list does not name.
- `access: {}` or `groups: []` leaves the server to organization admins.
- `Everyone` is the group of all organization members. Without a Coder license, only `Everyone` exists.

When a group in `access.groups` does not exist, the sync fails closed:

1. It removes every entry that `access.groups` no longer names, also user entries.
2. It adds nothing. Access never widens while a group is missing.
3. If the same spec generation disables the server (`enabled: false`), it sends one update that holds only `enabled: false`.
4. It leaves the rest of the generation unapplied. The object stalls with `GroupNotFound`. When the name is valid but no such group exists yet, the operator checks again every 10 minutes. The API server refuses a name that is not a valid Coder group name. If such a name reaches the operator anyway, the operator does not check again until the spec changes: fix the name.
5. Once the missing groups exist, the sync adds them and applies the rest of the generation.

A new server stays disabled in Coder while one of its groups is missing.

Removing a group from `access` does not detach chats that already use the server. Those chats keep it until the server is disabled or deleted. In an emergency, disable the server (see [Kill switches](#kill-switches)).

## Rotate a credential

Change the Secret value. The operator sends the new value with one update. Coder stores all custom headers in one field, so a change of one header value sends every header value. A change of only the Secret's labels or annotations sends no value, as long as the Secret keeps its consent for the origins. The operator remembers a hash of each sent value in memory only. After an operator restart, the first change of the Secret sends its value once more, also a change of only its metadata. Coder records each credential write in its audit log, with the value marked as secret.

A new value that breaks the contract, for example a value with a trailing newline, triggers the [credential kill switch](#credential-kill-switch).

Coder shows only whether a credential is set, not its value. So the operator cannot detect a dashboard replacement of a value, or the removal of one custom header of several. The next change of the Secret value overwrites it.

## Change the URL

The Secret must list the origin of every URL that receives its value. Change a URL in this order:

1. Add the new origin to `coder.com/credential-destinations`.
2. Change `spec.url` (or an OAuth2 URL).
3. Wait for `Ready=True`. Then remove the old origin, if nothing else uses it.

If you change the URL before the Secret lists the new origin, the object stalls with `CredentialDestinationNotAllowed`. The operator sends nothing, and Coder keeps the old URL and credential, which the Secret still allows. When you add the origin, the operator applies the new URL.

An update that changes a URL or the auth type also sends every credential of the auth type. Coder deletes every user's OAuth2 token for the server when `url`, the auth type, `tokenURL`, `revocationURL` or `clientID` changes. Users must connect again.

## Kill switches

### Dashboard changes

After the operator applied a spec generation, three dashboard changes are kill switches:

- A disable stalls the object with `DisabledExternally`.
- A delete stalls it with `DeletedExternally`.
- A cleared credential, or a changed auth type, stalls it with `CredentialClearedExternally`.

The operator then changes nothing until the spec changes. A spec edit (any change of `spec`, for example `displayName`) enables the server again, sends the Secret's credential again, or creates the server again.

Coder sends no events. The operator sees a dashboard change at its next read: at the resync, about every 10 minutes, or at once when the object, its Secret or its control plane changes. Until then the object can still show `Ready=True`.

### Credential kill switch

For a server that the operator created, every credential error disables the server in Coder: the Secret is deleted, loses its label, no longer lists an origin that Coder sends the credential to, is refused, has no such key, or holds a malformed value. The update holds only `enabled: false`, with no credential and no URL. The operator first checks what Coder holds. When every credential that Coder holds comes from a recorded Secret that still allows the URLs that Coder holds, the operator disables nothing and sends nothing. This is the case during a URL change to an origin that the Secret does not list yet.

The object stalls with the credential reason. Repairing the Secret does not enable the server again: the object then reports `DisabledExternally`. To enable it, fix the Secret, then edit the spec. When the disable fails, the operator retries every 30 seconds, and the condition message starts with `Coder can still send the credentials`.

### The disable window

The operator marks the disable in the object's status before it sends the disable to Coder. This holds the disable also when the update reaches Coder but its answer is lost. If the operator stops after the status write and before the update, the next reconcile still sends the disable while consent or the opt-in stays withdrawn.

This window has one known limit. If consent or the opt-in comes back between the status write and the update, the sync applies the current spec without a later spec edit, and the server stays enabled. Coder never disabled the server in that window. The sync checks every credential first, so no credential goes to an origin without consent.

### Emergency

To stop a server at once:

1. Disable the server in the Coder dashboard (**AI settings > MCP servers**). The object reports `DisabledExternally`, and the operator does not enable it again until a spec edit.
2. Or set `spec.enabled: false`.
3. If a credential leaked, revoke it at the MCP server too.

Disabling stops every chat from using the server, also chats that selected it earlier.

## Delete a server

Deleting the object deletes the config in Coder. Coder removes the server from every chat and deletes the users' OAuth2 tokens for it. The finalizer `coder.com/mcp-server-cleanup` keeps the object until then.

- The operator deletes only the config ID that it recorded. With only an unconfirmed create, it deletes nothing: a person can edit the slug and the display name of any config, so not even the temporary display name proves that this object created a config. When a config holds the slug, or a config with the temporary display name has another slug, the finalizer goes without a delete, and the [cleanup record](#cleanup-records) stays for manual cleanup. A create can still commit for a short time, so within 2 minutes of the create request the operator reads again before it trusts that the config is absent. A record without a config ID also stays when the object reports `SlugConflict` after a create: a person can have renamed the config of that create.
- When Coder is unreachable, or the control plane has no operator access, the finalizer stays and the operator retries every 60 seconds. The operator trusts only Coder's own 404 as proof that the config is gone. A 404 from a proxy keeps the finalizer.
- When the control plane is gone, the operator cannot delete the config now. The finalizer goes at once. The operator logs the slug and the config ID with the reason `ControlPlaneGone`. The [cleanup record](#cleanup-records) stays, so a later sweep can delete the config.
- When you delete the control plane while `CoderMCPServer` objects that use it are being deleted, the control plane waits for them for up to 10 minutes, plus a grace period for a last bounded attempt. It keeps its finalizer, so Coder keeps running, and the delete above runs with its retries. An object that you do not delete holds the control plane for only 30 seconds, so delete the objects together with the control plane. A foreground deletion (`kubectl delete --cascade=foreground`) does not wait.
- When the control plane does not wait (its namespace is being deleted, it is deleted in the foreground, or the 10 minutes have passed) and its operator access still works, the operator sends one delete with a 10-second limit, then removes the finalizer, also when that delete fails. Without working access, the finalizer goes without a delete. So a namespace deletion finishes, but it can leave the config in Coder, and its cleanup record stays. The metric `coder_k8s_cleanup_released_without_delete_total` (label `kind`) counts each release without a delete.
- A withdrawn opt-in never blocks a delete.

Delete `CoderMCPServer` objects before their control plane and before their namespace. A sweep deletes a leftover config only when the namespace comes back with a control plane of the same Coder deployment.

### Cleanup records

Before the operator creates a config in Coder, it writes a cleanup record: a ConfigMap named `ck8s-cleanup-<object UID>` with the label `coder.com/cleanup-record=CoderMCPServer` in the operator's namespace (`coder-system`). If it cannot write the record, it does not create the config. The record holds no credential: it holds the object's namespace, name and UID, Coder's deployment ID from `/api/v2/buildinfo`, the nonce, the pinned organization ID, the slug, and the config ID once the operator knows it. A config that an earlier operator version created gets its record on the next reconcile. A config that was left in Coder before that has no record: delete it in the dashboard.

The nonce is the temporary display name of the create: `coder-k8s:` and a random secret. When a create answer is lost, the operator takes the config with the slug as its own only when its display name is the recorded nonce or its ID is the recorded config ID. As for [AI providers](manage-ai-providers.md#cleanup-records), only the operator and cluster admins can read the record, the operator sends the same nonce again while its last create is unconfirmed, and it makes a new nonce after it knows the config ID. Records of older operator versions hold the nonce `coder-k8s:<object UID>` until the next create. Do not give anyone but cluster admins read or write access to ConfigMaps in the operator's namespace.

The record of an older config needs the deployment ID that Coder answers. When that read or the record write fails, and the delete fails too, in the one bounded attempt while the control plane is being deleted (for example in a namespace deletion), the finalizer goes without a record. The operator does not hold the namespace deletion longer. It logs the slug and the config ID with `ControlPlaneGone`: delete the config in the dashboard.

When the control plane later reaches another Coder deployment (for example after its database Secret changed), the record still locates the config in the old one. The operator then stalls the object with `CoderDeploymentChanged`, keeps the record and creates nothing in the new deployment. Delete the config in the old deployment, then delete the ConfigMap.

The operator removes the record when Coder confirms that the config is deleted. When the finalizer goes without a delete, the record stays, and the operator sweeps it as for [AI providers](manage-ai-providers.md#cleanup-records): once the object is gone and 5 minutes have passed since the last create, through a ready control plane in the object's namespace whose Coder answers the record's deployment ID. The sweep deletes the recorded config ID. A record that no control plane reaches stays, with the gauge `coder_k8s_cleanup_records_unmatched` and a `NoMatchingDeployment` Warning Event. When that Coder deployment never comes back, delete the config in the Coder dashboard, then delete the ConfigMap.

A record without a config ID is left by a create that the operator could not confirm. The sweep deletes nothing for such a record: a person can edit the slug and the display name of any config, so neither proves that the operator created it. When the organization has no config with the recorded slug and none with the recorded nonce, the sweep removes the record. So when a person edits both the slug and the display name of the config of a lost create answer, nothing links that config to the record any more: the sweep removes the record, and the config stays in Coder. Otherwise the record stays, the gauge `coder_k8s_cleanup_records_unmatched` counts it, and the operator writes a `ManualCleanupNeeded` Warning Event on the ConfigMap that names the config. Clean up by hand:

1. Read the record and its Event:

    ```bash
    kubectl -n coder-system get configmap ck8s-cleanup-<object UID> -o yaml
    kubectl -n coder-system get events --field-selector reason=ManualCleanupNeeded
    ```

2. In the dashboard, open **AI settings > MCP servers** in the recorded organization and find the config that the Event names.
3. When that config is a leftover of the deleted object, delete it. The next sweep, within 10 minutes, then removes the record.
4. When a person owns that config, keep it and delete the ConfigMap.

To release a stuck object by hand, remove the finalizer with a JSON patch whose `test` operation checks the index first, as for [AI providers](manage-ai-providers.md#stuck-finalizer). Then check **AI settings > MCP servers** in the dashboard for a leftover config with the slug.

## Coder-side limits

These limits come from Coder, and the operator cannot change them:

- Coder checks server access when a chat selects the server. Chats that already use it keep it until the server is disabled or deleted.
- Coder applies tool allow and deny lists by exact, case-sensitive tool name. With both lists set, Coder ignores the deny list. This kind does not manage tool lists yet. Lists that you set in the dashboard stay.
- A change of the URL deletes every user's OAuth2 token for the server.
- Coder encrypts the stored credentials at rest only when enterprise database encryption keys are set.
- Coder has no address guard for MCP traffic. A server URL can point at a cluster-internal or private address. `auth.type: none` needs no Secret, so only the control plane opt-in and RBAC limit it.
- Coder's MCP client sets the API key, the header values and users' bearer tokens on every request, also after a redirect to another origin. A Secret's destination list therefore means "this origin and every place that it sends Coder".

## GitOps health checks

Argo CD and Flux need health rules to wait for a `CoderMCPServer`. The rules live in `config/gitops/`. This page copies them, and a unit test keeps the copies equal to the files. The controller sets `status.observedGeneration` to `metadata.generation` only when it writes the conditions for that generation. Both rules wait until `status.observedGeneration` equals `metadata.generation`. They also ignore a condition whose own `observedGeneration` is older, so a status that an older controller wrote in the middle of an update does not count as healthy. Then they read the conditions:

| Conditions | Argo CD | Flux |
| --- | --- | --- |
| no status, or a stale `observedGeneration` on the status or the `Ready` condition | `Progressing` | `InProgress` (`Unknown` before the first status) |
| `Reconciling=True` | `Progressing` | `InProgress` |
| `Ready=True` | `Healthy` | `Current` |
| `Stalled=True` | `Degraded` | `Failed` |

The Argo CD message is `<reason>: <message>` of the `Ready` condition. While the object is being deleted, Argo CD shows `Deleting the Coder MCP server config`.

Add the script as the `argocd-cm` key `resource.customizations.health.coder.com_CoderMCPServer` (`config/gitops/argocd-health-codermcpserver.lua`):

```lua
-- Argo CD health check for CoderMCPServer (coder.com/v1alpha1).
-- Install it as the argocd-cm key
-- resource.customizations.health.coder.com_CoderMCPServer.
-- docs/how-to/manage-mcp-servers.md copies this file. A test keeps the copy equal.
local hs = { status = "Progressing", message = "Waiting for the controller" }
if obj.metadata.deletionTimestamp ~= nil then
  -- Argo CD shows deletionMessage, not message, for a resource being deleted.
  hs.deletionMessage = "Deleting the Coder MCP server config"
  hs.message = hs.deletionMessage
  return hs
end
local st = obj.status
if st == nil or st.observedGeneration == nil or st.observedGeneration ~= obj.metadata.generation then
  return hs
end
-- A condition from an older generation does not describe the current spec.
local conditions = {}
for _, c in ipairs(st.conditions or {}) do
  if c.observedGeneration == obj.metadata.generation then
    table.insert(conditions, c)
  end
end
for _, c in ipairs(conditions) do
  if c.type == "Ready" and c.reason ~= nil and c.message ~= nil then
    hs.message = c.reason .. ": " .. c.message
  end
end
for _, c in ipairs(conditions) do
  if c.type == "Ready" and c.status == "True" then
    hs.status = "Healthy"
  elseif c.type == "Stalled" and c.status == "True" then
    hs.status = "Degraded"
  end
end
return hs
```

Add this entry (`config/gitops/flux-healthcheck-codermcpserver.yaml`) to `spec.healthCheckExprs` of a Flux v2.5+ Kustomization with `spec.wait: true`:

```yaml
# Flux health check expressions for CoderMCPServer (coder.com/v1alpha1).
# Add this entry to spec.healthCheckExprs of a Flux v2.5+ Kustomization
# with spec.wait: true.
# docs/how-to/manage-mcp-servers.md copies this file. A test keeps the copy equal.
- apiVersion: coder.com/v1alpha1
  kind: CoderMCPServer
  inProgress: "!has(status.observedGeneration) || status.observedGeneration != metadata.generation || !has(status.conditions) || !status.conditions.exists(c, c.type == 'Ready' && has(c.observedGeneration) && c.observedGeneration == metadata.generation) || status.conditions.exists(c, c.type == 'Reconciling' && c.status == 'True')"
  failed: "status.conditions.exists(c, c.type == 'Stalled' && c.status == 'True')"
  current: "status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')"
```

## Grant access

The operator acts as a Coder site owner. So each permission in the control plane's namespace maps to Coder power:

| Permission | Coder power |
| --- | --- |
| `create`, `update`, `patch` on `codermcpservers` | Organization admin for MCP servers in the organizations that the control plane lists: add servers with any URL and choose the access groups. With `allowForceOn`, also add servers to every member's new chats. The operator sends a credential only to origins that its Secret lists. |
| `update`, `patch` on `codermcpservers/status` | Operator only. `status.configID` tells the operator which config an object owns. A holder can point an object at any config in the listed organizations, then change or delete that config. |
| `delete` on `codermcpservers` | Removes owned servers from every chat |
| `update`, `patch` on `codercontrolplanes` | Chooses the organizations and allows `force_on` |
| `patch` on credential Secrets | Decides whether a credential can be used (label) and where it can go (annotation) |
| `get`, `list` on `codermcpservers` | Sees slugs, URLs, Secret names, Coder IDs and group names. No secrets. |

A viewer Role:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: codermcpserver-viewer
  namespace: coder
rules:
  - apiGroups: ["coder.com"]
    resources: ["codermcpservers"]
    verbs: ["get", "list", "watch"]
```

An editor Role. Give it only to subjects who may act as an organization admin for MCP servers and who can also read the credential Secrets that their objects use:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: codermcpserver-editor
  namespace: coder
rules:
  - apiGroups: ["coder.com"]
    resources: ["codermcpservers"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

Neither Role grants `codermcpservers/status`. Only the operator ServiceAccount must write it.

!!! warning "Wildcards grant subresources"
    A rule with `resources: ["*"]` in the `coder.com` group also grants `codermcpservers/status`. Do not give such a rule to anyone but the operator.

A `ResourceQuota` on `count/codermcpservers.coder.com` limits how many servers a namespace can create.

The emergency path needs no Kubernetes access: a Coder admin disables the server in the dashboard.

### Enforce operator-only status writes

`config/admission/status-operator-only.yaml` holds an opt-in `ValidatingAdmissionPolicy` and its binding. The policy denies every update and patch of `codermcpservers/status` by any subject except the operator ServiceAccount `system:serviceaccount:coder-system:coder-k8s`. It denies cluster admins too. The install bundle does not apply it. Apply it yourself:

```bash
kubectl apply -f config/admission/
```

`ValidatingAdmissionPolicy` is GA in Kubernetes v1.30, so the policy needs v1.30 or later. If the operator runs as another ServiceAccount or in another namespace, change the user name in the policy's expression first, or the operator cannot write status. To fix a status by hand, delete the binding `coder-k8s-status-operator-only` first, then apply it again.
