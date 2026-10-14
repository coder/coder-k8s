package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderWorkspacePoolConditionReady is True when at least spec.replicas
	// current members are ready, no outdated unclaimed member remains, and the
	// controller observed the latest generation.
	CoderWorkspacePoolConditionReady = "Ready"
	// CoderWorkspacePoolConditionReconciling is True while the controller
	// works toward the spec.
	CoderWorkspacePoolConditionReconciling = "Reconciling"
	// CoderWorkspacePoolConditionStalled is True when the controller stops
	// until the spec or an input changes, for example after too many member
	// failures in a row.
	CoderWorkspacePoolConditionStalled = "Stalled"

	// CoderWorkspacePoolStrategyManaged keeps the pool's workspaces with the
	// operator: it creates them for the pool owner, waits until they are
	// ready, and hands them out through claims.
	CoderWorkspacePoolStrategyManaged = "Managed"

	// CoderWorkspacePoolClaimModeReserve binds a ready workspace to a claim.
	// The consumer connects with credentials it already holds for the pool
	// owner.
	CoderWorkspacePoolClaimModeReserve = "Reserve"

	// CoderWorkspacePoolSelectorLabel is the label key of the pool's
	// status.selector: <key>=<pool UID>.
	CoderWorkspacePoolSelectorLabel = "coder.com/workspace-pool-uid"
)

// WorkspacePoolsSpec configures CoderWorkspacePool objects that use a
// CoderControlPlane (CoderControlPlane.spec.workspacePools).
type WorkspacePoolsSpec struct {
	// Owners are the Coder users that pools on this control plane can use as
	// owners, each with the templates it can build and a workspace cap. They
	// are the admin's consent that pools make Coder build these templates as
	// this owner. Every start build gives the owner's API token to the
	// template's Terraform. A pool whose owner or template is not listed
	// creates no workspace.
	//
	// This field has no effect yet: the CoderWorkspacePool controller is not
	// shipped.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=userID
	// +optional
	Owners []WorkspacePoolOwner `json:"owners,omitempty"`
}

