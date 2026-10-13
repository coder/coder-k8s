package controller_test

import (
	"context"
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

// spec.aiGateway.allowKeys is absent by default and round-trips through the
// merge patches that kubectl and GitOps tools send.
func TestControlPlaneAIGatewayAllowKeysRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, "ccp-aigw")},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "img:v1"},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	require.Nil(t, cp.Spec.AIGateway, "aiGateway must stay absent unless set")

	for _, step := range []struct {
		name  string
		patch string
		want  *coderv1alpha1.AIGatewaySpec
	}{
		{name: "turn on", patch: `{"spec":{"aiGateway":{"allowKeys":true}}}`, want: &coderv1alpha1.AIGatewaySpec{AllowKeys: true}},
		{name: "turn off", patch: `{"spec":{"aiGateway":{"allowKeys":false}}}`, want: &coderv1alpha1.AIGatewaySpec{}},
		// Structural pruning drops unknown fields, so a misspelled field
		// never turns key minting on.
		{name: "misspelled field", patch: `{"spec":{"aiGateway":{"allowkeys":true}}}`, want: &coderv1alpha1.AIGatewaySpec{}},
		{name: "remove the block", patch: `{"spec":{"aiGateway":null}}`, want: nil},
	} {
		require.NoError(t, k8sClient.Patch(ctx, cp, client.RawPatch(types.MergePatchType, []byte(step.patch))), step.name)
		got := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(cp), got), step.name)
		require.Equal(t, step.want, got.Spec.AIGateway, step.name)
	}
}

// The field has no effect until the CoderAIGatewayKey controller ships:
// turning it on must not roll the Coder Deployment.
func TestControlPlaneAIGatewayAllowKeysLeavesDeploymentUnchanged(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "ccp-aigw-inert")
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
	cp.Spec.AIGateway = &coderv1alpha1.AIGatewaySpec{AllowKeys: true}
	require.NoError(t, k8sClient.Update(ctx, cp))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	after := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, key, after))
	require.True(t, apiequality.Semantic.DeepEqual(before.Spec.Template, after.Spec.Template), "pod template changed")
	require.Equal(t, before.Generation, after.Generation)
}
