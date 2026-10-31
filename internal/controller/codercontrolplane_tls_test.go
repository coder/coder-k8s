package controller_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/tlstest"
)

func TestCoderControlPlaneOperatorTLSAdmission(t *testing.T) {
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "operator-tls-admission")
	caBundleRef := &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"}

	tests := []struct {
		name        string
		tls         coderv1alpha1.TLSSpec
		wantMessage string
	}{
		{name: "ca-bundle", tls: coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: caBundleRef}}},
		{name: "server-name", tls: coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com"}}},
		{
			name:        "no-tls-secrets",
			tls:         coderv1alpha1.TLSSpec{OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: caBundleRef}},
			wantMessage: "tls.operatorAccess needs tls.secretNames",
		},
		{
			name:        "no-trust",
			tls:         coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{}},
			wantMessage: "tls.operatorAccess needs caBundleRef or serverName",
		},
		{
			name:        "empty-key",
			tls:         coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca"}}},
			wantMessage: "spec.tls.operatorAccess.caBundleRef.key",
		},
		{
			name:        "server-name-with-port",
			tls:         coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com:443"}},
			wantMessage: "spec.tls.operatorAccess.serverName",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: namespace},
				Spec:       coderv1alpha1.CoderControlPlaneSpec{TLS: tt.tls},
			}
			err := k8sClient.Create(ctx, cp)
			if tt.wantMessage == "" {
				if err != nil {
					t.Fatalf("expected admission to accept %s, got %v", tt.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected admission to reject %s", tt.name)
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("expected Invalid error containing %q, got %v", tt.wantMessage, err)
			}
		})
	}
}

// TestReconcile_LicenseAndEntitlementsUseOperatorTLS checks that with
// tls.operatorAccess the license and entitlements calls go to status.url with
// the CA bundle, and that a missing CA bundle stops them instead of falling
// back to the plain HTTP Service URL.
func TestReconcile_LicenseAndEntitlementsUseOperatorTLS(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "operator-tls-license")

	licenseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "license", Namespace: namespace},
		Data:       map[string][]byte{coderv1alpha1.DefaultLicenseSecretKey: []byte("license-jwt")},
	}
	require.NoError(t, k8sClient.Create(ctx, licenseSecret))
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: namespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: "postgres://example/operator-tls-license"}},
			TLS: coderv1alpha1.TLSSpec{
				SecretNames:    []string{"coder-tls"},
				OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"}},
			},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: licenseSecret.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cp) })

	uploader := &fakeLicenseUploader{}
	inspector := &fakeEntitlementsInspector{}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: &fakeOperatorAccessProvisioner{token: "operator-token"},
		LicenseUploader:           uploader,
		EntitlementsInspector:     inspector,
	}
	reconcile := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
		require.NoError(t, err)
	}
	reconcile()
	deployment := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment))
	deployment.Status.ReadyReplicas, deployment.Status.Replicas = 1, 1
	require.NoError(t, k8sClient.Status().Update(ctx, deployment))

	reconcile()
	require.Empty(t, uploader.calls, "a missing CA bundle must stop the license upload")
	require.Zero(t, uploader.hasAnyLicenseCall)
	require.Empty(t, inspector.requests, "a missing CA bundle must stop the entitlements check")
	reconciled := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled))
	condition := meta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionLicenseApplied)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Contains(t, condition.Message, `CA bundle ConfigMap "coder-ca" does not exist`)
	requireOperatorTLSReady := func(status metav1.ConditionStatus, reason, message string) {
		t.Helper()
		reconciled := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled))
		condition := meta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
		require.NotNil(t, condition)
		require.Equal(t, status, condition.Status)
		require.Equal(t, reason, condition.Reason)
		require.Contains(t, condition.Message, message)
	}
	requireOperatorTLSReady(metav1.ConditionFalse, "CABundleUnavailable", `CA bundle ConfigMap "coder-ca" does not exist`)

	require.NoError(t, k8sClient.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-ca", Namespace: namespace},
		Data:       map[string]string{"ca.crt": string(tlstest.NewCA(t).PEM)},
	}))
	reconcile()
	statusURL := fmt.Sprintf("https://coder.%s.svc.cluster.local:443", namespace)
	require.Len(t, uploader.calls, 1)
	require.Equal(t, statusURL, uploader.calls[0].coderURL)
	require.NotNil(t, uploader.calls[0].coderTLS)
	require.NotEmpty(t, inspector.requests)
	for _, req := range inspector.requests {
		require.Equal(t, statusURL, req.coderURL)
		require.NotNil(t, req.coderTLS)
	}
	requireOperatorTLSReady(metav1.ConditionTrue, "Verified", statusURL)

	inspector.err = fmt.Errorf("query coder entitlements: %w", &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")})
	reconcile()
	requireOperatorTLSReady(metav1.ConditionFalse, "CertificateNotVerified", "x509: certificate signed by unknown authority")

	inspector.err = errors.New("connection refused")
	reconcile()
	requireOperatorTLSReady(metav1.ConditionFalse, "CertificateNotVerified", "x509: certificate signed by unknown authority")

	// A result for the old CA bundle must not stay after a rotation.
	inspector.err = nil
	reconcile()
	requireOperatorTLSReady(metav1.ConditionTrue, "Verified", statusURL)
	caConfigMap := &corev1.ConfigMap{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "coder-ca", Namespace: namespace}, caConfigMap))
	caConfigMap.Data["ca.crt"] = string(tlstest.NewCA(t).PEM)
	require.NoError(t, k8sClient.Update(ctx, caConfigMap))
	inspector.err = errors.New("connection refused")
	require.Eventually(t, func() bool {
		reconcile()
		reconciled := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled))
		condition := meta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
		return condition != nil && condition.Reason == "Pending"
	}, 10*time.Second, 100*time.Millisecond, "a rotated CA bundle without a verified call must be Pending, not Verified")
	requireOperatorTLSReady(metav1.ConditionUnknown, "Pending", "has not called Coder with it yet")

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, cp))
	cp.Spec.TLS.OperatorAccess = nil
	require.NoError(t, k8sClient.Update(ctx, cp))
	reconcile()
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled))
	require.Nil(t, meta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady),
		"without operatorAccess, the condition goes away")
}

