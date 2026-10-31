package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

const poolOwnerID = "5f0c3c4e-0000-4000-8000-000000000001"

// spec.workspacePools is absent by default, defaults maxWorkspaces, and
// round-trips through the merge patches that kubectl and GitOps tools send.
func TestControlPlaneWorkspacePoolsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, "ccp-pools")},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "img:v1"},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	require.Nil(t, cp.Spec.WorkspacePools, "workspacePools must stay absent unless set")

	owner := func(maxWorkspaces int32, templates ...string) coderv1alpha1.WorkspacePoolOwner {
		return coderv1alpha1.WorkspacePoolOwner{UserID: poolOwnerID, Templates: templates, MaxWorkspaces: ptrTo(maxWorkspaces)}
	}
	for _, step := range []struct {
		name  string
		patch string
		want  *coderv1alpha1.WorkspacePoolsSpec
	}{
		{name: "empty block", patch: `{"spec":{"workspacePools":{}}}`, want: &coderv1alpha1.WorkspacePoolsSpec{}},
		{
			name:  "allow an owner",
			patch: `{"spec":{"workspacePools":{"owners":[{"userID":"` + poolOwnerID + `","templates":["default.ci-runner"]}]}}}`,
			want:  &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{owner(10, "default.ci-runner")}},
		},
		{
			name:  "raise the cap",
			patch: `{"spec":{"workspacePools":{"owners":[{"userID":"` + poolOwnerID + `","templates":["default.ci-runner"],"maxWorkspaces":100}]}}}`,
			want:  &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{owner(100, "default.ci-runner")}},
		},
		// Structural pruning drops unknown fields, so a misspelled field
		// never widens the allowlist.
		{
			name:  "misspelled field",
			patch: `{"spec":{"workspacePools":{"owners":[{"userID":"` + poolOwnerID + `","templates":["default.ci-runner"],"maxWorkspaces":100,"template":"default.other"}]}}}`,
			want:  &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{owner(100, "default.ci-runner")}},
		},
		{name: "remove the block", patch: `{"spec":{"workspacePools":null}}`, want: nil},
	} {
		require.NoError(t, k8sClient.Patch(ctx, cp, client.RawPatch(types.MergePatchType, []byte(step.patch))), step.name)
		got := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(cp), got), step.name)
		require.Equal(t, step.want, got.Spec.WorkspacePools, step.name)
	}
}

func TestControlPlaneWorkspacePoolsValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	owners := make([]coderv1alpha1.WorkspacePoolOwner, 17)
	for i := range owners {
		owners[i] = coderv1alpha1.WorkspacePoolOwner{UserID: fmt.Sprintf("5f0c3c4e-0000-4000-8000-%012d", i), Templates: []string{"default.ci-runner"}}
	}
	templates, groups := make([]string, 17), make([]string, 17)
	for i := range templates {
		templates[i], groups[i] = fmt.Sprintf("default.t%d", i), fmt.Sprintf("5f0c3c4e-0000-4000-9000-%012d", i)
	}
	one := func(mutate func(*coderv1alpha1.WorkspacePoolOwner)) *coderv1alpha1.WorkspacePoolsSpec {
		o := coderv1alpha1.WorkspacePoolOwner{UserID: poolOwnerID, Templates: []string{"default.ci-runner"}}
		mutate(&o)
		return &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{o}}
	}
	for _, tc := range []struct {
		name    string
		spec    *coderv1alpha1.WorkspacePoolsSpec
		wantErr string // empty: the API server accepts the object
	}{
		{name: "no owners", spec: &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{}}},
		{name: "16 owners", spec: &coderv1alpha1.WorkspacePoolsSpec{Owners: owners[:16]}},
		{name: "16 templates", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = templates[:16] })},
		{name: "cap 1", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.MaxWorkspaces = ptrTo(int32(1)) })},
		{name: "17 owners", spec: &coderv1alpha1.WorkspacePoolsSpec{Owners: owners}, wantErr: "spec.workspacePools.owners"},
		{name: "duplicate owner", spec: &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{owners[0], owners[0]}}, wantErr: "Duplicate value"},
		{name: "owner not a UUID", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.UserID = "alice" }), wantErr: "spec.workspacePools.owners[0].userID"},
		{name: "prebuilds user", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) {
			o.UserID = "c42fdf75-3097-471c-8c33-fb52454d81c0"
		}), wantErr: "userID must not be the Coder prebuilds system user"},
		// format: uuid alone accepts other spellings of one UUID, and the list
		// map would then hold one Coder user as two owners.
		{name: "owner in upper case", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) {
			o.UserID = strings.ToUpper(poolOwnerID)
		}), wantErr: "spec.workspacePools.owners[0].userID"},
		{name: "owner without hyphens", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) {
			o.UserID = strings.ReplaceAll(poolOwnerID, "-", "")
		}), wantErr: "spec.workspacePools.owners[0].userID"},
		{name: "one owner in two spellings", spec: &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{
			{UserID: poolOwnerID, Templates: []string{"default.ci-runner"}},
			{UserID: strings.ToUpper(poolOwnerID), Templates: []string{"default.other"}},
		}}, wantErr: "spec.workspacePools.owners[1].userID"},
		{name: "no templates list", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = nil }), wantErr: "spec.workspacePools.owners[0].templates"},
		{name: "empty templates list", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = []string{} }), wantErr: "spec.workspacePools.owners[0].templates"},
		{name: "17 templates", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = templates }), wantErr: "spec.workspacePools.owners[0].templates"},
		{name: "duplicate template", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) {
			o.Templates = []string{"default.ci-runner", "default.ci-runner"}
		}), wantErr: "Duplicate value"},
		{name: "template without organization", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = []string{"ci-runner"} }), wantErr: "spec.workspacePools.owners[0].templates[0]"},
		{name: "reserved template name", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = []string{"default.new"} }), wantErr: "must not be new or create"},
		{name: "long organization name", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) {
			o.Templates = []string{strings.Repeat("o", 33) + ".ci-runner"}
		}), wantErr: "must each have at most 32 characters"},
		{name: "cap 0", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.MaxWorkspaces = ptrTo(int32(0)) }), wantErr: "spec.workspacePools.owners[0].maxWorkspaces"},
		{name: "cap 101", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.MaxWorkspaces = ptrTo(int32(101)) }), wantErr: "spec.workspacePools.owners[0].maxWorkspaces"},
		{name: "16 share groups", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.ShareGroupIDs = groups[:16] })},
		{name: "17 share groups", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) { o.ShareGroupIDs = groups }), wantErr: "spec.workspacePools.owners[0].shareGroupIDs"},
		{name: "share group in upper case", spec: one(func(o *coderv1alpha1.WorkspacePoolOwner) {
			o.ShareGroupIDs = []string{strings.ToUpper(groups[0])}
		}), wantErr: "spec.workspacePools.owners[0].shareGroupIDs[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cp := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, "ccp-pools-val")},
				Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "img:v1", WorkspacePools: tc.spec},
			}
			err := k8sClient.Create(ctx, cp)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// The field has no effect until the CoderWorkspacePool controller ships:
// setting it must not roll the Coder Deployment.
func TestControlPlaneWorkspacePoolsLeavesDeploymentUnchanged(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "ccp-pools-inert")
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: namespace},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "img:v1"},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	key := types.NamespacedName{Name: cp.Name, Namespace: namespace}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	before := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, key, before))

	require.NoError(t, k8sClient.Get(ctx, key, cp))
	cp.Spec.WorkspacePools = &coderv1alpha1.WorkspacePoolsSpec{Owners: []coderv1alpha1.WorkspacePoolOwner{{
		UserID: poolOwnerID, Templates: []string{"default.ci-runner"}, MaxWorkspaces: ptrTo(int32(20)),
	}}}
	require.NoError(t, k8sClient.Update(ctx, cp))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	after := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, key, after))
	require.True(t, apiequality.Semantic.DeepEqual(before.Spec.Template, after.Spec.Template), "pod template changed")
	require.Equal(t, before.Generation, after.Generation)
}
