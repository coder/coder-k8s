package storage

// Canonical identity coverage for issue #105: request names must use the backend's canonical
// organization and owner names. Coder itself resolves aliases ("default" organization, "me" user,
// raw IDs) server-side, which these tests model with a thin alias-resolving proxy in front of the
// package mock so that the real rest.Storage entrypoints are exercised end to end.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
)

// otherOrganization is a second organization that only the alias-resolving front server knows about.
var otherOrganization = codersdk.Organization{
	MinimalOrganization: codersdk.MinimalOrganization{
		ID:          uuid.MustParse("6f6d3ad4-9b6c-4d54-9a3c-2f0a7a1f7c11"),
		Name:        "other",
		DisplayName: "Other",
	},
}

// newAliasResolvingCoderServer fronts the package mock with Coder's server-side alias behavior:
// /organizations/default resolves to the mock organization ("acme") and /users/me to the mock user
// ("alice"); responses keep canonical names. It also serves a second organization ("other"),
// returns 403 for the "forbidden-org" organization and the "forbidden-user" user, and 503 for the
// "outage-org" organization.
func newAliasResolvingCoderServer(t *testing.T) (*httptest.Server, *mockCoderServerState) {
	t.Helper()

	backend, state := newMockCoderServer(t)
	t.Cleanup(backend.Close)

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.URL.Path = strings.Replace(r.URL.Path, "/api/v2/organizations/default", "/api/v2/organizations/acme", 1)
		if r.URL.Path == "/api/v2/users/me" {
			r.URL.Path = "/api/v2/users/alice"
		}
		r.URL.Path = strings.Replace(r.URL.Path, "/api/v2/users/me/", "/api/v2/users/alice/", 1)
		r.URL.RawPath = ""
	}

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v2/organizations/other" || r.URL.Path == "/api/v2/organizations/"+otherOrganization.ID.String()):
			writeJSON(w, http.StatusOK, otherOrganization)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/forbidden-org":
			writeCoderError(w, http.StatusForbidden, "forbidden organization")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/users/forbidden-user":
			writeCoderError(w, http.StatusForbidden, "forbidden user")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/outage-org":
			writeCoderError(w, http.StatusServiceUnavailable, "organization lookup unavailable")
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(front.Close)

	return front, state
}

// renameOrganization renames the mock organization so tests can model a deployment whose canonical
// organization name literally equals an alias keyword.
func (s *mockCoderServerState) renameOrganization(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.organization.Name
	s.organization.Name = name
	if templates, ok := s.templateIDsByOrg[previous]; ok {
		delete(s.templateIDsByOrg, previous)
		s.templateIDsByOrg[name] = templates
	}
	for id, template := range s.templatesByID {
		template.OrganizationName = name
		s.templatesByID[id] = template
	}
	for id, workspace := range s.workspacesByID {
		if workspace.OrganizationName == previous {
			workspace.OrganizationName = name
			s.workspacesByID[id] = workspace
		}
	}
}

// seedWorkspace adds a workspace owned by owner (registering the owner as a user) in the given organization.
func (s *mockCoderServerState) seedWorkspace(owner, name string, organization codersdk.Organization) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.usersByName[owner]; !ok {
		s.usersByName[owner] = codersdk.User{ReducedUser: codersdk.ReducedUser{MinimalUser: codersdk.MinimalUser{ID: uuid.New(), Username: owner}}}
	}

	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	workspaceID := uuid.New()
	var templateID uuid.UUID
	var templateName string
	for id, template := range s.templatesByID {
		templateID, templateName = id, template.Name
		break
	}
	s.workspacesByID[workspaceID] = codersdk.Workspace{
		ID:               workspaceID,
		CreatedAt:        now,
		UpdatedAt:        now,
		OwnerName:        owner,
		OwnerID:          s.usersByName[owner].ID,
		OrganizationID:   organization.ID,
		OrganizationName: organization.Name,
		TemplateID:       templateID,
		TemplateName:     templateName,
		Name:             name,
		LatestBuild: codersdk.WorkspaceBuild{
			ID:                 uuid.New(),
			WorkspaceID:        workspaceID,
			WorkspaceName:      name,
			WorkspaceOwnerName: owner,
			Transition:         codersdk.WorkspaceTransitionStart,
			Status:             codersdk.WorkspaceStatusRunning,
			CreatedAt:          now,
			UpdatedAt:          now,
		},
	}
	if _, ok := s.workspaceIDsByUser[owner]; !ok {
		s.workspaceIDsByUser[owner] = map[string]uuid.UUID{}
	}
	s.workspaceIDsByUser[owner][name] = workspaceID
}

