package coderbootstrap

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSameOriginRedirectPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		origin  string
		target  string
		allowed bool
	}{
		{name: "same origin", origin: "https://coder.example.com", target: "https://coder.example.com/api/v2/x", allowed: true},
		{name: "explicit default https port", origin: "https://coder.example.com", target: "https://coder.example.com:443/api/v2/x", allowed: true},
		{name: "explicit default http port in origin", origin: "http://coder.example.com:80", target: "http://coder.example.com/api/v2/x", allowed: true},
		{name: "host case differs", origin: "https://Coder.Example.com", target: "https://coder.example.com/api/v2/x", allowed: true},
		{name: "different host", origin: "https://coder.example.com", target: "https://attacker.example.com/api/v2/x"},
		{name: "different port", origin: "https://coder.example.com", target: "https://coder.example.com:8443/api/v2/x"},
		{name: "different scheme", origin: "http://coder.example.com", target: "https://coder.example.com/api/v2/x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			origin, err := url.Parse(tt.origin)
			require.NoError(t, err)
			target, err := url.Parse(tt.target)
			require.NoError(t, err)

			err = sameOriginRedirectPolicy(origin)(&http.Request{URL: target}, []*http.Request{{URL: origin}})
			if tt.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "refusing redirect to a different origin")
			}
		})
	}
}
