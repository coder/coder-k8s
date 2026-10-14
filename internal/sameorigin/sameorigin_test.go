package sameorigin_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/sameorigin"
)

func TestRedirectPolicy(t *testing.T) {
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
		{name: "same IPv6 zone", origin: "https://[fe80::1%25eth0]", target: "https://[FE80::1%25eth0]/api/v2/x", allowed: true},
		{name: "IPv6 zone case differs", origin: "https://[fe80::1%25eth0]", target: "https://[fe80::1%25ETH0]/api/v2/x"},
		{name: "Unicode sigma case fold", origin: "https://σ.example", target: "https://ς.example/api/v2/x"},
		{name: "Unicode sharp s case fold", origin: "https://ß.example", target: "https://ẞ.example/api/v2/x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			origin, err := url.Parse(tt.origin)
			require.NoError(t, err)
			target, err := url.Parse(tt.target)
			require.NoError(t, err)

			err = sameorigin.RedirectPolicy(origin)(&http.Request{URL: target}, []*http.Request{{URL: origin}})
			if tt.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, sameorigin.ErrCrossOrigin)
			}
		})
	}
}

func TestRedirectPolicyStopsAfterMaxRedirects(t *testing.T) {
	t.Parallel()

	origin, err := url.Parse("https://coder.example.com")
	require.NoError(t, err)
	via := make([]*http.Request, sameorigin.MaxRedirects)
	err = sameorigin.RedirectPolicy(origin)(&http.Request{URL: origin}, via)
	require.ErrorContains(t, err, "stopped after 10 redirects")
}

// recordingTransport records the requests that reach it and the calls to
// CloseIdleConnections.
type recordingTransport struct {
	requests  []*http.Request
	idleClose int
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests = append(r.requests, req)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func (r *recordingTransport) CloseIdleConnections() { r.idleClose++ }

// closeRecorder is a request body that records whether it was closed.
type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

func TestRoundTripper(t *testing.T) {
	t.Parallel()

	origin, err := url.Parse("https://coder.example.com")
	require.NoError(t, err)

	t.Run("refuses another origin without calling base and closes the body", func(t *testing.T) {
		t.Parallel()

		base := &recordingTransport{}
		body := &closeRecorder{Reader: strings.NewReader("payload")}
		req, err := http.NewRequest(http.MethodPost, "https://attacker.example.com/api/v2/x", body)
		require.NoError(t, err)

		res, err := sameorigin.RoundTripper(origin, base).RoundTrip(req)
		if res != nil {
			_ = res.Body.Close()
		}
		require.ErrorIs(t, err, sameorigin.ErrCrossOriginRequest)
		require.NotContains(t, err.Error(), "redirect")
		require.Nil(t, res)
		require.Empty(t, base.requests, "the base transport must receive no request")
		require.True(t, body.closed, "a refused request must close its body")
	})

	t.Run("passes the same origin through to base", func(t *testing.T) {
		t.Parallel()

		base := &recordingTransport{}
		req, err := http.NewRequest(http.MethodGet, "https://coder.example.com:443/api/v2/x", nil)
		require.NoError(t, err)

		res, err := sameorigin.RoundTripper(origin, base).RoundTrip(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, []*http.Request{req}, base.requests)
	})

	t.Run("forwards CloseIdleConnections", func(t *testing.T) {
		t.Parallel()

		base := &recordingTransport{}
		(&http.Client{Transport: sameorigin.RoundTripper(origin, base)}).CloseIdleConnections()
		require.Equal(t, 1, base.idleClose)
	})
}
