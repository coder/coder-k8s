---
name: verify-coder-k8s
description: Verify coder-k8s end to end. Launch the real operator (controller + aggregated API server, built from this checkout) on a disposable KIND cluster with a real Coder, drive the user surface (the coder.com/v1alpha1 CRDs and the aggregation.coder.com/v1alpha1 API) with kubectl, check side effects through the Coder API, keep evidence, and tear down only what the run created. Use it to prove a change to coder-k8s works for a user, to reproduce a bug on a live cluster, or to dogfood a feature before a PR.
---

# Verify coder-k8s on a disposable KIND cluster

The user surface of coder-k8s is the Kubernetes API:

- CRDs in `coder.com/v1alpha1`: `CoderControlPlane`, `CoderAIProvider`, `CoderAIGatewayKey`, `CoderMCPServer`, `CoderProvisioner`, `CoderTemplateTest`, `CoderWorkspaceProxy`. The operator reconciles them.
- The aggregated API `aggregation.coder.com/v1alpha1`: `CoderTemplate`, `CoderWorkspace`, `CoderTemplateVersion`. The operator pod serves them from a live Coder.

A user touches only `kubectl`. So this skill drives `kubectl`, then checks the result in Coder through the Coder REST API.

All commands run from the repository root. One helper does the work:

```bash
export V="$PWD/.mux/skills/verify-coder-k8s/scripts/verify.sh"
export VERIFY_RUN_DIR="${XUM_SCRATCH_DIR:-/tmp}/verify-coder-k8s/runs/$(date -u +%Y%m%dT%H%M%SZ)"
```

`VERIFY_RUN_DIR` is the run directory. It must be absolute and new. Every later command reads `$VERIFY_RUN_DIR/run.env`, so set the same value in every shell of the run. The evidence goes to `$VERIFY_RUN_DIR/evidence/` and stays there after cleanup.

The feature map is in [features/README.md](features/README.md). Pick a feature there after Launch and Doctor pass.

## Launch

Requirements: Linux amd64, Docker, Go (the version in `go.mod`), `jq`, `curl`, `git`, network access to GitHub, `dl.k8s.io`, Docker Hub, `ghcr.io` and the Terraform registry. Plan for 3 CPUs and 4 GiB of free memory. Run one cluster at a time on a shared host.

```bash
"$V" launch
```

`launch` goes from an empty state to a usable state. It refuses a `VERIFY_RUN_DIR` that already holds a run. Its steps, in order (`$VERIFY_RUN_DIR/launch-steps.tsv` records each duration):

1. Downloads kind `v0.26.0` and kubectl `v1.31.4` into `$VERIFY_BASE/bin` (default `${XUM_SCRATCH_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}}/verify-coder-k8s/bin`) and checks their SHA-256 sums. The script refuses `VERIFY_BASE`, `bin` and the run directory unless this user owns them and no other user can write them: `bin` goes first on `PATH` and `run.env` is sourced. These are the versions of `helm/kind-action` v1.12.0 in the CI job "E2E (Kind + CNPG + Templates)". The script puts this directory first on `PATH`, so a `kind` shim on the host is never used.
2. Picks a run ID `vck-<6 hex>`. The run ID is also the KIND cluster name and the image tag suffix, so two runs can live side by side. It reads the Coder image (`CODER_E2E_IMAGE`) and version (`E2E_EXPECT_CODER_VERSION`) from `.github/workflows/ci.yaml`.
3. Runs `./hack/kind-dev.sh up` with `CLUSTER_NAME=<run ID>`, `KIND_NODE_IMAGE=kindest/node:v1.32.0` and `KUBECONFIG=$VERIFY_RUN_DIR/kubeconfig`. That creates the cluster and applies `config/e2e/namespace.yaml`, `config/crd/bases/` and `config/rbac/`. Your own `~/.kube/config` does not change. Then it writes the ownership marker ConfigMap `kube-system/verify-coder-k8s-run` (`data.run` = run ID).
4. Builds `./` into `$VERIFY_RUN_DIR/build/linux/amd64/coder-k8s` (`GOFLAGS=-mod=vendor CGO_ENABLED=0`, Go cache in `$VERIFY_BASE/gocache`), and builds the image `coder-k8s-verify:<run ID>` from `Dockerfile.goreleaser`. Nothing is written into the checkout.
5. Loads the image into the node with `kind load docker-image` (falls back to `docker save | ctr import`), and tags it `ghcr.io/coder/coder-k8s:e2e` on the node with `ctr -n k8s.io images tag --force`. `config/e2e/deployment.yaml` uses that name with `imagePullPolicy: Never`, so the manifest stays unchanged.
6. Applies `deploy/apiserver-service.yaml`, `deploy/apiserver-apiservice.yaml` and `config/e2e/deployment.yaml` (`--app=all`: controller and aggregated API server in one pod). Waits for `deploy/coder-k8s` and `apiservice/v1alpha1.aggregation.coder.com` to be `Available`, for the APIService `caBundle` to equal `coder-system/coder-k8s-apiserver-tls` `ca.crt` with `insecureSkipTLSVerify` off, and for `kubectl get --raw /apis/aggregation.coder.com/v1alpha1` to answer.
7. Applies `examples/cloudnativepg/00-namespace.yaml` and `manifests/postgres.yaml`: PostgreSQL 17 on an emptyDir, and the Secret `coder/coder-db-app` with key `uri`. CI uses CloudNativePG instead. The Secret name and key are the same, so the example control plane works unchanged.
8. Renders `examples/cloudnativepg/codercontrolplane.yaml` with the CI overrides (pinned Coder image, `CODER_ACCESS_URL=http://coder.coder.svc.cluster.local`) plus `resources` of 1 CPU and 2 GiB (requests = limits). It keeps the rendered object in `evidence/launch-codercontrolplane.json` and applies it.
9. Waits for the usable state: condition `DatabaseSecretResolved=True`, `deployment/coder` rolled out, `status.phase=Ready`, `status.operatorAccessReady=true`, and `GET /api/v2/buildinfo` with the operator token reports the CI Coder version (`v2.37.2`). A Running pod alone is not ready: only `operatorAccessReady=true` means that the operator can call Coder.

