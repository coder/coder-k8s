package coder

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/tlstest"
)

// countingCoder answers every request with 200 and counts the requests that
// carry the session token.
type countingCoder struct{ requests atomic.Int32 }

func (c *countingCoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Coder-Session-Token") == "operator-token" {
		c.requests.Add(1)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func newTrust(t *testing.T, caBundle []byte, serverName string) *controlplaneurl.TLS {
	t.Helper()
	trust, err := controlplaneurl.NewTLS(caBundle, serverName)
	require.NoError(t, err)
	return trust
}

func callCoder(t *testing.T, rawURL string, trust *controlplaneurl.TLS) error {
	t.Helper()
	client, err := NewSDKClient(Config{CoderURL: mustParseURL(t, rawURL), TLS: trust, SessionToken: "operator-token"})
	require.NoError(t, err)
	res, err := client.Request(context.Background(), http.MethodGet, "/api/v2/buildinfo", nil)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

func requireCertificateRejected(t *testing.T, err error) {
	t.Helper()
	var verifyErr *tls.CertificateVerificationError
	require.True(t, errors.As(err, &verifyErr), "want a certificate verification error, got %v", err)
}

func TestNewSDKClient_VerifiesCoderWithTLSTrust(t *testing.T) {
	t.Parallel()

	ca, otherCA := tlstest.NewCA(t), tlstest.NewCA(t)
	tests := []struct {
		name     string
		certFor  string
		trust    *controlplaneurl.TLS
		accepted bool
	}{
		{name: "CA that signs the certificate", certFor: "127.0.0.1", trust: newTrust(t, ca.PEM, ""), accepted: true},
		{name: "other CA", certFor: "127.0.0.1", trust: newTrust(t, otherCA.PEM, "")},
		{name: "system roots", certFor: "127.0.0.1", trust: newTrust(t, nil, "")},
		{name: "server name that the certificate holds", certFor: "coder.example.com", trust: newTrust(t, ca.PEM, "coder.example.com"), accepted: true},
		{name: "URL host that the certificate does not hold", certFor: "coder.example.com", trust: newTrust(t, ca.PEM, "")},
		{name: "server name that the certificate does not hold", certFor: "coder.example.com", trust: newTrust(t, ca.PEM, "other.example.com")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			coder := &countingCoder{}
			server := ca.NewServer(t, coder, tt.certFor)

			err := callCoder(t, server.URL, tt.trust)
			if tt.accepted {
				require.NoError(t, err)
				require.EqualValues(t, 1, coder.requests.Load())
				return
			}
			requireCertificateRejected(t, err)
			require.Zero(t, coder.requests.Load(), "the operator token must not reach a server that failed verification")
		})
	}
}

// A rotated CA bundle must apply to the next client even while a connection
// verified with the old bundle stays open in the pool.
func TestNewSDKClient_NewTrustGetsNewConnections(t *testing.T) {
	t.Parallel()

	ca := tlstest.NewCA(t)
	coder := &countingCoder{}
	server := ca.NewServer(t, coder, "127.0.0.1")

	require.NoError(t, callCoder(t, server.URL, newTrust(t, ca.PEM, "")))
	requireCertificateRejected(t, callCoder(t, server.URL, newTrust(t, tlstest.NewCA(t).PEM, "")))
	require.NoError(t, callCoder(t, server.URL, newTrust(t, ca.PEM, "")))
	require.EqualValues(t, 2, coder.requests.Load())
}

func TestNewSDKClient_RefusesTLSTrustForHTTPURL(t *testing.T) {
	t.Parallel()

	_, err := NewSDKClient(Config{
		CoderURL:     mustParseURL(t, "http://coder.team-a.svc.cluster.local:80"),
		TLS:          newTrust(t, tlstest.NewCA(t).PEM, ""),
		SessionToken: "operator-token",
	})
	require.ErrorContains(t, err, "assertion failed: a TLS trust needs an HTTPS coder URL")
}
