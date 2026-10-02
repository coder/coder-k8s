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
		<-*release
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
