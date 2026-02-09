package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CoderControlPlaneSpec defines the desired state of a CoderControlPlane.
type CoderControlPlaneSpec struct {
	// Image is the placeholder container image for the control plane deployment.
	Image string `json:"image,omitempty"`
}

// CoderControlPlaneStatus defines the observed state of a CoderControlPlane.
type CoderControlPlaneStatus struct {
	// Phase is a placeholder status field for future reconciliation stages.
	Phase string `json:"phase,omitempty"`
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
