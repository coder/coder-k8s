package storage

// Workspace resourceVersion contract (#109): the token is an opaque fingerprint of the returned
// representation, so a changed projection must change it even when Coder does not advance
// Workspace.UpdatedAt (observed on Coder 2.37.2 for builds, rename, TTL and autostart). The mock
// models that with frozenWorkspaceUpdatedAt.

import (
	"testing"
	"time"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
)

const versionFixtureName = "acme.alice.dev-workspace"

func newFrozenWorkspaceStorage(t *testing.T) (*WorkspaceStorage, *mockCoderServerState) {
	t.Helper()

	server, state := newMockCoderServer(t)
	state.setFrozenWorkspaceUpdatedAt(true)

	return NewWorkspaceStorage(newTestClientProvider(t, server.URL)), state
}

func getWorkspaceVersionFixture(t *testing.T, workspaceStorage *WorkspaceStorage, name string) *aggregationv1alpha1.CoderWorkspace {
	t.Helper()

	obj, err := workspaceStorage.Get(namespacedContext("control-plane"), name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("GET %s: %v", name, err)
	}

	return obj.(*aggregationv1alpha1.CoderWorkspace)
}

func TestWorkspaceResourceVersionFollowsBackendChanges(t *testing.T) {
	workspaceStorage, state := newFrozenWorkspaceStorage(t)
	ctx := namespacedContext("control-plane")

	watcher, err := workspaceStorage.Watch(ctx, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watcher.Stop()

	before := getWorkspaceVersionFixture(t, workspaceStorage, versionFixtureName)
	if again := getWorkspaceVersionFixture(t, workspaceStorage, versionFixtureName); again.ResourceVersion != before.ResourceVersion {
		t.Fatalf("stable read: unchanged workspace must keep its resourceVersion, got %q then %q", before.ResourceVersion, again.ResourceVersion)
	}

	desired := before.DeepCopy()
	desired.Spec.Running = !before.Spec.Running
	updatedObj, _, err := workspaceStorage.Update(ctx, versionFixtureName, testUpdatedObjectInfo{obj: desired}, nil, rest.ValidateAllObjectUpdateFunc, false, nil)
	if err != nil {
		t.Fatalf("Update(spec.running): %v", err)
	}
	updated := updatedObj.(*aggregationv1alpha1.CoderWorkspace)

	after := getWorkspaceVersionFixture(t, workspaceStorage, versionFixtureName)
	if after.UID != before.UID {
		t.Fatalf("same workspace ID must keep its UID, got %q want %q", after.UID, before.UID)
	}
	if after.Status.LatestBuildID == before.Status.LatestBuildID {
		t.Fatal("fixture: Update must have created a new build")
	}
	if after.ResourceVersion == before.ResourceVersion {
		t.Fatalf("build changed the representation but GET resourceVersion stayed %q", before.ResourceVersion)
	}
	// The mock backend is quiescent between the response and this GET, so the projections agree.
	if updated.ResourceVersion != after.ResourceVersion {
		t.Fatalf("quiescent Update response resourceVersion %q must equal the next GET %q", updated.ResourceVersion, after.ResourceVersion)
	}

	modified := receiveWatchEvent(t, watcher, 5*time.Second)
	if modified.Type != watch.Modified {
		t.Fatalf("expected Modified watch event, got %s", modified.Type)
	}
	if got := modified.Object.(*aggregationv1alpha1.CoderWorkspace).ResourceVersion; got != updated.ResourceVersion {
		t.Fatalf("local Modified event token %q must equal the mutation response %q", got, updated.ResourceVersion)
	}

	// Stale UPDATE: the real pre-build token no longer matches the changed representation and is
	// rejected before any build transition (the actual #109 symptom, not a synthetic mismatch).
	transitionsBefore := len(state.buildTransitionsSnapshot())
	stale := before.DeepCopy()
	stale.Spec.Running = !after.Spec.Running
	if _, _, err := workspaceStorage.Update(ctx, versionFixtureName, testUpdatedObjectInfo{obj: stale}, nil, rest.ValidateAllObjectUpdateFunc, false, nil); !apierrors.IsConflict(err) {
		t.Fatalf("stale UPDATE with the pre-build token %q: want 409 Conflict, got %v", before.ResourceVersion, err)
	}
	if got := len(state.buildTransitionsSnapshot()); got != transitionsBefore {
		t.Fatalf("stale UPDATE must not request a build transition, transitions %d -> %d", transitionsBefore, got)
	}

	listObj, err := workspaceStorage.List(ctx, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, item := range listObj.(*aggregationv1alpha1.CoderWorkspaceList).Items {
		if item.Name != versionFixtureName {
			continue
		}
		found = true
		if item.ResourceVersion != after.ResourceVersion {
			t.Fatalf("LIST resourceVersion %q must equal GET %q", item.ResourceVersion, after.ResourceVersion)
		}
	}
	if !found {
		t.Fatalf("LIST must contain %s", versionFixtureName)
	}

	// Stale DELETE on the same ID: 409, no delete transition, object intact.
	transitionsBefore = len(state.buildTransitionsSnapshot())
	if _, _, err := workspaceStorage.Delete(ctx, versionFixtureName, nil, preconditions(nil, stringPtr(before.ResourceVersion))); !apierrors.IsConflict(err) {
		t.Fatalf("stale DELETE: want 409 Conflict, got %v", err)
	}
	if got := len(state.buildTransitionsSnapshot()); got != transitionsBefore {
		t.Fatalf("stale DELETE must not request a build transition, transitions %d -> %d", transitionsBefore, got)
	}
	if intact := getWorkspaceVersionFixture(t, workspaceStorage, versionFixtureName); intact.ResourceVersion != after.ResourceVersion {
		t.Fatal("object must be intact after the rejected delete")
	}

	// Live UID + token proceeds to the ordinary asynchronous delete.
	if _, _, err := workspaceStorage.Delete(ctx, versionFixtureName, nil, preconditions(uidPtr(string(after.UID)), stringPtr(after.ResourceVersion))); err != nil {
		t.Fatalf("live UID+resourceVersion delete must proceed: %v", err)
	}
	transitions := state.buildTransitionsSnapshot()
	if len(transitions) == 0 || transitions[len(transitions)-1] != codersdk.WorkspaceTransitionDelete {
		t.Fatalf("expected a delete transition after the accepted delete, got %v", transitions)
	}
}

func TestWorkspaceResourceVersionTracksOutOfBandChanges(t *testing.T) {
	newTTL := int64(7200000)
	newAutostart := "CRON_TZ=UTC 30 9 * * 1-5"

	cases := map[string]struct {
		mutate     func(*codersdk.Workspace)
		wantChange bool
	}{
		"noop":                      {mutate: func(*codersdk.Workspace) {}, wantChange: false},
		"backend-only updatedAt":    {mutate: func(w *codersdk.Workspace) { w.LatestBuild.UpdatedAt = w.LatestBuild.UpdatedAt.Add(time.Hour) }, wantChange: false},
		"ttl":                       {mutate: func(w *codersdk.Workspace) { w.TTLMillis = &newTTL }, wantChange: true},
		"autostart":                 {mutate: func(w *codersdk.Workspace) { w.AutostartSchedule = &newAutostart }, wantChange: true},
		"build status (same build)": {mutate: func(w *codersdk.Workspace) { w.LatestBuild.Status = codersdk.WorkspaceStatusStopping }, wantChange: true},
		"lastUsedAt":                {mutate: func(w *codersdk.Workspace) { w.LastUsedAt = w.LastUsedAt.Add(time.Minute) }, wantChange: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			workspaceStorage, state := newFrozenWorkspaceStorage(t)
			before := getWorkspaceVersionFixture(t, workspaceStorage, versionFixtureName)
			state.mutateWorkspace(t, "alice", "dev-workspace", tc.mutate)
			after := getWorkspaceVersionFixture(t, workspaceStorage, versionFixtureName)
			if after.UID != before.UID {
				t.Fatalf("UID must not change on an out-of-band %s", name)
			}
			if changed := after.ResourceVersion != before.ResourceVersion; changed != tc.wantChange {
				t.Fatalf("%s: resourceVersion changed=%v, want %v (%q -> %q)", name, changed, tc.wantChange, before.ResourceVersion, after.ResourceVersion)
			}
		})
	}

	t.Run("rename", func(t *testing.T) {
		workspaceStorage, state := newFrozenWorkspaceStorage(t)
		before := getWorkspaceVersionFixture(t, workspaceStorage, versionFixtureName)
		state.renameWorkspace(t, "alice", "dev-workspace", "dev-renamed")
		after := getWorkspaceVersionFixture(t, workspaceStorage, "acme.alice.dev-renamed")
		if after.UID != before.UID {
			t.Fatal("rename must keep the UID (same workspace ID)")
		}
		if after.ResourceVersion == before.ResourceVersion {
			t.Fatalf("rename changed the projection but resourceVersion stayed %q", before.ResourceVersion)
		}
	})
}
