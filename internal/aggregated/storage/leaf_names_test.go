package storage

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
)

// leafFixture wires one resource kind to the leaf-name matrix against a backend that resolves
// alternate-cased leaf names to the stored object (as coderd does) while returning the stored name.
type leafFixture struct {
	name       string
	canonical  string // existing object, canonical name
	alias      string // same object requested with alternate casing of the final segment
	mixed      string // genuinely mixed-case canonical name created during the test
	mixedAlias string // lower-cased alias of the mixed-case object
	get        func(ctx context.Context, name string) (runtime.Object, error)
	update     func(ctx context.Context, name string, info rest.UpdatedObjectInfo, forceAllowCreate bool) error
	del        func(ctx context.Context, name string, options *metav1.DeleteOptions) error
	create     func(ctx context.Context, name string) (runtime.Object, error)
	intact     func(state *mockCoderServerState) bool
	state      *mockCoderServerState
}

func leafFixtures(t *testing.T) []leafFixture {
	t.Helper()

	return []leafFixture{
		func() leafFixture {
			server, state := newAliasResolvingCoderServer(t)
			state.setCaseInsensitiveLeafLookups(true)
			templates := NewTemplateStorage(newTestClientProvider(t, server.URL))
			return leafFixture{
				name: "template", canonical: "acme.starter-template", alias: "acme.Starter-Template",
				mixed: "acme.Mixed-Case", mixedAlias: "acme.mixed-case",
				get: func(ctx context.Context, name string) (runtime.Object, error) { return templates.Get(ctx, name, nil) },
				update: func(ctx context.Context, name string, info rest.UpdatedObjectInfo, force bool) error {
					_, _, err := templates.Update(ctx, name, info, rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, force, nil)
					return err
				},
				del: func(ctx context.Context, name string, options *metav1.DeleteOptions) error {
					_, _, err := templates.Delete(ctx, name, rest.ValidateAllObjectFunc, options)
					return err
				},
				create: func(ctx context.Context, name string) (runtime.Object, error) {
					return templates.Create(ctx, newIdentityTemplate(name, "acme"), rest.ValidateAllObjectFunc, nil)
				},
				intact: func(state *mockCoderServerState) bool { return state.hasTemplate("acme", "starter-template") },
				state:  state,
			}
		}(),
		func() leafFixture {
			server, state := newAliasResolvingCoderServer(t)
			state.setCaseInsensitiveLeafLookups(true)
			workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
			return leafFixture{
				name: "workspace", canonical: "acme.alice.dev-workspace", alias: "acme.alice.Dev-Workspace",
				mixed: "acme.alice.Mixed-Case", mixedAlias: "acme.alice.mixed-case",
				get: func(ctx context.Context, name string) (runtime.Object, error) { return workspaces.Get(ctx, name, nil) },
				update: func(ctx context.Context, name string, info rest.UpdatedObjectInfo, force bool) error {
					_, _, err := workspaces.Update(ctx, name, info, rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, force, nil)
					return err
				},
				del: func(ctx context.Context, name string, options *metav1.DeleteOptions) error {
					_, _, err := workspaces.Delete(ctx, name, rest.ValidateAllObjectFunc, options)
					return err
				},
				create: func(ctx context.Context, name string) (runtime.Object, error) {
					return workspaces.Create(ctx, newIdentityWorkspace(name, "acme"), rest.ValidateAllObjectFunc, nil)
				},
				intact: func(state *mockCoderServerState) bool { return state.hasWorkspace("alice", "dev-workspace") },
				state:  state,
			}
		}(),
	}
}

