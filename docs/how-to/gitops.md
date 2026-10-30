# Gate template promotion with GitOps

This guide shows how to make Argo CD or Flux wait for a [`CoderTemplateTest`](test-templates.md) result, and how to promote a template version only after its test passed. The health rules live in `config/gitops/` in this repository. This page copies them, and a unit test keeps the copies equal to the files.

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

The controller sets `Succeeded` only after the test workspace is deleted, and always together with `Ready=True`. While a test is being deleted, Argo CD shows `Progressing` with the message `Deleting the test workspace`, which the script returns as `deletionMessage`. Argo CD versions without `deletionMessage` support show their default deletion message instead. Otherwise its message is `<status.reason>: <status.message>`.

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
  -- Argo CD shows deletionMessage, not message, for a resource being deleted.
  hs.deletionMessage = "Deleting the test workspace"
  hs.message = hs.deletionMessage
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

Set `spec.timeout` of the Kustomization above the test's `timeoutSeconds` (default 900 s) plus the time that the controller needs to start or recover, or Flux stops waiting before the test ends. The Flux timeout starts with the reconciliation, but the test's clock starts only when the controller starts the test and sets `status.startTime`.

## Patterns

### One test per version

Create one test per template version, and name it after the version. The spec is immutable, so a new version needs a new object anyway, and the name shows which version a result belongs to. Object names allow only lowercase letters, digits, `-`, and `.`, but a version name can also hold uppercase letters and `_`. `promote.sh name` of the [promotion example](#run-the-promotion-example) maps any valid version name to a safe name. It lowercases the name, replaces `_` and `.` with `-`, and appends 8 characters of the SHA-256 hash of the exact version name, so `V2` and `v2` get different names:

```sh
$ examples/gitops-promotion/promote.sh name docker V2.0_rc1
docker-v2-0-rc1-1c72012b
```

Do not set `ttlSecondsAfterFinished` on tests that GitOps manages. The controller deletes the finished test, GitOps creates it again, and a new workspace starts, forever.

### Gate before promotion

1. CI pushes the new version under a fixed name, without activating it and without prompts: `coder templates push docker --directory ./docker --name v2 --activate=false --yes`.
2. Git holds a test of that version, with `spec.version.name`.
3. A promotion step waits until the test is final. Only after `Succeeded`, it reads `status.templateVersionID` of the test and calls the `codertemplates/promote` subresource of the [aggregated API](../reference/aggregated-api-behavior.md#promote-a-template-version). Promoting the active version again answers `AlreadyActive`. After a `409` or a `503`, the step must read the active version before it tries again, because another writer can have activated a third version, and the last writer wins. See the confirmation rules of the [promote subresource](../reference/aggregated-api-behavior.md#promote-a-template-version).

With **Argo CD**, put the test in sync wave 1 and the promotion step in wave 2. Argo CD applies wave 2 only after every resource of wave 1 is healthy. With **Flux**, a plain `dependsOn` does not wait until the dependency has applied the same Git revision, so the promotion step can start while the previous test still shows its result. The promotion step must therefore wait for its own test.

Any promotion step must check these points. The [promotion example](#run-the-promotion-example) checks all of them:

- **Full spec.** The test's whole `spec`, including `spec.parameters`, equals the spec in Git. Read spec and status with one `get`, so both come from the same object. Anyone who can create tests can create the test name first with another spec.
- **Timeout.** The wait and the deadline of the step cover the test's `timeoutSeconds` (900 s by default, at most 7200 s) and the workspace delete.
- **Names.** Test names and the names of promotion objects, such as a Job per version, are Kubernetes-safe, as described in [One test per version](#one-test-per-version).
- **Result.** The pipeline waits for the promotion result and shows a failed promotion as a failure. With Flux, set `wait: true` and a `timeout` above the step's deadline on the Kustomization that runs the step.

### Run the promotion example

The directory `examples/gitops-promotion/` in this repository is a Kustomize directory for one template, `default.docker` in namespace `coder`. Argo CD and Flux both render it with Kustomize. It holds:

| File | Content |
| --- | --- |
| `codertemplatetest.json` | The test of one version, in sync wave 1. It is JSON, so the Job can read it with `jq`. |
| `promote.sh` | The promotion script. It needs `bash`, `kubectl`, and `jq`. `promote.sh name` also needs `sha256sum` or `shasum`. |
| `promote-job.yaml` | A Job that runs `promote.sh promote`. For Argo CD, it is a `Sync` hook in wave 2, and Argo CD deletes it after a success. |
| `rbac.yaml` | A ServiceAccount that can `get` only this test and `create` only `codertemplates/promote` of the test's template. |
| `kustomization.yaml` | Puts `promote.sh` and `codertemplatetest.json` into a ConfigMap for the Job. Copies the test name to the Job and the Role, the template to the Role, and the namespace to every object. |
| `argocd-application.yaml`, `flux-kustomization.yaml` | An Argo CD Application and a Flux Kustomization for the directory. Apply one of them, not both. |

The Job reads the test in Git from its ConfigMap. It reads the test in the cluster with one `get` every 30 s, and then does one of these:

| The test in the cluster | Result |
| --- | --- |
| Its `spec` differs from the spec in Git. The API server default `timeoutSeconds: 900`, the order of `spec.parameters`, and an omitted versus an empty parameter `value` do not count. | The Job fails at once and does not promote. |
| It does not exist, is being deleted, has a stale `status.observedGeneration`, or is `Pending` or `Running` | The Job waits. |
| `Failed` | The Job fails and does not promote. Its log shows `status.reason` and `status.message`. |
| `Succeeded` with `Ready=True` | The Job promotes `status.templateVersionID` of that object and completes when the answer is `Promoted` or `AlreadyActive`. |

The Job waits at most the test's `timeoutSeconds` plus 900 s for the controller to start the test. The controller writes `Succeeded` only after the workspace delete, and `Failed` at the latest at the test deadline, so this wait covers the delete. The Job's `activeDeadlineSeconds` is 8400: 7200 s, the longest `timeoutSeconds`, plus the 900 s, plus 300 s for the promotion. The Flux Kustomization has `wait: true` and `timeout: 2h30m`, so Flux is ready only after the Job completed, and it fails when the Job fails. It also has `force: true`: a change of `promote.sh` changes the pod template of the Job, which Kubernetes cannot update, so Flux replaces the Job, and the new Job promotes again. Argo CD waits for the hook Job and fails the sync when the Job fails.

The Job has `backoffLimit: 0`, and `promote.sh` sends the promotion only once. After a failed promotion, read the active version before you sync again.

To use the example:

1. Install the [Argo CD health check](#install-the-argo-cd-health-check) if you use Argo CD. With Flux, the Job is the gate.
2. Copy the directory into your Git repository. Set the control plane, the template, the namespace, and the parameters of your test in `codertemplatetest.json`. `spec.template` must be the exact name that `kubectl get codertemplates` shows, because the promote subresource refuses other casing with `400`. For a second template in the same namespace, also replace the name `docker-promote` in `rbac.yaml`, `promote-job.yaml`, and `kustomization.yaml`. Set `repoURL` or `sourceRef` and `path` in the Argo CD or Flux manifest, and apply it.
3. For each new version, CI pushes the version without activating it, sets the version and the test name in `codertemplatetest.json`, and commits the file. Git then holds a new test and a new Job, both with the new name.

The CI commands for step 3, in the copied directory:

```sh
coder templates push docker --directory ./docker --name V2.0_rc1 --activate=false --yes
name=$(./promote.sh name docker V2.0_rc1)
jq --arg name "$name" --arg version V2.0_rc1 \
  '.metadata.name = $name | .spec.version.name = $version' \
  codertemplatetest.json >codertemplatetest.json.new
mv codertemplatetest.json.new codertemplatetest.json
```

The Job promotes with the rights of its ServiceAccount. Anyone who can change ConfigMaps, Jobs, or Pods in the namespace can change what it runs. A test creator who has only the test rights from [Test templates](test-templates.md) cannot change it.

### Check after promotion

A `CoderTemplate` update of `spec.files` activates the new version at once, before any test. A test with `spec.version.active: true` in a later wave can still detect breakage, but only after users got the version.

If the import takes longer than the 34-second write budget of the aggregated API ([#117](https://github.com/coder/coder-k8s/issues/117)), the update fails and the previous version stays active, so the test resolves the previous version. `status.templateVersionName` shows which version ran.

## What CI covers

The unit test in `config/gitops/` runs the Lua script with gopher-lua, the Lua engine of Argo CD, and the Flux expressions with cel-go, both over fixed test objects. The unit test in `examples/gitops-promotion/` renders the example with Kustomize and runs `promote.sh` against a fake `kubectl`. Its cases include a test that someone created first with other parameters, which the script must refuse, and a test that runs for the longest `timeoutSeconds`. CI does not run Argo CD or Flux.
