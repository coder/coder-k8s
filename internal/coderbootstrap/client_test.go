package coderbootstrap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

type proxyResponse struct {
	Proxy struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		DisplayName      string `json:"display_name"`
		IconURL          string `json:"icon_url"`
		Healthy          bool   `json:"healthy"`
		PathAppURL       string `json:"path_app_url"`
		WildcardHostname string `json:"wildcard_hostname"`
		Status           struct {
			Status    string `json:"status"`
			CheckedAt string `json:"checked_at"`
			Report    struct {
				Errors   []string `json:"errors"`
				Warnings []string `json:"warnings"`
			} `json:"report"`
		} `json:"status"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		Deleted   bool   `json:"deleted"`
		Version   string `json:"version"`
	} `json:"proxy"`
	ProxyToken string `json:"proxy_token"`
}

func TestEnsureWorkspaceProxyCreatesProxyWhenMissing(t *testing.T) {
	t.Parallel()

	const (
		proxyName = "proxy-one"
		iconPath  = "/emojis/1f5fa.png"
	)

	created := false
	now := time.Now().UTC().Format(time.RFC3339)
	proxyID := uuid.NewString()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/workspaceproxies/"+proxyName:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/workspaceproxies":
			var payload struct {
				Name        string `json:"name"`
				DisplayName string `json:"display_name"`
				Icon        string `json:"icon"`
			}
			err := json.NewDecoder(r.Body).Decode(&payload)
			require.NoError(t, err)
			require.Equal(t, proxyName, payload.Name)
			require.Equal(t, "Proxy One", payload.DisplayName)
			require.Equal(t, iconPath, payload.Icon)
			created = true

			response := proxyResponse{}
			response.Proxy.ID = proxyID
			response.Proxy.Name = payload.Name
			response.Proxy.DisplayName = payload.DisplayName
			response.Proxy.IconURL = payload.Icon
			response.Proxy.Healthy = true
			response.Proxy.PathAppURL = "https://proxy-one.example.com"
			response.Proxy.WildcardHostname = "*.proxy-one.example.com"
			response.Proxy.Status.Status = "unregistered"
			response.Proxy.Status.CheckedAt = now
			response.Proxy.Status.Report.Errors = []string{}
			response.Proxy.Status.Report.Warnings = []string{}
			response.Proxy.CreatedAt = now
			response.Proxy.UpdatedAt = now
			response.Proxy.Deleted = false
			response.Proxy.Version = "2.0.0"
			response.ProxyToken = "token-created"

			w.WriteHeader(http.StatusCreated)
			err = json.NewEncoder(w).Encode(response)
			require.NoError(t, err)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "unexpected route"})
			return
		}
	}))
	defer server.Close()

	client := coderbootstrap.NewSDKClient()
	result, err := client.EnsureWorkspaceProxy(context.Background(), coderbootstrap.RegisterWorkspaceProxyRequest{
		CoderURL:     server.URL,
		SessionToken: "session-token",
		ProxyName:    proxyName,
		DisplayName:  "Proxy One",
		Icon:         iconPath,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, proxyName, result.ProxyName)
	require.Equal(t, "token-created", result.ProxyToken)
}

func TestEnsureWorkspaceProxyUpdatesExistingProxy(t *testing.T) {
	t.Parallel()

	const proxyName = "proxy-updated"
	proxyID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	patched := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/workspaceproxies/"+proxyName:
			response := map[string]any{
				"id":                proxyID,
				"name":              proxyName,
				"display_name":      "Existing Proxy",
				"icon_url":          "/emojis/1f5fa.png",
				"healthy":           true,
				"path_app_url":      "https://proxy.example.com",
				"wildcard_hostname": "*.proxy.example.com",
				"status": map[string]any{
					"status":     "ok",
					"checked_at": now,
					"report": map[string]any{
						"errors":   []string{},
						"warnings": []string{},
					},
				},
				"created_at": now,
				"updated_at": now,
				"deleted":    false,
				"version":    "2.0.0",
			}
			w.WriteHeader(http.StatusOK)
			err := json.NewEncoder(w).Encode(response)
			require.NoError(t, err)
			return
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v2/workspaceproxies/"+proxyID:
			var payload map[string]any
			err := json.NewDecoder(r.Body).Decode(&payload)
			require.NoError(t, err)
			require.Equal(t, true, payload["regenerate_token"])
			patched = true

			response := proxyResponse{}
			response.Proxy.ID = proxyID
			response.Proxy.Name = proxyName
			response.Proxy.DisplayName = "Updated Proxy"
			response.Proxy.IconURL = "/emojis/1f916.png"
			response.Proxy.Healthy = true
			response.Proxy.PathAppURL = "https://proxy.example.com"
			response.Proxy.WildcardHostname = "*.proxy.example.com"
			response.Proxy.Status.Status = "ok"
			response.Proxy.Status.CheckedAt = now
			response.Proxy.Status.Report.Errors = []string{}
			response.Proxy.Status.Report.Warnings = []string{}
			response.Proxy.CreatedAt = now
			response.Proxy.UpdatedAt = now
			response.Proxy.Version = "2.0.0"
			response.ProxyToken = "token-updated"

			w.WriteHeader(http.StatusOK)
			err = json.NewEncoder(w).Encode(response)
			require.NoError(t, err)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "unexpected route"})
			return
		}
	}))
	defer server.Close()

	client := coderbootstrap.NewSDKClient()
	result, err := client.EnsureWorkspaceProxy(context.Background(), coderbootstrap.RegisterWorkspaceProxyRequest{
		CoderURL:     server.URL,
		SessionToken: "session-token",
		ProxyName:    proxyName,
		DisplayName:  "Updated Proxy",
		Icon:         "/emojis/1f916.png",
	})
	require.NoError(t, err)
	require.True(t, patched)
	require.Equal(t, proxyName, result.ProxyName)
	require.Equal(t, "token-updated", result.ProxyToken)
}

func TestEnsureWorkspaceProxyValidatesInputs(t *testing.T) {
	t.Parallel()

	client := coderbootstrap.NewSDKClient()

	_, err := client.EnsureWorkspaceProxy(context.Background(), coderbootstrap.RegisterWorkspaceProxyRequest{})
	require.Error(t, err)
}
