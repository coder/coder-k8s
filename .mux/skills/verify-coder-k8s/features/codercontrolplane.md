# Deploy Coder with a CoderControlPlane

A user applies one `CoderControlPlane` object, and the operator runs Coder for it: a Deployment and a Service with the object's name, the PostgreSQL connection from a Secret reference, and an operator user with a token Secret. The object reports `status.phase` (`Pending`, then `Ready`), `status.url`, `status.operatorAccessReady`, and the conditions `DatabaseSecretResolved`, `OperatorAccessReady`, `OperatorTokenSecretOwned` and `LicenseApplied`. Other coder-k8s objects (`CoderAIProvider`, `CoderTemplateTest`, the aggregated API) work only after `operatorAccessReady` is `true`.

## Sub-features

- Coder Deployment and Service from `spec.image`, `spec.replicas`, `spec.service`, `spec.extraEnv`, `spec.resources`.
- Database by Secret reference (`spec.database.connectionSecretRef`), injected as `CODER_PG_CONNECTION_URL` with `valueFrom`.
- Operator access: the operator user `coder-k8s-operator` and its token Secret (`status.operatorTokenSecretRef`).
- Optional license upload (`spec.licenseSecretRef`) and entitlement status (`status.licenseTier`, `status.externalProvisionerDaemonsEntitlement`).
- Spec changes roll the Coder Deployment.

## How to get to it (user POV)

Docs: `docs/tutorials/getting-started.md`, `docs/how-to/deploy-controller.md`. The user writes a `CoderControlPlane` like `examples/cloudnativepg/codercontrolplane.yaml` and a database Secret. The baseline launch already applied both (`evidence/launch-codercontrolplane.json`), so this journey checks what that apply produced and then changes the spec once.

## Driving it with kubectl

Preconditions: the baseline only (`"$V" doctor` prints `doctor: PASS`).

- **Read the phase and URL.** `"$V" capture ccp-status -- "$V" kubectl -n coder get codercontrolplane coder -o jsonpath='{.status.phase} {.status.url} {.status.operatorAccessReady}{"\n"}'` prints `Ready http://coder.coder.svc.cluster.local:80 true`.
- **Read the conditions.** `"$V" capture ccp-conditions -- "$V" kubectl -n coder get codercontrolplane coder -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'` shows `DatabaseSecretResolved=True Resolved`, `OperatorTokenSecretOwned=True TokenSecretOwned` and, without a license, `LicenseApplied=Unknown Pending`. Operator access is the field `status.operatorAccessReady`, not a condition.
- **See the owned Coder objects.** `"$V" capture ccp-children -- "$V" kubectl -n coder get deployment,service coder` shows the Deployment `coder` `1/1` ready and the Service `coder` on port `80`.
- **Check the database injection.** `"$V" capture ccp-pg-env -- bash -c '"$V" kubectl -n coder get deployment coder -o json | jq -c "[.spec.template.spec.containers[] | select(.name == \"coder\") | .env[] | select(.name == \"CODER_PG_CONNECTION_URL\")]"'` prints exactly one entry with `valueFrom.secretKeyRef` `{"name":"coder-db-app","key":"uri"}` and no `value`.
- **Check the operator token Secret.** `"$V" capture ccp-token-ref -- "$V" kubectl -n coder get codercontrolplane coder -o jsonpath='{.status.operatorTokenSecretRef}{"\n"}'` names a Secret and key. Do not print the Secret.
- **Prove operator access in Coder.** `"$V" capture coder-me -- bash -c '"$V" coder-api GET /api/v2/users/me | jq "{username, status, roles: [.roles[].name]}"'` prints the user `coder-k8s-operator`, status `active`, and the role `owner`.
- **Change the spec and watch the rollout.** `"$V" capture ccp-patch -- "$V" kubectl -n coder patch codercontrolplane coder --type=json -p '[{"op":"add","path":"/spec/extraEnv/-","value":{"name":"CODER_VERIFY_MARKER","value":"1"}}]'`, then `"$V" capture ccp-rollout -- "$V" kubectl -n coder rollout status deployment/coder --timeout=600s`. The Coder Deployment gets a new ReplicaSet (`"$V" kubectl -n coder get rs` shows the old one at 0), and `"$V" kubectl -n coder get deployment coder -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CODER_VERIFY_MARKER")].value}'` prints `1`. Afterwards `"$V" kubectl -n coder wait --for=jsonpath='{.status.operatorAccessReady}'=true codercontrolplane/coder --timeout=600s` succeeds again.
- **Check the license state without a license.** `"$V" capture ccp-license -- "$V" kubectl -n coder get codercontrolplane coder -o jsonpath='{.status.licenseTier} {.status.externalProvisionerDaemonsEntitlement}{"\n"}'` prints `none not_entitled` once the operator checked the entitlements (`status.entitlementsLastChecked` is set).

## Gotchas

- The conditions do not include `OperatorAccessReady`: read `status.operatorAccessReady`. `CoderProvisioner` objects do have an `OperatorAccessReady` condition.
- The CRD has no short name: `ccp` fails. Use `codercontrolplane`.
- A Running Coder pod is not ready. Wait for `operatorAccessReady=true`. It flips to `false` on short Postgres errors.
- The baseline Postgres uses an emptyDir. A Postgres pod restart loses the Coder database, and Coder then fails or starts empty. Do not delete the Postgres pod during a journey.
- The control plane `coder` creates the Service `coder`, so `status.url` is `http://coder.coder.svc.cluster.local:80`. A control plane with another name gets another Service name.
- Deleting namespace `coder` removes Coder at once, so `CoderAIProvider`, `CoderAIGatewayKey` and `CoderMCPServer` objects cannot clean up in Coder first. Delete them before the control plane.
- When you reset a cluster by hand instead of `cleanup`, delete an unavailable APIService before its namespace, or the namespace deletion hangs on discovery.
- In KIND, `kubernetes-admin` is in `kubeadm:cluster-admins`, not `system:masters`. Use `--as` and RBAC to test other identities.
