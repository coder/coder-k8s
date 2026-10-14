package controller

import (
	"testing"

	"github.com/stretchr/testify/require"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// Plan S10: Coder sends the API key and header values as HTTP header values,
// so each byte must be valid in an RFC 9110 field value.
func TestIsHTTPFieldValue(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"Bearer abc.DEF-123_~":  true,
		"a\tb c":                true, // HTAB and SP.
		"!\x7e":                 true, // Visible ASCII bounds.
		"caf\xc3\xa9 \x80\xff":  true, // obs-text.
		"a\rb":                  false,
		"a\nb":                  false,
		"a\x00b":                false,
		"a\x1fb":                false,
		"a\x7fb":                false, // DEL.
		"Bearer x\r\nX-Evil: 1": false,
	} {
		require.Equal(t, want, isHTTPFieldValue(value), "%q", value)
	}
}

// The controller repeats the URL rules of admission, so they hold without it.
func TestMCPServerURLs(t *testing.T) {
	t.Parallel()
	oauth2 := func(revocation string) coderv1alpha1.CoderMCPServerSpec {
		return coderv1alpha1.CoderMCPServerSpec{URL: "https://mcp.example.com/mcp", Auth: coderv1alpha1.CoderMCPServerAuth{Type: "oauth2", OAuth2: &coderv1alpha1.CoderMCPServerOAuth2Auth{
			AuthURL: "https://auth.example.com/a", TokenURL: "https://auth.example.com/t", RevocationURL: revocation,
		}}}
	}
	urls, step := mcpServerURLs(oauth2("https://auth.example.com/r"))
	require.Nil(t, step)
	require.Len(t, urls, 4)
	for name, spec := range map[string]coderv1alpha1.CoderMCPServerSpec{
		"spec.url":                       {URL: "https://user:pass@mcp.example.com/mcp"},
		"spec.auth.oauth2.revocationURL": oauth2("http://auth.example.com/r"),
	} {
		_, step := mcpServerURLs(spec)
		require.NotNil(t, step, name)
		require.Equal(t, credentialsecret.ReasonMalformed, step.reason)
		require.Contains(t, step.message, name+" is not a URL that Coder accepts")
		require.NotContains(t, step.message, "pass", "the message never repeats the URL")
	}
}
