# `CoderTemplate`

## API identity

- Group/version: `aggregation.coder.com/v1alpha1`
- Kind: `CoderTemplate`
- Resource: `codertemplates`
- Scope: namespaced

## Spec

| Field | Type | Description |
| --- | --- | --- |
| `spec.running` | `bool` | Indicates whether the template should be marked as running. |

## Status

| Field | Type | Description |
| --- | --- | --- |
| `status.autoShutdown` | `metav1.Time` | Next planned shutdown time for workspaces created by this template. |

## Source

- Go type: `api/aggregation/v1alpha1/types.go`
- Storage implementation: `internal/aggregated/storage/template.go`
- APIService registration manifest: `deploy/apiserver-apiservice.yaml`
