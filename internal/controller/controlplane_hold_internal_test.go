package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// coderDeployment is the Coder Deployment of controlPlane. A control plane
// holds only while it exists (#353).
func coderDeployment(controlPlane *coderv1alpha1.CoderControlPlane) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: controlPlane.Name, Namespace: controlPlane.Namespace}}
}

// #353: the grace period after the hold grows by one bounded pass per
// dependent of one kind, up to a cap, so dependents that someone keeps
// creating cannot keep a deleted control plane forever.
func TestControlPlaneDependentGraceFor(t *testing.T) {
	t.Parallel()
	require.Equal(t, 30*time.Second, controlPlaneDependentGraceFor(0))
	require.Equal(t, time.Minute, controlPlaneDependentGraceFor(3))
	require.Equal(t, 5*time.Minute, controlPlaneDependentGraceFor(1000), "the grace period has a cap")
}

// #384 follow-up 5: the grace period counts the dependents of the largest
// kind, whichever kind that is. A dependent of another control plane does not
// count.
func TestControlPlaneDependentsPerKind(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))
	deleted := metav1.NewTime(time.Now())
	controlPlane := &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns", DeletionTimestamp: &deleted}}
	kinds := []func(name, controlPlane string) client.Object{
		func(name, controlPlane string) client.Object {
			return &coderv1alpha1.CoderAIGatewayKey{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Spec: coderv1alpha1.CoderAIGatewayKeySpec{ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: controlPlane}}}
		},
		func(name, controlPlane string) client.Object {
			return &coderv1alpha1.CoderAIProvider{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Spec: coderv1alpha1.CoderAIProviderSpec{ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: controlPlane}}}
		},
		func(name, controlPlane string) client.Object {
			return &coderv1alpha1.CoderMCPServer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Spec: coderv1alpha1.CoderMCPServerSpec{ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: controlPlane}}}
		},
	}
	for name, counts := range map[string][3]int{"keys": {3, 1, 2}, "providers": {1, 3, 2}, "MCP servers": {1, 2, 3}} {
		t.Run(name+" largest", func(t *testing.T) {
			t.Parallel()
			var objects []client.Object
			for kind, count := range counts {
				for i := range count {
					objects = append(objects, kinds[kind](fmt.Sprintf("d%d", i), "coder"))
				}
				objects = append(objects, kinds[kind]("other", "another-control-plane"))
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			dependents, perKind, err := controlPlaneDependents(context.Background(), reader, controlPlane, deleted.Time)
			require.NoError(t, err)
			require.Len(t, dependents, 6)
			require.Equal(t, 3, perKind)
		})
	}
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
