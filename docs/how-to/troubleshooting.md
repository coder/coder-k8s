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
2. The operator bootstrap token is not ready yet.
3. `spec.licenseSecretRef` points to a license Secret that is missing or not valid.
4. The controller cannot resolve `spec.database.connectionSecretRef`. Look at the reason of the `DatabaseSecretResolved` condition. See [Connect an external PostgreSQL database](deploy-controller.md#connect-an-external-postgresql-database).

```bash
kubectl get codercontrolplane <name> -n <namespace> -o yaml
kubectl get deploy,svc -n <namespace>
```

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

## Aggregated reads return `ServiceUnavailable`

This section is about `ServiceUnavailable` errors from the aggregated API server itself. Their messages are specific. For example, they say that the server found no eligible `CoderControlPlane`, or that the configuration for standalone mode is missing. kube-apiserver returns the general message `the server is currently unable to handle the request` instead. For that error, see [Proxied requests fail with 503 and an x509 error](#proxied-requests-fail-with-503-and-an-x509-error).

- `all` mode: no eligible `CoderControlPlane` exists yet. A control plane is eligible when its operator access is enabled and ready, its status has an operator token reference and a URL, and its name does not contain a `.` character.
- Standalone mode (`--app=aggregated-apiserver`): set all three flags `--coder-url`, `--coder-session-token`, and `--coder-namespace`.

The logs show which provider configuration the server used.

## Aggregated reads return `multiple eligible CoderControlPlane ...`

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
- `422`: `limitBytes` is below 1.
- `429` that says `for this user`: this user already has 4 open log requests on this server. Open `follow=true` streams count too. Close one, or wait and try again.
- `429` that says `on this server`: the server has 64 open log requests. Wait for the `Retry-After` time and try again.
- `429` that says `You've been rate limited`: Coder rate-limited the operator token, which every aggregated API call uses. Coder allows 512 requests per minute by default (`CODER_API_RATE_LIMIT`).
- `504`: Coder did not answer in time before the response started. Either one Coder call took longer than the Coder request timeout (30 seconds by default), or the time limit of the request ran out: 60 seconds for a snapshot, or 25 minutes with `follow=true`. Check that Coder is healthy, then try again.
- A `Warning` that says the server cut the log: the log is larger than the server limit. See [Workspace build log](../reference/aggregated-api-behavior.md#workspace-build-log).
- A `follow=true` stream ends before the build ends: the stream reached the 4 MiB response limit or the 25-minute time limit. The server sends no `Warning` for a cut in the live part of a stream. Send a new request with `follow=true`. It sends the existing entries again.

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

A test without any status means that the controller has not reconciled it. See [The controller runs but nothing reconciles](#the-controller-runs-but-nothing-reconciles). Every wait ends at `spec.timeoutSeconds` with `DeadlineExceeded`. For every reason, see [Read the result](test-templates.md#4-read-the-result).

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
2. With the aggregated API server installed, a namespace without an eligible `CoderControlPlane` never finishes deleting ([#209](https://github.com/coder/coder-k8s/issues/209)). Its condition `NamespaceDeletionContentFailure` is `True` with the message `no eligible CoderControlPlane instances found in namespace "<namespace>"`. The aggregated API answers the namespace controller's LIST with `503`. Read the conditions:

    ```bash
    kubectl get namespace <namespace> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.message}{"\n"}{end}'
    ```

    If `NamespaceContentRemaining` and `NamespaceFinalizersRemaining` are `False`, nothing is left in the namespace, and only #209 holds it.
