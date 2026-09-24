# Deploy with Argo CD

Deploy `coder-k8s` and a CloudNativePG-backed Coder instance from one Argo CD `ApplicationSet`.

**Time:** 20–30 minutes.

**You end up with:**

- The `coder-k8s` operator and aggregated API (`aggregation.coder.com/v1alpha1`).
- The CloudNativePG operator and a PostgreSQL cluster named `coder-db`.
- A `CoderControlPlane` named `coder`, served by `svc/coder`.

## Prerequisites

- A Kubernetes cluster with `kubectl` v1.26+ access.
- Argo CD, including the ApplicationSet controller, running in namespace `argocd`.
- `jq` and `curl`.
- The `coder` CLI (only for the optional steps).

## 1. Check Argo CD

```bash
kubectl -n argocd get deploy,pods
```

`argocd-application-controller`, `argocd-applicationset-controller`, `argocd-repo-server`, and `argocd-server` should be running.

## 2. Apply the ApplicationSet

```bash
kubectl apply -f https://raw.githubusercontent.com/coder/coder-k8s/main/examples/argocd/applicationset.yaml
kubectl -n argocd wait --for=create application/coder-k8s-stack --timeout=120s
```

The example already sets the `CreateNamespace=true` and `ServerSideApply=true` sync options.

## 3. Wait for Synced and Healthy

```bash
kubectl -n argocd wait --for=jsonpath='{.status.sync.status}'=Synced application/coder-k8s-stack --timeout=20m
kubectl -n argocd wait --for=jsonpath='{.status.health.status}'=Healthy application/coder-k8s-stack --timeout=20m
```

## 4. Check the operator and aggregated API

```bash
kubectl -n coder-system get deploy,pods
kubectl get apiservice v1alpha1.aggregation.coder.com \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} reason={.reason} message={.message}{"\n"}{end}'
```

The APIService should report `Available=True`.

## 5. Check the database and Coder

```bash
kubectl -n coder wait --for=condition=Ready cluster/coder-db --timeout=10m
kubectl -n coder get secret coder-db-app
kubectl -n coder rollout status deployment/coder --timeout=10m
kubectl -n coder get codercontrolplane coder -o yaml
```

## 6. Create the first admin user

Port-forward Coder in a separate terminal and keep it running:

```bash
kubectl -n coder port-forward svc/coder 3000:80
```

Open <http://127.0.0.1:3000/setup>, create the admin user, and confirm the templates page loads.

## 7. Push a template (optional)

```bash
export CODER_URL=http://127.0.0.1:3000
export CODER_SESSION_TOKEN=$(curl -sS -X POST "$CODER_URL/api/v2/users/login" \
  -H 'Content-Type: application/json' \
  -d '{"email":"<admin-email>","password":"<admin-password>"}' | jq -r '.session_token')

coder templates init --id scratch /tmp/coder-template-scratch
coder templates push starter-scratch --directory /tmp/coder-template-scratch --yes
```

## 8. See the template through `kubectl` (optional)

```bash
kubectl get codertemplates.aggregation.coder.com -A
kubectl get coderworkspaces.aggregation.coder.com -A
```

`starter-scratch` appears as `<organization>.starter-scratch`. Inspect it:

```bash
TEMPLATE=$(kubectl get codertemplates.aggregation.coder.com -A \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep starter-scratch | head -n1)
kubectl -n coder get codertemplates.aggregation.coder.com "$TEMPLATE" -o yaml
```

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| Sync error `metadata.annotations: Too long` on CNPG CRDs | Make sure `ServerSideApply=true` is in `spec.template.spec.syncPolicy.syncOptions` before applying the `ApplicationSet`. |
| CloudNativePG pod crashes with `no matches for kind "Pooler"` | The CRD apply failed. Fix the sync options and re-sync. |
| APIService `v1alpha1.aggregation.coder.com` is not Available | Check the `coder-k8s` logs and that Service `coder-k8s-apiserver` exists in `coder-system`. |
| Coder Deployment does not roll out | Check that `coder-db` is Ready and Secret `coder-db-app` exists. |

## Clean up

```bash
kubectl -n argocd delete applicationset coder-k8s-stack
```

The generated `Application` has the resources finalizer, so its managed resources are deleted too.
