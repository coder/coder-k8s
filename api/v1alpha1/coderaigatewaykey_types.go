package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderAIGatewayKeyConditionReady is True when the output Secret holds a
	// key that Coder lists.
	CoderAIGatewayKeyConditionReady = "Ready"
	// CoderAIGatewayKeyConditionReconciling is True while the controller
	// issues, rotates, or retires keys.
	CoderAIGatewayKeyConditionReconciling = "Reconciling"
	// CoderAIGatewayKeyConditionStalled is True when the controller creates no
	// key until the spec or an input changes.
	CoderAIGatewayKeyConditionStalled = "Stalled"

	// CoderAIGatewayKeyCleanupFinalizer keeps a CoderAIGatewayKey until every
	// Coder key that it created is deleted.
	CoderAIGatewayKeyCleanupFinalizer = "coder.com/ai-gateway-key-cleanup"

	// CoderAIGatewayKeySecretDataKey is the data key that holds the AI Gateway
	// key in the output Secret. It is the default of the Coder AI Gateway Helm
	// chart (aigateway.keySecret.key).
	CoderAIGatewayKeySecretDataKey = "key"
)

// CoderAIGatewayKeySpec defines a Coder AI Gateway key and its rotation.
type CoderAIGatewayKeySpec struct {
	// ControlPlaneRef names the CoderControlPlane in the same namespace. The
	// controller creates and deletes keys with that control plane's operator
	// token. The control plane must allow keys in spec.aiGateway.allowKeys.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="controlPlaneRef is immutable"
	ControlPlaneRef CoderControlPlaneReference `json:"controlPlaneRef"`
	// SecretName is the Secret in the same namespace that receives the key,
	// under the data key "key". The controller creates the Secret and owns it.
	// It never writes a Secret that already exists and that this object does
	// not own. To move the key to another Secret, delete and re-create this
	// object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="secretName is immutable"
	SecretName string `json:"secretName"`
	// Rotation configures when the controller replaces the key and how long a
	// replaced key stays valid.
	// +kubebuilder:default={}
	// +optional
	Rotation CoderAIGatewayKeyRotationSpec `json:"rotation"`
}

// CoderAIGatewayKeyRotationSpec configures key rotation.
// +kubebuilder:validation:XValidation:rule="!has(self.intervalSeconds) || !has(self.gracePeriodSeconds) || self.intervalSeconds == 0 || self.gracePeriodSeconds < self.intervalSeconds",message="gracePeriodSeconds must be shorter than intervalSeconds"
type CoderAIGatewayKeyRotationSpec struct {
	// IntervalSeconds is the time between scheduled rotations, counted from
	// the time the current key was written to the Secret. 0 turns scheduled
	// rotation off. Otherwise it is between 1 day and 365 days. The default is
	// 30 days, because Coder keys never expire.
	// +kubebuilder:default=2592000
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=31536000
	// +kubebuilder:validation:XValidation:rule="self == 0 || self >= 86400",message="intervalSeconds must be 0 or at least 86400 (1 day)"
	// +optional
	IntervalSeconds *int32 `json:"intervalSeconds,omitempty"`
	// GracePeriodSeconds is the minimum time between writing a new key to the
	// Secret and deleting the replaced key in Coder. Consumers must reload
	// within it. Between 10 minutes and 7 days, default 1 hour.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=600
	// +kubebuilder:validation:Maximum=604800
	// +optional
	GracePeriodSeconds *int32 `json:"gracePeriodSeconds,omitempty"`
	// ConsumerWaitSeconds is how long after the grace period the controller
	// keeps a replaced key while Coder still reports connections that use it.
	// After that time the controller deletes the key, and gateways that still
	// use it exit and restart with the key from the Secret. 0 deletes the key
	// when the grace period ends. At most 7 days, default 1 day.
	// +kubebuilder:default=86400
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=604800
	// +optional
	ConsumerWaitSeconds *int32 `json:"consumerWaitSeconds,omitempty"`
	// Trigger starts one rotation each time it changes to a new non-empty
	// value. The value set when the object is created starts no rotation.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Trigger string `json:"trigger,omitempty"`
}

