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

	// DefaultProvisionerKeySecretKey is the default data key for provisioner key secrets.
	DefaultProvisionerKeySecretKey = "key"

	// ProvisionerKeyCleanupFinalizer is applied to ensure coderd key cleanup on deletion.
	ProvisionerKeyCleanupFinalizer = "coder.com/provisioner-key-cleanup"
)

// CoderProvisionerBootstrapSpec configures credentials for provisioner key management.
type CoderProvisionerBootstrapSpec struct {
	// CredentialsSecretRef points to a Secret containing a Coder session token
	// with permission to manage provisioner keys.
	CredentialsSecretRef SecretKeySelector `json:"credentialsSecretRef"`
}

// CoderProvisionerKeySpec configures provisioner key naming and storage.
type CoderProvisionerKeySpec struct {
	// Name is the provisioner key name in coderd. Defaults to the CR name.
	Name string `json:"name,omitempty"`
	// SecretName is the Kubernetes Secret to store the key. Defaults to "{crName}-provisioner-key".
	SecretName string `json:"secretName,omitempty"`
	// SecretKey is the data key in the Secret. Defaults to "key".
	SecretKey string `json:"secretKey,omitempty"`
}

// CoderProvisionerSpec defines the desired state of a CoderProvisioner.
type CoderProvisionerSpec struct {
	// ControlPlaneRef identifies which CoderControlPlane instance to join.
	ControlPlaneRef corev1.LocalObjectReference `json:"controlPlaneRef"`
	// OrganizationName is the Coder organization. Defaults to "default".
	OrganizationName string `json:"organizationName,omitempty"`
	// Bootstrap configures credentials for provisioner key management.
	Bootstrap CoderProvisionerBootstrapSpec `json:"bootstrap"`
	// Key configures provisioner key naming and secret storage.
	Key CoderProvisionerKeySpec `json:"key,omitempty"`
	// Replicas is the desired number of provisioner pods.
	Replicas *int32 `json:"replicas,omitempty"`
	// Tags are attached to the provisioner key for job routing.
	Tags map[string]string `json:"tags,omitempty"`
	// Image is the container image. Defaults to the control plane image.
	Image string `json:"image,omitempty"`
	// ExtraArgs are appended after "provisionerd start".
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
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	OrganizationID     string             `json:"organizationID,omitempty"`
	ProvisionerKeyID   string             `json:"provisionerKeyID,omitempty"`
	ProvisionerKeyName string             `json:"provisionerKeyName,omitempty"`
	SecretRef          *SecretKeySelector `json:"secretRef,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status

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
