package controller_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/controller"
)

// TestSDKLicenseClient_RefusesCrossOriginRedirect checks that the license and
// entitlements client never sends the operator token to another origin, for
// example the access URL of CODER_REDIRECT_TO_ACCESS_URL.
func TestSDKLicenseClient_RefusesCrossOriginRedirect(t *testing.T) {
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
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherOrigin.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer controlPlane.Close()

	ctx := context.Background()
	_, err := controller.NewSDKLicenseUploader().HasAnyLicense(ctx, controlPlane.URL, nil, "operator-token")
	require.ErrorContains(t, err, "refusing redirect to a different origin")
	err = controller.NewSDKLicenseUploader().AddLicense(ctx, controlPlane.URL, nil, "operator-token", "license-jwt")
	require.ErrorContains(t, err, "refusing redirect to a different origin")
	_, err = controller.NewSDKEntitlementsInspector().Entitlements(ctx, controlPlane.URL, nil, "operator-token")
	require.ErrorContains(t, err, "refusing redirect to a different origin")

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, otherOriginTokens, "the other origin must receive no request, so no Coder-Session-Token")
}

func TestSDKLicenseClient_FollowsSameOriginRedirect(t *testing.T) {
	t.Parallel()

	var redirectedToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/licenses":
			http.Redirect(w, r, "/moved/api/v2/licenses", http.StatusTemporaryRedirect)
		case "/moved/api/v2/licenses":
			redirectedToken = r.Header.Get(codersdk.SessionTokenHeader)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	hasLicense, err := controller.NewSDKLicenseUploader().HasAnyLicense(context.Background(), server.URL, nil, "operator-token")
	require.NoError(t, err)
	require.False(t, hasLicense)
	require.Equal(t, "operator-token", redirectedToken)
}
