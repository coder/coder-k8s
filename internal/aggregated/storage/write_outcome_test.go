package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrl "sigs.k8s.io/controller-runtime"

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

// answerWithoutID sends Coder's answer without its "id" field.
func answerWithoutID(t *testing.T, w http.ResponseWriter, coderAnswer *httptest.ResponseRecorder) {
	var object map[string]any
	if err := json.Unmarshal(coderAnswer.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode Coder answer: %v", err)
	}
	delete(object, "id")
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	jsonAnswer(coderAnswer.Code, string(body))(t, w, coderAnswer)
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

// closedAddress returns an address that refuses connections. Port 1 is outside the ephemeral
// port range, so a server of a parallel test cannot take it, as it can take a port that a test
// closed.
func closedAddress(_ *testing.T) string {
	return "127.0.0.1:1"
}

type writeCase struct {
	name    string
	isWrite func(*http.Request) bool
	// checksAnswer is true when the storage checks the object in Coder's answer: the created
	// object, the build of a workspace update or delete, or the template of a metadata update.
	checksAnswer bool
	// postsBuild is true when the write is a workspace build POST.
	postsBuild bool
	// readsBody is false when the SDK ignores the body of a successful answer.
	readsBody bool
	run       func(t *testing.T, provider coder.ClientProvider) error
}

// createTemplate creates a template with files. The create creates a template version and then
// the template.
func createTemplate(_ *testing.T, provider coder.ClientProvider) error {
	_, err := NewTemplateStorage(provider).Create(namespacedContext("control-plane"), &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "acme.proxied"},
		Spec: aggregationv1alpha1.CoderTemplateSpec{
			Organization: "acme",
			Files:        map[string]string{"main.tf": `resource "null_resource" "proxied" {}`},
		},
	}, rest.ValidateAllObjectFunc, nil)
	return err
}

