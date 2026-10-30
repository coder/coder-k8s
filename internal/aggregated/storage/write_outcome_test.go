package storage

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// brokenAnswer replaces Coder's answer to a write that Coder has already applied.
type brokenAnswer func(t *testing.T, w http.ResponseWriter, coderAnswer *httptest.ResponseRecorder)

func gatewayPage(status int) brokenAnswer {
	return func(_ *testing.T, w http.ResponseWriter, _ *httptest.ResponseRecorder) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<html>proxy error</html>"))
	}
}

func jsonAnswer(status int, body string) brokenAnswer {
	return func(_ *testing.T, w http.ResponseWriter, _ *httptest.ResponseRecorder) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func hijack(t *testing.T, w http.ResponseWriter) net.Conn {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	return conn
}

// truncatedAnswer sends Coder's status and half of its body, then closes the connection.
func truncatedAnswer(t *testing.T, w http.ResponseWriter, coderAnswer *httptest.ResponseRecorder) {
	body := coderAnswer.Body.Bytes()
	conn := hijack(t, w)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n",
		coderAnswer.Code, http.StatusText(coderAnswer.Code), len(body))
	_, _ = conn.Write(body[:len(body)/2])
}

func closedWithoutAnswer(t *testing.T, w http.ResponseWriter, _ *httptest.ResponseRecorder) {
	_ = hijack(t, w).Close()
}

// writeOutcomeServer fronts the mock Coder. Coder handles every request, and the write that
// isWrite matches gets answer instead of Coder's answer.
func writeOutcomeServer(t *testing.T, coderServer *httptest.Server, isWrite func(*http.Request) bool, answer brokenAnswer) *httptest.Server {
	t.Helper()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWrite(r) {
			coderServer.Config.Handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		coderServer.Config.Handler.ServeHTTP(recorder, r)
		if recorder.Code >= http.StatusMultipleChoices {
			t.Errorf("mock Coder answered %s %s with %d: %s", r.Method, r.URL.Path, recorder.Code, recorder.Body.String())
		}
		answer(t, w, recorder)
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

// refusingWrites sends the write that isWrite matches to a closed port, so the connection is
// refused before the request reaches Coder.
type refusingWrites struct {
	isWrite    func(*http.Request) bool
	closedAddr string
}

func (rt refusingWrites) RoundTrip(r *http.Request) (*http.Response, error) {
	if rt.isWrite(r) {
		r = r.Clone(r.Context())
		r.URL.Host = rt.closedAddr
		r.Host = ""
	}
	return http.DefaultTransport.RoundTrip(r)
}

func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

type writeCase struct {
	name    string
	isWrite func(*http.Request) bool
	// checksAnswer is true when the storage needs the object in Coder's answer: the created object,
	// or the build of a workspace update or delete.
	checksAnswer bool
	// readsBody is false when the SDK ignores the body of a successful answer.
	readsBody bool
	run       func(t *testing.T, provider coder.ClientProvider) error
}

func writeCases() []writeCase {
	ctx := func() context.Context { return namespacedContext("control-plane") }
	isPost := func(suffix string) func(*http.Request) bool {
		return func(r *http.Request) bool {
			return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, suffix)
		}
	}
	isBuild := isPost("/builds")

	return []writeCase{
		{
			name: "workspace create",
			isWrite: func(r *http.Request) bool {
				return r.Method == http.MethodPost && r.URL.Path == "/api/v2/users/alice/workspaces"
			},
			checksAnswer: true,
			readsBody:    true,
			run: func(_ *testing.T, provider coder.ClientProvider) error {
				_, err := NewWorkspaceStorage(provider).Create(ctx(), &aggregationv1alpha1.CoderWorkspace{
					ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.proxied"},
					Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Organization: "acme", TemplateName: "starter-template", Running: true},
				}, rest.ValidateAllObjectFunc, nil)
				return err
			},
		},
		{
			name:         "workspace update",
			isWrite:      isBuild,
			checksAnswer: true,
			readsBody:    true,
			run: func(t *testing.T, provider coder.ClientProvider) error {
				workspaces := NewWorkspaceStorage(provider)
				current, err := workspaces.Get(ctx(), "acme.alice.dev-workspace", nil)
				if err != nil {
					t.Fatalf("get workspace: %v", err)
				}
				desired := current.(*aggregationv1alpha1.CoderWorkspace).DeepCopy()
				desired.Spec.Running = false
				_, _, err = workspaces.Update(ctx(), desired.Name, rest.DefaultUpdatedObjectInfo(desired), nil, nil, false, nil)
				return err
			},
		},
		{
			name:         "workspace delete",
			isWrite:      isBuild,
			checksAnswer: true,
			readsBody:    true,
			run: func(_ *testing.T, provider coder.ClientProvider) error {
				_, _, err := NewWorkspaceStorage(provider).Delete(ctx(), "acme.alice.dev-workspace", nil, nil)
				return err
			},
		},
		{
			name:         "template create",
			isWrite:      isPost("/templates"),
			checksAnswer: true,
			readsBody:    true,
			run: func(_ *testing.T, provider coder.ClientProvider) error {
				_, err := NewTemplateStorage(provider).Create(ctx(), &aggregationv1alpha1.CoderTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "acme.proxied"},
					Spec: aggregationv1alpha1.CoderTemplateSpec{
						Organization: "acme",
						Files:        map[string]string{"main.tf": `resource "null_resource" "proxied" {}`},
					},
				}, rest.ValidateAllObjectFunc, nil)
				return err
			},
		},
		{
			name: "template update",
			isWrite: func(r *http.Request) bool {
				return r.Method == http.MethodPatch && !strings.HasSuffix(r.URL.Path, "/versions")
			},
			readsBody: true,
			run: func(t *testing.T, provider coder.ClientProvider) error {
				templates := NewTemplateStorage(provider)
				current, err := templates.Get(ctx(), "acme.starter-template", nil)
				if err != nil {
					t.Fatalf("get template: %v", err)
				}
				desired := current.(*aggregationv1alpha1.CoderTemplate).DeepCopy()
				desired.Spec.DisplayName = "Renamed behind a proxy"
				_, _, err = templates.Update(ctx(), desired.Name, rest.DefaultUpdatedObjectInfo(desired), nil, nil, false, nil)
				return err
			},
		},
		{
			name: "template activation of new files",
			isWrite: func(r *http.Request) bool {
				return r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/versions")
			},
			run: func(t *testing.T, provider coder.ClientProvider) error {
				templates := NewTemplateStorage(provider)
				current, err := templates.Get(ctx(), "acme.starter-template", nil)
				if err != nil {
					t.Fatalf("get template: %v", err)
				}
				desired := current.(*aggregationv1alpha1.CoderTemplate).DeepCopy()
				desired.Spec.Files = map[string]string{"main.tf": `resource "null_resource" "changed" {}`}
				_, _, err = templates.Update(ctx(), desired.Name, rest.DefaultUpdatedObjectInfo(desired), nil, nil, false, nil)
				return err
			},
		},
		{
			name:    "template delete",
			isWrite: func(r *http.Request) bool { return r.Method == http.MethodDelete },
			run: func(_ *testing.T, provider coder.ClientProvider) error {
				_, _, err := NewTemplateStorage(provider).Delete(ctx(), "acme.starter-template", nil, nil)
				return err
			},
		},
	}
}