// WorkspacePoolOwner allows one Coder user as the owner of pool workspaces.
type WorkspacePoolOwner struct {
	// UserID is the UUID of the Coder user, in lower case with hyphens, as
	// Coder writes it. One user has one spelling, so the list holds each
	// owner once. Coder's prebuilds system user is refused: Coder's prebuild
	// reconciler can delete or hand out the workspaces it owns.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self != 'c42fdf75-3097-471c-8c33-fb52454d81c0'",message="userID must not be the Coder prebuilds system user"
	UserID string `json:"userID"`
	// Templates are the only Coder templates, as `<organization>.<template>`,
	// that pools of this owner can build.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=65
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*\.[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self.all(t, t.matches('^[^.]{1,32}[.][^.]{1,32}$'))",message="organization and template names must each have at most 32 characters"
	// +kubebuilder:validation:XValidation:rule="self.all(t, !t.matches('^(new|create)[.]') && !t.matches('[.](new|create)$'))",message="organization and template names must not be new or create"
	// +listType=set
	Templates []string `json:"templates"`
	// MaxWorkspaces caps the workspaces of this owner across all pools of
	// this control plane: unclaimed, bound, and used.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxWorkspaces *int32 `json:"maxWorkspaces,omitempty"`
}

// CoderWorkspacePoolVersion selects the template version of new members. Set
// exactly one field.
// +kubebuilder:validation:XValidation:rule="(has(self.name) ? 1 : 0) + (has(self.id) ? 1 : 0) + (has(self.active) ? 1 : 0) == 1",message="set exactly one of name, id, or active"
// +kubebuilder:validation:XValidation:rule="!has(self.active) || self.active",message="active must be true when set"
type CoderWorkspacePoolVersion struct {
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
	// Active follows the template's active version. After a promotion, the
	// controller replaces unclaimed members, new before old.
	// +optional
	Active *bool `json:"active,omitempty"`
}

// CoderWorkspacePoolParameter is one rich parameter value for the start
// builds. Values are stored in plain text, so never put secrets here.
type CoderWorkspacePoolParameter struct {
	// Name is the parameter name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// Value is the parameter value.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Value string `json:"value,omitempty"`
}

// CoderWorkspacePoolClaims configures how the pool hands out workspaces.
type CoderWorkspacePoolClaims struct {
	// Mode is how a claim gets its workspace. Reserve binds a ready workspace
	// to the claim, and the workspace stays with the pool owner.
	// +kubebuilder:validation:Enum=Reserve
	// +kubebuilder:default=Reserve
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="claims.mode is immutable"
	// +optional
	Mode string `json:"mode,omitempty"`
}

// CoderWorkspacePoolSpec defines a pool of ready Coder workspaces from one
// template for one Coder owner.
type CoderWorkspacePoolSpec struct {
	// ControlPlaneRef names the CoderControlPlane in the same namespace. The
	// controller calls Coder with that control plane's operator token.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="controlPlaneRef is immutable"
	ControlPlaneRef CoderControlPlaneReference `json:"controlPlaneRef"`
	// Strategy is how the pool keeps its workspaces. Managed is the only
	// strategy: the operator creates the workspaces itself.
	// +kubebuilder:validation:Enum=Managed
	// +kubebuilder:default=Managed
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="strategy is immutable"
	// +optional
	Strategy string `json:"strategy,omitempty"`
	// OwnerUserID is the UUID of the Coder user who owns every workspace of
	// the pool. The control plane's admin must allow this owner and the
	// template. Every start build gives the owner's API token to the
	// template's Terraform, so use a dedicated low-privilege user. Write the
	// UUID in lower case with hyphens, as in the control plane's owners list.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ownerUserID is immutable"
	// +kubebuilder:validation:XValidation:rule="self != 'c42fdf75-3097-471c-8c33-fb52454d81c0'",message="ownerUserID must not be the Coder prebuilds system user"
	OwnerUserID string `json:"ownerUserID"`
	// Template is the Coder template as `<organization>.<template>`, the same
	// format as aggregated CoderTemplate names. Each name has at most 32
	// characters and is not one of Coder's reserved names new or create.
	// +kubebuilder:validation:MaxLength=65
	// +kubebuilder:validation:XValidation:rule="self.matches('^[^.]{1,32}[.][^.]{1,32}$')",message="organization and template names must each have at most 32 characters"
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(new|create)[.]') && !self.matches('[.](new|create)$')",message="organization and template names must not be new or create"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="template is immutable"
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*\.[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`
	Template string `json:"template"`
	// Version selects the template version of new members. A change replaces
	// the unclaimed members, new before old.
	Version CoderWorkspacePoolVersion `json:"version"`
	// PresetName is the name of a preset of the selected version. The start
	// builds use its parameter values.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	PresetName string `json:"presetName,omitempty"`
	// Parameters are rich parameter values for the start builds.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Parameters []CoderWorkspacePoolParameter `json:"parameters,omitempty"`
	// Replicas is the number of unclaimed ready workspaces to keep. Waiting
	// claims get a workspace built on demand on top of this number. The scale
	// subresource writes this field, so kubectl scale, HPA, and KEDA can
	// resize the pool within the same bounds.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// MaxInFlight is the number of members of this pool that build at the
	// same time.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +optional
	MaxInFlight *int32 `json:"maxInFlight,omitempty"`
	// ReadyTimeoutSeconds bounds the time from a member's creation until its
	// workspace is ready. A member that is not ready in time fails, and the
	// controller replaces it.
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=7200
	// +optional
	ReadyTimeoutSeconds *int32 `json:"readyTimeoutSeconds,omitempty"`
	// Claims configures how the pool hands out workspaces.
	// +kubebuilder:default={}
	// +optional
	Claims CoderWorkspacePoolClaims `json:"claims,omitempty"`
}

// CoderWorkspacePoolResolved holds the inputs that the controller resolved
// in Coder. New members pin these values.
type CoderWorkspacePoolResolved struct {
	// OrganizationID is the UUID of the template's organization.
	// +optional
	OrganizationID string `json:"organizationID,omitempty"`
	// TemplateID is the UUID of the template.
	// +optional
	TemplateID string `json:"templateID,omitempty"`
	// TemplateVersionID is the UUID of the selected version.
	// +optional
	TemplateVersionID string `json:"templateVersionID,omitempty"`
	// TemplateVersionName is the name of the selected version.
	// +optional
	TemplateVersionName string `json:"templateVersionName,omitempty"`
	// PresetID is the UUID of the preset named by spec.presetName.
	// +optional
	PresetID string `json:"presetID,omitempty"`
	// OwnerID is the UUID of the owner the controller checked.
	// +optional
	OwnerID string `json:"ownerID,omitempty"`
	// ParametersHash identifies spec.parameters, so the controller can tell
	// current members from outdated ones without storing values twice.
	// +optional
	ParametersHash string `json:"parametersHash,omitempty"`
	// ResolvedTime is when the controller last changed these values.
	// +optional
	ResolvedTime *metav1.Time `json:"resolvedTime,omitempty"`
}

// CoderWorkspacePoolFailure is the newest member failure of a pool.
type CoderWorkspacePoolFailure struct {
	// Member is the name of the failed CoderWorkspacePoolMember.
	// +optional
	Member string `json:"member,omitempty"`
	// Reason is the member's failure reason.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message explains Reason. It never holds Coder error details, build
	// logs, parameter values, or tokens.
	// +optional
	Message string `json:"message,omitempty"`
	// Time is when the controller recorded the failure.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
}

// CoderWorkspacePoolStatus defines the observed state of a CoderWorkspacePool.
type CoderWorkspacePoolStatus struct {
	// ObservedGeneration is the generation the controller last processed.
	// Every scale change bumps metadata.generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Replicas is the number of unclaimed members that count toward
	// spec.replicas: current and outdated, not failed, not used, not being
	// deleted. The scale subresource reads this field.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// ReadyReplicas is the number of those members whose workspace is ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// UpdatedReplicas is the number of those members that use the resolved
	// inputs.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
	// BoundReplicas is the number of members that claims hold.
	// +optional
	BoundReplicas int32 `json:"boundReplicas,omitempty"`
	// UsedReplicas is the number of members that someone used without a
	// claim. They no longer count toward the pool, and the controller never
	// deletes their workspaces.
	// +optional
	UsedReplicas int32 `json:"usedReplicas,omitempty"`
	// PendingClaims is the number of claims that wait for a workspace.
	// +optional
	PendingClaims int32 `json:"pendingClaims,omitempty"`
	// Selector is the label selector of the scale subresource, in string
	// form: coder.com/workspace-pool-uid=<pool UID>. It matches no pods
	// unless a template sets that label, so autoscalers must use External or
	// Object metrics with target type AverageValue.
	// +optional
	Selector string `json:"selector,omitempty"`
	// Resolved holds the inputs that the controller resolved in Coder.
	// +optional
	Resolved *CoderWorkspacePoolResolved `json:"resolved,omitempty"`
	// ConsecutiveFailures is the number of member failures in a row: the
	// length of RunFailures. A ready member, a spec change, and a new
	// version reset it.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
	// LastFailure is the newest member failure.
	// +optional
	LastFailure *CoderWorkspacePoolFailure `json:"lastFailure,omitempty"`
	// FailureRunStart is the start of the current run of failures: the
	// latest of the last spec or version change and the newest ready time
	// of a member. Failures at or before it do not count.
	// +optional
	FailureRunStart *metav1.Time `json:"failureRunStart,omitempty"`
	// RunFailures are the member failures of the current run, oldest first,
	// at most 5. The controller stores each one before it deletes the
	// failed member, so it counts every failure once.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=5
	// +optional
	RunFailures []CoderWorkspacePoolFailure `json:"runFailures,omitempty"`
	// Reason is the current wait reason.
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
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.boundReplicas`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.resolved.templateVersionName`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CoderWorkspacePool keeps a number of ready Coder workspaces from one
// template for one Coder owner, and hands them out through claims (#153).
// The scale subresource lets kubectl scale, HPA, and KEDA resize it.
type CoderWorkspacePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderWorkspacePoolSpec   `json:"spec"`
	Status CoderWorkspacePoolStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderWorkspacePoolList contains a list of CoderWorkspacePool objects.
type CoderWorkspacePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderWorkspacePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderWorkspacePool{}, &CoderWorkspacePoolList{})
}
