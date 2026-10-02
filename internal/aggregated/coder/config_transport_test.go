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
	forURL := func(rawURL string) *http.Transport {
		transport, err := registry.forURL(mustParseURL(t, rawURL))
		if err != nil {
			t.Fatalf("forURL(%q): %v", rawURL, err)
		}
		return transport
	}

	base := forURL("http://coder.coder.svc.cluster.local")
	if got := forURL("HTTP://Coder.coder.svc.cluster.local/api/v2"); got != base {
		t.Fatal("expected one transport for the same scheme and host")
	}
	for _, other := range []string{
		"https://coder.coder.svc.cluster.local",
		"http://coder.coder.svc.cluster.local:8080",
		"http://coder.other.svc.cluster.local",
	} {
		if forURL(other) == base {
			t.Fatalf("expected %q to get its own transport", other)
		}
	}
}