// updateTemplateFiles changes the files of a template. The update creates a template version
// and then activates it.
func updateTemplateFiles(t *testing.T, provider coder.ClientProvider) error {
	ctx := namespacedContext("control-plane")
	templates := NewTemplateStorage(provider)
	current, err := templates.Get(ctx, "acme.starter-template", nil)
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	desired := current.(*aggregationv1alpha1.CoderTemplate).DeepCopy()
	desired.Spec.Files = map[string]string{"main.tf": `resource "null_resource" "changed" {}`}
	_, _, err = templates.Update(ctx, desired.Name, rest.DefaultUpdatedObjectInfo(desired), nil, nil, false, nil)
	return err
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
			postsBuild:   true,
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
			postsBuild:   true,
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
			run:          createTemplate,
		},
		{
			name:         "template version of a create",
			isWrite:      isPost("/templateversions"),
			checksAnswer: true,
			readsBody:    true,
			run:          createTemplate,
		},
		{
			name: "template update",
			isWrite: func(r *http.Request) bool {
				return r.Method == http.MethodPatch && !strings.HasSuffix(r.URL.Path, "/versions")
			},
			checksAnswer: true,
			readsBody:    true,
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
			name:         "template version of new files",
			isWrite:      isPost("/templateversions"),
			checksAnswer: true,
			readsBody:    true,
			run:          updateTemplateFiles,
		},
		{
			name: "template activation of new files",
			isWrite: func(r *http.Request) bool {
				return r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/versions")
			},
			run: updateTemplateFiles,
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
	postsBuild := func(w writeCase) bool { return w.postsBuild }
	outcomes := []outcome{
		{"proxy 502", gatewayPage(http.StatusBadGateway), always},
		{"proxy 503", gatewayPage(http.StatusServiceUnavailable), always},
		{"connection closed without an answer", closedWithoutAnswer, always},
		{"body breaks off", truncatedAnswer, readsBody},
		{"200 with malformed JSON", jsonAnswer(http.StatusOK, `{"id":`), readsBody},
		{"201 with an empty object", jsonAnswer(http.StatusCreated, `{}`), checksAnswer},
		{"201 with null", jsonAnswer(http.StatusCreated, `null`), checksAnswer},
		{"answer without the ID", answerWithoutID, checksAnswer},
		{"proxy 307 without Location", gatewayPage(http.StatusTemporaryRedirect), always},
		{"proxy 400 page", gatewayPage(http.StatusBadRequest), always},
		{"proxy 500 page", gatewayPage(http.StatusInternalServerError), always},
		{"409 with an empty JSON object", jsonAnswer(http.StatusConflict, `{}`), always},
		{"500 with an empty body", jsonAnswer(http.StatusInternalServerError, ``), always},
		{"201 with an empty body", jsonAnswer(http.StatusCreated, ``), readsBody},
		{"201 with a JSON array", jsonAnswer(http.StatusCreated, `[]`), readsBody},
		{"201 with malformed JSON", jsonAnswer(http.StatusCreated, `{"id":`), readsBody},
		{"201 with only a build ID", jsonAnswer(http.StatusCreated, fmt.Sprintf(`{"id":%q}`, uuid.NewString())), postsBuild},
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

// TestTemplateFilesUpdateReReadAfterActivation checks the re-read of the template after Coder
// accepted the activation of new files. When it fails or shows another active version, the new
// files can be active, so the update returns the uncertain 504.
func TestTemplateFilesUpdateReReadAfterActivation(t *testing.T) {
	t.Parallel()

	otherActiveVersion := func(t *testing.T, w http.ResponseWriter, coderAnswer *httptest.ResponseRecorder) {
		var object map[string]any
		if err := json.Unmarshal(coderAnswer.Body.Bytes(), &object); err != nil {
			t.Fatalf("decode Coder answer: %v", err)
		}
		object["active_version_id"] = uuid.NewString()
		body, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		jsonAnswer(coderAnswer.Code, string(body))(t, w, coderAnswer)
	}
	coderMessage := uuid.NewString()
	for _, tc := range []struct {
		name   string
		answer brokenAnswer
		// refused sends the re-read to a closed port instead of answer.
		refused bool
		// logsStatus is the status that the coder-errors log must keep with the re-read error,
		// or -1 when the test does not check the log.
		logsStatus int
	}{
		{name: "proxy 502", answer: gatewayPage(http.StatusBadGateway), logsStatus: -1},
		{
			name: "Coder 503",
			answer: func(_ *testing.T, w http.ResponseWriter, _ *httptest.ResponseRecorder) {
				writeCoderError(w, http.StatusServiceUnavailable, coderMessage)
			},
			logsStatus: http.StatusServiceUnavailable,
		},
		{name: "connection closed without an answer", answer: closedWithoutAnswer, logsStatus: -1},
		{name: "another active version", answer: otherActiveVersion, logsStatus: -1},
		{name: "connection refused", refused: true, logsStatus: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			coderServer, _ := newMockCoderServer(t)
			t.Cleanup(coderServer.Close)

			var activated, reRead atomic.Bool
			isReRead := func(r *http.Request) bool {
				if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/versions") {
					activated.Store(true)
					return false
				}
				return activated.Load() && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/templates/") &&
					reRead.CompareAndSwap(false, true)
			}
			provider := newTestClientProvider(t, writeOutcomeServer(t, coderServer, isReRead, tc.answer).URL)
			// logMarker is a unique part of the re-read error.
			logMarker := coderMessage
			if tc.refused {
				logMarker = coderServer.URL + "/api/v2/templates/"
				client := newTestSDKClient(t, coderServer.URL)
				client.HTTPClient = &http.Client{Transport: refusingWrites{isWrite: isReRead, closedAddr: closedAddress(t)}}
				provider = &coder.StaticClientProvider{Client: client, Namespace: "control-plane"}
			}
			readLog := captureCoderErrorLog()

			err := updateTemplateFiles(t, provider)
			if !reRead.Load() {
				t.Fatal("the update did not re-read the template after the activation")
			}
			if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") {
				t.Fatalf("expected the uncertain 504, got %v", err)
			}
			if tc.logsStatus < 0 {
				return
			}
			wantStatus := fmt.Sprintf(`"status"=%d `, tc.logsStatus)
			for _, line := range readLog() {
				if strings.Contains(line, logMarker) && strings.Contains(line, wantStatus) {
					return
				}
			}
			t.Fatalf("the coder-errors log has no line with %q and %s", logMarker, wantStatus)
		})
	}
}

var (
	coderErrorLogOnce  sync.Once
	coderErrorLogMu    sync.Mutex
	coderErrorLogLines []string
)

