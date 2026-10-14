package coder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestNewSDKClient_RefusesCrossOriginRedirect checks that the aggregated
// client, which CoderTemplateTest, CoderAIProvider, CoderMCPServer and
// CoderAIGatewayKey also use, never sends the operator token to another origin.
func TestNewSDKClient_RefusesCrossOriginRedirect(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var otherOriginTokens []string
	otherOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherOriginTokens = append(otherOriginTokens, r.Header.Get(codersdk.SessionTokenHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer otherOrigin.Close()
	coderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherOrigin.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer coderServer.Close()

	coderURL, err := url.Parse(coderServer.URL)
	require.NoError(t, err)
	client, err := NewSDKClient(Config{CoderURL: coderURL, SessionToken: "operator-token"})
	require.NoError(t, err)

	_, err = client.Workspaces(context.Background(), codersdk.WorkspaceFilter{})
	require.ErrorContains(t, err, "refusing redirect to a different origin")

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, otherOriginTokens, "the other origin must receive no request, so no Coder-Session-Token")
}

func TestNewSDKClient_FollowsSameOriginRedirect(t *testing.T) {
	t.Parallel()

	var redirectedToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/buildinfo":
			http.Redirect(w, r, "/moved/api/v2/buildinfo", http.StatusTemporaryRedirect)
		case "/moved/api/v2/buildinfo":
			redirectedToken = r.Header.Get(codersdk.SessionTokenHeader)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"v2.37.2"}`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	coderURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := NewSDKClient(Config{CoderURL: coderURL, SessionToken: "operator-token"})
	require.NoError(t, err)

	buildInfo, err := client.BuildInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v2.37.2", buildInfo.Version)
	require.Equal(t, "operator-token", redirectedToken)
}

// TestNewSDKClient_WebsocketRefusesCrossOriginRedirect covers ?follow=true
// build logs. codersdk dials that websocket with a new http.Client that keeps
// only the Transport, so CheckRedirect alone does not protect the token.
func TestNewSDKClient_WebsocketRefusesCrossOriginRedirect(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var otherOriginTokens []string
	otherOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherOriginTokens = append(otherOriginTokens, r.Header.Get(codersdk.SessionTokenHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer otherOrigin.Close()
	coderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherOrigin.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer coderServer.Close()

	coderURL, err := url.Parse(coderServer.URL)
	require.NoError(t, err)
	client, err := NewSDKClient(Config{CoderURL: coderURL, SessionToken: "operator-token"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = client.WorkspaceBuildLogsAfter(ctx, uuid.New(), 0)
	require.ErrorContains(t, err, "refusing redirect to a different origin")

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, otherOriginTokens, "the other origin must receive no request, so no Coder-Session-Token")
}

func TestNewSDKClient_WebsocketFollowsSameOriginRedirect(t *testing.T) {
	t.Parallel()

	buildID := uuid.New()
	logsPath := "/api/v2/workspacebuilds/" + buildID.String() + "/logs"
	tokens := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case logsPath:
			http.Redirect(w, r, "/moved"+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		case "/moved" + logsPath:
			tokens <- r.Header.Get(codersdk.SessionTokenHeader)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
			data, err := json.Marshal(codersdk.ProvisionerJobLog{ID: 1, Output: "hello"})
			if err != nil {
				t.Errorf("marshal log: %v", err)
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, data); err != nil {
				t.Errorf("write log: %v", err)
			}
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	coderURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := NewSDKClient(Config{CoderURL: coderURL, SessionToken: "operator-token"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logs, closer, err := client.WorkspaceBuildLogsAfter(ctx, buildID, 0)
	require.NoError(t, err)
	defer func() { _ = closer.Close() }()
	require.Equal(t, "operator-token", <-tokens)

	select {
	case log, ok := <-logs:
		require.True(t, ok, "log channel closed before the first log")
		require.Equal(t, "hello", log.Output)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the redirected websocket log")
	}
}
