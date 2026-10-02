// Package coder provides shared Coder backend helpers for the aggregated API server.
package coder

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/coder/v2/codersdk"
)

const (
	defaultRequestTimeout = 30 * time.Second

	// maxConnsPerCoderHost bounds the connections to one Coder deployment. It is also the
	// idle limit, so a connection is closed only when it stays idle for IdleConnTimeout.
	// A lower idle limit (Go's default is 2) closes the extra connections after every
	// request burst and exhausts local ports with TIME_WAIT sockets (issue #181).
	maxConnsPerCoderHost = 128
)

// sharedTransport carries every Coder SDK request of the aggregated API server.
// http.Transport pools connections per scheme and host, so each Coder deployment gets
// its own pool of at most maxConnsPerCoderHost connections. Clients stay per request:
// the session token is set on each client, never on the transport.
var sharedTransport = newSharedTransport()

func newSharedTransport() *http.Transport {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("assertion failed: http.DefaultTransport is not *http.Transport")
	}

	// Clone keeps the default proxy, dial, TLS handshake, and idle timeouts.
	transport := defaultTransport.Clone()
	transport.MaxConnsPerHost = maxConnsPerCoderHost
	transport.MaxIdleConnsPerHost = maxConnsPerCoderHost
	// No cross-host idle cap: the per-host limit and IdleConnTimeout bound it.
	transport.MaxIdleConns = 0

	return transport
}

// Config describes how to construct a Coder SDK client.
type Config struct {
	CoderURL       *url.URL
	SessionToken   string
	RequestTimeout time.Duration
}

// NewSDKClient creates a configured Coder SDK client from cfg.
func NewSDKClient(cfg Config) (*codersdk.Client, error) {
	if cfg.CoderURL == nil {
		return nil, fmt.Errorf("assertion failed: coder URL must not be nil")
	}
	if cfg.SessionToken == "" {
		return nil, fmt.Errorf("assertion failed: session token must not be empty")
	}

	requestTimeout := cfg.RequestTimeout
	switch {
	case requestTimeout < 0:
		return nil, fmt.Errorf("assertion failed: request timeout must not be negative")
	case requestTimeout == 0:
		requestTimeout = defaultRequestTimeout
	}

	if sharedTransport == nil {
		return nil, fmt.Errorf("assertion failed: shared Coder transport must not be nil")
	}

	coderURL := *cfg.CoderURL
	client := codersdk.New(&coderURL, codersdk.WithHTTPClient(&http.Client{
		Transport: sharedTransport,
		Timeout:   requestTimeout,
	}))
	if client == nil {
		return nil, fmt.Errorf("assertion failed: coder SDK client is nil after successful construction")
	}
	if client.HTTPClient == nil {
		return nil, fmt.Errorf("assertion failed: coder SDK HTTP client is nil after successful construction")
	}

	client.SetSessionToken(cfg.SessionToken)
	if client.SessionToken() == "" {
		return nil, fmt.Errorf("assertion failed: coder SDK session token is empty after successful configuration")
	}

	return client, nil
}
