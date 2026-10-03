package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func TestTemplateTestCoderURL(t *testing.T) {
	t.Parallel()
	cp := &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns"}}
	cp.Status.URL = "http://127.0.0.1:8080"
	got, err := templateTestCoderURL(cp)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8080", got.String(), "an HTTP status URL stays unchanged")

	// With TLS, certificates rarely cover the service name: use internal HTTP.
	cp.Spec.TLS.SecretNames, cp.Spec.Service.Port = []string{"coder-tls"}, 443
	cp.Status.URL = "https://coder.ns.svc.cluster.local:443"
	got, err = templateTestCoderURL(cp)
	require.NoError(t, err)
	require.Equal(t, "http://coder.ns.svc.cluster.local:80", got.String())
}
