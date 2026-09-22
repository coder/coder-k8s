package storage

// DELETE precondition coverage (#108): metav1.DeleteOptions.Preconditions (UID / resourceVersion) must be
// compared against the fetched object before any Coder mutation, for both CoderTemplate and CoderWorkspace.

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
)

func uidPtr(value string) *types.UID {
	uid := types.UID(value)
	return &uid
}

func stringPtr(value string) *string { return &value }

func preconditions(uid *types.UID, resourceVersion *string) *metav1.DeleteOptions {
	return &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: uid, ResourceVersion: resourceVersion}}
}

// deleteFixture wires one resource kind to the shared matrix: fetch the live UID/RV, delete with options, and
// report whether the backend object is still intact and how many mutations the backend saw.
type deleteFixture struct {
	name          string
	mutationPath  string // suffix of the single expected backend mutation on success
	setup         func(t *testing.T) (state *mockCoderServerState, current metav1.Object, del func(rest.ValidateObjectFunc, *metav1.DeleteOptions) (bool, error), intact func() bool)
	wantDeletedOK bool // value of the `deleted` return on success
}

func deleteFixtures() []deleteFixture {
	return []deleteFixture{
		{
			name: "template", mutationPath: "/api/v2/templates/", wantDeletedOK: true,
			setup: func(t *testing.T) (*mockCoderServerState, metav1.Object, func(rest.ValidateObjectFunc, *metav1.DeleteOptions) (bool, error), func() bool) {
				t.Helper()
				server, state := newAliasResolvingCoderServer(t)
				templates := NewTemplateStorage(newTestClientProvider(t, server.URL))
				ctx := namespacedContext("control-plane")
				obj, err := templates.Get(ctx, "acme.starter-template", nil)
				if err != nil {
					t.Fatalf("fetch live template identity: %v", err)
				}
				del := func(validate rest.ValidateObjectFunc, options *metav1.DeleteOptions) (bool, error) {
					_, deleted, err := templates.Delete(ctx, "acme.starter-template", validate, options)
					return deleted, err
				}
				return state, obj.(metav1.Object), del, func() bool { return state.hasTemplate("acme", "starter-template") }
			},
		},
		{
			name: "workspace", mutationPath: "/builds", wantDeletedOK: false,
			setup: func(t *testing.T) (*mockCoderServerState, metav1.Object, func(rest.ValidateObjectFunc, *metav1.DeleteOptions) (bool, error), func() bool) {
				t.Helper()
				server, state := newAliasResolvingCoderServer(t)
				workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
				ctx := namespacedContext("control-plane")
				obj, err := workspaces.Get(ctx, "acme.alice.dev-workspace", nil)
				if err != nil {
					t.Fatalf("fetch live workspace identity: %v", err)
				}
				del := func(validate rest.ValidateObjectFunc, options *metav1.DeleteOptions) (bool, error) {
					_, deleted, err := workspaces.Delete(ctx, "acme.alice.dev-workspace", validate, options)
					return deleted, err
				}
				return state, obj.(metav1.Object), del, func() bool { return len(state.mutations()) == 0 }
			},
		},
	}
}

