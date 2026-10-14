# Manage AI Gateway keys with CoderAIGatewayKey

A `CoderAIGatewayKey` keeps one Coder AI Gateway key in a Secret. The operator creates the key in Coder (Coder's `/api/v2/ai-gateway/keys`), writes it into a Secret that the object owns, rotates it, deletes replaced keys after a grace period, and deletes every key of the object when you delete the object. A standalone AI Gateway reads the key from the Secret.

!!! warning "A gateway key is a full-trust credential"
    Coder trusts every holder of a gateway key. The holder can load the API keys of every enabled AI provider, read the external-auth tokens that users gave to MCP servers, and act as any user on the gateway. Treat the output Secret like the operator token Secret. Grant `create`, `update`, `patch` and `delete` on `coderaigatewaykeys`, and `get` on the output Secrets, only to principals who may hold that power.

## Before you start

- A Coder Premium license with the AI Gateway enabled (`CODER_AI_GATEWAY_ENABLED`). Without it, Coder answers `403` and the object stalls with `LicenseRequired`.
- A `CoderControlPlane` in the same namespace with `status.operatorAccessReady: true`. The operator user must be a site owner.
- Node clocks within 2 minutes of each other, and the API server's default request timeout of 60 seconds (`--request-timeout`). The operator waits a 5-minute settle window after each key create before it treats a key as an orphan. That window holds only under these two conditions. With a longer request timeout, a late create can commit after the window, and its key then stays in Coder.

## 1. Allow keys on the control plane

The control plane admin opts in. Without this, the operator creates no key and the object stalls with `NotAllowed`. An object that never created a key makes no Coder call.

```bash
kubectl -n coder patch codercontrolplane coder --type=merge -p '{"spec":{"aiGateway":{"allowKeys":true}}}'
```

This delegates the power of a gateway key to everyone who can create `CoderAIGatewayKey` objects in the namespace. Setting `allowKeys` back to `false` stops new keys and rotations. The current key of each object stays in its Secret and in Coder until you delete the object.

## 2. Create the key

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderAIGatewayKey
metadata:
  name: gateway
  namespace: coder
spec:
  controlPlaneRef:
    name: coder
  secretName: coder-ai-gateway-key
  rotation:
    intervalSeconds: 2592000   # 30 days (default). 0 turns scheduled rotation off.
    gracePeriodSeconds: 3600   # 1 hour (default), between 10 minutes and 7 days.
    consumerWaitSeconds: 86400 # 1 day (default), at most 7 days.
```

```bash
kubectl apply -f gateway-key.yaml
kubectl -n coder wait --for=condition=Ready coderaigatewaykey/gateway --timeout=2m
```

The sample is in `config/samples/coder_v1alpha1_coderaigatewaykey.yaml`. `spec.controlPlaneRef` and `spec.secretName` cannot change. To move the key to another Secret, delete the object and create a new one.

The operator creates the Secret and owns it. It holds one data key, `key`, which is the default of the AI Gateway Helm chart. The annotations `coder.com/ai-gateway-key-id` and `coder.com/ai-gateway-key-name` name the current key and change on each rotation. The operator keeps every other label and annotation, for example reloader annotations.

The operator never writes a Secret that it does not own. An existing Secret with that name stalls the object with `SecretNotOwned`, and the Secret stays unchanged. Names that a control plane or another Coder resource in the namespace uses stall with `SecretNameReserved`.

The key names in Coder are `ck8s-<UID>-<serial>`, where `<UID>` is the object's `metadata.uid` without dashes. Status holds key IDs, names and times only, never the key or its prefix.

## 3. Use the key in a gateway

The gateway reads the key once, at startup. A new key in the Secret takes effect only when the gateway pods restart. Set these in the AI Gateway Helm chart:

- `aigateway.keySecret.name` to `spec.secretName`.
- A reloader, such as Stakater Reloader, that restarts the pods when the Secret changes, for example the pod annotation `secret.reloader.stakater.com/reload: coder-ai-gateway-key`.
- At least 2 replicas, so a restart causes no gap.

Without a reloader, run `kubectl rollout restart` on the gateway Deployment after each rotation. Without either, the operator deletes the old key when `consumerWaitSeconds` ends. Each old pod then fails its next connection, exits and restarts with the new key, so each replica has a short outage.

## 4. Read the result

```bash
kubectl -n coder get coderaigatewaykeys
kubectl -n coder get coderaigatewaykey gateway -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

The object has the conditions `Ready`, `Reconciling` and `Stalled`. `Ready=True` means that the Secret holds the current key and Coder lists it. A rotation keeps `Ready=True`, because the Secret stays valid all the time. A cleanup failure next to a valid Secret shows `Ready=True` together with `Stalled=True`, and its message starts with "The key in Secret ... still works."

| Reason | Meaning |
| --- | --- |
| `Ready` | The Secret holds the current key, and Coder lists it. |
| `IssuingKey` | The operator creates a key, or retries a create whose answer was uncertain. |
| `WaitingForGracePeriod`, `WaitingForConsumers`, `RetiringKey` | A replaced key waits for deletion. See [Rotation](#rotation). |
| `RotationBlocked` | A rotation is due, but replaced keys still wait for deletion. |
| `DeletingOrphans` | A key of this object that no status names waits for the settle window, then the operator deletes it. |
| `ControlPlaneNotReady`, `OperatorAccessNotReady` | The control plane cannot serve Coder calls yet. |
| `CoderUnavailable` | A Coder call failed. The operator retries every 30 seconds. |
| `NotAllowed` | The control plane does not allow keys. See [step 1](#1-allow-keys-on-the-control-plane). |
| `LicenseRequired`, `CoderRejected`, `NotSupported` | Coder refused: no Premium license or AI Gateway, an operator user that is not a site owner, or a Coder version without the key API. |
| `SecretNotOwned`, `SecretNameReserved`, `SecretWriteRejected` | The output Secret cannot be used. Choose another `secretName` (delete and re-create the object), or fix the admission policy that rejects the write. |
| `KeyDeletedExternally` | Someone deleted the current key in Coder. See [Kill switch](#kill-switch). |
| `SecretConflict` | The Secret lost the key again soon after a re-issue. See [Secret drift](#secret-drift). |
| `IssueFailing`, `CoderAnswerMismatch` | Key creates failed 10 times in a row, or Coder answered a create with a key that does not match the request. See [Stalls](#stalls). |
| `DeletingKeys` | The object is being deleted. See [Delete a key](#delete-a-key). |

## Rotation

A scheduled rotation is due `intervalSeconds` after the current key was written (`status.nextRotationTime`). A requested rotation starts each time `spec.rotation.trigger` changes to a new non-empty value:

```bash
kubectl -n coder patch coderaigatewaykey gateway --type=merge -p '{"spec":{"rotation":{"trigger":"2026-10-14"}}}'
```

A rotation creates a new key, writes it into the Secret, and keeps the replaced key in `status.retiringKeys`. The operator deletes a replaced key after `gracePeriodSeconds`, once Coder shows that no gateway uses it: it has no heartbeat for 10 minutes, or the new key's heartbeat is newer. While a gateway still uses the old key, the object shows `WaitingForConsumers`: restart the gateway pods. The operator deletes the old key when `consumerWaitSeconds` ends anyway.

At most 3 replaced keys wait at a time. A scheduled rotation starts only while no key waits, and a requested rotation only while fewer than 2 wait. Otherwise the object shows `RotationBlocked` and the rotation starts when a slot frees.

## Secret drift

When the Secret loses the current key (someone edits or deletes it), the operator creates a new key, writes it, and lets the old key retire with the full grace period. When the Secret loses the key again within the grace period, the object stalls with `SecretConflict`: another writer, such as a GitOps tool, likely manages the same Secret. Remove the other writer. The stall ends when the grace period after the last re-issue ends. With 3 replaced keys waiting, it ends when the operator deletes one of them.

## Kill switch

Delete the current key in Coder to revoke it at once. Gateways that use it lose their connection within 60 seconds.

```bash
coder ai-gateway keys list
coder ai-gateway keys delete <name|id>
```

The object then stalls with `KeyDeletedExternally`, and the operator creates no key until the spec changes. Edit the spec, for example change `spec.rotation.trigger`, to issue a new key. Two cases need a second spec edit: a spec change made before the operator saw the deleted key, and a spec edit that ended a `SecretNotOwned`, `SecretNameReserved` or `NotAllowed` stall while the key was already deleted.

## Stalls

A stalled object creates no key until its spec changes. It still deletes orphans and replaced keys.

- `IssueFailing`: 10 key creates in a row had an uncertain answer, or their Secret write failed. Fix the cause in the message, then edit the spec.
- `CoderAnswerMismatch`: Coder answered a create with a key that does not match the request. The created key is an orphan, and the operator deletes it after the settle window. Check the Coder version and any proxy between the operator and Coder, then edit the spec.

## Delete a key

Deleting the object deletes every key of the object in Coder, the current key included. Gateways that use them lose access within 60 seconds. The Secret goes with the object.

1. The operator waits until 5 minutes after its last key create, so that a create still in flight commits first (`DeletingKeys`).
2. It lists the keys, deletes each key of this object, and removes the finalizer `coder.com/ai-gateway-key-cleanup` only after a list shows none.

The finalizer stays while Coder cannot confirm the deletion:

| Condition | Retry |
| --- | --- |
| `CoderUnavailable`: Coder does not answer, answers `5xx` or `429`, or answers `404` that is not Coder's own (for example an HTML page from a proxy) | Every 60 seconds, with no deadline |
| `ControlPlaneNotReady`, `OperatorAccessNotReady` | Every 60 seconds |
| `LicenseRequired`, `CoderRejected`, `NotSupported` (`Stalled=True`) | Every 10 minutes |

When the control plane is gone, nothing can delete the keys. The finalizer goes at once. The operator logs `Releasing the finalizer without deleting the keys in Coder` with reason `ControlPlaneGone` and the key IDs: delete those keys with `coder ai-gateway keys delete`.

When you delete the control plane while `CoderAIGatewayKey` or `CoderAIProvider` objects that use it are being deleted, the control plane delete can take up to about 15 minutes. The control plane waits for the objects for up to 10 minutes: it keeps its finalizer, so Coder keeps running, and the objects delete their keys and providers as usual, with retries. The operator log shows `Waiting for the objects that use this control plane` with their names. The operator removes the workspace RBAC of the control plane before it waits, so Coder loses its Kubernetes access at once. An object that you do not delete holds the control plane for only 30 seconds, so delete the objects together with the control plane (or within 30 seconds). After 10 minutes, the objects that remain get at most 5 more minutes (30 seconds plus 10 seconds per object of one kind) for their bounded pass below, and then the control plane goes anyway. A foreground deletion (`kubectl delete --cascade=foreground`) does not wait, because Kubernetes then deletes the Coder Deployment first.

When the control plane does not wait (its namespace is being deleted, it is deleted in the foreground, or the 10 minutes have passed) and its operator access still works, the operator runs one pass of at most 10 seconds without the settle window, then removes the finalizer whatever the answer. A key create that was still in flight at that moment can commit later, and its key stays in Coder. When the pass fails, the log lists the IDs that it did not delete.

Each release without a delete counts in the controller metric `coder_k8s_cleanup_released_without_delete_total` (label `kind`), next to the log line.

!!! warning "Delete the keys before the namespace"
    When you delete the namespace, Kubernetes deletes the control plane and the `CoderAIGatewayKey` objects at the same time. The control plane is usually gone before the operator handles the objects, so the finalizers go without a delete, and the keys stay valid in Coder. The namespace deletion finishes, and the operator logs the key IDs with reason `ControlPlaneGone`.

Delete the `CoderAIGatewayKey` objects first and wait until they are gone, then delete the control plane or the namespace:

```bash
kubectl -n coder delete coderaigatewaykeys --all --wait --timeout=420s
```

If the namespace is already gone, delete the logged keys with `coder ai-gateway keys delete`.

### Stuck finalizer

A deletion that keeps failing never ends by itself. Leave it in one of these ways:

1. **Fix Coder or the operator user.** Restore Coder access, the Premium license or the AI Gateway, the operator user's site-owner role, or the proxy that answers `404`. The next retry deletes the keys and releases the object.
2. **Delete the control plane.** The control plane first waits up to 10 minutes for its objects, then the operator runs its one bounded pass and releases the object. Do this only when you remove that Coder deployment anyway.
3. **Remove the finalizer by hand, then delete the keys in Coder.**

   Note the key names from `status.currentKey` and `status.retiringKeys`, and the object's UID (`kubectl -n coder get coderaigatewaykey gateway -o jsonpath='{.metadata.uid}'`). Find the index of the finalizer:

   ```bash
   kubectl -n coder get coderaigatewaykey gateway -o jsonpath='{.metadata.finalizers}{"\n"}'
   ```

   Remove it with a JSON patch. The `test` operation makes the patch fail, with no change, if another finalizer is at that index. Replace `0` with the index:

   ```bash
   kubectl -n coder patch coderaigatewaykey gateway --type=json -p '[
     {"op": "test", "path": "/metadata/finalizers/0", "value": "coder.com/ai-gateway-key-cleanup"},
     {"op": "remove", "path": "/metadata/finalizers/0"}
   ]'
   ```

   Then delete every key whose name starts with `ck8s-<UID without dashes>-` with `coder ai-gateway keys delete <name>`.

## Known limits

- **Leftover keys.** A key that commits more than 5 minutes after a lost create answer, or after the object is gone, stays in Coder. So do the keys of objects that go together with their control plane, for example in a namespace deletion. Find keys of deleted objects with `coder ai-gateway keys list`: their names start with `ck8s-` and a UID that no `CoderAIGatewayKey` has.
- **A short key list.** The operator reads all keys with one list call. When a list answer names the current key but leaves out a replaced key, the operator treats the replaced key as deleted. If the key appears again, the operator deletes it as an orphan, without the rest of its grace period. Coder lists all keys in one answer, so only a proxy between the operator and Coder that changes the answer can cause this.

## Grant access

| Permission | Coder power |
| --- | --- |
| `create`, `update`, `patch` on `coderaigatewaykeys` (with `allowKeys: true`) | Mint full-trust gateway keys with the operator token |
| `delete` on `coderaigatewaykeys` | Revokes the object's keys for every gateway that uses them |
| `get` on the output Secret | Holds a full-trust gateway key |
| `get`, `list` on `coderaigatewaykeys` | Sees Secret names, key names, Coder key IDs and times. No secrets. |

A viewer Role:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: coderaigatewaykey-viewer
  namespace: coder
rules:
  - apiGroups: ["coder.com"]
    resources: ["coderaigatewaykeys"]
    verbs: ["get", "list", "watch"]
```
