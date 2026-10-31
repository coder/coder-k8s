# Keep ready workspaces with CoderWorkspacePool

A `CoderWorkspacePool` keeps a number of ready Coder workspaces from one template for one Coder user, the pool owner. A `CoderWorkspaceClaim` takes one of them at once, with no build. The operator then builds a replacement. Use a pool when CI jobs or people must not wait for a workspace build. For every field, see the [`CoderWorkspacePool`](../reference/api/coderworkspacepool.md), [`CoderWorkspaceClaim`](../reference/api/coderworkspaceclaim.md), and [`CoderWorkspacePoolMember`](../reference/api/coderworkspacepoolmember.md) references.

Every workspace of a pool belongs to the pool owner from its first build, because Coder cannot move a workspace to another user. A claim reserves one workspace for whoever holds the owner's Coder credentials, for example a CI system that runs `coder ssh <ownerName>/<workspaceName>` with the owner's token. The claim status gives both names.

You need:

- a `CoderControlPlane` whose `status.operatorAccessReady` is `true` (see [Deploy the controller](deploy-controller.md)). The operator user must be a site owner,
- a template whose workspaces have at least one agent,
- a Coder user that owns the pool workspaces (the pool owner), described in step 1.

## 1. Create the pool owner

Every start build hands the owner's token to the template's Terraform. So the owner must pass the same rules as the tester of a template test (see [Test a template version](test-templates.md#1-create-the-tester-user)): status `active`, a service account or a password user, no site role other than `member`, and only `organization-member` or `organization-workspace-access` in each organization. Coder's prebuilds system user is refused. Without a Premium license, Coder has no service accounts, so use a password user that no person logs in as.

Coder creates users through its API as `dormant`. Activate the owner, then log in as it once, for example with `POST /api/v2/users/login`. Coder checks every 15 minutes and marks each active user dormant whose last login or API use is older than 90 days. An activation does not count as a use, so a new owner that never logged in turns dormant at the next check. A dormant owner stops the pool with `OwnerNotEligible`, and the pool deletes its unclaimed workspaces. Use the owner's credentials at least once every 90 days. A claim consumer that connects with the owner's token does that.

## 2. Allow the owner on the control plane

The control plane admin lists each owner, the templates that its pools can build, and a cap. Pools of an owner that is not listed wait with `OwnerNotAllowed` and create nothing:

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderControlPlane
metadata:
  name: coder
  namespace: coder
spec:
  workspacePools:
    owners:
      - userID: 5f0c3c4e-0000-4000-8000-000000000001 # the pool owner's Coder user ID
        templates: [coder.ci-runner]                 # <organization>.<template>
        maxWorkspaces: 10                            # the default; at most 100
```

`maxWorkspaces` caps the owner's pool members on this control plane: unclaimed, claimed, and used ones, across every pool. Workspaces of the owner that no pool made do not count. It is the real cost cap, whoever resizes a pool. When you remove an owner or a template, its pools delete their unclaimed workspaces and keep the claimed ones.

## 3. Create a pool

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderWorkspacePool
metadata:
  name: ci-runners
  namespace: coder
spec:
  controlPlaneRef:
    name: coder
  ownerUserID: 5f0c3c4e-0000-4000-8000-000000000001
  template: coder.ci-runner
  version:
    active: true # or name: v1.4.0, or id: <uuid>
  replicas: 3    # unclaimed ready workspaces to keep, 0 to 100
```

`controlPlaneRef`, `ownerUserID`, `template`, and `claims.mode` cannot change. The sample is in `config/samples/coder_v1alpha1_coderworkspacepool.yaml`. Watch the pool and its members:

```bash
kubectl -n coder get coderworkspacepools,coderworkspacepoolmembers
```

Each member owns one workspace named `kwp-` plus 28 hex characters of the member's UID. Do not rename these workspaces, and do not use an unclaimed one directly. The pool releases a workspace that someone used without a claim, and never deletes it on its own.

`replicas` has a scale subresource, so `kubectl scale`, HPA, and KEDA can change it:

```bash
kubectl -n coder scale coderworkspacepool ci-runners --replicas=5
```

## 4. Claim a workspace

```yaml
apiVersion: coder.com/v1alpha1
kind: CoderWorkspaceClaim
metadata:
  name: build-1234
  namespace: coder
spec:
  poolName: ci-runners
```

A claim with a ready workspace in its pool is `Bound` and `Ready` at once. In the Kind end-to-end test, that took about 1 s. `status.ownerName` and `status.workspaceName` name the workspace. When the workspace has an autostop deadline, the operator moves it once at the bind, to the template's default TTL from now, at least 30 minutes. A workspace's max deadline (from a Premium autostop requirement) can shorten or prevent that extension, and so can a refusal by Coder: the operator then keeps the deadline and does not try again. After that, Coder's normal autostop applies. One claim is one workspace, and a claim never binds a second time. Delete the claim when you are done: that deletes its workspace.

## 5. Grant access

The operator's own RBAC comes with the install. People who use pools need, in their namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: workspace-pool-user
  namespace: coder
rules:
  - apiGroups: [coder.com]
    resources: [coderworkspacepools]
    verbs: [create, get, list, watch, update, patch, delete]
  - apiGroups: [coder.com]
    resources: [coderworkspacepools/scale]
    verbs: [get, update, patch]
  - apiGroups: [coder.com]
    resources: [coderworkspaceclaims]
    verbs: [create, get, list, watch, delete]
```

The install adds `get`, `list`, and `watch` on `coderworkspacepoolmembers` to the built-in `view`, `edit`, and `admin` roles (ClusterRole `coder-k8s-coderworkspacepoolmember-viewer`). No user role can write members: only the operator creates and changes them.

!!! warning "Who can resize a pool"
    The HPA controller and KEDA can write `/scale` of every pool. So anyone who can create an HPA in the namespace, which includes the built-in `edit` role, can resize a pool up to 100. `maxWorkspaces` on the control plane still caps the owner's workspaces.

To allow an autoscaler only for users who can resize the pool themselves, add this `ValidatingAdmissionPolicy` (Kubernetes 1.30 or later). It refuses an HPA or a KEDA `ScaledObject` that targets a pool unless its author has `update` on that pool's `coderworkspacepools/scale`. Other autoscalers are not affected. In Kind, an `edit` user without that right got the denial, and with it the HPA was created.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: coderworkspacepool-autoscalers
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [autoscaling]
        apiVersions: ["*"]
        operations: [CREATE, UPDATE]
        resources: [horizontalpodautoscalers]
      - apiGroups: [keda.sh]
        apiVersions: ["*"]
        operations: [CREATE, UPDATE]
        resources: [scaledobjects]
  matchConditions:
    - name: targets-a-pool
      expression: has(object.spec.scaleTargetRef.kind) && object.spec.scaleTargetRef.kind == 'CoderWorkspacePool'
  validations:
    - expression: >-
        authorizer.group('coder.com').resource('coderworkspacepools').subresource('scale')
        .namespace(object.metadata.namespace).name(object.spec.scaleTargetRef.name).check('update').allowed()
      message: Only a user who can update coderworkspacepools/scale of the target pool can autoscale it.
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: coderworkspacepool-autoscalers
spec:
  policyName: coderworkspacepool-autoscalers
  validationActions: [Deny]
```

KEDA's operator creates the HPA for a `ScaledObject`, and its own role can update every `/scale`, so the policy checks the `ScaledObject`'s author instead.

## Cost

Each unclaimed workspace runs all the time, so a pool of `replicas: N` costs N running workspaces plus the claimed ones. A claimed workspace keeps a normal autostop deadline after the bind, so Coder stops it when nobody uses it. A template with a default TTL of 0 has no deadline: its claimed workspaces run until someone stops or deletes them.

Further limits: `maxInFlight` (default 5) caps the members of a pool that build at the same time, and a pool stops creating with `Stalled` and `MemberFailureLimit` after 5 failed members in a row. To cap the number of pools and claims in a namespace, use a `ResourceQuota` on `count/coderworkspacepools.coder.com` and `count/coderworkspaceclaims.coder.com`.

### Coder API requests

The operator sends every pool request with its operator token. Measured in Kind with Coder v2.37.2:

| What | Requests per minute | Requests |
| --- | --- | --- |
| A ready member | 2 | `GET /api/v2/workspaces/<id>` and `GET /api/v2/users/me` once a minute |
| A member that builds | 24 | The same two requests every 5 s, until the member is ready |
| A pool that follows the active version | 6 | The owner, its organization membership, the organization (twice), the template, and the version, once a minute |
| A pool of 10 ready members | 26 | 10 × 2 + 6 |

A pool sends no request to `/api/v2/files`, so Coder's limit of 12 file requests per minute never applies. Coder's general limit of 512 requests per minute counts each user and each path. Each member reads its workspace on its own path, but every member also reads `/api/v2/users/me`, one path for all members of the operator. From the rates above, about 500 ready members, or about 40 members that build, reach that limit (not tested at that size). Coder then answers 429, and the operator backs off and retries.

One worker reconciles every member of the operator, so the owner cap stays exact. A slow Coder therefore delays every member. Measured in Kind with ready members, and a delay added to each Coder answer to the operator:

| Delay per answer | Ready members | Time per member check | Worker busy | Checks per minute | Mean wait in the queue |
| --- | --- | --- | --- | --- | --- |
| none | 10 | 0.03 s | 1 % | 10 | 0.1 s |
| 1 s | 11 | 2 s | 40 % | 11.9 | 6 s |
| 3 s | 11 | 6 s | 100 % | 10 | 23 s |

Checks that fall due together wait in line, so with a 1 s delay some checks waited 10 s or more. The worker is full once the number of members times twice the answer time reaches 60 s. Then each member is checked less often than once a minute, and a member that builds is checked less often than every 5 s. No member failed in these runs. The deadline extension starts 15 minutes before a deadline, so a check that comes late still extends in time.

## Cleanup

- Delete a claim to delete its workspace.
- Delete a pool to delete its unclaimed workspaces. Claimed workspaces stay with their claims, and the claims stay `Bound`.
- A member whose workspace someone used without a claim (`Used=True`) leaves the pool: the pool removes itself as the member's owner, so neither a scale-down nor deleting the pool deletes it. A cluster admin who deletes such a member, or its namespace, deletes its workspace in Coder, as deleting a claim does. To keep the work, copy it out of the workspace first.
- When the `CoderControlPlane` is deleted, members release their finalizers without a Coder call. Find leftover workspaces in Coder with the filter `owner:<pool owner>` and delete those whose names start with `kwp-`.

With TLS on the control plane and without `spec.tls.operatorAccess`, pools send the operator token to Coder over plain HTTP inside the cluster. See [Where the operator token goes](deploy-controller.md#where-the-operator-token-goes).
