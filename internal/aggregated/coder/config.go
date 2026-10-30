// Package coder provides shared Coder backend helpers for the aggregated API server.
package coder

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/coder/v2/codersdk"

	"github.com/coder/coder-k8s/internal/sameorigin"
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

// transportIdleEviction is how long a deployment's transport stays unused before the
// registry drops it. It is far longer than IdleConnTimeout and the default request timeout, so
// an evicted transport has no idle connections and no regular request in flight.
const transportIdleEviction = 10 * time.Minute

// transportRegistry holds one long-lived transport per Coder deployment (URL scheme,
// host, and port). Separate transports keep the per-deployment limits separate even
// when an HTTP proxy would make deployments share one connection-pool key. Clients stay
// per request: the session token is set on each client, never on a transport.
type transportRegistry struct {
	newTransport func() (*http.Transport, error)
	now          func() time.Time

	mu           sync.Mutex
	byDeployment map[string]*registeredTransport
}

type registeredTransport struct {
	transport *http.Transport
	lastUsed  time.Time
}

func newTransportRegistry(newTransport func() (*http.Transport, error)) *transportRegistry {
	return &transportRegistry{
		newTransport: newTransport,
		now:          time.Now,
		byDeployment: make(map[string]*registeredTransport),
	}
}

// forURL returns the transport for the deployment that serves coderURL. It also drops
// the transports that no request used for transportIdleEviction, so deployments whose
// control plane changed its URL or went away do not stay in the registry.
func (r *transportRegistry) forURL(coderURL *url.URL) (*http.Transport, error) {
	if r == nil {
		return nil, fmt.Errorf("assertion failed: transport registry must not be nil")
	}
	if coderURL == nil {
		return nil, fmt.Errorf("assertion failed: coder URL must not be nil")
	}

	key := deploymentKey(coderURL)

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	for otherKey, entry := range r.byDeployment {
		if otherKey != key && now.Sub(entry.lastUsed) > transportIdleEviction {
			delete(r.byDeployment, otherKey)
			entry.transport.CloseIdleConnections()
		}
	}

	if entry, ok := r.byDeployment[key]; ok {
		entry.lastUsed = now
		return entry.transport, nil
	}
	transport, err := r.newTransport()
	if err != nil {
		return nil, err
	}
	if transport == nil {
		return nil, fmt.Errorf("assertion failed: new Coder transport is nil after successful construction")
	}
	r.byDeployment[key] = &registeredTransport{transport: transport, lastUsed: now}

	return transport, nil
}

// deploymentKey identifies a deployment by scheme, host, and port, with the scheme's
// default port filled in, so https://x and https://x:443 share one transport.
func deploymentKey(coderURL *url.URL) string {
	scheme := strings.ToLower(coderURL.Scheme)
	port := coderURL.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}

	return scheme + "://" + net.JoinHostPort(strings.ToLower(coderURL.Hostname()), port)
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
		// codersdk dials websockets (WorkspaceBuildLogsAfter for ?follow=true)
		// with a new http.Client that keeps only this Transport, not
		// CheckRedirect, so the Transport also refuses other origins.
		Transport: sameorigin.RoundTripper(&coderURL, connectionWaitRoundTripper{base: transport}),
		Timeout:   requestTimeout,
		// net/http copies Coder-Session-Token to every redirect target, so refuse
		// redirects that leave the Coder origin.
		CheckRedirect: sameorigin.RedirectPolicy(&coderURL),
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
