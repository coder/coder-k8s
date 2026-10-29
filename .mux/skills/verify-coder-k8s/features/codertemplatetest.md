# Template tests with CoderTemplateTest

A user checks that a Coder template version can start a working workspace by creating one `CoderTemplateTest` object. The controller creates a throwaway workspace from the version, owned by the tester user named in the control plane's `spec.templateTests.ownerUserID`, waits until every top-level agent is `connected` and `ready`, records `Succeeded` or `Failed` with a reason, and deletes the workspace. The spec is immutable: to test again, the user creates a new object.

## Sub-features

- Version selection: `spec.version.active`, `spec.version.name` or `spec.version.id`.
- Template parameters: `spec.parameters` (`name`, `value`).
- Time limit `spec.timeoutSeconds` (reason `DeadlineExceeded`), and expiry `spec.ttlSecondsAfterFinished`.
- Result: `status.phase` (`Pending`, `Running`, `Succeeded`, `Failed`), `status.reason`, conditions `Ready`, `Reconciling`, `Stalled`, `WorkspaceDeleted`.
- Workspace cleanup with `WorkspaceDeleted` reasons `Deleted`, `Deleting`, `Retained`, `ControlPlaneGone`.

## How to get to it (user POV)

Docs: `docs/how-to/test-templates.md`. Sample: `config/samples/coder_v1alpha1_codertemplatetest.yaml`. An admin creates the tester and sets `spec.templateTests.ownerUserID` once. A user then runs `kubectl create -f` with a test that names `<organization>.<template>`, and reads the result with `kubectl get codertemplatetests`.

## Driving it with kubectl

Preconditions: the baseline, plus these four, in order. They need network from the KIND node to `public.ecr.aws`, `registry.terraform.io` and GitHub releases.

1. The workspace image, pulled inside the node only: `IMG=public.ecr.aws/docker/library/alpine@sha256:3e9b4b680bfc9fb5269227cffbd6d42be39fbf7c0b908123913864aa4447e764; "$V" node ctr -n k8s.io images pull "$IMG" && "$V" node ctr -n k8s.io images tag --force "$IMG" e2e.local/alpine:3.22.6` (the CI step "Load the E2E workspace image" uses the same digest).
2. The agent template: `"$V" kubectl --request-timeout=60s apply -f config/e2e/codertemplate-agent.yaml` prints `codertemplate.aggregation.coder.com/coder.e2e-agent created` (about 10 seconds).
3. An active tester user, created through the Coder API with the password in a 0600 file:

   ```bash
   umask 077
   ORG_ID=$("$V" coder-api GET /api/v2/organizations | jq -er 'map(select(.is_default)) | .[0].id')
   jq -n --arg p "$(head -c 24 /dev/urandom | base64)" --arg o "$ORG_ID" \
     '{username: "verify-tester", name: "Verify tester", email: "verify-tester@verify.example.com",
       password: $p, login_type: "password", organization_ids: [$o]}' >"$VERIFY_RUN_DIR/secrets/tester.json"
   TESTER_ID=$("$V" coder-api POST /api/v2/users "$VERIFY_RUN_DIR/secrets/tester.json" | jq -er .id)
   "$V" coder-api PUT "/api/v2/users/$TESTER_ID/status/activate" | jq -c '{username, status}'
   ```

   The last command prints `{"username":"verify-tester","status":"active"}`. `$VERIFY_RUN_DIR/secrets/` exists after the first `coder-api` call, and `cleanup` deletes it.
4. The control plane names the tester: `"$V" kubectl -n coder patch codercontrolplane coder --type=merge -p "{\"spec\":{\"templateTests\":{\"ownerUserID\":\"$TESTER_ID\"}}}"`.

Journey:

