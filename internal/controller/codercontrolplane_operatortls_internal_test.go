package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/tlstest"
)

// A change of the CA bundle ConfigMap must reconcile the control planes that
// reference it, so that OperatorTLSReady follows the ConfigMap.
func TestReconcileRequestsForCABundleConfigMap(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))
	controlPlane := func(namespace, name string, access *coderv1alpha1.OperatorTLSSpec) *coderv1alpha1.CoderControlPlane {
		return &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       coderv1alpha1.CoderControlPlaneSpec{TLS: coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: access}},
		}
	}
	caBundle := &coderv1alpha1.OperatorTLSSpec{CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			controlPlane("ns", "uses-ca", caBundle),
			controlPlane("ns", "public-roots", &coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com"}),
			controlPlane("ns", "plain", nil),
			controlPlane("other", "uses-ca", caBundle),
		).
		WithIndex(&coderv1alpha1.CoderControlPlane{}, caBundleConfigMapNameFieldIndex, indexByCABundleConfigMapName).
		Build()
	r := &CoderControlPlaneReconciler{Client: c, Scheme: scheme}

	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coder-ca", Namespace: "ns"}}
	require.Equal(t,
		[]reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "uses-ca"}}},
		r.reconcileRequestsForCABundleConfigMap(context.Background(), configMap))
	configMap.Name = "unrelated"
	require.Empty(t, r.reconcileRequestsForCABundleConfigMap(context.Background(), configMap))
}

func operatorTLSFixture(t *testing.T, statusURL string, caBundle []byte) (*CoderControlPlaneReconciler, *coderv1alpha1.CoderControlPlane) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns", UID: types.UID("cp-uid"), Generation: 1},
		Spec: coderv1alpha1.CoderControlPlaneSpec{TLS: coderv1alpha1.TLSSpec{
			SecretNames:    []string{"coder-tls"},
			OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"}},
		}},
		Status: coderv1alpha1.CoderControlPlaneStatus{
			URL: statusURL, ObservedGeneration: 1, Phase: coderv1alpha1.CoderControlPlanePhaseReady, OperatorAccessReady: true,
			OperatorTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "operator-token", Key: coderv1alpha1.DefaultTokenSecretKey},
		},
	}
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-token", Namespace: "ns"},
		Data:       map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("operator-token")},
	}
	require.NoError(t, ctrl.SetControllerReference(cp, token, scheme))
	caConfigMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coder-ca", Namespace: "ns"}, Data: map[string]string{"ca.crt": string(caBundle)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cp).WithObjects(cp, token, caConfigMap).Build()
	return &CoderControlPlaneReconciler{Client: c, APIReader: c, Scheme: scheme, EntitlementsInspector: NewSDKEntitlementsInspector()}, cp
}

func persistedOperatorTLSReady(t *testing.T, r *CoderControlPlaneReconciler) *metav1.Condition {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "coder"}, cp))
	condition := meta.FindStatusCondition(cp.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTLSReady)
	require.NotNil(t, condition)
	return condition
}

type entitlementsFunc func(context.Context, string, *controlplaneurl.TLS, string) (codersdk.Entitlements, error)

func (f entitlementsFunc) Entitlements(ctx context.Context, coderURL string, coderTLS *controlplaneurl.TLS, token string) (codersdk.Entitlements, error) {
	return f(ctx, coderURL, coderTLS, token)
}

// rotatingCAReader returns replacement as the CA bundle from the second read
// of the CA ConfigMap on, as a cache does after a rotation.
type rotatingCAReader struct {
	client.Client
	reads       int
	replacement string
}

func (c *rotatingCAReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if configMap, ok := obj.(*corev1.ConfigMap); ok && err == nil && key.Name == "coder-ca" {
		c.reads++
		if c.reads >= 2 {
			configMap.Data["ca.crt"] = c.replacement
		}
	}
	return err
}

// A CA rotation between the reads of one reconcile must not leave Verified
// for the old CA: the condition and the call use one trust, and the status
// write publishes no result for a trust other than the one it reads.
func TestOperatorTLSRotationDuringReconcile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	oldCA, newCA := tlstest.NewCA(t), tlstest.NewCA(t)
	r, cp := operatorTLSFixture(t, "https://coder.ns.svc.cluster.local:443", oldCA.PEM)
	oldTrust, err := controlplaneurl.NewTLS(oldCA.PEM, "")
	require.NoError(t, err)
	newTrust, err := controlplaneurl.NewTLS(newCA.PEM, "")
	require.NoError(t, err)
	require.NoError(t, recordOperatorTLSCall(cp, &cp.Status, cp.Status.URL, oldTrust, nil))
	require.NoError(t, r.Status().Update(ctx, cp))

	rotating := &rotatingCAReader{Client: r.Client, replacement: string(newCA.PEM)}
	r.Client, r.APIReader = rotating, rotating
	var calledWith string
	r.EntitlementsInspector = entitlementsFunc(func(_ context.Context, _ string, coderTLS *controlplaneurl.TLS, _ string) (codersdk.Entitlements, error) {
		calledWith = coderTLS.Key()
		return codersdk.Entitlements{}, errors.New("connection refused")
	})
	base := *cp.Status.DeepCopy()
	next := *cp.Status.DeepCopy()
	_, err = r.reconcileEntitlements(ctx, cp, &next)
	require.NoError(t, err)
	require.Equal(t, oldTrust.Key(), calledWith, "the call uses the trust that the condition was computed from")
	require.NoError(t, r.reconcileStatus(ctx, cp, base, next))

	condition := persistedOperatorTLSReady(t, r)
	require.Equal(t, operatorTLSReasonPending, condition.Reason)
	require.True(t, strings.HasSuffix(condition.Message, operatorTLSLabel(newTrust, cp.Status.URL)), condition.Message)
}

// A verified call that started before a CA rotation and ends after the
// result for the new CA was written must not overwrite that result.
func TestOperatorTLSInFlightCallCannotRepublishVerified(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ca := tlstest.NewCA(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := ca.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"features":{}}`))
	}), "127.0.0.1")
	r, cp := operatorTLSFixture(t, server.URL, ca.PEM)

	oldBase, oldNext := *cp.Status.DeepCopy(), *cp.Status.DeepCopy()
	done := make(chan error, 1)
	go func() {
		_, err := r.reconcileEntitlements(ctx, cp, &oldNext)
		if err == nil {
			err = r.reconcileStatus(ctx, cp, oldBase, oldNext)
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the call with the old CA never reached Coder")
	}

	caConfigMap := &corev1.ConfigMap{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "coder-ca"}, caConfigMap))
	caConfigMap.Data["ca.crt"] = string(tlstest.NewCA(t).PEM)
	require.NoError(t, r.Update(ctx, caConfigMap))
	current := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "coder"}, current))
	next := *current.Status.DeepCopy()
	_, err := r.reconcileEntitlements(ctx, current, &next)
	require.NoError(t, err)
	require.NoError(t, r.reconcileStatus(ctx, current, current.Status, next))
	require.Equal(t, operatorTLSReasonCertificateNotVerified, persistedOperatorTLSReady(t, r).Reason)

	close(release)
	require.NoError(t, <-done)
	require.EqualValues(t, 1, requests.Load(), "the new CA does not verify Coder, so Coder gets no request with it")
	require.Equal(t, operatorTLSReasonCertificateNotVerified, persistedOperatorTLSReady(t, r).Reason,
		"the result of the old CA must not overwrite the result of the new one")
}
