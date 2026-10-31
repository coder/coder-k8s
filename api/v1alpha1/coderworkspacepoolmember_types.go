package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderWorkspacePoolMemberPhasePending means the member has sent no
	// workspace create request, or a request had provably no effect.
	CoderWorkspacePoolMemberPhasePending = "Pending"
	// CoderWorkspacePoolMemberPhaseCreating means a create request was sent
	// and the workspace is not ready yet.
	CoderWorkspacePoolMemberPhaseCreating = "Creating"
	// CoderWorkspacePoolMemberPhaseReady means the start build succeeded and
	// every agent is ready.
	CoderWorkspacePoolMemberPhaseReady = "Ready"
	// CoderWorkspacePoolMemberPhaseFailed means the workspace failed. The
	// pool replaces the member.
	CoderWorkspacePoolMemberPhaseFailed = "Failed"
	// CoderWorkspacePoolMemberPhaseDeleting means the member deletes its
	// workspace.
	CoderWorkspacePoolMemberPhaseDeleting = "Deleting"

	// CoderWorkspacePoolMemberConditionReady is True when the workspace is ready.
	CoderWorkspacePoolMemberConditionReady = "Ready"
	// CoderWorkspacePoolMemberConditionUsed is True when someone may have
	// used the workspace without a claim: its last_used_at moved past the
	// value at ready. The pool then releases the member.
	CoderWorkspacePoolMemberConditionUsed = "Used"
	// CoderWorkspacePoolMemberConditionWorkspaceDeleted reports whether the
	// member's workspace is gone, with the CoderTemplateTest reasons.
	CoderWorkspacePoolMemberConditionWorkspaceDeleted = "WorkspaceDeleted"
	// CoderWorkspacePoolMemberConditionShared is True while Coder's ACL gives
	// status.sharedUserID a role on the workspace of a Share claim (use, or
	// one that a person set later) and the share policy allows it. Reason
	// ShareRefused: the policy or Coder refused the share. ShareRevoked: the
	// policy no longer allowed the share, and the controller removed it
	// (ShareRevoking until that worked).
	// ShareRemoved: someone else removed the share. The controller does not
	// share again after ShareRevoked or ShareRemoved.
	CoderWorkspacePoolMemberConditionShared = "Shared"

	// CoderWorkspacePoolMemberCleanupFinalizer keeps a member until its
	// workspace is deleted.
	CoderWorkspacePoolMemberCleanupFinalizer = "coder.com/workspace-pool-member-cleanup"
)

// CoderWorkspaceClaimReference names the CoderWorkspaceClaim that holds a
// member.
type CoderWorkspaceClaimReference struct {
	// Name is the claim name, in the member's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// UID is the claim's metadata.uid.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	UID string `json:"uid"`
}

// CoderWorkspacePoolMemberParameter is one pinned rich parameter value.
// Value is required and always serialized, so an empty value looks the same
// in a stored object and in a typed update, and the immutability rule holds.
type CoderWorkspacePoolMemberParameter struct {
	// Name is the parameter name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// Value is the parameter value. It can be empty.
	// +kubebuilder:validation:MaxLength=4096
	Value string `json:"value"`
}

// CoderWorkspacePoolMemberSpec holds the inputs that a member pinned from its
// pool's status.resolved. Only claimRef changes after creation, and only
// once. Every ID is a UUID in lower case with hyphens, as Coder writes it.
// +kubebuilder:validation:XValidation:rule="has(self.presetID) == has(oldSelf.presetID) && (!has(self.presetID) || self.presetID == oldSelf.presetID)",message="presetID is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.parameters) == has(oldSelf.parameters) && (!has(self.parameters) || self.parameters == oldSelf.parameters)",message="parameters are immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.claimRef) || (has(self.claimRef) && self.claimRef == oldSelf.claimRef)",message="claimRef can be set once"
type CoderWorkspacePoolMemberSpec struct {
	// ControlPlaneRef names the pool's CoderControlPlane.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="controlPlaneRef is immutable"
	ControlPlaneRef CoderControlPlaneReference `json:"controlPlaneRef"`
	// PoolName is the name of the member's CoderWorkspacePool.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="poolName is immutable"
	PoolName string `json:"poolName"`
	// OwnerID is the Coder user who owns the workspace. Coder's prebuilds
	// system user is refused.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ownerID is immutable"
	// +kubebuilder:validation:XValidation:rule="self != 'c42fdf75-3097-471c-8c33-fb52454d81c0'",message="ownerID must not be the Coder prebuilds system user"
	OwnerID string `json:"ownerID"`
	// OrganizationID is the template's organization.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="organizationID is immutable"
	OrganizationID string `json:"organizationID"`
	// TemplateID is the template.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="templateID is immutable"
	TemplateID string `json:"templateID"`
	// TemplateVersionID is the version of the start build.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="templateVersionID is immutable"
	TemplateVersionID string `json:"templateVersionID"`
	// PresetID is the preset of the start build.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +optional
	PresetID string `json:"presetID,omitempty"`
	// Parameters are the rich parameter values of the start build, in plain
	// text. Leave the field out when there are none: typed clients drop an
	// empty list, and the immutability rule would then refuse their updates.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Parameters []CoderWorkspacePoolMemberParameter `json:"parameters,omitempty"`
	// ParametersHash is the pool's hash of Parameters when it created the
	// member.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="parametersHash is immutable"
	ParametersHash string `json:"parametersHash"`
	// ReadyTimeoutSeconds bounds the time from the member's creation until
	// its workspace is ready.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=7200
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="readyTimeoutSeconds is immutable"
	ReadyTimeoutSeconds int32 `json:"readyTimeoutSeconds"`
	// ClaimRef is the claim that holds this member. The pool sets it once,
	// when it binds a ready member.
	// +optional
	ClaimRef *CoderWorkspaceClaimReference `json:"claimRef,omitempty"`
}

