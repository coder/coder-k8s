# Templates and workspaces through the aggregated API

A user manages Coder templates and workspaces as Kubernetes objects in `aggregation.coder.com/v1alpha1`. `kubectl apply` of a `CoderTemplate` with `spec.files` uploads the files, waits until Coder finishes the import, and makes the new version active. A `CoderWorkspace` create starts a workspace build in Coder. `start`, `stop` and `log` subresources drive and read builds. `kubectl delete` deletes the workspace or template in Coder. Kubernetes stores none of these objects: every read comes from the live Coder behind the namespace's `CoderControlPlane`.

## Sub-features

- `CoderTemplate` create, re-apply (a files change adds a version) and delete. Name `<organization>.<template>`.
- `CoderTemplateVersion` get and list (read-only), and `codertemplates/<name>/promote`.
- `CoderWorkspace` create, get, list, delete. Name `<organization>.<owner>.<workspace>`.
- `coderworkspaces/<name>/start`, `/stop` (with `dryRun=All` preview) and `/log` (with `follow=true`).
- Delegated authentication and RBAC: callers need grants per resource and per subresource.

## How to get to it (user POV)

Docs: `docs/how-to/deploy-aggregated-apiserver.md`, `docs/how-to/start-stop-workspaces.md`, `docs/reference/aggregated-api-behavior.md`. The user applies a manifest like `config/e2e/codertemplate.yaml` (`coder.e2e-template`, one `null_resource`, provider `hashicorp/null` `3.2.4`), then creates a workspace that names the template.

## Driving it with kubectl

Preconditions: the baseline, and network from the KIND node to `registry.terraform.io` (the import downloads `hashicorp/null`). Export the paths once (the `bash -c` steps read them):

```bash
export API=/apis/aggregation.coder.com/v1alpha1/namespaces/coder/coderworkspaces
export WS=coder.coder-k8s-operator.verify-ws
```

