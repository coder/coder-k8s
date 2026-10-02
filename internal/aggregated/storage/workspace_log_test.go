package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/warning"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

const (
	logTestNamespace = "log-ns"
	logTestName      = "acme.alice.dev"
)

// logFakeCoder serves one workspace (acme.alice.dev) and its latest build log. Lookups resolve
// names case-insensitively, as Coder does.
type logFakeCoder struct {
	server      *httptest.Server
	buildID     uuid.UUID
	lookups     atomic.Int32
	logFetches  atomic.Int32
	logsHandler atomic.Pointer[http.HandlerFunc]
	// lookupGate, when set, blocks workspace lookups until the request ends.
	lookupGate atomic.Pointer[chan struct{}]
}

func newLogFakeCoder(t *testing.T) *logFakeCoder {
	t.Helper()
	f := &logFakeCoder{buildID: uuid.New()}
	orgID, otherOrgID := uuid.New(), uuid.New()
	workspace := codersdk.Workspace{
		ID: uuid.New(), Name: "dev", OwnerName: "alice", OrganizationName: "acme", OrganizationID: orgID,
		LatestBuild: codersdk.WorkspaceBuild{ID: f.buildID},
	}
	f.setLogs(nil)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case len(parts) == 6 && parts[2] == "users" && parts[4] == "workspace":
			f.lookups.Add(1)
			if gate := f.lookupGate.Load(); gate != nil {
				select {
				case <-*gate:
				case <-r.Context().Done():
					return
				}
			}
			if !strings.EqualFold(parts[3], "alice") || !strings.EqualFold(parts[5], "dev") {
				writeLogFakeError(w, http.StatusNotFound)
				return
			}
			writeLogFakeJSON(w, workspace)
		case len(parts) == 4 && parts[2] == "organizations":
			switch {
			case strings.EqualFold(parts[3], "acme"):
				writeLogFakeJSON(w, codersdk.Organization{MinimalOrganization: codersdk.MinimalOrganization{ID: orgID, Name: "acme"}})
			case parts[3] == "other":
				writeLogFakeJSON(w, codersdk.Organization{MinimalOrganization: codersdk.MinimalOrganization{ID: otherOrgID, Name: "other"}})
			default:
				writeLogFakeError(w, http.StatusNotFound)
			}
		case len(parts) == 5 && parts[2] == "workspacebuilds" && parts[4] == "logs" && parts[3] == f.buildID.String():
			f.logFetches.Add(1)
			(*f.logsHandler.Load())(w, r)
		default:
			writeLogFakeError(w, http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *logFakeCoder) setLogs(entries []codersdk.ProvisionerJobLog) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeLogFakeJSON(w, entries) })
	f.logsHandler.Store(&h)
}

func (f *logFakeCoder) setLogsHandler(h http.HandlerFunc) { f.logsHandler.Store(&h) }

func writeLogFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeLogFakeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(codersdk.Response{Message: http.StatusText(status)})
}

func testLogEntries(n int) []codersdk.ProvisionerJobLog {
	entries := make([]codersdk.ProvisionerJobLog, n)
	for i := range entries {
		entries[i] = codersdk.ProvisionerJobLog{
			ID: int64(i + 1), CreatedAt: time.Date(2026, 10, 2, 10, 0, i, 0, time.UTC),
			Level: codersdk.LogLevelInfo, Stage: "Planning infrastructure", Output: fmt.Sprintf("line %d", i+1),
		}
	}
	return entries
}

func renderedLog(entries []codersdk.ProvisionerJobLog) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Text() + "\n")
	}
	return b.String()
}

func newTestLogStorage(t *testing.T, f *logFakeCoder, tune func(*workspaceLogLimits)) *WorkspaceLogStorage {
	t.Helper()
	serverURL, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	workspaces := NewWorkspaceStorage(&coder.StaticClientProvider{Client: codersdk.New(serverURL), Namespace: logTestNamespace})
	t.Cleanup(workspaces.Destroy)
	limits := defaultWorkspaceLogLimits()
	if tune != nil {
		tune(&limits)
	}
	return newWorkspaceLogStorage(workspaces, limits)
}

type recordedWarnings struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recordedWarnings) AddWarning(_, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, text)
}

func logRequestContext(parent context.Context, userName string, warnings *recordedWarnings) context.Context {
	ctx := request.WithNamespace(request.WithUser(parent, &user.DefaultInfo{Name: userName}), logTestNamespace)
	return warning.WithWarningRecorder(ctx, warnings)
}

// openLog runs Get and InputStream as the generic GET handler does, without reading the body.
func openLog(ctx context.Context, s *WorkspaceLogStorage, name string, opts *aggregationv1alpha1.CoderWorkspaceLogOptions) (io.ReadCloser, string, error) {
	if opts == nil {
		opts = &aggregationv1alpha1.CoderWorkspaceLogOptions{}
	}
	obj, err := s.Get(ctx, name, opts)
	if err != nil {
		return nil, "", err
	}
	body, _, contentType, err := obj.(rest.ResourceStreamer).InputStream(ctx, "v1alpha1", "*/*")
	return body, contentType, err
}

