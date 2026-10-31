package coderbootstrap_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/tlstest"
)

func TestSDKClientVerifiesCoderTLS(t *testing.T) {
	t.Parallel()

	ca := tlstest.NewCA(t)
	var requests atomic.Int32
	server := ca.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"features":{}}`))
	}), "127.0.0.1")
	newTrust := func(caBundle []byte) *controlplaneurl.TLS {
		trust, err := controlplaneurl.NewTLS(caBundle, "")
		require.NoError(t, err)
		return trust
	}
	client := coderbootstrap.NewSDKClient()

	_, err := client.Entitlements(context.Background(), server.URL, newTrust(ca.PEM), "operator-token")
	require.NoError(t, err)
	require.EqualValues(t, 1, requests.Load())

	otherCA := newTrust(tlstest.NewCA(t).PEM)
	_, err = client.Entitlements(context.Background(), server.URL, otherCA, "operator-token")
	var verifyErr *tls.CertificateVerificationError
	require.True(t, errors.As(err, &verifyErr), "want a certificate verification error, got %v", err)
	err = client.DeleteProvisionerKeyByID(context.Background(), coderbootstrap.DeleteProvisionerKeyByIDRequest{
		CoderURL: server.URL, CoderTLS: otherCA, SessionToken: "operator-token", OrganizationID: uuid.New(), KeyID: uuid.New(),
	})
	require.True(t, errors.As(err, &verifyErr), "want a certificate verification error, got %v", err)
	require.EqualValues(t, 1, requests.Load(), "a server that failed verification must get no request")
}
