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

// coderNotFound is the body of the 404 that Coder answers for a missing
// organization or provisioner key.
var coderNotFound = map[string]any{"message": "Resource not found or you do not have access to this resource"}

func TestDeleteProvisionerKeyByID(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	recordedKeyID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	keysPath := "/api/v2/organizations/" + orgID.String() + "/provisionerkeys"

	tests := []struct {
		name        string
		listStatus  int
		listBody    map[string]any
		listedID    uuid.UUID
		wantDeletes []string
		wantErr     bool
	}{
		{name: "recorded key exists", listStatus: http.StatusOK, listedID: recordedKeyID, wantDeletes: []string{keysPath + "/shared-name"}},
		{name: "same name, other ID", listStatus: http.StatusOK, listedID: uuid.New()},
		{name: "organization gone", listStatus: http.StatusNotFound, listBody: coderNotFound},
		{name: "404 not from Coder", listStatus: http.StatusNotFound, listBody: map[string]any{"message": "Route not found."}, wantErr: true},
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
						writeJSONResponse(t, w, tt.listStatus, tt.listBody)
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
			if tt.wantErr {
				require.Error(t, err, "only Coder's own 404 proves that the key is gone")
			} else {
				require.NoError(t, err)
			}
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, tt.wantDeletes, deletes)
		})
	}
}

func TestProvisionerKeyByID(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	recordedKeyID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	keysPath := "/api/v2/organizations/" + orgID.String() + "/provisionerkeys"

	tests := []struct {
		name       string
		listStatus int
		listBody   map[string]any
		listedID   uuid.UUID
		wantKey    bool
		wantErr    bool
	}{
		{name: "recorded key exists", listStatus: http.StatusOK, listedID: recordedKeyID, wantKey: true},
		{name: "same name, other ID", listStatus: http.StatusOK, listedID: uuid.New()},
		{name: "organization gone", listStatus: http.StatusNotFound, listBody: coderNotFound},
		{name: "404 not from Coder", listStatus: http.StatusNotFound, listBody: map[string]any{"message": "Route not found."}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != keysPath {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if tt.listStatus != http.StatusOK {
					writeJSONResponse(t, w, tt.listStatus, tt.listBody)
					return
				}
				writeJSONResponse(t, w, http.StatusOK, []map[string]any{{
					"id":           tt.listedID.String(),
					"created_at":   now,
					"organization": orgID.String(),
					"name":         "shared-name",
					"tags":         map[string]string{},
				}})
			}))
			defer server.Close()

			key, err := coderbootstrap.NewSDKClient().ProvisionerKeyByID(context.Background(), coderbootstrap.ProvisionerKeyByIDRequest{
				CoderURL:       server.URL,
				SessionToken:   "session-token",
				OrganizationID: orgID,
				KeyID:          recordedKeyID,
			})
			if tt.wantErr {
				require.Error(t, err, "only Coder's own 404 proves that the key is gone")
				return
			}
			require.NoError(t, err)
			if !tt.wantKey {
				require.Nil(t, key)
				return
			}
			require.NotNil(t, key)
			require.Equal(t, recordedKeyID, key.ID)
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

func TestProvisionerKeyByName(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	listedID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339)
	keysPath := "/api/v2/organizations/" + orgID.String() + "/provisionerkeys"

	tests := []struct {
		name       string
		orgGone    bool
		listBody   map[string]any
		listedName string
		wantKeyID  uuid.UUID
		wantErr    bool
	}{
		{name: "organization gone", orgGone: true},
		{name: "organization gone before the list", listBody: coderNotFound},
		{name: "key exists", listedName: "provisioner", wantKeyID: listedID},
		{name: "no key with the name", listedName: "other"},
		{name: "404 not from Coder", listBody: map[string]any{"message": "Route not found."}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/default" && tt.orgGone:
					writeJSONResponse(t, w, http.StatusNotFound, coderNotFound)
				case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/default":
					writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": orgID.String(), "name": "default", "created_at": now, "updated_at": now})
				case r.Method == http.MethodGet && r.URL.Path == keysPath && tt.listBody != nil:
					writeJSONResponse(t, w, http.StatusNotFound, tt.listBody)
				case r.Method == http.MethodGet && r.URL.Path == keysPath:
					writeJSONResponse(t, w, http.StatusOK, []map[string]any{{
						"id":           listedID.String(),
						"created_at":   now,
						"organization": orgID.String(),
						"name":         tt.listedName,
						"tags":         map[string]string{},
					}})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			key, err := coderbootstrap.NewSDKClient().ProvisionerKeyByName(context.Background(), coderbootstrap.ProvisionerKeyByNameRequest{
				CoderURL:     server.URL,
				SessionToken: "session-token",
				KeyName:      "provisioner",
			})
			if tt.wantErr {
				require.Error(t, err, "only Coder's own 404 proves that the key is gone")
				return
			}
			require.NoError(t, err)
			if tt.wantKeyID == uuid.Nil {
				require.Nil(t, key)
				return
			}
			require.NotNil(t, key)
			require.Equal(t, tt.wantKeyID, key.ID)
		})
	}
}
