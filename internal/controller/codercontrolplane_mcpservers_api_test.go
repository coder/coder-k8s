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

// spec.mcpServers is absent by default and round-trips through the merge
// patches that kubectl and GitOps tools send.
func TestControlPlaneMCPServersRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, "ccp-mcp")},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "img:v1"},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	require.Nil(t, cp.Spec.MCPServers, "mcpServers must stay absent unless set")

	for _, step := range []struct {
		name  string
		patch string
		want  *coderv1alpha1.MCPServersSpec
	}{
		{name: "list organizations", patch: `{"spec":{"mcpServers":{"organizations":["coder","Data-Science"]}}}`, want: &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder", "Data-Science"}}},
		{name: "allow force_on", patch: `{"spec":{"mcpServers":{"allowForceOn":true}}}`, want: &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder", "Data-Science"}, AllowForceOn: true}},
		{name: "disallow force_on", patch: `{"spec":{"mcpServers":{"allowForceOn":false}}}`, want: &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder", "Data-Science"}}},
		// Structural pruning drops unknown fields, so a misspelled field
		// never turns force_on on.
		{name: "misspelled field", patch: `{"spec":{"mcpServers":{"allowforceon":true}}}`, want: &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder", "Data-Science"}}},
		{name: "remove the block", patch: `{"spec":{"mcpServers":null}}`, want: nil},
	} {
		require.NoError(t, k8sClient.Patch(ctx, cp, client.RawPatch(types.MergePatchType, []byte(step.patch))), step.name)
		got := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(cp), got), step.name)
		require.Equal(t, step.want, got.Spec.MCPServers, step.name)
	}
}

func TestControlPlaneMCPServersValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("org-%d", i)
	}
	for _, tc := range []struct {
		name    string
		spec    *coderv1alpha1.MCPServersSpec
		wantErr string // empty: the API server accepts the object
	}{
		{name: "64 organizations", spec: &coderv1alpha1.MCPServersSpec{Organizations: tooMany[:64]}},
		{name: "organization New, which Coder allows", spec: &coderv1alpha1.MCPServersSpec{Organizations: []string{"New"}}},
		{name: "no organizations list", spec: &coderv1alpha1.MCPServersSpec{AllowForceOn: true}, wantErr: "spec.mcpServers.organizations"},
		{name: "empty organizations list", spec: &coderv1alpha1.MCPServersSpec{Organizations: []string{}}, wantErr: "spec.mcpServers.organizations"},
		{name: "65 organizations", spec: &coderv1alpha1.MCPServersSpec{Organizations: tooMany}, wantErr: "spec.mcpServers.organizations"},
		{name: "duplicate organization", spec: &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder", "coder"}}, wantErr: "spec.mcpServers.organizations"},
		{name: "organization with an underscore", spec: &coderv1alpha1.MCPServersSpec{Organizations: []string{"data_science"}}, wantErr: "spec.mcpServers.organizations"},
		{name: "organization with 33 characters", spec: &coderv1alpha1.MCPServersSpec{Organizations: []string{strings.Repeat("o", 33)}}, wantErr: "spec.mcpServers.organizations"},
		{name: "organization new", spec: &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder", "new"}}, wantErr: "mcpServers.organizations must not hold new or create"},
		{name: "organization create", spec: &coderv1alpha1.MCPServersSpec{Organizations: []string{"create"}}, wantErr: "mcpServers.organizations must not hold new or create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cp := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, "ccp-mcp-val")},
				Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "img:v1", MCPServers: tc.spec},
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

// The field has no effect until the CoderMCPServer controller ships: setting
// it must not roll the Coder Deployment.
func TestControlPlaneMCPServersLeavesDeploymentUnchanged(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "ccp-mcp-inert")
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
	cp.Spec.MCPServers = &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}, AllowForceOn: true}
	require.NoError(t, k8sClient.Update(ctx, cp))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	after := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, key, after))
	require.True(t, apiequality.Semantic.DeepEqual(before.Spec.Template, after.Spec.Template), "pod template changed")
	require.Equal(t, before.Generation, after.Generation)
}