- **Run a passing test.** `"$V" capture tt-create -- bash -c 'printf "%s\n" "apiVersion: coder.com/v1alpha1" "kind: CoderTemplateTest" "metadata: {name: verify-agent-pass, namespace: coder}" "spec:" "  controlPlaneRef: {name: coder}" "  template: coder.e2e-agent" "  version: {active: true}" "  timeoutSeconds: 300" | "$V" kubectl create -f -'` prints `codertemplatetest.coder.com/verify-agent-pass created`. Then `export TT=codertemplatetest/verify-agent-pass`.
- **Wait for the result.** `"$V" capture tt-succeeded -- "$V" kubectl -n coder wait --for=jsonpath='{.status.phase}'=Succeeded "$TT" --timeout=6m` prints `condition met` (about 20 seconds).
- **Read the result.** `"$V" capture tt-status -- "$V" kubectl -n coder get "$TT" -o jsonpath='{.status.phase} {.status.reason} {.status.workspaceName}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'` shows `Succeeded Succeeded` and a workspace name that starts with `ktt-`, `Ready=True Succeeded` and `WorkspaceDeleted=True Deleted`. `"$V" kubectl -n coder get codertemplatetests` shows the columns `PHASE`, `TEMPLATE`, `VERSION` and `REASON`.
- **Prove the workspace is gone in Coder.** `"$V" capture tt-coder -- bash -c '"$V" coder-api GET "/api/v2/workspaces?q=owner:verify-tester" | jq -c "{count}"'` prints `{"count":0}`.
- **Run a failing test.** `"$V" capture tt-fail-create -- bash -c 'printf "%s\n" "apiVersion: coder.com/v1alpha1" "kind: CoderTemplateTest" "metadata: {name: verify-agent-fail, namespace: coder}" "spec:" "  controlPlaneRef: {name: coder}" "  template: coder.e2e-agent" "  version: {active: true}" "  parameters: [{name: fail, value: \"true\"}]" "  timeoutSeconds: 300" | "$V" kubectl create -f -'`, then `export TT2=codertemplatetest/verify-agent-fail`. `"$V" capture tt-failed -- "$V" kubectl -n coder wait --for=jsonpath='{.status.phase}'=Failed "$TT2" --timeout=6m` succeeds in about 10 seconds. The status shows `Failed AgentStartError`, `Ready=False AgentStartError` and `Stalled=True AgentStartError`. Then `"$V" kubectl -n coder wait --for=condition=WorkspaceDeleted "$TT2" --timeout=3m` succeeds: the controller deletes the failed workspace too.
- **Delete the tests.** `"$V" kubectl -n coder delete codertemplatetests --all --timeout=2m` returns once each finalizer is gone.

## Gotchas

- Coder creates API users as `dormant`. A dormant owner's agents get `401` and the test ends `AgentConnectionTimeout`. Activate the tester first (precondition 3).
- The tester must not be a site owner or have another elevated role: the test then waits with `OwnerNotEligible`. Without `spec.templateTests.ownerUserID` it waits with `OwnerNotConfigured`.
- Setting `spec.templateTests.ownerUserID` does not roll `deployment/coder`.
- The baseline has no Terraform provider mirror. `config/e2e/codertemplate-agent.yaml` downloads `coder/coder` `2.19.0` from GitHub releases and `hashicorp/kubernetes` `3.2.1` from the registry. A GitHub 500 fails the import (CI #369). For a stable run, copy the CI step "Mirror the Terraform coder provider into the Kind node" and mount the mirror through the control plane's `spec.volumes`, as the CI step "Provision PostgreSQL and CoderControlPlane" does.
- The agent reaches Coder through `CODER_ACCESS_URL=http://coder.coder.svc.cluster.local`, which the launch sets. The example's `http://localhost:3000` does not work for workspace pods.
- Scaling the control plane to 0 replicas gives `CoderUnavailable`, not `OperatorAccessNotReady`: operator access uses Postgres.
- A namespace deletion while a test runs releases the test through `ControlPlaneGone` after about 11 seconds, and the workspace can stay in Coder.
