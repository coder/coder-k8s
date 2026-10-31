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

// reconcileOperatorTLSTrust reads the trust for the operator's calls to
// coderControlPlane and sets the OperatorTLSReady condition from it: False
// when the CA bundle cannot be read or parsed (unavailable), Unknown until a
// Coder call verifies the certificate with it (see recordOperatorTLSCall). A
// result of another trust or URL, for example before a CA rotation, does not
// count. Without spec.tls.operatorAccess, it removes the condition and returns
// no trust.
func (r *CoderControlPlaneReconciler) reconcileOperatorTLSTrust(
	ctx context.Context,
	coderControlPlane *coderv1alpha1.CoderControlPlane,
	nextStatus *coderv1alpha1.CoderControlPlaneStatus,
) (coderTLS *controlplaneurl.TLS, unavailable bool, err error) {
	if coderControlPlane.Spec.TLS.OperatorAccess == nil {
		meta.RemoveStatusCondition(&nextStatus.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
		return nil, false, nil
	}
	coderTLS, err = controlplaneurl.OperatorTLS(ctx, r.Client, coderControlPlane)
	if err != nil && !errors.Is(err, controlplaneurl.ErrUnavailable) {
		return nil, false, err
	}
	unavailable = err != nil
	current := meta.FindStatusCondition(nextStatus.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
	if current != nil && !unavailable && operatorTLSConditionCurrent(current, coderTLS, err, coderControlPlane.Status.URL) {
		return coderTLS, false, nil
	}
	return coderTLS, unavailable, setOperatorTLSTrustCondition(nextStatus, coderControlPlane.Generation, coderTLS, err, coderControlPlane.Status.URL)
}

// setOperatorTLSTrustCondition sets the OperatorTLSReady condition that the
// trust alone gives, before any call: False when trustErr shows that the CA
// bundle is unavailable, Unknown otherwise.
func setOperatorTLSTrustCondition(
	status *coderv1alpha1.CoderControlPlaneStatus,
	generation int64,
	coderTLS *controlplaneurl.TLS,
	trustErr error,
	statusURL string,
) error {
	if trustErr != nil {
		return setControlPlaneCondition(status, generation, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady,
			metav1.ConditionFalse, operatorTLSReasonCABundleUnavailable, trustErr.Error())
	}
	return setControlPlaneCondition(status, generation, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady,
		metav1.ConditionUnknown, operatorTLSReasonPending, "The CA bundle is valid. The operator has not called Coder with it yet "+operatorTLSLabel(coderTLS, statusURL))
}

// operatorTLSConditionCurrent reports whether condition is about the trust
// that the operator reads now (coderTLS, or trustErr when the CA bundle is
// unavailable) and statusURL.
func operatorTLSConditionCurrent(condition *metav1.Condition, coderTLS *controlplaneurl.TLS, trustErr error, statusURL string) bool {
	if trustErr != nil {
		return condition.Reason == operatorTLSReasonCABundleUnavailable
	}
	return condition.Reason != operatorTLSReasonCABundleUnavailable && strings.HasSuffix(condition.Message, operatorTLSLabel(coderTLS, statusURL))
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
	label := operatorTLSLabel(coderTLS, coderControlPlane.Status.URL)
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

// keepCurrentOperatorTLSCondition runs before a status write, against the
// latest control plane and the CA bundle that reader reads then. A merged
// OperatorTLSReady condition about another trust or status.url, for example
// the result of a call that started before a CA rotation, is not written:
// merged gets the condition of latest when that one is current, and the
// condition that the current trust alone gives otherwise. A ConfigMap update
// does not change the control plane generation, so the generation check of
// the write cannot catch this.
func keepCurrentOperatorTLSCondition(
	ctx context.Context,
	reader client.Reader,
	latest *coderv1alpha1.CoderControlPlane,
	merged *coderv1alpha1.CoderControlPlaneStatus,
) error {
	condition := meta.FindStatusCondition(merged.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
	if condition == nil || latest.Spec.TLS.OperatorAccess == nil {
		return nil
	}
	coderTLS, err := controlplaneurl.OperatorTLS(ctx, reader, latest)
	if err != nil && !errors.Is(err, controlplaneurl.ErrUnavailable) {
		return err
	}
	if operatorTLSConditionCurrent(condition, coderTLS, err, merged.URL) {
		return nil
	}
	latestCondition := meta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
	if latestCondition != nil && operatorTLSConditionCurrent(latestCondition, coderTLS, err, merged.URL) {
		*condition = *latestCondition
		return nil
	}
	return setOperatorTLSTrustCondition(merged, latest.Generation, coderTLS, err, merged.URL)
}

// operatorTLSLabel names the trust and status.url that an OperatorTLSReady
// message is about, so that a result for an older CA bundle, server name or
// URL is not shown as current.
func operatorTLSLabel(coderTLS *controlplaneurl.TLS, statusURL string) string {
	sum := sha256.Sum256([]byte(coderTLS.Key() + " " + strings.TrimSpace(statusURL)))
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
