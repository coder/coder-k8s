# Gate template promotion with GitOps

This guide shows how to make Argo CD or Flux wait for a [`CoderTemplateTest`](test-templates.md) result, and how to promote a template version only after its test passed. The health rules live in [`config/gitops/`](https://github.com/coder/coder-k8s/tree/main/config/gitops). This page copies them, and a unit test keeps the copies equal to the files.

## Why custom health rules are needed

A GitOps tool decides from health rules when a resource is ready.

- **Argo CD** has no built-in health check for `CoderTemplateTest`. Without one, it cannot use the test result to hold back a later sync wave.
- **Flux** falls back to kstatus. kstatus reports a resource without conditions as current. A new test has no status until the controller writes the first one, and that lasts as long as the controller is down. In that window, Flux treats the test as passed.

Both rules below wait until `status.observedGeneration` equals `metadata.generation`. The controller sets `status.observedGeneration` on every status write. The rules then map `status.phase`:

| `status.phase` | Argo CD | Flux |
| --- | --- | --- |
| no status, or a stale `observedGeneration` | `Progressing` | `InProgress` |
| `Pending`, `Running` | `Progressing` | `InProgress` |
| `Succeeded` with condition `Ready=True` | `Healthy` | `Current` |
| `Failed` | `Degraded` | `Failed` |

The controller sets `Succeeded` only after the test workspace is deleted, and always together with `Ready=True`. While a test is being deleted, Argo CD shows `Progressing` with the message `Deleting the test workspace`. Otherwise its message is `<status.reason>: <status.message>`.

## Install the Argo CD health check

Add the script as the `argocd-cm` key `resource.customizations.health.coder.com_CoderTemplateTest`. If Git manages your `argocd-cm`, add the key there. Otherwise, patch the ConfigMap from the file:

```sh
kubectl -n argocd patch configmap argocd-cm --type merge -p "$(
  jq -n --rawfile lua config/gitops/argocd-health-codertemplatetest.lua \
    '{data: {"resource.customizations.health.coder.com_CoderTemplateTest": $lua}}')"
```

The script (`config/gitops/argocd-health-codertemplatetest.lua`):

```lua
-- Argo CD health check for CoderTemplateTest (coder.com/v1alpha1).
-- Install it as the argocd-cm key
-- resource.customizations.health.coder.com_CoderTemplateTest.
-- docs/how-to/gitops.md copies this file. A test keeps the copy equal.
local hs = { status = "Progressing", message = "Waiting for the controller" }
if obj.metadata.deletionTimestamp ~= nil then
  hs.message = "Deleting the test workspace"
  return hs
end
local st = obj.status
if st == nil or st.observedGeneration == nil or st.observedGeneration ~= obj.metadata.generation then
  return hs
end
if st.reason ~= nil and st.message ~= nil then
  hs.message = st.reason .. ": " .. st.message
end
if st.phase == "Succeeded" then
  for _, c in ipairs(st.conditions or {}) do
    if c.type == "Ready" and c.status == "True" then
      hs.status = "Healthy"
    end
  end
elseif st.phase == "Failed" then
  hs.status = "Degraded"
end
return hs
```

## Install the Flux health check

Flux v2.5 and later evaluate `healthCheckExprs` of a Kustomization with `spec.wait: true`. Add this entry (`config/gitops/flux-healthcheck-codertemplatetest.yaml`) to `spec.healthCheckExprs`:

```yaml
# Flux health check expressions for CoderTemplateTest (coder.com/v1alpha1).
# Add this entry to spec.healthCheckExprs of a Flux v2.5+ Kustomization
# with spec.wait: true.
# docs/how-to/gitops.md copies this file. A test keeps the copy equal.
- apiVersion: coder.com/v1alpha1
  kind: CoderTemplateTest
  inProgress: "!has(status.observedGeneration) || status.observedGeneration != metadata.generation || !has(status.phase) || status.phase in ['Pending', 'Running']"
  failed: "status.phase == 'Failed'"
  current: "status.phase == 'Succeeded' && status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')"
```

Flux evaluates `inProgress`, then `failed`, then `current`, and the first true expression wins. Before the controller writes the first status, the expressions cannot read `status`. Flux then reports the test as `Unknown` and keeps waiting, like `InProgress`.

Set `spec.timeout` of the Kustomization above the test's `timeoutSeconds` (default 900 s), or Flux stops waiting before the test ends.

## Patterns

### One test per version

Create one test per template version, and name it after the version, for example `docker-v2` for version `v2`. The spec is immutable, so a new version needs a new object anyway, and the name shows which version a result belongs to.

Do not set `ttlSecondsAfterFinished` on tests that GitOps manages. The controller deletes the finished test, GitOps creates it again, and a new workspace starts, forever.

### Gate before promotion

1. CI pushes the new version without activating it: `coder templates push --activate=false`.
2. Git holds a test of that version, with `spec.version.name`.
3. A promotion Job runs only after the test is healthy. It reads `status.templateVersionID` of the test and calls the `codertemplates/promote` subresource of the [aggregated API](../reference/aggregated-api-behavior.md#promote-a-template-version).

The Job needs only `get` on its test and `create` on `codertemplates/promote` for one template:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: promote-docker
  namespace: coder
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: promote-docker
  namespace: coder
rules:
  - apiGroups: ["coder.com"]
    resources: ["codertemplatetests"]
    resourceNames: ["docker-v2"]
    verbs: ["get"]
  - apiGroups: ["aggregation.coder.com"]
    resources: ["codertemplates/promote"]
    resourceNames: ["default.docker"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: promote-docker
  namespace: coder
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: promote-docker
subjects:
  - kind: ServiceAccount
    name: promote-docker
    namespace: coder
---
apiVersion: batch/v1
kind: Job
metadata:
  name: promote-docker-v2
  namespace: coder
spec:
  backoffLimit: 4
  template:
    spec:
      serviceAccountName: promote-docker
      restartPolicy: Never
      containers:
        - name: promote
          image: <an image with sh and kubectl>
          command:
            - sh
            - -ec
            - |
              phase=$(kubectl -n coder get codertemplatetest docker-v2 -o jsonpath='{.status.phase}')
              test "$phase" = Succeeded
              id=$(kubectl -n coder get codertemplatetest docker-v2 -o jsonpath='{.status.templateVersionID}')
              test -n "$id"
              echo '{"spec":{"versionID":"'"$id"'"}}' | kubectl create --raw \
                /apis/aggregation.coder.com/v1alpha1/namespaces/coder/codertemplates/default.docker/promote -f -
```

A retry is safe: promoting the active version again answers `AlreadyActive`. Name the Job after the version too, because the spec of a Job is immutable.

With **Argo CD**, put the test in sync wave 1 and the promotion resources in wave 2. Argo CD applies wave 2 only after every resource of wave 1 is healthy:

```yaml
metadata:
  annotations:
    argocd.argoproj.io/sync-wave: "1"   # "2" on the RBAC objects and the Job
```

With **Flux**, use two Kustomizations. Kustomization A applies the test, with `wait: true` and the health check above. Kustomization B applies the RBAC objects and the Job, with `dependsOn` A:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: docker-promote
  namespace: flux-system
spec:
  dependsOn:
    - name: docker-test   # Kustomization A
  interval: 10m
  path: ./templates/docker/promote
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
```

### Check after promotion

A `CoderTemplate` update of `spec.files` activates the new version at once, before any test. A test with `spec.version.active: true` in a later wave can still detect breakage, but only after users got the version.

If the import takes longer than the 34-second write budget of the aggregated API ([#117](https://github.com/coder/coder-k8s/issues/117)), the update fails and the previous version stays active, so the test resolves the previous version. `status.templateVersionName` shows which version ran.

## What CI covers

The unit test in `config/gitops/` runs the Lua script with gopher-lua, the Lua engine of Argo CD, and the Flux expressions with cel-go, both over fixed test objects. CI does not run Argo CD or Flux.
