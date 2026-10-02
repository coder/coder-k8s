package storage

import (
	"context"
	"encoding/json"
	"errors"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// transitionFakeCoder serves one workspace (acme.alice.dev), its re-read by ID and build POSTs.
// Lookups resolve names case-insensitively, as Coder does. A POST answers 201 with a new pending
// build and leaves the workspace unchanged, unless onPost is set.
type transitionFakeCoder struct {
	server   *httptest.Server
	activeID uuid.UUID

	mu         sync.Mutex
	ws         codersdk.Workspace
	posts      []codersdk.CreateWorkspaceBuildRequest
	posted     []codersdk.WorkspaceBuild
	onPost     func(w http.ResponseWriter, r *http.Request)
	onReread   func(w http.ResponseWriter, r *http.Request)
	lookupGate chan struct{}

	lookups atomic.Int32
	rereads atomic.Int32
}

func newTransitionFakeCoder(t *testing.T, latest codersdk.WorkspaceBuild) *transitionFakeCoder {
	t.Helper()
	orgID, otherOrgID := uuid.New(), uuid.New()
	f := &transitionFakeCoder{activeID: uuid.New()}
	if latest.ID == uuid.Nil {
		latest.ID = uuid.New()
	}
	if latest.BuildNumber == 0 {
		latest.BuildNumber = 7
	}
	f.ws = codersdk.Workspace{
		ID: uuid.New(), Name: "dev", OwnerName: "alice", OrganizationName: "acme", OrganizationID: orgID,
		TemplateActiveVersionID: f.activeID, AutomaticUpdates: codersdk.AutomaticUpdatesNever, LatestBuild: latest,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		f.mu.Lock()
		ws, gate, onPost, onReread := f.ws, f.lookupGate, f.onPost, f.onReread
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && len(parts) == 6 && parts[2] == "users" && parts[4] == "workspace":
			f.lookups.Add(1)
			if gate != nil {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			if !strings.EqualFold(parts[3], "alice") || !strings.EqualFold(parts[5], "dev") {
				writeLogFakeError(w, http.StatusNotFound)
				return
			}
			writeLogFakeJSON(w, ws)
		case r.Method == http.MethodGet && len(parts) == 4 && parts[2] == "organizations":
			switch {
			case strings.EqualFold(parts[3], "acme"):
				writeLogFakeJSON(w, codersdk.Organization{MinimalOrganization: codersdk.MinimalOrganization{ID: orgID, Name: "acme"}})
			case parts[3] == "other":
				writeLogFakeJSON(w, codersdk.Organization{MinimalOrganization: codersdk.MinimalOrganization{ID: otherOrgID, Name: "other"}})
			default:
				writeLogFakeError(w, http.StatusNotFound)
			}
		case r.Method == http.MethodGet && len(parts) == 4 && parts[2] == "workspaces" && parts[3] == ws.ID.String():
			f.rereads.Add(1)
			if onReread != nil {
				onReread(w, r)
				return
			}
			writeLogFakeJSON(w, ws)
		case r.Method == http.MethodPost && len(parts) == 5 && parts[2] == "workspaces" && parts[3] == ws.ID.String() && parts[4] == "builds":
			var req codersdk.CreateWorkspaceBuildRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeLogFakeError(w, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.posts = append(f.posts, req)
			f.mu.Unlock()
			if onPost != nil {
				onPost(w, r)
				return
			}
			build := codersdk.WorkspaceBuild{
				ID: uuid.New(), BuildNumber: ws.LatestBuild.BuildNumber + 1, Transition: req.Transition,
				Job: codersdk.ProvisionerJob{Status: codersdk.ProvisionerJobPending},
			}
			f.mu.Lock()
			f.posted = append(f.posted, build)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(build)
		default:
			writeLogFakeError(w, http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *transitionFakeCoder) update(change func(*codersdk.Workspace)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.ws)
}

func (f *transitionFakeCoder) set(change func(*transitionFakeCoder)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *transitionFakeCoder) recordedPosts() ([]codersdk.CreateWorkspaceBuildRequest, []codersdk.WorkspaceBuild) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codersdk.CreateWorkspaceBuildRequest(nil), f.posts...), append([]codersdk.WorkspaceBuild(nil), f.posted...)
}

func newTestWorkspaces(t *testing.T, f *transitionFakeCoder, clientTimeout time.Duration) *WorkspaceStorage {
	t.Helper()
	serverURL, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := codersdk.New(serverURL)
	client.HTTPClient.Timeout = clientTimeout
	workspaces := NewWorkspaceStorage(&coder.StaticClientProvider{Client: client, Namespace: logTestNamespace})
	t.Cleanup(workspaces.Destroy)
	return workspaces
}

func newTestTransitionStorage(t *testing.T, f *transitionFakeCoder, transition codersdk.WorkspaceTransition) *WorkspaceTransitionStorage {
	t.Helper()
	return NewWorkspaceTransitionStorage(newTestWorkspaces(t, f, 30*time.Second), transition)
}

func transitionContext(t *testing.T) context.Context {
	return request.WithNamespace(t.Context(), logTestNamespace)
}

// runTransition calls Create as the generic create handler does, with an empty body.
func runTransition(t *testing.T, s *WorkspaceTransitionStorage, dryRun bool) (*aggregationv1alpha1.CoderWorkspaceTransition, error) {
	t.Helper()
	options := &metav1.CreateOptions{}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	obj, err := s.Create(transitionContext(t), logTestName, &aggregationv1alpha1.CoderWorkspaceTransition{}, nil, options)
	if err != nil {
		return nil, err
	}
	result, ok := obj.(*aggregationv1alpha1.CoderWorkspaceTransition)
	if !ok {
		t.Fatalf("result type %T", obj)
	}
	if result.Name != logTestName || result.Namespace != logTestNamespace {
		t.Fatalf("result metadata: %+v", result.ObjectMeta)
	}
	return result, nil
}

func build(transition codersdk.WorkspaceTransition, status codersdk.ProvisionerJobStatus) codersdk.WorkspaceBuild {
	return codersdk.WorkspaceBuild{Transition: transition, Job: codersdk.ProvisionerJob{Status: status}}
}

const (
	wantConflict = "409"
	wantInternal = "500"
	wantNotFound = "404"
)

// TestWorkspaceTransitionDecisions covers every row of the decision table for start and stop.
func TestWorkspaceTransitionDecisions(t *testing.T) {
	const (
		start  = codersdk.WorkspaceTransitionStart
		stop   = codersdk.WorkspaceTransitionStop
		del    = codersdk.WorkspaceTransitionDelete
		queued = transitionOutcomeQueued
	)
	rows := []struct {
		latest      codersdk.WorkspaceBuild
		start, stop string
	}{
		{build(start, codersdk.ProvisionerJobPending), transitionOutcomeInProgress, wantConflict},
		{build(start, codersdk.ProvisionerJobRunning), transitionOutcomeInProgress, wantConflict},
		{build(start, codersdk.ProvisionerJobSucceeded), transitionOutcomeUnchanged, queued},
		{build(stop, codersdk.ProvisionerJobPending), wantConflict, transitionOutcomeInProgress},
		{build(stop, codersdk.ProvisionerJobRunning), wantConflict, transitionOutcomeInProgress},
		{build(stop, codersdk.ProvisionerJobSucceeded), queued, transitionOutcomeUnchanged},
		{build(del, codersdk.ProvisionerJobPending), wantConflict, wantConflict},
		{build(del, codersdk.ProvisionerJobRunning), wantConflict, wantConflict},
		{build(start, codersdk.ProvisionerJobCanceling), wantConflict, wantConflict},
		{build(stop, codersdk.ProvisionerJobCanceling), wantConflict, wantConflict},
		{build(del, codersdk.ProvisionerJobCanceling), wantConflict, wantConflict},
		{build(start, codersdk.ProvisionerJobFailed), queued, queued},
		{build(start, codersdk.ProvisionerJobCanceled), queued, queued},
		{build(stop, codersdk.ProvisionerJobFailed), queued, queued},
		{build(stop, codersdk.ProvisionerJobCanceled), queued, queued},
		{build(del, codersdk.ProvisionerJobFailed), queued, queued},
		{build(del, codersdk.ProvisionerJobCanceled), queued, queued},
		{build(del, codersdk.ProvisionerJobSucceeded), wantNotFound, wantNotFound},
		{build(start, ""), wantInternal, wantInternal},
		{build(start, codersdk.ProvisionerJobUnknown), wantInternal, wantInternal},
		{build(start, "paused"), wantInternal, wantInternal},
		{build("", codersdk.ProvisionerJobSucceeded), wantInternal, wantInternal},
		{build("bogus", codersdk.ProvisionerJobFailed), wantInternal, wantInternal},
	}
	for _, row := range rows {
		for transition, want := range map[codersdk.WorkspaceTransition]string{start: row.start, stop: row.stop} {
			name := string(transition) + " on " + string(row.latest.Transition) + "/" + string(row.latest.Job.Status)
			t.Run(name, func(t *testing.T) {
				f := newTransitionFakeCoder(t, row.latest)
				result, err := runTransition(t, newTestTransitionStorage(t, f, transition), false)
				posts, posted := f.recordedPosts()
				checkTransitionResult(t, f, transition, want, false, result, err)
				if want != queued {
					if len(posts) != 0 {
						t.Fatalf("POSTs: %d, want 0", len(posts))
					}
					return
				}
				if len(posts) != 1 || posts[0].Transition != transition || posts[0].TemplateVersionID != uuid.Nil || posts[0].DryRun || posts[0].Reason != "" {
					t.Fatalf("POSTs: %+v, want one %s without a version", posts, transition)
				}
				if result.Status.BuildID != posted[0].ID.String() || result.Status.BuildNumber != posted[0].BuildNumber || result.Status.JobStatus != "pending" {
					t.Fatalf("status %+v does not name the new build %+v", result.Status, posted[0])
				}
			})
		}
	}
}

// checkTransitionResult checks an outcome or an error class against want.
func checkTransitionResult(t *testing.T, f *transitionFakeCoder, transition codersdk.WorkspaceTransition, want string, dryRun bool, result *aggregationv1alpha1.CoderWorkspaceTransition, err error) {
	t.Helper()
	f.mu.Lock()
	latest := f.ws.LatestBuild
	f.mu.Unlock()
	switch want {
	case wantConflict:
		if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "retry after") || !strings.Contains(err.Error(), "#7") {
			t.Fatalf("err=%v, want a 409 that names the build and says to retry", err)
		}
		return
	case wantNotFound:
		if !apierrors.IsNotFound(err) {
			t.Fatalf("err=%v, want 404", err)
		}
		return
	case wantInternal:
		if err == nil || !strings.Contains(err.Error(), "assertion failed") {
			t.Fatalf("err=%v, want an assertion failure", err)
		}
		if latest.Job.Status != "" && !strings.Contains(err.Error(), string(latest.Job.Status)) && !strings.Contains(err.Error(), string(latest.Transition)) {
			t.Fatalf("err=%v does not name the value", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("err=%v, want outcome %s", err, want)
	}
	if result.Status.Transition != string(transition) || result.Status.Outcome != want || result.Status.DryRun != dryRun {
		t.Fatalf("status %+v, want transition=%s outcome=%s dryRun=%t", result.Status, transition, want, dryRun)
	}
	switch want {
	case transitionOutcomeInProgress, transitionOutcomeUnchanged:
		if result.Status.BuildID != latest.ID.String() || result.Status.BuildNumber != latest.BuildNumber || result.Status.JobStatus != string(latest.Job.Status) {
			t.Fatalf("status %+v does not name the latest build %+v", result.Status, latest)
		}
	case transitionOutcomeWouldQueue:
		if result.Status.BuildID != "" || result.Status.BuildNumber != 0 || result.Status.JobStatus != "" {
			t.Fatalf("WouldQueue status carries build fields: %+v", result.Status)
		}
	}
}

// TestWorkspaceTransitionDryRun: a dry-run never posts, reports dryRun and WouldQueue, and still
// runs admission.
func TestWorkspaceTransitionDryRun(t *testing.T) {
	start, stop := codersdk.WorkspaceTransitionStart, codersdk.WorkspaceTransitionStop
	for _, tc := range []struct {
		latest     codersdk.WorkspaceBuild
		transition codersdk.WorkspaceTransition
		want       string
	}{
		{build(stop, codersdk.ProvisionerJobSucceeded), start, transitionOutcomeWouldQueue},
		{build(start, codersdk.ProvisionerJobFailed), stop, transitionOutcomeWouldQueue},
		{build(start, codersdk.ProvisionerJobRunning), start, transitionOutcomeInProgress},
		{build(stop, codersdk.ProvisionerJobSucceeded), stop, transitionOutcomeUnchanged},
		{build(stop, codersdk.ProvisionerJobRunning), start, wantConflict},
	} {
		f := newTransitionFakeCoder(t, tc.latest)
		result, err := runTransition(t, newTestTransitionStorage(t, f, tc.transition), true)
		checkTransitionResult(t, f, tc.transition, tc.want, true, result, err)
		if posts, _ := f.recordedPosts(); len(posts) != 0 {
			t.Fatalf("dry-run %s on %+v posted %d builds", tc.transition, tc.latest, len(posts))
		}
	}

	f := newTransitionFakeCoder(t, build(stop, codersdk.ProvisionerJobSucceeded))
	s := newTestTransitionStorage(t, f, start)
	var validated atomic.Int32
	accept := func(context.Context, runtime.Object) error { validated.Add(1); return nil }
	reject := func(context.Context, runtime.Object) error {
		return apierrors.NewForbidden(aggregationv1alpha1.Resource("coderworkspaces"), logTestName, errors.New("denied by policy"))
	}
	dryRun := &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	if _, err := s.Create(transitionContext(t), logTestName, &aggregationv1alpha1.CoderWorkspaceTransition{}, accept, dryRun); err != nil || validated.Load() != 1 {
		t.Fatalf("dry-run with admission: err=%v validations=%d", err, validated.Load())
	}
	for _, options := range []*metav1.CreateOptions{dryRun, {}} {
		if _, err := s.Create(transitionContext(t), logTestName, &aggregationv1alpha1.CoderWorkspaceTransition{}, reject, options); !apierrors.IsForbidden(err) {
			t.Fatalf("rejecting admission (dryRun=%v): err=%v, want 403", options.DryRun, err)
		}
	}
	if posts, _ := f.recordedPosts(); len(posts) != 0 {
		t.Fatalf("admission tests posted %d builds", len(posts))
	}
}

// TestWorkspaceTransitionVersionPolicy: start uses the active version when the template requires
// it or the workspace always updates. Stop never sends a version.
func TestWorkspaceTransitionVersionPolicy(t *testing.T) {
	start, stop := codersdk.WorkspaceTransitionStart, codersdk.WorkspaceTransitionStop
	for _, tc := range []struct {
		name          string
		requireActive bool
		updates       codersdk.AutomaticUpdates
		transition    codersdk.WorkspaceTransition
		wantActive    bool
	}{
		{"start, no policy", false, codersdk.AutomaticUpdatesNever, start, false},
		{"start, require active version", true, codersdk.AutomaticUpdatesNever, start, true},
		{"start, automatic updates always", false, codersdk.AutomaticUpdatesAlways, start, true},
		{"stop, both policies", true, codersdk.AutomaticUpdatesAlways, stop, false},
	} {
		latest := build(stop, codersdk.ProvisionerJobSucceeded)
		if tc.transition == stop {
			latest = build(start, codersdk.ProvisionerJobSucceeded)
		}
		f := newTransitionFakeCoder(t, latest)
		f.update(func(ws *codersdk.Workspace) {
			ws.TemplateRequireActiveVersion = tc.requireActive
			ws.AutomaticUpdates = tc.updates
		})
		if _, err := runTransition(t, newTestTransitionStorage(t, f, tc.transition), false); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := uuid.Nil
		if tc.wantActive {
			want = f.activeID
		}
		if posts, _ := f.recordedPosts(); len(posts) != 1 || posts[0].TemplateVersionID != want {
			t.Fatalf("%s: POSTs %+v, want version %s", tc.name, posts, want)
		}
	}
}

// TestWorkspaceUpdateRunningFollowsVersionPolicy: spec.running=true starts the workspace with
// the same version policy as the start subresource.
func TestWorkspaceUpdateRunningFollowsVersionPolicy(t *testing.T) {
	for _, tc := range []struct {
		updates    codersdk.AutomaticUpdates
		wantActive bool
	}{{codersdk.AutomaticUpdatesNever, false}, {codersdk.AutomaticUpdatesAlways, true}} {
		latest := build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobSucceeded)
		latest.Status = codersdk.WorkspaceStatusStopped
		f := newTransitionFakeCoder(t, latest)
		f.update(func(ws *codersdk.Workspace) { ws.AutomaticUpdates = tc.updates })
		workspaces := newTestWorkspaces(t, f, 30*time.Second)
		current, err := workspaces.Get(transitionContext(t), logTestName, nil)
		if err != nil {
			t.Fatal(err)
		}
		desired := current.(*aggregationv1alpha1.CoderWorkspace).DeepCopy()
		desired.Spec.Running = true
		if _, _, err := workspaces.Update(transitionContext(t), logTestName, rest.DefaultUpdatedObjectInfo(desired), nil, nil, false, nil); err != nil {
			t.Fatalf("updates=%s: %v", tc.updates, err)
		}
		want := uuid.Nil
		if tc.wantActive {
			want = f.activeID
		}
		if posts, _ := f.recordedPosts(); len(posts) != 1 || posts[0].Transition != codersdk.WorkspaceTransitionStart || posts[0].TemplateVersionID != want {
			t.Fatalf("updates=%s: POSTs %+v, want one start with version %s", tc.updates, posts, want)
		}
	}
}

// TestWorkspaceTransitionCoderConflictRereads: after a Coder 409 the server re-reads the workspace
// once and never posts again.
func TestWorkspaceTransitionCoderConflictRereads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after codersdk.WorkspaceBuild
		want  string
	}{
		{"another client started it", build(codersdk.WorkspaceTransitionStart, codersdk.ProvisionerJobPending), transitionOutcomeInProgress},
		{"a stop is still active", build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobRunning), wantConflict},
	} {
		f := newTransitionFakeCoder(t, build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobSucceeded))
		after := tc.after
		after.ID, after.BuildNumber = uuid.New(), 8
		f.set(func(f *transitionFakeCoder) {
			f.onPost = func(w http.ResponseWriter, _ *http.Request) {
				f.update(func(ws *codersdk.Workspace) { ws.LatestBuild = after })
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(codersdk.Response{Message: "A workspace build is already active."})
			}
		})
		result, err := runTransition(t, newTestTransitionStorage(t, f, codersdk.WorkspaceTransitionStart), false)
		if posts, _ := f.recordedPosts(); len(posts) != 1 || f.rereads.Load() != 1 {
			t.Fatalf("%s: POSTs=%d rereads=%d, want 1 and 1", tc.name, len(posts), f.rereads.Load())
		}
		if tc.want == wantConflict {
			if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "A workspace build is already active.") {
				t.Fatalf("%s: err=%v, want 409 with Coder's message", tc.name, err)
			}
			continue
		}
		if err != nil || result.Status.Outcome != tc.want || result.Status.BuildID != after.ID.String() || result.Status.BuildNumber != 8 {
			t.Fatalf("%s: err=%v status=%+v", tc.name, err, result)
		}
	}
}