func noopUpdate() rest.UpdatedObjectInfo {
	return rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, current runtime.Object) (runtime.Object, error) {
		return current.DeepCopyObject(), nil
	})
}

func newIdentityTemplate(name, organization string) *aggregationv1alpha1.CoderTemplate {
	return &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: aggregationv1alpha1.CoderTemplateSpec{
			Organization: organization,
			Files:        map[string]string{"main.tf": "resource \"null_resource\" \"example\" {}"},
		},
	}
}

func newIdentityWorkspace(name, organization string) *aggregationv1alpha1.CoderWorkspace {
	return &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: aggregationv1alpha1.CoderWorkspaceSpec{
			Organization: organization,
			TemplateName: "starter-template",
			Running:      true,
		},
	}
}

func assertAliasBadRequest(t *testing.T, verb string, err error, canonicalName string) {
	t.Helper()

	if !apierrors.IsBadRequest(err) {
		t.Fatalf("%s: expected BadRequest naming the canonical name %q, got %v", verb, canonicalName, err)
	}
	if !strings.Contains(err.Error(), `"`+canonicalName+`"`) {
		t.Fatalf("%s: BadRequest must name the canonical name %q, got %q", verb, canonicalName, err.Error())
	}
}

func assertOpaqueNotFound(t *testing.T, verb string, err error, mustNotDisclose ...string) {
	t.Helper()

	if !apierrors.IsNotFound(err) {
		t.Fatalf("%s: expected opaque NotFound, got %v", verb, err)
	}
	for _, secret := range mustNotDisclose {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: NotFound must not disclose %q, got %q", verb, secret, err.Error())
		}
	}
}

func assertNoMutations(t *testing.T, state *mockCoderServerState) {
	t.Helper()

	if mutations := state.mutations(); len(mutations) != 0 {
		t.Fatalf("expected no backend mutations, got %v", mutations)
	}
}

func TestTemplateStorageRejectsOrganizationAliasBeforeMutation(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	templates := NewTemplateStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	verbs := []struct {
		verb      string
		canonical string
		run       func() error
	}{
		{"get", "acme.starter-template", func() error {
			_, err := templates.Get(ctx, "default.starter-template", nil)
			return err
		}},
		{"create", "acme.new-template", func() error {
			_, err := templates.Create(ctx, newIdentityTemplate("default.new-template", "default"), rest.ValidateAllObjectFunc, nil)
			return err
		}},
		{"update", "acme.starter-template", func() error {
			_, _, err := templates.Update(ctx, "default.starter-template", noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil)
			return err
		}},
		{"create-on-update", "acme.new-template", func() error {
			_, _, err := templates.Update(
				ctx,
				"default.new-template",
				testUpdatedObjectInfo{obj: newIdentityTemplate("default.new-template", "default")},
				rest.ValidateAllObjectFunc,
				rest.ValidateAllObjectUpdateFunc,
				true,
				nil,
			)
			return err
		}},
		{"delete", "acme.starter-template", func() error {
			_, _, err := templates.Delete(ctx, "default.starter-template", rest.ValidateAllObjectFunc, nil)
			return err
		}},
	}
	for _, tc := range verbs {
		assertAliasBadRequest(t, tc.verb, tc.run(), tc.canonical)
	}

	assertNoMutations(t, state)
	if !state.hasTemplate("acme", "starter-template") || state.hasTemplate("acme", "new-template") {
		t.Fatal("rejected requests must leave backend templates unchanged")
	}
}

