# Manage AI providers with CoderAIProvider

A `CoderAIProvider` manages one AI provider in Coder (Coder's `/api/v2/ai/providers`). The operator creates the provider, keeps its settings and its API key equal to the spec, and deletes it when you delete the object. The object name is the Coder provider name.

!!! warning "Who may write these resources"
    The operator calls Coder as a site owner. Grant `create`, `update`, `patch` and `delete` on `coderaiproviders` only to principals who may act as a Coder owner for AI providers in the whole deployment. A delete makes the operator delete the provider in Coder. Such a principal can use any credential Secret in the namespace that opted in, but only toward the origins that the Secret lists (see [Prepare the key Secret](#1-prepare-the-key-secret)).

## Before you start

- A `CoderControlPlane` in the same namespace with `status.operatorAccessReady: true`. The operator user must be a site owner. Coder answers `403` to anyone else on the provider routes.
- An API key for the provider, for example an OpenAI key.
- Coder v2.37.2 or later. The Kind end-to-end test runs Coder v2.37.2, and the operator is built against the v2.37.3 SDK. Coder has the provider API since v2.34.0, but versions before v2.37.2 are not tested.
- coder-k8s v0.1.4 or later. Install from the release tag, and pin the image to the same version (see [Deploy a Coder Control Plane](../tutorials/getting-started.md#1-install-the-operator)).

## 1. Prepare the key Secret

The operator reads the key only from a Secret that opted in to the credential contract. The Secret needs both of these:

- The label `coder.com/credential: "true"`.
- The annotation `coder.com/credential-destinations` with the origin of `spec.baseURL`, for example `https://api.openai.com`. An origin is `scheme://host`, plus `:port` for a port that is not the default. List only origins that you trust: an origin that redirects can pass the key on.

```bash
kubectl -n coder create secret generic openai-api-key --from-literal=api-key="$OPENAI_API_KEY"
kubectl -n coder label secret openai-api-key coder.com/credential=true
kubectl -n coder annotate secret openai-api-key coder.com/credential-destinations=https://api.openai.com
```

The operator refuses the value when it is empty, larger than 8 KiB, starts or ends with whitespace (a trailing newline counts), is not valid UTF-8, or holds a control byte (for example NUL, CR or LF). It also refuses Secrets that the operator generates or that a `CoderControlPlane` uses, and Secrets with a type other than `Opaque`. [Configure credential Secrets](workspace-proxy-credentials.md#checks-the-operator-runs) lists every rule.

A `baseURL` with plain `http://` is allowed for model servers inside the cluster, but Coder then sends the key in clear text. The Secret must list the `http://` origin.

## 2. Create the provider

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderAIProvider
metadata:
  name: openai
  namespace: coder
spec:
  controlPlaneRef:
    name: coder
  type: openai
  baseURL: https://api.openai.com/v1
  displayName: OpenAI
  apiKeySecretRef:
    name: openai-api-key
    key: api-key
```

```bash
kubectl apply -f openai-provider.yaml
kubectl -n coder wait --for=condition=Ready coderaiprovider/openai --timeout=2m
```

`spec.type` and `spec.controlPlaneRef` cannot change. The name cannot be a UUID, because Coder reads a UUID-shaped name as a provider ID. The sample is in `config/samples/coder_v1alpha1_coderaiprovider.yaml`.

The operator never takes over a provider that it did not create. When Coder already has a provider with the name, the object stalls with `NameConflict` and the operator sends no write. Choose another name, or delete that provider in Coder.

To rotate the key, change the Secret value. The operator sends the new key with one update. A change of only the Secret's labels or annotations sends no key, as long as the Secret keeps its consent for the origin: removing the label or the origin is the [credential kill switch](#the-credential-kill-switch). The operator remembers the sent key in memory only, so after an operator restart the first change of the Secret sends the key once more. Coder's audit log records the masked key on every key write.

## 3. Read the result

```bash
kubectl -n coder get coderaiproviders
kubectl -n coder get coderaiprovider openai -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

The object has the conditions `Ready`, `Reconciling` and `Stalled`. A stalled object waits for a change of its spec or of an input. The most common reasons:

| Reason | Meaning |
| --- | --- |
| `Applied` | Coder matches the spec (`Ready=True`). |
| `ControlPlaneNotReady`, `OperatorAccessNotReady` | The control plane cannot serve Coder calls yet. |
| `CoderUnavailable` | A Coder call failed. The operator retries every 30 seconds. |
| `CredentialSecretNotFound`, `CredentialNotOptedIn`, `CredentialDestinationNotAllowed`, `CredentialRefused`, `CredentialKeyMissing`, `CredentialMalformed` | The key Secret fails the credential contract. See [the credential kill switch](#the-credential-kill-switch). |
| `NameConflict` | Coder has a provider with this name that the operator did not create. |
| `NameReservedByEnv` | The control plane can seed a provider with this name. See [Seeding limits](#seeding-limits). |
| `CoderRejected` | Coder rejected the create or update request. Fix the spec. |
| `DisabledExternally`, `KeyChangedExternally`, `DeletedExternally` | Someone disabled the provider, changed its key or deleted it in Coder. See [Kill switches](#kill-switches). |

## Kill switches

### The credential kill switch

When the key Secret fails the credential contract for a provider that the operator created, the operator disables the provider in Coder at once. This covers every credential error: the Secret is deleted, loses its label or the destinations annotation, no longer lists the origin that Coder uses, is refused, has no such key, or holds a malformed value. The update holds only `enabled: false`. Coder keeps the old key, but no longer uses it. When a Coder call for the disable fails, including a 404 that Coder did not send (for example from a proxy), the operator retries every 30 seconds, and the condition message starts with `Coder can still send the key`.

The object stalls with the credential reason. Repairing the Secret does not enable the provider again. To enable it, fix the Secret, then edit the spec (any change of `spec`, for example `displayName`). A short read error of the Secret, such as an API server timeout, is not a credential error: the operator retries and changes nothing.

### Dashboard changes

A disable, a key change or a delete in the Coder dashboard is a kill switch too. The object stalls with `DisabledExternally`, `KeyChangedExternally` or `DeletedExternally`, and the operator does not undo the change until the spec changes. A spec edit then enables the provider, sends the Secret's key, or creates the provider again.

The kill switch holds once the operator has fully applied the current spec. Until then, the operator replaces a key that it did not send with the Secret's key. For example, when a create answer is lost and someone changes the key in the dashboard before the operator reads the provider, the operator sends the Secret's key and only then reports `Ready`.

Coder sends no events, so the operator notices a dashboard change at its next read of the provider: at the resync, about every 10 minutes, or at once when the object, its key Secret or its control plane changes. Until then the object can still show `Ready=True`.

To revoke a key in an emergency:

1. Disable the provider in the Coder dashboard, or delete the key Secret or remove its label.
2. Revoke the key at the upstream provider.

## Seeding limits

Coder seeds AI providers at startup from its own settings, and it fails to start when a seeded provider differs from a provider with the same name in its database. So the operator does not create a provider whose name the control plane can seed. The object stalls with `NameReservedByEnv` and the message names the setting, never its value.

The operator checks the control plane's `spec.extraEnv` and `spec.extraArgs`, in any letter case and with any value or `valueFrom`:

| Setting | Reserved name |
| --- | --- |
| `CODER_AI_GATEWAY_OPENAI_KEY`, `CODER_AIBRIDGE_OPENAI_KEY`, or the matching `--ai-gateway-openai-key` and `--aibridge-openai-key` flags | `openai` |
| `CODER_AI_GATEWAY_ANTHROPIC_KEY`, `CODER_AIBRIDGE_ANTHROPIC_KEY`, any `CODER_AI_GATEWAY_BEDROCK_*` or `CODER_AIBRIDGE_BEDROCK_*` variable, and the matching flags | `anthropic` |
| A config file: `CODER_CONFIG_PATH`, `--config` or `-c`. Its Bedrock settings seed `anthropic`. The operator does not read the file. | `anthropic` |
| Any `CODER_AI_GATEWAY_PROVIDER_<N>_*` or `CODER_AIBRIDGE_PROVIDER_<N>_*` variable, or a `$(VAR)` reference in `spec.extraArgs` | every name: no provider is created |

These limits remain:

- The operator does not read `spec.envFrom` sources. A ConfigMap or Secret there that sets one of the variables above is not checked. Do not seed providers through `envFrom` on a control plane that `CoderAIProvider` objects use.
- The check runs only before a create. When you add a seeding setting after the provider exists, the control plane rolls coderd at once, and coderd fails to start if the seeded provider differs. Remove the setting, or delete the `CoderAIProvider` and create it with another name.

## Delete a provider

Deleting the object deletes the provider in Coder. With only an unconfirmed create, the operator reads Coder first and deletes only a provider that this object created. A create request can still commit for a short time, so within 2 minutes of that request the operator reads again before it trusts that the provider is absent.

- When Coder is unreachable, or a control plane that is not being deleted has no operator access, the finalizer `coder.com/ai-provider-cleanup` stays and the operator retries every 60 seconds. See [Stuck finalizer](#stuck-finalizer).
- When the control plane is gone, the operator cannot delete the provider now. The finalizer goes at once, and the operator logs the provider ID. The [cleanup record](#cleanup-records) stays, so a later sweep can delete the provider.
- When you delete the control plane while `CoderAIProvider`, `CoderAIGatewayKey` or `CoderMCPServer` objects that use it are being deleted, the control plane delete can take up to about 15 minutes. The control plane waits for the objects for up to 10 minutes, then for at most 5 more minutes (30 seconds plus 10 seconds per object of one kind) for a last bounded attempt. It keeps its finalizer, so Coder keeps running, and the delete above runs with its retries. The operator removes the workspace RBAC of the control plane before it waits, so Coder loses its Kubernetes access at once. An object that you do not delete holds the control plane for only 30 seconds, so delete the objects together with the control plane. A foreground deletion (`kubectl delete --cascade=foreground`) does not wait, and neither does a control plane without operator access.
- When the control plane does not wait (its namespace is being deleted, it is deleted in the foreground, or the 10 minutes have passed) and its operator access still works, the operator sends one delete with a 10-second limit, then removes the finalizer. Without working access, the finalizer goes without a delete. Either way a namespace deletion finishes. When the delete did not succeed, the operator logs the provider ID: delete the provider in the Coder dashboard. The metric `coder_k8s_cleanup_released_without_delete_total` (label `kind`) counts these releases.

Delete `CoderAIProvider` objects before their namespace: a namespace deletion removes Coder's pods at once, so the control plane cannot wait for them. A sweep deletes the leftover provider only when the namespace comes back with a control plane of the same Coder deployment. Coder deletes a provider even while chat models use it. Those models stop working.

### Cleanup records

Before the operator creates a provider in Coder, it writes a cleanup record: a ConfigMap named `ck8s-cleanup-<object UID>` with the label `coder.com/cleanup-record=CoderAIProvider` in the operator's namespace (`coder-system`). If it cannot write the record, it does not create the provider. The record holds no key: it holds the object's namespace, name and UID, Coder's deployment ID from `/api/v2/buildinfo`, the provider's display-name nonce, and the provider ID once the operator knows it. The deployment ID lives in the Coder database, so it names the database that holds the provider.

When the control plane later reaches another Coder deployment (for example after its database Secret changed), the record still locates the provider in the old one. The operator then stalls the object with `CoderDeploymentChanged`, keeps the record and creates nothing in the new deployment. Delete the provider in the old deployment, then delete the ConfigMap.

The operator removes the record when Coder confirms that the provider is deleted. When the finalizer goes without a delete, for example in a namespace deletion, the record stays, and the operator sweeps it:

1. It waits until the object with that UID is gone and 5 minutes have passed since the last create, so that a create still in flight commits first.
2. It looks for a ready `CoderControlPlane` in the object's namespace whose Coder answers the record's deployment ID now. It never matches by URL or name: a new Coder database behind the same URL has another deployment ID and is never swept. A control plane in another namespace never sweeps the record, because Coder reports its deployment ID itself and a control plane that someone else runs could claim any ID.
3. Through that control plane it deletes the recorded provider ID. Without a recorded ID, it deletes only the provider of that name whose display name is still the record's nonce.
4. It removes the record when Coder answers the delete with success or with its own 404. Any other answer keeps the record, and the operator retries every minute.

So after a namespace deletion with an external database, the leftover provider goes once you create the namespace again with a control plane that uses the same database. The metric `coder_k8s_cleanup_records_swept_total` (label `kind`) counts the swept records.

When no ready control plane reaches the record's deployment, the record stays. The gauge `coder_k8s_cleanup_records_unmatched` counts these records, and the operator writes a `NoMatchingDeployment` Warning Event on the ConfigMap every 10 minutes. When that Coder deployment never comes back, delete the provider in the Coder dashboard (the Event names the object, and the record holds the provider ID), then delete the ConfigMap. Find the records with `kubectl -n coder-system get configmaps -l coder.com/cleanup-record`. The operator gets write access to ConfigMaps only in that namespace (the Role `manager-role` and the RoleBinding `coder-k8s-cleanup-records`). Do not give other users write access to ConfigMaps there: a record names a Coder provider ID that a cleanup deletes. If you install the operator in another namespace, change the namespace of both.

### Stuck finalizer

A deleted `CoderAIProvider` stays `Terminating` while the operator cannot prove that Coder no longer holds its provider. Two cases never end by themselves:

- **An endless `CoderUnavailable` wait.** The `Ready` condition message starts with `delete provider <name>` or `get provider <name>`, and the operator retries every 60 seconds. Coder is unreachable, or the delete got a 404 that Coder did not send, for example an HTML page from a proxy or `Route not found.` from a wrong URL. The operator trusts only Coder's own answer `Resource not found or you do not have access to this resource`.
- **An `assertion failed:` error.** `status.providerID` is not a UUID, so the operator sends no request to Coder. The status does not change. The error is in the operator log:

  ```bash
  kubectl -n coder-system logs deploy/coder-k8s | grep -F 'assertion failed: AI provider coder/openai'
  ```

Fix the cause if you can (Coder access, the proxy, the base URL), and the next retry deletes the provider. Otherwise release the object by hand:

1. Note the provider name (the object name) and the provider ID from the condition message or from the `ControlPlaneGone` and `assertion failed:` log lines. After an `assertion failed:` error, do not trust `status.providerID`.
2. Find the index of the finalizer:

   ```bash
   kubectl -n coder get coderaiprovider openai -o jsonpath='{.metadata.finalizers}{"\n"}'
   ```

3. Remove it with a JSON patch. The `test` operation makes the patch fail, with no change, if another finalizer is at that index. Replace `0` with the index from step 2:

   ```bash
   kubectl -n coder patch coderaiprovider openai --type=json -p '[
     {"op": "test", "path": "/metadata/finalizers/0", "value": "coder.com/ai-provider-cleanup"},
     {"op": "remove", "path": "/metadata/finalizers/0"}
   ]'
   ```

4. Check Coder for a leftover provider: open **AI settings > Providers** in the dashboard (`/ai/settings/providers`) as an owner, and look for the provider name. If it is there and nothing else uses it, delete it in the dashboard.

## Grant access

Each permission in the control plane's namespace maps to Coder power:

| Permission | Coder power |
| --- | --- |
| `create`, `update`, `patch` on `coderaiproviders` | Coder owner for AI providers in the whole deployment, limited to the origins that opted-in Secrets list |
| `delete` on `coderaiproviders` | Deletes the provider in Coder, for every user of the deployment |
| `patch` on credential Secrets | Decides whether a key can be used (label) and where it can go (annotation) |
| `get`, `list` on `coderaiproviders` | Sees names, URLs, Secret names and Coder IDs. No secrets. |

A viewer Role:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: coderaiprovider-viewer
  namespace: coder
rules:
  - apiGroups: ["coder.com"]
    resources: ["coderaiproviders"]
    verbs: ["get", "list", "watch"]
```

Give the editor verbs (`create`, `update`, `patch`, `delete`) only to subjects who may act as a Coder owner for AI providers and may also read the credential Secrets. A `ResourceQuota` on `count/coderaiproviders.coder.com` limits how many providers a namespace can create.
