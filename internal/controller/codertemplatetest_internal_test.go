package controller

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

func TestCreateHadNoEffect(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]bool{
		codersdk.NewTestError(429, "POST", "/"):            true,
		codersdk.NewTestError(401, "POST", "/"):            true,
		codersdk.NewTestError(406, "POST", "/"):            true,
		codersdk.NewTestError(502, "POST", "/"):            false,
		codersdk.NewTestError(409, "POST", "/"):            false,
		&net.OpError{Op: "dial", Err: io.ErrUnexpectedEOF}: true,
		&net.OpError{Op: "read", Err: io.ErrUnexpectedEOF}: false,
		&tls.CertificateVerificationError{Err: io.EOF}:     true,
		io.EOF: false,
	} {
		require.Equal(t, want, createHadNoEffect(err), "%v", err)
	}
}

// Only a bad first record proves that no request bytes were sent. On a
// reused connection, crypto/tls returns the same error type with a nil Conn
// after Coder may already have committed the request.
func TestCreateHadNoEffectTLSRecordHeader(t *testing.T) {
	t.Parallel()
	conn, peer := net.Pipe()
	t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
	firstRecord := tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake", Conn: conn}
	laterRecord := tls.RecordHeaderError{Msg: "received record with version 301 when expecting version 303"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "first record", err: firstRecord, want: true},
		{name: "first record from the HTTP client", err: &url.Error{Op: "Post", URL: "https://coder.example", Err: firstRecord}, want: true},
		{name: "later record", err: laterRecord, want: false},
		{name: "later record from the HTTP client", err: &url.Error{Op: "Post", URL: "https://coder.example", Err: laterRecord}, want: false},
	} {
		require.Equal(t, tc.want, createHadNoEffect(tc.err), tc.name)
	}
}

// A create that Coder got is never without effect, even when the error is a
// refused dial: here the server takes the create, answers with a same-origin
// redirect, and stops listening, so the request after the redirect cannot
// connect.
func TestCreateHadNoEffectRedirectThenRefusedDial(t *testing.T) {
	t.Parallel()
	var received atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_ = server.Listener.Close()
		w.Header().Set("Connection", "close")
		http.Redirect(w, r, "/api/v2/moved", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	sdk, err := coder.NewSDKClient(coder.Config{CoderURL: serverURL, SessionToken: "token", RequestTimeout: 5 * time.Second})
	require.NoError(t, err)

	_, err = sdk.CreateUserWorkspace(context.Background(), codersdk.Me, codersdk.CreateWorkspaceRequest{TemplateVersionID: uuid.New(), Name: "kwp-test"})
	require.Error(t, err)
	var opErr *net.OpError
	require.ErrorAs(t, err, &opErr, "the request after the redirect fails to dial")
	require.Equal(t, "dial", opErr.Op)
	require.EqualValues(t, 1, received.Load(), "the server got the create")
	require.False(t, createHadNoEffect(err), "%v", err)
}