// TestLeafNameAliasesAreRejectedBeforeMutation: an alternate-cased final segment resolves to an
// existing object on the backend, but the aggregated API must reject it with the canonical name
// before returning the object, running admission, or mutating anything. Matching #108 preconditions
// must not turn that rejection into a success.
func TestLeafNameAliasesAreRejectedBeforeMutation(t *testing.T) {
	t.Parallel()

	for _, fixture := range leafFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			ctx := namespacedContext("control-plane")

			current, err := fixture.get(ctx, fixture.canonical)
			if err != nil {
				t.Fatalf("canonical GET must succeed: %v", err)
			}
			meta := current.(metav1.Object)
			if meta.GetName() != fixture.canonical {
				t.Fatalf("canonical GET returned %q", meta.GetName())
			}
			state := fixture.state

			verbs := []struct {
				verb string
				run  func() error
			}{
				{"get", func() error { _, err := fixture.get(ctx, fixture.alias); return err }},
				{"update", func() error { return fixture.update(ctx, fixture.alias, noopUpdate(), false) }},
				{"create-on-update", func() error {
					return fixture.update(ctx, fixture.alias, testUpdatedObjectInfo{obj: newLeafObject(fixture, fixture.alias)}, true)
				}},
				{"delete", func() error { return fixture.del(ctx, fixture.alias, nil) }},
				{"delete-with-matching-preconditions", func() error {
					return fixture.del(ctx, fixture.alias, preconditions(uidPtr(string(meta.GetUID())), stringPtr(meta.GetResourceVersion())))
				}},
			}
			for _, tc := range verbs {
				assertAliasBadRequest(t, tc.verb+" "+fixture.alias, tc.run(), fixture.canonical)
			}
			assertNoMutations(t, state)
			if !fixture.intact(state) {
				t.Fatal("rejected alias requests must leave the backend object intact")
			}
		})
	}
}

// TestLeafNameMixedCaseCanonicalRoundTrips: a genuinely mixed-case canonical name is accepted and
// returned exactly (no lowercase blacklist); only its differently cased alias is rejected. Repeated
// canonical apply stays a no-op.
func TestLeafNameMixedCaseCanonicalRoundTrips(t *testing.T) {
	t.Parallel()

	for _, fixture := range leafFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			ctx := namespacedContext("control-plane")

			if _, err := fixture.create(ctx, fixture.mixed); err != nil {
				t.Fatalf("create mixed-case canonical %q: %v", fixture.mixed, err)
			}
			obj, err := fixture.get(ctx, fixture.mixed)
			if err != nil {
				t.Fatalf("GET mixed-case canonical %q: %v", fixture.mixed, err)
			}
			if got := obj.(metav1.Object).GetName(); got != fixture.mixed {
				t.Fatalf("mixed-case canonical GET returned %q, want %q", got, fixture.mixed)
			}
			if err := fixture.update(ctx, fixture.mixed, noopUpdate(), false); err != nil {
				t.Fatalf("repeated canonical apply of %q must succeed: %v", fixture.mixed, err)
			}
			if err := fixture.update(ctx, fixture.mixed, noopUpdate(), false); err != nil {
				t.Fatalf("second repeated canonical apply of %q must succeed: %v", fixture.mixed, err)
			}

			_, err = fixture.get(ctx, fixture.mixedAlias)
			assertAliasBadRequest(t, "get "+fixture.mixedAlias, err, fixture.mixed)
			assertAliasBadRequest(t, "delete "+fixture.mixedAlias, fixture.del(ctx, fixture.mixedAlias, nil), fixture.mixed)
		})
	}
}

// TestLeafNameCrossOrganizationStaysOpaque: an alternate-cased leaf for a workspace in another
// organization keeps the opaque NotFound of the membership check; the leaf check never runs first.
func TestLeafNameCrossOrganizationStaysOpaque(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	state.setCaseInsensitiveLeafLookups(true)
	state.seedWorkspace("alice", "other-workspace", otherOrganization)
	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	_, err := workspaces.Get(ctx, "acme.alice.Other-Workspace", nil)
	assertOpaqueNotFound(t, "get acme.alice.Other-Workspace", err, "other-workspace", otherOrganization.Name)
	_, _, err = workspaces.Delete(ctx, "acme.alice.Other-Workspace", rest.ValidateAllObjectFunc, nil)
	assertOpaqueNotFound(t, "delete acme.alice.Other-Workspace", err, "other-workspace", otherOrganization.Name)
	assertNoMutations(t, state)
}

func newLeafObject(fixture leafFixture, name string) runtime.Object {
	if fixture.name == "template" {
		return newIdentityTemplate(name, "acme")
	}
	return newIdentityWorkspace(name, "acme")
}
