# External provisioners with CoderProvisioner

A user adds external provisioner daemons to a Coder control plane by applying one `CoderProvisioner` object. When the Coder deployment is entitled to external provisioners, the operator creates a provisioner key in Coder, stores it in a Secret that it owns, and runs a Deployment of `coder provisionerd` pods with their own ServiceAccount, Role and RoleBinding. The object reports `status.phase` and the conditions `ControlPlaneReady`, `OperatorAccessReady`, `CredentialsReady`, `ExternalProvisionersEntitled`, `ProvisionerKeyReady`, `ProvisionerKeySecretReady` and `DeploymentReady`. Deleting the object deletes the key in Coder.

## Sub-features

- Entitlement gate: `ExternalProvisionersEntitled` from Coder's `/api/v2/entitlements`.
- Provisioner key in Coder (`spec.key.name`) and its owned Secret (`spec.key.secretName`, default `<name>-provisioner-key`).
- Deployment `provisioner-<name>`, ServiceAccount `<name>-provisioner`, Role and RoleBinding `provisioner-<name>`.
- Tags (`spec.tags`), replicas, image, `extraArgs`, `extraEnv` with credential-checked `secretKeyRef`s.
- Delete: the operator deletes the key by `status.organizationID` and `status.provisionerKeyID`.

## How to get to it (user POV)

Docs: `docs/tutorials/deploy-coderprovisioner.md`. Sample: `config/samples/coder_v1alpha1_coderprovisioner.yaml`. The user checks `status.externalProvisionerDaemonsEntitlement` of the control plane, then applies the sample with `controlPlaneRef.name` set to the control plane.

## Driving it with kubectl

Preconditions: the baseline plus a Coder Premium license. The baseline has no license, so without one this journey reports `FAIL: precondition missing: Premium license` after the entitlement step. To add a license, set `CODER_LICENSE_FILE` to the path of the license JWT file (outside the repo), put it in a Secret and point the control plane at it: `"$V" kubectl -n coder create secret generic coder-license --from-file=license="$CODER_LICENSE_FILE"` and `"$V" kubectl -n coder patch codercontrolplane coder --type=merge -p '{"spec":{"licenseSecretRef":{"name":"coder-license","key":"license"}}}'`, then wait for `LicenseApplied=True`.

- **Check the entitlement.** `"$V" capture prov-entitlement -- "$V" kubectl -n coder get codercontrolplane coder -o jsonpath='{.status.externalProvisionerDaemonsEntitlement}{"\n"}'` prints `entitled` or `grace_period` with a license, and `not_entitled` without one. `"$V" capture prov-coder-entitlement -- bash -c '"$V" coder-api GET /api/v2/entitlements | jq -c .features.external_provisioner_daemons'` shows the same value from Coder.
- **Apply the provisioner.** `"$V" capture prov-apply -- bash -c 'sed "s/codercontrolplane-sample/coder/; s/name: coderprovisioner-sample/name: verify-provisioner/" config/samples/coder_v1alpha1_coderprovisioner.yaml | "$V" kubectl apply -f -'` prints `coderprovisioner.coder.com/verify-provisioner created`.
- **Read the conditions.** `"$V" capture prov-status -- "$V" kubectl -n coder get coderprovisioner verify-provisioner -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'`. With a license: phase `Ready`, `CredentialsReady=True` and `DeploymentReady=True`. Without one: an empty phase, `ControlPlaneReady=True ControlPlaneAvailable`, `CredentialsReady=True CredentialsAccepted`, `OperatorAccessReady=True` and `ExternalProvisionersEntitled=False NotEntitled`. Stop here and report the failure.
- **See the owned objects.** `"$V" capture prov-children -- "$V" kubectl -n coder get deployment,role,rolebinding provisioner-verify-provisioner` and `"$V" kubectl -n coder get sa verify-provisioner-provisioner` and `"$V" kubectl -n coder get secret verify-provisioner-provisioner-key -o name` (name only, never the data) show each object. The Deployment is `1/1` ready.
- **Prove the key and daemon in Coder.** `"$V" capture prov-coder-keys -- bash -c '"$V" coder-api GET /api/v2/organizations/coder/provisionerkeys | jq -c "[.[] | {id, name, tags}]"'` lists a key named `verify-provisioner` whose `id` equals `status.provisionerKeyID`. `"$V" capture prov-coder-daemons -- bash -c '"$V" coder-api GET /api/v2/organizations/coder/provisionerdaemons | jq -c "[.[] | {name, key_name, status}]"'` lists a daemon with `key_name` `verify-provisioner`.
- **Delete it.** `"$V" capture prov-delete -- "$V" kubectl -n coder delete coderprovisioner verify-provisioner --timeout=2m`. The key list in Coder no longer has `verify-provisioner`, and the Deployment and Secret are gone.

## Gotchas

- Without a license the operator never creates the key, the Secret or the Deployment, and `status.phase` stays empty. That is the documented gate, so the journey is a failure with the reason `precondition missing: Premium license`, not a pass.
- After a `NotEntitled` result the operator checks the entitlement again only after the retry interval, so a license added later takes effect at the next check, not at once.
- A `spec.key.secretName` that names an existing Secret not owned by the provisioner stalls with `KeySecretNotOwned`.
- An `extraEnv` `secretKeyRef` needs the label `coder.com/credential=true` and the destination annotation with the origin of the control plane `status.url` (`http://coder.coder.svc.cluster.local`), or `CredentialsReady` turns `False`.
- Unproven here: the licensed half of this journey (from "See the owned objects" on). The 2026-10-29 check ran only the unlicensed half, because the baseline has no license. The licensed steps follow `docs/tutorials/deploy-coderprovisioner.md` and the Coder routes `/api/v2/organizations/{organization}/provisionerkeys` and `/provisionerdaemons`.
- Do not commit or capture the license JWT. Keep it outside the repo, and pass it only with `--from-file`.
