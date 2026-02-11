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

func TestEnsureProvisionerKey_Create(t *testing.T) {
	t.Parallel()

	const keyName = "provisioner-key"
	orgID := uuid.New()
	keyID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)

	createCalls := 0
	listCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/default":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				"id":         orgID.String(),
				"name":       "default",
				"created_at": now,
				"updated_at": now,
			})
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/"+orgID.String()+"/provisionerkeys":
			listCalls++
			if createCalls == 0 {
				writeJSONResponse(t, w, http.StatusOK, []any{})
				return
			}
			writeJSONResponse(t, w, http.StatusOK, []map[string]any{{
				"id":           keyID.String(),
				"name":         keyName,
				"organization": orgID.String(),
				"created_at":   now,
				"tags": map[string]string{
					"cluster": "dev",
				},
			}})
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/organizations/"+orgID.String()+"/provisionerkeys":
			createCalls++
			var payload struct {
				Name string            `json:"name"`
				Tags map[string]string `json:"tags"`
			}
			err := json.NewDecoder(r.Body).Decode(&payload)
			require.NoError(t, err)
			require.Equal(t, keyName, payload.Name)
			require.Equal(t, map[string]string{"cluster": "dev"}, payload.Tags)

			writeJSONResponse(t, w, http.StatusCreated, map[string]any{
				"key": "plaintext-provisioner-key",
			})
			return
		default:
			writeJSONResponse(t, w, http.StatusNotFound, map[string]any{
				"message": "unexpected route",
			})
			return
		}
	}))
	defer server.Close()

	client := coderbootstrap.NewSDKClient()
	resp, err := client.EnsureProvisionerKey(context.Background(), coderbootstrap.EnsureProvisionerKeyRequest{
		CoderURL:     server.URL,
		SessionToken: "session-token",
		KeyName:      keyName,
		Tags: map[string]string{
			"cluster": "dev",
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, createCalls)
	require.Equal(t, 2, listCalls)
	require.Equal(t, orgID, resp.OrganizationID)
	require.Equal(t, keyID, resp.KeyID)
	require.Equal(t, keyName, resp.KeyName)
	require.Equal(t, "plaintext-provisioner-key", resp.Key)
}

func TestEnsureProvisionerKey_Exists(t *testing.T) {
	t.Parallel()

	const (
		orgName = "engineering"
		keyName = "existing-key"
	)
	orgID := uuid.New()
	keyID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	createCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/"+orgName:
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				"id":         orgID.String(),
				"name":       orgName,
				"created_at": now,
				"updated_at": now,
			})
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/"+orgID.String()+"/provisionerkeys":
			writeJSONResponse(t, w, http.StatusOK, []map[string]any{{
				"id":           keyID.String(),
				"name":         keyName,
				"organization": orgID.String(),
				"created_at":   now,
				"tags":         map[string]string{"cluster": "prod"},
			}})
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/organizations/"+orgID.String()+"/provisionerkeys":
			createCalled = true
			writeJSONResponse(t, w, http.StatusCreated, map[string]any{"key": "should-not-be-created"})
			return
		default:
			writeJSONResponse(t, w, http.StatusNotFound, map[string]any{"message": "unexpected route"})
			return
		}
	}))
	defer server.Close()

	client := coderbootstrap.NewSDKClient()
	resp, err := client.EnsureProvisionerKey(context.Background(), coderbootstrap.EnsureProvisionerKeyRequest{
		CoderURL:         server.URL,
		SessionToken:     "session-token",
		OrganizationName: orgName,
		KeyName:          keyName,
	})
	require.NoError(t, err)
	require.False(t, createCalled)
	require.Equal(t, orgID, resp.OrganizationID)
	require.Equal(t, keyID, resp.KeyID)
	require.Equal(t, keyName, resp.KeyName)
	require.Empty(t, resp.Key)
}

func TestEnsureProvisionerKey_ValidationErrors(t *testing.T) {
	t.Parallel()

	client := coderbootstrap.NewSDKClient()
	tests := []struct {
		name      string
		request   coderbootstrap.EnsureProvisionerKeyRequest
		errSubstr string
	}{
		{
			name: "missing coder URL",
			request: coderbootstrap.EnsureProvisionerKeyRequest{
				SessionToken: "session-token",
				KeyName:      "key",
			},
			errSubstr: "coder URL is required",
		},
		{
			name: "missing session token",
			request: coderbootstrap.EnsureProvisionerKeyRequest{
				CoderURL: "https://coder.example.com",
				KeyName:  "key",
			},
			errSubstr: "session token is required",
		},
		{
			name: "missing key name",
			request: coderbootstrap.EnsureProvisionerKeyRequest{
				CoderURL:     "https://coder.example.com",
				SessionToken: "session-token",
			},
			errSubstr: "provisioner key name is required",
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := client.EnsureProvisionerKey(context.Background(), testCase.request)
			require.Error(t, err)
			require.Contains(t, err.Error(), testCase.errSubstr)
		})
	}
}

func TestDeleteProvisionerKey_Success(t *testing.T) {
	t.Parallel()

	const keyName = "delete-me"
	orgID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	deleteCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/default":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				"id":         orgID.String(),
				"name":       "default",
				"created_at": now,
				"updated_at": now,
			})
			return
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2/organizations/"+orgID.String()+"/provisionerkeys/"+keyName:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			writeJSONResponse(t, w, http.StatusNotFound, map[string]any{"message": "unexpected route"})
			return
		}
	}))
	defer server.Close()

	client := coderbootstrap.NewSDKClient()
	err := client.DeleteProvisionerKey(context.Background(), server.URL, "session-token", "", keyName)
	require.NoError(t, err)
	require.Equal(t, 1, deleteCalls)
}

func TestDeleteProvisionerKey_NotFound(t *testing.T) {
	t.Parallel()

	const keyName = "already-deleted"
	orgID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	deleteCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/default":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				"id":         orgID.String(),
				"name":       "default",
				"created_at": now,
				"updated_at": now,
			})
			return
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2/organizations/"+orgID.String()+"/provisionerkeys/"+keyName:
			deleteCalls++
			writeJSONResponse(t, w, http.StatusNotFound, map[string]any{
				"message": "provisioner key not found",
			})
			return
		default:
			writeJSONResponse(t, w, http.StatusNotFound, map[string]any{"message": "unexpected route"})
			return
		}
	}))
	defer server.Close()

	client := coderbootstrap.NewSDKClient()
	err := client.DeleteProvisionerKey(context.Background(), server.URL, "session-token", "", keyName)
	require.NoError(t, err)
	require.Equal(t, 1, deleteCalls)
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, statusCode int, payload any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	err := json.NewEncoder(w).Encode(payload)
	require.NoError(t, err)
}
