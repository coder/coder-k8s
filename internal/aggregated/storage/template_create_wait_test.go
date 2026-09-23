package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
)

// newTemplateCreateWaitHarness serves the mock Coder API through hook, which may answer a request itself by
// returning true. It returns template storage with a template watch opened before any mutation.
func newTemplateCreateWaitHarness(
	t *testing.T,
	hook func(w http.ResponseWriter, r *http.Request) bool,
) (*TemplateStorage, *mockCoderServerState, watch.Interface) {
	t.Helper()

	mock, state := newMockCoderServer(t)
	t.Cleanup(mock.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil && hook(w, r) {
			return
		}
		state.handleRequest(t, w, r)
	}))
	t.Cleanup(server.Close)

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	t.Cleanup(templateStorage.Destroy)
	watcher, err := templateStorage.Watch(namespacedContext("control-plane"), nil)
	if err != nil {
		t.Fatalf("start template watch: %v", err)
	}
	t.Cleanup(watcher.Stop)

	return templateStorage, state, watcher
}

func filesTemplateForCreate(leafName string) *aggregationv1alpha1.CoderTemplate {
	return &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "acme." + leafName},
		Spec: aggregationv1alpha1.CoderTemplateSpec{
			Organization: "acme",
			Files:        map[string]string{"main.tf": `resource "null_resource" "import_wait" {}`},
		},
	}
}

// createTemplateCalls counts CreateTemplate requests (POST /api/v2/organizations/{org}/templates).
func createTemplateCalls(state *mockCoderServerState) int {
	calls := 0
	for _, mutation := range state.mutations() {
		if strings.HasPrefix(mutation, "POST /api/v2/organizations/") && strings.HasSuffix(mutation, "/templates") {
			calls++
		}
	}
	return calls
}

func isTemplateVersionPoll(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/templateversions/")
}

func setFastTemplateVersionBuildPolling(t *testing.T, waitTimeout string) {
	t.Helper()
	t.Setenv(templateVersionBuildWaitTimeoutEnv, waitTimeout)
	t.Setenv(templateVersionBuildBackoffAfterEnv, "0s")
	t.Setenv(templateVersionBuildInitialPollIntervalEnv, "10ms")
	t.Setenv(templateVersionBuildMaxPollIntervalEnv, "20ms")
}

func expectNoWatchEvent(t *testing.T, watcher watch.Interface, window time.Duration) {
	t.Helper()
	select {
	case event := <-watcher.ResultChan():
		t.Fatalf("expected no watch event, got %s", event.Type)
	case <-time.After(window):
	}
}

func TestTemplateStorageCreateWithFilesWaitsForImportBeforeCreatingTemplate(t *testing.T) {
	setFastTemplateVersionBuildPolling(t, "5s")

	var polls atomic.Int32
	secondPoll := make(chan struct{})
	release := make(chan struct{})
	templateStorage, state, watcher := newTemplateCreateWaitHarness(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if isTemplateVersionPoll(r) && polls.Add(1) == 2 {
			close(secondPoll)
			<-release // hold the import in progress until the test has checked for early side effects
		}
		return false
	})
	releaseImport := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseImport)
	state.setNextCreatedTemplateVersionPendingForPolls(3)

	type createResult struct {
		obj runtime.Object
		err error
	}
	done := make(chan createResult, 1)
	go func() {
		obj, err := templateStorage.Create(namespacedContext("control-plane"), filesTemplateForCreate("import-wait"), rest.ValidateAllObjectFunc, nil)
		done <- createResult{obj: obj, err: err}
	}()

	select {
	case <-secondPoll:
	case result := <-done:
		t.Fatalf("expected Create to wait for the template version import; it returned (err=%v) after %d CreateTemplate call(s) and %d version poll(s)",
			result.err, createTemplateCalls(state), polls.Load())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the second template version poll")
	}
	if calls := createTemplateCalls(state); calls != 0 {
		t.Fatalf("expected no CreateTemplate call while the import is in progress, got %d", calls)
	}
	expectNoWatchEvent(t, watcher, 50*time.Millisecond)

	releaseImport()
	var result createResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Create after the import succeeded")
	}
	if result.err != nil {
		t.Fatalf("expected Create to succeed after the import succeeded: %v", result.err)
	}
	if calls := createTemplateCalls(state); calls != 1 {
		t.Fatalf("expected exactly one CreateTemplate call, got %d", calls)
	}
	added := receiveWatchEvent(t, watcher, watchEventTimeout)
	if added.Type != watch.Added || templateFromWatchEvent(t, added).Name != "acme.import-wait" {
		t.Fatalf("expected one Added event for acme.import-wait, got %s", added.Type)
	}
	expectNoWatchEvent(t, watcher, 100*time.Millisecond)
}

