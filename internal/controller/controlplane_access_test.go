package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/tlstest"
)

func TestControlPlaneCoderClient(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))

	readyControlPlane := func() *coderv1alpha1.CoderControlPlane {
		cp := &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns", UID: "cp-uid"}}
		cp.Status.URL = "http://coder.ns.svc.cluster.local:80"
		cp.Status.OperatorAccessReady = true
		cp.Status.OperatorTokenSecretRef = &coderv1alpha1.SecretKeySelector{Name: "coder-operator-token"}
		return cp
	}
	tokenSecret := func(key, value string) *corev1.Secret {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "coder-operator-token", Namespace: "ns"},
			Data:       map[string][]byte{key: []byte(value)},
		}
		secret.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(readyControlPlane(), coderv1alpha1.GroupVersion.WithKind("CoderControlPlane"))}
		return secret
	}

	tests := []struct {
		name        string
		objects     func() []client.Object
		wantReason  string
		wantMessage string
	}{
		{
			name:        "control plane missing",
			objects:     func() []client.Object { return nil },
			wantReason:  "ControlPlaneNotReady",
			wantMessage: "CoderControlPlane coder does not exist.",
		},
		{
			name: "control plane being deleted",
			objects: func() []client.Object {
				cp := readyControlPlane()
				now := metav1.Now()
				cp.DeletionTimestamp, cp.Finalizers = &now, []string{"test.coder.com/hold"}
				return []client.Object{cp, tokenSecret("token", "secret-token")}
			},
			wantReason:  "ControlPlaneNotReady",
			wantMessage: "CoderControlPlane coder is being deleted.",
		},
		{
			name: "no status URL",
			objects: func() []client.Object {
				cp := readyControlPlane()
				cp.Status.URL = ""
				return []client.Object{cp, tokenSecret("token", "secret-token")}
			},
			wantReason:  "ControlPlaneNotReady",
			wantMessage: "CoderControlPlane coder has no status.url yet.",
		},
		{
			name: "operator access not ready",
			objects: func() []client.Object {
				cp := readyControlPlane()
				cp.Status.OperatorAccessReady = false
				return []client.Object{cp, tokenSecret("token", "secret-token")}
			},
			wantReason:  "OperatorAccessNotReady",
			wantMessage: "CoderControlPlane coder has no operator access yet.",
		},
		{
			name: "no operator token reference",
			objects: func() []client.Object {
				cp := readyControlPlane()
				cp.Status.OperatorTokenSecretRef = nil
				return []client.Object{cp, tokenSecret("token", "secret-token")}
			},
			wantReason:  "OperatorAccessNotReady",
			wantMessage: "CoderControlPlane coder has no operator access yet.",
		},
		{
			name:        "operator token Secret missing",
			objects:     func() []client.Object { return []client.Object{readyControlPlane()} },
			wantReason:  "OperatorAccessNotReady",
			wantMessage: `CoderControlPlane coder: operator token unavailable: Secret "coder-operator-token" does not exist.`,
		},
		{
			name: "operator token key empty",
			objects: func() []client.Object {
				return []client.Object{readyControlPlane(), tokenSecret("token", "")}
			},
			wantReason:  "OperatorAccessNotReady",
			wantMessage: `CoderControlPlane coder: operator token unavailable: Secret "coder-operator-token" has no value for key "token".`,
		},
		{
			name: "operator token Secret replaced by one without an owner",
			objects: func() []client.Object {
				secret := tokenSecret("token", "foreign-token")
				secret.OwnerReferences = nil
				return []client.Object{readyControlPlane(), secret}
			},
			wantReason:  "OperatorAccessNotReady",
			wantMessage: `CoderControlPlane coder: operator token unavailable: the control plane is not the controller owner of Secret "coder-operator-token".`,
		},
		{
			name: "operator TLS with an HTTP status URL",
			objects: func() []client.Object {
				cp := readyControlPlane()
				cp.Spec.TLS = coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com"}}
				return []client.Object{cp, tokenSecret("token", "secret-token")}
			},
			wantReason:  "ControlPlaneNotReady",
			wantMessage: `CoderControlPlane coder has no usable Coder URL: no verified HTTPS endpoint: status.url "http://coder.ns.svc.cluster.local:80" is not an HTTPS URL`,
		},
		{
			name: "operator TLS CA bundle ConfigMap missing",
			objects: func() []client.Object {
				cp := readyControlPlane()
				cp.Status.URL = "https://coder.ns.svc.cluster.local:443"
				cp.Spec.TLS = coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{
					CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"},
				}}
				return []client.Object{cp, tokenSecret("token", "secret-token")}
			},
			wantReason:  "ControlPlaneNotReady",
			wantMessage: `CoderControlPlane coder: no verified HTTPS endpoint: CA bundle ConfigMap "coder-ca" does not exist.`,
		},
		{
			name: "unusable status URL",
			objects: func() []client.Object {
				cp := readyControlPlane()
				cp.Status.URL = "http://[::1"
				return []client.Object{cp, tokenSecret("token", "secret-token")}
			},
			wantReason:  "ControlPlaneNotReady",
			wantMessage: `CoderControlPlane coder has no usable Coder URL: parse "http://[::1": missing ']' in host`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects()...).Build()
			sdk, cp, wait, err := controlPlaneCoderClient(context.Background(), reader, "ns", "coder")
			require.NoError(t, err)
			require.Nil(t, sdk)
			require.Nil(t, cp)
			require.Equal(t, &controlPlaneWait{reason: tt.wantReason, message: tt.wantMessage}, wait)
		})
	}

	t.Run("ready with a custom token key", func(t *testing.T) {
		t.Parallel()
		cp := readyControlPlane()
		cp.Status.OperatorTokenSecretRef.Key = " custom "
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, tokenSecret("custom", "secret-token")).Build()
		sdk, gotCP, wait, err := controlPlaneCoderClient(context.Background(), reader, "ns", "coder")
		require.NoError(t, err)
		require.Nil(t, wait)
		require.Equal(t, "coder", gotCP.Name)
		require.Equal(t, "http://coder.ns.svc.cluster.local:80", sdk.URL.String())
		require.Equal(t, "secret-token", sdk.SessionToken())
	})

	t.Run("ready with operator TLS", func(t *testing.T) {
		t.Parallel()
		ca := tlstest.NewCA(t)
		tokens := make(chan string, 10)
		server := ca.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokens <- r.Header.Get("Coder-Session-Token")
			_, _ = w.Write([]byte(`{}`))
		}), "127.0.0.1")
		cp := readyControlPlane()
		cp.Status.URL = server.URL
		cp.Spec.TLS = coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{
			CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"},
		}}
		caConfigMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coder-ca", Namespace: "ns"}, Data: map[string]string{"ca.crt": string(ca.PEM)}}
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, tokenSecret("token", "secret-token"), caConfigMap).Build()

		sdk, _, wait, err := controlPlaneCoderClient(context.Background(), reader, "ns", "coder")
		require.NoError(t, err)
		require.Nil(t, wait)
		require.Equal(t, server.URL, sdk.URL.String())
		res, err := sdk.Request(context.Background(), http.MethodGet, "/api/v2/buildinfo", nil)
		require.NoError(t, err, "the client must verify Coder with the CA bundle")
		require.NoError(t, res.Body.Close())
		require.Equal(t, "secret-token", <-tokens)
	})

	t.Run("read error", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("apiserver down")
			},
		}).Build()
		sdk, cp, wait, err := controlPlaneCoderClient(context.Background(), reader, "ns", "coder")
		require.ErrorContains(t, err, "get codercontrolplane ns/coder: apiserver down")
		require.Nil(t, sdk)
		require.Nil(t, cp)
		require.Nil(t, wait)
	})

	t.Run("assertions", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := controlPlaneCoderClient(context.Background(), nil, "ns", "coder")
		require.ErrorContains(t, err, "assertion failed:")
		reader := fake.NewClientBuilder().WithScheme(scheme).Build()
		_, _, _, err = controlPlaneCoderClient(context.Background(), reader, "ns", "")
		require.ErrorContains(t, err, "assertion failed:")
	})
}

func TestControlPlaneCoderURL(t *testing.T) {
	t.Parallel()
	cp := &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns"}}
	cp.Status.URL = "http://127.0.0.1:8080"
	got, err := controlPlaneCoderURL(cp)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8080", got.String(), "an HTTP status URL stays unchanged")

	// With TLS, certificates rarely cover the service name: use internal HTTP.
	cp.Spec.TLS.SecretNames, cp.Spec.Service.Port = []string{"coder-tls"}, 443
	cp.Status.URL = "https://coder.ns.svc.cluster.local:443"
	got, err = controlPlaneCoderURL(cp)
	require.NoError(t, err)
	require.Equal(t, "http://coder.ns.svc.cluster.local:80", got.String())
}