func TestTemplateStorageAcceptsLiteralDefaultOrganizationName(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	state.renameOrganization("default")

	templates := NewTemplateStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	obj, err := templates.Get(ctx, "default.starter-template", nil)
	if err != nil {
		t.Fatalf("get with literal canonical organization name: %v", err)
	}
	if got := obj.(*aggregationv1alpha1.CoderTemplate).Name; got != "default.starter-template" {
		t.Fatalf("expected metadata.name default.starter-template, got %q", got)
	}

	created, err := templates.Create(ctx, newIdentityTemplate("default.new-template", "default"), rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("create with literal canonical organization name: %v", err)
	}
	if got := created.(*aggregationv1alpha1.CoderTemplate).Name; got != "default.new-template" {
		t.Fatalf("expected metadata.name default.new-template, got %q", got)
	}

	if _, _, err := templates.Update(ctx, "default.new-template", noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil); err != nil {
		t.Fatalf("repeated apply with literal canonical organization name: %v", err)
	}
	if _, _, err := templates.Delete(ctx, "default.new-template", rest.ValidateAllObjectFunc, nil); err != nil {
		t.Fatalf("delete with literal canonical organization name: %v", err)
	}
}

func TestTemplateStorageCanonicalNamesRoundTripWithAliasCapableBackend(t *testing.T) {
	t.Parallel()

	server, _ := newAliasResolvingCoderServer(t)
	templates := NewTemplateStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	created, err := templates.Create(ctx, newIdentityTemplate("acme.new-template", "acme"), rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("canonical create: %v", err)
	}
	if got := created.(*aggregationv1alpha1.CoderTemplate).Name; got != "acme.new-template" {
		t.Fatalf("expected created metadata.name acme.new-template, got %q", got)
	}

	listObj, err := templates.List(ctx, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	list := listObj.(*aggregationv1alpha1.CoderTemplateList)
	if len(list.Items) != 2 {
		t.Fatalf("expected two templates, got %d", len(list.Items))
	}
	for _, item := range list.Items {
		obj, err := templates.Get(ctx, item.Name, nil)
		if err != nil {
			t.Fatalf("get listed template %q: %v", item.Name, err)
		}
		if got := obj.(*aggregationv1alpha1.CoderTemplate).Name; got != item.Name {
			t.Fatalf("LIST/GET identity violated: listed %q, got %q", item.Name, got)
		}
	}

	for attempt := range 2 {
		_, wasCreated, err := templates.Update(ctx, "acme.new-template", noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil)
		if err != nil {
			t.Fatalf("repeated apply %d: %v", attempt+1, err)
		}
		if wasCreated {
			t.Fatalf("repeated apply %d must not report creation", attempt+1)
		}
	}
}

func TestTemplateStorageOrganizationLookupFailuresKeepMappedErrors(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	templates := NewTemplateStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	if _, err := templates.Get(ctx, "missing-org.starter-template", nil); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for a missing organization, got %v", err)
	}
	if _, err := templates.Get(ctx, "forbidden-org.starter-template", nil); !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden for a forbidden organization, got %v", err)
	}
	if _, err := templates.Create(ctx, newIdentityTemplate("missing-org.new-template", "missing-org"), rest.ValidateAllObjectFunc, nil); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound creating in a missing organization, got %v", err)
	}
	assertNoMutations(t, state)
}

func TestWorkspaceStorageRejectsAliasSegmentsBeforeMutation(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	for _, requested := range []string{"default.me.dev-workspace", "acme.me.dev-workspace", "default.alice.dev-workspace"} {
		_, err := workspaces.Get(ctx, requested, nil)
		assertAliasBadRequest(t, "get "+requested, err, "acme.alice.dev-workspace")

		_, _, err = workspaces.Update(ctx, requested, noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil)
		assertAliasBadRequest(t, "update "+requested, err, "acme.alice.dev-workspace")

		_, _, err = workspaces.Delete(ctx, requested, rest.ValidateAllObjectFunc, nil)
		assertAliasBadRequest(t, "delete "+requested, err, "acme.alice.dev-workspace")
	}

	for _, requested := range []string{"default.me.new-workspace", "acme.me.new-workspace", "default.alice.new-workspace"} {
		organization := strings.SplitN(requested, ".", 2)[0]
		_, err := workspaces.Create(ctx, newIdentityWorkspace(requested, organization), rest.ValidateAllObjectFunc, nil)
		assertAliasBadRequest(t, "create "+requested, err, "acme.alice.new-workspace")
	}

	_, _, err := workspaces.Update(
		ctx,
		"default.me.new-workspace",
		testUpdatedObjectInfo{obj: newIdentityWorkspace("default.me.new-workspace", "default")},
		rest.ValidateAllObjectFunc,
		rest.ValidateAllObjectUpdateFunc,
		true,
		nil,
	)
	assertAliasBadRequest(t, "create-on-update default.me.new-workspace", err, "acme.alice.new-workspace")

	assertNoMutations(t, state)
}

