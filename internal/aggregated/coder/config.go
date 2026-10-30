// Package coder provides shared Coder backend helpers for the aggregated API server.
package coder

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// transportIdleEviction is how long a deployment's transport must stay without requests
// before the registry drops it. It is far longer than IdleConnTimeout, so an evicted
// transport has no idle connections left.
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

// registeredTransport counts its requests in flight, so that the registry never drops a
// transport that still holds connections. Two transports for one deployment would allow
// twice maxConnsPerCoderHost connections.
type registeredTransport struct {
	registry  *transportRegistry
	key       string
	transport *http.Transport

	// Guarded by registry.mu.
	active   int
	lastUsed time.Time
}

func newTransportRegistry(newTransport func() (*http.Transport, error)) *transportRegistry {
	return &transportRegistry{
		newTransport: newTransport,
		now:          time.Now,
		byDeployment: make(map[string]*registeredTransport),
	}
}

// forURL returns the transport for the deployment that serves coderURL. It also drops
// the transports without a request for transportIdleEviction, so deployments whose
// control plane changed its URL or went away do not stay in the registry.
func (r *transportRegistry) forURL(coderURL *url.URL) (*registeredTransport, error) {
	if r == nil {
		return nil, fmt.Errorf("assertion failed: transport registry must not be nil")
	}
	if coderURL == nil {
		return nil, fmt.Errorf("assertion failed: coder URL must not be nil")
	}

	key := sameorigin.Key(coderURL)

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	for otherKey, entry := range r.byDeployment {
		if otherKey != key && entry.active == 0 && now.Sub(entry.lastUsed) > transportIdleEviction {
			delete(r.byDeployment, otherKey)
			entry.transport.CloseIdleConnections()
		}
	}

	if entry, ok := r.byDeployment[key]; ok {
		entry.lastUsed = now
		return entry, nil
	}
	transport, err := r.newTransport()
	if err != nil {
		return nil, err
	}
	if transport == nil {
		return nil, fmt.Errorf("assertion failed: new Coder transport is nil after successful construction")
	}
	entry := &registeredTransport{registry: r, key: key, transport: transport, lastUsed: now}
	r.byDeployment[key] = entry

	return entry, nil
}

// RoundTrip sends the request through the deployment's current entry, so a client that
// outlived the eviction of its entry does not open a second pool. It counts the request
// as active until its response body is closed, because a connection stays in use while
// the body (or an upgraded websocket) is open.
func (t *registeredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	current := t.begin()
	res, err := current.transport.RoundTrip(req)
	if err != nil {
		current.end()
		return nil, err
	}

	done := sync.OnceFunc(current.end)
	// codersdk's websocket dial needs the upgraded body to stay an io.ReadWriteCloser.
	if rwc, ok := res.Body.(io.ReadWriteCloser); ok {
		res.Body = &activeReadWriteCloser{ReadWriteCloser: rwc, done: done}
	} else {
		res.Body = &activeBody{ReadCloser: res.Body, done: done}
	}
	return res, nil
}

// CloseIdleConnections closes the idle connections of the deployment's current entry,
// the pool that RoundTrip uses. An evicted entry has none left.
func (t *registeredTransport) CloseIdleConnections() {
	r := t.registry
	r.mu.Lock()
	current, ok := r.byDeployment[t.key]
	r.mu.Unlock()
	if !ok {
		current = t
	}
	current.transport.CloseIdleConnections()
}

// begin returns the registry's entry for t's deployment, with the request counted. It
// puts t back when the deployment has no entry.
func (t *registeredTransport) begin() *registeredTransport {
	r := t.registry
	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.byDeployment[t.key]
	if !ok {
		current = t
		r.byDeployment[t.key] = t
	}
	current.active++
	current.lastUsed = r.now()
	return current
}

func (t *registeredTransport) end() {
	t.registry.mu.Lock()
	defer t.registry.mu.Unlock()

	t.active--
	t.lastUsed = t.registry.now()
}

type activeBody struct {
	io.ReadCloser
	done func()
}

func (b *activeBody) Close() error {
	defer b.done()
	return b.ReadCloser.Close()
}

type activeReadWriteCloser struct {
	io.ReadWriteCloser
	done func()
}

func (b *activeReadWriteCloser) Close() error {
	defer b.done()
	return b.ReadWriteCloser.Close()
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
