package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// staleMemberListClient answers every member list from an empty cache, as a
// cache does before it sees new members.
type staleMemberListClient struct{ client.Client }

func (c staleMemberListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*coderv1alpha1.CoderWorkspacePoolMemberList); ok {
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

// poolMembers lists the members of the pool name in the test namespace.
func (e *poolMemberEnv) poolMembers(t *testing.T, name string) []coderv1alpha1.CoderWorkspacePoolMember {
	t.Helper()
	pool := &coderv1alpha1.CoderWorkspacePool{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: name}, pool))
	list := &coderv1alpha1.CoderWorkspacePoolMemberList{}
	require.NoError(t, k8sClient.List(e.ctx, list, client.InNamespace(e.ns), client.MatchingLabels{coderv1alpha1.CoderWorkspacePoolSelectorLabel: string(pool.UID)}))
	return list.Items
}

func memberKey(m coderv1alpha1.CoderWorkspacePoolMember) types.NamespacedName {
	return types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
}

// TestPoolCreatesMembers: the pool creates members that pin its resolved
// inputs, and a stale cache never makes it create too many (plan mutation 6).
func TestPoolCreatesMembers(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.ReadyTimeoutSeconds = new(int32(2)), new(int32(600))
		s.Parameters = []coderv1alpha1.CoderWorkspacePoolParameter{{Name: "region", Value: "eu"}}
	})
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	stale := &controller.CoderWorkspacePoolReconciler{Client: staleMemberListClient{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	for range 3 {
		_, err := stale.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 2, "the uncached list stops creation at spec.replicas")
	pool := e.reconcilePool(t, 0)
	r := pool.Status.Resolved
	for _, m := range members {
		require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberSpec{
			ControlPlaneRef: pool.Spec.ControlPlaneRef, PoolName: pool.Name, OwnerID: r.OwnerID, OrganizationID: r.OrganizationID,
			TemplateID: r.TemplateID, TemplateVersionID: r.TemplateVersionID,
			Parameters:     []coderv1alpha1.CoderWorkspacePoolMemberParameter{{Name: "region", Value: "eu"}},
			ParametersHash: parametersHash(coderv1alpha1.CoderWorkspacePoolMemberParameter{Name: "region", Value: "eu"}), ReadyTimeoutSeconds: 600,
		}, m.Spec)
		owner := metav1.GetControllerOf(&m)
		require.NotNil(t, owner)
		require.Equal(t, pool.UID, owner.UID)
	}
	requirePool(t, pool, "WaitingForMembers", false, false)
	require.Equal(t, int32(2), pool.Status.Replicas)
	require.Equal(t, int32(2), pool.Status.UpdatedReplicas)

	// The member accepts what the pool created: it passes the input check.
	member := e.reconcileMember(t, memberKey(members[0]), 3)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, member.Status.Phase, member.Status.Message)
}

// TestPoolCreationCaps: maxInFlight bounds the members that build at once,
// and maxWorkspaces bounds the owner's members on the control plane across
// pools, from an uncached count.
func TestPoolCreationCaps(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas, s.MaxInFlight = new(int32(3)), new(int32(2)) })
	members := func() []coderv1alpha1.CoderWorkspacePoolMember { return e.poolMembers(t, e.pool.Name) }
	e.reconcilePool(t, 2)
	require.Len(t, members(), 2)
	e.setMemberStatus(t, memberKey(members()[0]), readyStatus)
	e.reconcilePool(t, 2)
	require.Len(t, members(), 3, "a ready member frees a build slot")

	// Another pool of the same owner gets only what the owner cap leaves.
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Spec.WorkspacePools.Owners[0].MaxWorkspaces = new(int32(4))
	require.NoError(t, k8sClient.Update(e.ctx, cp))
	other := &coderv1alpha1.CoderWorkspacePool{
		ObjectMeta: metav1.ObjectMeta{Name: "builders", Namespace: e.ns},
		Spec:       *e.pool.Spec.DeepCopy(),
	}
	require.NoError(t, k8sClient.Create(e.ctx, other))
	otherKey := types.NamespacedName{Namespace: e.ns, Name: "builders"}
	stale := &controller.CoderWorkspacePoolReconciler{Client: staleMemberListClient{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	for range 2 {
		_, err := stale.Reconcile(e.ctx, ctrl.Request{NamespacedName: otherKey})
		require.NoError(t, err)
	}
	require.Len(t, e.poolMembers(t, "builders"), 1, "3 + 1 = maxWorkspaces")
	require.NoError(t, k8sClient.Get(e.ctx, otherKey, other))
	requirePool(t, other, "OwnerWorkspaceLimit", false, false)

	// A member whose workspace is deleted no longer counts.
	e.setMemberStatus(t, memberKey(members()[1]), memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed,
		memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, metav1.ConditionTrue, "Deleted")))
	_, err := stale.Reconcile(e.ctx, ctrl.Request{NamespacedName: otherKey})
	require.NoError(t, err)
	require.Len(t, e.poolMembers(t, "builders"), 2)
}