func TestWorkspaceStorageAcceptsLiteralDefaultOrganizationAndMeOwner(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	state.renameOrganization("default")
	state.seedWorkspace("me", "me-workspace", state.organization)

	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	obj, err := workspaces.Get(ctx, "default.me.me-workspace", nil)
	if err != nil {
		t.Fatalf("get with literal canonical names: %v", err)
	}
	if got := obj.(*aggregationv1alpha1.CoderWorkspace).Name; got != "default.me.me-workspace" {
		t.Fatalf("expected metadata.name default.me.me-workspace, got %q", got)
	}

	created, err := workspaces.Create(ctx, newIdentityWorkspace("default.me.new-workspace", "default"), rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("create with literal canonical names: %v", err)
	}
	if got := created.(*aggregationv1alpha1.CoderWorkspace).Name; got != "default.me.new-workspace" {
		t.Fatalf("expected metadata.name default.me.new-workspace, got %q", got)
	}

	if _, _, err := workspaces.Update(ctx, "default.me.new-workspace", noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil); err != nil {
		t.Fatalf("repeated apply with literal canonical names: %v", err)
	}
	if _, _, err := workspaces.Delete(ctx, "default.me.new-workspace", rest.ValidateAllObjectFunc, nil); err != nil {
		t.Fatalf("delete with literal canonical names: %v", err)
	}
}

func TestWorkspaceStorageCrossOrganizationMismatchStaysOpaque(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	state.seedWorkspace("alice", "other-workspace", otherOrganization)

	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	// Disclosures are checked as individually quoted segments: the requested name itself is
	// always echoed back as one quoted string, but canonical segments must never appear.
	cases := []struct {
		requested       string
		mustNotDisclose []string
	}{
		// Genuine mismatch: the workspace exists in "other", the request names "acme".
		{"acme.alice.other-workspace", []string{`"other"`, otherOrganization.ID.String(), "canonical"}},
		// Alias segments plus the wrong organization must not turn into alias guidance.
		{"default.me.other-workspace", []string{`"alice"`, `"other"`, otherOrganization.ID.String(), "canonical"}},
		// The workspace exists in "acme", the request names the existing organization "other".
		{"other.alice.dev-workspace", []string{`"acme"`, "canonical"}},
		{"other.me.dev-workspace", []string{`"alice"`, `"acme"`, "canonical"}},
		// Unknown organizations keep their mapped NotFound.
		{"missing-org.alice.dev-workspace", []string{`"acme"`, "canonical"}},
	}
	for _, tc := range cases {
		_, err := workspaces.Get(ctx, tc.requested, nil)
		assertOpaqueNotFound(t, "get "+tc.requested, err, tc.mustNotDisclose...)

		_, _, err = workspaces.Update(ctx, tc.requested, noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil)
		assertOpaqueNotFound(t, "update "+tc.requested, err, tc.mustNotDisclose...)

		_, _, err = workspaces.Delete(ctx, tc.requested, rest.ValidateAllObjectFunc, nil)
		assertOpaqueNotFound(t, "delete "+tc.requested, err, tc.mustNotDisclose...)
	}

	assertNoMutations(t, state)
}

