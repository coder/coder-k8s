package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// TestPoolIdleReapKeepsLaterUse: the pool records the last_used_at of its
// idle check on the member before the claim becomes Lost. A crash then stops
// the member delete, and the pool is deleted: the claim path still deletes
// the member. The member's cleanup then reads a newer last_used_at, so it
// keeps the workspace and starts no delete build.
func TestPoolIdleReapKeepsLaterUse(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.Claims.IdleTimeoutSeconds = new(int32(0)), new(int32(600))
	})
	member, workspaceID := e.readyMember(t)
	claimKey := e.createClaim(t, "idle")
	e.reconcilePool(t, 1)
	e.requireBound(t, member, claimKey)

	e.clock.SetTime(e.clock.Now().Add(11 * time.Minute))
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	r := &controller.CoderWorkspacePoolReconciler{Client: failMemberDelete{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.ErrorContains(t, err, "crash before the member delete")
	requireIdleLost(t, e.claim(t, claimKey))
	require.NoError(t, k8sClient.Delete(e.ctx, e.reconcilePool(t, 0)))
	r = &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err = r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.False(t, e.reconcileMember(t, member, 0).DeletionTimestamp.IsZero(), "the claim path deletes the member of a gone pool")

	workspaceName := e.reconcileMember(t, member, 0).Status.WorkspaceName
	e.fake.markUsed(workspaceID, e.clock.Now())
	builds := e.fake.requestCount(routeCreateBuild)
	require.Nil(t, e.reconcileMember(t, member, 1), "the member goes")
	require.Equal(t, builds, e.fake.requestCount(routeCreateBuild), "no delete build after a use")
	_, err = e.fake.client(t, 5*time.Second).Workspace(e.ctx, workspaceID)
	require.NoError(t, err, "the workspace stays in Coder")
	claim := e.claim(t, claimKey)
	requireIdleLost(t, claim)
	require.Contains(t, claim.Status.Message, "unless its last check before the delete sees a use")
	require.Contains(t, claim.Status.Message, "stays in Coder as "+workspaceName+", owned by tester", "the claim tells where a kept workspace is")
}

// TestPoolIdleCleanupNeedsLastUsedAt: a reaped member's last Coder read
// before the delete build that has no last_used_at, zero or left out, proves
// no idleness, so the member keeps the workspace.
func TestPoolIdleCleanupNeedsLastUsedAt(t *testing.T) {
	t.Parallel()
	for _, omit := range []bool{false, true} {
		t.Run(map[bool]string{false: "zero", true: "omitted"}[omit], func(t *testing.T) {
			t.Parallel()
			e := newPoolMemberEnv(t)
			e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
				s.Replicas, s.Claims.IdleTimeoutSeconds = new(int32(0)), new(int32(600))
			})
			member, workspaceID := e.readyMember(t)
			claimKey := e.createClaim(t, "idle")
			e.reconcilePool(t, 1)
			e.requireBound(t, member, claimKey)
			e.clock.SetTime(e.clock.Now().Add(11 * time.Minute))
			e.reconcilePool(t, 1)
			requireIdleLost(t, e.claim(t, claimKey))

			// The identity guard reads a full answer, and the delete step's own read lacks last_used_at.
			e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(answer any) any { return answer }})
			e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(answer any) any {
				raw, err := json.Marshal(answer)
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, json.Unmarshal(raw, &body))
				body["last_used_at"] = time.Time{}
				if omit {
					delete(body, "last_used_at")
				}
				return body
			}})
			builds := e.fake.requestCount(routeCreateBuild)
			require.Nil(t, e.reconcileMember(t, member, 1), "the member goes")
			require.Equal(t, builds, e.fake.requestCount(routeCreateBuild), "no delete build without last_used_at")
			_, err := e.fake.client(t, 5*time.Second).Workspace(e.ctx, workspaceID)
			require.NoError(t, err, "the workspace stays in Coder")
		})
	}
}

// failClaimStatus refuses claim status writes, as a crash right after the
// pool recorded its idle check would end the reconcile.
type failClaimStatus struct{ client.Client }

func (c failClaimStatus) Status() client.SubResourceWriter {
	return failClaimStatusWriter{c.Client.Status()}
}

type failClaimStatusWriter struct{ client.SubResourceWriter }