// TestPoolFailureBackoffAndLimit: the pool counts each failed member once,
// deletes it, backs off, and stops after 5 failures in a row until its spec
// changes. A member that becomes ready resets the count.
func TestPoolFailureBackoffAndLimit(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	failNewest := func(t *testing.T) string {
		t.Helper()
		members := e.poolMembers(t, e.pool.Name)
		require.Len(t, members, 1)
		e.setMemberStatus(t, memberKey(members[0]), func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
			memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed)(s)
			s.Reason, s.Message = "BuildFailed", "The start build failed."
		})
		return members[0].Name
	}
	e.reconcilePool(t, 1)
	for n := int32(1); n <= 5; n++ {
		name := failNewest(t)
		pool := e.reconcilePool(t, 2) // Counted once, though read twice.
		require.Equal(t, n, pool.Status.ConsecutiveFailures)
		require.Equal(t, name, pool.Status.LastFailure.Member)
		require.Equal(t, "BuildFailed", pool.Status.LastFailure.Reason)
		require.Empty(t, e.poolMembers(t, e.pool.Name), "the failed member is deleted")
		if n == 5 {
			requirePool(t, pool, "MemberFailureLimit", false, true)
			require.Zero(t, e.lastStep.RequeueAfter)
			break
		}
		backoff := 30 * time.Second << (n - 1)
		requirePool(t, pool, "FailureBackoff", false, false)
		require.Equal(t, backoff, e.lastStep.RequeueAfter)
		e.clock.SetTime(e.clock.Now().Add(backoff - time.Second))
		e.reconcilePool(t, 1)
		require.Empty(t, e.poolMembers(t, e.pool.Name))
		e.clock.SetTime(e.clock.Now().Add(time.Second))
		e.reconcilePool(t, 1)
	}
	e.clock.SetTime(e.clock.Now().Add(time.Hour))
	e.reconcilePool(t, 1)
	require.Empty(t, e.poolMembers(t, e.pool.Name), "the limit holds")

	// A spec change lifts the limit, and a ready member resets the count.
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.MaxInFlight = new(int32(4)) })
	pool := e.reconcilePool(t, 1)
	require.Zero(t, pool.Status.ConsecutiveFailures)
	failNewest(t)
	require.Equal(t, int32(1), e.reconcilePool(t, 1).Status.ConsecutiveFailures)
	e.clock.SetTime(e.clock.Now().Add(30 * time.Second))
	e.reconcilePool(t, 1)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 1)
	e.clock.SetTime(e.clock.Now().Add(time.Minute))
	e.setMemberStatus(t, memberKey(members[0]), func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
		readyStatus(s)
		s.ReadyTime = &metav1.Time{Time: e.clock.Now()}
	})
	pool = e.reconcilePool(t, 1)
	require.Zero(t, pool.Status.ConsecutiveFailures)
	requirePool(t, pool, "Ready", true, false)
	require.True(t, meta.IsStatusConditionTrue(pool.Status.Conditions, coderv1alpha1.CoderWorkspacePoolConditionReady))
}

// TestPoolCountsDeletingFailedMemberOnce: a failed member whose finalizer
// keeps it after the delete is counted once.
func TestPoolCountsDeletingFailedMemberOnce(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Finalizers = []string{"test.coder.com/hold"} })
	e.setMemberStatus(t, key, memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed))
	t.Cleanup(func() {
		m := e.reconcileMember(t, key, 0)
		m.Finalizers = nil
		_ = k8sClient.Update(e.ctx, m)
	})
	pool := e.reconcilePool(t, 2)
	require.Equal(t, int32(1), pool.Status.ConsecutiveFailures, "a member that stays deleting is counted once")
	require.False(t, e.reconcileMember(t, key, 0).DeletionTimestamp.IsZero())
}
