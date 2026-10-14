package coder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/coder/coder/v2/codersdk"
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
