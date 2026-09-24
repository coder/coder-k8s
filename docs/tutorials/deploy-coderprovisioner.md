# Deploy an External Provisioner

Add a `CoderProvisioner` (an external provisioner daemon) to an existing control plane.

**Time:** 5 minutes.

## Prerequisites

Finish [Deploy a Coder Control Plane](getting-started.md). You need `codercontrolplane-sample` in namespace `coder`, `Ready`, with operator access ready (the default).

Check:

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

External provisioners need the matching Coder license entitlement:

```bash
kubectl get codercontrolplane codercontrolplane-sample -n coder \
  -o jsonpath='{.status.externalProvisionerDaemonsEntitlement}{"\n"}'
```

- `entitled` or `grace_period`: continue.
- `not_entitled`: update the control plane's license first.

## 2. Deploy the provisioner

```bash
kubectl apply -f "https://raw.githubusercontent.com/coder/coder-k8s/main/config/samples/coder_v1alpha1_coderprovisioner.yaml"
```

## 3. Verify

```bash
kubectl get coderprovisioner coderprovisioner-sample -n coder \
  -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Expected: phase `Ready` and `DeploymentReady=True`.

The operator creates these resources in `coder`:

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

To remove everything, follow [the control plane cleanup](getting-started.md#5-clean-up-optional).
