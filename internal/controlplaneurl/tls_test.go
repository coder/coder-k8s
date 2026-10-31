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

func TestNewTLS(t *testing.T) {
	t.Parallel()

	ca := tlstest.NewCA(t)
	trust, err := controlplaneurl.NewTLS(ca.PEM, "")
	require.NoError(t, err)
	require.NotNil(t, trust.ClientConfig().RootCAs)
	require.Empty(t, trust.ClientConfig().ServerName, "the default server name is the URL host")

	trust, err = controlplaneurl.NewTLS(nil, "coder.example.com")
	require.NoError(t, err)
	require.Nil(t, trust.ClientConfig().RootCAs, "a nil bundle uses the system roots")
	require.Equal(t, "coder.example.com", trust.ClientConfig().ServerName)

	truncated := ca.PEM[:len(ca.PEM)/2]
	invalid := []struct {
		name     string
		caBundle string
		want     string
	}{
		{name: "empty bundle", caBundle: "", want: "no PEM certificate"},
		{name: "not PEM", caBundle: "not a certificate", want: "the bundle does not start with a PEM block"},
		{name: "text before a valid certificate", caBundle: "MIIB-a-cut-off-certificate\n" + string(ca.PEM), want: "the bundle does not start with a PEM block"},
		{name: "text between valid certificates", caBundle: string(ca.PEM) + "stray text\n" + string(ca.PEM), want: "the data after certificate 1 is not a PEM block"},
		{name: "undecodable block before a valid certificate", caBundle: "-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n" + string(ca.PEM), want: "the bundle does not start with a PEM block"},
		{name: "undecodable block between valid certificates", caBundle: string(ca.PEM) + "-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n" + string(ca.PEM), want: "the data after certificate 1 is not a PEM block"},
		{name: "private key block", caBundle: string(ca.PEM) + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", want: `PEM block 2 is "PRIVATE KEY", not a CERTIFICATE`},
		{name: "broken certificate", caBundle: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", want: "parse certificate 1"},
		{name: "cut-off certificate after a valid one", caBundle: string(ca.PEM) + string(truncated), want: "the data after certificate 1 is not a PEM block"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			trust, err := controlplaneurl.NewTLS([]byte(tt.caBundle), "")
			require.Nil(t, trust)
			require.ErrorContains(t, err, tt.want)
		})
	}
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

	trust, err := controlplaneurl.OperatorTLS(context.Background(), nil, controlPlane(nil))
	require.NoError(t, err)
	require.Nil(t, trust, "no trust without operatorAccess")

	trust, err = controlplaneurl.OperatorTLS(context.Background(), nil, controlPlane(&coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com"}))
	require.NoError(t, err)
	require.Nil(t, trust.ClientConfig().RootCAs, "the system roots without caBundleRef")
	require.Equal(t, "coder.example.com", trust.ClientConfig().ServerName)

	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(caConfigMap("team-a", map[string]string{"ca.crt": string(ca.PEM)})).Build()
	trust, err = controlplaneurl.OperatorTLS(context.Background(), reader, controlPlane(withCABundle))
	require.NoError(t, err)
	want, err := controlplaneurl.NewTLS(ca.PEM, "")
	require.NoError(t, err)
	require.Equal(t, want.Key(), trust.Key(), "the trust holds the bundle of the ConfigMap key")

	unavailable := []struct {
		name    string
		objects []client.Object
		want    string
	}{
		{name: "ConfigMap missing", want: `CA bundle ConfigMap "coder-ca" does not exist`},
		{name: "ConfigMap in another namespace", objects: []client.Object{caConfigMap("team-b", map[string]string{"ca.crt": string(ca.PEM)})}, want: `CA bundle ConfigMap "coder-ca" does not exist`},
		{name: "key missing", objects: []client.Object{caConfigMap("team-a", map[string]string{"other": string(ca.PEM)})}, want: `CA bundle ConfigMap "coder-ca" has no key "ca.crt"`},
		{name: "invalid bundle", objects: []client.Object{caConfigMap("team-a", map[string]string{"ca.crt": "not a certificate"})}, want: `CA bundle ConfigMap "coder-ca" key "ca.crt": the bundle does not start with a PEM block`},
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
}
