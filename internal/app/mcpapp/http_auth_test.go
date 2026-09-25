package mcpapp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const initializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"auth-test","version":"1"}}}`

func TestHTTPBearerTokenRequiredForInitialize(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{name: "missing header", wantStatus: http.StatusUnauthorized},
		{name: "basic scheme", authorization: "Basic " + testToken, wantStatus: http.StatusUnauthorized},
		{name: "bearer without credential", authorization: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "bare token", authorization: testToken, wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authorization: "Bearer " + strings.Repeat("x", len(testToken)), wantStatus: http.StatusUnauthorized},
		{name: "token prefix", authorization: "Bearer " + testToken[:len(testToken)-1], wantStatus: http.StatusUnauthorized},
		{name: "token with suffix", authorization: "Bearer " + testToken + "x", wantStatus: http.StatusUnauthorized},
		{name: "token with extra field", authorization: "Bearer " + testToken + " extra", wantStatus: http.StatusUnauthorized},
		{name: "correct token", authorization: "Bearer " + testToken, wantStatus: http.StatusOK},
		{name: "case-insensitive scheme", authorization: "bearer " + testToken, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient, server := newHTTPTestServer(t, false)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			status, headers, body := postMCPWithAuth(ctx, t, server, tt.authorization, "", "", "", "", "application/json", initializeRequest)
			if status != tt.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", status, tt.wantStatus, body)
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if headers.Get("Mcp-Session-Id") != "" {
					t.Fatalf("unauthenticated initialize must not create a session, got %q", headers.Get("Mcp-Session-Id"))
				}
				if !strings.HasPrefix(headers.Get("WWW-Authenticate"), "Bearer") {
					t.Fatalf("expected Bearer challenge, got %q", headers.Get("WWW-Authenticate"))
				}
			}
			if got := k8sClient.gets.Load(); got != 0 {
				t.Fatalf("initialize must not read resources, got %d reads", got)
			}
		})
	}
}

// TestHTTPSessionReuseRequiresToken proves that an existing session ID is never a credential:
// every POST, GET (stream reconnect/resume), and DELETE on an established session needs the token.
func TestHTTPSessionReuseRequiresToken(t *testing.T) {
	k8sClient, server := newHTTPTestServer(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	status, headers, body := postMCP(ctx, t, server, "", "", "", "", "application/json", initializeRequest)
	sessionID := headers.Get("Mcp-Session-Id")
	if status != http.StatusOK || sessionID == "" {
		t.Fatalf("initialize: status=%d session=%q body=%s", status, sessionID, body)
	}
	status, _, body = postMCP(ctx, t, server, sessionID, "", "", "", "application/json", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted {
		t.Fatalf("initialized: status=%d body=%s", status, body)
	}

	for _, authorization := range []string{"", "Bearer wrong-" + testToken, "Basic " + testToken} {
		status, _, body = postMCPWithAuth(ctx, t, server, authorization, sessionID, "", "", "", "application/json", workspaceCall)
		if status != http.StatusUnauthorized {
			t.Fatalf("tools/call with session and authorization %q: status=%d body=%s", authorization, status, body)
		}
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			if status := doSessionRequest(ctx, t, server, method, sessionID, authorization); status != http.StatusUnauthorized {
				t.Fatalf("%s with session and authorization %q: status=%d", method, authorization, status)
			}
		}
	}
	if got := k8sClient.gets.Load(); got != 0 {
		t.Fatalf("unauthenticated session requests read %d resources", got)
	}
	assertWorkspaceRunning(t, k8sClient.Client, false)

	// The rejected DELETE must not have ended the session: the token holder can still use it.
	status, _, body = postMCP(ctx, t, server, sessionID, "", "", "", "application/json", workspaceCall)
	if status != http.StatusOK || k8sClient.gets.Load() != 1 {
		t.Fatalf("authenticated follow-up: status=%d reads=%d body=%s", status, k8sClient.gets.Load(), body)
	}
	assertWorkspaceRunning(t, k8sClient.Client, true)

	if status := doSessionRequest(ctx, t, server, http.MethodDelete, sessionID, "Bearer "+testToken); status != http.StatusNoContent {
		t.Fatalf("authenticated delete: status=%d", status)
	}
}

func TestHTTPOnlyExactHealthPathsAreUnauthenticated(t *testing.T) {
	_, server := newHTTPTestServer(t, false)
	for path, want := range map[string]int{
		"/healthz":        http.StatusOK,
		"/readyz":         http.StatusOK,
		"/healthz/":       http.StatusUnauthorized,
		"/readyz/x":       http.StatusUnauthorized,
		"/mcp":            http.StatusUnauthorized,
		"/mcp/":           http.StatusUnauthorized,
		"/MCP":            http.StatusUnauthorized,
		"/":               http.StatusUnauthorized,
		"/healthz/../mcp": http.StatusUnauthorized,
	} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.URL.Path = path
			req.URL.RawPath = path
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != want {
				t.Fatalf("GET %s without token: status=%d, want %d", path, resp.StatusCode, want)
			}
		})
	}
}

func TestHTTPTransportSDKClientWithoutTokenFails(t *testing.T) {
	k8sClient, server := newHTTPTestServer(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: server.Client()}, nil)
	if err == nil {
		_ = session.Close()
		t.Fatal("expected Connect without token to fail")
	}
	if got := k8sClient.gets.Load(); got != 0 {
		t.Fatalf("unauthenticated SDK client read %d resources", got)
	}
}

func TestDefaultHTTPAddrIsLoopback(t *testing.T) {
	host, port, err := net.SplitHostPort(DefaultHTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("DefaultHTTPAddr host must be a loopback IP literal, got %q", host)
	}
	if port != "8090" {
		t.Fatalf("DefaultHTTPAddr port changed unexpectedly: %q", port)
	}
}

func TestReadTokenFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name      string
		path      string
		want      string
		wantError string
	}{
		{name: "flag missing", path: "", wantError: "--mcp-token-file is required"},
		{name: "file missing", path: filepath.Join(dir, "absent"), wantError: "read --mcp-token-file"},
		{name: "directory", path: dir, wantError: "read --mcp-token-file"},
		{name: "empty file", path: write("empty", ""), wantError: "is empty"},
		{name: "newline only", path: write("newline", "\n"), wantError: "is empty"},
		{name: "too short", path: write("short", "abc\n"), wantError: "at least 32 are required"},
		{name: "inner whitespace", path: write("space", strings.Repeat("a", 20)+" "+strings.Repeat("b", 20)), wantError: "without whitespace"},
		{name: "two lines", path: write("lines", testToken+"\n"+testToken+"\n"), wantError: "without whitespace"},
		{name: "trailing newline", path: write("lf", testToken+"\n"), want: testToken},
		{name: "trailing CRLF", path: write("crlf", testToken+"\r\n"), want: testToken},
		{name: "no newline", path: write("plain", testToken), want: testToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := ReadTokenFile(tt.path)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got token=%q err=%v", tt.wantError, token, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(token) != tt.want {
				t.Fatalf("token=%q, want %q", token, tt.want)
			}
		})
	}
}

func TestReadTokenFileUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 000 files")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testToken), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTokenFile(path); err == nil || !strings.Contains(err.Error(), "read --mcp-token-file") {
		t.Fatalf("expected unreadable token file error, got %v", err)
	}
}

// TestRunHTTPFailsBeforeClusterAccessWithoutToken runs the real entry point: token validation must
// fail before any Kubernetes client is built.
func TestRunHTTPFailsBeforeClusterAccessWithoutToken(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "must-not-be-read"))
	if err := RunHTTP(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "--mcp-token-file is required") {
		t.Fatalf("expected missing token error, got %v", err)
	}
	if err := RunHTTPWithClients(t.Context(), nil, nil, nil); err == nil {
		t.Fatal("expected assertion error for nil clients")
	}
}

func doSessionRequest(ctx context.Context, t *testing.T, server *httptest.Server, method, sessionID, authorization string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Mcp-Session-Id", sessionID)
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", "0")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
	return resp.StatusCode
}

type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (tr tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tr.token)
	return tr.base.RoundTrip(clone)
}

func tokenHTTPClient(server *httptest.Server, token string) *http.Client {
	base := server.Client()
	return &http.Client{Transport: tokenTransport{token: token, base: base.Transport}, Timeout: base.Timeout}
}
