package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// creator is the Create method of the workspace and template storages.
type creator interface {
	Create(context.Context, runtime.Object, rest.ValidateObjectFunc, *metav1.CreateOptions) (runtime.Object, error)
}

// proxyAfterCreate fronts the mock Coder like a misbehaving proxy: Coder handles the create POST
// at createPath, but the client gets a 2xx HTML page instead of Coder's JSON answer.
func proxyAfterCreate(t *testing.T, coder *httptest.Server, createPath string, status int) *httptest.Server {
	t.Helper()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != createPath {
			coder.Config.Handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		coder.Config.Handler.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusCreated {
			t.Errorf("mock Coder answered the create with %d: %s", recorder.Code, recorder.Body.String())
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<html>proxy page</html>"))
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestCreateWithUnreadable2xxAnswerIsUncertain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		createPath string
		storage    func(t *testing.T, url string) creator
		object     runtime.Object
		created    func(state *mockCoderServerState) bool
	}{
		{
			name:       "workspace",
			createPath: "/api/v2/users/alice/workspaces",
			storage: func(t *testing.T, url string) creator {
				return NewWorkspaceStorage(newTestClientProvider(t, url))
			},
			object: &aggregationv1alpha1.CoderWorkspace{
				ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.proxied"},
				Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Organization: "acme", TemplateName: "starter-template", Running: true},
			},
			created: func(state *mockCoderServerState) bool { return state.hasWorkspace("alice", "proxied") },
		},
		{
			name: "template without files",
			storage: func(t *testing.T, url string) creator {
				return NewTemplateStorage(newTestClientProvider(t, url))
			},
			object: &aggregationv1alpha1.CoderTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "acme.proxied"},
				Spec:       aggregationv1alpha1.CoderTemplateSpec{Organization: "acme", VersionID: uuid.NewString()},
			},
			created: func(state *mockCoderServerState) bool {
				_, ok := state.templateActiveVersionID("acme", "proxied")
				return ok
			},
		},
		{
			name: "template with files",
			storage: func(t *testing.T, url string) creator {
				return NewTemplateStorage(newTestClientProvider(t, url))
			},
			object: &aggregationv1alpha1.CoderTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "acme.proxied"},
				Spec: aggregationv1alpha1.CoderTemplateSpec{
					Organization: "acme",
					Files:        map[string]string{"main.tf": `resource "null_resource" "proxied" {}`},
				},
			},
			created: func(state *mockCoderServerState) bool {
				_, ok := state.templateActiveVersionID("acme", "proxied")
				return ok
			},
		},
	} {
		for _, status := range []int{http.StatusOK, http.StatusCreated} {
			coderServer, state := newMockCoderServer(t)
			t.Cleanup(coderServer.Close)
			createPath := tc.createPath
			if createPath == "" {
				createPath = "/api/v2/organizations/" + state.organization.ID.String() + "/templates"
			}
			proxy := proxyAfterCreate(t, coderServer, createPath, status)
			storage := tc.storage(t, proxy.URL)

			_, err := storage.Create(namespacedContext("control-plane"), tc.object.DeepCopyObject(), rest.ValidateAllObjectFunc, nil)
			if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") || strings.Contains(err.Error(), proxy.URL) {
				t.Fatalf("%s, proxy status %d: expected an uncertain 504 without the Coder URL, got %v", tc.name, status, err)
			}
			if !tc.created(state) {
				t.Fatalf("%s, proxy status %d: the mock Coder must have created the object", tc.name, status)
			}
		}
	}
}