// CoderAIGatewayKeyPending is the create marker. The controller writes it
// before every key create request. It holds no secret data.
type CoderAIGatewayKeyPending struct {
	// Serial is the key serial of the create request.
	// +kubebuilder:validation:Minimum=0
	Serial int64 `json:"serial"`
	// Name is the Coder key name of the create request.
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`
	// CreateAttemptTime is when the controller wrote this marker.
	CreateAttemptTime metav1.Time `json:"createAttemptTime"`
}

// CoderAIGatewayKeyCurrent records the key that the output Secret holds. It
// holds no secret data and nothing derived from it.
type CoderAIGatewayKeyCurrent struct {
	// Serial is the key serial.
	// +kubebuilder:validation:Minimum=0
	Serial int64 `json:"serial"`
	// ID is the Coder key ID. It is not the key.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	ID string `json:"id"`
	// Name is the Coder key name.
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`
	// IssuedAt is when the controller wrote the key to the Secret.
	IssuedAt metav1.Time `json:"issuedAt"`
}

// CoderAIGatewayKeyRetiring records a replaced key that waits for deletion in
// Coder. It holds no secret data and nothing derived from it.
type CoderAIGatewayKeyRetiring struct {
	// Serial is the key serial.
	// +kubebuilder:validation:Minimum=0
	Serial int64 `json:"serial"`
	// ID is the Coder key ID. It is not the key.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	ID string `json:"id"`
	// Name is the Coder key name.
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`
	// ReplacedAt is when a newer key replaced this key in the Secret. The
	// grace period counts from this time.
	ReplacedAt metav1.Time `json:"replacedAt"`
}

// CoderAIGatewayKeyStatus defines the observed state of a CoderAIGatewayKey.
// It holds Coder key IDs, names, serials, and times only. It never holds a
// key, a key prefix, a hash, or a masked key.
type CoderAIGatewayKeyStatus struct {
	// ObservedGeneration is the generation the controller last processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AppliedGeneration is the last generation that the controller fully
	// applied. A spec change re-arms a stall that waits for one.
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`
	// NextSerial is the serial of the next key create request. It never
	// decreases, so the controller never reuses a key name.
	// +kubebuilder:validation:Minimum=0
	// +optional
	NextSerial int64 `json:"nextSerial,omitempty"`
	// LastCreateAttemptTime is when the controller last sent a key create
	// request. Orphaned keys are deleted only after a settle window from this
	// time.
	// +optional
	LastCreateAttemptTime *metav1.Time `json:"lastCreateAttemptTime,omitempty"`
	// PendingKey is set before every key create request and cleared after it.
	// While it is set, a create request can be in flight or already committed
	// in Coder.
	// +optional
	PendingKey *CoderAIGatewayKeyPending `json:"pendingKey,omitempty"`
	// CurrentKey is the key that the output Secret holds.
	// +optional
	CurrentKey *CoderAIGatewayKeyCurrent `json:"currentKey,omitempty"`
	// RetiringKeys are replaced keys that wait for deletion in Coder.
	// +listType=map
	// +listMapKey=serial
	// +kubebuilder:validation:MaxItems=3
	// +optional
	RetiringKeys []CoderAIGatewayKeyRetiring `json:"retiringKeys,omitempty"`
	// LastHandledTrigger is the spec.rotation.trigger value that the
	// controller last acted on.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	LastHandledTrigger string `json:"lastHandledTrigger,omitempty"`
	// NextRotationTime is when the next scheduled rotation is due.
	// +optional
	NextRotationTime *metav1.Time `json:"nextRotationTime,omitempty"`
	// ConsecutiveIssueFailures counts create requests in a row whose outcome
	// was uncertain, and failed Secret writes. A success resets it.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ConsecutiveIssueFailures int32 `json:"consecutiveIssueFailures,omitempty"`
	// LastDriftReissueTime is when the controller last issued a key because
	// the Secret no longer held the current key.
	// +optional
	LastDriftReissueTime *metav1.Time `json:"lastDriftReissueTime,omitempty"`
	// Conditions are Ready, Reconciling, and Stalled.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=coderaigatewaykeys,scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.spec.secretName`
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.status.currentKey.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Next rotation",type=date,JSONPath=`.status.nextRotationTime`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CoderAIGatewayKey manages a Coder AI Gateway key for standalone AI Gateway
// replicas and stores it in a Secret that this object owns. A gateway key
// lets its holder load the API key of every enabled AI provider, so write
// access to this kind is admin-only. Deleting the object revokes every key
// that it created at once, and gateways that use them stop within a minute.
type CoderAIGatewayKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderAIGatewayKeySpec   `json:"spec"`
	Status CoderAIGatewayKeyStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderAIGatewayKeyList contains a list of CoderAIGatewayKey objects.
type CoderAIGatewayKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderAIGatewayKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderAIGatewayKey{}, &CoderAIGatewayKeyList{})
}