func TestDeletePreconditionsMatrix(t *testing.T) {
	t.Parallel()

	const wrongUID = "00000000-0000-0000-0000-00000000dead"
	const staleRV = "1"

	type matrixCase struct {
		name    string
		options func(current metav1.Object) *metav1.DeleteOptions
		succeed bool
		message string // required substring of the Conflict message when !succeed
	}
	cases := []matrixCase{
		{"nil options", func(metav1.Object) *metav1.DeleteOptions { return nil }, true, ""},
		{"nil preconditions", func(metav1.Object) *metav1.DeleteOptions { return &metav1.DeleteOptions{} }, true, ""},
		{"empty preconditions struct", func(metav1.Object) *metav1.DeleteOptions { return preconditions(nil, nil) }, true, ""},
		{"uid match", func(c metav1.Object) *metav1.DeleteOptions { return preconditions(uidPtr(string(c.GetUID())), nil) }, true, ""},
		{"rv match", func(c metav1.Object) *metav1.DeleteOptions {
			return preconditions(nil, stringPtr(c.GetResourceVersion()))
		}, true, ""},
		{"uid and rv match", func(c metav1.Object) *metav1.DeleteOptions {
			return preconditions(uidPtr(string(c.GetUID())), stringPtr(c.GetResourceVersion()))
		}, true, ""},
		{"uid mismatch", func(metav1.Object) *metav1.DeleteOptions { return preconditions(uidPtr(wrongUID), nil) }, false, "UID in precondition"},
		{"rv stale", func(metav1.Object) *metav1.DeleteOptions { return preconditions(nil, stringPtr(staleRV)) }, false, "ResourceVersion in precondition"},
		{"uid match rv stale", func(c metav1.Object) *metav1.DeleteOptions {
			return preconditions(uidPtr(string(c.GetUID())), stringPtr(staleRV))
		}, false, "ResourceVersion in precondition"},
		{"uid mismatch rv match", func(c metav1.Object) *metav1.DeleteOptions {
			return preconditions(uidPtr(wrongUID), stringPtr(c.GetResourceVersion()))
		}, false, "UID in precondition"},
		{"explicit empty uid", func(metav1.Object) *metav1.DeleteOptions { return preconditions(uidPtr(""), nil) }, false, "UID in precondition"},
		{"explicit empty rv", func(metav1.Object) *metav1.DeleteOptions { return preconditions(nil, stringPtr("")) }, false, "ResourceVersion in precondition"},
	}

	for _, fixture := range deleteFixtures() {
		for _, tc := range cases {
			t.Run(fixture.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				state, current, del, intact := fixture.setup(t)
				if current.GetUID() == "" || current.GetResourceVersion() == "" {
					t.Fatalf("fixture must expose a live UID and resourceVersion, got %q / %q", current.GetUID(), current.GetResourceVersion())
				}

				deleted, err := del(rest.ValidateAllObjectFunc, tc.options(current))
				mutations := state.mutations()
				if tc.succeed {
					if err != nil {
						t.Fatalf("expected delete to proceed, got %v", err)
					}
					if deleted != fixture.wantDeletedOK {
						t.Fatalf("expected deleted=%v, got %v", fixture.wantDeletedOK, deleted)
					}
					if len(mutations) != 1 || !strings.Contains(mutations[0], fixture.mutationPath) {
						t.Fatalf("expected exactly one backend mutation on %q, got %v", fixture.mutationPath, mutations)
					}
					return
				}
				if !apierrors.IsConflict(err) {
					t.Fatalf("expected Conflict for failed precondition, got %v", err)
				}
				if !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("Conflict must explain the failed precondition (%q), got %q", tc.message, err.Error())
				}
				if len(mutations) != 0 || !intact() {
					t.Fatalf("failed precondition must not mutate the backend; mutations=%v intact=%v", mutations, intact())
				}
			})
		}
	}
}

func TestDeletePreconditionsAreEvaluatedAfterIdentityChecks(t *testing.T) {
	t.Parallel()

	server, state := newAliasResolvingCoderServer(t)
	state.seedWorkspace("alice", "other-workspace", otherOrganization)
	templates := NewTemplateStorage(newTestClientProvider(t, server.URL))
	workspaces := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	// The real UID of the cross-organization workspace must not be confirmed or denied through preconditions.
	realOther := ""
	for id, workspace := range state.workspacesByID {
		if workspace.Name == "other-workspace" {
			realOther = id.String()
		}
	}
	if realOther == "" {
		t.Fatal("fixture: cross-organization workspace not seeded")
	}
	for _, uid := range []string{realOther, "00000000-0000-0000-0000-00000000dead"} {
		_, _, err := workspaces.Delete(ctx, "acme.alice.other-workspace", rest.ValidateAllObjectFunc, preconditions(uidPtr(uid), nil))
		assertOpaqueNotFound(t, "cross-organization delete with uid "+uid, err, `"other"`, "Precondition", uid)
	}

	// A prior alias rejection wins over any precondition.
	_, _, err := templates.Delete(ctx, "default.starter-template", rest.ValidateAllObjectFunc, preconditions(uidPtr("x"), nil))
	assertAliasBadRequest(t, "alias delete with preconditions", err, "acme.starter-template")

	// A missing object is NotFound before preconditions are considered.
	if _, _, err := templates.Delete(ctx, "acme.missing-template", rest.ValidateAllObjectFunc, preconditions(uidPtr("x"), nil)); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for a missing template, got %v", err)
	}
	if _, _, err := workspaces.Delete(ctx, "acme.alice.missing-workspace", rest.ValidateAllObjectFunc, preconditions(uidPtr("x"), nil)); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for a missing workspace, got %v", err)
	}

	assertNoMutations(t, state)
}

func TestDeletePreconditionsRunBeforeAdmissionAndMutation(t *testing.T) {
	t.Parallel()

	rejected := errors.New("admission rejected")
	reject := func(context.Context, runtime.Object) error { return rejected }

	for _, fixture := range deleteFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			state, current, del, intact := fixture.setup(t)
			_, err := del(reject, preconditions(uidPtr(string(current.GetUID())), stringPtr(current.GetResourceVersion())))
			if !errors.Is(err, rejected) {
				t.Fatalf("matching preconditions must still run admission, got %v", err)
			}
			if mutations := state.mutations(); len(mutations) != 0 || !intact() {
				t.Fatalf("admission rejection must not mutate the backend; mutations=%v", mutations)
			}
		})
	}
}
