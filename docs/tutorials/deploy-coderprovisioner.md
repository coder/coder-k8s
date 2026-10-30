# Deploy an External Provisioner

In this tutorial, you add a `CoderProvisioner` to a control plane that already exists. A `CoderProvisioner` runs external provisioner daemons for Coder.

Time: 5 minutes.

!!! warning "Who may write CoderProvisioner resources"
    Grant `create`, `update` and `patch` on `coderprovisioners` only to principals that may already read Secrets and create Pods in that namespace. A custom `spec.image` runs with everything the pod receives: the provisioner key, every `extraEnv` value, and the ServiceAccount token that manages workspace pods. See [Configure credential Secrets for proxies and provisioners](../how-to/workspace-proxy-credentials.md).

## Prerequisites

Complete [Deploy a Coder Control Plane](getting-started.md) first. The control plane `codercontrolplane-sample` in the namespace `coder` must be `Ready`, and its operator access must be ready (the default).

Make sure that the phase is `Ready` and that `operatorAccessReady` is `true`:

```bash
kubectl get codercontrolplane codercontrolplane-sample -n coder \
  -o jsonpath='{.status.phase}{"\n"}{.status.operatorAccessReady}{"\n"}'
```

Expected output:

```text
Ready
true
```

## 1. Check the license entitlement

External provisioners require the applicable Coder license entitlement. Show its value:

```bash
kubectl get codercontrolplane codercontrolplane-sample -n coder \
  -o jsonpath='{.status.externalProvisionerDaemonsEntitlement}{"\n"}'
```

- If the value is `entitled` or `grace_period`, continue to step 2.
- If the value is `not_entitled`, update the license of the control plane first.

## 2. Deploy the provisioner

```bash
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/samples/coder_v1alpha1_coderprovisioner.yaml"
```

## 3. Verify

```bash
kubectl get coderprovisioner coderprovisioner-sample -n coder \
  -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Expected result: the phase is `Ready`, and the conditions include `CredentialsReady=True` and `DeploymentReady=True`.

The operator creates the provisioner key Secret and owns it. If you set `spec.key.secretName`, name a Secret that does not exist yet: the operator refuses a Secret that it does not own (reason `KeySecretNotOwned`).

Each provisioner needs its own key name in its Coder organization. The key name is `spec.key.name`, or the provisioner name when `spec.key.name` is empty. Before the operator creates or replaces a key, it looks up the key name in Coder. When Coder holds a key with that name, and the status of this provisioner does not record its ID, the operator does not delete or replace that key, because another `CoderProvisioner` (for example one with the same name in another namespace) or a person can use it. It then changes nothing for this provisioner: `ProvisionerKeyReady` is `False` with reason `KeyNameInUse`, and the message names the key and its ID. Set `spec.key.name` to a name that nothing else uses, or delete that key in Coder if nothing uses it. The operator checks again every 2 minutes. The same happens when the operator lost the answer to a key create, because status then has no ID of the new key, and for status without key records, for example after a restore without status.

If you add `spec.extraEnv` entries with a `secretKeyRef`, each Secret needs the label `coder.com/credential=true` and the annotation `coder.com/credential-destinations` with the origin of the control plane `status.url`. Otherwise `CredentialsReady` is `False` and the operator does not change the Deployment. See [Configure credential Secrets for proxies and provisioners](../how-to/workspace-proxy-credentials.md).

The operator creates these resources in the `coder` namespace:

| Kind | Name |
| --- | --- |
| Deployment | `provisioner-coderprovisioner-sample` |
| ServiceAccount | `coderprovisioner-sample-provisioner` |
| Role | `provisioner-coderprovisioner-sample` |
| RoleBinding | `provisioner-coderprovisioner-sample` |
| Secret (provisioner key) | `coderprovisioner-sample-provisioner-key` |

```bash
kubectl get deployment,role,rolebinding provisioner-coderprovisioner-sample -n coder
kubectl get sa coderprovisioner-sample-provisioner -n coder
kubectl get secret coderprovisioner-sample-provisioner-key -n coder
```

## 4. Clean up (optional)

```bash
kubectl delete coderprovisioner coderprovisioner-sample -n coder
```

The operator then deletes the provisioner keys in Coder that this provisioner created. It finds the key in the provisioner key Secret by the organization and key IDs in `status.organizationID` and `status.provisionerKeyID`, not by the names in the spec. It also deletes the key of each entry in `status.pendingKeys` that has an `organizationID` and a `keyID`. The entries name keys that the operator created but did not write to the Secret, for example because a reconcile failed, and keys that a rotation replaced but that Coder did not confirm as deleted. Each entry names the Coder URL of the key and the control plane whose operator token the operator uses to delete it. An entry without these IDs names a key that the operator started to create before Coder returned its ID. For such an entry, the operator deletes no key, because another provisioner or a person can use the same key name in that organization. It removes the entry when Coder has no key with that name, or when that key is the key in `status.provisionerKeyID`. Otherwise, and while the operator cannot reach Coder for that check, the entry stays, the condition `UnverifiedKeyKept` is `True` and names the entry, and deletion of the provisioner leaves the key in Coder. If no other provisioner or person uses the key, delete it with `coder provisioner keys delete <name> --org <organization>`. The operator checks again every 2 minutes, and removes the entry and the condition once the key is gone. When the status has no key, for example because the provisioner never passed its checks, the operator removes its finalizer without a call to Coder.

The same records apply while the provisioner runs. When `spec.controlPlaneRef`, the control plane URL, the organization, the key name, or the tags change, the operator creates a new key and deletes the replaced keys, each at its own Coder URL. When a rotation fails after Coder created the new key, the next reconcile writes another new key to the Secret, also when you reverted the change. An entry stays in `status.pendingKeys` until Coder confirms that its key is gone, and the operator tries again every 2 minutes. Each entry and `status.controlPlaneUID` also record the UID of the control plane. The operator sends the operator token of a control plane for a record only while the control plane has the recorded UID. When a control plane is deleted and created again under the same name, the new one can run another Coder backend, also at the same URL. The operator then creates a new key for the provisioner, and sends the new token for no key of the old control plane. Such an entry stays, the condition `ControlPlaneChangedKeyKept` is `True` and names it, and deletion of the provisioner leaves that key in Coder. A record without a UID, from an operator version that did not record it, counts as bound to its control plane only at the current URL of that control plane. For status without `status.controlPlaneUID`, the next reconcile looks the applied key up at the current control plane. When Coder holds the key there, the operator records the UID of that control plane. Otherwise it creates a new key. An entry whose control plane was deleted, or that `ControlPlaneChangedKeyKept` names, stays for good: delete its key by hand in Coder, then remove the entry, for example the first one with `kubectl patch coderprovisioner coderprovisioner-sample -n coder --subresource=status --type=json -p '[{"op":"remove","path":"/status/pendingKeys/0"}]'`.

This cleanup is best-effort. When the operator cannot reach Coder, for example because the control plane or its operator token is gone, or when the delete call fails, the operator still removes its finalizer, and the key stays valid in Coder. Delete the provisioner while the control plane runs. If cleanup could not reach Coder, list the keys with `coder provisioner keys list --org <organization>`, and delete the key by hand with `coder provisioner keys delete <name> --org <organization>`.

To remove all the other resources, do the steps in [the control plane cleanup](getting-started.md#5-clean-up-optional).