// TestWorkspaceTransitionUncertainPost: when the POST times out, one confirming re-read decides
// between Queued and an uncertain 504. The request budget also ends a stalled lookup.
func TestWorkspaceTransitionUncertainPost(t *testing.T) {
	stalledPost := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	for _, tc := range []struct {
		name    string
		changed bool
		status  int // a Coder status for the POST; 0 stalls it until the client times out
	}{
		{"the build was queued", true, 0},
		{"nothing changed", false, 0},
		{"a proxy answered 502", false, http.StatusBadGateway},
		{"a proxy answered 503 after the build was queued", true, http.StatusServiceUnavailable},
		{"Coder answered 504", false, http.StatusGatewayTimeout},
	} {
		f := newTransitionFakeCoder(t, build(codersdk.WorkspaceTransitionStart, codersdk.ProvisionerJobSucceeded))
		queued := build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobPending)
		queued.ID, queued.BuildNumber = uuid.New(), 8
		f.set(func(f *transitionFakeCoder) {
			f.onPost = func(w http.ResponseWriter, r *http.Request) {
				if tc.changed {
					f.update(func(ws *codersdk.Workspace) { ws.LatestBuild = queued })
				}
				if tc.status != 0 {
					writeLogFakeError(w, tc.status)
					return
				}
				stalledPost(w, r)
			}
		})
		s := NewWorkspaceTransitionStorage(newTestWorkspaces(t, f, 300*time.Millisecond), codersdk.WorkspaceTransitionStop)
		watcher, err := s.workspaces.Watch(transitionContext(t), nil)
		if err != nil {
			t.Fatalf("start workspace watch: %v", err)
		}
		defer watcher.Stop()
		result, err := runTransition(t, s, false)
		if posts, _ := f.recordedPosts(); len(posts) != 1 || f.rereads.Load() != 1 {
			t.Fatalf("%s: POSTs=%d rereads=%d, want 1 and 1", tc.name, len(posts), f.rereads.Load())
		}
		if !tc.changed {
			if !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") || !strings.Contains(err.Error(), "re-read") {
				t.Fatalf("%s: err=%v, want an uncertain 504", tc.name, err)
			}
			continue
		}
		if err != nil || result.Status.Outcome != transitionOutcomeQueued || result.Status.BuildID != queued.ID.String() {
			t.Fatalf("%s: err=%v result=%+v, want Queued with the new build", tc.name, err, result)
		}
		if event := receiveWatchEvent(t, watcher, watchEventTimeout); workspaceFromWatchEvent(t, event).Status.LatestBuildID != queued.ID.String() {
			t.Fatalf("%s: watch event %s, want the confirmed build", tc.name, event.Type)
		}
	}

	f := newTransitionFakeCoder(t, build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobSucceeded))
	gate := make(chan struct{})
	defer close(gate)
	f.set(func(f *transitionFakeCoder) { f.lookupGate = gate })
	s := newTestTransitionStorage(t, f, codersdk.WorkspaceTransitionStart)
	s.budget, s.confirmTimeout = 200*time.Millisecond, 100*time.Millisecond
	began := time.Now()
	if _, err := runTransition(t, s, false); !apierrors.IsTimeout(err) {
		t.Fatalf("stalled lookup: err=%v, want 504", err)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("stalled lookup took %s", elapsed)
	}
	if posts, _ := f.recordedPosts(); len(posts) != 0 {
		t.Fatalf("stalled lookup posted %d builds", len(posts))
	}

	// A stalled POST and a stalled confirming re-read together stay inside the one budget.
	f = newTransitionFakeCoder(t, build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobSucceeded))
	stall := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	f.set(func(f *transitionFakeCoder) { f.onPost, f.onReread = stall, stall })
	s = newTestTransitionStorage(t, f, codersdk.WorkspaceTransitionStart)
	s.budget, s.confirmTimeout = 600*time.Millisecond, 200*time.Millisecond
	began = time.Now()
	if _, err := runTransition(t, s, false); !apierrors.IsTimeout(err) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("stalled POST and re-read: err=%v, want an uncertain 504", err)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("stalled POST and re-read took %s, want about the 600ms budget", elapsed)
	}
	if posts, _ := f.recordedPosts(); len(posts) != 1 || f.rereads.Load() != 1 {
		t.Fatalf("stalled POST and re-read: POSTs=%d rereads=%d, want 1 and 1", len(posts), f.rereads.Load())
	}

	// The confirm reserve must be a part of the budget.
	s.budget, s.confirmTimeout = time.Second, time.Second
	if _, err := runTransition(t, s, false); err == nil || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("budget equal to the confirm reserve: err=%v, want an assertion failure", err)
	}
}

