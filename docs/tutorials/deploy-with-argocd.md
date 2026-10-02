# Deploy with Argo CD

In this tutorial, you use one Argo CD `ApplicationSet` to deploy `coder-k8s` and a Coder instance that uses a CloudNativePG database.

Time: 20–30 minutes.

When you complete the tutorial, you have:

- The `coder-k8s` operator and the aggregated API (`aggregation.coder.com/v1alpha1`).
- The CloudNativePG operator and a PostgreSQL cluster with the name `coder-db`.
- A `CoderControlPlane` with the name `coder`, served by `svc/coder`.

## Prerequisites

- A Kubernetes cluster, and access to it with `kubectl` v1.26 or later.
- Argo CD, including the ApplicationSet controller, that runs in the namespace `argocd`.
- `jq` and `curl`.
- The `coder` CLI (only for the optional steps).

## 1. Check Argo CD

```bash
kubectl -n argocd get deploy,pods
```

Make sure that `argocd-application-controller`, `argocd-applicationset-controller`, `argocd-repo-server`, and `argocd-server` are running.

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

Make sure that the APIService shows `Available=True`.

## 5. Check the database and Coder

```bash
kubectl -n coder wait --for=condition=Ready cluster/coder-db --timeout=10m
kubectl -n coder get secret coder-db-app
kubectl -n coder rollout status deployment/coder --timeout=10m
kubectl -n coder get codercontrolplane coder -o yaml
```

## 6. Create the first admin user

1. In a different terminal, start a port-forward to Coder. Keep it running:

    ```bash
    kubectl -n coder port-forward svc/coder 3000:80
    ```

2. Open `http://127.0.0.1:3000/setup` and create the admin user.
3. Make sure that the templates page opens.

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

In Kubernetes, the template `starter-scratch` has the name `<organization>.starter-scratch`. To see all its fields:

```bash
TEMPLATE=$(kubectl get codertemplates.aggregation.coder.com -A \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep starter-scratch | head -n1)
kubectl -n coder get codertemplates.aggregation.coder.com "$TEMPLATE" -o yaml
```

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| The sync fails with `metadata.annotations: Too long` on the CloudNativePG CRDs | Make sure that `ServerSideApply=true` is in `spec.template.spec.syncPolicy.syncOptions` before you apply the `ApplicationSet`. |
| The CloudNativePG pod crashes with `no matches for kind "Pooler"` | The CRDs were not applied. Correct the sync options and sync again. |
| The APIService `v1alpha1.aggregation.coder.com` is not Available | Examine the `coder-k8s` logs. Make sure that the Service `coder-k8s-apiserver` exists in `coder-system`. |
| The Coder Deployment does not roll out | Make sure that `coder-db` is Ready and that the Secret `coder-db-app` exists. |

## Clean up

```bash
kubectl -n argocd delete applicationset coder-k8s-stack
```

The generated `Application` has the resources finalizer. Thus Argo CD also deletes the resources that the `Application` manages.
