package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// failedStatus fails a member now, as the member reconciler does: phase
// Failed, Ready=False since the build started 10 minutes ago, and a
// WorkspaceDeleted condition set at the test clock's time.
func (e *poolMemberEnv) failedStatus(reason string) func(*coderv1alpha1.CoderWorkspacePoolMemberStatus) {
	return func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
		s.Phase, s.Reason, s.Message = coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed, reason, "The start build failed."
		for _, c := range []metav1.Condition{
			{Type: coderv1alpha1.CoderWorkspacePoolMemberConditionReady, Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(e.clock.Now().Add(-10 * time.Minute))},
			{Type: coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(e.clock.Now())},
		} {
			c.Reason, c.Message = reason, reason
			meta.SetStatusCondition(&s.Conditions, c)
		}
	}
}

func (e *poolMemberEnv) readyAt(at time.Time) func(*coderv1alpha1.CoderWorkspacePoolMemberStatus) {
	return func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
		readyStatus(s)
		s.ReadyTime = &metav1.Time{Time: at}
	}
}

// tick moves the test clock one second, so the next event comes after the
// last one at status precision.
func (e *poolMemberEnv) tick() { e.clock.SetTime(e.clock.Now().Add(time.Second)) }

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
		e.tick()
		e.setMemberStatus(t, memberKey(members[0]), e.failedStatus("BuildFailed"))
		return members[0].Name
	}
	e.reconcilePool(t, 1)
	for n := int32(1); n <= 5; n++ {
		name := failNewest(t)
		pool := e.reconcilePool(t, 2) // Counted once, though read twice.
		require.Equal(t, n, pool.Status.ConsecutiveFailures)
		require.Len(t, pool.Status.RunFailures, int(n))
		require.Equal(t, name, pool.Status.LastFailure.Member)
		require.Equal(t, "BuildFailed", pool.Status.LastFailure.Reason)
		require.Empty(t, e.poolMembers(t, e.pool.Name), "the failed member is deleted")
		if n == 5 {
			requirePool(t, pool, "MemberFailureLimit", false, true)
			require.Equal(t, time.Minute, e.lastStep.RequeueAfter, "a stalled pool still reads Coder for a new version")
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
	e.setMemberStatus(t, memberKey(members[0]), e.readyAt(e.clock.Now()))
	pool = e.reconcilePool(t, 1)
	require.Zero(t, pool.Status.ConsecutiveFailures)
	requirePool(t, pool, "Ready", true, false)
}

// TestPoolFailureRunOrder: failures and successes count in the order they
// happened, not in the order the pool saw them. All failed members are
// classified before any create. A failure in the same second as a success
// does not count, and members that the pool does not control never take
// part.
func TestPoolFailureRunOrder(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	e.reconcilePool(t, 1) // Stores observedGeneration: the run starts now.
	start := e.clock.Now().Add(time.Minute)
	// A failure, a success, and a failure, all before one reconcile.
	e.clock.SetTime(start)
	e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	e.setMemberStatus(t, e.createMember(t, nil), e.readyAt(start.Add(time.Second)))
	e.clock.SetTime(start.Add(2 * time.Second))
	e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	// A ready member that the pool does not control never resets the run.
	foreign := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.OwnerReferences = nil })
	e.setMemberStatus(t, foreign, e.readyAt(start.Add(time.Hour)))
	// Two replicas, so the pool wants a create. Mark the edit as seen, so
	// that only the members decide the run.
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(2)) })
	pool := e.reconcilePool(t, 0)
	pool.Status.ObservedGeneration = pool.Generation
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	e.clock.SetTime(start.Add(10 * time.Second))
	pool = e.reconcilePool(t, 1)
	require.Equal(t, int32(1), pool.Status.ConsecutiveFailures, "only the failure after the success counts")
	require.True(t, pool.Status.LastFailure.Time.Time.Equal(start.Add(2*time.Second)), "the failure time is when the member failed")
	requirePool(t, pool, "FailureBackoff", false, false)
	require.Len(t, e.poolMembers(t, e.pool.Name), 2, "both failed members are deleted, and the backoff stops a create")

	// A member that became ready and was bound before the pool saw it ends
	// the run too: its claim controls it, and the pool label stays.
	e.clock.SetTime(start.Add(2 * time.Minute))
	e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	require.Equal(t, int32(2), e.reconcilePool(t, 1).Status.ConsecutiveFailures)
	boundKey := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.OwnerReferences = []metav1.OwnerReference{{APIVersion: coderv1alpha1.GroupVersion.String(), Kind: "CoderWorkspaceClaim", Name: "build-1", UID: "5f0c3c4e-0000-4000-8000-0000000000c1", Controller: new(true)}}
		m.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1", UID: "5f0c3c4e-0000-4000-8000-0000000000c1"}
	})
	e.setMemberStatus(t, boundKey, e.readyAt(start.Add(2*time.Minute+time.Second)))
	e.clock.SetTime(start.Add(150 * time.Second))
	require.Zero(t, e.reconcilePool(t, 1).Status.ConsecutiveFailures, "the bound member's ready time ended the run")

	// A failure in the same second as a success does not count.
	e.clock.SetTime(start.Add(3 * time.Minute))
	e.setMemberStatus(t, e.createMember(t, nil), e.readyAt(e.clock.Now()))
	e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	pool = e.reconcilePool(t, 1)
	require.Zero(t, pool.Status.ConsecutiveFailures)
}

