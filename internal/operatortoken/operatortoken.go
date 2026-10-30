// Package operatortoken reads the operator token that a CoderControlPlane
// publishes in status.operatorTokenSecretRef. Every component that sends that
// token to Coder reads it here, so each one refuses a Secret that the control
// plane does not control.
package operatortoken

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// ErrUnavailable means that the control plane has no usable operator token
// now: operator access is not ready, the Secret does not exist or holds no
// token, or the control plane is not its controller owner. Callers wait and
// retry. Errors that wrap it never hold Secret data.
var ErrUnavailable = errors.New("operator token unavailable")

// ControlledBy reports whether controlPlane is the controller owner of secret,
// matched by group/version, kind, name and UID.
func ControlledBy(secret *corev1.Secret, controlPlane *coderv1alpha1.CoderControlPlane) bool {
	if secret == nil || controlPlane == nil {
		return false
	}
	owner := metav1.GetControllerOf(secret)
	return owner != nil &&
		owner.APIVersion == coderv1alpha1.GroupVersion.String() &&
		owner.Kind == "CoderControlPlane" &&
		owner.Name == controlPlane.Name &&
		owner.UID == controlPlane.UID
}

// Read returns the operator token of controlPlane from the Secret in
// status.operatorTokenSecretRef. Errors that wrap ErrUnavailable mean that the
// token is not usable now. Other errors are assertion failures or come from
// reader.
func Read(ctx context.Context, reader client.Reader, controlPlane *coderv1alpha1.CoderControlPlane) (string, error) {
	if reader == nil {
		return "", errors.New("assertion failed: operator token read needs a Kubernetes reader")
	}
	if controlPlane == nil {
		return "", errors.New("assertion failed: operator token read needs a control plane")
	}
	// The API server sets the UID. Without it, a Secret whose controller
	// reference has an empty UID would count as owned.
	if controlPlane.Namespace == "" || controlPlane.UID == "" {
		return "", fmt.Errorf("assertion failed: control plane %q needs a namespace and a UID", controlPlane.Name)
	}
	if !controlPlane.Status.OperatorAccessReady {
		return "", fmt.Errorf("%w: operator access is not ready", ErrUnavailable)
	}
	ref := controlPlane.Status.OperatorTokenSecretRef
	if ref == nil || strings.TrimSpace(ref.Name) == "" {
		return "", fmt.Errorf("%w: status.operatorTokenSecretRef names no Secret", ErrUnavailable)
	}
	name := strings.TrimSpace(ref.Name)
	key := strings.TrimSpace(ref.Key)
	if key == "" {
		key = coderv1alpha1.DefaultTokenSecretKey
	}

	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: controlPlane.Namespace, Name: name}, secret); apierrors.IsNotFound(err) {
		return "", fmt.Errorf("%w: Secret %q does not exist", ErrUnavailable, name)
	} else if err != nil {
		return "", fmt.Errorf("read operator token Secret %s/%s: %w", controlPlane.Namespace, name, err)
	}
	if !ControlledBy(secret, controlPlane) {
		return "", fmt.Errorf("%w: the control plane is not the controller owner of Secret %q", ErrUnavailable, name)
	}
	token := string(secret.Data[key])
	if token == "" {
		return "", fmt.Errorf("%w: Secret %q has no value for key %q", ErrUnavailable, name, key)
	}
	return token, nil
}