func (w failClaimStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*coderv1alpha1.CoderWorkspaceClaim); ok {
		return errors.New("crash before the claim status")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// TestPoolIdleCheckNeedsEvidence: a workspace read without last_used_at
// proves no idleness, and a recorded idle check whose claim never became Lost
// is no reap: deleting the member then deletes the workspace as usual, even
// after a newer use.
func TestPoolIdleCheckNeedsEvidence(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.Claims.IdleTimeoutSeconds = new(int32(0)), new(int32(600))
	})
	member, workspaceID := e.readyMember(t)
	claimKey := e.createClaim(t, "idle")
	e.reconcilePool(t, 1)
	e.requireBound(t, member, claimKey)

	e.clock.SetTime(e.clock.Now().Add(11 * time.Minute))
	e.fake.markUsed(workspaceID, time.Time{})
	e.reconcilePool(t, 1)
	requireClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)

	e.fake.markUsed(workspaceID, e.clock.Now().Add(-11*time.Minute))
	r := &controller.CoderWorkspacePoolReconciler{Client: failClaimStatus{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.ErrorContains(t, err, "crash before the claim status")
	require.NotEmpty(t, e.reconcileMember(t, member, 0).Annotations, "the idle check is recorded")
	requireClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)

	e.fake.markUsed(workspaceID, e.clock.Now())
	e.reconcilePool(t, 1)
	requireClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	require.True(t, e.reconcileMember(t, member, 0).DeletionTimestamp.IsZero(), "a recorded check without a Lost claim deletes nothing")
	e.deleteMember(t, member) // A person's delete, while the claim is Bound.
	m := e.reconcileMember(t, member, 1)
	requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "Deleting", "started a delete build")
}

// hourlyClock moves an hour forward at every reading.
type hourlyClock struct{ now time.Time }

func (c *hourlyClock) Now() time.Time                  { c.now = c.now.Add(time.Hour); return c.now }
func (c *hourlyClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// TestPoolAgesOnOneClockReading: a member that gets too old between two
// clock readings of one reconcile counts the same in every step, so binding
// it leaves room for one new member.
func TestPoolAgesOnOneClockReading(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.MaxUnclaimedAgeSeconds = new(int32(3600)) })
	start := e.clock.Now()
	member := e.createMember(t, nil)
	// Current at the first reading, start + 1 h, and too old from start + 1.5 h.
	e.setMemberStatus(t, member, withWorkspace(e.readyAt(start.Add(30*time.Minute))))
	claimKey := e.createClaim(t, "c")
	r := &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: &hourlyClock{now: start}}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.NoError(t, err)
	e.requireBound(t, member, claimKey)
	require.Len(t, e.poolMembers(t, e.pool.Name), 2, "one new member for replicas 1")
}

// TestPoolIdleReapSkipsForeignMember: a new pool with the name of a deleted
// one and another owner never reaps a bound member of the old pool, even when
// the member's pool label points at the new pool.
func TestPoolIdleReapSkipsForeignMember(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	member, _ := e.readyMember(t)
	claimKey := e.createClaim(t, "build-1")
	e.reconcilePool(t, 1)
	e.requireBound(t, member, claimKey)

	require.NoError(t, k8sClient.Delete(e.ctx, e.reconcilePool(t, 0)))
	bob := e.fake.addUser("bob", codersdk.LoginTypePassword)
	e.fake.addMember(e.orgID, bob)
	e.allowOwner(t, bob.String(), "default.docker")
	e.pool = &coderv1alpha1.CoderWorkspacePool{
		ObjectMeta: metav1.ObjectMeta{Name: e.pool.Name, Namespace: e.ns},
		Spec: coderv1alpha1.CoderWorkspacePoolSpec{
			ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
			OwnerUserID:     bob.String(), Template: "default.docker",
			Version: coderv1alpha1.CoderWorkspacePoolVersion{Name: "v1"}, Replicas: new(int32(0)),
			Claims: coderv1alpha1.CoderWorkspacePoolClaims{IdleTimeoutSeconds: new(int32(600))},
		},
	}
	require.NoError(t, k8sClient.Create(e.ctx, e.pool))
	m := e.reconcileMember(t, member, 0)
	m.Labels[coderv1alpha1.CoderWorkspacePoolSelectorLabel] = string(e.pool.UID)
	require.NoError(t, k8sClient.Update(e.ctx, m))

	e.clock.SetTime(e.clock.Now().Add(11 * time.Minute))
	e.reconcilePool(t, 2)
	requireClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	require.True(t, e.reconcileMember(t, member, 0).DeletionTimestamp.IsZero())
}
