package apiserverapp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func integrationMockRenderedLog() string {
	var b strings.Builder
	for _, entry := range integrationMockBuildLog {
		b.WriteString(entry.Text() + "\n")
	}
	return b.String()
}

// TestWorkspaceLogRoute runs coderworkspaces/log through the production server.
func TestWorkspaceLogRoute(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) })
	cert := f.frontProxyCert(t)
	alice := remoteUser("alice")

	status, body := f.server.do(t, cert, http.MethodGet, "/apis/aggregation.coder.com/v1alpha1", alice, "")
	if status != http.StatusOK {
		t.Fatalf("discovery: status=%d", status)
	}
	var resources metav1.APIResourceList
	if err := json.Unmarshal([]byte(body), &resources); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range resources.APIResources {
		if r.Name == "coderworkspaces/log" {
			found = true
			if len(r.Verbs) != 1 || r.Verbs[0] != "get" || !r.Namespaced {
				t.Fatalf("coderworkspaces/log discovery entry: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("discovery lacks coderworkspaces/log: %.500s", body)
	}

	if status, body := f.server.do(t, cert, http.MethodGet, testLogPath, alice, ""); status != http.StatusOK || body != integrationMockRenderedLog() {
		t.Fatalf("log GET: status=%d body=%q", status, body)
	}
	if status, body := f.server.do(t, cert, http.MethodGet, testLogPath+"?limitBytes=10", alice, ""); status != http.StatusOK || body != integrationMockRenderedLog()[:10] {
		t.Fatalf("limitBytes=10: status=%d body=%q", status, body)
	}
	for query, want := range map[string]int{"?limitBytes=0": http.StatusUnprocessableEntity, "?limitBytes=abc": http.StatusBadRequest} {
		if status, body := f.server.do(t, cert, http.MethodGet, testLogPath+query, alice, ""); status != want {
			t.Errorf("%s: status=%d want %d body=%.200s", query, status, want, body)
		}
	}
	if status, body := f.server.do(t, cert, http.MethodGet, testLogPath, mergeHeaders(alice, map[string]string{"Accept": "text/plain"}), ""); status != http.StatusNotAcceptable {
		t.Errorf("Accept text/plain only: status=%d body=%.200s", status, body)
	}
	if status, _ := f.server.do(t, nil, http.MethodGet, testLogPath, nil, ""); status != http.StatusUnauthorized {
		t.Errorf("anonymous log GET: status=%d, want 401", status)
	}
}

// TestWorkspaceLogRoutePerUserSlots: a user holding every per-user slot gets 429 without a Coder
// call, other users are unaffected, and slots return when clients disconnect before the first
// byte.
func TestWorkspaceLogRoutePerUserSlots(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) })
	cert := f.frontProxyCert(t)
	hold := make(chan struct{})
	f.server.mock.holdLogs.Store(&hold)
	defer func() {
		if f.server.mock.holdLogs.Load() != nil {
			close(hold)
		}
	}()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{*cert}}}} //nolint:gosec // Test server uses an ephemeral self-signed cert.
	defer client.CloseIdleConnections()
	type result struct {
		status int
		body   string
		err    error
	}
	start := func(ctx context.Context, userName string) chan result {
		out := make(chan result, 1)
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.baseURL+testLogPath, nil)
			if err != nil {
				out <- result{err: err}
				return
			}
			for k, v := range remoteUser(userName) {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				out <- result{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			data, err := io.ReadAll(resp.Body)
			out <- result{status: resp.StatusCode, body: string(data), err: err}
		}()
		return out
	}
	waitHeld := func(n int32) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); f.server.mock.heldLogs.Load() != n; {
			if time.Now().After(deadline) {
				t.Fatalf("held log requests: %d, want %d", f.server.mock.heldLogs.Load(), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	aliceCtx, cancelAlice := context.WithCancel(t.Context())
	var aliceHeld []chan result
	for i := 0; i < 4; i++ {
		aliceHeld = append(aliceHeld, start(aliceCtx, "alice"))
	}
	waitHeld(4)
	calls := f.server.provider.calls.Load()
	status, body := f.server.do(t, cert, http.MethodGet, testLogPath, remoteUser("alice"), "")
	if status != http.StatusTooManyRequests || !strings.Contains(body, "for this user") {
		t.Fatalf("fifth stream for alice: status=%d body=%.200s", status, body)
	}
	if got := f.server.provider.calls.Load(); got != calls {
		t.Fatalf("the 429 reached Coder: calls %d -> %d", calls, got)
	}
	bob := start(t.Context(), "bob")
	waitHeld(5)

	cancelAlice()
	for _, ch := range aliceHeld {
		if res := <-ch; res.err == nil {
			t.Fatalf("cancelled alice request finished with status %d", res.status)
		}
	}
	waitHeld(1)
	again := start(t.Context(), "alice")
	waitHeld(2)
	close(hold)
	f.server.mock.holdLogs.Store(nil)
	for name, ch := range map[string]chan result{"bob": bob, "alice after release": again} {
		if res := <-ch; res.err != nil || res.status != http.StatusOK || res.body != integrationMockRenderedLog() {
			t.Fatalf("%s: status=%d err=%v body=%q", name, res.status, res.err, res.body)
		}
	}
}
