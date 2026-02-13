package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderControlPlanePhasePending indicates the control plane has not reported ready yet.
	CoderControlPlanePhasePending = "Pending"
	// CoderControlPlanePhaseReady indicates at least one control plane pod is ready.
	CoderControlPlanePhaseReady = "Ready"
	// CoderControlPlaneConditionLicenseApplied indicates whether the operator uploaded the configured license.
	CoderControlPlaneConditionLicenseApplied = "LicenseApplied"

	// CoderControlPlaneLicenseTierNone indicates no license is currently installed.
	CoderControlPlaneLicenseTierNone = "none"
	// CoderControlPlaneLicenseTierTrial indicates a trial license is currently installed.
	CoderControlPlaneLicenseTierTrial = "trial"
	// CoderControlPlaneLicenseTierEnterprise indicates an enterprise license is currently installed.
	CoderControlPlaneLicenseTierEnterprise = "enterprise"
	// CoderControlPlaneLicenseTierPremium indicates a premium license is currently installed.
	CoderControlPlaneLicenseTierPremium = "premium"
	// CoderControlPlaneLicenseTierUnknown indicates the controller could not determine the current license tier.
	CoderControlPlaneLicenseTierUnknown = "unknown"

	// CoderControlPlaneEntitlementUnknown indicates the controller could not determine a feature entitlement.
	CoderControlPlaneEntitlementUnknown = "unknown"
)

// CoderControlPlaneSpec defines the desired state of a CoderControlPlane.
type CoderControlPlaneSpec struct {
	// Image is the container image used for the Coder control plane pod.
	// +kubebuilder:default="ghcr.io/coder/coder:latest"
	Image string `json:"image,omitempty"`
	// Replicas is the desired number of control plane pods.
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`
	// Service controls the service created in front of the control plane.
	// +kubebuilder:default={}
	Service ServiceSpec `json:"service,omitempty"`
	// ExtraArgs are appended to the default Coder server arguments.
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// ExtraEnv are injected into the Coder control plane container.
	ExtraEnv []corev1.EnvVar `json:"extraEnv,omitempty"`
	// ImagePullSecrets are used by the pod to pull private images.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// OperatorAccess configures bootstrap API access to the coderd instance.
	// +kubebuilder:default={}
	OperatorAccess OperatorAccessSpec `json:"operatorAccess,omitempty"`
	// LicenseSecretRef references a Secret key containing a Coder Enterprise
	// license JWT. When set, the controller uploads the license after the
	// control plane is ready and re-uploads when the Secret value changes.
	// +optional
	LicenseSecretRef *SecretKeySelector `json:"licenseSecretRef,omitempty"`

	// ServiceAccount configures the ServiceAccount for the control plane pod.
	// +kubebuilder:default={}
	ServiceAccount ServiceAccountSpec `json:"serviceAccount,omitempty"`
	// RBAC configures namespace-scoped RBAC for workspace provisioning.
	// +kubebuilder:default={}
	RBAC RBACSpec `json:"rbac,omitempty"`

	// Resources sets resource requests/limits for the control plane container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// SecurityContext sets the container security context.
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`
	// PodSecurityContext sets the pod-level security context.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// TLS configures Coder built-in TLS.
	// +kubebuilder:default={}
	TLS TLSSpec `json:"tls,omitempty"`

	// ReadinessProbe configures the readiness probe for the control plane container.
	// +kubebuilder:default={enabled:true}
	ReadinessProbe ProbeSpec `json:"readinessProbe,omitempty"`
	// LivenessProbe configures the liveness probe for the control plane container.
	// +kubebuilder:default={enabled:false}
	LivenessProbe ProbeSpec `json:"livenessProbe,omitempty"`

	// EnvUseClusterAccessURL injects a default CODER_ACCESS_URL when not explicitly set.
	// +kubebuilder:default=true
	EnvUseClusterAccessURL *bool `json:"envUseClusterAccessURL,omitempty"`

	// Expose configures external exposure via Ingress or Gateway API.
	// +optional
	Expose *ExposeSpec `json:"expose,omitempty"`

	// EnvFrom injects environment variables from ConfigMaps/Secrets.
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`
	// Volumes are additional volumes to add to the pod.
	Volumes []corev1.Volume `json:"volumes,omitempty"`
	// VolumeMounts are additional volume mounts for the control plane container.
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`
	// Certs configures additional CA certificate mounts.
	// +kubebuilder:default={}
	Certs CertsSpec `json:"certs,omitempty"`

	// NodeSelector constrains pod scheduling to nodes matching labels.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations are applied to the control plane pod.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity configures pod affinity/anti-affinity rules.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// TopologySpreadConstraints control pod topology spread.
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
}

// OperatorAccessSpec configures the controller-managed coderd operator user.
type OperatorAccessSpec struct {
	// Disabled turns off creation and management of the `coder-k8s-operator`
	// user and API token.
	// +kubebuilder:default=false
	Disabled bool `json:"disabled,omitempty"`
	// GeneratedTokenSecretName stores the generated operator API token.
	GeneratedTokenSecretName string `json:"generatedTokenSecretName,omitempty"`
}

// CoderControlPlaneStatus defines the observed state of a CoderControlPlane.
type CoderControlPlaneStatus struct {
	// ObservedGeneration tracks the spec generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ReadyReplicas is the number of ready pods observed in the deployment.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// URL is the in-cluster URL for the control plane service.
	URL string `json:"url,omitempty"`
	// OperatorTokenSecretRef points to the Secret key containing the `coder-k8s-operator` API token.
	OperatorTokenSecretRef *SecretKeySelector `json:"operatorTokenSecretRef,omitempty"`
	// OperatorAccessReady reports whether operator API access bootstrap succeeded.
	OperatorAccessReady bool `json:"operatorAccessReady,omitempty"`
	// LicenseLastApplied is the timestamp of the most recent successful
	// operator-managed license upload.
	// +optional
	LicenseLastApplied *metav1.Time `json:"licenseLastApplied,omitempty"`
	// LicenseLastAppliedHash is the SHA-256 hex hash of the trimmed license JWT
	// that LicenseLastApplied refers to.
	// +optional
	LicenseLastAppliedHash string `json:"licenseLastAppliedHash,omitempty"`
	// LicenseTier is a best-effort classification of the currently applied license.
	// Values: none, trial, enterprise, premium, unknown.
	// +optional
	LicenseTier string `json:"licenseTier,omitempty"`
	// EntitlementsLastChecked is when the operator last queried coderd entitlements.
	// +optional
	EntitlementsLastChecked *metav1.Time `json:"entitlementsLastChecked,omitempty"`
	// ExternalProvisionerDaemonsEntitlement is the entitlement value for feature
	// "external_provisioner_daemons".
	// Values: entitled, grace_period, not_entitled, unknown.
	// +optional
	ExternalProvisionerDaemonsEntitlement string `json:"externalProvisionerDaemonsEntitlement,omitempty"`
	// Phase is a high-level readiness indicator.
	Phase string `json:"phase,omitempty"`
	// Conditions are Kubernetes-standard conditions for this resource.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status

// CoderControlPlane is the schema for Coder control plane resources.
type CoderControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderControlPlaneSpec   `json:"spec,omitempty"`
	Status CoderControlPlaneStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderControlPlaneList contains a list of CoderControlPlane objects.
type CoderControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderControlPlane{}, &CoderControlPlaneList{})
}