func readLog(t *testing.T, s *WorkspaceLogStorage, name string, opts *aggregationv1alpha1.CoderWorkspaceLogOptions) (string, []string, error) {
	t.Helper()
	warnings := &recordedWarnings{}
	body, contentType, err := openLog(logRequestContext(t.Context(), "alice", warnings), s, name, opts)
	if err != nil {
		return "", warnings.msgs, err
	}
	defer func() { _ = body.Close() }()
	if contentType != logContentType {
		t.Fatalf("content type %q, want %q", contentType, logContentType)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), warnings.msgs, nil
}

func int64Ptr(v int64) *int64 { return &v }

func TestWorkspaceLogSnapshot(t *testing.T) {
	f := newLogFakeCoder(t)
	s := newTestLogStorage(t, f, nil)
	entries := testLogEntries(3)

	f.setLogs(entries)
	got, warnings, err := readLog(t, s, logTestName, nil)
	if err != nil || got != renderedLog(entries) || len(warnings) != 0 {
		t.Fatalf("snapshot: err=%v warnings=%v got=%q", err, warnings, got)
	}
	if !strings.HasPrefix(got, "2026-10-02T10:00:00Z [info] [provisioner|Planning infrastructure] line 1\n") {
		t.Fatalf("unexpected line format: %q", got)
	}

	for _, empty := range []string{"[]", "null"} {
		f.setLogsHandler(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, empty) })
		if got, _, err := readLog(t, s, logTestName, nil); err != nil || got != "" {
			t.Fatalf("empty log %s: err=%v got=%q", empty, err, got)
		}
	}

	f.setLogs(entries)
	got, warnings, err = readLog(t, s, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{LimitBytes: int64Ptr(30)})
	if err != nil || got != renderedLog(entries)[:30] || len(warnings) != 0 {
		t.Fatalf("limitBytes=30 must cut mid-line without a warning: err=%v warnings=%v got=%q", err, warnings, got)
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("slots in use after closed streams: %d", s.slots.inUse())
	}

	// A successful request keeps its slot until the response body is closed.
	body, _, err := openLog(logRequestContext(t.Context(), "alice", &recordedWarnings{}), s, logTestName, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.slots.inUse() != 1 {
		t.Fatalf("slots in use while the body is open: %d, want 1", s.slots.inUse())
	}
	_ = body.Close()
	if s.slots.inUse() != 0 {
		t.Fatalf("slots in use after Close: %d", s.slots.inUse())
	}
}

func TestWorkspaceLogServerCapsWarn(t *testing.T) {
	f := newLogFakeCoder(t)
	entries := testLogEntries(50)
	f.setLogs(entries)
	rendered := renderedLog(entries)

	byteCapped := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.maxBytes = 100 })
	got, warnings, err := readLog(t, byteCapped, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{LimitBytes: int64Ptr(1000)})
	if err != nil || got != rendered[:100] || len(warnings) != 1 || !strings.Contains(warnings[0], "server limit of 100 bytes") {
		t.Fatalf("byte cap: err=%v warnings=%v len=%d", err, warnings, len(got))
	}

	exactFit := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.maxBytes = int64(len(rendered)) })
	if got, warnings, err := readLog(t, exactFit, logTestName, nil); err != nil || got != rendered || len(warnings) != 0 {
		t.Fatalf("a log that exactly fits the cap: err=%v warnings=%v len=%d", err, warnings, len(got))
	}

	scanCapped := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.maxScanBytes = 1000 })
	got, warnings, err = readLog(t, scanCapped, logTestName, nil)
	if err != nil || got == "" || !strings.HasPrefix(rendered, got) || len(got) >= len(rendered) {
		t.Fatalf("scan cap must return a complete-entry prefix: err=%v len=%d", err, len(got))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "read limit of 1000 bytes") {
		t.Fatalf("scan cap warnings: %v", warnings)
	}
	for _, w := range warnings {
		if strings.Contains(w, "line ") {
			t.Fatalf("warning leaks log content: %q", w)
		}
	}
}

func TestWorkspaceLogOptions(t *testing.T) {
	f := newLogFakeCoder(t)
	s := newTestLogStorage(t, f, nil)
	ctx := logRequestContext(t.Context(), "alice", &recordedWarnings{})

	_, err := s.Get(ctx, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{LimitBytes: int64Ptr(0)})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("limitBytes=0: err=%v, want Invalid", err)
	}

	// The per-user slot key is the request user after impersonation. A request without one is a
	// bug in the handler chain, not an anonymous bucket.
	noUser := warning.WithWarningRecorder(request.WithNamespace(t.Context(), logTestNamespace), &recordedWarnings{})
	if _, err := s.Get(noUser, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{}); err == nil || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("request without a user: err=%v, want assertion failure", err)
	}
	noName := request.WithUser(noUser, &user.DefaultInfo{})
	if _, err := s.Get(noName, logTestName, &aggregationv1alpha1.CoderWorkspaceLogOptions{}); err == nil || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("request with an empty user name: err=%v, want assertion failure", err)
	}

	if f.lookups.Load() != 0 {
		t.Fatalf("option failures made %d Coder calls", f.lookups.Load())
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("option failures took slots: %d", s.slots.inUse())
	}
}