- **Discover the API.** `"$V" capture agg-discovery -- bash -c '"$V" kubectl get --raw /apis/aggregation.coder.com/v1alpha1 | jq -r ".resources[].name"'` lists `codertemplates`, `codertemplates/promote`, `codertemplateversions`, `coderworkspaces`, `coderworkspaces/log`, `coderworkspaces/start` and `coderworkspaces/stop`.
- **Create the template.** `"$V" capture tpl-apply -- "$V" kubectl --request-timeout=60s apply -f config/e2e/codertemplate.yaml` prints `codertemplate.aggregation.coder.com/coder.e2e-template created` after the import ends (about 5 to 20 seconds).
- **Read the template.** `"$V" capture tpl-get -- "$V" kubectl -n coder get codertemplates.aggregation.coder.com coder.e2e-template -o jsonpath='{.status.id} {.status.activeVersionID}{"\n"}'` prints two UUIDs.
- **Prove the template in Coder.** `"$V" capture tpl-coder -- bash -c '"$V" coder-api GET /api/v2/organizations/coder/templates/e2e-template | jq "{id, name, active_version_id, display_name}"'` shows the same `id` and `active_version_id`, and `display_name` `E2E Template`. Then `export AV=$("$V" kubectl -n coder get codertemplates.aggregation.coder.com coder.e2e-template -o jsonpath='{.status.activeVersionID}')` and `"$V" capture tpl-import -- bash -c '"$V" coder-api GET "/api/v2/templateversions/$AV" | jq .job.status'` prints `"succeeded"`.
- **List the versions.** `"$V" capture tpl-versions -- "$V" kubectl -n coder get codertemplateversions.aggregation.coder.com` lists one version of `e2e-template` with `active` true.
- **Create the workspace.** `"$V" capture ws-create -- bash -c 'jq -n --arg n "$WS" "{apiVersion: \"aggregation.coder.com/v1alpha1\", kind: \"CoderWorkspace\", metadata: {name: \$n, namespace: \"coder\"}, spec: {organization: \"coder\", templateName: \"e2e-template\", running: true}}" | "$V" kubectl create -f -'` prints `coderworkspace.aggregation.coder.com/coder.coder-k8s-operator.verify-ws created`.
- **Wait for the build.** `"$V" capture ws-running -- "$V" kubectl -n coder wait --for=jsonpath='{.status.latestBuildStatus}'=running coderworkspaces.aggregation.coder.com/coder.coder-k8s-operator.verify-ws --timeout=180s` ends with `condition met`.
- **Prove the workspace in Coder.** `"$V" capture ws-coder -- bash -c '"$V" coder-api GET /api/v2/users/coder-k8s-operator/workspace/verify-ws | jq "{id, name, template_name, status: .latest_build.status, transition: .latest_build.transition}"'` shows `template_name` `e2e-template`, status `running`, transition `start`. Its `id` equals `status.id` of the `CoderWorkspace`.
- **Stop it.** `"$V" capture ws-stop -- bash -c '"$V" kubectl create --raw "$API/$WS/stop" -f - <<<"{}" | jq .status'` prints `outcome` `Queued` with a `buildID`. Then `"$V" kubectl -n coder wait --for=jsonpath='{.status.latestBuildStatus}'=stopped coderworkspaces.aggregation.coder.com/$WS --timeout=180s` succeeds, and the Coder API shows `latest_build.transition` `stop`.
- **Read the build log.** `"$V" capture ws-log -- "$V" kubectl get --raw "$API/$WS/log"` prints the Terraform log of the stop build.
- **Delete the workspace.** `"$V" capture ws-delete -- "$V" kubectl -n coder delete coderworkspaces.aggregation.coder.com "$WS" --timeout=180s`. Coder runs a delete build. Afterwards `"$V" capture ws-gone -- "$V" coder-api GET /api/v2/users/coder-k8s-operator/workspace/verify-ws` answers `HTTP 404` (exit 1 is the expected result), and the `kubectl get` of the workspace returns `NotFound`.
- **Delete the template.** `"$V" capture tpl-delete -- "$V" kubectl -n coder delete codertemplates.aggregation.coder.com coder.e2e-template`, then `"$V" capture tpl-gone -- "$V" coder-api GET /api/v2/organizations/coder/templates/e2e-template` answers `HTTP 404`.

## Gotchas

- Name the resources with the group (`codertemplates.aggregation.coder.com`). The short name `codertemplate` can also match a CRD in another group.
- Writes are capped at 34 seconds by the vendored `k8s.io/apiserver`. A slow template import returns `504` at 34 s even though Coder can still finish it. Read the template again before you retry.
- `config/e2e/codertemplate.yaml` downloads `hashicorp/null` from the registry. The agent template `config/e2e/codertemplate-agent.yaml` also needs `coder/coder` `2.19.0` and `hashicorp/kubernetes`, and the image `e2e.local/alpine:3.22.6` in the node. CI mirrors `coder/coder` into the node (step "Mirror the Terraform coder provider into the Kind node") because GitHub 500s failed imports. The baseline has no mirror, so a registry or GitHub outage fails the import here.
- `kubectl diff`, `--dry-run=server` and server-side dry-run writes return `400`: Coder has no dry-run. Only `promote`, `start` and `stop` accept `dryRun=All`.
- `kubectl wait` and `kubectl delete --timeout` on aggregated objects print `Failed to watch *unstructured.Unstructured: the server rejected our request for an unknown reason` once a second. The server refuses the watch that kubectl sends, and kubectl falls back to polling by list. The command still ends with `condition met` or `deleted`. The noise is not a failure.
- A watch shows only writes made through this server. There is no replay and no initial event list.
- The workspace `resourceVersion` is a fingerprint. Workspace activity alone can change it and cause `409` on a delete with preconditions.
- The `null_resource` template builds finish in about 3 seconds, so a second `start` right after the first one returns `Unchanged`, not `InProgress`.
- Aliases (`default`, `me`) and wrong letter case in names return `400`.
