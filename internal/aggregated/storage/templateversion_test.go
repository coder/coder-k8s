package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
)

// seedTemplateVersion adds a version named name to the mock's only template and returns it.
func seedTemplateVersion(t *testing.T, state *mockCoderServerState, name string) codersdk.TemplateVersion {
	t.Helper()

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.templatesByID) != 1 {
		t.Fatalf("assertion failed: expected one seeded template, got %d", len(state.templatesByID))
	}
	for templateID := range state.templatesByID {
		version := codersdk.TemplateVersion{
			ID:         uuid.New(),
			TemplateID: &templateID,
			CreatedAt:  time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC),
			UpdatedAt:  time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC),
			Name:       name,
			Job:        codersdk.ProvisionerJob{Status: codersdk.ProvisionerJobSucceeded},
		}
		version.OrganizationID = state.organization.ID
		state.templateVersionsByID[version.ID] = version
		return version
	}
	panic("unreachable")
}

func getTemplateVersion(t *testing.T, storage *TemplateVersionStorage, name string) (*aggregationv1alpha1.CoderTemplateVersion, error) {
	t.Helper()

	obj, err := storage.Get(namespacedContext("control-plane"), name, nil)
	if err != nil {
		return nil, err
	}
	version, ok := obj.(*aggregationv1alpha1.CoderTemplateVersion)
	if !ok {
		t.Fatalf("expected *CoderTemplateVersion, got %T", obj)
	}
	return version, nil
}

func TestTemplateVersionStorageGet(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))
	dotted := seedTemplateVersion(t, state, "v1.2.3")
	// Older Coder releases allowed names that the current name rules reject; they stay readable.
	legacy := seedTemplateVersion(t, state, "-legacy--v1_")

	state.resetRequests()
	active, err := getTemplateVersion(t, storage, "acme.starter-template.starter-template-v1")
	if err != nil {
		t.Fatalf("get active version: %v", err)
	}
	if !active.Status.Active || active.Name != "acme.starter-template.starter-template-v1" || active.Namespace != "control-plane" {
		t.Fatalf("unexpected active version: name=%q namespace=%q active=%v", active.Name, active.Namespace, active.Status.Active)
	}
	// Exactly organization, template and version-by-name; no template source download.
	if got := state.requests(); len(got) != 3 || strings.Contains(strings.Join(got, " "), "/files") {
		t.Fatalf("expected exactly 3 Coder requests and no file download, got %v", got)
	}

	again, err := getTemplateVersion(t, storage, "acme.starter-template.starter-template-v1")
	if err != nil || again.Name != active.Name || again.ResourceVersion != active.ResourceVersion {
		t.Fatalf("repeated get changed identity or resourceVersion: %v %+v", err, again)
	}

	for _, version := range []codersdk.TemplateVersion{dotted, legacy} {
		got, err := getTemplateVersion(t, storage, "acme.starter-template."+version.Name)
		if err != nil {
			t.Fatalf("get version %q: %v", version.Name, err)
		}
		if got.Name != "acme.starter-template."+version.Name || string(got.UID) != version.ID.String() || got.Status.Active {
			t.Fatalf("unexpected version %q: name=%q uid=%q active=%v", version.Name, got.Name, got.UID, got.Status.Active)
		}
	}

	// Promoting another version changes the active flag and therefore the fingerprint.
	state.mu.Lock()
	for id, template := range state.templatesByID {
		template.ActiveVersionID = dotted.ID
		state.templatesByID[id] = template
	}
	state.mu.Unlock()
	demoted, err := getTemplateVersion(t, storage, "acme.starter-template.starter-template-v1")
	if err != nil {
		t.Fatalf("get demoted version: %v", err)
	}
	if demoted.Status.Active || demoted.ResourceVersion == active.ResourceVersion {
		t.Fatalf("expected a demoted version with a new resourceVersion, got active=%v rv=%q", demoted.Status.Active, demoted.ResourceVersion)
	}
	if len(state.mutations()) != 0 {
		t.Fatalf("reads must not mutate Coder: %v", state.mutations())
	}
}

func TestTemplateVersionStorageGetIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		requestName     func(orgID uuid.UUID) string
		caseInsensitive bool
		wantBadRequest  bool
		wantNotFound    bool
	}{
		{name: "organization ID alias", requestName: func(orgID uuid.UUID) string { return orgID.String() + ".starter-template.starter-template-v1" }, wantBadRequest: true},
		{name: "wrong template casing", requestName: func(uuid.UUID) string { return "acme.Starter-Template.starter-template-v1" }, caseInsensitive: true, wantBadRequest: true},
		{name: "wrong version casing", requestName: func(uuid.UUID) string { return "acme.starter-template.Starter-Template-V1" }, wantNotFound: true},
		// A backend that matched version names case-insensitively must not alias the object.
		{name: "wrong version casing, case-insensitive backend", requestName: func(uuid.UUID) string { return "acme.starter-template.Starter-Template-V1" }, caseInsensitive: true, wantBadRequest: true},
		{name: "unknown version", requestName: func(uuid.UUID) string { return "acme.starter-template.missing" }, wantNotFound: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, state := newMockCoderServer(t)
			defer server.Close()
			state.caseInsensitiveLeafLookups = tc.caseInsensitive
			storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))

			got, err := getTemplateVersion(t, storage, tc.requestName(state.organization.ID))
			if tc.wantBadRequest && !apierrors.IsBadRequest(err) {
				t.Fatalf("expected BadRequest, got object=%v err=%v", got, err)
			}
			if tc.wantNotFound && !apierrors.IsNotFound(err) {
				t.Fatalf("expected NotFound, got object=%v err=%v", got, err)
			}
		})
	}
}

func TestTemplateVersionStorageGetRejectsUnsafeNamesWithoutCoderRequests(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))

	for _, name := range []string{
		"acme.starter-template", "acme..v1", "acme.starter-template.",
		"acme.starter-template.a?b", "acme.starter-template.a#b", "acme.starter-template..",
		"acme.starter-template.a/../b", "acme.starter-template.a%2Fb", "acme.starter-template.a b",
		"acme.starter-template.a\\b", "acme.starter-template.a\nb", "ac?me.starter-template.v1",
		"acme.star#ter-template.v1", "acme.a%2Fb.v1", "acme.a b.v1",
	} {
		if _, err := getTemplateVersion(t, storage, name); !apierrors.IsBadRequest(err) {
			t.Fatalf("expected BadRequest for %q, got %v", name, err)
		}
	}
	if got := state.requests(); len(got) != 0 {
		t.Fatalf("rejected names must not reach Coder, got %v", got)
	}
}

func TestRequireCanonicalVersionSegmentBuildsHintOnlyOnMismatch(t *testing.T) {
	t.Parallel()

	mustNotBuild := func() string { panic("hint built on a non-mismatch path") }
	if err := requireCanonicalVersionSegment("acme.docker.v1", "version", "v1", "v1", mustNotBuild); err != nil {
		t.Fatalf("matching names: %v", err)
	}
	if err := requireCanonicalVersionSegment("acme.docker.v1", "organization", "acme", "", mustNotBuild); err == nil || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("expected an assertion error for an empty resolved name, got %v", err)
	}
	err := requireCanonicalVersionSegment("acme.docker.V1", "version", "V1", "v1", func() string { return "acme.docker.v1" })
	if !apierrors.IsBadRequest(err) || !strings.Contains(err.Error(), `"acme.docker.v1"`) {
		t.Fatalf("expected a 400 naming the canonical name, got %v", err)
	}
}

func TestTemplateVersionStorageGetStopsAtTimeBudget(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	state.mu.Lock()
	state.versionReadDelay = 500 * time.Millisecond
	state.mu.Unlock()
	storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))
	storage.readBudget = 200 * time.Millisecond

	started := time.Now()
	got, err := getTemplateVersion(t, storage, "acme.starter-template.starter-template-v1")
	if !apierrors.IsTimeout(err) || got != nil {
		t.Fatalf("expected a 504 Timeout, got object=%v err=%v", got, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the get ran for %s; the budget did not stop it", elapsed)
	}
}
