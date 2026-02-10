---
name: kind-dev
description: Per-workspace KIND clusters for coder-k8s dev + demos.
---

# KIND dev clusters (coder-k8s)

Load this skill only when you need a real Kubernetes cluster (KIND) during development or demos.

## Unique cluster names (parallel agents)

Prefer a per-workspace name to avoid collisions:

```bash
export CLUSTER_NAME="coder-k8s-${MUX_WORKSPACE_NAME:-dev}"
```

## Bootstrap

```bash
./hack/kind-dev.sh up
kubectl --context kind-${CLUSTER_NAME} get nodes
```

## Start controller (out-of-cluster)

```bash
# up already switches your current context; run ctx again if needed.
./hack/kind-dev.sh ctx

GOFLAGS=-mod=vendor go run . --app=controller
```

## Optional: in-cluster controller

```bash
./hack/kind-dev.sh load-image
kubectl --context kind-${CLUSTER_NAME} apply -f config/e2e/deployment.yaml
kubectl --context kind-${CLUSTER_NAME} -n coder-system wait --for=condition=Available deploy/coder-k8s --timeout=120s
```

## Demo with k9s

```bash
k9s --context kind-${CLUSTER_NAME}
```

## Cleanup

```bash
./hack/kind-dev.sh down
```

## Validation when implementing

1. Nix tools available:

   ```bash
   nix develop -c kubectl version --client
   nix develop -c kind version
   nix develop -c k9s version
   ```

2. Script sanity:

   ```bash
   bash -n hack/kind-dev.sh
   ```

3. Bootstrap smoke test (deterministic cluster name):

   ```bash
   CLUSTER_NAME=coder-k8s-dev ./hack/kind-dev.sh up
   test "$(kubectl config current-context)" = "kind-coder-k8s-dev"
   kubectl --context kind-coder-k8s-dev get crd codercontrolplanes.coder.com
   kubectl --context kind-coder-k8s-dev get clusterrole manager-role
   kubectl --context kind-coder-k8s-dev -n coder-system get sa coder-k8s
   ```

4. Optional in-cluster controller smoke test:

   ```bash
   CLUSTER_NAME=coder-k8s-dev ./hack/kind-dev.sh load-image
   kubectl --context kind-coder-k8s-dev apply -f config/e2e/deployment.yaml
   kubectl --context kind-coder-k8s-dev -n coder-system wait --for=condition=Available deploy/coder-k8s --timeout=120s
   ```

5. Skill file exists:

   ```bash
   test -f .mux/skills/kind-dev/SKILL.md
   ```

6. Cleanup:

   ```bash
   CLUSTER_NAME=coder-k8s-dev ./hack/kind-dev.sh down
   ```

Use explicit `kubectl --context kind-${CLUSTER_NAME}` commands whenever possible to avoid acting on the wrong cluster.
