# Test a template version with CoderTemplateTest

This guide shows how to check that a Coder template version can start a workspace whose agents become ready. A `CoderTemplateTest` creates a throwaway workspace from one template version, waits until every top-level agent is `connected` and `ready`, records `Succeeded` or `Failed`, and deletes the workspace. For every field, see the [`CoderTemplateTest` reference](../reference/api/codertemplatetest.md).

You need:

- a `CoderControlPlane` whose `status.operatorAccessReady` is `true` (see [Deploy the controller](deploy-controller.md)),
- a Coder user that owns the test workspaces (the tester), described in step 1.

## 1. Create the tester user

Every start build hands the owner's token to Terraform, so the controller refuses an owner whose token is worth more than a plain workspace user's. The tester must:

- have status `active`. Coder creates users through its API as `dormant` and refuses a dormant owner's agents, so activate the tester first,
- be a service account or a password user (login type `password` or `none`), never a person's OIDC or GitHub login,
- have no site role other than `member`,
- have only the `organization-member` or `organization-workspace-access` role in each of its organizations, and no organization may grant other roles by default.

Point the control plane at the tester's user ID:

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderControlPlane
metadata:
  name: coder
  namespace: coder
spec:
  templateTests:
    ownerUserID: 6f1c...   # the tester's Coder user ID
```

The operator user (the control plane's operator token) sends every Coder request. It needs the site `owner` role: only a site owner can read every workspace, so only its "not found" answer proves that no workspace exists.

## 2. Run a test

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderTemplateTest
metadata:
  generateName: docker-
  namespace: coder
spec:
  controlPlaneRef:
    name: coder
  template: default.docker      # <organization>.<template>
  version:
    active: true                # or name: v2, or id: <uuid>
  timeoutSeconds: 900           # the default
  ttlSecondsAfterFinished: 3600 # optional
```

Create it with `kubectl create -f`, then watch it:

```bash
kubectl -n coder get codertemplatetests -w
```

The spec is immutable. To test again, create a new object.

## 3. Grant access

The operator's own RBAC comes with the install. People who run tests need `create`, `get`, `list`, `watch`, and `delete` on `codertemplatetests.coder.com` in their namespace. They never need access to Coder or to the operator token.

## 4. Read the result

`status.phase` is `Pending` until the controller may have sent a create request, then `Running`, and finally `Succeeded` or `Failed`. `status.reason` and `status.message` say what the controller waits for or why the test failed. `status.templateVersionName` shows which version ran.

### Waiting reasons

While the test waits, the condition `Reconciling` is `True` with the same reason. Every wait ends at `spec.timeoutSeconds`: the test then fails as `DeadlineExceeded`, and the message names the last wait.

| Reason | Meaning | What to do |
| --- | --- | --- |
| `Initializing` | The controller started the test. | Nothing. |
| `ControlPlaneNotReady` | The `CoderControlPlane` does not exist, is being deleted, or has no usable `status.url`. | Check `spec.controlPlaneRef` and the control plane status. |
| `OperatorAccessNotReady` | `status.operatorAccessReady` of the control plane is not `true`, or its operator token Secret is missing or has no token. | See [Deploy the controller](deploy-controller.md), and check the Secret in `status.operatorTokenSecretRef` of the control plane. |
| `OwnerNotConfigured` | The control plane has no `spec.templateTests.ownerUserID`. | Set it, as in step 1. |
| `OwnerNotEligible` | The tester does not exist, is not `active`, has a person's login type, has a role that is not allowed, or is not a member of the template's organization. | Fix the tester as described in step 1. The message names the problem. |
| `TemplateNotFound` | The organization or the template in `spec.template` does not exist. | Check `spec.template` (`<organization>.<template>`). |
| `TemplateVersionNotFound` | The version in `spec.version` does not exist. | Check `spec.version`, or push the version. |
| `TemplateVersionImporting` | The version is still importing. | Wait. |
| `CreatingWorkspace`, `CreateRetrying` | The controller sends the create request, or sends it again after a request that had no effect. | Wait. |
| `ConfirmingCreate` | The answer to the create request was lost. The controller reads Coder until the workspace appears. | Wait. |
| `WaitingForBuild`, `WaitingForAgents` | The start build runs, or an agent is not yet `connected` and `ready`. | Wait. The message names the agent. |
| `AgentsReady`, `DeletingWorkspace` | Every top-level agent was ready. The controller does not check devcontainer sub-agents. The controller deletes the workspace. | Wait. |
| `CoderUnavailable` | A Coder request failed. After HTTP 429, the controller retries with backoff. | Check Coder and its logs. |
| `CoderAnswerMismatch` | Coder answered about another object than the one asked for. The controller logs it and retries. | Check proxies between the controller and Coder. |

