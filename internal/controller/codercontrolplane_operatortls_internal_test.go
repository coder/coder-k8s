package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
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
