package coder

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"golang.org/x/net/http2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// TestMapCoderWriteErrorRealTransport sends a write through the HTTP client of NewSDKClient and
// checks that a write the Coder handler got returns the uncertain 504. A write that it did not
// get returns the 500 only when the error proves that the request was not sent.
func TestMapCoderWriteErrorRealTransport(t *testing.T) {
	t.Parallel()

	var received atomic.Int32
	created := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	certServer := httptest.NewTLSServer(created)
	t.Cleanup(certServer.Close)
	plainServer := httptest.NewServer(created)
	t.Cleanup(plainServer.Close)
	tls13Server := httptest.NewUnstartedServer(created)
	tls13Server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	tls13Server.StartTLS()
	t.Cleanup(tls13Server.Close)
	cipherServer := httptest.NewUnstartedServer(created)
	cipherServer.TLS = &tls.Config{ //nolint:gosec // G402: only TLS 1.2 lets a server refuse the cipher suites.
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
	}
	cipherServer.StartTLS()
	t.Cleanup(cipherServer.Close)
	brokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(brokenServer.Close)
	// recordServer reads the whole write after the TLS handshake and answers with header, the
	// header of a TLS record that the client rejects.
	recordServer := func(header []byte) *httptest.Server {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("read body: %v", err)
				return
			}
			received.Add(1)
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.(*tls.Conn).NetConn().Write(header); err != nil {
				t.Errorf("write record header: %v", err)
			}
		}))
		t.Cleanup(server.Close)
		return server
	}
	wrongVersionServer := recordServer([]byte{23, 3, 1, 0, 0})
	// alertServer reads the whole write, then reads a record with the wrong version, which makes
	// it send a protocol_version alert: a TLS alert after the request, not a handshake refusal.
	alertServer := func(version uint16) *httptest.Server {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("read body: %v", err)
				return
			}
			received.Add(1)
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			tlsConn := conn.(*tls.Conn)
			tlsConn.NetConn().(*injectReadConn).injected = []byte{23, 3, 1, 0, 0}
			_, _ = tlsConn.Read(make([]byte, 1))
		}))
		server.Listener = injectReadListener{server.Listener}
		server.TLS = &tls.Config{MinVersion: version, MaxVersion: version} //nolint:gosec // G402: the test pins TLS 1.2 and TLS 1.3 servers.
		server.StartTLS()
		t.Cleanup(server.Close)
		return server
	}
	tls12AlertServer := alertServer(tls.VersionTLS12)
	tls13AlertServer := alertServer(tls.VersionTLS13)
	h2ResetURL, h2ResetTLS := h2ResetServer(t, &received)
	oversizedRecordServer := recordServer([]byte{23, 3, 3, 0xff, 0xff})
	// redirectServer gets the write, answers with a same-origin redirect, and stops listening, so
	// the request that follows the redirect cannot connect.
	var redirectServer *httptest.Server
	redirectServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_ = redirectServer.Listener.Close()
		w.Header().Set("Connection", "close")
		http.Redirect(w, r, "/api/v2/moved", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirectServer.Close)
	refusingProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("expected CONNECT, got %s", r.Method)
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	t.Cleanup(refusingProxy.Close)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	mustParse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return u
	}
	tls12With := func(suites ...uint16) func(*http.Transport) {
		return func(tr *http.Transport) {
			tr.TLSClientConfig = &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: suites} //nolint:gosec // G402: the test needs a TLS 1.2 client that the servers refuse.
		}
	}
	trust := func(server *httptest.Server) func(*http.Transport) {
		return func(tr *http.Transport) {
			tr.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		}
	}
	proxy := func(raw string) func(*http.Transport) {
		return func(tr *http.Transport) { tr.Proxy = http.ProxyURL(mustParse(raw)) }
	}

	for _, tc := range []struct {
		name string
		// coderURL is the URL of the Coder client. sameOrigin, when set, is the origin that the
		// client allows, to make it refuse a request to coderURL.
		coderURL   string
		sameOrigin string
		configure  func(*http.Transport)
		wantSent   bool
		// wantUncertain is set for a write that the handler did not get, but whose error does not
		// prove that.
		wantUncertain bool
	}{
		{name: "connection refused", coderURL: "http://" + closedAddr},
		{name: "certificate rejected", coderURL: certServer.URL},
		{name: "proxy connection refused", coderURL: "https://coder.invalid", configure: proxy("http://" + closedAddr)},
		{name: "request to another origin", coderURL: plainServer.URL, sameOrigin: "http://allowed.invalid"},
		{name: "proxy refuses CONNECT with 407", coderURL: "https://coder.invalid", configure: proxy(refusingProxy.URL)},
		{name: "https URL answered by an HTTP server", coderURL: strings.Replace(plainServer.URL, "http://", "https://", 1)},
		{name: "server refuses the TLS version", coderURL: tls13Server.URL, configure: tls12With(), wantUncertain: true},
		{name: "server refuses the cipher suites", coderURL: cipherServer.URL, configure: tls12With(tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305, tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305), wantUncertain: true},
		{name: "connection closed after the request", coderURL: brokenServer.URL, wantSent: true},
		{name: "redirect, then the next connection is refused", coderURL: redirectServer.URL, wantSent: true},
		{name: "TLS record with the wrong version after the request", coderURL: wrongVersionServer.URL, configure: trust(wrongVersionServer), wantSent: true},
		{name: "oversized TLS record after the request", coderURL: oversizedRecordServer.URL, configure: trust(oversizedRecordServer), wantSent: true},
		{name: "TLS 1.2 protocol_version alert after the request", coderURL: tls12AlertServer.URL, configure: trust(tls12AlertServer), wantSent: true},
		{name: "TLS 1.3 protocol_version alert after the request", coderURL: tls13AlertServer.URL, configure: trust(tls13AlertServer), wantSent: true},
		{
			name: "HTTP/2 stream reset after the request, then the next connection is refused", coderURL: h2ResetURL,
			configure: func(tr *http.Transport) { tr.TLSClientConfig = h2ResetTLS.Clone() }, wantSent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coderURL := mustParse(tc.coderURL)
			allowed := coderURL
			if tc.sameOrigin != "" {
				allowed = mustParse(tc.sameOrigin)
			}
			transport, err := newCoderTransport()
			if err != nil {
				t.Fatalf("new transport: %v", err)
			}
			if tc.configure != nil {
				tc.configure(transport)
			}
			t.Cleanup(transport.CloseIdleConnections)
			client := codersdk.New(coderURL, codersdk.WithHTTPClient(newCoderHTTPClient(allowed, transport, 5*time.Second)))

			before := received.Load()
			resp, err := client.Request(context.Background(), http.MethodPost, "/api/v2/workspaces", map[string]string{"name": "dev"})
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("expected a transport error")
			}
			if sent := received.Load() != before; sent != tc.wantSent {
				t.Fatalf("expected the Coder handler to get the request: %t, got %t (error %v)", tc.wantSent, sent, err)
			}

			mapped := MapCoderWriteError(err, WriteCreate, aggregationv1alpha1.Resource("coderworkspaces"), "acme.alice.dev")
			if tc.wantSent || tc.wantUncertain {
				if !apierrors.IsTimeout(mapped) || !strings.Contains(mapped.Error(), "uncertain") {
					t.Fatalf("expected the uncertain 504 for %v, got %v", err, mapped)
				}
				return
			}
			if !apierrors.IsInternalError(mapped) || !strings.Contains(mapped.Error(), "could not be reached") {
				t.Fatalf("expected the 500 that says Coder could not be reached for %v, got %v", err, mapped)
			}
		})
	}
}

