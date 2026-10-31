package controller_test

import (
	"context"
	"fmt"
	"strings"
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
	t.Run("share user", func(t *testing.T) {
		withUser := func(name, user string) *coderv1alpha1.CoderWorkspaceClaim {
			c := newClaim(name, "ci-runners")
			c.Spec.Share = &coderv1alpha1.CoderWorkspaceClaimShare{User: user}
			return c
		}
		for i, user := range []string{"bob", "bob-2", "0f8fad5b-d9cb-469f-a165-70867728950e"} {
			require.NoError(t, k8sClient.Create(ctx, withUser(fmt.Sprintf("share-ok-%d", i), user)), user)
		}
		for i, c := range []struct{ user, wantErr string }{
			{"0F8FAD5B-D9CB-469F-A165-70867728950E", "at most 32 characters or a lower-case UUID"},
			{strings.Repeat("b", 33), "at most 32 characters or a lower-case UUID"},
			{"bob_x", "spec.share.user"},
			{"c42fdf75-3097-471c-8c33-fb52454d81c0", "prebuilds system user"},
		} {
			requireInvalid(t, k8sClient.Create(ctx, withUser(fmt.Sprintf("share-bad-%d", i), c.user)), c.wantErr)
		}
		shared := withUser("share-immutable", "bob")
		require.NoError(t, k8sClient.Create(ctx, shared))
		shared.Spec.Share.User = "alice"
		requireInvalid(t, k8sClient.Update(ctx, shared), "spec is immutable")
	})
	t.Run("shared user ID", func(t *testing.T) {
		for _, id := range []string{"c42fdf75-3097-471c-8c33-fb52454d81c0", "0F8FAD5B-D9CB-469F-A165-70867728950E"} {
			changed := claim.DeepCopy()
			changed.Status.SharedUserID = id
			requireInvalid(t, k8sClient.Status().Update(ctx, changed), "status.sharedUserID")
		}
	})
	t.Run("no spec", func(t *testing.T) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(coderv1alpha1.GroupVersion.WithKind("CoderWorkspaceClaim"))
		obj.SetNamespace(namespace)
		obj.SetName("no-spec")
		requireInvalid(t, k8sClient.Create(ctx, obj), "spec: Required value")
	})
}
