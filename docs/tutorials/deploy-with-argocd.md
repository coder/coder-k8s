# Deploy operator stack with Argo CD

This tutorial deploys `coder-k8s` and the CloudNativePG-backed example stack through Argo CD.

Estimated time: **20–30 minutes**.

By the end, your cluster will have:

- the `coder-k8s` operator stack
- the aggregated API service (`aggregation.coder.com/v1alpha1`)
- CloudNativePG operator and a `coder-db` PostgreSQL cluster
- a `CoderControlPlane` named `coder`
- a live Coder deployment reachable through `svc/coder`

## Prerequisites

- A Kubernetes cluster with `kubectl` access
- Argo CD installed in namespace `argocd` (with ApplicationSet controller)
- `kubectl` v1.26+, `jq`, and `curl`
- `coder` CLI (for optional template + aggregated API verification)

!!! note
    This tutorial assumes Argo CD is already installed and healthy.

## 1) Verify your Argo CD control plane is healthy

```bash
kubectl -n argocd get deploy
kubectl -n argocd get pods
```

You should see the core Argo CD controllers running (`argocd-application-controller`, `argocd-applicationset-controller`, `argocd-repo-server`, `argocd-server`, etc).

## 2) Apply the ApplicationSet

```bash
kubectl apply -f https://raw.githubusercontent.com/coder/coder-k8s/main/examples/argocd/applicationset.yaml
kubectl -n argocd wait --for=create application/coder-k8s-stack --timeout=120s
```

!!! note
    The example `ApplicationSet` already includes `syncOptions` for
    `CreateNamespace=true` and `ServerSideApply=true`.

## 3) Wait for the Application to become Synced and Healthy

```bash
kubectl -n argocd wait --for=jsonpath='{.status.sync.status}'=Synced application/coder-k8s-stack --timeout=20m
kubectl -n argocd wait --for=jsonpath='{.status.health.status}'=Healthy application/coder-k8s-stack --timeout=20m
kubectl -n argocd get application coder-k8s-stack \
  -o jsonpath='{.status.sync.status}{"\n"}{.status.health.status}{"\n"}{.status.operationState.phase}{"\n"}'
```

## 4) Validate operator and aggregated API wiring

```bash
kubectl -n coder-system get deploy coder-k8s -o wide
kubectl -n coder-system get pods -o wide

kubectl get apiservice v1alpha1.aggregation.coder.com
kubectl get apiservice v1alpha1.aggregation.coder.com \
  -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{" reason="}{.reason}{" message="}{.message}{"\n"}{end}'

```

## 5) Validate database and Coder control plane bring-up

```bash
kubectl -n coder get cluster.postgresql.cnpg.io coder-db
kubectl -n coder wait --for=condition=Ready cluster/coder-db --timeout=10m
kubectl -n coder get secret coder-db-app

kubectl -n coder get codercontrolplane coder -o yaml
kubectl -n coder rollout status deployment/coder --timeout=10m
kubectl -n coder get svc coder
```

## 6) Complete initial Coder setup from the UI

Port-forward Coder (in a separate terminal, keep it running):

```bash
kubectl -n coder port-forward svc/coder 3000:80
```

Open the setup page in your browser (`/setup` on the forwarded endpoint).

Create your initial admin user and verify you can reach the templates page.

## 7) (Optional) Create a template directly in Coder

If you want to validate template round-trip behavior:

```bash
TOKEN=$(curl -sS -X POST http://127.0.0.1:3000/api/v2/users/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"<admin-email>","password":"<admin-password>"}' | jq -r '.session_token')

export CODER_URL=http://127.0.0.1:3000
export CODER_SESSION_TOKEN="$TOKEN"

coder templates init --id scratch /tmp/coder-template-scratch
coder templates push starter-scratch --directory /tmp/coder-template-scratch --yes
coder templates list
```

## 8) (Optional) Validate template visibility from aggregated API

```bash
kubectl get --raw /apis/aggregation.coder.com/v1alpha1
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A

TEMPLATE_FQN="$(kubectl get codertemplates.aggregation.coder.com -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep 'starter-scratch' | head -n1)"
[ -n "$TEMPLATE_FQN" ]
kubectl -n coder get codertemplates.aggregation.coder.com "$TEMPLATE_FQN" -o yaml
```

## Troubleshooting

- **Argo CD sync error: `metadata.annotations: Too long` on CNPG CRDs**
  - Ensure `ServerSideApply=true` is present in `spec.template.spec.syncPolicy.syncOptions` before applying the `ApplicationSet`.

- **CloudNativePG pod crash with `no matches for kind "Pooler"`**
  - This is a downstream effect of failed CRD apply; fix sync options and re-sync the application.

- **`APIService v1alpha1.aggregation.coder.com` not Available**
  - Check `coder-k8s` pod logs and confirm `coder-k8s-apiserver` Service exists in `coder-system`.

- **Coder deployment not rolling out**
  - Verify `coder-db` is Ready and `coder-db-app` secret exists.

## Cleanup

```bash
kubectl -n argocd delete applicationset coder-k8s-stack
```

The generated `Application` has the resources finalizer and will cascade deletion of managed resources.
