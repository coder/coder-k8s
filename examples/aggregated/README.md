# Aggregated API examples

This directory contains example `CoderTemplate` resources for exercising the
aggregated API server (`aggregation.coder.com/v1alpha1`).

## Smoke template export

- [`codertemplate-smoke-scratch.yaml`](./codertemplate-smoke-scratch.yaml)
  is an exported template manifest captured from a live smoke-test cluster.
- It is intended as a reusable baseline for template create/update API testing.

Apply it with:

```bash
kubectl apply -f examples/aggregated/codertemplate-smoke-scratch.yaml
```

If you are updating an already-existing object with `kubectl replace`, include
the latest `metadata.resourceVersion` from `kubectl get -o yaml`.
