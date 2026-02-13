# Helm Chart Parity Tracking

This document tracks the mapping between upstream `coder/coder` Helm chart
values and the `CoderControlPlane` CRD fields managed by this operator.

## Legend

| Status | Meaning |
|--------|---------|
| ✅ | Implemented in CRD |
| 🚧 | Planned / in progress |
| ❌ | Not planned / out of scope |

## Phase 1 — Production Readiness

| Helm Chart Value | CRD Field | Status | Notes |
|------------------|-----------|--------|-------|
| `coder.image.repo` / `coder.image.tag` | `spec.image` | ✅ | Combined as full image reference |
| `coder.replicaCount` | `spec.replicas` | ✅ | |
| `coder.env` | `spec.extraEnv` | ✅ | |
| `coder.service.type` | `spec.service.type` | ✅ | |
| `coder.service.httpNodePort` | `spec.service.port` | ✅ | Port only; nodePort inferred by Kubernetes |
| `coder.service.annotations` | `spec.service.annotations` | ✅ | |
| `coder.serviceAccount.create` | `spec.serviceAccount.disableCreate` | ✅ | Inverted sense |
| `coder.serviceAccount.name` | `spec.serviceAccount.name` | ✅ | |
| `coder.serviceAccount.annotations` | `spec.serviceAccount.annotations` | ✅ | |
| `coder.serviceAccount.labels` | `spec.serviceAccount.labels` | ✅ | |
| `coder.workspaceProxy` | — | ❌ | Workspace proxy mode not in scope |
| `coder.resources` | `spec.resources` | ✅ | |
| `coder.securityContext` | `spec.securityContext` | ✅ | Container-level |
| `coder.podSecurityContext` | `spec.podSecurityContext` | ✅ | Pod-level |
| `coder.tls.secretNames` | `spec.tls.secretNames` | ✅ | Enables Coder built-in TLS |
| `coder.readinessProbe` | `spec.readinessProbe` | ✅ | |
| `coder.livenessProbe` | `spec.livenessProbe` | ✅ | |
| `coder.env` (`CODER_ACCESS_URL`) | `spec.envUseClusterAccessURL` | ✅ | Auto-injects default in-cluster URL |
| `coder.rbac.createWorkspacePerms` | `spec.rbac.workspacePerms` | ✅ | |
| `coder.rbac.enableDeployments` | `spec.rbac.enableDeployments` | ✅ | |
| `coder.rbac.extraRules` | `spec.rbac.extraRules` | ✅ | |

## Phase 2 — Operability & HA

| Helm Chart Value | CRD Field | Status | Notes |
|------------------|-----------|--------|-------|
| `coder.envFrom` | `spec.envFrom` | ✅ | |
| `coder.volumes` | `spec.volumes` | ✅ | |
| `coder.volumeMounts` | `spec.volumeMounts` | ✅ | |
| `coder.certs.secrets` | `spec.certs.secrets` | ✅ | CA cert Secret selectors |
| `coder.nodeSelector` | `spec.nodeSelector` | ✅ | |
| `coder.tolerations` | `spec.tolerations` | ✅ | |
| `coder.affinity` | `spec.affinity` | ✅ | |
| `coder.topologySpreadConstraints` | `spec.topologySpreadConstraints` | ✅ | |
| `coder.ingress.*` | `spec.expose.ingress` | ✅ | Part of unified expose API |
| Gateway API | `spec.expose.gateway` | ✅ | HTTPRoute; Gateway CRDs optional |
| `coder.imagePullSecrets` | `spec.imagePullSecrets` | ✅ | |

## Not Planned

| Helm Chart Value | Reason |
|------------------|--------|
| `coder.workspaceProxy` | Workspace proxy mode is a separate concern |
| `coder.podDisruptionBudget` | Future enhancement |
| `coder.initContainers` | Future enhancement |
| `coder.command` | Not safe to override in operator mode |
| `provisionerDaemon.*` | Separate provisioner deployment (future) |
