package storage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// TestCoderAnswersWithoutNamesFailCleanly checks that a Coder answer whose objects lack the fields
// of the Kubernetes name, for example a proxy that answers {} (#242 case 5), returns a 500 that
// names the problem instead of a panic.
func TestCoderAnswersWithoutNamesFailCleanly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// isRead matches the Coder read that gets body instead of Coder's answer.
		isRead func(*http.Request) bool
		body   func(state *mockCoderServerState) string
		read   func(provider coder.ClientProvider) (runtime.Object, error)
	}{
		{
			name:   "workspace list",
			isRead: func(r *http.Request) bool { return r.URL.Path == "/api/v2/workspaces" },
			body:   func(*mockCoderServerState) string { return `{"workspaces":[{}],"count":1}` },
			read: func(provider coder.ClientProvider) (runtime.Object, error) {
				return NewWorkspaceStorage(provider).List(namespacedContext("control-plane"), nil)
			},
		},
		{
			name:   "template list",
			isRead: func(r *http.Request) bool { return r.URL.Path == "/api/v2/templates" },
			body:   func(*mockCoderServerState) string { return `[{}]` },
			read: func(provider coder.ClientProvider) (runtime.Object, error) {
				return NewTemplateStorage(provider).List(namespacedContext("control-plane"), nil)
			},
		},
		{
			name: "template get",
			isRead: func(r *http.Request) bool {
				return strings.HasSuffix(r.URL.Path, "/templates/starter-template")
			},
			body: func(*mockCoderServerState) string { return `{"name":"starter-template"}` },
			read: func(provider coder.ClientProvider) (runtime.Object, error) {
				return NewTemplateStorage(provider).Get(namespacedContext("control-plane"), "acme.starter-template", nil)
			},
		},
		{
			name:   "template version list",
			isRead: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/versions") },
			body: func(state *mockCoderServerState) string {
				for id := range state.templatesByID {
					return `[{"template_id":"` + id.String() + `"}]`
				}
				panic("assertion failed: the mock Coder must have a template")
			},
			read: func(provider coder.ClientProvider) (runtime.Object, error) {
				return NewTemplateVersionStorage(provider).List(namespacedContext("control-plane"), nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			coderServer, state := newMockCoderServer(t)
			t.Cleanup(coderServer.Close)
			body := tc.body(state)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !tc.isRead(r) {
					coderServer.Config.Handler.ServeHTTP(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(proxy.Close)

			var err error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("the read panicked: %v", recovered)
					}
				}()
				_, err = tc.read(newTestClientProvider(t, proxy.URL))
			}()
			if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), "the Coder API answered") {
				t.Fatalf("expected a 500 that names the Coder answer, got %v", err)
			}
		})
	}
}
