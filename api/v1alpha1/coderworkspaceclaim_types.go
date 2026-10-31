package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderWorkspaceClaimPhasePending means no member holds the claim yet.
	CoderWorkspaceClaimPhasePending = "Pending"
	// CoderWorkspaceClaimPhaseBound means a member holds the claim.
	CoderWorkspaceClaimPhaseBound = "Bound"
	// CoderWorkspaceClaimPhaseLost means someone deleted the claim's
	// workspace, or the pool deleted it after its idle timeout. It is final:
	// a claim never binds again.
	CoderWorkspaceClaimPhaseLost = "Lost"

	// CoderWorkspaceClaimConditionReady is True when the claim is bound, its
	// workspace is ready, and, with spec.share, shared.
	CoderWorkspaceClaimConditionReady = "Ready"
	// CoderWorkspaceClaimConditionReconciling is True while the claim waits.
	CoderWorkspaceClaimConditionReconciling = "Reconciling"
	// CoderWorkspaceClaimConditionStalled is True when the claim cannot
	// progress until an input changes.
	CoderWorkspaceClaimConditionStalled = "Stalled"
)

// CoderWorkspaceClaimSpec names the pool that a claim takes a workspace
// from. It is immutable, so one claim is one workspace.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type CoderWorkspaceClaimSpec struct {
	// PoolName is the CoderWorkspacePool in the claim's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	PoolName string `json:"poolName"`
	// Share names the Coder user that gets the workspace. Required when the
	// pool's claims.mode is Share, refused otherwise: such a claim never binds
	// and is Stalled with ClaimModeMismatch.
	// +optional
	Share *CoderWorkspaceClaimShare `json:"share,omitempty"`
}

// CoderWorkspaceClaimShare names the Coder user of a Share claim.
type CoderWorkspaceClaimShare struct {
	// User is a Coder username or user UUID (lower case with hyphens).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^([a-zA-Z0-9]+(-[a-zA-Z0-9]+)*|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`
	// +kubebuilder:validation:XValidation:rule="self.size() <= 32 || self.matches('^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')",message="user must be a Coder username of at most 32 characters or a lower-case UUID"
	// +kubebuilder:validation:XValidation:rule="self != 'c42fdf75-3097-471c-8c33-fb52454d81c0'",message="user must not be the Coder prebuilds system user"
	User string `json:"user"`
}

// CoderWorkspaceClaimStatus defines the observed state of a
// CoderWorkspaceClaim.
type CoderWorkspaceClaimStatus struct {
	// Phase is Pending, Bound, or Lost. Lost is final.
	// +kubebuilder:validation:Enum=Pending;Bound;Lost
	// +optional
	Phase string `json:"phase,omitempty"`
	// MemberName is the CoderWorkspacePoolMember that holds the claim. While
	// the claim is Pending, memberName and workspaceName name the member that
	// the pool reserved for it.
	// +optional
	MemberName string `json:"memberName,omitempty"`
	// WorkspaceID is the Coder workspace ID.
	// +optional
	WorkspaceID string `json:"workspaceID,omitempty"`
	// WorkspaceName is the Coder workspace name.
	// +optional
	WorkspaceName string `json:"workspaceName,omitempty"`
	// OwnerName is the Coder username of the workspace owner, so that
	// `coder ssh <ownerName>/<workspaceName>` reaches the workspace.
	// +optional
	OwnerName string `json:"ownerName,omitempty"`
	// BoundTime is when a member took the claim.
	// +optional
	BoundTime *metav1.Time `json:"boundTime,omitempty"`
	// SharedUserID is the Coder user that has a role on the workspace
	// through this claim, as the member last read it from Coder. The
	// controller grants `use`.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self != 'c42fdf75-3097-471c-8c33-fb52454d81c0'",message="sharedUserID must not be the Coder prebuilds system user"
	// +optional
	SharedUserID string `json:"sharedUserID,omitempty"`
	// Reason is the current wait reason or the reason for Lost.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message explains Reason. It never holds Coder error details, build
	// logs, parameter values, or tokens.
	// +optional
	Message string `json:"message,omitempty"`
	// Conditions are Ready, Reconciling, and Stalled.
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
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.status.ownerName`
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.status.workspaceName`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CoderWorkspaceClaim takes one ready workspace from a CoderWorkspacePool
// (#153). Deleting the claim deletes its workspace.
type CoderWorkspaceClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderWorkspaceClaimSpec   `json:"spec"`
	Status CoderWorkspaceClaimStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderWorkspaceClaimList contains a list of CoderWorkspaceClaim objects.
type CoderWorkspaceClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderWorkspaceClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderWorkspaceClaim{}, &CoderWorkspaceClaimList{})
}