// captureCoderErrorLog routes the controller-runtime logger to memory, once for the test binary,
// and returns a function that reads the coder-errors lines. Tests of this package share the log,
// so a test looks for a part of its own error.
func captureCoderErrorLog() func() []string {
	coderErrorLogOnce.Do(func() {
		ctrl.SetLogger(funcr.New(func(prefix, args string) {
			if prefix != "coder-errors" {
				return
			}
			coderErrorLogMu.Lock()
			defer coderErrorLogMu.Unlock()
			coderErrorLogLines = append(coderErrorLogLines, args)
		}, funcr.Options{Verbosity: 1}))
	})
	return func() []string {
		coderErrorLogMu.Lock()
		defer coderErrorLogMu.Unlock()
		return append([]string(nil), coderErrorLogLines...)
	}
}

// TestWorkspaceCreateStoppedStopBuildAnswer checks a create with spec.running=false whose stop
// build POST gets an error or an answer without the requested build, and whose re-read works. A
// re-read that shows the stop build confirms it. After a stop that can have reached Coder, a re-read
// that still shows the start build does not: Coder can still be queueing the stop, so the create
// returns the uncertain 504. After a stop that Coder refused, the start build is the result.
func TestWorkspaceCreateStoppedStopBuildAnswer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		answer        brokenAnswer
		forwarded     bool
		wantUncertain bool
	}{
		{"201 with an empty object", jsonAnswer(http.StatusCreated, `{}`), true, false},
		{"201 with null", jsonAnswer(http.StatusCreated, `null`), true, false},
		{"201 with only a build ID", jsonAnswer(http.StatusCreated, fmt.Sprintf(`{"id":%q}`, uuid.NewString())), true, false},
		{"proxy 502", gatewayPage(http.StatusBadGateway), true, false},
		{"201 with an empty object for a stop that Coder has not queued yet", jsonAnswer(http.StatusCreated, `{}`), false, true},
		{"proxy 502 for a stop that Coder has not queued yet", gatewayPage(http.StatusBadGateway), false, true},
		{"proxy 504 for a stop that Coder has not queued yet", gatewayPage(http.StatusGatewayTimeout), false, true},
		{"Coder 400 for the stop", jsonAnswer(http.StatusBadRequest, `{"message":"bad stop"}`), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			coderServer, _ := newMockCoderServer(t)
			t.Cleanup(coderServer.Close)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/builds") {
					coderServer.Config.Handler.ServeHTTP(w, r)
					return
				}
				if tc.forwarded {
					recorder := httptest.NewRecorder()
					coderServer.Config.Handler.ServeHTTP(recorder, r)
					if recorder.Code != http.StatusCreated {
						t.Errorf("mock Coder answered the stop with %d: %s", recorder.Code, recorder.Body.String())
					}
				}
				tc.answer(t, w, nil)
			}))
			t.Cleanup(proxy.Close)

			ctx := namespacedContext("control-plane")
			createdObj, err := NewWorkspaceStorage(newTestClientProvider(t, proxy.URL)).Create(ctx, &aggregationv1alpha1.CoderWorkspace{
				ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.stopped"},
				Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Organization: "acme", TemplateName: "starter-template", Running: false},
			}, rest.ValidateAllObjectFunc, nil)
			coderObj, getErr := NewWorkspaceStorage(newTestClientProvider(t, coderServer.URL)).Get(ctx, "acme.alice.stopped", nil)
			if getErr != nil {
				t.Fatalf("get from Coder: %v", getErr)
			}
			inCoder := coderObj.(*aggregationv1alpha1.CoderWorkspace)
			if inCoder.Spec.Running == tc.forwarded {
				t.Fatalf("Coder has running=%t, want %t", inCoder.Spec.Running, !tc.forwarded)
			}
			if tc.wantUncertain {
				if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") || !strings.Contains(err.Error(), "stop") || createdObj != nil {
					t.Fatalf("expected the uncertain 504 about the stop and no object, got %v (object returned: %t)", err, createdObj != nil)
				}
				return
			}
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			created := createdObj.(*aggregationv1alpha1.CoderWorkspace)
			if created.Status.LatestBuildID != inCoder.Status.LatestBuildID || created.Spec.Running != inCoder.Spec.Running {
				t.Fatalf("created object has build %s running=%t, Coder has build %s running=%t",
					created.Status.LatestBuildID, created.Spec.Running, inCoder.Status.LatestBuildID, inCoder.Spec.Running)
			}
		})
	}
}

