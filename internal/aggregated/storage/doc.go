// Package storage implements codersdk-backed REST storage for the aggregated API
// server's CoderWorkspace, CoderTemplate and CoderTemplateVersion resources.
//
// v1 Semantics:
//   - Resources are namespace-scoped; the namespace represents the CoderControlPlane namespace.
//   - Template object names follow the format "<organization>.<template-name>".
//   - Workspace object names follow "<organization>.<user>.<workspace-name>".
//   - Template version object names follow "<organization>.<template-name>.<version-name>".
//     Version names can contain dots. The first two dots separate the segments.
//   - The dot separator works because organization, user, template and workspace names are
//     alphanumeric-with-hyphens (no dots),
//     while Kubernetes object names allow dots (DNS-1123 subdomains).
//   - A single admin session token is used for all API calls (no per-request impersonation in v1).
//   - Storage resolves the backing codersdk.Client via a ClientProvider interface.
//   - All-namespaces LIST aggregates results across eligible CoderControlPlane namespaces
//     when the provider implements NamespaceLister.
package storage
