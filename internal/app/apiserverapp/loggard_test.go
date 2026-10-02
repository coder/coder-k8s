package apiserverapp

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"
)

const (
	testLogPath      = workspacesTestNS + "/" + testWorkspaceName + "/log"
	testLogLifetime  = 1500 * time.Millisecond
	stubChunkBytes   = 32 << 10
	stubQueryParam   = "guardstub"
	stubStall        = "stall"
	stubShort        = "short"
	stubSlowNonLog   = "slow"
	stubWriteTimeout = testLogLifetime + 10*time.Second
)

// stubResult reports how a stub handler ended.
type stubResult struct {
	writeErr error
	elapsed  time.Duration
}

// guardStubs answers marked requests inside the real handler chain, so the tests exercise the
// production composition of both guards around DefaultBuildHandlerChain. Requests without the
// marker reach the real API handler.
type guardStubs struct {
	stallDone chan stubResult
}

func (g *guardStubs) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		switch r.URL.Query().Get(stubQueryParam) {
		case stubStall:
			chunk := []byte(strings.Repeat("x", stubChunkBytes))
			var err error
			for err == nil && time.Since(start) < stubWriteTimeout {
				if _, err = w.Write(chunk); err == nil {
					err = http.NewResponseController(w).Flush()
				}
			}
			g.stallDone <- stubResult{writeErr: err, elapsed: time.Since(start)}
		case stubShort:
			_, _ = io.WriteString(w, "short")
		case stubSlowNonLog:
			_, _ = io.WriteString(w, "first ")
			_ = http.NewResponseController(w).Flush()
			time.Sleep(testLogLifetime + time.Second)
			_, _ = io.WriteString(w, "second")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func newGuardStubFixture(t *testing.T) (*authFixture, *guardStubs) {
	t.Helper()
	stubs := &guardStubs{stallDone: make(chan stubResult, 1)}
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) }, func(c *genericapiserver.RecommendedConfig) {
		chain := newLogGuardedHandlerChain(testLogLifetime)
		c.BuildHandlerChainFunc = func(h http.Handler, cfg *genericapiserver.Config) http.Handler {
			return chain(stubs.wrap(h), cfg)
		}
	})
	return f, stubs
}

func guardTestClient(cert *tls.Certificate, http2 bool) *http.Client {
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{*cert}}, //nolint:gosec // Test server uses an ephemeral self-signed cert.
		ForceAttemptHTTP2: http2,
	}
	if http2 {
		// One connection, so concurrent requests share it as separate streams.
		transport.MaxConnsPerHost = 1
	} else {
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{Transport: transport}
}

func guardRequest(ctx context.Context, t *testing.T, f *authFixture, path, stub string, reused *bool) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.baseURL+path+"?"+stubQueryParam+"="+stub, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range remoteUser("alice") {
		req.Header.Set(k, v)
	}
	if reused != nil {
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { *reused = info.Reused },
		}))
	}
	return req
}