// TestPoolFailureRunResets: failures from before a spec change or a new
// version never count, also when their members are still there at the
// change, and a failed member with old inputs never counts.
func TestPoolFailureRunResets(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.reconcilePool(t, 1)
	for range 2 {
		e.tick()
		e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	}
	e.tick()
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.MaxInFlight = new(int32(3)) })
	pool := e.reconcilePool(t, 1)
	require.Zero(t, pool.Status.ConsecutiveFailures, "the spec change started a new run")

	e.tick()
	outdated := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.Spec.TemplateVersionID = e.fake.addVersion(e.tplID, "v0", codersdk.ProvisionerJobSucceeded).String()
	})
	e.setMemberStatus(t, outdated, e.failedStatus("BuildFailed"))
	require.Zero(t, e.reconcilePool(t, 1).Status.ConsecutiveFailures)
	require.Nil(t, e.reconcileMember(t, outdated, 0), "deleted without counting")

	// A new version ends the run too. Two failures run first, so that the
	// limit and the backoff stay out of the way.
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Version = coderv1alpha1.CoderWorkspacePoolVersion{Active: new(true)}
	})
	e.tick()
	e.reconcilePool(t, 1) // The spec change starts a new run.
	e.tick()
	e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	require.Equal(t, int32(1), e.reconcilePool(t, 1).Status.ConsecutiveFailures)
	e.tick()
	e.fake.promoteVersion(e.fake.addVersion(e.tplID, "v2", codersdk.ProvisionerJobSucceeded))
	require.Zero(t, e.reconcilePool(t, 1).Status.ConsecutiveFailures, "the new version started a new run")
}

// deleteHookClient runs hook before each member delete and fails the pool
// status writes after a member delete when failStatus is set.
type deleteHookClient struct {
	client.Client
	hook       func(m *coderv1alpha1.CoderWorkspacePoolMember)
	failStatus bool
	deleted    *bool
}