// CoderWorkspacePoolMemberStatus defines the observed state of a
// CoderWorkspacePoolMember.
type CoderWorkspacePoolMemberStatus struct {
	// Phase is Pending, Creating, Ready, Failed, or Deleting.
	// +kubebuilder:validation:Enum=Pending;Creating;Ready;Failed;Deleting
	// +optional
	Phase string `json:"phase,omitempty"`
	// WorkspaceName is the Coder workspace name: kwp- and the first 28 hex
	// characters of the member's UID.
	// +optional
	WorkspaceName string `json:"workspaceName,omitempty"`
	// CreateAttemptTime is set before every workspace create request. While
	// it is set and WorkspaceID is empty, a create request can be in flight
	// or already committed in Coder.
	// +optional
	CreateAttemptTime *metav1.Time `json:"createAttemptTime,omitempty"`
	// WorkspaceID is the Coder workspace ID.
	// +optional
	WorkspaceID string `json:"workspaceID,omitempty"`
	// StartBuildID is the ID of the start build that the controller requested.
	// +optional
	StartBuildID string `json:"startBuildID,omitempty"`
	// ReadyTime is when the workspace became ready.
	// +optional
	ReadyTime *metav1.Time `json:"readyTime,omitempty"`
	// ReadyLastUsedAt is the workspace's last_used_at when it became ready,
	// in microseconds like Coder. A later value counts as use. It comes from
	// use, or from a template schedule update with
	// update_workspace_last_used_at, which looks the same.
	// +optional
	ReadyLastUsedAt *metav1.MicroTime `json:"readyLastUsedAt,omitempty"`
	// LastExtendTime is when the controller last extended the autostop
	// deadline of the unclaimed workspace.
	// +optional
	LastExtendTime *metav1.Time `json:"lastExtendTime,omitempty"`
	// BindExtended is true after the one deadline extension at bind.
	// +optional
	BindExtended bool `json:"bindExtended,omitempty"`
	// BindExtendDeadline is the deadline that the extension at bind asks
	// Coder for. The controller stores it before the first extend request.
	// While BindExtended is false, a Coder deadline at or after it proves
	// that the extension happened.
	// +optional
	BindExtendDeadline *metav1.Time `json:"bindExtendDeadline,omitempty"`
	// SharedUserID is the Coder user that the controller shares the
	// workspace with for a Share claim. The controller stores it before its
	// first ACL write and never changes it, so a renamed or reused username
	// never moves the share to another user. The Shared condition reports
	// whether Coder's ACL gives this user a role.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self != 'c42fdf75-3097-471c-8c33-fb52454d81c0'",message="sharedUserID must not be the Coder prebuilds system user"
	// +optional
	SharedUserID string `json:"sharedUserID,omitempty"`
	// DeleteBuildID is the ID of the current delete build.
	// +optional
	DeleteBuildID string `json:"deleteBuildID,omitempty"`
	// DeleteAttempts counts failed delete builds. It drives the retry backoff.
	// +optional
	DeleteAttempts int32 `json:"deleteAttempts,omitempty"`
	// Reason is the current wait reason or the failure reason.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message explains Reason. It never holds Coder error details, build
	// logs, parameter values, or tokens.
	// +optional
	Message string `json:"message,omitempty"`
	// Conditions are Ready, Used, WorkspaceDeleted, and, for a Share claim,
	// Shared.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.status.workspaceName`
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.claimRef.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CoderWorkspacePoolMember is one workspace of a CoderWorkspacePool (#153).
// The pool controller creates members. Each member creates, watches, and
// deletes its own Coder workspace.
type CoderWorkspacePoolMember struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderWorkspacePoolMemberSpec   `json:"spec"`
	Status CoderWorkspacePoolMemberStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderWorkspacePoolMemberList contains a list of CoderWorkspacePoolMember
// objects.
type CoderWorkspacePoolMemberList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderWorkspacePoolMember `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderWorkspacePoolMember{}, &CoderWorkspacePoolMemberList{})
}