func TestSDKLicenseClientsVerifyCoderTLS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ca := tlstest.NewCA(t)
	var requests atomic.Int32
	server := ca.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v2/licenses" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{"features":{}}`))
	}), "127.0.0.1")
	newTrust := func(caBundle []byte) *controlplaneurl.TLS {
		trust, err := controlplaneurl.NewTLS(caBundle, "")
		require.NoError(t, err)
		return trust
	}
	uploader, inspector := controller.NewSDKLicenseUploader(), controller.NewSDKEntitlementsInspector()

	_, err := uploader.HasAnyLicense(ctx, server.URL, newTrust(ca.PEM), "operator-token")
	require.NoError(t, err)
	_, err = inspector.Entitlements(ctx, server.URL, newTrust(ca.PEM), "operator-token")
	require.NoError(t, err)
	require.EqualValues(t, 2, requests.Load())

	otherCA := newTrust(tlstest.NewCA(t).PEM)
	var verifyErr *tls.CertificateVerificationError
	_, err = uploader.HasAnyLicense(ctx, server.URL, otherCA, "operator-token")
	require.True(t, errors.As(err, &verifyErr), "want a certificate verification error, got %v", err)
	err = uploader.AddLicense(ctx, server.URL, otherCA, "operator-token", "license-jwt")
	require.True(t, errors.As(err, &verifyErr), "want a certificate verification error, got %v", err)
	_, err = inspector.Entitlements(ctx, server.URL, otherCA, "operator-token")
	require.True(t, errors.As(err, &verifyErr), "want a certificate verification error, got %v", err)
	require.EqualValues(t, 2, requests.Load(), "a server that failed verification must get no request")
}