func TestTemplateStorageCreateWithFilesImportFailureCreatesNoTemplate(t *testing.T) {
	pending := func(state *mockCoderServerState) {
		state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobPending)
	}
	tests := []struct {
		name        string
		waitTimeout string
		setup       func(state *mockCoderServerState)
		failPolls   bool
		requestCtx  func(t *testing.T) context.Context
		wantErr     func(error) bool
		wantMessage string
	}{
		{
			name: "failed import",
			setup: func(state *mockCoderServerState) {
				state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobFailed)
			},
			wantErr:     apierrors.IsBadRequest,
			wantMessage: "build ended with status",
		},
		{
			name: "canceled import",
			setup: func(state *mockCoderServerState) {
				state.setNextCreatedTemplateVersionStatus(codersdk.ProvisionerJobCanceled)
			},
			wantErr:     apierrors.IsBadRequest,
			wantMessage: "build ended with status",
		},
		{name: "configured wait timeout", waitTimeout: "120ms", setup: pending, wantErr: apierrors.IsBadRequest, wantMessage: "did not succeed within"},
		{
			name:  "request deadline",
			setup: pending,
			requestCtx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(namespacedContext("control-plane"), 300*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			wantErr:     apierrors.IsTimeout,
			wantMessage: "did not succeed within",
		},
		{
			name:  "request cancellation",
			setup: pending,
			requestCtx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(namespacedContext("control-plane"))
				time.AfterFunc(300*time.Millisecond, cancel)
				t.Cleanup(cancel)
				return ctx
			},
			wantErr:     apierrors.IsTimeout,
			wantMessage: "did not succeed within",
		},
		{name: "polling error", failPolls: true, wantErr: apierrors.IsInternalError, wantMessage: "fetch template version"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			waitTimeout := tc.waitTimeout
			if waitTimeout == "" {
				waitTimeout = "5s"
			}
			setFastTemplateVersionBuildPolling(t, waitTimeout)

			templateStorage, state, watcher := newTemplateCreateWaitHarness(t, func(w http.ResponseWriter, r *http.Request) bool {
				if tc.failPolls && isTemplateVersionPoll(r) {
					writeCoderError(w, http.StatusInternalServerError, "template version lookup failed")
					return true
				}
				return false
			})
			if tc.setup != nil {
				tc.setup(state)
			}
			ctx := namespacedContext("control-plane")
			if tc.requestCtx != nil {
				ctx = tc.requestCtx(t)
			}
			versionsBefore := state.templateVersionCount()

			obj, err := templateStorage.Create(ctx, filesTemplateForCreate("import-fails"), rest.ValidateAllObjectFunc, nil)
			if err == nil {
				t.Fatalf("expected Create to fail; it returned %T after %d CreateTemplate call(s)", obj, createTemplateCalls(state))
			}
			if obj != nil {
				t.Fatalf("expected nil object on failure, got %T", obj)
			}
			if !tc.wantErr(err) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("expected mapped wait error containing %q, got %T: %v", tc.wantMessage, err, err)
			}
			assertTopLevelStatusError(t, err)

			// No template may be created, not even late after a timeout or cancellation.
			time.Sleep(200 * time.Millisecond)
			if calls := createTemplateCalls(state); calls != 0 {
				t.Fatalf("expected no CreateTemplate call after a failed import, got %d", calls)
			}
			if state.hasTemplate("acme", "import-fails") {
				t.Fatal("expected no template after a failed import")
			}
			if versions := state.templateVersionCount(); versions != versionsBefore+1 {
				t.Fatalf("expected the uploaded template version to remain (no cleanup), before=%d after=%d", versionsBefore, versions)
			}
			expectNoWatchEvent(t, watcher, 50*time.Millisecond)
		})
	}
}
