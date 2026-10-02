# Helm Chart Parity Tracking

This page maps the values of the upstream `coder/coder` Helm chart to the `CoderControlPlane` CRD fields that this operator manages.

## Legend

| Status | Meaning |
|--------|---------|
| Implemented | The CRD has a field for this value. |
| Separate resource | A different `coder-k8s` resource covers this value, not a `CoderControlPlane` field. |
| Not planned | This value is out of scope. |

## Phase 1: production readiness

| Helm Chart Value | CRD Field | Status | Notes |
|------------------|-----------|--------|-------|
| `coder.image.repo` / `coder.image.tag` | `spec.image` | Implemented | One full image reference |
| `coder.replicaCount` | `spec.replicas` | Implemented | |
| `coder.env` | `spec.extraEnv` | Implemented | |
| `coder.service.type` | `spec.service.type` | Implemented | |
| `coder.service.httpNodePort` | `spec.service.port` | Implemented | Port only. Kubernetes chooses the node port. |
| `coder.service.annotations` | `spec.service.annotations` | Implemented | |
| `coder.serviceAccount.create` | `spec.serviceAccount.disableCreate` | Implemented | The opposite meaning |
| `coder.serviceAccount.name` | `spec.serviceAccount.name` | Implemented | |
| `coder.serviceAccount.annotations` | `spec.serviceAccount.annotations` | Implemented | |
| `coder.serviceAccount.labels` | `spec.serviceAccount.labels` | Implemented | |
| `coder.resources` | `spec.resources` | Implemented | |
| `coder.securityContext` | `spec.securityContext` | Implemented | Container level |
| `coder.podSecurityContext` | `spec.podSecurityContext` | Implemented | Pod level |
| `coder.tls.secretNames` | `spec.tls.secretNames` | Implemented | Turns on the built-in TLS of Coder |
| `coder.readinessProbe` | `spec.readinessProbe` | Implemented | |
| `coder.livenessProbe` | `spec.livenessProbe` | Implemented | |
| `coder.env` (`CODER_ACCESS_URL`) | `spec.envUseClusterAccessURL` | Implemented | Adds the default in-cluster URL automatically |
| `coder.rbac.createWorkspacePerms` | `spec.rbac.workspacePerms` | Implemented | |
| `coder.rbac.enableDeployments` | `spec.rbac.enableDeployments` | Implemented | |
| `coder.rbac.extraRules` | `spec.rbac.extraRules` | Implemented | |

## Phase 2: operability and high availability

| Helm Chart Value | CRD Field | Status | Notes |
|------------------|-----------|--------|-------|
| `coder.envFrom` | `spec.envFrom` | Implemented | |
| `coder.volumes` | `spec.volumes` | Implemented | |
| `coder.volumeMounts` | `spec.volumeMounts` | Implemented | |
| `coder.certs.secrets` | `spec.certs.secrets` | Implemented | Secret selectors for CA certificates |
| `coder.nodeSelector` | `spec.nodeSelector` | Implemented | |
| `coder.tolerations` | `spec.tolerations` | Implemented | |
| `coder.affinity` | `spec.affinity` | Implemented | |
| `coder.topologySpreadConstraints` | `spec.topologySpreadConstraints` | Implemented | |
| `coder.ingress.*` | `spec.expose.ingress` | Implemented | Part of the unified expose API |
| Gateway API | `spec.expose.gateway` | Implemented | HTTPRoute. The Gateway CRDs are optional. |
| `coder.imagePullSecrets` | `spec.imagePullSecrets` | Implemented | |

## Separate resources

| Helm Chart Value | Resource |
|------------------|----------|
| `coder.workspaceProxy` | `CoderWorkspaceProxy` runs `coder wsproxy server` in its own Deployment. |
| `provisionerDaemon.*` | `CoderProvisioner` runs external provisioner daemons in their own Deployment. |

## Not planned

| Helm Chart Value | Reason |
|------------------|--------|
| `coder.podDisruptionBudget` | Possible future change |
| `coder.initContainers` | Possible future change |
| `coder.command` | It is not safe to change the command in operator mode |