func (c deleteHookClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if m, ok := obj.(*coderv1alpha1.CoderWorkspacePoolMember); ok {
		if c.hook != nil {
			c.hook(m)
		}
		*c.deleted = true
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c deleteHookClient) Status() client.SubResourceWriter {
	return deleteHookStatusWriter{c.Client.Status(), c}
}

type deleteHookStatusWriter struct {
	client.SubResourceWriter
	c deleteHookClient
}

func (w deleteHookStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*coderv1alpha1.CoderWorkspacePool); ok && w.c.failStatus && *w.c.deleted {
		return apierrors.NewConflict(schema.GroupResource{Group: "coder.com", Resource: "coderworkspacepools"}, obj.GetName(), errors.New("stale object"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// TestPoolCountsFailedMemberOnce: the pool stores the run before it deletes
// failed members. A lost status write after the delete, or a crash before
// it, neither loses nor repeats a failure. The delete carries the listed UID
// and resourceVersion, so a member that changed after the list stays.
func TestPoolCountsFailedMemberOnce(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	// observedGeneration is current, so no spec change restarts the run,
	// and no member exists, so each reconcile wants a create.
	pool := e.reconcilePool(t, 0)
	pool.Status.ObservedGeneration = pool.Generation
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	failed := func(t *testing.T) types.NamespacedName {
		t.Helper()
		e.tick()
		member := e.createMember(t, nil)
		e.setMemberStatus(t, member, e.failedStatus("BuildFailed"))
		return member
	}
	reconcileWith := func(t *testing.T, c deleteHookClient) error {
		t.Helper()
		c.Client, c.deleted = k8sClient, new(false)
		r := &controller.CoderWorkspacePoolReconciler{Client: c, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	// The status write after the delete is lost: the failure stays stored.
	member := failed(t)
	require.True(t, apierrors.IsConflict(reconcileWith(t, deleteHookClient{failStatus: true})))
	require.Nil(t, e.reconcileMember(t, member, 0))
	require.Equal(t, int32(1), e.reconcilePool(t, 0).Status.ConsecutiveFailures)

	// A crash after the run is stored, before the delete: the next
	// reconcile deletes the member and does not count it again.
	member = failed(t)
	pool = e.reconcilePool(t, 0)
	pool.Status.RunFailures = append(pool.Status.RunFailures, coderv1alpha1.CoderWorkspacePoolFailure{
		Member: member.Name, Reason: "BuildFailed", Time: &metav1.Time{Time: e.clock.Now()},
	})
	pool.Status.ConsecutiveFailures = 2
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	require.Equal(t, int32(2), e.reconcilePool(t, 1).Status.ConsecutiveFailures)
	require.Nil(t, e.reconcileMember(t, member, 0))

	// The member changes between the list and the delete: it stays.
	member = failed(t)
	err := reconcileWith(t, deleteHookClient{hook: func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		fresh := e.reconcileMember(t, memberKey(*m), 0)
		fresh.Annotations = map[string]string{"test.coder.com/touched": "true"}
		require.NoError(t, k8sClient.Update(e.ctx, fresh))
	}})
	require.True(t, apierrors.IsConflict(err), "the resourceVersion precondition refuses the delete: %v", err)
	require.NotNil(t, e.reconcileMember(t, member, 0))
}

// TestPoolPublishGateAfterFailureWrite: the failure write in a reconcile
// that also publishes new resolved inputs does not open the gate.
func TestPoolPublishGateAfterFailureWrite(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.reconcilePool(t, 1)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 1)
	e.tick()
	e.setMemberStatus(t, memberKey(members[0]), e.failedStatus("BuildFailed"))
	e.clock.SetTime(e.clock.Now().Add(time.Hour)) // The backoff is over: only the gate stops a create.
	pool := e.reconcilePool(t, 0)
	pool.Status.Resolved = nil
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	pool = e.reconcilePool(t, 1)
	require.Equal(t, int32(1), pool.Status.ConsecutiveFailures)
	require.Empty(t, e.poolMembers(t, e.pool.Name), "the failure write does not count as publishing")
	e.reconcilePool(t, 1)
	require.Len(t, e.poolMembers(t, e.pool.Name), 1)
}

// stalePoolStatusClient answers pool reads from a cache that has not seen
// the stored failure run.
type stalePoolStatusClient struct{ client.Client }

func (c stalePoolStatusClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if pool, ok := obj.(*coderv1alpha1.CoderWorkspacePool); ok {
		pool.Status.RunFailures, pool.Status.ConsecutiveFailures, pool.Status.LastFailure, pool.Status.FailureRunStart = nil, 0, nil, nil
	}
	return nil
}

// TestPoolFailureRunUncached: the pool reads its own object without the
// cache, so a reconcile that a member delete starts before the cache sees
// the stored run still backs off.
func TestPoolFailureRunUncached(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.reconcilePool(t, 1)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 1)
	e.tick()
	e.setMemberStatus(t, memberKey(members[0]), e.failedStatus("BuildFailed"))
	requirePool(t, e.reconcilePool(t, 1), "FailureBackoff", false, false)
	r := &controller.CoderWorkspacePoolReconciler{Client: stalePoolStatusClient{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.NoError(t, err)
	require.Empty(t, e.poolMembers(t, e.pool.Name), "the backoff holds on a stale cache")
}

// TestPoolFailureRunSorted: a failure that becomes visible after a newer
// one still lands in time order, so lastFailure and the backoff use the
// newest failure.
func TestPoolFailureRunSorted(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	e.reconcilePool(t, 1)
	e.tick()
	older := e.clock.Now()
	e.tick()
	newer := e.clock.Now()
	pool := e.reconcilePool(t, 0)
	pool.Status.RunFailures = []coderv1alpha1.CoderWorkspacePoolFailure{{Member: "seen-first", Reason: "BuildFailed", Time: &metav1.Time{Time: newer}}}
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	e.clock.SetTime(older)
	late := e.createMember(t, nil)
	e.setMemberStatus(t, late, e.failedStatus("BuildFailed"))
	e.clock.SetTime(newer.Add(time.Minute))
	pool = e.reconcilePool(t, 1)
	require.Len(t, pool.Status.RunFailures, 2)
	require.Equal(t, late.Name, pool.Status.RunFailures[0].Member, "oldest first")
	require.Equal(t, "seen-first", pool.Status.LastFailure.Member, "the newest failure")
}
