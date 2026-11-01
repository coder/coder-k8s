# Troubleshooting

Start with the operator logs. Most problems show there:

```bash
kubectl logs -n coder-system deploy/coder-k8s
```

## A component that you did not expect is running

`--app` is optional. Its default is `all`, which runs the controller and the aggregated API server. The MCP server never runs in `all` mode. To run only one component, set `--app` explicitly:

```bash
GOFLAGS=-mod=vendor go run . --app=controller   # or aggregated-apiserver, or mcp-http --mcp-token-file=<file>
```

If the value is unknown, the process stops at startup with `assertion failed: unsupported --app value ...`.

## `no matches for kind` when applying a `CoderControlPlane`

The CRDs are missing. Install them:

```bash
kubectl apply -f config/crd/bases/
kubectl get crd | grep coder.com
```

## The controller runs but nothing reconciles

1. Make sure that the Deployment and the RBAC from this repository exist:

    ```bash
    kubectl get deploy coder-k8s -n coder-system
    kubectl get clusterrole manager-role
    kubectl get clusterrolebinding coder-k8s
    ```

2. Examine the events and the status of the object:

    ```bash
    kubectl describe codercontrolplane <name> -n <namespace>
    ```

## `CoderControlPlane` stays `Pending`

Usual causes:

1. The control plane Deployment has no ready pods.
2. The operator bootstrap token is not ready yet. If the `OperatorTokenSecretOwned` condition is `False` with the reason `TokenSecretNotOwned`, another owner controls the operator token Secret. See [Operator token Secret](deploy-controller.md#operator-token-secret).
3. `spec.licenseSecretRef` points to a license Secret that is missing or not valid.
4. The controller cannot resolve `spec.database.connectionSecretRef`. Look at the reason of the `DatabaseSecretResolved` condition. See [Connect an external PostgreSQL database](deploy-controller.md#connect-an-external-postgresql-database).

```bash
kubectl get codercontrolplane <name> -n <namespace> -o yaml
kubectl get deploy,svc -n <namespace>
```

## Operator calls fail with `refusing redirect to a different origin`

Coder answered an operator call with a redirect to another origin. The operator does not follow such redirects, so that the operator token stays on the control plane origin. The usual cause is `CODER_REDIRECT_TO_ACCESS_URL=true` in `spec.extraEnv` of the `CoderControlPlane`. Remove it, and redirect browsers from HTTP to HTTPS in your Ingress, Gateway or load balancer instead. See [Redirects to another origin](deploy-controller.md#redirects-to-another-origin).

Most aggregated API reads then answer the caller with a `500` that says `the Coder API could not be reached`. Writes answer with the uncertain `504`, because the server that sent the redirect got the write. Re-read the object before you retry. The `coder-errors` line in the aggregated API server log shows the refusal. When Coder or a proxy redirects only the websocket request of the workspace build log stream (`coderworkspaces/log?follow=true`), that line shows `refusing request to a different origin than the coder URL` instead.

## Operator calls to Coder are slow or time out under load

The aggregated API server and most controllers share one connection pool per Coder deployment in each operator process. A pool opens at most 128 connections. A Coder call beyond that waits for a free connection, up to the request timeout. The controllers use 30 seconds. The aggregated API server uses `--coder-request-timeout` (default 30 seconds). Two kinds of controller calls use separate connections and are not in the pool or the metric: the license and entitlement calls of the `CoderControlPlane` controller, and the provisioner key calls of the `CoderProvisioner` controller.

The histogram `coder_k8s_coder_connection_wait_seconds` shows how long each call through the pool waited for a connection, including the dial of a new connection. A call that times out before it gets a connection also counts, with the time it waited. Most samples fall in the lowest buckets. Samples near the request timeout have two usual causes: all 128 connections are in use, or Coder is slow to reach (DNS, TCP connect, proxy or TLS handshake). A slow network usually also shows dial or TLS errors in the logs.

- The controller serves the metric at `/metrics` on port `8080`.
- The aggregated API server serves the metric at `/metrics` on its secure port `6443`. The caller needs RBAC for the `get` verb on the non-resource URL `/metrics`.

A process keeps one connection pool per Coder deployment, identified by URL scheme, host and port. `https://coder.example.com` and `https://coder.example.com:443` are the same deployment. The process drops a pool after 10 minutes without an open call, for example after a control plane changed its URL or went away. A workspace build log stream counts as an open call until it ends.

## APIService is `False` / `Unavailable`

```bash
kubectl get svc coder-k8s-apiserver -n coder-system
kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml
```

Do not install CRDs for the same resources (`coderworkspaces.aggregation.coder.com`, `codertemplates.aggregation.coder.com`, `codertemplateversions.aggregation.coder.com`). They conflict with the aggregated API.

## Proxied requests fail with 503 and an x509 error

Symptoms:

- `kubectl get` on `codertemplates` or `coderworkspaces`, or `kubectl get --raw /apis/aggregation.coder.com/v1alpha1`, fails with `Error from server (ServiceUnavailable): the server is currently unable to handle the request`.
- The kube-apiserver log shows `error trying to reach service: tls: failed to verify certificate: x509: certificate signed by unknown authority` for `v1alpha1.aggregation.coder.com`.

Cause: kube-apiserver cannot verify the certificate of the aggregated API server against the APIService `caBundle`. The APIService can still show `Available=True`, because the availability check of kube-apiserver does not verify the certificate. Thus `Available=True` does not exclude this cause. For how the `caBundle` is managed, see [How kube-apiserver trusts the server](deploy-aggregated-apiserver.md#how-kube-apiserver-trusts-the-server).

Make sure that this is the error. On kubeadm-based clusters (including Kind), kube-apiserver runs as a Pod. On managed clusters, look in the control-plane logs of your provider instead.

```bash
kubectl get --raw /apis/aggregation.coder.com/v1alpha1
kubectl -n kube-system logs -l component=kube-apiserver --since=10m --tail=-1 |
  grep v1alpha1.aggregation.coder.com | grep x509
```

Then compare the APIService `caBundle` with the CA that the server uses. The two commands must show the same value:

```bash
kubectl -n coder-system get secret coder-k8s-apiserver-tls -o jsonpath='{.data.ca\.crt}'; echo
kubectl get apiservice v1alpha1.aggregation.coder.com -o jsonpath='{.spec.caBundle}'; echo
```

If the values are different, find the cause:

- The APIService is opted out, and its `caBundle` is old or wrong. In the output of `kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml`, `metadata.annotations` contains `coder.com/manage-ca-bundle: "false"`. Set the `caBundle` to the `ca.crt` of the Secret yourself, or give the field back to coder-k8s:

    ```bash
    kubectl annotate apiservice v1alpha1.aggregation.coder.com coder.com/manage-ca-bundle-
    ```

- The server does not have permission to update the APIService. The server log gives the name of the missing permission:

    ```bash
    kubectl -n coder-system logs deploy/coder-k8s | grep 'missing permission'
    kubectl auth can-i patch apiservices.apiregistration.k8s.io/v1alpha1.aggregation.coder.com \
      --as=system:serviceaccount:coder-system:coder-k8s
    ```

    In standalone mode, use `--as=system:serviceaccount:coder-system:coder-k8s-apiserver`. Apply the RBAC. The RBAC must give the permission to the ServiceAccount that the pod runs as. Standalone mode also needs `config/apiserver-standalone/apiservice-cabundle-binding.yaml`. The server tries again within about one minute:

    ```bash
    kubectl apply -f config/rbac/apiservice-cabundle-role.yaml
    ```

- A different tool writes the `caBundle`. Examples are the CA injector of cert-manager (a `cert-manager.io/inject-ca-from` annotation), or a GitOps tool that sets `caBundle` from Git. The value changes back again and again, and each time the server logs `Set the APIService caBundle` again. To find the other writer, look in the managed fields. coder-k8s writes as `coder-k8s-apiservice-cabundle`:

    ```bash
    kubectl get apiservice v1alpha1.aggregation.coder.com -o yaml --show-managed-fields
    kubectl -n coder-system logs deploy/coder-k8s | grep -c 'Set the APIService caBundle'
    ```

    Keep one owner. Remove the `caBundle` injection of the other tool. Or [opt out](deploy-aggregated-apiserver.md#opt-out), and make that tool inject the `ca.crt` from `coder-k8s-apiserver-tls`. That is the CA of the certificate that the server serves.

If the values are the same, the most likely cause is a CA rotation that is not complete: old replicas still serve a certificate from the previous CA. Wait until the rollout is complete. Then restart all replicas that started before the Secret changed:

```bash
kubectl -n coder-system rollout status deployment/coder-k8s
kubectl -n coder-system rollout restart deployment/coder-k8s
```

For the full procedure and how long the failures usually last, see [Replace the CA](deploy-aggregated-apiserver.md#replace-the-ca).

## The pod exits with `configure delegated authentication`

The aggregated API server uses the Kubernetes API to check each caller. Without this access, it does not start.

- `... configmaps "extension-apiserver-authentication" is forbidden`: apply `config/rbac/authentication-reader-binding.yaml`. In standalone mode, apply `config/apiserver-standalone/authentication-reader-binding.yaml` instead. The binding must name the ServiceAccount that the pod runs as, for example after you install into a different namespace.
- `no Kubernetes configuration for delegated authentication and authorization` (outside a cluster): set `KUBECONFIG` to one kubeconfig file, or create `~/.kube/config`.
- `load kubeconfig ...` or `invalid kubeconfig ...`: the file in `KUBECONFIG` is missing or not complete. The server does not use a different configuration instead.

## The pod exits with `configure aggregated API server serving certificate`

The Secret `coder-k8s-apiserver-tls` exists, but the server cannot use it. The message gives the name of the field, for example `data["ca.key"] is missing or empty`. Correct the Secret. Or delete it, and the server generates a new CA when it starts again:

```bash
kubectl -n coder-system delete secret coder-k8s-apiserver-tls
kubectl -n coder-system rollout restart deployment/coder-k8s
```

In standalone mode, the server does not have permission to create the Secret. Thus, after you delete it, apply the placeholder again with `kubectl apply -f config/apiserver-standalone/serving-ca-secret.yaml` before the restart.

If the error does not name a field, the ServiceAccount does not have a permission on this Secret. The message tells you which permission:

- `get secret …`: the ServiceAccount cannot read the Secret. Give it `get` on `coder-k8s-apiserver-tls`.
- `create secret …`: the Secret does not exist, and the ServiceAccount cannot create Secrets. In standalone mode with `coder-k8s-apiserver`, this is expected until the placeholder exists. Apply `config/apiserver-standalone/serving-ca-secret.yaml`, and the pod recovers on its next restart. In other cases, give the ServiceAccount `create`, or create the empty placeholder that the message describes. The server fills the placeholder.
- `fill placeholder secret …` or `update secret …`: the ServiceAccount cannot update the Secret. It needs this permission to fill a placeholder or to renew the serving certificate. Give it `update` on `coder-k8s-apiserver-tls`.

## Aggregated requests fail with `401 Unauthorized` or `403 Forbidden`

- `401`: the request has no valid credential. A request that you send directly to port `6443` needs a Kubernetes bearer token. Requests without credentials can reach only `/healthz`, `/livez`, and `/readyz`. Use `kubectl`, which sends requests through kube-apiserver.
- `403`: the caller does not have RBAC for `aggregation.coder.com` in that namespace. To find out, run `kubectl auth can-i list codertemplates.aggregation.coder.com -n <namespace> --as=<user>`. Before you give this RBAC, know that it gives owner-equivalent access in Coder. See [How callers are checked](deploy-aggregated-apiserver.md#how-callers-are-checked).
- `500` that mentions `subjectaccessreviews`: the ServiceAccount of the server cannot create SubjectAccessReviews. Apply `config/rbac/auth-delegator-binding.yaml`. In standalone mode, apply `config/apiserver-standalone/auth-delegator-binding.yaml` instead.
- `403` that says `the Coder API denied the request`: kube-apiserver let the caller in, but Coder refused the call that the server made with its Coder token. The message leaves out the Coder URL and Coder's own text. The server log has the full Coder error, on a line from the `coder-errors` logger.
- `500` that says `the Coder API failed the request ... with status <code>` or `the Coder API could not be reached`: Coder answered with a server error, or the server could not reach Coder. Read the `coder-errors` line in the server log for the Coder URL and the full error.
- `504` that says `the result is uncertain`: Coder, or a proxy in front of it, gave an answer to a write that leaves it open whether Coder applied the write. Re-read the object with `kubectl get` before you retry. See [Coder timeouts](../reference/aggregated-api-behavior.md#coder-timeouts).

## Aggregated reads return `ServiceUnavailable`

This section is about `ServiceUnavailable` errors from the aggregated API server itself. Their messages are specific. For example, they say that the server found no eligible `CoderControlPlane`, or that the configuration for standalone mode is missing. kube-apiserver returns the general message `the server is currently unable to handle the request` instead. For that error, see [Proxied requests fail with 503 and an x509 error](#proxied-requests-fail-with-503-and-an-x509-error).

- `all` mode: no eligible `CoderControlPlane` exists yet. A control plane is eligible when its operator access is enabled and ready, its status has an operator token reference and a URL, and its name does not contain a `.` character.
- Standalone mode (`--app=aggregated-apiserver`): set all three flags `--coder-url`, `--coder-session-token`, and `--coder-namespace`.

A `list` in one namespace that contains no `CoderControlPlane` at all does not return this error. It returns an empty list. See [Namespaces without a Coder backend](../reference/aggregated-api-behavior.md#namespaces-without-a-coder-backend).

The logs show which provider configuration the server used.

## Aggregated reads return `multiple eligible CoderControlPlane instances`

The server expects one eligible control plane for each request scope. If more than one control plane is eligible, do one of these steps:

- Send the request to one namespace (`-n <namespace>`).
- Run a dedicated aggregated API server in standalone mode, pinned to one namespace with `--coder-namespace`. Standalone mode needs all three flags `--coder-url`, `--coder-session-token`, and `--coder-namespace`.

If that namespace has more than one eligible control plane, the first step does not help. Keep only one eligible control plane in that namespace, or use a dedicated aggregated API server.

## `kubectl diff` returns `server-side dry-run is not supported`

`kubectl diff` and `--dry-run=server` send a server-side dry-run request. The server rejects it with `400` for `coderworkspaces` and `codertemplates`, and Coder does not change. To preview a change, compare the output of `kubectl get -o yaml` with your manifest, or use `--dry-run=client`. In Argo CD, keep server-side diff off for these resources. See [Server-side dry-run](../reference/aggregated-api-behavior.md#server-side-dry-run).

## A template promotion returns `400`, `403`, `409`, `422`, `429`, `503`, or `504`

See [Promote a template version](../reference/aggregated-api-behavior.md#promote-a-template-version) for the full rules.

- `400` that says `is not a version of template`: `spec.versionID` is unknown or belongs to another template. Use `status.id` of a `codertemplateversion` of the same template.
- `400` that says `is archived` or `its import job is`: only versions whose import succeeded and that are not archived can be promoted.
- `403`: the caller has no `create` grant on `codertemplates/promote`, or its `resourceNames` do not include the template. Check with `kubectl auth can-i create codertemplates.aggregation.coder.com/<organization>.<template> --subresource=promote -n <namespace>`.
- `400` that says `Coder refused to activate`: Coder rejected the activation, and nothing changed. Check that the import of the version succeeded and that it is not archived.
- `409` that says `superseded by a concurrent change`: another version became active at the same time. Check which version is active before you try again.
- `409` that says `was not found in Coder during the activation`: the template or the version changed during the request. Nothing changed. Check that both still exist.
- `422`: `spec.versionID` is not a UUID, or the `dryRun` value is not `All`.
- `429`: Coder rate-limited the operator token. Nothing changed. Wait and try again.
- `503` that says `could not confirm the promotion`: the server could not find out if the activation was applied. Check the active version with `kubectl get codertemplateversions` before you try again.
- `504` that says `was not attempted`: the lookups in Coder were slow, or the client timeout was too short, so the server sent no activation. Nothing changed. If you set `kubectl --request-timeout`, make it longer than 15 seconds. Then try again.
- A rollback disappears after a while: a GitOps tool applied the manifest again and created a new version. Pause self-heal or syncing before you roll back. See the GitOps note in [Promote a template version](../reference/aggregated-api-behavior.md#promote-a-template-version).

## `coderworkspaces/log` returns `403`, `406`, `422`, `429`, or `504`

- `403`: the caller has no `get` grant on `coderworkspaces/log`. Access to `coderworkspaces` alone is not enough. See [How callers are checked](deploy-aggregated-apiserver.md#how-callers-are-checked).
- `406`: the `Accept` header allows only `text/plain`. Allow `*/*` or JSON, or use `kubectl get --raw`.
- `422`: `limitBytes` is below 1, or `tailLines` is below 0.
- `429` that says `for this user`: this user already has 4 open log requests on this server. Open `follow=true` streams count too. Close one, or wait and try again.
- `429` that says `on this server`: the server has 64 open log requests. Wait for the `Retry-After` time and try again.
- `429` that says `You've been rate limited`: Coder rate-limited the operator token, which every aggregated API call uses. Coder allows 512 requests per minute by default (`CODER_API_RATE_LIMIT`).
- `504`: Coder did not answer in time before the response started. Either one Coder call took longer than the Coder request timeout (30 seconds by default), or the time limit of the request ran out: 60 seconds for a snapshot, or 25 minutes with `follow=true`. Check that Coder is healthy, then try again.
- A `Warning` that says the server cut the log: the log is larger than the server limit. See [Workspace build log](../reference/aggregated-api-behavior.md#workspace-build-log).
- A `follow=true` stream ends before the build ends: the stream reached the 4 MiB response limit or the 25-minute time limit. The server sends no `Warning` for a cut in the live part of a stream. Send a new request with `follow=true&tailLines=<n>`. It sends the last `n` existing lines, then follows. Without `tailLines`, it sends all existing entries again. In both cases, it continues to follow only if the existing log fits within the size limits of a log request: with `tailLines`, the log that Coder returns must fit within the 4 MiB read limit, and the last `n` lines within 4 MiB. Otherwise it returns only part of the log, with a `Warning`, and ends.

## `coderworkspaces/start` or `/stop` returns `400`, `403`, `409`, or `504`

- `400` with a Coder message about parameters: the start used the active template version, because the template requires it or the workspace always updates. That version needs parameter values that the workspace does not have. Set them in Coder, for example with `coder update`, then try again.
- `403`: the caller has no `create` grant on `coderworkspaces/start` or `coderworkspaces/stop`. Access to `coderworkspaces` or to the log is not enough. See [How callers are checked](deploy-aggregated-apiserver.md#how-callers-are-checked).
- `409`: another build of the workspace is active, or it is being canceled. The message names the build. Try again after it ends.
- `504`: Coder did not answer in time, and the result is uncertain. Coder can have queued the build. Re-read the latest build of the workspace (`kubectl get coderworkspace <name> -o yaml`) before you try again. See [Time limit and retries](../reference/aggregated-api-behavior.md#time-limit-and-retries).

## Aggregated requests return `400` or `409`

These errors often come from the rules for names or for `resourceVersion`. See [Aggregated API behavior](../reference/aggregated-api-behavior.md).

## A `CoderTemplateTest` stays `Pending`

The test waits for something outside it. `status.reason` names it:

```bash
kubectl get codertemplatetest <name> -n <namespace> -o jsonpath='{.status.reason}: {.status.message}{"\n"}'
```

Usual causes:

1. `OwnerNotConfigured` or `OwnerNotEligible`: the control plane has no tester, or the tester is not allowed. The message names the problem. See [Create the tester user](test-templates.md#1-create-the-tester-user).
2. `OperatorAccessNotReady` or `ControlPlaneNotReady`: the control plane in `spec.controlPlaneRef` is missing or not ready. See [`CoderControlPlane` stays `Pending`](#codercontrolplane-stays-pending).
3. `TemplateNotFound` or `TemplateVersionNotFound`: `spec.template` or `spec.version` names nothing in Coder. The spec is immutable, so create a new test with the right names.
4. `CoderUnavailable`: a Coder request failed. Check Coder and its logs.

A new test has no status for a moment: the first reconcile only adds the finalizer `coder.com/template-test-cleanup`, and the next reconcile writes the status. If the status stays empty, check the finalizer:

```bash
kubectl get codertemplatetest <name> -n <namespace> -o jsonpath='{.metadata.finalizers}{"\n"}'
```

Without the finalizer, the controller has not reconciled the test. See [The controller runs but nothing reconciles](#the-controller-runs-but-nothing-reconciles). With the finalizer, the controller reconciled the test once: read the controller logs for errors about the test.

Every wait ends at `spec.timeoutSeconds` with `DeadlineExceeded`. For every reason, see [Read the result](test-templates.md#4-read-the-result).

## A `CoderTemplateTest` does not finish deleting

The test keeps the finalizer `coder.com/template-test-cleanup` until the condition `WorkspaceDeleted` is `True`, or its reason is `Retained` or `ControlPlaneGone`. Read the condition:

```bash
kubectl get codertemplatetest <name> -n <namespace> \
  -o jsonpath='{range .status.conditions[?(@.type=="WorkspaceDeleted")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

- `Deleting` or `DeleteRetrying`: the delete build runs or failed. Read its logs in Coder. The controller retries.
- `ControlPlaneUnavailable` or `CoderAnswerMismatch`: Coder is not usable. Fix Coder, and the controller continues.
- `OwnershipUnknown`: a workspace with the test's name exists, but nothing proves that the test created it. The controller never touches it.

If the controller cannot finish, use the [escape hatch](test-templates.md#cleanup-and-the-escape-hatch). As a last resort, check Coder for a workspace named `status.workspaceName`, then remove only the controller's finalizer. Other controllers can have their own finalizers on the test, so do not remove the whole list:

1. Find the position of `coder.com/template-test-cleanup` in the list. The first entry has position 0.

    ```bash
    kubectl get codertemplatetest <name> -n <namespace> -o jsonpath='{.metadata.finalizers}'
    ```

2. Remove the entry at that position. The `test` operation makes the patch fail if the entry at `<position>` is a different finalizer.

    ```bash
    kubectl patch codertemplatetest <name> -n <namespace> --type json -p \
      '[{"op":"test","path":"/metadata/finalizers/<position>","value":"coder.com/template-test-cleanup"},{"op":"remove","path":"/metadata/finalizers/<position>"}]'
    ```

CAUTION: Delete the workspace in Coder before you remove the finalizer. Otherwise the workspace, and the tester's session key, stay in Coder.

## A namespace stays `Terminating`

1. Tests in the namespace wait for their cleanup. List them with `kubectl get codertemplatetests -n <namespace>`, then see [A `CoderTemplateTest` does not finish deleting](#a-codertemplatetest-does-not-finish-deleting).
2. If the namespace condition `NamespaceDeletionContentFailure` is `True`, read its message. Kubernetes sets this condition when any API fails while the namespace controller removes the resources of the namespace. If the message names `aggregation.coder.com`, it contains the error that the aggregated API returned when the namespace controller listed its resources, for example a `503` or a `400`. For a `503`, see [Namespaces without a Coder backend](../reference/aggregated-api-behavior.md#namespaces-without-a-coder-backend) and [Aggregated reads return `ServiceUnavailable`](#aggregated-reads-return-serviceunavailable). For a `400` that says `multiple eligible CoderControlPlane instances`, see [Aggregated reads return `multiple eligible CoderControlPlane instances`](#aggregated-reads-return-multiple-eligible-codercontrolplane-instances). If the message names another API, examine that API instead. If a `CoderControlPlane` is still in the namespace, check its finalizers, for example `coder.com/workspace-rbac-cleanup`, and the controller logs.

    ```bash
    kubectl get namespace <namespace> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.message}{"\n"}{end}'
    kubectl get codercontrolplanes -n <namespace> -o jsonpath='{range .items[*]}{.metadata.name}: {.metadata.finalizers}{"\n"}{end}'
    ```

## Troubleshoot workspace pools

`kubectl get` shows the reason of each [pool](workspace-pools.md), member, and claim in the `REASON` column. `status.message` says more:

```bash
kubectl get coderworkspacepools,coderworkspacepoolmembers,coderworkspaceclaims -n <namespace>
kubectl get coderworkspacepoolmember <name> -n <namespace> -o jsonpath='{.status.reason}: {.status.message}{"\n"}'
```

A wait ends by itself when its cause goes away. A failure is final for a member: its phase becomes `Failed`. The pool deletes a failed unclaimed member and creates a new one. A claimed member that fails stays, and its claim stays `Bound` and not ready until you delete the claim. If its workspace is gone, the claim becomes `Lost` instead. A failure of the pool sets `Stalled=True`. A stalled pool reads Coder again every 60 s, so a fix in Coder ends the stall without a spec change. A claim often does not show why its pool cannot create workspaces. If a claim waits for a long time, read the reason of its pool.

### Reasons of a pool and of a member before its create request

The pool checks its inputs in Coder, and each member checks them again right before it sends its create request.

| Reason | Kind | Cause | Fix |
| --- | --- | --- | --- |
| `ControlPlaneNotReady`, `OperatorAccessNotReady` | wait | The control plane in `spec.controlPlaneRef` does not exist, is being deleted, or has no URL, operator access, or TLS trust yet. | See [`CoderControlPlane` stays `Pending`](#codercontrolplane-stays-pending). |
| `ControlPlaneDeleting` | wait | A read without the cache shows that the control plane is being deleted, before the cache shows it. No member creates a workspace, and members no longer check or remove shares ([#448](https://github.com/coder/coder-k8s/issues/448)). | None. See [Cleanup](workspace-pools.md#cleanup). |
| `OwnerNotAllowed`, `TemplateNotAllowed` | wait | `spec.workspacePools.owners` of the control plane does not list the owner, or does not list the template for that owner. The pool deletes its unclaimed workspaces. | The control plane admin adds the owner or the template. |
| `OwnerNotEligible` | wait | The owner breaks an [owner rule](workspace-pools.md#1-create-the-pool-owner), and the message names the rule, or the owner is not a member of the template's organization. The pool deletes its unclaimed workspaces. | Fix the owner in Coder. For a dormant owner, see [The owner is dormant](#the-pool-owner-is-dormant). |
| `TemplateNotFound`, `TemplateVersionNotFound`, `PresetNotFound` | wait | `spec.template`, `spec.version`, or `spec.presetName` names nothing in Coder. | Create it in Coder, or fix `spec.version` or `spec.presetName`. `spec.template` cannot change: create a new pool. |
| `TemplateVersionImporting` | wait | The template version is still importing, or Coder answered `406` to a member's create request. | None. |
| `TemplateDeprecated`, `TemplateVersionArchived`, `TemplateVersionImportFailed`, `TemplateVersionMismatch` | failure | Coder accepts no new workspaces from the template or the version, or the version belongs to another template. | Remove the deprecation or the archive flag in Coder, or select another version. |
| `CoderUnavailable` | wait | A Coder request failed. The message holds the HTTP status and Coder's short message. After a `429`, a member waits 2 s, doubled per answer up to 2 min. | Check Coder and its logs. |
| `CoderAnswerMismatch` | wait | Coder, a proxy, or the SDK answered about another object than the one the operator asked for. The operator logs the error. | Check the proxies between the operator and Coder. |

### Reasons of a pool

| Reason | Kind | Cause | Fix |
| --- | --- | --- | --- |
| `Ready` | ready | At least `spec.replicas` current unclaimed workspaces are ready, and no outdated unclaimed workspace is left. | None. |
| `WaitingForMembers` | wait | Workspaces build, or outdated workspaces wait for their replacements. | Read the reasons of the members that are not `Ready`. |
| `OwnerWorkspaceLimit` | wait | The owner has `maxWorkspaces` members on this control plane, across all pools. Claimed and used members count. A member checks the cap again right before its create request, and waits with the same reason. | Delete claims or used members, or raise `maxWorkspaces`. |
| `FailureBackoff` | wait | A member failed. The pool waits 30 s, doubled per failure in a row up to 10 min, before it creates the next member. `status.lastFailure` names the failed member and its reason. | Fix the cause of the failure. |
| `ShareNotAllowed` | wait | The pool is in Share mode, and its owner is not allowed to share: the control plane lists no `shareGroupIDs` for the owner, the deployment disables sharing, or the organization's sharing setting does not let the owner share. The claims of the pool wait with the same reason and are no demand. | See [Share a workspace with a person](workspace-pools.md#6-share-a-workspace-with-a-person). |
| `MemberFailureLimit` | failure | 5 members failed in a row, and the pool creates no more. `status.runFailures` lists them. | Fix the cause. Then change `spec.version`, `spec.presetName`, `spec.parameters`, or `spec.readyTimeoutSeconds`, or wait for a new active version. A resize does not reset the count. |

### Reasons of a member

| Reason | Kind | Cause | Fix |
| --- | --- | --- | --- |
| `NotOwned` | failure | No pool controls the member, or its pool was deleted and created again under the same name. Only the operator creates members. | Delete the member. |
| `PoolNotFound`, `PoolDeleting` | wait | The member's pool does not exist or is being deleted. | None. Garbage collection deletes the member. |
| `InputsMismatch` | failure | The member pins inputs that its pool no longer builds, for example after a spec change. | None. The pool replaces the member. |
| `WorkspaceNameConflict` | failure | Coder already has a workspace with the member's `kwp-` name, and nothing proves that the member created it. The operator never touches that workspace. | None. The pool replaces the member under a new name. If the member then does not finish deleting, see [A member does not finish deleting](#a-pool-member-does-not-finish-deleting). |
| `CreatingWorkspace` | wait | The member stored its create marker and sends the create request. | None. |
| `CreateRetrying` | wait | Coder answered `401` or `429`, or the request was not sent, so nothing changed in Coder. The member sends the request again. | If it stays, check the operator token and Coder. |
| `CreateRejected` | failure | Coder refused the create request with another `4xx` answer. | Fix what the message names. |
| `ConfirmingCreate` | wait | The result of the create request is unknown, for example after a timeout or a `5xx`. The member reads Coder and never sends the request again. | None. |
| `CreateOutcomeUnknown` | failure | No workspace of the member appeared within 15 min of a create request with an unknown result. Only a site owner's reads prove that, so with another operator user the member waits with `ConfirmingCreate` until `DeadlineExceeded`. | None. The pool replaces the member. |
| `WaitingForBuild`, `WaitingForAgents` | wait | The start build runs, or an agent has not connected or is not ready yet. | None. |
| `BuildFailed`, `BuildCanceled` | failure | The start build failed, or someone else canceled it. The message holds only Coder's error code. | Read the build log in Coder. |
| `NoAgents` | failure | The workspace has no agent, so nothing proves that it works. | Add an agent to the template. |
| `AgentStartError`, `AgentStartTimeout`, `AgentStopped`, `AgentConnectionTimeout` | failure | The startup script of an agent failed or timed out, an agent stopped, or an agent did not connect in time. | Read the agent logs in Coder. |
| `DeadlineExceeded` | failure | The workspace was not ready within `spec.readyTimeoutSeconds` (default 900) of the member's creation. The message names the last wait. | Fix the cause of the last wait, or raise `readyTimeoutSeconds`. |
| `WorkspaceReady` | ready | Every agent is ready. A claim can take the workspace. | None. |
| `Used` | wait | The workspace's `last_used_at` moved after it became ready, so someone used it without a claim, or Coder counted something else as a use. Opening the workspace's page in the Coder dashboard counts ([#444](https://github.com/coder/coder-k8s/issues/444)). The member gets `Used=True`, and the pool releases it. | See [Cleanup](workspace-pools.md#cleanup). |
| `AgentNotReady` | wait | An agent of a ready workspace disconnected or is no longer ready. No claim takes the workspace meanwhile. A failed Coder read keeps this reason, and the 5 min keep counting. | None, unless it becomes `AgentLost`. |
| `AgentLost` | failure | An agent stayed not ready for 5 min. While Coder is unreachable, the member waits. A failed read keeps `AgentNotReady` and its 5 min. `AgentLost` comes with the first successful read after the 5 min, if the agent is still not ready. | Read the agent logs in Coder. |
| `ChangedExternally` | failure | Someone else started a build of the workspace, for example a stop, an update, or Coder's autostop. | None. The pool replaces an unclaimed member. |
| `Dormant` | failure | Coder marked the workspace dormant, or scheduled its deletion. | Check the template's dormancy settings. |
| `MaxDeadline` | wait | The workspace's max deadline allows no extension of its autostop deadline. Coder stops the workspace at the max deadline, and the pool replaces it. | None. |
| `ExtendRefused` | wait | Coder refused to extend the autostop deadline. The member tries again at its next check, every 60 s. | Read Coder's message. |
| `OwnershipUnknown` | failure | Nothing proves that the workspace is the member's: someone renamed it, it has another owner or template, or the operator did not start its build. During a delete, the member waits with this reason and never deletes the workspace. | See [A member does not finish deleting](#a-pool-member-does-not-finish-deleting). |
| `WorkspaceDeletedExternally` | failure | Someone deleted the workspace in Coder. A claim that holds the member becomes `Lost`. | None. |
| `Bound` | ready | A claim holds the member. The operator extends its autostop deadline at most once, at the bind. | None. |
| `Shared`, `ShareRefused`, `ShareRevoking`, `ShareRevoked`, `ShareRemoved` | ready | The state of the share for a Share claim, also in the member's condition `Shared`. `Shared` means that Coder lists the claim's user on the workspace. | See [Reasons of a claim](#reasons-of-a-claim). |
| `Deleting`, `DeleteRetrying` | wait | The member is being deleted, and its delete build runs, or failed and runs again after 1 min, doubled per failure up to 30 min. The operator cancels an active start or stop build first. | Read the log of the delete build in Coder. |
| `Deleted` | done | The delete build succeeded, and the member's finalizer goes. | None. |

The condition `WorkspaceDeleted` tells whether the member's workspace still exists. During a delete, it is `False` with the reason of the current delete step, for example `Deleting` or `CoderUnavailable`. While it is `True`, with the reason `NotCreated`, `DeletedExternally`, or `Deleted`, a deleted member releases its finalizer without a Coder call. `CleanupPending` (`False`) marks a failed member whose workspace can exist: a delete of the member deletes it. With `OwnershipUnknown` or `CreateOutcomeUnknown` (`Unknown`), the finalizer stays. `CreateOutcomeUnknown` during a delete means that the operator cannot prove whether a create request with an unknown result made a workspace. The member reads Coder again every 10 min. Check that the operator user is a site owner: only then does a `404` prove that no workspace exists.

### Reasons of a claim

| Reason | Phase | Cause | Fix |
| --- | --- | --- | --- |
| `WaitingForReadyWorkspace` | `Pending` | The pool has no ready unclaimed workspace for the claim yet. The pool creates one for each waiting claim that can bind, also at `replicas: 0`. | If it stays, read the reason of the pool. |
| `PoolNotFound` | `Pending` | `spec.poolName` names no pool, or the pool is being deleted. | Create the pool. |
| `ShareUserNotFound` | `Pending` | `spec.share.user` names no Coder user. The pool checks the claim again every minute. | Create the user in Coder, or create a claim with the right name. |
| `ShareUserNotEligible` | `Pending` | The user is not active, is not a member of the template's organization, or is in none of the groups in the owner's `shareGroupIDs`. The message names each group that the operator failed to read. The pool checks the claim again every minute. A claim that names the prebuilds system user is `Stalled`. | See [A Share claim waits with `ShareUserNotEligible`](#a-share-claim-waits-with-shareusernoteligible). |
| `ShareNotAllowed` | `Pending` | The pool's owner is not allowed to share. | See the pool reason `ShareNotAllowed`. |
| `ClaimModeMismatch` | `Pending` | The claim sets `spec.share`, but the pool is in Reserve mode, or the other way around. Both are immutable, so the claim is `Stalled` and never binds. | Delete the claim, and create one that matches the pool's `claims.mode`. |
| `WorkspaceReady` | `Bound` | The claimed workspace is ready, and in Share mode shared. `status.ownerName` and `status.workspaceName` name it. | None. |
| `SharingWorkspace` | `Bound` | The claim is bound, and its member shares the workspace with `spec.share.user`. | None. |
| `ShareRefused` | `Bound` | The share policy or Coder refused the share after the bind. The claim is `Stalled`, and the member checks again every minute. | Fix what the message names. |
| `ShareRevoking`, `ShareRevoked` | `Bound` | The admin withdrew what allowed the share, and the operator removes it (`ShareRevoking`) or removed it. The claim is `Stalled`. | See [A user keeps access after a share ends](#a-user-keeps-access-after-a-share-ends). |
| `ShareRemoved` | `Bound` | Coder no longer lists the user on the workspace: an admin tightened the organization's sharing setting, or a person removed the share. The claim is `Stalled`. | See [A user keeps access after a share ends](#a-user-keeps-access-after-a-share-ends). |
| a member reason, or `WorkspaceNotReady` | `Bound` | The claimed workspace is not ready. The claim shows the reason of its member. | See [Reasons of a member](#reasons-of-a-member). |
| `MemberDeleted` | `Lost` | The member that held the claim is gone. | Delete the claim, and create a new one. |
| `WorkspaceDeleted` | `Lost` | Someone deleted the claimed workspace in Coder. | Delete the claim, and create a new one. |
| `IdleTimeout` | `Lost` | Nobody used the workspace for `spec.claims.idleTimeoutSeconds` since the bind and since its last use, so the pool deletes it, unless its last check before the delete sees a use. | See [Idle reaping](workspace-pools.md#4-claim-a-workspace). |

`Lost` is final: the claim never binds again. A claim of a Share pool can also wait with a reason of its pool, for example `OwnerNotAllowed` or `CoderUnavailable`.

### A Share claim waits with `ShareUserNotEligible`

The message says which check failed. When it names a group that the operator failed to read, Coder answered `403` or `404` to the group read. Every group other than an organization's `Everyone` group needs a Premium license, and without one Coder answers `403`. Without a license, list the ID of the template's organization in `shareGroupIDs`: it stands for that organization's `Everyone` group, so every active member of the organization can get a share. The message tells the claim's author facts about the Coder user, such as its status ([#449](https://github.com/coder/coder-k8s/issues/449)).

### A user keeps access after a share ends

After `ShareRevoked` or `ShareRemoved`, Coder no longer lists the user on the workspace, but the user keeps access until the workspace restarts or stops. Coder applies the removal of a share only then, and the operator does not restart or stop the workspace ([#450](https://github.com/coder/coder-k8s/issues/450)). To end the access now, restart or stop the workspace in Coder, or delete the claim. The operator does not share the workspace again for this claim.

### The pool owner is dormant

The pool waits with `OwnerNotEligible`, and the message names the status `dormant`. Coder creates users through its API as `dormant`, unless the create request sets another status. Every 15 minutes, Coder marks each active user dormant whose last login or API use is older than 90 days. An activation does not count as a use. Activate the owner in Coder, then send one request with the owner's credentials, for example a password login with `POST /api/v2/users/login`. A service account cannot log in: send a request such as `GET /api/v2/users/me` with its API token instead. After that, use the owner's credentials at least once every 90 days. The pool deletes its unclaimed workspaces while the owner is not eligible, and builds new ones once the owner is active.

### A claim stays `Pending` and names a member

Before the pool binds a claim, it stores the chosen member in the claim's `status.memberName`. That reservation holds until the member is gone, is being deleted, failed, was used, or holds another claim. A member that stays ready but whose workspace the pool cannot read keeps the reservation, and the claim waits. If the pool cannot read any workspace, fix Coder first. Otherwise, to end the wait, delete the member that `status.memberName` names. Only a cluster admin can delete members. The member's cleanup deletes its workspace, unless it sees a use. The pool then binds the claim to another member:

```bash
kubectl delete coderworkspacepoolmember "$(kubectl get coderworkspaceclaim <claim> -n <namespace> -o jsonpath='{.status.memberName}')" -n <namespace>
```

### A pool member does not finish deleting

A member keeps the finalizer `coder.com/workspace-pool-member-cleanup` until its cleanup ends: its workspace is deleted, the operator proves that the member made none, the cleanup keeps a workspace that someone used, or the control plane is gone. When the condition `WorkspaceDeleted` is `Unknown` with the reason `OwnershipUnknown`, nothing proves that the workspace is the member's, and the operator never deletes it. A renamed `kwp-` workspace is the usual cause: the operator proves that a workspace is the member's by its `kwp-` name, so a rename fails that proof. Do not rename these workspaces. A claimed member with `OwnershipUnknown` keeps its claim `Bound` and not ready. Delete the claim first.

1. Find the workspace in Coder by the member's `status.workspaceID`, and delete it, or keep it on purpose.
2. Remove only the controller's finalizer. Find its position in the list, where the first entry has position 0:

    ```bash
    kubectl get coderworkspacepoolmember <name> -n <namespace> -o jsonpath='{.metadata.finalizers}'
    kubectl patch coderworkspacepoolmember <name> -n <namespace> --type json -p \
      '[{"op":"test","path":"/metadata/finalizers/<position>","value":"coder.com/workspace-pool-member-cleanup"},{"op":"remove","path":"/metadata/finalizers/<position>"}]'
    ```

CAUTION: Delete the workspace in Coder before you remove the finalizer. Otherwise the workspace stays in Coder, and it keeps the pool owner's token from its start build.
