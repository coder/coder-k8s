package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=coderworkspaceproxies,scope=Namespaced
// +kubebuilder:subresource:status

// CoderWorkspaceProxy is the schema for Coder workspace proxy resources.
type CoderWorkspaceProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceProxySpec   `json:"spec,omitempty"`
	Status WorkspaceProxyStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderWorkspaceProxyList contains a list of CoderWorkspaceProxy objects.
type CoderWorkspaceProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderWorkspaceProxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderWorkspaceProxy{}, &CoderWorkspaceProxyList{})
}
