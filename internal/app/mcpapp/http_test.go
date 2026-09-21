package mcpapp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const workspaceCall = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"set_workspace_running","arguments":{"namespace":"default","name":"dev","running":true}}}`

func TestHTTPTransportSecurity(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		podAddress  bool
		origin      string
		fetchSite   string
		contentType string
		body        string
		wantStatus  int
		wantCall    bool
	}{
		{name: "direct client", wantCall: true},
		{name: "localhost port forward", host: "localhost:8090", wantCall: true},
		{name: "IPv6 loopback Host", host: "[::1]:8090", wantCall: true},
		{name: "same origin", host: "localhost:8090", origin: "http://localhost:8090", wantCall: true},
		{name: "rebound Host", host: "attacker.example:8090", wantStatus: http.StatusForbidden},
		{name: "localhost suffix", host: "localhost.attacker.example:8090", wantStatus: http.StatusForbidden},
		{name: "service Host over loopback", host: "coder-k8s.coder-system.svc:8090", wantStatus: http.StatusForbidden},
		{name: "service short name", podAddress: true, host: "coder-k8s:8090", wantCall: true},
		{name: "service namespace name", podAddress: true, host: "coder-k8s.coder-system:8090", wantCall: true},
		{name: "service FQDN", podAddress: true, host: "coder-k8s.coder-system.svc.cluster.local:8090", wantCall: true},
		// Localhost protection is not a general Host allowlist or authentication.
		{name: "nonloopback arbitrary Host", podAddress: true, host: "attacker.example:8090", wantCall: true},
		{name: "cross origin", origin: "https://attacker.example", wantStatus: http.StatusForbidden},
		{name: "opaque origin", origin: "null", wantStatus: http.StatusForbidden},
		{name: "cross site fetch", fetchSite: "cross-site", wantStatus: http.StatusForbidden},
		{name: "cross origin service", podAddress: true, host: "coder-k8s.coder-system.svc:8090", origin: "https://attacker.example", wantStatus: http.StatusForbidden},
		{name: "plain text", contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "form encoded", contentType: "application/x-www-form-urlencoded", wantStatus: http.StatusUnsupportedMediaType},
		{name: "missing content type", contentType: "omit", wantStatus: http.StatusUnsupportedMediaType},
		{name: "JSON charset parameter", contentType: "application/json; charset=utf-8", wantStatus: http.StatusUnsupportedMediaType},
		// A noncanonical key must not override the exact "method" key (GHSA-wvj2-96wp-fq3f).
		{name: "case mismatch", body: strings.Replace(workspaceCall, `"method":"tools/call"`, `"method":"ping","Method":"tools/call"`, 1)},
		// GHSA-q382-vc8q-7jhj concerns null characters in keys, not a JSON null value.
		{name: "null suffixed key", body: strings.Replace(workspaceCall, `"method":"tools/call"`, `"method":"ping","method\u0000":"tools/call"`, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient, server := newHTTPTestServer(t, tt.podAddress)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"security-test","version":"1"}}}`
			status, headers, body := postMCP(ctx, t, server, "", "", "", "", "application/json", initialize)
			if status != http.StatusOK || headers.Get("Mcp-Session-Id") == "" {
				t.Fatalf("initialize: status=%d headers=%v body=%s", status, headers, body)
			}
			sessionID := headers.Get("Mcp-Session-Id")
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				req, err := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, server.URL+"/mcp", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Mcp-Session-Id", sessionID)
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Errorf("delete session: %v", err)
					return
				}
				defer func() {
					if err := resp.Body.Close(); err != nil {
						t.Error(err)
					}
				}()
				if resp.StatusCode != http.StatusNoContent {
					t.Errorf("delete session: status=%d", resp.StatusCode)
				}
			})
			status, _, body = postMCP(ctx, t, server, sessionID, "", "", "", "application/json", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			if status != http.StatusAccepted {
				t.Fatalf("initialized: status=%d body=%s", status, body)
			}
			if tt.contentType == "" {
				tt.contentType = "application/json"
			}
			if tt.body == "" {
				tt.body = workspaceCall
			}
			if tt.wantStatus == 0 {
				tt.wantStatus = http.StatusOK
			}
			status, _, body = postMCP(ctx, t, server, sessionID, tt.host, tt.origin, tt.fetchSite, tt.contentType, tt.body)
			if status != tt.wantStatus {
				t.Errorf("status=%d, want %d; body=%s", status, tt.wantStatus, body)
			}
			wantCalls := int32(0)
			if tt.wantCall {
				wantCalls = 1
			}
			if got := k8sClient.gets.Load(); got != wantCalls {
				t.Errorf("tool resource reads=%d, want %d", got, wantCalls)
			}
			assertWorkspaceRunning(t, k8sClient.Client, tt.wantCall)
			t.Logf("status=%d tool resource reads=%d running=%v", status, k8sClient.gets.Load(), tt.wantCall)

			if !tt.wantCall {
				// Rejection must not damage the session or prevent a legitimate tool call.
				status, _, body = postMCP(ctx, t, server, sessionID, "", "", "", "application/json", workspaceCall)
				if status != http.StatusOK || k8sClient.gets.Load() != 1 {
					t.Fatalf("legitimate follow-up: status=%d reads=%d body=%s", status, k8sClient.gets.Load(), body)
				}
				assertWorkspaceRunning(t, k8sClient.Client, true)
			}
		})
	}
}

func TestHTTPTransportSDKClient(t *testing.T) {
	k8sClient, server := newHTTPTestServer(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: server.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "set_workspace_running",
		Arguments: map[string]any{"namespace": "default", "name": "dev", "running": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || k8sClient.gets.Load() != 1 {
		t.Fatalf("tool result=%+v reads=%d", result, k8sClient.gets.Load())
	}
	assertWorkspaceRunning(t, k8sClient.Client, true)
}

func newHTTPTestServer(t *testing.T, podAddress bool) (*observedHTTPClient, *httptest.Server) {
	t.Helper()
	k8sClient := &observedHTTPClient{Client: mustNewFakeClient(t, &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
	})}
	handler := newMCPHTTPHandler(NewServer(k8sClient, k8sfake.NewClientset()))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if podAddress {
			// Model net/http's Pod-side local address without opening a non-loopback
			// listener or contacting a cluster. This is not a Kubernetes network test.
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8090}))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	addr, ok := server.Listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		t.Fatalf("test listener must be loopback: %v", server.Listener.Addr())
	}
	return k8sClient, server
}

func postMCP(ctx context.Context, t *testing.T, server *httptest.Server, sessionID, host, origin, fetchSite, contentType, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Accept", "application/json, text/event-stream")
	if contentType != "omit" {
		req.Header.Set("Content-Type", contentType)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
		req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(data)
}

type observedHTTPClient struct {
	client.Client
	gets atomic.Int32
}

func (c *observedHTTPClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets.Add(1)
	return c.Client.Get(ctx, key, obj, opts...)
}

func assertWorkspaceRunning(t *testing.T, k8sClient client.Client, want bool) {
	t.Helper()
	workspace := &aggregationv1alpha1.CoderWorkspace{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "dev"}, workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Spec.Running != want {
		t.Fatalf("workspace running=%v, want %v", workspace.Spec.Running, want)
	}
}