// TestLogGuardEndsStalledLogResponse is the stall proof: a client that stops reading a log
// response cannot hold the handler past the write deadline, over HTTP/1 and HTTP/2, while other
// requests keep working.
func TestLogGuardEndsStalledLogResponse(t *testing.T) {
	for _, proto := range []struct {
		name  string
		http2 bool
	}{{"HTTP/1.1", false}, {"HTTP/2", true}} {
		t.Run(proto.name, func(t *testing.T) {
			f, stubs := newGuardStubFixture(t)
			client := guardTestClient(f.frontProxyCert(t), proto.http2)
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			resp, err := client.Do(guardRequest(ctx, t, f, testLogPath, stubStall, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK || resp.ProtoMajor != map[bool]int{false: 1, true: 2}[proto.http2] {
				t.Fatalf("stalled log: status=%d proto=%s", resp.StatusCode, resp.Proto)
			}
			if _, err := io.ReadFull(resp.Body, make([]byte, stubChunkBytes)); err != nil {
				t.Fatalf("read first chunk: %v", err)
			}

			// A non-log request on the same connection (HTTP/2) or a new one (HTTP/1) gets no
			// deadline: it finishes although it lasts longer than the log lifetime.
			var reused bool
			slow, err := client.Do(guardRequest(ctx, t, f, workspacesTestNS+"/other", stubSlowNonLog, &reused))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(slow.Body)
			_ = slow.Body.Close()
			if err != nil || string(body) != "first second" {
				t.Fatalf("slow non-log request: body=%q err=%v", body, err)
			}
			if proto.http2 && !reused {
				t.Fatal("the slow non-log request must share the HTTP/2 connection with the stalled log stream")
			}

			select {
			case res := <-stubs.stallDone:
				if res.writeErr == nil {
					t.Fatalf("stalled log handler ended without a write error after %s", res.elapsed)
				}
				if res.elapsed > testLogLifetime+5*time.Second {
					t.Fatalf("stalled log handler ended after %s, want about %s", res.elapsed, testLogLifetime)
				}
			case <-time.After(testLogLifetime + 10*time.Second):
				t.Fatal("stalled log handler never returned")
			}
		})
	}
}

// TestLogGuardClearsDeadlineOnKeepAlive: on HTTP/1 the next request on a kept-alive connection
// must not inherit the log response's write deadline. net/http clears it after each response.
func TestLogGuardClearsDeadlineOnKeepAlive(t *testing.T) {
	f, _ := newGuardStubFixture(t)
	client := guardTestClient(f.frontProxyCert(t), false)
	defer client.CloseIdleConnections()

	resp, err := client.Do(guardRequest(t.Context(), t, f, testLogPath, stubShort, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "short" {
		t.Fatalf("short log: status=%d body=%q", resp.StatusCode, body)
	}
	time.Sleep(testLogLifetime + 500*time.Millisecond)

	var reused bool
	next, err := client.Do(guardRequest(t.Context(), t, f, workspacesTestNS+"/other", stubShort, &reused))
	if err != nil {
		t.Fatalf("request after the log deadline on the same connection: %v", err)
	}
	body, _ = io.ReadAll(next.Body)
	_ = next.Body.Close()
	if !reused || next.StatusCode != http.StatusOK || string(body) != "short" {
		t.Fatalf("keep-alive request: reused=%v status=%d body=%q", reused, next.StatusCode, body)
	}
}

// TestLogGuardWiring runs the production config: authentication still runs before the upgrade
// refusal, and plain log GETs reach the router.
func TestLogGuardWiring(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) })
	cert := f.frontProxyCert(t)
	websocket := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}
	spdy := map[string]string{"Connection": "Upgrade", "Upgrade": "SPDY/3.1"}

	if status, body := f.server.do(t, nil, http.MethodGet, testLogPath, websocket, ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket: status=%d body=%.200s", status, body)
	}
	for name, headers := range map[string]map[string]string{"websocket": websocket, "spdy": spdy} {
		status, body := f.server.do(t, cert, http.MethodGet, testLogPath, mergeHeaders(remoteUser("alice"), headers), "")
		if status != http.StatusBadRequest || !strings.Contains(body, "does not support websocket") {
			t.Fatalf("authorized %s upgrade: status=%d body=%.200s", name, status, body)
		}
	}
	if status, body := f.server.do(t, cert, http.MethodGet, testLogPath, remoteUser("alice"), ""); status != http.StatusNotFound {
		t.Fatalf("plain log GET before the route exists: status=%d body=%.200s", status, body)
	}
	// Upgrades elsewhere are untouched by the guard.
	if status, body := f.server.do(t, cert, http.MethodGet, workspacesTestNS+"/"+testWorkspaceName, mergeHeaders(remoteUser("alice"), spdy), ""); status == http.StatusBadRequest {
		t.Fatalf("upgrade on a non-log path was refused: body=%.200s", body)
	}
}

func TestOuterLogGuardFailsClosedWithoutWriteDeadline(t *testing.T) {
	resolver := &apirequest.RequestInfoFactory{APIPrefixes: sets.NewString("api", "apis"), GrouplessAPIPrefixes: sets.NewString("api")}
	reached := 0
	guard := outerLogGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }), resolver, time.Minute)

	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://example.test"+testLogPath, nil))
	if rec.Code != http.StatusInternalServerError || reached != 0 {
		t.Fatalf("log request without write-deadline support: code=%d reached=%d", rec.Code, reached)
	}
	rec = httptest.NewRecorder()
	guard.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://example.test"+workspacesTestNS, nil))
	if rec.Code != http.StatusOK || reached != 1 {
		t.Fatalf("non-log request: code=%d reached=%d", rec.Code, reached)
	}
}

func TestValidateLogResponseLifetime(t *testing.T) {
	if err := validateLogResponseLifetime(logResponseLifetime, defaultRequestTimeout); err != nil {
		t.Fatalf("default limits rejected: %v", err)
	}
	for _, lifetime := range []time.Duration{0, defaultRequestTimeout, defaultRequestTimeout + time.Second} {
		err := validateLogResponseLifetime(lifetime, defaultRequestTimeout)
		if err == nil || !strings.Contains(err.Error(), "assertion failed") {
			t.Fatalf("lifetime %s: err=%v, want assertion failure", lifetime, err)
		}
	}
}
