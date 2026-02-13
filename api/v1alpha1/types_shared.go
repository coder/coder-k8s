package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

const (
	// DefaultTokenSecretKey is the default key used for proxy session tokens.
	DefaultTokenSecretKey = "token"
	// DefaultLicenseSecretKey is the default key used for Coder license JWTs.
	DefaultLicenseSecretKey = "license"
)

// ServiceSpec defines the Service configuration reconciled by the operator.
type ServiceSpec struct {
	// Type controls the Kubernetes service type.
	// +kubebuilder:default="ClusterIP"
	Type corev1.ServiceType `json:"type,omitempty"`
	// Port controls the exposed service port.
	// +kubebuilder:default=80
	Port int32 `json:"port,omitempty"`
	// Annotations are applied to the reconciled service object.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SecretKeySelector identifies a key in a Secret.
type SecretKeySelector struct {
	// Name is the Kubernetes Secret name.
	Name string `json:"name"`
	// Key is the key inside the Secret data map.
	Key string `json:"key,omitempty"`
}

// ServiceAccountSpec configures the ServiceAccount used by the Coder pod.
type ServiceAccountSpec struct {
	// DisableCreate skips ServiceAccount creation (use an existing SA).
	// +kubebuilder:default=false
	DisableCreate bool `json:"disableCreate,omitempty"`
	// Name overrides the ServiceAccount name. Defaults to the CoderControlPlane name.
	Name string `json:"name,omitempty"`
	// Annotations are applied to the managed ServiceAccount.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Labels are applied to the managed ServiceAccount.
	Labels map[string]string `json:"labels,omitempty"`
}

// RBACSpec configures namespace-scoped RBAC for workspace provisioning.
type RBACSpec struct {
	// WorkspacePerms enables Role/RoleBinding creation for workspace resources.
	// When omitted, the default is true.
	// +kubebuilder:default=true
	WorkspacePerms *bool `json:"workspacePerms,omitempty"`
	// EnableDeployments grants apps/deployments permissions (only when WorkspacePerms is true).
	// When omitted, the default is true.
	// +kubebuilder:default=true
	EnableDeployments *bool `json:"enableDeployments,omitempty"`
	// ExtraRules are appended to the managed Role rules.
	ExtraRules []rbacv1.PolicyRule `json:"extraRules,omitempty"`
	// WorkspaceNamespaces lists additional namespaces for Role/RoleBinding creation.
	WorkspaceNamespaces []string `json:"workspaceNamespaces,omitempty"`
}

// TLSSpec configures Coder built-in TLS.
type TLSSpec struct {
	// SecretNames lists TLS secrets to mount for built-in TLS.
	// When non-empty, TLS is enabled on the Coder control plane.
	SecretNames []string `json:"secretNames,omitempty"`
}

// ProbeSpec configures a Kubernetes probe with an enable toggle.
type ProbeSpec struct {
	// Enabled toggles the probe on or off.
	// When omitted, readiness defaults to enabled while liveness defaults to disabled.
	Enabled *bool `json:"enabled,omitempty"`
	// InitialDelaySeconds is the delay before the probe starts.
	// +kubebuilder:default=0
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	// PeriodSeconds controls how often the probe is performed.
	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`
	// TimeoutSeconds is the probe timeout.
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`
	// SuccessThreshold is the minimum consecutive successes for the probe to be considered successful.
	SuccessThreshold *int32 `json:"successThreshold,omitempty"`
	// FailureThreshold is the minimum consecutive failures for the probe to be considered failed.
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`
}

// ExposeSpec configures external exposure for the control plane.
// At most one of Ingress or Gateway may be set.
// +kubebuilder:validation:XValidation:rule="!(has(self.ingress) && has(self.gateway))",message="only one of ingress or gateway may be set"
type ExposeSpec struct {
	// Ingress configures a networking.k8s.io/v1 Ingress.
	// +optional
	Ingress *IngressExposeSpec `json:"ingress,omitempty"`
	// Gateway configures a gateway.networking.k8s.io/v1 HTTPRoute.
	// +optional
	Gateway *GatewayExposeSpec `json:"gateway,omitempty"`
}

// IngressExposeSpec defines Ingress exposure configuration.
type IngressExposeSpec struct {
	// ClassName is the Ingress class name.
	ClassName *string `json:"className,omitempty"`
	// Host is the primary hostname for the Ingress rule.
	Host string `json:"host"`
	// WildcardHost is an optional wildcard hostname (e.g., for workspace apps).
	WildcardHost string `json:"wildcardHost,omitempty"`
	// Annotations are applied to the managed Ingress.
	Annotations map[string]string `json:"annotations,omitempty"`
	// TLS configures TLS termination at the Ingress.
	// +optional
	TLS *IngressTLSExposeSpec `json:"tls,omitempty"`
}

// IngressTLSExposeSpec defines TLS configuration for the Ingress.
type IngressTLSExposeSpec struct {
	// SecretName is the TLS Secret for the primary host.
	SecretName string `json:"secretName,omitempty"`
	// WildcardSecretName is the TLS Secret for the wildcard host.
	WildcardSecretName string `json:"wildcardSecretName,omitempty"`
}

// GatewayExposeSpec defines Gateway API (HTTPRoute) exposure configuration.
type GatewayExposeSpec struct {
	// Host is the primary hostname for the HTTPRoute.
	Host string `json:"host"`
	// WildcardHost is an optional wildcard hostname.
	WildcardHost string `json:"wildcardHost,omitempty"`
	// ParentRefs are Gateways that the HTTPRoute attaches to.
	// At least one parentRef is required when gateway exposure is configured.
	// +kubebuilder:validation:MinItems=1
	ParentRefs []GatewayParentRef `json:"parentRefs"`
}

// GatewayParentRef identifies a Gateway for HTTPRoute attachment.
type GatewayParentRef struct {
	// Name is the Gateway name.
	Name string `json:"name"`
	// Namespace is the Gateway namespace.
	// +optional
	Namespace *string `json:"namespace,omitempty"`
	// SectionName is the listener name within the Gateway.
	// +optional
	SectionName *string `json:"sectionName,omitempty"`
}

// CertsSpec configures additional CA certificate mounts.
type CertsSpec struct {
	// Secrets lists Secret key selectors for CA certificates.
	// Each is mounted at `/etc/ssl/certs/{name}.crt`.
	Secrets []CertSecretSelector `json:"secrets,omitempty"`
}

// CertSecretSelector identifies a key within a Secret for CA cert mounting.
type CertSecretSelector struct {
	// Name is the Secret name.
	Name string `json:"name"`
	// Key is the key within the Secret data map.
	Key string `json:"key"`
}
