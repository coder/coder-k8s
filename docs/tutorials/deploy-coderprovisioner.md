# Deploy an External Provisioner

In this tutorial, you add a `CoderProvisioner` to a control plane that already exists. A `CoderProvisioner` runs external provisioner daemons for Coder.

Time: 5 minutes.

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

Expected result: the phase is `Ready`, and the conditions include `DeploymentReady=True`.

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

To remove all the other resources, do the steps in [the control plane cleanup](getting-started.md#5-clean-up-optional).
