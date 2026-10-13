package credentialsecrettest

import (
	corev1 "k8s.io/api/core/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// ControlPlaneSecretRoles are the roles, as named in refusal messages, of the
// Secrets that a CoderControlPlane spec references (#249, #260).
var ControlPlaneSecretRoles = []string{
	"license", "database", "extraEnv", "envFrom", "image pull", "TLS", "Ingress TLS", "CA certificate", "volume",
}

// SetControlPlaneSecret makes spec reference secretName in role, or reference
// no Secret in role when secretName is empty. It changes only the field of
// role, and keeps each field valid for the CRD.
func SetControlPlaneSecret(spec *coderv1alpha1.CoderControlPlaneSpec, role, secretName string) {
	set := secretName != ""
	switch role {
	case "license":
		spec.LicenseSecretRef = nil
		if set {
			spec.LicenseSecretRef = &coderv1alpha1.SecretKeySelector{Name: secretName, Key: "license"}
		}
	case "database":
		spec.Database = nil
		if set {
			spec.Database = &coderv1alpha1.DatabaseSpec{ConnectionSecretRef: coderv1alpha1.SecretKeySelector{Name: secretName, Key: "url"}}
		}
	case "extraEnv":
		spec.ExtraEnv = nil
		if set {
			spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_OIDC_CLIENT_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: "value",
			}}}}
		}
	case "envFrom":
		spec.EnvFrom = nil
		if set {
			spec.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}}}}
		}
	case "image pull":
		spec.ImagePullSecrets = nil
		if set {
			spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: secretName}}
		}
	case "TLS":
		spec.TLS.SecretNames = nil
		if set {
			spec.TLS.SecretNames = []string{secretName}
		}
	case "Ingress TLS":
		spec.Expose = nil
		if set {
			spec.Expose = &coderv1alpha1.ExposeSpec{Ingress: &coderv1alpha1.IngressExposeSpec{
				Host: "coder.example.com", TLS: &coderv1alpha1.IngressTLSExposeSpec{SecretName: secretName},
			}}
		}
	case "CA certificate":
		spec.Certs.Secrets = nil
		if set {
			spec.Certs.Secrets = []coderv1alpha1.CertSecretSelector{{Name: secretName, Key: "ca.crt"}}
		}
	case "volume":
		spec.Volumes = nil
		if set {
			spec.Volumes = []corev1.Volume{{Name: "extra", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName}}}}
		}
	default:
		panic("assertion failed: unknown control plane Secret role " + role)
	}
}
