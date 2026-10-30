package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderProvisionerPhasePending indicates the provisioner deployment is not ready.
	CoderProvisionerPhasePending = "Pending"
	// CoderProvisionerPhaseReady indicates at least one provisioner pod is ready.
	CoderProvisionerPhaseReady = "Ready"

	// CoderProvisionerConditionControlPlaneReady indicates whether the referenced control plane is reachable.
	CoderProvisionerConditionControlPlaneReady = "ControlPlaneReady"
	// CoderProvisionerConditionOperatorAccessReady indicates whether operator-managed
	// access from the referenced control plane is ready for provisioner key management.
	CoderProvisionerConditionOperatorAccessReady = "OperatorAccessReady"
	// CoderProvisionerConditionProvisionerKeyReady indicates whether the provisioner key exists in coderd.
	CoderProvisionerConditionProvisionerKeyReady = "ProvisionerKeyReady"
	// CoderProvisionerConditionProvisionerKeySecretReady indicates whether the provisioner key secret is populated.
	CoderProvisionerConditionProvisionerKeySecretReady = "ProvisionerKeySecretReady"
	// CoderProvisionerConditionExternalProvisionersEntitled indicates whether the
	// referenced Coder deployment is entitled to run external provisioner daemons.
	CoderProvisionerConditionExternalProvisionersEntitled = "ExternalProvisionersEntitled"
	// CoderProvisionerConditionDeploymentReady indicates whether the provisioner deployment has ready replicas.
	CoderProvisionerConditionDeploymentReady = "DeploymentReady"
	// CoderProvisionerConditionCredentialsReady indicates whether every
	// referenced credential Secret and setting passed the operator's checks.
	// When it is False, the operator keeps the last applied Deployment.
	CoderProvisionerConditionCredentialsReady = "CredentialsReady"
	// CoderProvisionerConditionUnverifiedKeyKept is True while
	// status.pendingKeys holds an entry without key IDs that the operator
	// could not remove: Coder holds a key with its name, which nothing shows
	// this provisioner created, or the operator could not check Coder. The
	// operator deletes no key for such an entry. The message names each
	// entry, and the operator removes the condition when none is left.
	CoderProvisionerConditionUnverifiedKeyKept = "UnverifiedKeyKept"
	// CoderProvisionerConditionControlPlaneChangedKeyKept is True while
	// status.pendingKeys holds an entry whose control plane has another UID
	// than the entry records, or an entry without a UID whose coderURL is not
	// the current Coder API URL of its control plane. The operator sends no
	// operator token for such an entry. The message names each entry, and the
	// operator removes the condition when none is left.
	CoderProvisionerConditionControlPlaneChangedKeyKept = "ControlPlaneChangedKeyKept"

	// DefaultProvisionerKeySecretKey is the default data key for provisioner key secrets.
	DefaultProvisionerKeySecretKey = "key"

	// ProvisionerKeyCleanupFinalizer is applied to ensure coderd key cleanup on deletion.
	ProvisionerKeyCleanupFinalizer = "coder.com/provisioner-key-cleanup"
)

// CoderProvisionerKeySpec configures provisioner key naming and storage.
type CoderProvisionerKeySpec struct {
	// Name is the provisioner key name in coderd. Defaults to the CR name.
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name,omitempty"`
	// SecretName is the Kubernetes Secret to store the key. Defaults to "{crName}-provisioner-key".
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName,omitempty"`
	// SecretKey is the data key in the Secret. Defaults to "key".
	// +kubebuilder:validation:MaxLength=253
	SecretKey string `json:"secretKey,omitempty"`
}

