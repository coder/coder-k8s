# Start, stop, and read logs of workspaces

This guide shows how to start and stop a Coder workspace and read its build log with `kubectl`, through the aggregated API server. For the full rules, see the [start and stop](../reference/aggregated-api-behavior.md#start-and-stop) and [build log](../reference/aggregated-api-behavior.md#workspace-build-log) reference.

You need:

- an aggregated API server that is registered and serves `aggregation.coder.com/v1alpha1` (see [Deploy the aggregated API server](deploy-aggregated-apiserver.md)),
- the canonical name of the workspace, `<organization>.<owner>.<workspace>`, for example `acme.alice.dev` (see [Find canonical names](../reference/aggregated-api-behavior.md#find-canonical-names)),
- `jq`, to read the responses.

## 1. Grant access

Start, stop, and the log are separate subresources. No default role (`view`, `edit`, `admin`) grants them. This Role lets a subject start and stop one workspace, read its build log, and read its status:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: coder-workspace-operator
  namespace: coder
rules:
  - apiGroups: ["aggregation.coder.com"]
    resources: ["coderworkspaces/start", "coderworkspaces/stop"]
    verbs: ["create"]
    resourceNames: ["acme.alice.dev"]
  - apiGroups: ["aggregation.coder.com"]
    resources: ["coderworkspaces/log"]
    verbs: ["get"]
    resourceNames: ["acme.alice.dev"]
  - apiGroups: ["aggregation.coder.com"]
    resources: ["coderworkspaces"]
    verbs: ["get"]
    resourceNames: ["acme.alice.dev"]
```

- Leave out `resourceNames` to allow every workspace in the namespace.
- Leave out `coderworkspaces/start` for a subject that may only stop workspaces.
- The `get` rule on `coderworkspaces` is for the status check in step 3. Leave it out if another identity checks the status.

Bind the Role to the subject, for example the user `alice`, and check it:

```bash
kubectl create rolebinding coder-workspace-operator -n coder --role=coder-workspace-operator --user=alice
kubectl auth can-i create coderworkspaces.aggregation.coder.com/acme.alice.dev --subresource=start -n coder --as=alice
kubectl auth can-i get coderworkspaces.aggregation.coder.com/acme.alice.dev --subresource=log -n coder --as=alice
```

Both checks print `yes`. The subject runs the next steps with its own credentials. To test them as an admin, add `--as=alice` to each `kubectl` command.

## 2. Stop or start the workspace

```bash
API=/apis/aggregation.coder.com/v1alpha1/namespaces/coder/coderworkspaces

# Preview a stop. Nothing changes.
kubectl create --raw "$API/acme.alice.dev/stop?dryRun=All" -f - <<<'{}' | jq .status

# Stop the workspace.
kubectl create --raw "$API/acme.alice.dev/stop" -f - <<<'{}' | jq .status

# Start it again, after the stop build ended.
kubectl create --raw "$API/acme.alice.dev/start" -f - <<<'{}' | jq .status
```

The response is a `CoderWorkspaceTransition`. Read `status.outcome`:

- `Queued`: the server queued a build. `status.buildID` and `status.buildNumber` name it.
- `InProgress`: a build of the same kind is already pending or running. Nothing was queued.
- `Unchanged`: the workspace is already started or stopped. Nothing was queued.
- `WouldQueue`: only with `dryRun=All`. A real request would queue a build.

A repeated request does not queue a second build, so you can retry a start or stop that is still in progress. A start while a stop build runs, or a stop while a start build runs, returns `409`.

## 3. Wait for the build and read its log

Follow the log of the latest build until it ends, then check the status:

```bash
kubectl get --raw "$API/acme.alice.dev/log?follow=true"
kubectl get coderworkspace.aggregation.coder.com acme.alice.dev -n coder \
  -o jsonpath='{.status.latestBuildStatus}{"\n"}'
```

- The follow ends when the build ends. If the build has already ended, it prints the whole log and returns at once.
- The status is `running` after a start and `stopped` after a stop.
- Without `follow=true`, the log request returns the log so far. Add `limitBytes=<n>` to read at most `n` bytes.
- Build logs can contain secrets that Terraform printed. Share them with care.

## If a request fails

- `403`: the subject has no grant for this subresource or this workspace name. Check the Role and step 1.
- `409`: another build is active, for example a stop while a start is running. Wait until it ends, then try again.
- `504`: the result is uncertain. Coder can have queued the build. Read `status.latestBuildID` and `status.latestBuildStatus` of the workspace before you retry. See [Time limit and retries](../reference/aggregated-api-behavior.md#time-limit-and-retries).
- Other errors: see [Troubleshooting](troubleshooting.md#coderworkspacesstart-or-stop-returns-400-403-409-or-504).
