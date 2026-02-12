package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// WorkspaceProxyPhasePending indicates the proxy deployment is not ready.
	WorkspaceProxyPhasePending = "Pending"
	// WorkspaceProxyPhaseReady indicates at least one proxy pod is ready.
	WorkspaceProxyPhaseReady = "Ready"
)

// ProxyBootstrapSpec configures optional registration with the Coder API.
type ProxyBootstrapSpec struct {
	// CoderURL is the URL for the primary Coder control plane API.
	CoderURL string `json:"coderURL"`
	// CredentialsSecretRef points to a Secret containing a Coder session token.
	CredentialsSecretRef SecretKeySelector `json:"credentialsSecretRef"`
	// ProxyName is the name used when registering the proxy in Coder.
	ProxyName string `json:"proxyName,omitempty"`
	// DisplayName is the human-readable name for the proxy region.
	DisplayName string `json:"displayName,omitempty"`
	// Icon is the optional icon URL or emoji path for the proxy region.
	Icon string `json:"icon,omitempty"`
	// GeneratedTokenSecretName stores the generated proxy token.
	GeneratedTokenSecretName string `json:"generatedTokenSecretName,omitempty"`
}

// WorkspaceProxySpec defines the desired state of a WorkspaceProxy.
type WorkspaceProxySpec struct {
	// Image is the container image used for the workspace proxy pod.
	Image string `json:"image,omitempty"`
	// Replicas is the desired number of proxy pods.
	Replicas *int32 `json:"replicas,omitempty"`
	// Service controls the service created in front of the workspace proxy.
	// +kubebuilder:default={}
	Service ServiceSpec `json:"service,omitempty"`
	// PrimaryAccessURL is the coderd URL the proxy should connect to.
	PrimaryAccessURL string `json:"primaryAccessURL,omitempty"`
	// ProxySessionTokenSecretRef points to a Secret key containing the proxy token.
	ProxySessionTokenSecretRef *SecretKeySelector `json:"proxySessionTokenSecretRef,omitempty"`
	// Bootstrap optionally registers the proxy and mints a proxy token.
	Bootstrap *ProxyBootstrapSpec `json:"bootstrap,omitempty"`
	// DerpOnly configures the workspace proxy to only serve DERP traffic.
	DerpOnly bool `json:"derpOnly,omitempty"`
	// ExtraArgs are appended to the default workspace proxy arguments.
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// ExtraEnv are injected into the workspace proxy container.
	ExtraEnv []corev1.EnvVar `json:"extraEnv,omitempty"`
	// ImagePullSecrets are used by the pod to pull private images.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// WorkspaceProxyStatus defines the observed state of a WorkspaceProxy.
type WorkspaceProxyStatus struct {
	// ObservedGeneration tracks the spec generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ReadyReplicas is the number of ready pods observed in the deployment.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Registered reports whether bootstrap registration completed successfully.
	Registered bool `json:"registered,omitempty"`
	// ProxyTokenSecretRef is the Secret used for the proxy session token.
	ProxyTokenSecretRef *SecretKeySelector `json:"proxyTokenSecretRef,omitempty"`
	// Phase is a high-level readiness indicator.
	Phase string `json:"phase,omitempty"`
	// Conditions are Kubernetes-standard conditions for this resource.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
