# coder-k8s feature map

Each file in this directory is one user-facing feature: what a user does with `kubectl`, and how to prove the result. Use it after `"$V" launch` and `"$V" doctor` pass (see [../SKILL.md](../SKILL.md)).

## Index

| Feature | File | Needs beyond the baseline | Last checked |
| --- | --- | --- | --- |
| Deploy Coder with a `CoderControlPlane` | [codercontrolplane.md](codercontrolplane.md) | Nothing | 2026-10-29, full journey, proof run |
| Templates and workspaces through the aggregated API | [aggregated-templates-workspaces.md](aggregated-templates-workspaces.md) | Network to the Terraform registry | 2026-10-29, full journey, proof run |
| AI providers with `CoderAIProvider` | [coderaiprovider.md](coderaiprovider.md) | Nothing (a throwaway key value) | 2026-10-29, full journey, trial run |
| Template tests with `CoderTemplateTest` | [codertemplatetest.md](codertemplatetest.md) | A tester user, the alpine image in the node, network to the Terraform registry and GitHub | 2026-10-29, pass and fail tests, trial run |
| External provisioners with `CoderProvisioner` | [coderprovisioner.md](coderprovisioner.md) | A Coder Premium license | 2026-10-29, unlicensed half only (`NotEntitled`) |

Features without a file yet: `CoderAIGatewayKey` (Premium license, `docs/how-to/manage-ai-gateway-keys.md`), `CoderMCPServer` (`docs/how-to/manage-mcp-servers.md`), `CoderWorkspaceProxy` (Premium license, `docs/how-to/workspace-proxy-credentials.md`), `CoderTemplateVersion` promote (`docs/reference/aggregated-api-behavior.md`).

## Baseline preconditions

Every journey assumes the state that `"$V" launch` leaves, and that `"$V" doctor` prints `doctor: PASS`:

- KIND cluster `<run ID>`, private kubeconfig `$VERIFY_RUN_DIR/kubeconfig`.
- The operator built from this checkout in `coder-system/deploy/coder-k8s` (`--app=all`), and the APIService `v1alpha1.aggregation.coder.com` `Available`.
- `CoderControlPlane` `coder/coder` with `status.phase=Ready` and `status.operatorAccessReady=true`, Coder `v2.37.2` without a license.
- The default Coder organization `coder`, and the operator user `coder-k8s-operator` (site owner).

## Driving conventions

- Export `V="$PWD/.mux/skills/verify-coder-k8s/scripts/verify.sh"` and `VERIFY_RUN_DIR` in every shell. The `bash -c` steps read `$V` from the environment.
- Run every kubectl command as `"$V" kubectl ...`, never as a bare `kubectl`: the helper refuses a cluster that this run did not create.
- Read Coder only with `"$V" coder-api ...`. It prints the body on stdout and `HTTP <code>` on stderr.
- Use the namespace `coder` and the control plane `coder` unless the journey says otherwise.
- Give objects that a journey creates a `verify-` prefix, so they are easy to find in `collect` output.
- Wait with `"$V" kubectl wait --for=...` and a timeout, never with a bare `sleep`.

## Proof and skip reporting

- Wrap every step that proves something in `"$V" capture <label> -- ...`. A journey result cites the evidence files.
- A journey passes only when every labeled step shows its observable result, including the Coder API check.
- Report each journey as `PASS`, `FAIL` or `SKIP`, with one sentence of reason and the evidence file names.
- A journey whose precondition the baseline lacks (for example a Premium license) is a `FAIL` with the reason `precondition missing: <what>`, not a pass and not a weaker journey. Capture the state that shows the missing precondition (for example `ExternalProvisionersEntitled=False NotEntitled`). Report `SKIP` only for a journey that you did not start.
- Never change a journey to make it pass. If the documented result is wrong, report the difference and put the reason in the feature's `Gotchas`.

## Feature entry contract

A feature file has, in this order:

1. An H1 with the feature name.
2. One paragraph of user-visible behavior.
3. `## Sub-features`: a list of the parts a user can drive.
4. `## How to get to it (user POV)`: the docs page and the objects a user writes.
5. `## Driving it with kubectl`: starts with `Preconditions:`, then labeled bullets. Each bullet pairs one user action with its exact command and its observable result.
6. `## Gotchas`: the traps, the missing preconditions and the timings that a journey runs into.

Use real names from the repo: manifests under `config/` and `examples/`, condition types and reasons from `api/v1alpha1/` and `docs/`.