// TestWorkspaceStorageDeniedOrganizationVerificationStaysOpaque: when the requested organization denies
// the membership lookup (403), an existing workspace in another organization and a missing workspace must
// be indistinguishable (opaque NotFound) across Get/Update/Delete and create-on-update. Direct Create keeps
// the mapped Forbidden of its organization lookup; outages during verification keep their normal mapping.
func TestWorkspaceStorageDeniedOrganizationVerificationStaysOpaque(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	mustNotDisclose := []string{`"acme"`, "canonical", "forbidden organization"}
	for _, requested := range []string{"forbidden-org.alice.dev-workspace", "forbidden-org.alice.missing-workspace"} {
		_, err := workspaces.Get(ctx, requested, nil)
		assertOpaqueNotFound(t, "get "+requested, err, mustNotDisclose...)

		_, _, err = workspaces.Update(ctx, requested, noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil)
		assertOpaqueNotFound(t, "update "+requested, err, mustNotDisclose...)

		_, _, err = workspaces.Delete(ctx, requested, rest.ValidateAllObjectFunc, nil)
		assertOpaqueNotFound(t, "delete "+requested, err, mustNotDisclose...)

		_, _, err = workspaces.Update(
			ctx,
			requested,
			testUpdatedObjectInfo{obj: newIdentityWorkspace(requested, "forbidden-org")},
			rest.ValidateAllObjectFunc,
			rest.ValidateAllObjectUpdateFunc,
			true,
			nil,
		)
		assertOpaqueNotFound(t, "create-on-update "+requested, err, mustNotDisclose...)
	}

	if _, err := workspaces.Create(ctx, newIdentityWorkspace("forbidden-org.alice.new-workspace", "forbidden-org"), rest.ValidateAllObjectFunc, nil); !apierrors.IsForbidden(err) {
		t.Fatalf("direct create must keep the mapped Forbidden of the organization lookup, got %v", err)
	}
	if _, err := workspaces.Get(ctx, "outage-org.alice.dev-workspace", nil); !apierrors.IsInternalError(err) {
		t.Fatalf("an organization lookup outage during membership verification must keep its normal mapping, got %v", err)
	}
	assertNoMutations(t, state)
}

func TestWorkspaceStorageCreateOwnerLookupFailuresKeepMappedErrors(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	if _, err := workspaces.Create(ctx, newIdentityWorkspace("acme.ghost.new-workspace", "acme"), rest.ValidateAllObjectFunc, nil); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for an unknown owner, got %v", err)
	}
	if _, err := workspaces.Create(ctx, newIdentityWorkspace("acme.forbidden-user.new-workspace", "acme"), rest.ValidateAllObjectFunc, nil); !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden for a forbidden owner lookup, got %v", err)
	}
	assertNoMutations(t, state)
}

func TestWorkspaceStorageCanonicalNamesRoundTripWithAliasCapableBackend(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	created, err := workspaces.Create(ctx, newIdentityWorkspace("acme.alice.new-workspace", "acme"), rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("canonical create: %v", err)
	}
	if got := created.(*aggregationv1alpha1.CoderWorkspace).Name; got != "acme.alice.new-workspace" {
		t.Fatalf("expected created metadata.name acme.alice.new-workspace, got %q", got)
	}
	if mutations := state.mutations(); len(mutations) != 1 || !strings.HasSuffix(mutations[0], "/api/v2/users/alice/workspaces") {
		t.Fatalf("expected exactly one canonical owner workspace creation, got %v", mutations)
	}

	listObj, err := workspaces.List(ctx, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	list := listObj.(*aggregationv1alpha1.CoderWorkspaceList)
	if len(list.Items) != 2 {
		t.Fatalf("expected two workspaces, got %d", len(list.Items))
	}
	for _, item := range list.Items {
		obj, err := workspaces.Get(ctx, item.Name, nil)
		if err != nil {
			t.Fatalf("get listed workspace %q: %v", item.Name, err)
		}
		if got := obj.(*aggregationv1alpha1.CoderWorkspace).Name; got != item.Name {
			t.Fatalf("LIST/GET identity violated: listed %q, got %q", item.Name, got)
		}
	}

	for attempt := range 2 {
		_, wasCreated, err := workspaces.Update(ctx, "acme.alice.new-workspace", noopUpdate(), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, nil)
		if err != nil {
			t.Fatalf("repeated apply %d: %v", attempt+1, err)
		}
		if wasCreated {
			t.Fatalf("repeated apply %d must not report creation", attempt+1)
		}
	}
	if mutations := state.mutations(); len(mutations) != 1 {
		t.Fatalf("repeated apply must not mutate the backend, got %v", mutations)
	}
}
