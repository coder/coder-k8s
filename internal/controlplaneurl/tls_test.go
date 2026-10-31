package controlplaneurl_test

import (
	"testing"

	"github.com/stretchr/testify/require"

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
