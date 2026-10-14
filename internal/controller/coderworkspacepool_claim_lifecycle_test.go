package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// TestPoolCreatesOnDemand: a pool at replicas 0 creates a member for each
// waiting claim (decision D4: want is replicas plus the waiting claims),
// binds it once it is ready, and creates no more after that.
func TestPoolCreatesOnDemand(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	e.reconcilePool(t, 1)
	require.Empty(t, e.poolMembers(t, e.pool.Name), "no claim, no member")

	claimKey := e.createClaim(t, "build-1")
	e.reconcilePool(t, 2)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 1, "one member for the one waiting claim")
	requireClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhasePending, "WaitingForReadyWorkspace", false)

	e.setMemberStatus(t, memberKey(members[0]), readyWithWorkspace("kwp-on-demand"))
	e.reconcilePool(t, 2)
	e.requireBound(t, memberKey(members[0]), claimKey)
	requireClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	require.Len(t, e.poolMembers(t, e.pool.Name), 1, "no member after the claim is bound")
}

// TestPoolDeleteKeepsBoundClaims: the claim controls a bound member, so
// deleting the pool does not reach it, and its claim stays Bound. A claim
// that no member holds waits with PoolNotFound, as does a claim on a pool
// that never existed.
func TestPoolDeleteKeepsBoundClaims(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	member := e.createMember(t, nil)
	e.setMemberStatus(t, member, readyWithWorkspace("kwp-kept"))
	bound := e.createClaim(t, "bound")
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has second precision.
	waiting := e.createClaim(t, "waiting")
	e.reconcilePool(t, 1)
	e.requireBound(t, member, bound)

	require.NoError(t, k8sClient.Delete(e.ctx, e.reconcilePool(t, 0))) // No finalizer: gone at once.
	r := &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.NoError(t, err)
	e.requireBound(t, member, bound)
	claim := e.claim(t, bound)
	requireClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	require.Equal(t, "kwp-kept", claim.Status.WorkspaceName)
	require.Equal(t, "tester", claim.Status.OwnerName, "kept from the bind")
	requireClaim(t, e.claim(t, waiting), coderv1alpha1.CoderWorkspaceClaimPhasePending, "PoolNotFound", false)

	stray := &coderv1alpha1.CoderWorkspaceClaim{}
	stray.Name, stray.Namespace, stray.Spec.PoolName = "stray", e.ns, "no-such-pool"
	require.NoError(t, k8sClient.Create(e.ctx, stray))
	_, err = r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: "no-such-pool"}})
	require.NoError(t, err)
	requireClaim(t, e.claim(t, types.NamespacedName{Namespace: e.ns, Name: "stray"}), coderv1alpha1.CoderWorkspaceClaimPhasePending, "PoolNotFound", false)
}

// TestPoolClaimLostWhenWorkspaceDeleted: after the one extension at bind, a
// bound member keeps reading its workspace every 60 s. Builds by the
// claimant, a stop included, change nothing. A deleted workspace sets
// WorkspaceDeleted=True, and the claim becomes Lost, which is final: it
// never binds again (plan 6.2 step 11 rule 3).
func TestPoolClaimLostWhenWorkspaceDeleted(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	member, workspaceID := e.readyMember(t)
	claimKey := e.createClaim(t, "build-1")
	e.reconcilePool(t, 1)
	e.requireBound(t, member, claimKey)
	m := e.reconcileMember(t, member, 1)
	require.True(t, m.Status.BindExtended)
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter, "a bound member keeps watching")

	stop := e.fake.addExternalBuild(workspaceID, e.tester, codersdk.WorkspaceTransitionStop)
	e.fake.setBuildJob(stop.ID, codersdk.ProvisionerJobSucceeded)
	m = e.reconcileMember(t, member, 1)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, m.Status.Phase, "a stop by the claimant changes nothing")

	e.fake.markDeleted(workspaceID)
	m = e.reconcileMember(t, member, 1)
	requireMemberFailed(t, m, "WorkspaceDeletedExternally", "deleted", metav1.ConditionTrue)
	e.reconcilePool(t, 1)
	claim := e.claim(t, claimKey)
	require.Equal(t, coderv1alpha1.CoderWorkspaceClaimPhaseLost, claim.Status.Phase)
	require.Equal(t, "WorkspaceDeleted", claim.Status.Reason)

	// The failed member goes, and a ready member appears: the Lost claim
	// never binds again.
	e.deleteMember(t, member)
	require.Nil(t, e.reconcileMember(t, member, 2), "the workspace is already deleted")
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(1)) })
	spare := e.createMember(t, nil)
	e.setMemberStatus(t, spare, readyWithWorkspace("kwp-spare"))
	// The cache still shows the claim without its Lost status: the pool
	// reads the claim again without the cache before a bind.
	stale := &controller.CoderWorkspacePoolReconciler{Client: staleClaims{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := stale.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.NoError(t, err)
	require.Nil(t, e.reconcileMember(t, spare, 0).Spec.ClaimRef)
	e.reconcilePool(t, 1)
	require.Nil(t, e.reconcileMember(t, spare, 0).Spec.ClaimRef)
	require.Equal(t, coderv1alpha1.CoderWorkspaceClaimPhaseLost, e.claim(t, claimKey).Status.Phase)
}

// crashAfterBind lets the first member update land, then reports an
// error, as a crash right after the bind would end the reconcile.
type crashAfterBind struct{ client.Client }

func (c crashAfterBind) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.Client.Update(ctx, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*coderv1alpha1.CoderWorkspacePoolMember); ok {
		return errors.New("crash after the bind")
	}
	return nil
}

// TestPoolOrphanClaimKeepsOwnerName: the pool crashes right after a bind,
// before the claim status, and the pool is deleted before the next
// reconcile. The claim still shows Bound with the owner's Coder username,
// which the pool stored in the claim status before the bind.
func TestPoolOrphanClaimKeepsOwnerName(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	member := e.createMember(t, nil)
	e.setMemberStatus(t, member, readyWithWorkspace("kwp-crash"))
	claimKey := e.createClaim(t, "build-1")
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	r := &controller.CoderWorkspacePoolReconciler{Client: crashAfterBind{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.ErrorContains(t, err, "crash after the bind")
	e.requireBound(t, member, claimKey)

	require.NoError(t, k8sClient.Delete(e.ctx, e.reconcilePool(t, 0)))
	r = &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err = r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	claim := e.claim(t, claimKey)
	requireClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	require.Equal(t, "tester", claim.Status.OwnerName)
	require.Equal(t, "kwp-crash", claim.Status.WorkspaceName)
}
