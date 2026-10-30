package coder

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/component-base/metrics/legacyregistry"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// blockingCoderServer holds every request until the test releases the current round,
// so each round needs as many connections as it has concurrent requests.
type blockingCoderServer struct {
	server   *httptest.Server
	newConns atomic.Int64
	arrived  chan string
	release  atomic.Pointer[chan struct{}]
}

func newBlockingCoderServer(t *testing.T) *blockingCoderServer {
	t.Helper()

	s := &blockingCoderServer{arrived: make(chan string, 1024)}
	s.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := s.release.Load()
		if release == nil {
			http.Error(w, "assertion failed: no release channel", http.StatusInternalServerError)
			return
		}
		s.arrived <- r.Header.Get(codersdk.SessionTokenHeader)
		// Also stop on a closed connection, so a failed test can close the server.
		select {
		case <-*release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	s.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			s.newConns.Add(1)
		}
	}
	s.server.Start()
	t.Cleanup(func() {
		s.server.CloseClientConnections()
		s.server.Close()
	})

	return s
}

// startRound sends concurrency requests, each through a fresh SDK client with its own
// token (as ControlPlaneClientProvider does per request), and returns once they all arrived.
func (s *blockingCoderServer) startRound(t *testing.T, concurrency, wantArrivals int) (release, wait func()) {
	t.Helper()

	releaseCh := make(chan struct{})
	s.release.Store(&releaseCh)

	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := range concurrency {
		token := fmt.Sprintf("token-%d", i)
		client, err := NewSDKClient(Config{
			CoderURL:       mustParseURL(t, s.server.URL),
			SessionToken:   token,
			RequestTimeout: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("new SDK client: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := client.Request(context.Background(), http.MethodGet, "/api/v2/buildinfo", nil)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = res.Body.Close() }()
			if _, err := io.Copy(io.Discard, res.Body); err != nil {
				errs <- err
			}
		}()
	}

	seen := make(map[string]bool, concurrency)
	for range wantArrivals {
		select {
		case token := <-s.arrived:
			if seen[token] {
				t.Fatalf("assertion failed: token %q arrived twice in one round", token)
			}
			seen[token] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out after %d of %d arrivals", len(seen), wantArrivals)
		}
	}

	return func() { close(releaseCh) }, func() {
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("request failed: %v", err)
		}
	}
}

func TestNewSDKClientReusesConnectionsAcrossRequests(t *testing.T) {
	t.Parallel()

	const (
		concurrency = 64
		rounds      = 5
	)
	s := newBlockingCoderServer(t)

	for round := range rounds {
		release, wait := s.startRound(t, concurrency, concurrency)
		release()
		wait()
		if got := s.newConns.Load(); got != concurrency {
			t.Fatalf("after round %d: server accepted %d connections, want %d (idle connections must be reused, not closed)", round+1, got, concurrency)
		}
	}
}

func TestNewSDKClientBoundsConnectionsPerHost(t *testing.T) {
	t.Parallel()

	const extra = 8
	s := newBlockingCoderServer(t)

	slowBefore := slowConnectionWaits(t)
	release, wait := s.startRound(t, maxConnsPerCoderHost+extra, maxConnsPerCoderHost)
	select {
	case token := <-s.arrived:
		release()
		wait()
		t.Fatalf("request %q reached the server beyond the %d-connection limit", token, maxConnsPerCoderHost)
	case <-time.After(300 * time.Millisecond):
	}
	if got := s.newConns.Load(); got != maxConnsPerCoderHost {
		t.Fatalf("server accepted %d connections while blocked, want %d", got, maxConnsPerCoderHost)
	}

	release()
	wait()
	if got := s.newConns.Load(); got != maxConnsPerCoderHost {
		t.Fatalf("server accepted %d connections in total, want %d", got, maxConnsPerCoderHost)
	}
	if got := slowConnectionWaits(t) - slowBefore; got < extra {
		t.Fatalf("connection wait metric shows %d waits over %vs, want at least %d", got, slowWaitSeconds, extra)
	}
}

// TestNewSDKClientRecordsConnectionWaitThatTimesOut is not parallel, so no other
// test adds slow connection waits while it counts them.
func TestNewSDKClientRecordsConnectionWaitThatTimesOut(t *testing.T) {
	s := newBlockingCoderServer(t)
	release, wait := s.startRound(t, maxConnsPerCoderHost, maxConnsPerCoderHost)
	defer wait()
	defer release()

	slowBefore := slowConnectionWaits(t)
	client, err := NewSDKClient(Config{
		CoderURL:       mustParseURL(t, s.server.URL),
		SessionToken:   "token-timeout",
		RequestTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new SDK client: %v", err)
	}
	if res, err := client.Request(context.Background(), http.MethodGet, "/api/v2/buildinfo", nil); err == nil {
		_ = res.Body.Close()
		t.Fatalf("request beyond the %d-connection limit succeeded, want a timeout", maxConnsPerCoderHost)
	}
	if got := slowConnectionWaits(t) - slowBefore; got != 1 {
		t.Fatalf("connection wait metric shows %d new waits over %vs, want 1 for the timed-out request", got, slowWaitSeconds)
	}
}

const slowWaitSeconds = 0.1

// slowConnectionWaits returns how many connection waits over slowWaitSeconds the
// controller and the aggregated API server metrics endpoints report, after checking
// that both report the same histogram.
func slowConnectionWaits(t *testing.T) uint64 {
	t.Helper()

	count := func(gatherer prometheus.Gatherer) uint64 {
		families, err := gatherer.Gather()
		if err != nil {
			t.Fatalf("gather metrics: %v", err)
		}
		for _, family := range families {
			if family.GetName() != "coder_k8s_coder_connection_wait_seconds" {
				continue
			}
			histogram := family.GetMetric()[0].GetHistogram()
			for _, bucket := range histogram.GetBucket() {
				if bucket.GetUpperBound() == slowWaitSeconds {
					return histogram.GetSampleCount() - bucket.GetCumulativeCount()
				}
			}
			t.Fatalf("connection wait histogram has no %vs bucket", slowWaitSeconds)
		}
		t.Fatal("connection wait histogram is not registered")
		return 0
	}

	controller := count(metrics.Registry)
	if apiserver := count(legacyregistry.DefaultGatherer); apiserver != controller {
		t.Fatalf("aggregated API server reports %d slow connection waits, controller %d", apiserver, controller)
	}
	return controller
}

func TestTransportRegistrySeparatesDeploymentsBehindProxy(t *testing.T) {
	t.Parallel()

	// An HTTP proxy puts every plain-HTTP target behind one connection-pool key, so a
	// single shared transport would let deployment A use up deployment B's connections.
	release := make(chan struct{})
	arrivedA := make(chan struct{}, maxConnsPerCoderHost+1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "deployment-a.invalid" {
			arrivedA <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(func() {
		proxy.CloseClientConnections()
		proxy.Close()
	})
	proxyURL := mustParseURL(t, proxy.URL)

	registry := newTransportRegistry(func() (*http.Transport, error) {
		transport, err := newCoderTransport()
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
		return transport, nil
	})
	get := func(ctx context.Context, rawURL string) error {
		target := mustParseURL(t, rawURL)
		transport, err := registry.forURL(target)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return err
		}
		res, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = res.Body.Close() }()
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	for range maxConnsPerCoderHost {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := get(context.Background(), "http://deployment-a.invalid/api/v2/buildinfo"); err != nil {
				t.Errorf("deployment A request failed: %v", err)
			}
		}()
	}
	for i := range maxConnsPerCoderHost {
		select {
		case <-arrivedA:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out after %d of %d deployment A arrivals", i, maxConnsPerCoderHost)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := get(ctx, "http://deployment-b.invalid/api/v2/buildinfo"); err != nil {
		t.Fatalf("deployment B request through the same proxy failed while deployment A held %d connections: %v", maxConnsPerCoderHost, err)
	}
	releaseOnce()
}

func TestTransportRegistryKeysByDeployment(t *testing.T) {
	t.Parallel()

	registry := newTransportRegistry(newCoderTransport)
	forURL := func(rawURL string) *registeredTransport {
		transport, err := registry.forURL(mustParseURL(t, rawURL))
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		return transport
	}

	sameDeployment := [][]string{
		{"http://coder.coder.svc.cluster.local", "HTTP://Coder.coder.svc.cluster.local/api/v2", "http://coder.coder.svc.cluster.local:80"},
		{"https://coder.example.com", "https://coder.example.com:443/api/v2"},
		{"http://[fd00::1]", "http://[FD00::1]:80"},
		{"http://[fe80::1%25eth0]", "http://[FE80::1%25eth0]:80"},
	}
	var bases []*registeredTransport
	for _, urls := range sameDeployment {
		base := forURL(urls[0])
		for _, rawURL := range urls[1:] {
			if forURL(rawURL) != base {
				t.Fatalf("expected %q to share the transport of %q", rawURL, urls[0])
			}
		}
		bases = append(bases, base)
	}

	base := bases[0]
	for _, other := range []string{
		"https://coder.coder.svc.cluster.local",
		"https://coder.coder.svc.cluster.local:80",
		"http://coder.coder.svc.cluster.local:443",
		"http://coder.coder.svc.cluster.local:8080",
		"http://coder.other.svc.cluster.local",
	} {
		if forURL(other) == base {
			t.Fatalf("expected %q to get its own transport", other)
		}
	}
	if bases[1] == base || bases[2] == base || bases[1] == bases[2] {
		t.Fatal("expected separate deployments to get separate transports")
	}
	if forURL("http://[fe80::1%25ETH0]") == bases[3] {
		t.Fatal("expected IPv6 zones that differ in case to get separate transports")
	}
}

func TestTransportRegistryEvictsUnusedDeployments(t *testing.T) {
	t.Parallel()

	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	now := time.Unix(0, 0)
	registry := newTransportRegistry(newCoderTransport)
	registry.now = func() time.Time { return now }
	forURL := func(rawURL string) *registeredTransport {
		transport, err := registry.forURL(mustParseURL(t, rawURL))
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		return transport
	}

	unused := forURL(server.URL)
	res, err := (&http.Client{Transport: unused}).Get(server.URL)
	if err != nil {
		t.Fatalf("request through the unused transport: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	inUse := forURL("http://in-use.invalid")
	now = now.Add(transportIdleEviction / 2)
	if forURL("http://in-use.invalid") != inUse {
		t.Fatal("expected a used deployment to keep its transport")
	}
	select {
	case <-closed:
		t.Fatal("the idle connection closed before its deployment went unused for the eviction period")
	default:
	}

	now = now.Add(transportIdleEviction/2 + time.Second)
	forURL("http://other.invalid")
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("expected the evicted transport to close its idle connection")
	}
	if forURL("http://in-use.invalid") != inUse {
		t.Fatal("expected a deployment used within the eviction period to keep its transport")
	}
	if forURL(server.URL) == unused {
		t.Fatal("expected an evicted deployment to get a new transport")
	}
}

func TestTransportRegistryKeepsDeploymentsWithOpenResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	now := time.Unix(0, 0)
	registry := newTransportRegistry(newCoderTransport)
	registry.now = func() time.Time { return now }
	forURL := func(rawURL string) *registeredTransport {
		transport, err := registry.forURL(mustParseURL(t, rawURL))
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		return transport
	}

	open := forURL(server.URL)
	res, err := (&http.Client{Transport: open}).Get(server.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	now = now.Add(2 * transportIdleEviction)
	forURL("http://other.invalid")
	if forURL(server.URL) != open {
		t.Fatal("expected a deployment with an open response to keep its transport")
	}

	now = now.Add(transportIdleEviction / 2)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	now = now.Add(transportIdleEviction/2 + time.Second)
	forURL("http://other.invalid")
	if forURL(server.URL) != open {
		t.Fatal("expected the eviction period to start when the response closed")
	}

	now = now.Add(transportIdleEviction + time.Second)
	forURL("http://other.invalid")
	if forURL(server.URL) == open {
		t.Fatal("expected the deployment to lose its transport once its response closed and the eviction period passed")
	}
}

func TestNewSDKClientCloseIdleConnectionsReachesTransport(t *testing.T) {
	t.Parallel()

	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	client, err := NewSDKClient(Config{CoderURL: mustParseURL(t, server.URL), SessionToken: "token"})
	if err != nil {
		t.Fatalf("new SDK client: %v", err)
	}
	res, err := client.Request(context.Background(), http.MethodGet, "/api/v2/buildinfo", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	client.HTTPClient.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("expected CloseIdleConnections to close the idle connection")
	}
}

func TestTransportRegistryRoutesEvictedTransportToCurrentEntry(t *testing.T) {
	t.Parallel()

	var newConns atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	now := time.Unix(0, 0)
	registry := newTransportRegistry(newCoderTransport)
	registry.now = func() time.Time { return now }
	forURL := func(rawURL string) *registeredTransport {
		transport, err := registry.forURL(mustParseURL(t, rawURL))
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		return transport
	}
	get := func(transport http.RoundTripper) {
		res, err := (&http.Client{Transport: transport}).Get(server.URL)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	evict := func() {
		now = now.Add(transportIdleEviction + time.Second)
		forURL("http://other.invalid")
	}

	held := forURL(server.URL)
	evict()
	current := forURL(server.URL)
	if current == held {
		t.Fatal("expected the unused deployment to get a new transport")
	}
	get(current)
	get(held)
	if got := newConns.Load(); got != 1 {
		t.Fatalf("server accepted %d connections, want 1: a held transport must reuse the current pool", got)
	}

	evict()
	get(held)
	if forURL(server.URL) != held {
		t.Fatal("expected a held transport to become the entry of a deployment without one")
	}
}

func TestTransportRegistryClosesIdleConnectionsOfCurrentEntry(t *testing.T) {
	t.Parallel()

	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	now := time.Unix(0, 0)
	registry := newTransportRegistry(newCoderTransport)
	registry.now = func() time.Time { return now }
	forURL := func(rawURL string) *registeredTransport {
		transport, err := registry.forURL(mustParseURL(t, rawURL))
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		return transport
	}

	held := forURL(server.URL)
	now = now.Add(transportIdleEviction + time.Second)
	forURL("http://other.invalid")
	current := forURL(server.URL)
	if current == held {
		t.Fatal("expected the unused deployment to get a new transport")
	}
	res, err := (&http.Client{Transport: current}).Get(server.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	held.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("expected CloseIdleConnections on a held transport to close the current pool's idle connection")
	}
}

func TestTransportRegistryBoundsConnectionsAcrossHostnameCase(t *testing.T) {
	t.Parallel()

	const maxConns = 2
	release := make(chan struct{})
	arrived := make(chan string, maxConns+1)
	var newConns atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- r.Host
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	server.Start()
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})

	registry := newTransportRegistry(func() (*http.Transport, error) {
		transport, err := newCoderTransport()
		if err != nil {
			return nil, err
		}
		transport.Proxy = nil
		transport.MaxConnsPerHost = maxConns
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		return transport, nil
	})

	var wg sync.WaitGroup
	defer wg.Wait()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	start := func(rawURL string) {
		target := mustParseURL(t, rawURL)
		transport, err := registry.forURL(target)
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := (&http.Client{Transport: transport}).Get(target.String())
			if err != nil {
				t.Errorf("GET %s: %v", rawURL, err)
				return
			}
			defer func() { _ = res.Body.Close() }()
			if _, err := io.Copy(io.Discard, res.Body); err != nil {
				t.Errorf("read %s: %v", rawURL, err)
			}
		}()
	}
	waitArrival := func() string {
		t.Helper()
		select {
		case host := <-arrived:
			return host
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for a request to arrive")
			return ""
		}
	}

	for range maxConns {
		start("http://coder.invalid/api/v2/buildinfo")
	}
	for range maxConns {
		waitArrival()
	}
	start("http://CODER.INVALID/api/v2/buildinfo")
	select {
	case host := <-arrived:
		t.Fatalf("request for %q opened a connection beyond MaxConnsPerHost=%d", host, maxConns)
	case <-time.After(300 * time.Millisecond):
	}

	releaseOnce()
	if host := waitArrival(); host != "CODER.INVALID" {
		t.Fatalf("server got Host %q, want the host as written, %q", host, "CODER.INVALID")
	}
	wg.Wait()
	if got := newConns.Load(); got != maxConns {
		t.Fatalf("server accepted %d connections, want %d", got, maxConns)
	}
}

func TestTransportRegistryDialsLowercaseHostname(t *testing.T) {
	t.Parallel()

	dialed := make(chan string, 1)
	registry := newTransportRegistry(func() (*http.Transport, error) {
		transport, err := newCoderTransport()
		if err != nil {
			return nil, err
		}
		transport.Proxy = nil
		transport.DialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed <- addr
			return nil, fmt.Errorf("test dialer refuses %s", addr)
		}
		return transport, nil
	})

	for rawURL, wantAddr := range map[string]string{
		"http://CODER.Invalid:8080/api/v2": "coder.invalid:8080",
		"http://[FD00::1]/api/v2":          "[fd00::1]:80",
		// Interface names are case-sensitive, so the zone keeps its case.
		"http://[FE80::1%25ETH0]/api/v2": "[fe80::1%ETH0]:80",
	} {
		target := mustParseURL(t, rawURL)
		transport, err := registry.forURL(target)
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		res, err := (&http.Client{Transport: transport}).Get(target.String())
		if err == nil {
			_ = res.Body.Close()
			t.Fatalf("GET %s succeeded through a refusing dialer", rawURL)
		}
		if got := <-dialed; got != wantAddr {
			t.Errorf("GET %s dialed %q, want %q", rawURL, got, wantAddr)
		}
	}
}
