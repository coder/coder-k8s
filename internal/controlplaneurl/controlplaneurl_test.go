package controlplaneurl_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controlplaneurl"
)

func TestCoderAPIURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusURL  string
		port       int32
		tlsSecrets []string
		want       string
	}{
		{name: "HTTP status URL is kept", statusURL: "http://coder.team-a.svc.cluster.local:80", want: "http://coder.team-a.svc.cluster.local:80"},
		{name: "TLS uses the internal HTTP URL", statusURL: "https://coder.team-a.svc.cluster.local:443", tlsSecrets: []string{"coder-tls"}, want: "http://coder.team-a.svc.cluster.local:80"},
		{name: "TLS keeps a custom HTTP service port", statusURL: "https://coder.team-a.svc.cluster.local:443", port: 8080, tlsSecrets: []string{"coder-tls"}, want: "http://coder.team-a.svc.cluster.local:8080"},
		{name: "TLS on service port 443 uses the default HTTP port", statusURL: "https://coder.team-a.svc.cluster.local:443", port: 443, tlsSecrets: []string{"coder-tls"}, want: "http://coder.team-a.svc.cluster.local:80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			controlPlane := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
				Spec: coderv1alpha1.CoderControlPlaneSpec{
					Service: coderv1alpha1.ServiceSpec{Port: tt.port},
					TLS:     coderv1alpha1.TLSSpec{SecretNames: tt.tlsSecrets},
				},
				Status: coderv1alpha1.CoderControlPlaneStatus{URL: tt.statusURL},
			}
			got, err := controlplaneurl.CoderAPIURL(controlPlane)
			require.NoError(t, err)
			require.Equal(t, tt.want, got.String())
		})
	}
}

func TestCoderAPIURL_Errors(t *testing.T) {
	t.Parallel()

	_, err := controlplaneurl.CoderAPIURL(nil)
	require.ErrorContains(t, err, "assertion failed")

	_, err = controlplaneurl.CoderAPIURL(&coderv1alpha1.CoderControlPlane{Status: coderv1alpha1.CoderControlPlaneStatus{URL: "://bad"}})
	require.ErrorContains(t, err, `parse "://bad"`)
}

func TestCoderAPIURLWithOperatorTLS(t *testing.T) {
	t.Parallel()

	controlPlane := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			TLS: coderv1alpha1.TLSSpec{
				SecretNames:    []string{"coder-tls"},
				OperatorAccess: &coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com"},
			},
		},
		Status: coderv1alpha1.CoderControlPlaneStatus{URL: "https://coder.team-a.svc.cluster.local:443"},
	}
	got, err := controlplaneurl.CoderAPIURL(controlPlane)
	require.NoError(t, err)
	require.Equal(t, "https://coder.team-a.svc.cluster.local:443", got.String())

	for _, statusURL := range []string{"", "http://coder.team-a.svc.cluster.local:80", "https:///no-host"} {
		controlPlane.Status.URL = statusURL
		got, err = controlplaneurl.CoderAPIURL(controlPlane)
		require.Truef(t, errors.Is(err, controlplaneurl.ErrUnavailable), "status.url %q: got %v, %v; want no URL, never the plain HTTP one", statusURL, got, err)
	}
}
