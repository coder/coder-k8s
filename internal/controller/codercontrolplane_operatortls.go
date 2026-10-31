package controller

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controlplaneurl"
)

const (
	// #nosec G101 -- this is a field index key, not a credential.
	caBundleConfigMapNameFieldIndex = ".spec.tls.operatorAccess.caBundleRef.name"

	operatorTLSReasonVerified               = "Verified"
	operatorTLSReasonPending                = "Pending"
	operatorTLSReasonCABundleUnavailable    = "CABundleUnavailable"
	operatorTLSReasonCertificateNotVerified = "CertificateNotVerified"
)

// reconcileOperatorTLSTrust sets the OperatorTLSReady condition from the CA
// bundle: False when it cannot be read or parsed, Unknown until a Coder call
// verifies the certificate with it (see recordOperatorTLSCall). A result of
// another trust or URL, for example before a CA rotation, does not count.
// Without spec.tls.operatorAccess, it removes the condition.
func (r *CoderControlPlaneReconciler) reconcileOperatorTLSTrust(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
) error {
	if coderControlPlane.Spec.TLS.OperatorAccess == nil {
		meta.RemoveStatusCondition(&nextStatus.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
		return nil
	}
	coderTLS, err := controlplaneurl.OperatorTLS(ctx, r.Client, coderControlPlane)
	switch {
	case errors.Is(err, controlplaneurl.ErrUnavailable):
		return setControlPlaneCondition(nextStatus, coderControlPlane.Generation, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady,
			metav1.ConditionFalse, operatorTLSReasonCABundleUnavailable, err.Error())
	case err != nil:
		return err
	}
	label := operatorTLSLabel(coderControlPlane, coderTLS)
	current := meta.FindStatusCondition(nextStatus.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
	if current != nil && current.Reason != operatorTLSReasonCABundleUnavailable && strings.HasSuffix(current.Message, label) {
		return nil
	}
	return setControlPlaneCondition(nextStatus, coderControlPlane.Generation, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady,
		metav1.ConditionUnknown, operatorTLSReasonPending, "The CA bundle is valid. The operator has not called Coder with it yet "+label)
}

// recordOperatorTLSCall sets the OperatorTLSReady condition from the result
// of a Coder call to coderURL verified with coderTLS: True when it succeeded,
// False when the certificate failed verification. Other errors leave the
// condition as it is.
func recordOperatorTLSCall(
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
	coderURL string,
	coderTLS *controlplaneurl.TLS,
	callErr error,
) error {
	label := operatorTLSLabel(coderControlPlane, coderTLS)
	var verifyErr *tls.CertificateVerificationError
	switch {
	case callErr == nil:
		return setControlPlaneCondition(nextStatus, coderControlPlane.Generation, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady,
			metav1.ConditionTrue, operatorTLSReasonVerified, fmt.Sprintf("The operator verified the Coder certificate at %s %s", coderURL, label))
	case errors.As(callErr, &verifyErr):
		return setControlPlaneCondition(nextStatus, coderControlPlane.Generation, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady,
			metav1.ConditionFalse, operatorTLSReasonCertificateNotVerified, fmt.Sprintf("%v %s", verifyErr, label))
	}
	return nil
}

// operatorTLSLabel names the trust and status.url that an OperatorTLSReady
// message is about, so that a result for an older CA bundle, server name or
// URL is not shown as current.
func operatorTLSLabel(coderControlPlane *coderv1alpha1.CoderControlPlane, coderTLS *controlplaneurl.TLS) string {
	sum := sha256.Sum256([]byte(coderTLS.Key() + " " + strings.TrimSpace(coderControlPlane.Status.URL)))
	return "(trust " + hex.EncodeToString(sum[:6]) + ")."
}

func indexByCABundleConfigMapName(obj client.Object) []string {
	coderControlPlane, ok := obj.(*coderv1alpha1.CoderControlPlane)
	if !ok || coderControlPlane.Spec.TLS.OperatorAccess == nil || coderControlPlane.Spec.TLS.OperatorAccess.CABundleRef == nil {
		return nil
	}
	name := strings.TrimSpace(coderControlPlane.Spec.TLS.OperatorAccess.CABundleRef.Name)
	if name == "" {
		return nil
	}
	return []string{name}
}

func (r *CoderControlPlaneReconciler) reconcileRequestsForCABundleConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.reconcileRequestsForIndexedControlPlanes(ctx, obj.GetNamespace(), caBundleConfigMapNameFieldIndex, obj.GetName())
}
