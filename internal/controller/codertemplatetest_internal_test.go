package controller

import (
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"testing"

	"github.com/coder/coder/v2/codersdk"

	"github.com/stretchr/testify/require"
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