A launch took about 3.5 minutes on this host (2026-10-29), with the node image pull (70 s) and a cold Go build (60 s). Plan for up to 15 minutes when the Coder image pull is slow. The log is `$VERIFY_RUN_DIR/launch.log`.

If `launch` fails, read the last lines of `launch.log`, run [Cleanup](#cleanup), fix the cause, and start again with a new `VERIFY_RUN_DIR`.

Teardown is the [Cleanup](#cleanup) command `"$V" cleanup`.

## Doctor

```bash
"$V" doctor
```

`doctor` is read-only. It prints one `PASS` or `FAIL` line per check and ends with `doctor: PASS` or `doctor: FAIL` (exit code 1):

1. The cluster is ours: `kind get clusters` lists the run ID, the node container carries the label `io.x-k8s.kind.cluster=<run ID>`, and the marker ConfigMap holds the run ID. If this fails, doctor checks nothing else.
2. Exactly one Ready, non-terminating operator pod (`coder-system`, label `app=coder-k8s`) runs the built image: the node image `ghcr.io/coder/coder-k8s:e2e` has the Docker image ID of `coder-k8s-verify:<run ID>`, and the pod's image digest is one of its repo digests.
3. Every CRD in `config/crd/bases/` is `Established`.
4. `apiservice/v1alpha1.aggregation.coder.com` is `Available`, and the discovery document lists all 7 resources: `codertemplates`, `codertemplates/promote`, `codertemplateversions`, `coderworkspaces`, `coderworkspaces/log`, `coderworkspaces/start` and `coderworkspaces/stop`.
5. `codercontrolplane/coder` in namespace `coder` has `phase=Ready` and `operatorAccessReady=true`.
6. The Coder API answers the operator token with the expected version.

Run doctor after launch, before each feature, and when a journey fails in a way that looks like the environment.

## Drive

Drive through three helper commands. All three refuse to act unless the cluster passes the ownership check of doctor step 1.

- `"$V" kubectl <args>` runs kubectl `v1.31.4` with this run's kubeconfig and context `kind-<run ID>`. It refuses any other cluster.
- `"$V" node <cmd...>` runs a command in the KIND node container (`docker exec -i <run ID>-control-plane`), for example `"$V" node crictl images` or `"$V" node ctr -n k8s.io images pull <image>`. Images pulled this way live only in the node, so `cleanup` removes them with the cluster.
- `"$V" coder-api <METHOD> <PATH> [BODY_FILE]` calls Coder as the operator user (the site owner `coder-k8s-operator`). It opens a new `kubectl port-forward` to `svc/coder` on a random local port for each call and closes it after the call. It prints the JSON body on stdout and `HTTP <code> <METHOD> <PATH>` on stderr, and exits non-zero for a non-2xx answer. It reads the token from the Secret in `status.operatorTokenSecretRef` into `$VERIFY_RUN_DIR/secrets/coder.hdr` (mode 0600) and passes it with `curl -H @file`, so the token never shows in argv or output. A `BODY_FILE` is a JSON file.

Stable handles that the launch creates:

| Handle | Value |
| --- | --- |
| Operator | `coder-system/deploy/coder-k8s`, pod label `app=coder-k8s`, container `manager` |
| Aggregated API | `apiservice/v1alpha1.aggregation.coder.com`, Service `coder-system/coder-k8s-apiserver`, CA Secret `coder-system/coder-k8s-apiserver-tls` |
| Control plane | `coder/codercontrolplane/coder`, Deployment and Service `coder/coder`, URL `http://coder.coder.svc.cluster.local:80` |
| Database | `coder/deploy/postgres`, Secret `coder/coder-db-app` key `uri` |
| Coder organization | `coder` (the default organization) |
| Operator user | `coder-k8s-operator`, site role `owner` |
| Aggregated names | `CoderTemplate` = `<organization>.<template>`, `CoderWorkspace` = `<organization>.<owner>.<workspace>` |

Examples:

```bash
"$V" kubectl -n coder get codercontrolplane coder \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
"$V" kubectl -n coder get codertemplates.aggregation.coder.com
"$V" coder-api GET /api/v2/users/me
```

Name aggregated resources with their full group (`codertemplates.aggregation.coder.com`), as the repo docs do. Each feature file in `features/` gives the journey: preconditions, user actions with exact commands, and the observable result of each action.

## Evidence

Capture every proof step with `capture`. It runs the command, prints its output, and writes `$VERIFY_RUN_DIR/evidence/<NNN>-<label>.txt` with the exact command line, the start time, the output, the exit code and the duration. It also appends a line to `evidence/index.tsv`. The label must match `^[a-z0-9][a-z0-9._-]*$`.

```bash
"$V" capture doctor -- "$V" doctor
"$V" capture ccp-conditions -- "$V" kubectl -n coder get codercontrolplane coder -o yaml
"$V" capture coder-template -- "$V" coder-api GET /api/v2/organizations/coder/templates/e2e-template
"$V" collect
```

`collect` writes `evidence/collect-<UTC time>/`: nodes and pods, events, every `coder.com` object, the aggregated templates and workspaces in `coder`, the APIService, the operator log and the Coder log. `cleanup` runs `collect` once more before it deletes the cluster.

Proof standards:

1. Use the real user path: `kubectl` against the real kube-apiserver, the real operator image built from this checkout, and a real Coder. Do not call the operator's internal endpoints, and do not edit status by hand.
2. Pair each action with the state it causes: the `kubectl` command, then the object's `status` (phase, conditions with `type`, `status` and `reason`) or the aggregated object it returns.
3. Prove side effects in Coder through the Coder API (`"$V" coder-api ...`): the template, workspace, build, provider or key exists, has the expected fields, or is gone. A Kubernetes status alone is not proof of a Coder change.
4. Use a mock only where a production boundary isolates it, for example an upstream LLM or MCP server (`hack/e2e/mcpstub`). Never mock Coder or the Kubernetes API.
5. Keep the run's `doctor` output and the `cleanup.txt` in the evidence.
6. Never capture secret values: no `get secret -o yaml`, no token files, no `env` dumps of the Coder pod.

## Cleanup

```bash
"$V" cleanup
```

`cleanup` deletes only what this run created, by the names in `$VERIFY_RUN_DIR/run.env`:

1. If the KIND cluster of the run exists and passes the ownership check, it runs `collect`.
2. It deletes the KIND cluster by its recorded name with `kind delete cluster --name <run ID>`, only if `run.env` records `CLUSTER_CREATED=1` and the node container carries the KIND label of that name.
3. It removes the image tag `coder-k8s-verify:<run ID>`.
4. It removes `kindest/node:v1.32.0` and `gcr.io/distroless/static:nonroot` only when this run pulled them (`PULLED_IMAGES` in `run.env`) and no container uses them.
5. It removes the run's build directory, token files and kubeconfig.
6. It prints the checks that prove the removal (`kind get clusters`, `docker ps -a --filter label=io.x-k8s.kind.cluster=<run ID>`, `docker images coder-k8s-verify:<run ID>`), and lists the evidence that stays. Its own output is `evidence/cleanup.txt`.

`cleanup` never stops processes by name and never touches another cluster. Run it after every failed attempt too: it is safe to run more than once.

After cleanup, check that the evidence survived:

```bash
ls -l "$VERIFY_RUN_DIR/evidence"
```

## Helpers

| File | Use |
| --- | --- |
| `scripts/verify.sh` (executable) | `"$V" launch`, `"$V" doctor`, `"$V" kubectl <args>`, `"$V" node <cmd...>`, `"$V" coder-api <METHOD> <PATH> [BODY_FILE]`, `"$V" capture <label> -- <cmd...>`, `"$V" collect`, `"$V" cleanup`. Run it with no arguments for the usage text. |
| `manifests/postgres.yaml` | Throwaway PostgreSQL and the `coder-db-app` Secret. `launch` applies it. |
| `features/*.md` | The feature map: one journey per user-facing feature. |

Environment variables of `scripts/verify.sh`:

| Variable | Default | Meaning |
| --- | --- | --- |
| `VERIFY_RUN_DIR` | none (required) | Absolute run directory: `run.env`, logs, evidence. |
| `VERIFY_BASE` | `${XUM_SCRATCH_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}}/verify-coder-k8s` (created with mode 0700, never a shared `/tmp`) | Shared tools (`bin/`) and Go cache (`gocache/`). |
| `GOCACHE` | `$VERIFY_BASE/gocache` | Go build cache for the operator binary. |

Other repo harnesses, for deeper checks:

- `bash ./hack/e2e-workspace-lifecycle.sh` is the CI lifecycle driver. It expects the CI cluster (`kind-e2e`, CloudNativePG, the Terraform mirror) and it deletes namespace `coder` at its end. Do not point it at a verify run unless you want those effects.
- `bash ./hack/e2e-workspace-lifecycle_test.sh` runs the driver offline against stubs. It needs no cluster.
