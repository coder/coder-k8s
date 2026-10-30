package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderAIProviderConditionReady is True when Coder holds the provider as
	// the spec describes.
	CoderAIProviderConditionReady = "Ready"
	// CoderAIProviderConditionReconciling is True while the controller works
	// toward the spec.
	CoderAIProviderConditionReconciling = "Reconciling"
	// CoderAIProviderConditionStalled is True when the controller stops until
	// the spec or an input changes.
	CoderAIProviderConditionStalled = "Stalled"

	// CoderAIProviderCleanupFinalizer keeps a provider until its Coder
	// provider is deleted.
	CoderAIProviderCleanupFinalizer = "coder.com/ai-provider-cleanup"
)

// CoderAIProviderSpec defines one AI provider in Coder.
type CoderAIProviderSpec struct {
	// ControlPlaneRef names the CoderControlPlane in the same namespace. The
	// controller calls Coder with that control plane's operator token.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="controlPlaneRef is immutable"
	ControlPlaneRef CoderControlPlaneReference `json:"controlPlaneRef"`
	// Type is the Coder provider type. Coder cannot change it after create.
	// +kubebuilder:validation:Enum=openai;anthropic;azure;google;openai-compat;openrouter;vercel
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	Type string `json:"type"`
	// BaseURL is the http:// or https:// URL that Coder sends model requests
	// to. It must not hold user info. The Secret in apiKeySecretRef must list
	// its origin as an allowed destination.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getHostname() != ''",message="baseURL must be a valid URL with a host"
	// +kubebuilder:validation:XValidation:rule="self.matches('^https?://[^/?#@\\\\s]+([/?#].*)?$')",message="baseURL must be an http:// or https:// URL with a host and no user info"
	BaseURL string `json:"baseURL"`
	// DisplayName is the name that Coder shows. When empty, Coder shows the
	// provider name.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Icon is the icon URL or path that Coder shows.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	Icon string `json:"icon,omitempty"`
	// Enabled turns the provider on or off in Coder. When omitted, the
	// default is true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// APIKeySecretRef references the key in a Secret in the same namespace
	// that holds the provider API key. The Secret must opt in to the
	// credential Secret contract.
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="apiKeySecretRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="has(self.key) && size(self.key) > 0",message="apiKeySecretRef.key must not be empty"
	APIKeySecretRef SecretKeySelector `json:"apiKeySecretRef"`
}

// CoderAIProviderCredentialStatus records which Secret value was last sent to
// Coder. It holds no secret data and nothing derived from it.
type CoderAIProviderCredentialStatus struct {
	// SecretName is the Secret name that was read.
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// SecretKey is the key in the Secret that was read.
	// +optional
	SecretKey string `json:"secretKey,omitempty"`
	// SecretUID is the UID of the Secret that was read.
	// +optional
	SecretUID string `json:"secretUID,omitempty"`
	// SecretResourceVersion is the resourceVersion of the Secret that was read.
	// +optional
	SecretResourceVersion string `json:"secretResourceVersion,omitempty"`
	// KeyID is the ID that Coder gave the key. It is not the key.
	// +optional
	KeyID string `json:"keyID,omitempty"`
	// ResendAttemptTime is set before every key resend. While it is set, a
	// resend can be in flight or already committed in Coder.
	// +optional
	ResendAttemptTime *metav1.Time `json:"resendAttemptTime,omitempty"`
}

// CoderAIProviderStatus defines the observed state of a CoderAIProvider.
type CoderAIProviderStatus struct {
	// ObservedGeneration is the generation the controller last processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AppliedGeneration is the last generation whose spec Coder fully holds.
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`
	// ProviderID is the Coder provider ID.
	// +optional
	ProviderID string `json:"providerID,omitempty"`
	// Enabled is the enabled flag of the Coder provider at the controller's
	// last read or update of it. It is empty while Coder holds no provider of
	// this object, or before the first read.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// CreateAttemptTime is set before every provider create request. While it
	// is set and ProviderID is empty, a create request can be in flight or
	// already committed in Coder.
	// +optional
	CreateAttemptTime *metav1.Time `json:"createAttemptTime,omitempty"`
	// Credential records the Secret value last sent to Coder.
	// +optional
	Credential *CoderAIProviderCredentialStatus `json:"credential,omitempty"`
	// Conditions are Ready, Reconciling, and Stalled.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=coderaiproviders,scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.status.enabled`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63 && self.metadata.name.matches('^[a-z0-9]+(-[a-z0-9]+)*$')",message="metadata.name is the Coder provider name: at most 63 lowercase letters, digits, and single hyphens between them"
// +kubebuilder:validation:XValidation:rule="!self.metadata.name.matches('^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') && !self.metadata.name.matches('^[0-9a-f]{32}$')",message="metadata.name must not be a UUID: Coder reads a UUID-shaped provider name as a provider ID"

// CoderAIProvider manages one AI provider in Coder. The object name is the
// Coder provider name. It cannot be a UUID, because Coder looks up a
// UUID-shaped name as a provider ID.
type CoderAIProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderAIProviderSpec   `json:"spec"`
	Status CoderAIProviderStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderAIProviderList contains a list of CoderAIProvider objects.
type CoderAIProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderAIProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderAIProvider{}, &CoderAIProviderList{})
}
