package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CoderWorkspaceSpec defines the desired state of a CoderWorkspace.
type CoderWorkspaceSpec struct {
	// Organization is the Coder organization name.
	Organization string `json:"organization,omitempty"`

	// TemplateName resolves via TemplateByName(organization, templateName).
	TemplateName string `json:"templateName,omitempty"`

	// TemplateVersionID optionally pins to a specific template version.
	TemplateVersionID string `json:"templateVersionID,omitempty"`

	// Running drives start/stop via CreateWorkspaceBuild.
	Running bool `json:"running"`

	TTLMillis         *int64  `json:"ttlMillis,omitempty"`
	AutostartSchedule *string `json:"autostartSchedule,omitempty"`
}

// CoderWorkspaceStatus defines the observed state of a CoderWorkspace.
type CoderWorkspaceStatus struct {
	ID               string `json:"id,omitempty"`
	OwnerName        string `json:"ownerName,omitempty"`
	OrganizationName string `json:"organizationName,omitempty"`
	TemplateName     string `json:"templateName,omitempty"`

	LatestBuildID     string `json:"latestBuildID,omitempty"`
	LatestBuildStatus string `json:"latestBuildStatus,omitempty"`

	AutoShutdown *metav1.Time `json:"autoShutdown,omitempty"`
	LastUsedAt   *metav1.Time `json:"lastUsedAt,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CoderWorkspace is the schema for Coder workspace resources.
// metadata.name is <organization>.<user>.<workspace-name>.
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
	// Organization is the Coder organization name (must match the organization prefix in metadata.name).
	Organization string `json:"organization"`

	// VersionID is the Coder template version UUID used on creation (required for CREATE).
	VersionID string `json:"versionID"`

	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`

	// Files is the template source tree for the active template version.
	//
	// Keys are slash-delimited relative paths (e.g. "main.tf").
	// Values are UTF-8 file contents.
	//
	// Populated on GET; intentionally omitted from LIST to keep responses small.
	// On CREATE/UPDATE with files, the server uploads source and creates a new template version.
	Files map[string]string `json:"files,omitempty"`

	// Running is a legacy flag retained temporarily for in-repo callers that still read template run-state directly.
	Running bool `json:"running,omitempty"`
}

// CoderTemplateStatus defines the observed state of a CoderTemplate.
type CoderTemplateStatus struct {
	ID               string       `json:"id,omitempty"`
	OrganizationName string       `json:"organizationName,omitempty"`
	ActiveVersionID  string       `json:"activeVersionID,omitempty"`
	Deprecated       bool         `json:"deprecated,omitempty"`
	UpdatedAt        *metav1.Time `json:"updatedAt,omitempty"`

	// AutoShutdown is a legacy timestamp retained temporarily for in-repo callers that still surface template shutdown timestamps.
	AutoShutdown *metav1.Time `json:"autoShutdown,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CoderTemplate is the schema for Coder template resources.
// metadata.name is <organization>.<template-name>.
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
