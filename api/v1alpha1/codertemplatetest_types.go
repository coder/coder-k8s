package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderTemplateTestPhasePending means the test waits for its inputs and has
	// not sent a create request.
	CoderTemplateTestPhasePending = "Pending"
	// CoderTemplateTestPhaseRunning means a workspace create request was sent or
	// the test workspace exists.
	CoderTemplateTestPhaseRunning = "Running"
	// CoderTemplateTestPhaseSucceeded means the start build succeeded, every
	// top-level agent became ready, and the delete build succeeded. It is final.
	CoderTemplateTestPhaseSucceeded = "Succeeded"
	// CoderTemplateTestPhaseFailed means the test failed. It is final.
	CoderTemplateTestPhaseFailed = "Failed"

	// CoderTemplateTestConditionReady is True when the phase is Succeeded.
	CoderTemplateTestConditionReady = "Ready"
	// CoderTemplateTestConditionReconciling is True while the phase is Pending or Running.
	CoderTemplateTestConditionReconciling = "Reconciling"
	// CoderTemplateTestConditionStalled is True when the phase is Failed.
	CoderTemplateTestConditionStalled = "Stalled"
	// CoderTemplateTestConditionWorkspaceDeleted reports whether a workspace of
	// this test can still exist in Coder. Unknown means the controller cannot
	// prove it either way.
	CoderTemplateTestConditionWorkspaceDeleted = "WorkspaceDeleted"

	// CoderTemplateTestCleanupFinalizer keeps a test until its workspace is
	// deleted in Coder.
	CoderTemplateTestCleanupFinalizer = "coder.com/template-test-cleanup"
)

// CoderTemplateTestVersion selects the template version under test. Set
// exactly one field.
// +kubebuilder:validation:XValidation:rule="(has(self.name) ? 1 : 0) + (has(self.id) ? 1 : 0) + (has(self.active) ? 1 : 0) == 1",message="set exactly one of name, id, or active"
// +kubebuilder:validation:XValidation:rule="!has(self.active) || self.active",message="active must be true when set"
type CoderTemplateTestVersion struct {
	// Name is the name of a version of the template. It follows Coder's
	// template version name rules.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]+([_.-][a-zA-Z0-9]+)*$`
	// +optional
	Name string `json:"name,omitempty"`
	// ID is the UUID of a version of the template.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	// +optional
	ID string `json:"id,omitempty"`
	// Active selects the template's active version when the test starts. The
	// controller resolves it once and records the version, so a later
	// promotion does not change the version under test.
	// +optional
	Active *bool `json:"active,omitempty"`
}

// CoderTemplateTestParameter is one rich parameter value for the start build.
// Values are stored in plain text, so never put secrets here.
type CoderTemplateTestParameter struct {
	// Name is the parameter name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// Value is the parameter value.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Value string `json:"value,omitempty"`
}

// CoderControlPlaneReference names a CoderControlPlane in the same namespace.
type CoderControlPlaneReference struct {
	// Name is the CoderControlPlane name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// CoderTemplateTestSpec defines one test run of a Coder template version.
type CoderTemplateTestSpec struct {
	// ControlPlaneRef names the CoderControlPlane in the same namespace. The
	// controller calls Coder with that control plane's operator token.
	ControlPlaneRef CoderControlPlaneReference `json:"controlPlaneRef"`
	// Template is the Coder template as `<organization>.<template>`, the same
	// format as aggregated CoderTemplate names. Each name has at most 32
	// characters and is not one of Coder's reserved names new or create.
	// +kubebuilder:validation:MaxLength=65
	// +kubebuilder:validation:XValidation:rule="self.matches('^[^.]{1,32}[.][^.]{1,32}$')",message="organization and template names must each have at most 32 characters"
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(new|create)[.]') && !self.matches('[.](new|create)$')",message="organization and template names must not be new or create"
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*\.[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`
	Template string `json:"template"`
	// Version selects the template version under test.
	Version CoderTemplateTestVersion `json:"version"`
	// Parameters are rich parameter values for the start build.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Parameters []CoderTemplateTestParameter `json:"parameters,omitempty"`
	// TimeoutSeconds bounds the whole run: waiting for inputs, the start
	// build, agent readiness, and the delete build. Cleanup continues after
	// the deadline.
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=7200
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`
	// TTLSecondsAfterFinished deletes the test this long after it finished
	// and its workspace is gone, like Job. Do not set it under GitOps: the
	// GitOps tool recreates the deleted object, which runs the test again.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// CoderTemplateTestStatus defines the observed state of a CoderTemplateTest.
type CoderTemplateTestStatus struct {
	// ObservedGeneration is the generation the controller last processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Pending, Running, Succeeded, or Failed. Succeeded and Failed
	// are final.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// Reason is the current wait reason or the final result.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message explains Reason. It never holds Coder error details, build
	// logs, parameter values, or tokens.
	// +optional
	Message string `json:"message,omitempty"`
	// StartTime is when the controller first processed the test.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is when the phase became final.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// OrganizationID is the resolved Coder organization ID.
	// +optional
	OrganizationID string `json:"organizationID,omitempty"`
	// TemplateID is the resolved Coder template ID.
	// +optional
	TemplateID string `json:"templateID,omitempty"`
	// TemplateVersionID is the pinned template version ID.
	// +optional
	TemplateVersionID string `json:"templateVersionID,omitempty"`
	// TemplateVersionName is the name of the pinned template version.
	// +optional
	TemplateVersionName string `json:"templateVersionName,omitempty"`
	// OwnerID is the Coder user that owns the test workspace.
	// +optional
	OwnerID string `json:"ownerID,omitempty"`

	// WorkspaceName is the deterministic Coder workspace name of this test.
	// +optional
	WorkspaceName string `json:"workspaceName,omitempty"`
	// CreateAttemptTime is set before every workspace create request. While it
	// is set and WorkspaceID is empty, a create request can be in flight or
	// already committed in Coder.
	// +optional
	CreateAttemptTime *metav1.Time `json:"createAttemptTime,omitempty"`
	// WorkspaceID is the Coder workspace ID.
	// +optional
	WorkspaceID string `json:"workspaceID,omitempty"`
	// StartBuildID is the ID of the start build that the controller requested.
	// +optional
	StartBuildID string `json:"startBuildID,omitempty"`
	// DeleteBuildID is the ID of the current delete build.
	// +optional
	DeleteBuildID string `json:"deleteBuildID,omitempty"`
	// AgentsReadyTime is when every top-level agent was ready.
	// +optional
	AgentsReadyTime *metav1.Time `json:"agentsReadyTime,omitempty"`
	// DeleteAttempts counts failed delete builds. It drives the retry backoff.
	// +optional
	DeleteAttempts int32 `json:"deleteAttempts,omitempty"`

	// Conditions are Ready, Reconciling, Stalled, and WorkspaceDeleted.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.template`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.templateVersionName`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CoderTemplateTest tests one Coder template version like a Job: the
// controller creates a throwaway workspace, waits until every top-level agent
// is ready, deletes the workspace, and records Succeeded or Failed. The spec
// is immutable, so one object is one run.
type CoderTemplateTest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable, create a new CoderTemplateTest"
	Spec   CoderTemplateTestSpec   `json:"spec"`
	Status CoderTemplateTestStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderTemplateTestList contains a list of CoderTemplateTest objects.
type CoderTemplateTestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderTemplateTest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderTemplateTest{}, &CoderTemplateTestList{})
}
