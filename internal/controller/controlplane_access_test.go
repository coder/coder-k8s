package controller

import (
	"context"
	"errors"
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
)

func TestControlPlaneCoderClient(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))

	readyControlPlane := func() *coderv1alpha1.CoderControlPlane {
		cp := &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns"}}
		cp.Status.URL = "http://coder.ns.svc.cluster.local:80"
		cp.Status.OperatorAccessReady = true
		cp.Status.OperatorTokenSecretRef = &coderv1alpha1.SecretKeySelector{Name: "coder-operator-token"}
		return cp
	}
	tokenSecret := func(key, value string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "coder-operator-token", Namespace: "ns"},
			Data:       map[string][]byte{key: []byte(value)},
		}
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
			wantMessage: "The operator token Secret of CoderControlPlane coder has no token yet.",
		},
		{
			name: "operator token key empty",
			objects: func() []client.Object {
				return []client.Object{readyControlPlane(), tokenSecret("token", "")}
			},
			wantReason:  "OperatorAccessNotReady",
			wantMessage: "The operator token Secret of CoderControlPlane coder has no token yet.",
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
