package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CoderWorkspaceSpec defines the desired state of a CoderWorkspace.
type CoderWorkspaceSpec struct {
	// Running indicates whether the workspace should be running.
	Running bool `json:"running"`
}

// CoderWorkspaceStatus defines the observed state of a CoderWorkspace.
type CoderWorkspaceStatus struct {
	// AutoShutdown is the next planned shutdown time for the workspace.
	AutoShutdown *metav1.Time `json:"autoShutdown,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CoderWorkspace is the schema for Coder workspace resources.
type CoderWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderWorkspaceSpec   `json:"spec,omitempty"`
	Status CoderWorkspaceStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderWorkspaceList contains a list of CoderWorkspace objects.
type CoderWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderWorkspace `json:"items"`
}

// CoderTemplateSpec defines the desired state of a CoderTemplate.
type CoderTemplateSpec struct {
	// Running indicates whether the template should be marked as running.
	Running bool `json:"running"`
}

// CoderTemplateStatus defines the observed state of a CoderTemplate.
type CoderTemplateStatus struct {
	// AutoShutdown is the next planned shutdown time for workspaces created by this template.
	AutoShutdown *metav1.Time `json:"autoShutdown,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CoderTemplate is the schema for Coder template resources.
type CoderTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderTemplateSpec   `json:"spec,omitempty"`
	Status CoderTemplateStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderTemplateList contains a list of CoderTemplate objects.
type CoderTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderTemplate `json:"items"`
}