// TestWriteOutcomes checks every Coder write against answers that leave its result open (#242).
// Each of them returns the uncertain 504 without the Coder URL. A connection that is refused
// before the request is sent keeps the 500 that says that Coder could not be reached.
func TestWriteOutcomes(t *testing.T) {
	t.Parallel()

	type outcome struct {
		name   string
		answer brokenAnswer
		// applies reports whether the outcome breaks the result of the write. A body that the
		// SDK does not read, or an object that the storage does not use, leaves it intact.
		applies func(writeCase) bool
	}
	always := func(writeCase) bool { return true }
	readsBody := func(w writeCase) bool { return w.readsBody }
	checksAnswer := func(w writeCase) bool { return w.checksAnswer }
	outcomes := []outcome{
		{"proxy 502", gatewayPage(http.StatusBadGateway), always},
		{"proxy 503", gatewayPage(http.StatusServiceUnavailable), always},
		{"connection closed without an answer", closedWithoutAnswer, always},
		{"body breaks off", truncatedAnswer, readsBody},
		{"200 with malformed JSON", jsonAnswer(http.StatusOK, `{"id":`), readsBody},
		{"201 with an empty object", jsonAnswer(http.StatusCreated, `{}`), checksAnswer},
		{"201 with null", jsonAnswer(http.StatusCreated, `null`), checksAnswer},
	}

	for _, write := range writeCases() {
		for _, out := range outcomes {
			if !out.applies(write) {
				continue
			}
			t.Run(write.name+"/"+out.name, func(t *testing.T) {
				t.Parallel()
				coderServer, _ := newMockCoderServer(t)
				t.Cleanup(coderServer.Close)
				proxy := writeOutcomeServer(t, coderServer, write.isWrite, out.answer)

				err := write.run(t, newTestClientProvider(t, proxy.URL))
				if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") ||
					strings.Contains(err.Error(), strings.TrimPrefix(proxy.URL, "http://")) {
					t.Fatalf("expected an uncertain 504 without the Coder URL, got %v", err)
				}
			})
		}

		t.Run(write.name+"/connection refused before the send", func(t *testing.T) {
			t.Parallel()
			coderServer, _ := newMockCoderServer(t)
			t.Cleanup(coderServer.Close)
			closedAddr := closedAddress(t)
			client := newTestSDKClient(t, coderServer.URL)
			client.HTTPClient = &http.Client{Transport: refusingWrites{isWrite: write.isWrite, closedAddr: closedAddr}}

			err := write.run(t, &coder.StaticClientProvider{Client: client, Namespace: "control-plane"})
			if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), "could not be reached") ||
				strings.Contains(err.Error(), closedAddr) {
				t.Fatalf("expected the 500 that says Coder could not be reached, got %v", err)
			}
		})
	}
}
