package controller_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// setOwnerCap sets maxWorkspaces of the control plane's only owner entry.
func (e *poolMemberEnv) setOwnerCap(t *testing.T, maxWorkspaces int32) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Spec.WorkspacePools.Owners[0].MaxWorkspaces = &maxWorkspaces
	require.NoError(t, k8sClient.Update(e.ctx, cp))
}

// TestPoolLoweredOwnerCap: an admin lowers maxWorkspaces while members of
// the pool wait for their create (Codex 4271920249). No member creates a
// workspace above the new cap: each member checks the cap again, without
// the cache, right before its create marker. The pool then deletes the
// members that sent no create request, down to the cap.
func TestPoolLoweredOwnerCap(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.MaxInFlight = new(int32(3)), new(int32(3))
	})
	e.reconcilePool(t, 2)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 3)
	e.setOwnerCap(t, 1)

	// Every member reconciles before the pool sees the new cap.
	waiting := map[string]bool{}
	for _, m := range members {
		got := e.reconcileMember(t, memberKey(m), 2) // The finalizer, then the create.
		if got.Status.Reason == "OwnerWorkspaceLimit" {
			require.Nil(t, got.Status.CreateAttemptTime, "a waiting member writes no marker")
			waiting[got.Name] = true
		}
	}
	require.Equal(t, 1, e.fake.workspaceCount(), "one workspace at maxWorkspaces 1")
	require.Len(t, waiting, 2)

	// The pool deletes the two members without a create request and keeps
	// the member that created its workspace.
	e.reconcilePool(t, 1)
	for _, m := range e.poolMembers(t, e.pool.Name) {
		require.Equal(t, waiting[m.Name], !m.DeletionTimestamp.IsZero(), "member %s", m.Name)
	}
	requirePool(t, e.reconcilePool(t, 1), "OwnerWorkspaceLimit", false, false)
	require.Len(t, e.poolMembers(t, e.pool.Name), 3, "no new member: the deleting members wait for their finalizers")
	require.Equal(t, 1, e.fake.workspaceCount())
}