func TestWorkspaceLogIdentityMakesNoLogFetch(t *testing.T) {
	f := newLogFakeCoder(t)
	f.setLogs(testLogEntries(2))
	s := newTestLogStorage(t, f, nil)
	for name, check := range map[string]func(error) bool{
		"Acme.alice.dev":   apierrors.IsBadRequest, // organization alias
		"acme.alice.Dev":   apierrors.IsBadRequest, // case variant of the leaf
		"other.alice.dev":  apierrors.IsNotFound,   // another organization, opaque
		"not-a-valid-name": apierrors.IsBadRequest,
	} {
		if _, _, err := readLog(t, s, name, nil); !check(err) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if f.logFetches.Load() != 0 {
		t.Fatalf("identity failures fetched logs %d times", f.logFetches.Load())
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("slots in use after identity failures: %d", s.slots.inUse())
	}
}

func TestWorkspaceLogMapsCoderErrors(t *testing.T) {
	f := newLogFakeCoder(t)
	s := newTestLogStorage(t, f, nil)
	for status, check := range map[int]func(error) bool{
		http.StatusNotFound:            apierrors.IsNotFound,
		http.StatusInternalServerError: apierrors.IsInternalError,
	} {
		f.setLogsHandler(func(w http.ResponseWriter, _ *http.Request) { writeLogFakeError(w, status) })
		if _, _, err := readLog(t, s, logTestName, nil); !check(err) {
			t.Errorf("Coder %d: err=%v", status, err)
		}
	}
	f.setLogsHandler(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"secret":"x"}`) })
	if _, _, err := readLog(t, s, logTestName, nil); !apierrors.IsInternalError(err) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("malformed log: err=%v", err)
	}
	// Trailing data after a complete JSON value is malformed, not silently dropped.
	for _, body := range []string{`null[{"id":1}]`, `[]x`, `[] []`} {
		f.setLogsHandler(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		if _, _, err := readLog(t, s, logTestName, nil); !apierrors.IsInternalError(err) {
			t.Errorf("trailing data %q: err=%v, want 500", body, err)
		}
	}
	// A huge error body is not read to the end.
	const hugeErrorBody = 64 << 20
	var written atomic.Int64
	f.setLogsHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		chunk := []byte(strings.Repeat("x", 32<<10))
		for written.Load() < hugeErrorBody {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	if _, _, err := readLog(t, s, logTestName, nil); !apierrors.IsInternalError(err) {
		t.Fatalf("huge error body: err=%v, want 500", err)
	}
	if got := written.Load(); got >= hugeErrorBody/2 {
		t.Fatalf("huge error body was read to %d bytes", got)
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("slots in use after Coder errors: %d", s.slots.inUse())
	}
}

// TestWorkspaceLogReleasesSlotOnStall: a Coder call that never answers ends at the log deadline,
// and a client that disconnects before the first byte frees its slot.
func TestWorkspaceLogReleasesSlotOnStall(t *testing.T) {
	f := newLogFakeCoder(t)
	s := newTestLogStorage(t, f, func(l *workspaceLogLimits) { l.duration = 300 * time.Millisecond })
	f.setLogsHandler(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	start := time.Now()
	if _, _, err := readLog(t, s, logTestName, nil); !apierrors.IsTimeout(err) {
		t.Fatalf("Coder stalled before response headers: err=%v, want 504 timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stalled snapshot ended after %s", elapsed)
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("slot held after a stalled snapshot: %d", s.slots.inUse())
	}
	// Any failure after the log deadline is a 504: a stalled error body, or a stalled lookup.
	f.setLogsHandler(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	if _, _, err := readLog(t, s, logTestName, nil); !apierrors.IsTimeout(err) {
		t.Fatalf("Coder stalled in an error body: err=%v, want 504 timeout", err)
	}
	stalledLookup := make(chan struct{})
	f.lookupGate.Store(&stalledLookup)
	if _, _, err := readLog(t, s, logTestName, nil); !apierrors.IsTimeout(err) {
		t.Fatalf("Coder stalled in the workspace lookup: err=%v, want 504 timeout", err)
	}
	close(stalledLookup)
	if s.slots.inUse() != 0 {
		t.Fatalf("slot held after stalled requests: %d", s.slots.inUse())
	}

	gate := make(chan struct{})
	defer close(gate)
	f.lookupGate.Store(&gate)
	ctx, cancel := context.WithCancel(logRequestContext(t.Context(), "alice", &recordedWarnings{}))
	done := make(chan error, 1)
	lookups := f.lookups.Load()
	go func() { _, _, err := openLog(ctx, s, logTestName, nil); done <- err }()
	for f.lookups.Load() == lookups {
		time.Sleep(10 * time.Millisecond)
	}
	if s.slots.inUse() != 1 {
		t.Fatalf("slots in use during the lookup: %d", s.slots.inUse())
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request did not return")
	}
	if s.slots.inUse() != 0 {
		t.Fatalf("slot held after client disconnect: %d", s.slots.inUse())
	}
}