// TestWorkspaceTransitionRequestChecks covers the body, identity checks, and Coder errors.
func TestWorkspaceTransitionRequestChecks(t *testing.T) {
	f := newTransitionFakeCoder(t, build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobSucceeded))
	s := newTestTransitionStorage(t, f, codersdk.WorkspaceTransitionStart)
	ctx := transitionContext(t)

	mismatch := &aggregationv1alpha1.CoderWorkspaceTransition{ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.other"}}
	if _, err := s.Create(ctx, logTestName, mismatch, nil, nil); !apierrors.IsBadRequest(err) {
		t.Fatalf("body name mismatch: err=%v, want 400", err)
	}
	if _, err := s.Create(ctx, logTestName, &aggregationv1alpha1.CoderWorkspace{}, nil, nil); !apierrors.IsBadRequest(err) {
		t.Fatalf("wrong body type: err=%v, want 400", err)
	}
	if f.lookups.Load() != 0 {
		t.Fatalf("invalid bodies reached Coder: %d lookups", f.lookups.Load())
	}

	for name, check := range map[string]func(error) bool{
		"Acme.alice.dev":   apierrors.IsBadRequest, // organization alias
		"acme.alice.Dev":   apierrors.IsBadRequest, // case variant of the leaf
		"other.alice.dev":  apierrors.IsNotFound,   // another organization, opaque
		"not-a-valid-name": apierrors.IsBadRequest,
	} {
		if _, err := s.Create(ctx, name, &aggregationv1alpha1.CoderWorkspaceTransition{}, nil, nil); !check(err) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if posts, _ := f.recordedPosts(); len(posts) != 0 {
		t.Fatalf("identity failures posted %d builds", len(posts))
	}

	// The body name may equal the URL name, and a client status is ignored.
	same := &aggregationv1alpha1.CoderWorkspaceTransition{
		ObjectMeta: metav1.ObjectMeta{Name: logTestName},
		Status:     aggregationv1alpha1.CoderWorkspaceTransitionStatus{Outcome: "Unchanged", DryRun: true, BuildID: "x"},
	}
	obj, err := s.Create(ctx, logTestName, same, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status := obj.(*aggregationv1alpha1.CoderWorkspaceTransition).Status; status.Outcome != transitionOutcomeQueued || status.DryRun || status.BuildID == "x" {
		t.Fatalf("client status leaked into the result: %+v", status)
	}

	f.set(func(f *transitionFakeCoder) {
		f.onPost = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(codersdk.Response{Message: "Missing required parameter \"region\"."})
		}
	})
	if _, err := runTransition(t, s, false); !apierrors.IsBadRequest(err) || !strings.Contains(err.Error(), "region") {
		t.Fatalf("Coder 400: err=%v, want 400 with Coder's message", err)
	}
	if f.rereads.Load() != 0 {
		t.Fatalf("a definite Coder error was re-read %d times", f.rereads.Load())
	}
}

// TestWorkspaceTransitionQueuedSendsWatchEvent: a queued build sends a Modified event for the
// workspace, as an update of spec.running does. A dry-run and a request that queues nothing send
// none.
func TestWorkspaceTransitionQueuedSendsWatchEvent(t *testing.T) {
	f := newTransitionFakeCoder(t, build(codersdk.WorkspaceTransitionStop, codersdk.ProvisionerJobSucceeded))
	s := newTestTransitionStorage(t, f, codersdk.WorkspaceTransitionStart)
	watcher, err := s.workspaces.Watch(transitionContext(t), nil)
	if err != nil {
		t.Fatalf("start workspace watch: %v", err)
	}
	defer watcher.Stop()

	if _, err := runTransition(t, s, true); err != nil {
		t.Fatalf("dry-run start: %v", err)
	}
	assertNoWatchEvent(t, watcher, 200*time.Millisecond)

	result, err := runTransition(t, s, false)
	if err != nil || result.Status.Outcome != transitionOutcomeQueued {
		t.Fatalf("start: err=%v result=%+v, want Queued", err, result)
	}
	event := receiveWatchEvent(t, watcher, watchEventTimeout)
	workspace := workspaceFromWatchEvent(t, event)
	if event.Type != watch.Modified || workspace.Name != logTestName || workspace.Namespace != logTestNamespace ||
		workspace.Status.LatestBuildID != result.Status.BuildID {
		t.Fatalf("event %s %s/%s build %q, want Modified %s/%s build %q", event.Type, workspace.Namespace, workspace.Name,
			workspace.Status.LatestBuildID, logTestNamespace, logTestName, result.Status.BuildID)
	}

	stop := NewWorkspaceTransitionStorage(s.workspaces, codersdk.WorkspaceTransitionStop)
	if result, err := runTransition(t, stop, false); err != nil || result.Status.Outcome != transitionOutcomeUnchanged {
		t.Fatalf("stop of a stopped workspace: err=%v result=%+v, want Unchanged", err, result)
	}
	assertNoWatchEvent(t, watcher, 200*time.Millisecond)
}