// latestBuildOnlyID sends Coder's workspace with a latest build that has only its ID.
func latestBuildOnlyID(t *testing.T, w http.ResponseWriter, coderAnswer *httptest.ResponseRecorder) {
	var object map[string]any
	if err := json.Unmarshal(coderAnswer.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode Coder answer: %v", err)
	}
	object["latest_build"] = map[string]any{"id": uuid.NewString()}
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	jsonAnswer(coderAnswer.Code, string(body))(t, w, coderAnswer)
}

// TestWorkspaceCreateStoppedFailedReRead checks a create with spec.running=false whose stop build
// is not confirmed and whose re-read of the workspace fails or has no usable latest build. A stop
// that can have reached Coder returns the uncertain 504 and no object, never the start build as a
// success. A stop that Coder refused leaves the start build, so the create succeeds with it.
func TestWorkspaceCreateStoppedFailedReRead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		stopAnswer    brokenAnswer
		forwarded     bool
		reRead        brokenAnswer
		wantUncertain bool
	}{
		{"201 with an empty object, re-read 502", jsonAnswer(http.StatusCreated, `{}`), true, gatewayPage(http.StatusBadGateway), true},
		{"201 with an empty object, re-read 404", jsonAnswer(http.StatusCreated, `{}`), true, jsonAnswer(http.StatusNotFound, `{"message":"not found"}`), true},
		{"201 with an empty object, re-read with only a build ID", jsonAnswer(http.StatusCreated, `{}`), true, latestBuildOnlyID, true},
		{"proxy 502, re-read 502", gatewayPage(http.StatusBadGateway), true, gatewayPage(http.StatusBadGateway), true},
		{"proxy 502, re-read 404", gatewayPage(http.StatusBadGateway), true, jsonAnswer(http.StatusNotFound, `{"message":"not found"}`), true},
		{"proxy 400 page, re-read 502", gatewayPage(http.StatusBadRequest), true, gatewayPage(http.StatusBadGateway), true},
		{"proxy 400 page, re-read 404", gatewayPage(http.StatusBadRequest), true, jsonAnswer(http.StatusNotFound, `{"message":"not found"}`), true},
		{"proxy 500 page, re-read 502", gatewayPage(http.StatusInternalServerError), true, gatewayPage(http.StatusBadGateway), true},
		{"proxy 500 page, re-read 404", gatewayPage(http.StatusInternalServerError), true, jsonAnswer(http.StatusNotFound, `{"message":"not found"}`), true},
		{"Coder 400 without the stop, re-read 502", jsonAnswer(http.StatusBadRequest, `{"message":"bad stop"}`), false, gatewayPage(http.StatusBadGateway), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			coderServer, _ := newMockCoderServer(t)
			t.Cleanup(coderServer.Close)
			var stops, reReads atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/builds") {
					stops.Add(1)
					recorder := httptest.NewRecorder()
					if tc.forwarded {
						coderServer.Config.Handler.ServeHTTP(recorder, r)
						if recorder.Code != http.StatusCreated {
							t.Errorf("mock Coder answered the stop with %d: %s", recorder.Code, recorder.Body.String())
						}
					}
					tc.stopAnswer(t, w, recorder)
					return
				}
				if stops.Load() > 0 && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/workspaces/") {
					reReads.Add(1)
					recorder := httptest.NewRecorder()
					coderServer.Config.Handler.ServeHTTP(recorder, r)
					tc.reRead(t, w, recorder)
					return
				}
				coderServer.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)

			ctx := namespacedContext("control-plane")
			createdObj, err := NewWorkspaceStorage(newTestClientProvider(t, proxy.URL)).Create(ctx, &aggregationv1alpha1.CoderWorkspace{
				ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.stopped"},
				Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Organization: "acme", TemplateName: "starter-template", Running: false},
			}, rest.ValidateAllObjectFunc, nil)
			if stops.Load() != 1 || reReads.Load() != 1 {
				t.Fatalf("stop POSTs=%d re-reads=%d, want 1 and 1", stops.Load(), reReads.Load())
			}
			coderObj, getErr := NewWorkspaceStorage(newTestClientProvider(t, coderServer.URL)).Get(ctx, "acme.alice.stopped", nil)
			if getErr != nil {
				t.Fatalf("get from Coder: %v", getErr)
			}
			inCoder := coderObj.(*aggregationv1alpha1.CoderWorkspace)
			if inCoder.Spec.Running == tc.forwarded {
				t.Fatalf("Coder has running=%t, want %t", inCoder.Spec.Running, !tc.forwarded)
			}

			if tc.wantUncertain {
				if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") || !strings.Contains(err.Error(), "stop") || createdObj != nil {
					t.Fatalf("expected the uncertain 504 about the stop and no object, got %v (object returned: %t)", err, createdObj != nil)
				}
				return
			}
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if created := createdObj.(*aggregationv1alpha1.CoderWorkspace); created.Status.LatestBuildID != inCoder.Status.LatestBuildID || !created.Spec.Running {
				t.Fatalf("created object has build %s running=%t, Coder has the start build %s", created.Status.LatestBuildID, created.Spec.Running, inCoder.Status.LatestBuildID)
			}
		})
	}
}

