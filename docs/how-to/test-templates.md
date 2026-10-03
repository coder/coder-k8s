# Test a template version with CoderTemplateTest

This guide shows how to check that a Coder template version can start a workspace whose agents become ready. A `CoderTemplateTest` creates a throwaway workspace from one template version, waits until every agent is `connected` and `ready`, records `Succeeded` or `Failed`, and deletes the workspace. For every field, see the [`CoderTemplateTest` reference](../reference/api/codertemplatetest.md).

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
