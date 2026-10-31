package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// readyUnclaimed counts the pool's unclaimed ready members that exist: the
// workspaces a claim can take right now.
func (e *poolMemberEnv) readyUnclaimed(t *testing.T) int {
	t.Helper()
	n := 0
	for _, m := range e.poolMembers(t, e.pool.Name) {
		if m.Spec.ClaimRef == nil && m.DeletionTimestamp.IsZero() && meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady) {
			n++
		}
	}
	return n
}

// TestPoolRefreshesOldUnclaimedMembers: with maxUnclaimedAgeSeconds, an
// unclaimed member that has been ready that long is outdated. The pool builds
// its replacement first and deletes it only once a current member is ready,
// so the claimable members never drop below replicas. Without the field,
// nothing ages (plan 6.6 item 1).
func TestPoolRefreshesOldUnclaimedMembers(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.MaxInFlight = new(int32(2)), new(int32(2))
	})
	now := e.clock.Now()
	oldest := e.createMember(t, nil)
	e.setMemberStatus(t, oldest, withWorkspace(e.readyAt(now.Add(-3*time.Hour))))
	older := e.createMember(t, nil)
	e.setMemberStatus(t, older, withWorkspace(e.readyAt(now.Add(-2*time.Hour))))
	requirePool(t, e.reconcilePool(t, 1), "Ready", true, false)
	require.Len(t, e.poolMembers(t, e.pool.Name), 2, "without maxUnclaimedAgeSeconds nothing ages")
	require.Zero(t, e.lastStep.RequeueAfter)

	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.MaxUnclaimedAgeSeconds = new(int32(3600)) })
	requirePool(t, e.reconcilePool(t, 1), "WaitingForMembers", false, false)
	var fresh []types.NamespacedName
	for _, m := range e.poolMembers(t, e.pool.Name) {
		if key := memberKey(m); key != oldest && key != older {
			fresh = append(fresh, key)
		}
	}
	require.Len(t, fresh, 2, "two replacements, before any old member goes")
	require.Equal(t, 2, e.readyUnclaimed(t), "the old members stay claimable")

	e.setMemberStatus(t, fresh[0], withWorkspace(e.readyAt(now)))
	e.reconcilePool(t, 1)
	require.True(t, e.gone(t, oldest), "one ready replacement retires the oldest member")
	require.False(t, e.gone(t, older))
	require.Equal(t, 2, e.readyUnclaimed(t))
	e.setMemberStatus(t, fresh[1], withWorkspace(e.readyAt(now)))
	e.reconcilePool(t, 1)
	require.True(t, e.gone(t, older))
	require.Equal(t, 2, e.readyUnclaimed(t))
	requirePool(t, e.reconcilePool(t, 1), "Ready", true, false)
	require.Equal(t, time.Hour, e.lastStep.RequeueAfter, "the pool wakes when the next member gets too old")

	e.clock.SetTime(now.Add(time.Hour))
	e.reconcilePool(t, 1)
	require.Len(t, e.poolMembers(t, e.pool.Name), 4, "at the age limit the pool builds replacements again")
	require.Equal(t, 2, e.readyUnclaimed(t))
}

// holder returns the member that holds a claim and its workspace ID.
func (e *poolMemberEnv) holder(t *testing.T, claimKey types.NamespacedName) (types.NamespacedName, uuid.UUID) {
	t.Helper()
	key := types.NamespacedName{Namespace: e.ns, Name: e.claim(t, claimKey).Status.MemberName}
	return key, uuid.MustParse(e.reconcileMember(t, key, 0).Status.WorkspaceID)
}

func requireIdleLost(t *testing.T, claim *coderv1alpha1.CoderWorkspaceClaim) {
	t.Helper()
	require.Equal(t, coderv1alpha1.CoderWorkspaceClaimPhaseLost, claim.Status.Phase, claim.Status.Message)
	require.Equal(t, "IdleTimeout", claim.Status.Reason)
	require.True(t, meta.IsStatusConditionTrue(claim.Status.Conditions, coderv1alpha1.CoderWorkspaceClaimConditionStalled))
	require.False(t, meta.IsStatusConditionTrue(claim.Status.Conditions, coderv1alpha1.CoderWorkspaceClaimConditionReady))
}

