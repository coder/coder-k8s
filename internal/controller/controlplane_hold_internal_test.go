package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// #353: the grace period after the hold grows by one bounded pass per
// dependent of one kind, up to a cap, so dependents that someone keeps
// creating cannot keep a deleted control plane forever.
func TestControlPlaneDependentGraceFor(t *testing.T) {
	t.Parallel()
	require.Equal(t, 30*time.Second, controlPlaneDependentGraceFor(0))
	require.Equal(t, time.Minute, controlPlaneDependentGraceFor(3))
	require.Equal(t, 5*time.Minute, controlPlaneDependentGraceFor(1000), "the grace period has a cap")
}

// #353, #382: the removal of each dependent kind requeues the control plane
// that it references, so a waiting control plane goes at once.
func TestRequestForDependentControlPlane(t *testing.T) {
	t.Parallel()
	meta := metav1.ObjectMeta{Name: "dependent", Namespace: "coder"}
	ref := coderv1alpha1.CoderControlPlaneReference{Name: "coder"}
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "coder", Name: "coder"}}}
	for _, obj := range []client.Object{
		&coderv1alpha1.CoderAIGatewayKey{ObjectMeta: meta, Spec: coderv1alpha1.CoderAIGatewayKeySpec{ControlPlaneRef: ref}},
		&coderv1alpha1.CoderAIProvider{ObjectMeta: meta, Spec: coderv1alpha1.CoderAIProviderSpec{ControlPlaneRef: ref}},
		&coderv1alpha1.CoderMCPServer{ObjectMeta: meta, Spec: coderv1alpha1.CoderMCPServerSpec{ControlPlaneRef: ref}},
	} {
		require.Equal(t, want, requestForDependentControlPlane(context.Background(), obj), "%T", obj)
	}
	require.Nil(t, requestForDependentControlPlane(context.Background(), &coderv1alpha1.CoderTemplateTest{ObjectMeta: meta}), "not a dependent kind")
}