// TestWorkspaceCreateStoppedStopNotSent: the stop build POST of a create with spec.running=false
// is refused before it is sent, and so is the re-read. The stop did not reach Coder, so the create
// succeeds with the start build.
func TestWorkspaceCreateStoppedStopNotSent(t *testing.T) {
	t.Parallel()

	coderServer, _ := newMockCoderServer(t)
	t.Cleanup(coderServer.Close)
	var stopTried atomic.Bool
	refused := func(r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/builds") {
			stopTried.Store(true)
			return true
		}
		return stopTried.Load() && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/workspaces/")
	}
	client := newTestSDKClient(t, coderServer.URL)
	client.HTTPClient = &http.Client{Transport: refusingWrites{isWrite: refused, closedAddr: closedAddress(t)}}

	ctx := namespacedContext("control-plane")
	createdObj, err := NewWorkspaceStorage(&coder.StaticClientProvider{Client: client, Namespace: "control-plane"}).Create(ctx, &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.stopped"},
		Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Organization: "acme", TemplateName: "starter-template", Running: false},
	}, rest.ValidateAllObjectFunc, nil)
	if err != nil || !stopTried.Load() {
		t.Fatalf("create: err=%v, stop tried=%t; want success after a stop attempt", err, stopTried.Load())
	}
	coderObj, err := NewWorkspaceStorage(newTestClientProvider(t, coderServer.URL)).Get(ctx, "acme.alice.stopped", nil)
	if err != nil {
		t.Fatalf("get from Coder: %v", err)
	}
	created, inCoder := createdObj.(*aggregationv1alpha1.CoderWorkspace), coderObj.(*aggregationv1alpha1.CoderWorkspace)
	if !inCoder.Spec.Running || !created.Spec.Running || created.Status.LatestBuildID != inCoder.Status.LatestBuildID {
		t.Fatalf("created object has build %s running=%t, Coder has the start build %s running=%t",
			created.Status.LatestBuildID, created.Spec.Running, inCoder.Status.LatestBuildID, inCoder.Spec.Running)
	}
}

// TestTemplateUpdateReadAfterChange checks the read of the template that ends an update. When it
// fails after Coder applied the change, the update returns the uncertain 504.
func TestTemplateUpdateReadAfterChange(t *testing.T) {
	t.Parallel()

	ran := 0
	for _, wc := range writeCases() {
		if wc.name != "template update" && wc.name != "template activation of new files" {
			continue
		}
		ran++
		t.Run(wc.name, func(t *testing.T) {
			t.Parallel()
			coderServer, _ := newMockCoderServer(t)
			t.Cleanup(coderServer.Close)

			var changed, finalRead atomic.Bool
			isFinalRead := func(r *http.Request) bool {
				if wc.isWrite(r) {
					changed.Store(true)
					return false
				}
				return changed.Load() && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/organizations/") &&
					finalRead.CompareAndSwap(false, true)
			}
			provider := newTestClientProvider(t, writeOutcomeServer(t, coderServer, isFinalRead, gatewayPage(http.StatusBadGateway)).URL)

			err := wc.run(t, provider)
			if !finalRead.Load() {
				t.Fatal("the update did not read the template after the change")
			}
			if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") {
				t.Fatalf("expected the uncertain 504, got %v", err)
			}
		})
	}
	if ran != 2 {
		t.Fatalf("expected 2 template update cases, found %d", ran)
	}
}
