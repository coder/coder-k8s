# `CoderWorkspace`

## API identity

- Group/version: `aggregation.coder.com/v1alpha1`
- Kind: `CoderWorkspace`
- Resource: `coderworkspaces`
- Scope: namespaced

## Spec

| Field | Type | Description |
| --- | --- | --- |
| `spec.running` | `bool` | Indicates whether the workspace should be running. |

## Status

| Field | Type | Description |
| --- | --- | --- |
| `status.autoShutdown` | `metav1.Time` | Next planned shutdown time for the workspace. |

## Source

- Go type: `api/aggregation/v1alpha1/types.go`
- Storage implementation: `internal/aggregated/storage/workspace.go`
- APIService registration manifest: `deploy/apiserver-apiservice.yaml`
