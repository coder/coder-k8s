package v1alpha1

import corev1 "k8s.io/api/core/v1"

const (
	// DefaultTokenSecretKey is the default key used for proxy session tokens.
	DefaultTokenSecretKey = "token"
)

// ServiceSpec defines the Service configuration reconciled by the operator.
type ServiceSpec struct {
	// Type controls the Kubernetes service type.
	Type corev1.ServiceType `json:"type,omitempty"`
	// Port controls the exposed service port.
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
