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

The operator then deletes the provisioner key in Coder that this provisioner created. It finds the key by the organization and key IDs in `status.organizationID` and `status.provisionerKeyID`, not by the names in the spec. When the status has no key, for example because the provisioner never passed its checks, the operator removes its finalizer without a call to Coder.

To remove all the other resources, do the steps in [the control plane cleanup](getting-started.md#5-clean-up-optional).
