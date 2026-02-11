package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderControlPlanePhasePending indicates the control plane has not reported ready yet.
	CoderControlPlanePhasePending = "Pending"
	// CoderControlPlanePhaseReady indicates at least one control plane pod is ready.
	CoderControlPlanePhaseReady = "Ready"
)

// CoderControlPlaneSpec defines the desired state of a CoderControlPlane.
type CoderControlPlaneSpec struct {
	// Image is the container image used for the Coder control plane pod.
	// +kubebuilder:default="ghcr.io/coder/coder:latest"
	Image string `json:"image,omitempty"`
	// Replicas is the desired number of control plane pods.
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`
	// Service controls the service created in front of the control plane.
	// +kubebuilder:default={}
	Service ServiceSpec `json:"service,omitempty"`
	// ExtraArgs are appended to the default Coder server arguments.
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// ExtraEnv are injected into the Coder control plane container.
	ExtraEnv []corev1.EnvVar `json:"extraEnv,omitempty"`
	// ImagePullSecrets are used by the pod to pull private images.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// OperatorAccess configures bootstrap API access to the coderd instance.
	// +kubebuilder:default={}
	OperatorAccess OperatorAccessSpec `json:"operatorAccess,omitempty"`
}

// OperatorAccessSpec configures the controller-managed coderd operator user.
type OperatorAccessSpec struct {
	// Disabled turns off creation and management of the `coder-k8s-operator`
	// user and API token.
	// +kubebuilder:default=false
	Disabled bool `json:"disabled,omitempty"`
	// GeneratedTokenSecretName stores the generated operator API token.
	GeneratedTokenSecretName string `json:"generatedTokenSecretName,omitempty"`
}

// CoderControlPlaneStatus defines the observed state of a CoderControlPlane.
type CoderControlPlaneStatus struct {
	// ObservedGeneration tracks the spec generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ReadyReplicas is the number of ready pods observed in the deployment.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// URL is the in-cluster URL for the control plane service.
	URL string `json:"url,omitempty"`
	// OperatorTokenSecretRef points to the Secret key containing the `coder-k8s-operator` API token.
	OperatorTokenSecretRef *SecretKeySelector `json:"operatorTokenSecretRef,omitempty"`
	// OperatorAccessReady reports whether operator API access bootstrap succeeded.
	OperatorAccessReady bool `json:"operatorAccessReady,omitempty"`
	// Phase is a high-level readiness indicator.
	Phase string `json:"phase,omitempty"`
	// Conditions are Kubernetes-standard conditions for this resource.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status

// CoderControlPlane is the schema for Coder control plane resources.
type CoderControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderControlPlaneSpec   `json:"spec,omitempty"`
	Status CoderControlPlaneStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderControlPlaneList contains a list of CoderControlPlane objects.
type CoderControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderControlPlane{}, &CoderControlPlaneList{})
}