// CoderProvisionerSpec defines the desired state of a CoderProvisioner.
type CoderProvisionerSpec struct {
	// ControlPlaneRef identifies which CoderControlPlane instance to join.
	// Provisioner key management uses operator-managed access from this
	// control plane's status.operatorTokenSecretRef.
	ControlPlaneRef corev1.LocalObjectReference `json:"controlPlaneRef"`
	// OrganizationName is the Coder organization. Defaults to "default".
	// +kubebuilder:validation:MaxLength=128
	OrganizationName string `json:"organizationName,omitempty"`
	// Key configures provisioner key naming and secret storage.
	Key CoderProvisionerKeySpec `json:"key,omitempty"`
	// Replicas is the desired number of provisioner pods.
	Replicas *int32 `json:"replicas,omitempty"`
	// Tags are attached to the provisioner key for job routing.
	Tags map[string]string `json:"tags,omitempty"`
	// Image is the container image. Defaults to the control plane image.
	Image string `json:"image,omitempty"`
	// ExtraArgs are appended after "provisioner start".
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// ExtraEnv are injected into the provisioner container.
	ExtraEnv []corev1.EnvVar `json:"extraEnv,omitempty"`
	// Resources for the provisioner container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// ImagePullSecrets are used by the pod to pull private images.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// TerminationGracePeriodSeconds for the provisioner pods.
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// CoderProvisionerStatus defines the observed state of a CoderProvisioner.
type CoderProvisionerStatus struct {
	// ObservedGeneration tracks the spec generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ReadyReplicas is the number of ready pods observed in the deployment.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Phase is a high-level readiness indicator.
	Phase string `json:"phase,omitempty"`
	// Conditions are Kubernetes-standard conditions for this resource.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// OrganizationID is the organization ID last applied to the provisioner key.
	OrganizationID string `json:"organizationID,omitempty"`
	// OrganizationName is the organization name last applied to the provisioner key.
	OrganizationName string `json:"organizationName,omitempty"`
	// ProvisionerKeyID is the provisioner key ID last applied in coderd.
	ProvisionerKeyID string `json:"provisionerKeyID,omitempty"`
	// ProvisionerKeyName is the provisioner key name last applied in coderd.
	ProvisionerKeyName string `json:"provisionerKeyName,omitempty"`
	// TagsHash is a deterministic hash of spec.tags last applied to the provisioner key.
	TagsHash string `json:"tagsHash,omitempty"`
	// ControlPlaneRefName is the control plane ref name last applied to the provisioner key.
	ControlPlaneRefName string `json:"controlPlaneRefName,omitempty"`
	// ControlPlaneURL is the control plane URL last applied to the provisioner key.
	ControlPlaneURL string `json:"controlPlaneURL,omitempty"`
	// ControlPlaneUID is the UID of the control plane last applied to the
	// provisioner key. The operator sends the operator token of the control
	// plane with controlPlaneRefName for this key only while that control
	// plane has this UID, and rotates the key when it has another UID. When it
	// is empty, it sends that token only to the current Coder API URL of that
	// control plane.
	ControlPlaneUID string `json:"controlPlaneUID,omitempty"`
	// SecretRef references the provisioner key secret data currently in use.
	SecretRef *SecretKeySelector `json:"secretRef,omitempty"`
	// PendingKeys lists the provisioner keys that can exist in Coder besides
	// the key in provisionerKeyID: a key that the operator is about to create
	// or has not yet written to the provisioner key Secret, and replaced keys
	// whose deletion Coder has not confirmed. The operator deletes the key of
	// each entry with organizationID and keyID at its coderURL with the
	// operator token of its control plane, and removes the entry when Coder
	// confirms that the key is gone.
	// +listType=atomic
	PendingKeys []CoderProvisionerKeyRecord `json:"pendingKeys,omitempty"`
}

// CoderProvisionerKeyRecord names a provisioner key that the operator created,
// or was about to create, at one Coder endpoint.
type CoderProvisionerKeyRecord struct {
	// ControlPlaneRefName is the CoderControlPlane whose operator token the
	// operator used for the key.
	ControlPlaneRefName string `json:"controlPlaneRefName"`
	// ControlPlaneUID is the UID of that CoderControlPlane. The operator
	// sends its operator token for this entry only while the control plane
	// with controlPlaneRefName has this UID. When it is empty, it sends that
	// token only when coderURL is the current Coder API URL of that control
	// plane. Otherwise it keeps the entry and sets the condition
	// ControlPlaneChangedKeyKept.
	ControlPlaneUID string `json:"controlPlaneUID,omitempty"`
	// CoderURL is the Coder API URL that the operator sent the key request to.
	CoderURL string `json:"coderURL"`
	// OrganizationName is the Coder organization of the key.
	OrganizationName string `json:"organizationName"`
	// KeyName is the provisioner key name in Coder.
	KeyName string `json:"keyName"`
	// OrganizationID is the organization ID that Coder returned for the key.
	// When organizationID or keyID is empty, the operator deletes no key. It
	// removes the entry when Coder has no key with keyName, or when that key
	// is the key in provisionerKeyID. Otherwise it keeps the entry and sets
	// the condition UnverifiedKeyKept.
	OrganizationID string `json:"organizationID,omitempty"`
	// KeyID is the provisioner key ID that Coder returned.
	KeyID string `json:"keyID,omitempty"`
	// Unpublished is true from before the create until the operator records
	// a key as applied. While it is true, the Secret can hold this key, or a
	// key that a rotation deleted, so the operator replaces the key in the
	// Secret.
	Unpublished bool `json:"unpublished,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CoderProvisioner is the schema for Coder external provisioner daemon resources.
type CoderProvisioner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderProvisionerSpec   `json:"spec,omitempty"`
	Status CoderProvisionerStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderProvisionerList contains a list of CoderProvisioner objects.
type CoderProvisionerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderProvisioner `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderProvisioner{}, &CoderProvisionerList{})
}
