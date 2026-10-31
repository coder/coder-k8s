package coder

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/tlstest"
)

// countingCoder answers every request with 200 and records the client
// address of the requests that carry the session token.
type countingCoder struct {
	mu      sync.Mutex
	clients []string
}

func (c *countingCoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Coder-Session-Token") == "operator-token" {
		c.mu.Lock()
		c.clients = append(c.clients, r.RemoteAddr)
		c.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (c *countingCoder) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.clients)
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
	// Read to the end, so that the connection goes back to the pool.
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
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
				require.Len(t, coder.requests(), 1)
				return
			}
			requireCertificateRejected(t, err)
			require.Empty(t, coder.requests(), "the operator token must not reach a server that failed verification")
		})
	}
}

// Clients with the same trust share pooled connections. A changed trust, such
// as a rotated CA bundle, must never get a connection verified with the old
// one.
func TestNewSDKClient_NewTrustGetsNewConnections(t *testing.T) {
	t.Parallel()

	ca := tlstest.NewCA(t)
	coder := &countingCoder{}
	server := ca.NewServer(t, coder, "127.0.0.1")

	require.NoError(t, callCoder(t, server.URL, newTrust(t, ca.PEM, "")))
	require.NoError(t, callCoder(t, server.URL, newTrust(t, ca.PEM, "")))
	require.NoError(t, callCoder(t, server.URL, newTrust(t, ca.PEM, "127.0.0.1")))
	requireCertificateRejected(t, callCoder(t, server.URL, newTrust(t, tlstest.NewCA(t).PEM, "")))

	clients := coder.requests()
	require.Len(t, clients, 3, "the client with the other CA must not reach Coder")
	require.Equal(t, clients[0], clients[1], "the same trust reuses the pooled connection")
	require.NotEqual(t, clients[0], clients[2], "another server name gets its own connection")
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
