package coderbootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

func TestDeleteProvisionerKeyByID(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	recordedKeyID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	keysPath := "/api/v2/organizations/" + orgID.String() + "/provisionerkeys"

	tests := []struct {
		name        string
		listStatus  int
		listedID    uuid.UUID
		wantDeletes []string
	}{
		{name: "recorded key exists", listStatus: http.StatusOK, listedID: recordedKeyID, wantDeletes: []string{keysPath + "/shared-name"}},
		{name: "same name, other ID", listStatus: http.StatusOK, listedID: uuid.New()},
		{name: "organization gone", listStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var deletes []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == keysPath:
					if tt.listStatus != http.StatusOK {
						writeJSONResponse(t, w, tt.listStatus, map[string]any{"message": "not found"})
						return
					}
					writeJSONResponse(t, w, http.StatusOK, []map[string]any{{
						"id":           tt.listedID.String(),
						"created_at":   now,
						"organization": orgID.String(),
						"name":         "shared-name",
						"tags":         map[string]string{},
					}})
				case r.Method == http.MethodDelete:
					mu.Lock()
					deletes = append(deletes, r.URL.Path)
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			err := coderbootstrap.NewSDKClient().DeleteProvisionerKeyByID(context.Background(), coderbootstrap.DeleteProvisionerKeyByIDRequest{
				CoderURL:       server.URL,
				SessionToken:   "session-token",
				OrganizationID: orgID,
				KeyID:          recordedKeyID,
			})
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, tt.wantDeletes, deletes)
		})
	}
}

func TestDeleteProvisionerKeyByID_ValidationErrors(t *testing.T) {
	t.Parallel()

	valid := coderbootstrap.DeleteProvisionerKeyByIDRequest{
		CoderURL:       "https://coder.example.com",
		SessionToken:   "session-token",
		OrganizationID: uuid.New(),
		KeyID:          uuid.New(),
	}
	tests := []struct {
		name    string
		mutate  func(*coderbootstrap.DeleteProvisionerKeyByIDRequest)
		wantErr string
	}{
		{name: "coder URL", mutate: func(r *coderbootstrap.DeleteProvisionerKeyByIDRequest) { r.CoderURL = "" }, wantErr: "coder URL is required"},
		{name: "session token", mutate: func(r *coderbootstrap.DeleteProvisionerKeyByIDRequest) { r.SessionToken = "" }, wantErr: "session token is required"},
		{name: "organization ID", mutate: func(r *coderbootstrap.DeleteProvisionerKeyByIDRequest) { r.OrganizationID = uuid.Nil }, wantErr: "organization ID is required"},
		{name: "key ID", mutate: func(r *coderbootstrap.DeleteProvisionerKeyByIDRequest) { r.KeyID = uuid.Nil }, wantErr: "provisioner key ID is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := valid
			tt.mutate(&req)
			err := coderbootstrap.NewSDKClient().DeleteProvisionerKeyByID(context.Background(), req)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
