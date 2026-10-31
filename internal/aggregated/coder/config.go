// Package coder provides shared Coder backend helpers for the aggregated API server.
package coder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/coder/v2/codersdk"

	"github.com/coder/coder-k8s/internal/controlplaneurl"
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
// host, and port) and TLS trust. Separate transports keep the per-deployment limits
// separate even when an HTTP proxy would make deployments share one connection-pool key.
// Clients stay per request: the session token is set on each client, never on a
// transport.
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

// forURL returns the transport for the deployment that serves coderURL, verified with
// trust (nil: the system roots). A changed trust, such as a rotated CA bundle, gets a new
// transport, so no connection verified with the old trust serves it. forURL also drops
// the transports without a request for transportIdleEviction, so deployments whose
// control plane changed its URL or trust or went away do not stay in the registry.
func (r *transportRegistry) forURL(coderURL *url.URL, trust *controlplaneurl.TLS) (*registeredTransport, error) {
	if r == nil {
		return nil, fmt.Errorf("assertion failed: transport registry must not be nil")
	}
	if coderURL == nil {
		return nil, fmt.Errorf("assertion failed: coder URL must not be nil")
	}

	key := sameorigin.Key(coderURL)
	if trust != nil {
		key += " " + trust.Key()
	}

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
	if trust != nil {
		transport.TLSClientConfig = trust.ClientConfig()
		transport.Proxy = refuseHTTPSProxy(transport.Proxy)
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
	sent := withLowercaseHost(req)
	current := t.begin()
	res, err := current.transport.RoundTrip(sent)
	if err != nil {
		current.end()
		return nil, err
	}
	res.Request = req

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

// withLowercaseHost returns req with the URL hostname in ASCII lowercase, because
// http.Transport keys its per-host connection limit by the hostname as written. The
// Host header keeps the original spelling, and an IPv6 zone keeps its case because
// interface names are case-sensitive.
func withLowercaseHost(req *http.Request) *http.Request {
	host, zone, hasZone := strings.Cut(req.URL.Host, "%")
	lower := []byte(host)
	for i, c := range lower {
		if 'A' <= c && c <= 'Z' {
			lower[i] = c + ('a' - 'A')
		}
	}
	if string(lower) == host {
		return req
	}

	sent := req.Clone(req.Context())
	sent.URL.Host = string(lower)
	if hasZone {
		sent.URL.Host += "%" + zone
	}
	if sent.Host == "" {
		sent.Host = req.URL.Host
	}
	return sent
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
	transport.OnProxyConnectResponse = refuseFailedProxyConnect

	return transport, nil
}

// errHTTPSProxyWithTrust marks a request with a TLS trust that would go through an https://
// proxy. net/http verifies the proxy with the TLSClientConfig of the transport too, so the
// trust for Coder would also apply to the proxy, and the Coder server name would go to it.
var errHTTPSProxyWithTrust = errors.New("an https:// proxy cannot carry a Coder request with a TLS trust; exclude the Coder host with NO_PROXY or use an http:// proxy")

// refuseHTTPSProxy wraps proxy so that it refuses https:// proxies (see errHTTPSProxyWithTrust).
// An http:// proxy stays: it tunnels the TLS connection to Coder with CONNECT.
func refuseHTTPSProxy(proxy func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	if proxy == nil {
		return nil
	}
	return func(req *http.Request) (*url.URL, error) {
		proxyURL, err := proxy(req)
		if err == nil && proxyURL != nil && proxyURL.Scheme == "https" {
			return nil, fmt.Errorf("%w: %s", errHTTPSProxyWithTrust, proxyURL.Redacted())
		}
		return proxyURL, err
	}
}

// errProxyConnectRefused marks a proxy answer other than 200 to the CONNECT for Coder. Go reports
// that answer with an untyped error, and without this mark NotSent cannot tell it apart from a
// connection that broke after the request.
var errProxyConnectRefused = errors.New("the proxy refused the CONNECT tunnel")

func refuseFailedProxyConnect(_ context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", errProxyConnectRefused, resp.Status)
	}
	return nil
}

// newCoderHTTPClient returns the HTTP client of NewSDKClient for the Coder origin coderURL.
func newCoderHTTPClient(coderURL *url.URL, transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{
		// codersdk dials websockets (WorkspaceBuildLogsAfter for ?follow=true)
		// with a new http.Client that keeps only this Transport, not
		// CheckRedirect, so the Transport also refuses other origins.
		Transport: markMaybeSent{next: sameorigin.RoundTripper(coderURL, transport)},
		Timeout:   timeout,
		// net/http copies Coder-Session-Token to every redirect target, so refuse
		// redirects that leave the Coder origin.
		CheckRedirect: sameorigin.RedirectPolicy(coderURL),
	}
}

// errAfterRedirect marks the error of a request that follows a redirect. The server that answered
// with the redirect got the write, so a follow-up that fails before its own send does not make
// the write certain.
var errAfterRedirect = errors.New("the request after a redirect failed")

// errAfterWrite marks the error of a request whose headers net/http wrote. A later error of a new
// connection does not make the write certain: the HTTP/2 client sends a request again on a new
// connection after the server reset its stream with PROTOCOL_ERROR, which it can do after it read
// the request.
var errAfterWrite = errors.New("the request failed after net/http wrote it")

// markMaybeSent marks the errors of requests that can have reached Coder with errAfterRedirect or
// errAfterWrite.
type markMaybeSent struct{ next http.RoundTripper }

func (m markMaybeSent) RoundTrip(req *http.Request) (*http.Response, error) {
	var wrote atomic.Bool
	traced := req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteHeaders: func() { wrote.Store(true) },
	}))
	resp, err := m.next.RoundTrip(traced)
	switch {
	case err == nil:
		return resp, nil
	case req.Response != nil:
		return nil, fmt.Errorf("%w: %w", errAfterRedirect, err)
	case wrote.Load():
		return nil, fmt.Errorf("%w: %w", errAfterWrite, err)
	}
	return nil, err
}

func (m markMaybeSent) CloseIdleConnections() {
	if closer, ok := m.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// Config describes how to construct a Coder SDK client.
type Config struct {
	CoderURL       *url.URL
	SessionToken   string
	RequestTimeout time.Duration
	// TLS verifies an HTTPS CoderURL. Nil uses the system roots.
	TLS *controlplaneurl.TLS
}

// NewSDKClient creates a configured Coder SDK client from cfg.
func NewSDKClient(cfg Config) (*codersdk.Client, error) {
	if cfg.CoderURL == nil {
		return nil, fmt.Errorf("assertion failed: coder URL must not be nil")
	}
	if cfg.SessionToken == "" {
		return nil, fmt.Errorf("assertion failed: session token must not be empty")
	}
	if cfg.TLS != nil && cfg.CoderURL.Scheme != "https" {
		return nil, fmt.Errorf("assertion failed: a TLS trust needs an HTTPS coder URL, not %q", cfg.CoderURL.Scheme)
	}

	requestTimeout := cfg.RequestTimeout
	switch {
	case requestTimeout < 0:
		return nil, fmt.Errorf("assertion failed: request timeout must not be negative")
	case requestTimeout == 0:
		requestTimeout = defaultRequestTimeout
	}

	coderURL := *cfg.CoderURL
	transport, err := coderTransports.forURL(&coderURL, cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("get Coder transport: %w", err)
	}

	client := codersdk.New(&coderURL, codersdk.WithHTTPClient(newCoderHTTPClient(&coderURL, connectionWaitRoundTripper{base: transport}, requestTimeout)))
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
