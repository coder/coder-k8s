package apiserverapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func decodeStatus(t *testing.T, body string) metav1.Status {
	t.Helper()
	var status metav1.Status
	if err := json.Unmarshal([]byte(body), &status); err != nil || status.Kind != "Status" {
		t.Fatalf("decode Status %q: %v", body, err)
	}
	return status
}

// TestCoderAnswerRoutes sends broken Coder answers through the production handler chain (#242):
// writes whose result is open return the uncertain 504, and objects without the fields of their
// name return a 500 that names the problem, not the panic message of the generic apiserver.
func TestCoderAnswerRoutes(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) })
	cert := f.frontProxyCert(t)
	alice := remoteUser("alice")
	mock := f.server.mock
	coderHost := strings.TrimPrefix(mock.URL(), "http://")

	status, body := f.server.do(t, cert, http.MethodGet, workspacesTestNS+"/"+testWorkspaceName, alice, "")
	if status != http.StatusOK {
		t.Fatalf("get workspace: status=%d body=%s", status, body)
	}
	var workspace map[string]any
	if err := json.Unmarshal([]byte(body), &workspace); err != nil {
		t.Fatal(err)
	}
	workspace["spec"].(map[string]any)["running"] = false
	stopped, err := json.Marshal(workspace)
	if err != nil {
		t.Fatal(err)
	}

	gateway := func(code int) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(code)
			_, _ = w.Write([]byte("<html>proxy error</html>"))
		}
	}
	closeConnection := func(w http.ResponseWriter) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}
	jsonBody := func(code int, body string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}
	}

	for _, tc := range []struct {
		name        string
		method      string
		path        string
		body        string
		coderMethod string
		coderPath   func(string) bool
		answer      func(http.ResponseWriter)
		wantCode    int32
		wantMessage string
	}{
		{
			name: "workspace delete gets 502", method: http.MethodDelete, path: workspacesTestNS + "/" + testWorkspaceName,
			coderMethod: http.MethodPost, coderPath: func(p string) bool { return strings.HasSuffix(p, "/builds") },
			answer: gateway(http.StatusBadGateway), wantCode: http.StatusGatewayTimeout, wantMessage: "uncertain",
		},
		{
			name: "workspace update loses the connection", method: http.MethodPut, path: workspacesTestNS + "/" + testWorkspaceName, body: string(stopped),
			coderMethod: http.MethodPost, coderPath: func(p string) bool { return strings.HasSuffix(p, "/builds") },
			answer: closeConnection, wantCode: http.StatusGatewayTimeout, wantMessage: "uncertain",
		},
		{
			name: "template delete gets 503", method: http.MethodDelete, path: templatesTestNS + "/" + testTemplateName,
			coderMethod: http.MethodDelete, coderPath: func(p string) bool { return strings.HasPrefix(p, "/api/v2/templates/") },
			answer: gateway(http.StatusServiceUnavailable), wantCode: http.StatusGatewayTimeout, wantMessage: "uncertain",
		},
		{
			name: "template create gets 201 with {}", method: http.MethodPost, path: templatesTestNS,
			body:        `{"apiVersion":"aggregation.coder.com/v1alpha1","kind":"CoderTemplate","metadata":{"name":"default.proxied"},"spec":{"organization":"default","versionID":"33333333-3333-3333-3333-333333333333"}}`,
			coderMethod: http.MethodPost, coderPath: func(p string) bool { return strings.HasSuffix(p, "/templates") },
			answer: jsonBody(http.StatusCreated, `{}`), wantCode: http.StatusGatewayTimeout, wantMessage: "uncertain",
		},
		{
			name: "workspace list gets an object without names", method: http.MethodGet, path: workspacesTestNS,
			coderMethod: http.MethodGet, coderPath: func(p string) bool { return p == "/api/v2/workspaces" },
			answer: jsonBody(http.StatusOK, `{"workspaces":[{}],"count":1}`), wantCode: http.StatusInternalServerError, wantMessage: "the Coder API answered",
		},
		{
			name: "template get gets a template without an organization name", method: http.MethodGet, path: templatesTestNS + "/" + testTemplateName,
			coderMethod: http.MethodGet, coderPath: func(p string) bool { return strings.HasSuffix(p, "/templates/my-template") },
			answer: jsonBody(http.StatusOK, `{"name":"my-template"}`), wantCode: http.StatusInternalServerError, wantMessage: "the Coder API answered",
		},
	} {
		override := func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != tc.coderMethod || !tc.coderPath(r.URL.Path) {
				return false
			}
			tc.answer(w)
			return true
		}
		mock.override.Store(&override)

		code, body := f.server.do(t, cert, tc.method, tc.path, alice, tc.body)
		mock.override.Store(nil)
		got := decodeStatus(t, body)
		if code != int(tc.wantCode) || got.Code != tc.wantCode || !strings.Contains(got.Message, tc.wantMessage) || strings.Contains(body, coderHost) {
			t.Errorf("%s: expected %d with %q and without the Coder URL, got %d: %s", tc.name, tc.wantCode, tc.wantMessage, code, body)
		}
	}
}