### Failure reasons

A failed test has `Ready=False` and `Stalled=True` with the same reason. It does not run again: create a new test.

| Reason | Meaning | What to do |
| --- | --- | --- |
| `DeadlineExceeded` | The test did not finish within `spec.timeoutSeconds`. | Fix the last wait in the message, or raise `spec.timeoutSeconds`. |
| `TemplateDeprecated` | The template is deprecated and accepts no new workspaces. | Test another template. |
| `TemplateVersionArchived`, `TemplateVersionImportFailed` | The version is archived, or its import did not succeed. | Push a working version. |
| `TemplateVersionMismatch` | The version in `spec.version.id` belongs to another template. | Fix `spec.version` or `spec.template`. |
| `CreateRejected` | Coder rejected the create request. The message holds Coder's answer. No workspace exists. | Fix the cause, for example `spec.parameters`, and create a new test. |
| `CreateOutcomeUnknown` | No workspace from this test appeared after an uncertain create request. | Check Coder, then create a new test. |
| `WorkspaceNameConflict` | A workspace with the test's name exists, but nothing proves that this test created it. | Check the workspace in Coder. See the escape hatch below. |
| `BuildFailed`, `BuildCanceled` | The start build failed, or someone else canceled it. | Read the build logs in Coder. |
| `NoAgents` | The workspace has no top-level agents, so nothing proves that it works. Devcontainer sub-agents do not count. | Add a top-level agent to the template. |
| `AgentConnectionTimeout`, `AgentStartError`, `AgentStartTimeout`, `AgentStopped` | An agent did not connect in time, its startup script failed or timed out, or it stopped. | Read the agent logs in Coder. The message names the agent. |
| `WorkspaceChangedExternally`, `WorkspaceDeletedExternally` | Someone else started a build of the workspace, or deleted it. | Leave test workspaces alone, and create a new test. |
| `DeleteBuildFailed` | Every top-level agent was ready, but the delete build failed. The controller retries the delete. | Read the delete build logs in Coder. |
| `ControlPlaneGone` | The `CoderControlPlane` was deleted while the workspace could exist. | Look for the workspace in Coder and delete it there. |

### `WorkspaceDeleted` reasons

The condition `WorkspaceDeleted` tracks the workspace after the test ends or while the test is being deleted. The controller keeps its finalizer until the condition is `True`, or the reason is `Retained` or `ControlPlaneGone`.

| Status | Reasons | Meaning |
| --- | --- | --- |
| `True` | `Deleted`, `DeletedExternally`, `NotCreated` | No workspace of this test exists. `ttlSecondsAfterFinished` applies. |
| `False` | `CleanupPending`, `Deleting`, `DeleteRetrying` | The controller deletes the workspace. |
| `False` | `ControlPlaneUnavailable`, `CoderAnswerMismatch` | Coder is not usable, or answered about another object. The controller retries. |
| `False` | `Retained` | `retain` is set and allowed, so the workspace stays in Coder. |
| `Unknown` | `CreateOutcomeUnknown` | The controller reads Coder to learn whether a workspace exists. |
| `Unknown` | `OwnershipUnknown`, `ControlPlaneGone` | The workspace can exist, and the controller cannot delete it. See the escape hatch below. |

## Limit the cost

Each test is a real workspace: it uses compute and provisioner time until the controller deletes it. To cap the number of tests in a namespace, use a `ResourceQuota`. The quota counts finished tests too, until they expire or someone deletes them:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: template-tests
  namespace: coder