// injectReadConn returns injected before the bytes of the connection, to model a corrupted record.
type injectReadConn struct {
	net.Conn
	injected []byte
}

func (c *injectReadConn) Read(p []byte) (int, error) {
	if len(c.injected) != 0 {
		n := copy(p, c.injected)
		c.injected = c.injected[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

type injectReadListener struct{ net.Listener }

func (l injectReadListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &injectReadConn{Conn: conn}, nil
}

// h2ResetServer starts an HTTP/2 server for one connection. It reads the whole write, counts it in
// received, and answers with GOAWAY and RST_STREAM(PROTOCOL_ERROR). The Go client then sends the
// write again on a new connection, which the closed listener refuses. h2ResetServer returns the
// URL of the server and the TLS config of a client that trusts it.
func h2ResetServer(t *testing.T, received *atomic.Int32) (string, *tls.Config) {
	t.Helper()
	certServer := httptest.NewUnstartedServer(nil)
	certServer.EnableHTTP2 = true
	certServer.StartTLS()
	serverTLS := certServer.TLS.Clone()
	serverTLS.NextProtos = []string{"h2"}
	clientTLS := certServer.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	certServer.Close()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		_ = listener.Close()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := io.ReadFull(conn, make([]byte, len(http2.ClientPreface))); err != nil {
			t.Errorf("read preface: %v", err)
			return
		}
		framer := http2.NewFramer(conn, conn)
		if err := framer.WriteSettings(); err != nil {
			t.Errorf("write settings: %v", err)
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				t.Errorf("read frame: %v", err)
				return
			}
			switch frame := frame.(type) {
			case *http2.SettingsFrame:
				if !frame.IsAck() {
					_ = framer.WriteSettingsAck()
				}
			case *http2.DataFrame:
				if !frame.StreamEnded() {
					continue
				}
				received.Add(1)
				_ = framer.WriteGoAway(frame.StreamID, http2.ErrCodeNo, nil)
				_ = framer.WriteRSTStream(frame.StreamID, http2.ErrCodeProtocol)
				// Keep the connection open until the client has read both frames.
				_, _ = io.Copy(io.Discard, conn)
				return
			}
		}
	}()
	return "https://" + listener.Addr().String(), clientTLS
}

// TestMapCoderWriteErrorPartlyWrittenHeaders: the server reads a part of the request headers and
// resets the connection before net/http records the headers as written. The write error is not one
// that proves the request was not sent, so the write returns the uncertain 504.
func TestMapCoderWriteErrorPartlyWrittenHeaders(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- -1
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		n, _ := io.ReadFull(conn, make([]byte, 4096))
		received <- n
		_ = conn.(*net.TCPConn).SetLinger(0)
	}()

	base, err := url.Parse("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport, err := newCoderTransport()
	if err != nil {
		t.Fatal(err)
	}
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base.String()+"/api/v2/workspaces", strings.NewReader(`{"name":"dev"}`))
	if err != nil {
		t.Fatal(err)
	}
	// A header far larger than what the server reads, so the reset comes while net/http writes it.
	req.Header.Set(codersdk.SessionTokenHeader, strings.Repeat("x", 16<<20))
	resp, err := newCoderHTTPClient(base, transport, 10*time.Second).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the reset connection to fail the request")
	}
	if n := <-received; n <= 0 {
		t.Fatalf("the server received %d bytes of the request, want some", n)
	}
	mapped := MapCoderWriteError(err, WriteCreate, aggregationv1alpha1.Resource("coderworkspaces"), "acme.alice.dev")
	if !apierrors.IsTimeout(mapped) || !strings.Contains(mapped.Error(), "uncertain") {
		t.Fatalf("expected the uncertain 504, got %v (from %v)", mapped, err)
	}
}
