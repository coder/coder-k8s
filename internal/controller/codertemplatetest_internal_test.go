package controller

import (
	"crypto/tls"
	"io"
	"net"
	"testing"

	"github.com/coder/coder/v2/codersdk"

	"github.com/stretchr/testify/require"
)

func TestCreateHadNoEffect(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]bool{
		codersdk.NewTestError(429, "POST", "/"):               true,
		codersdk.NewTestError(401, "POST", "/"):               true,
		codersdk.NewTestError(406, "POST", "/"):               true,
		codersdk.NewTestError(502, "POST", "/"):               false,
		codersdk.NewTestError(409, "POST", "/"):               false,
		&net.OpError{Op: "dial", Err: io.ErrUnexpectedEOF}:    true,
		&net.OpError{Op: "read", Err: io.ErrUnexpectedEOF}:    false,
		tls.RecordHeaderError{Msg: "first record is not TLS"}: true,
		io.EOF: false,
	} {
		require.Equal(t, want, createHadNoEffect(err), "%v", err)
	}
}
