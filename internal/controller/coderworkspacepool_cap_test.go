package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// setOwnerCap sets maxWorkspaces of the control plane's only owner entry.
func (e *poolMemberEnv) setOwnerCap(t *testing.T, maxWorkspaces int32) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Spec.WorkspacePools.Owners[0].MaxWorkspaces = &maxWorkspaces
	require.NoError(t, k8sClient.Update(e.ctx, cp))
}

// gone reports whether the member no longer exists.
func (e *poolMemberEnv) gone(t *testing.T, key types.NamespacedName) bool {
	t.Helper()
	return e.reconcileMember(t, key, 0) == nil
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

// TestPoolLoweredOwnerCapDeletesOnce: members that the pool deleted above
// the cap count no longer while their finalizers run, so the next reconcile
// deletes no more of them.
func TestPoolLoweredOwnerCapDeletesOnce(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.MaxInFlight = new(int32(3)), new(int32(3))
	})
	e.reconcilePool(t, 2)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 3)
	for _, m := range members {
		e.reconcileMember(t, memberKey(m), 1) // The finalizer only.
	}
	e.setOwnerCap(t, 1)
	e.reconcilePool(t, 2)
	deleting := 0
	for _, m := range e.poolMembers(t, e.pool.Name) {
		if !m.DeletionTimestamp.IsZero() {
			deleting++
		}
	}
	require.Equal(t, 2, deleting, "one member stays at maxWorkspaces 1")
	require.Len(t, e.poolMembers(t, e.pool.Name), 3, "no new member")
}

// firstReadOldCap answers the first control plane read with maxWorkspaces
// 10, as before an admin lowered it, and later reads as stored.
type firstReadOldCap struct {
	client.Reader
	reads *int
}

func (c firstReadOldCap) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if cp, ok := obj.(*coderv1alpha1.CoderControlPlane); ok && cp.Spec.WorkspacePools != nil {
		if *c.reads++; *c.reads == 1 {
			for i := range cp.Spec.WorkspacePools.Owners {
				cp.Spec.WorkspacePools.Owners[i].MaxWorkspaces = new(int32(10))
			}
		}
	}
	return nil
}

// TestPoolMemberRechecksCapLast: the member reads the cap again after its
// Coder reads, right before the marker, so a cap that an admin lowered
// during those reads holds (Codex 4274565367).
func TestPoolMemberRechecksCapLast(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.setOwnerCap(t, 1)
	e.reconcileMember(t, e.createMember(t, nil), 2)
	require.Equal(t, 1, e.fake.workspaceCount())
	second := e.createMember(t, nil)
	r := &controller.CoderWorkspacePoolMemberReconciler{Client: k8sClient, APIReader: firstReadOldCap{k8sClient, new(0)}, Scheme: scheme, Clock: e.clock}
	for range 2 { // The finalizer, then the checks.
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: second})
		require.NoError(t, err)
	}
	require.Equal(t, 1, e.fake.workspaceCount(), "no workspace above maxWorkspaces 1")
	require.Equal(t, "OwnerWorkspaceLimit", e.reconcileMember(t, second, 0).Status.Reason)
}
