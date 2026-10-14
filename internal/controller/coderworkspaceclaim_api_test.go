package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func TestCoderWorkspaceClaimAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "claim-api")
	newClaim := func(name, pool string) *coderv1alpha1.CoderWorkspaceClaim {
		return &coderv1alpha1.CoderWorkspaceClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec:       coderv1alpha1.CoderWorkspaceClaimSpec{PoolName: pool},
		}
	}

	claim := newClaim("build-1234", "ci-runners")
	require.NoError(t, k8sClient.Create(ctx, claim))

	t.Run("spec is immutable", func(t *testing.T) {
		changed := claim.DeepCopy()
		changed.Spec.PoolName = "other"
		requireInvalid(t, k8sClient.Update(ctx, changed), "spec is immutable")
	})
	t.Run("unknown phase", func(t *testing.T) {
		changed := claim.DeepCopy()
		changed.Status.Phase = "Ready"
		requireInvalid(t, k8sClient.Status().Update(ctx, changed), "status.phase")
	})
	t.Run("no pool name", func(t *testing.T) {
		requireInvalid(t, k8sClient.Create(ctx, newClaim("no-pool", "")), "spec.poolName")
	})
	t.Run("pool name not a DNS name", func(t *testing.T) {
		requireInvalid(t, k8sClient.Create(ctx, newClaim("bad-pool", "CI_Runners")), "spec.poolName")
	})
	t.Run("no spec", func(t *testing.T) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(coderv1alpha1.GroupVersion.WithKind("CoderWorkspaceClaim"))
		obj.SetNamespace(namespace)
		obj.SetName("no-spec")
		requireInvalid(t, k8sClient.Create(ctx, obj), "spec: Required value")
	})
}
