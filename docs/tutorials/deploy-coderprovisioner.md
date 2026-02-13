# Deploy an External Provisioner

This tutorial deploys a `CoderProvisioner` for an existing `CoderControlPlane` managed by `coder-k8s`.

Estimated time: **5 minutes**.

## Prerequisite

Complete [Deploy a Coder Control Plane](getting-started.md) first.

This tutorial assumes the setup from that guide:

- `CoderControlPlane` **`codercontrolplane-sample`** exists in namespace **`coder`** and is `Ready`.
- Operator access is enabled and ready (default behavior):
  `.status.operatorAccessReady=true`.

Quick check:

```bash
kubectl get codercontrolplane codercontrolplane-sample -n coder \
  -o jsonpath='{.status.phase}{"\n"}{.status.operatorAccessReady}{"\n"}'
```

Expected output:

```text
Ready
true
```

## 1) Confirm external provisioner entitlement

`CoderProvisioner` requires external provisioner entitlement in the referenced Coder deployment.

```bash
kubectl get codercontrolplane codercontrolplane-sample -n coder \
  -o jsonpath='{.status.externalProvisionerDaemonsEntitlement}{"\n"}'
```

Expected values to proceed: `entitled` or `grace_period`.

If the value is `not_entitled`, update the control-plane license before continuing.

## 2) Deploy the `CoderProvisioner`

Apply the sample manifest:

```bash
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/samples/coder_v1alpha1_coderprovisioner.yaml"
```

## 3) Verify reconciliation

```bash
kubectl get coderprovisioner coderprovisioner-sample -n coder
kubectl get coderprovisioner coderprovisioner-sample -n coder -o jsonpath='{.status.phase}{"\n"}'
kubectl get coderprovisioner coderprovisioner-sample -n coder -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Verify generated resources:

```bash
kubectl get deployment provisioner-coderprovisioner-sample -n coder
kubectl get sa coderprovisioner-sample-provisioner -n coder
kubectl get role provisioner-coderprovisioner-sample -n coder
kubectl get rb provisioner-coderprovisioner-sample -n coder
kubectl get secret coderprovisioner-sample-provisioner-key -n coder
```

Expected: `status.phase=Ready`, `DeploymentReady=True`, and a ready provisioner pod.

## 4) Clean up (optional)

```bash
kubectl delete coderprovisioner coderprovisioner-sample -n coder
```

To tear down the full stack, follow cleanup in
[Deploy a Coder Control Plane](getting-started.md#5-clean-up-optional).
