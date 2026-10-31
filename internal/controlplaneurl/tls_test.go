package controlplaneurl_test

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
	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/tlstest"
)

func TestOperatorTLS(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	ca := tlstest.NewCA(t)
	controlPlane := func(access *coderv1alpha1.OperatorTLSSpec) *coderv1alpha1.CoderControlPlane {
		return &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
			Spec:       coderv1alpha1.CoderControlPlaneSpec{TLS: coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: access}},
		}
	}
	withCABundle := &coderv1alpha1.OperatorTLSSpec{CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"}}
	caConfigMap := func(namespace string, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coder-ca", Namespace: namespace}, Data: data}
	}

	t.Run("no trust without operatorAccess", func(t *testing.T) {
		t.Parallel()
		trust, err := controlplaneurl.OperatorTLS(context.Background(), nil, controlPlane(nil))
		require.NoError(t, err)
		require.Nil(t, trust)
	})

	t.Run("system roots without caBundleRef", func(t *testing.T) {
		t.Parallel()
		trust, err := controlplaneurl.OperatorTLS(context.Background(), nil, controlPlane(&coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com"}))
		require.NoError(t, err)
		require.Nil(t, trust.ClientConfig().RootCAs)
		require.Equal(t, "coder.example.com", trust.ClientConfig().ServerName)
	})

	unavailable := []struct {
		name    string
		objects []client.Object
		want    string
	}{
		{name: "ConfigMap missing", want: `CA bundle ConfigMap "coder-ca" does not exist`},
		{name: "ConfigMap in another namespace", objects: []client.Object{caConfigMap("team-b", map[string]string{"ca.crt": string(ca.PEM)})}, want: `CA bundle ConfigMap "coder-ca" does not exist`},
		{name: "key missing", objects: []client.Object{caConfigMap("team-a", map[string]string{"other": string(ca.PEM)})}, want: `CA bundle ConfigMap "coder-ca" has no key "ca.crt"`},
		{name: "empty bundle", objects: []client.Object{caConfigMap("team-a", map[string]string{"ca.crt": ""})}, want: "no PEM certificate"},
		{name: "not PEM", objects: []client.Object{caConfigMap("team-a", map[string]string{"ca.crt": "not a certificate"})}, want: "no PEM certificate"},
		{name: "private key block", objects: []client.Object{caConfigMap("team-a", map[string]string{"ca.crt": string(ca.PEM) + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"})}, want: `PEM block 2 is "PRIVATE KEY", not a CERTIFICATE`},
		{name: "broken certificate", objects: []client.Object{caConfigMap("team-a", map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"})}, want: "parse certificate 1"},
	}
	for _, tt := range unavailable {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()
			trust, err := controlplaneurl.OperatorTLS(context.Background(), reader, controlPlane(withCABundle))
			require.Nil(t, trust)
			require.True(t, errors.Is(err, controlplaneurl.ErrUnavailable), "got %v", err)
			require.ErrorContains(t, err, tt.want)
		})
	}

	t.Run("read error is not unavailable", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("apiserver down")
			},
		}).Build()
		_, err := controlplaneurl.OperatorTLS(context.Background(), reader, controlPlane(withCABundle))
		require.ErrorContains(t, err, "get CA bundle ConfigMap team-a/coder-ca: apiserver down")
		require.False(t, errors.Is(err, controlplaneurl.ErrUnavailable))
	})

	t.Run("CA bundle", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(caConfigMap("team-a", map[string]string{"ca.crt": string(ca.PEM)})).Build()
		trust, err := controlplaneurl.OperatorTLS(context.Background(), reader, controlPlane(withCABundle))
		require.NoError(t, err)
		require.NotNil(t, trust.ClientConfig().RootCAs)
		require.Empty(t, trust.ClientConfig().ServerName, "the default server name is the URL host")
	})
}

func TestTLSKey(t *testing.T) {
	t.Parallel()

	caA, caB := tlstest.NewCA(t), tlstest.NewCA(t)
	newTLS := func(caBundle []byte, serverName string) *controlplaneurl.TLS {
		trust, err := controlplaneurl.NewTLS(caBundle, serverName)
		require.NoError(t, err)
		return trust
	}
	require.Equal(t, newTLS(caA.PEM, "").Key(), newTLS(caA.PEM, "").Key())
	keys := map[string]bool{}
	for _, trust := range []*controlplaneurl.TLS{newTLS(caA.PEM, ""), newTLS(caB.PEM, ""), newTLS(caA.PEM, "coder.example.com"), newTLS(nil, ""), newTLS(nil, "coder.example.com")} {
		require.False(t, keys[trust.Key()], "trust %q shares a key with another trust", trust.Key())
		keys[trust.Key()] = true
	}
}