spec:
  hard:
    count/codertemplatetests.coder.com: "10"
```

## Run nightly checks

A `CronJob` can test the active version every night. It needs only `create` on `codertemplatetests`. `ttlSecondsAfterFinished` removes old results:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: nightly-template-test
  namespace: coder
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: nightly-template-test
  namespace: coder
rules:
  - apiGroups: ["coder.com"]
    resources: ["codertemplatetests"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: nightly-template-test
  namespace: coder
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: nightly-template-test
subjects:
  - kind: ServiceAccount
    name: nightly-template-test
    namespace: coder
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: nightly-template-test
  namespace: coder
spec:
  schedule: "0 3 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      backoffLimit: 0 # a retry after a lost create response makes a second test
      template:
        spec:
          serviceAccountName: nightly-template-test
          restartPolicy: Never
          containers:
            - name: create-test
              image: <an image with sh and kubectl>
              command:
                - sh
                - -ec
                - |
                  kubectl create -f - <<'TEST'
                  apiVersion: coder.com/v1alpha1
                  kind: CoderTemplateTest
                  metadata:
                    generateName: docker-nightly-
                    namespace: coder
                  spec:
                    controlPlaneRef:
                      name: coder
                    template: default.docker
                    version:
                      active: true
                    ttlSecondsAfterFinished: 604800
                  TEST
```

To find failed tests, run `kubectl -n coder get codertemplatetests`, or alert on the condition `Stalled=True`. To gate promotions instead, see [Gate template promotion with GitOps](gitops.md).

## Cleanup and the escape hatch

The controller keeps a finalizer on each test until it has deleted the workspace or proved that none exists. These cases need an admin:

- **A test stuck as `ControlPlaneUnavailable` or `CoderUnavailable`.** Coder is unreachable, so the finalizer stays and the controller retries every 60 s. To release the test, remove the finalizer by hand, or use `retain` as described below. Either way, check Coder for a workspace named `status.workspaceName` and delete it there.
- **A test with `WorkspaceDeleted=Unknown` and reason `OwnershipUnknown`.** A workspace with the test's name exists, but nothing proves that the test created it, so the controller never touches it. Only removing the finalizer or `retain` releases the test.

The annotation `coder.com/deletion-policy: retain` releases a finished or deleted test without deleting its workspace. Anyone who can create a test can set this annotation, so it works only when the control plane opts in with `spec.templateTests.allowRetain: true`. Opting in gives that power to everyone who can create tests against the control plane: they can leave workspaces, and the tester's session keys, in Coder. Without the opt-in, the controller ignores `retain`, says so in the `WorkspaceDeleted` message, and deletes the workspace. If you switch a retained final test back to `coder.com/deletion-policy: delete`, the controller adds the finalizer again and deletes the workspace.

## Known limits

- **TTL.** `ttlSecondsAfterFinished` applies only when `WorkspaceDeleted=True` (reason `Deleted`, `DeletedExternally`, or `NotCreated`). A test with `Retained` or `ControlPlaneGone` never expires, because its workspace can still exist. Delete such tests by hand after you check Coder.
- **Tester API keys.** In Coder v2.37.2, each start build creates a session token for the workspace owner, and the stop or delete build removes it. A test that deletes its workspace leaves no key behind: the Kind E2E counted 0 tester keys before and after three passing tests. A retained workspace keeps its owner's key until the workspace is stopped or deleted.
- **More than 100 builds.** After an uncertain create request, the controller reads the newest 100 builds of the workspace to prove that the test created it. A workspace with more builds stays unproven: the test fails as `WorkspaceNameConflict`, keeps its finalizer, and never touches the workspace.
- **TLS (#198).** With TLS on the control plane, `CoderTemplateTest` calls Coder at the control plane's internal `http://` service URL. Its operator token therefore crosses the cluster network as plain HTTP, without encryption. Separately, the `CoderProvisioner` controller and the aggregated API server use the `https://` service URL, whose certificate often does not cover the service name, so their Coder calls can fail TLS verification. This is tracked in #198 and not verified in a live cluster.
