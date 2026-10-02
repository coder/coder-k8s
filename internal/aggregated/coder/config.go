// Package coder provides shared Coder backend helpers for the aggregated API server.
package coder

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

// coderTransports carries every Coder SDK request of the aggregated API server.
var coderTransports = newTransportRegistry(newCoderTransport)

// transportRegistry holds one long-lived transport per Coder deployment (URL scheme and
// host). Separate transports keep the per-deployment limits separate even when an HTTP
// proxy would make deployments share one connection-pool key. Clients stay per request:
// the session token is set on each client, never on a transport.
type transportRegistry struct {
	newTransport func() (*http.Transport, error)

	mu           sync.Mutex
	byDeployment map[string]*http.Transport
}

func newTransportRegistry(newTransport func() (*http.Transport, error)) *transportRegistry {
	return &transportRegistry{
		newTransport: newTransport,
		byDeployment: make(map[string]*http.Transport),
	}
}

// forURL returns the transport for the deployment that serves coderURL.
// Entries are never evicted: an unused transport closes its idle connections after
// IdleConnTimeout and keeps no sockets.
func (r *transportRegistry) forURL(coderURL *url.URL) (*http.Transport, error) {
	if r == nil {
		return nil, fmt.Errorf("assertion failed: transport registry must not be nil")
	}
	if coderURL == nil {
		return nil, fmt.Errorf("assertion failed: coder URL must not be nil")
	}

	key := strings.ToLower(coderURL.Scheme) + "://" + strings.ToLower(coderURL.Host)

	r.mu.Lock()
	defer r.mu.Unlock()

	if transport, ok := r.byDeployment[key]; ok {
		return transport, nil
	}
	transport, err := r.newTransport()
	if err != nil {
		return nil, err
	}
	if transport == nil {
		return nil, fmt.Errorf("assertion failed: new Coder transport is nil after successful construction")
	}
	r.byDeployment[key] = transport

	return transport, nil
}

func newCoderTransport() (*http.Transport, error) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("assertion failed: http.DefaultTransport is not *http.Transport")
	}

	// Clone keeps the default proxy, dial, TLS handshake, and idle timeouts.
	transport := defaultTransport.Clone()
	transport.MaxConnsPerHost = maxConnsPerCoderHost
	transport.MaxIdleConnsPerHost = maxConnsPerCoderHost
	transport.MaxIdleConns = maxConnsPerCoderHost

	return transport, nil
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

	coderURL := *cfg.CoderURL
	transport, err := coderTransports.forURL(&coderURL)
	if err != nil {
		return nil, fmt.Errorf("get Coder transport: %w", err)
	}

	client := codersdk.New(&coderURL, codersdk.WithHTTPClient(&http.Client{
		Transport: transport,
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