// TestPoolReapsIdleClaims: with claims.idleTimeoutSeconds, the pool deletes a
// bound member once both its bind and its workspace's last_used_at are older
// than the timeout, and the claim becomes Lost with IdleTimeout. A use or a
// bind within the timeout keeps the workspace, nothing is reaped without the
// field, and an idle unclaimed member never counts (plan 6.6, decision D9).
func TestPoolReapsIdleClaims(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	t0 := e.clock.Now()
	for range 3 {
		e.readyMember(t)
	}
	used, idle := e.createClaim(t, "used"), e.createClaim(t, "idle")
	e.reconcilePool(t, 1)
	var spare types.NamespacedName
	for _, m := range e.poolMembers(t, e.pool.Name) {
		if m.Spec.ClaimRef == nil {
			spare = memberKey(m)
		}
	}
	require.NotEmpty(t, spare.Name, "one member stays unclaimed")

	e.clock.SetTime(t0.Add(2 * time.Hour))
	e.reconcilePool(t, 1)
	requireClaim(t, e.claim(t, used), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	requireClaim(t, e.claim(t, idle), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)

	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Claims.IdleTimeoutSeconds = new(int32(3600)) })
	_, usedWorkspace := e.holder(t, used)
	e.fake.markUsed(usedWorkspace, e.clock.Now().Add(-10*time.Minute))
	idleMember, _ := e.holder(t, idle)
	e.reconcilePool(t, 1)
	requireClaim(t, e.claim(t, used), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	require.Equal(t, 50*time.Minute, e.lastStep.RequeueAfter, "the pool wakes when the used workspace can be idle")
	requireIdleLost(t, e.claim(t, idle))
	require.False(t, e.reconcileMember(t, idleMember, 0).DeletionTimestamp.IsZero(), "the idle claim's member is deleted")
	require.True(t, e.reconcileMember(t, spare, 0).DeletionTimestamp.IsZero(), "an idle unclaimed member stays")

	m := e.reconcileMember(t, idleMember, 1)
	e.fake.setBuildJob(uuid.MustParse(m.Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	require.Nil(t, e.reconcileMember(t, idleMember, 1), "the member deletes its workspace, then goes")
	e.reconcilePool(t, 1)
	requireIdleLost(t, e.claim(t, idle))

	late := e.createClaim(t, "late")
	e.reconcilePool(t, 2)
	e.requireBound(t, spare, late)
	requireClaim(t, e.claim(t, late), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)

	e.clock.SetTime(t0.Add(3 * time.Hour))
	e.reconcilePool(t, 1)
	requireIdleLost(t, e.claim(t, used))
	requireIdleLost(t, e.claim(t, late))
}

// failMemberDelete refuses member deletes, as a crash right after the claim
// status write would end the reconcile.
type failMemberDelete struct{ client.Client }

func (c failMemberDelete) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*coderv1alpha1.CoderWorkspacePoolMember); ok {
		return errors.New("crash before the member delete")
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// TestPoolIdleReapAfterCrash: the claim becomes Lost before its member is
// deleted. After a crash between the two, the next reconcile deletes the
// member, even once the timeout is gone from the spec: Lost is final.
func TestPoolIdleReapAfterCrash(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.Claims.IdleTimeoutSeconds = new(int32(0)), new(int32(600))
	})
	member, _ := e.readyMember(t)
	claimKey := e.createClaim(t, "idle")
	e.reconcilePool(t, 1)
	e.requireBound(t, member, claimKey)

	e.clock.SetTime(e.clock.Now().Add(11 * time.Minute))
	r := &controller.CoderWorkspacePoolReconciler{Client: failMemberDelete{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.ErrorContains(t, err, "crash before the member delete")
	requireIdleLost(t, e.claim(t, claimKey))
	require.True(t, e.reconcileMember(t, member, 0).DeletionTimestamp.IsZero())

	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Claims.IdleTimeoutSeconds = nil })
	e.reconcilePool(t, 1)
	require.False(t, e.reconcileMember(t, member, 0).DeletionTimestamp.IsZero(), "the member of a Lost IdleTimeout claim is deleted")
	requireIdleLost(t, e.claim(t, claimKey))
}
