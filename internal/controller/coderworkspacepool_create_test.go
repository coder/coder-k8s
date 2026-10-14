package controller_test

import (
	"context"
	"errors"
	"strings"
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
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.MaxInFlight = new(int32(3)), new(int32(2))
	})
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
		e.setMemberStatus(t, memberKey(members[0]), e.failedStatus("BuildFailed"))
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
	require.True(t, meta.IsStatusConditionTrue(pool.Status.Conditions, coderv1alpha1.CoderWorkspacePoolConditionReady))
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

// TestPoolCountsFailedMemberOnce: the pool stores the count before it
// deletes a failed member. A lost status write after the delete, or a crash
// before it, neither loses nor repeats the count. The delete carries the
// listed UID and resourceVersion, so a member that changed after the list
// stays.
func TestPoolCountsFailedMemberOnce(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	failed := func(t *testing.T) types.NamespacedName {
		t.Helper()
		member := e.createMember(t, nil)
		e.setMemberStatus(t, member, memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed))
		return member
	}
	reconcileWith := func(t *testing.T, c deleteHookClient) error {
		t.Helper()
		c.Client, c.deleted = k8sClient, new(false)
		r := &controller.CoderWorkspacePoolReconciler{Client: c, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	// observedGeneration is current, so no spec change resets the count.
	pool := e.reconcilePool(t, 0)
	pool.Status.ObservedGeneration = pool.Generation
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	// The status write after the delete is lost: the count stays stored.
	member := failed(t)
	require.True(t, apierrors.IsConflict(reconcileWith(t, deleteHookClient{failStatus: true})))
	require.Nil(t, e.reconcileMember(t, member, 0))
	require.Equal(t, int32(1), e.reconcilePool(t, 0).Status.ConsecutiveFailures)

	// A crash after the count, before the delete: the next reconcile deletes
	// the member and does not count it again.
	member = failed(t)
	pool = e.reconcilePool(t, 0)
	pool.Status.ConsecutiveFailures, pool.Status.LastFailure = 2, &coderv1alpha1.CoderWorkspacePoolFailure{Member: member.Name, Reason: "BuildFailed", Time: &metav1.Time{Time: e.clock.Now()}}
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

// TestPoolFailureRunOrder: failures and successes count in the order they
// happened, not in the order the pool saw them, and only members that the
// pool controls take part.
func TestPoolFailureRunOrder(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	start := e.clock.Now()
	// Two members failed, then a third became ready, all before one reconcile.
	for _, at := range []time.Duration{0, time.Second} {
		e.clock.SetTime(start.Add(at))
		e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	}
	e.setMemberStatus(t, e.createMember(t, nil), e.readyAt(start.Add(2*time.Second)))
	// A ready member that the pool does not control never resets the count.
	foreign := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.OwnerReferences = nil })
	e.setMemberStatus(t, foreign, e.readyAt(start.Add(time.Hour)))
	e.clock.SetTime(start.Add(time.Minute))
	var pool *coderv1alpha1.CoderWorkspacePool
	for range 3 { // Each reconcile counts one failure.
		pool = e.reconcilePool(t, 1)
		require.Zero(t, pool.Status.ConsecutiveFailures, "the success after both failures ended their run")
	}
	require.Len(t, e.poolMembers(t, e.pool.Name), 2, "both failed members are deleted: the ready and the foreign member remain")

	// A failure after the success starts a new run, and the foreign member
	// does not end it.
	e.clock.SetTime(start.Add(2 * time.Minute))
	e.setMemberStatus(t, e.createMember(t, nil), e.failedStatus("BuildFailed"))
	pool = e.reconcilePool(t, 1)
	require.Equal(t, int32(1), pool.Status.ConsecutiveFailures)
	require.True(t, pool.Status.LastFailure.Time.Time.Equal(start.Add(2*time.Minute)), "the failure time is when the member failed")
}

// staleControlPlaneClient answers control plane reads from a cache that
// still holds the owner entry before an admin changed it.
type staleControlPlaneClient struct {
	client.Client
	stale func(*coderv1alpha1.WorkspacePoolOwner)
}

func (c staleControlPlaneClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if cp, ok := obj.(*coderv1alpha1.CoderControlPlane); ok && cp.Spec.WorkspacePools != nil {
		for i := range cp.Spec.WorkspacePools.Owners {
			c.stale(&cp.Spec.WorkspacePools.Owners[i])
		}
	}
	return nil
}

// TestPoolOwnerCapUncached: the cap and the consent come from an uncached
// read right before the creates, so a cap that an admin just lowered, or a
// template that the admin just removed, holds even while the cache has the
// old values.
func TestPoolOwnerCapUncached(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(3)) })
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Spec.WorkspacePools.Owners[0].MaxWorkspaces = new(int32(1))
	require.NoError(t, k8sClient.Update(e.ctx, cp))
	reconcileStale := func(t *testing.T, stale func(*coderv1alpha1.WorkspacePoolOwner)) {
		t.Helper()
		r := &controller.CoderWorkspacePoolReconciler{Client: staleControlPlaneClient{k8sClient, stale}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
		for range 2 {
			_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
			require.NoError(t, err)
		}
	}
	reconcileStale(t, func(o *coderv1alpha1.WorkspacePoolOwner) { o.MaxWorkspaces = new(int32(10)) })
	require.Len(t, e.poolMembers(t, e.pool.Name), 1)
	requirePool(t, e.reconcilePool(t, 0), "OwnerWorkspaceLimit", false, false)

	// The same for consent: the admin removed the template, the cache still
	// allows it.
	cp.Spec.WorkspacePools.Owners[0].MaxWorkspaces, cp.Spec.WorkspacePools.Owners[0].Templates = new(int32(10)), []string{"default.other"}
	require.NoError(t, k8sClient.Update(e.ctx, cp))
	reconcileStale(t, func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = []string{"default.docker"} })
	require.Len(t, e.poolMembers(t, e.pool.Name), 1, "no member after consent is withdrawn")
	require.Equal(t, "TemplateNotAllowed", e.reconcilePool(t, 0).Status.Reason)
}

// TestPoolPublishesResolvedBeforeCreating: members check their inputs
// against the pool's status.resolved, so the pool stores new resolved
// inputs before it creates any member that pins them.
func TestPoolPublishesResolvedBeforeCreating(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.pool.Status = coderv1alpha1.CoderWorkspacePoolStatus{}
	require.NoError(t, k8sClient.Status().Update(e.ctx, e.pool))
	pool := e.reconcilePool(t, 1)
	require.NotNil(t, pool.Status.Resolved)
	require.Empty(t, e.poolMembers(t, e.pool.Name), "no member before status.resolved is stored")
	require.Equal(t, time.Second, e.lastStep.RequeueAfter)
	// The same when a failure write comes first in that reconcile.
	e.pool = pool // createMember pins the stored resolved inputs.
	failedKey := e.createMember(t, nil)
	e.setMemberStatus(t, failedKey, e.failedStatus("BuildFailed"))
	e.clock.SetTime(e.clock.Now().Add(time.Hour)) // The backoff is over: only the gate stops a create.
	pool = e.reconcilePool(t, 0)
	pool.Status.Resolved = nil
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	pool = e.reconcilePool(t, 1)
	require.Equal(t, int32(1), pool.Status.ConsecutiveFailures)
	require.Empty(t, e.poolMembers(t, e.pool.Name), "the failure write does not count as publishing")
	pool.Status.ConsecutiveFailures, pool.Status.LastFailure = 0, nil
	require.NoError(t, k8sClient.Status().Update(e.ctx, pool))
	e.clock.SetTime(e.clock.Now().Add(-time.Hour)) // Back within the members' ready timeout.
	e.reconcilePool(t, 1)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 1)
	// The member accepts its inputs: it passes the check and creates.
	member := e.reconcileMember(t, memberKey(members[0]), 3)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, member.Status.Phase, member.Status.Message)
}

// TestPoolMemberDiscovery: a member that the pool controls counts even
// after its label drifts (as outdated), a failure of a member with old inputs is deleted
// without counting, and a pool with a 253-character name still creates
// members.
func TestPoolMemberDiscovery(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.reconcilePool(t, 1)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 1)
	member := e.reconcileMember(t, memberKey(members[0]), 0)
	member.Labels = nil
	require.NoError(t, k8sClient.Update(e.ctx, member))
	// The member no longer matches the pool's inputs, so the pool counts it
	// as outdated and builds one current member, but never more.
	pool := e.reconcilePool(t, 3)
	require.Equal(t, int32(2), pool.Status.Replicas, "the controlled member still counts")
	require.Equal(t, int32(1), pool.Status.UpdatedReplicas)
	list := &coderv1alpha1.CoderWorkspacePoolMemberList{}
	require.NoError(t, k8sClient.List(e.ctx, list, client.InNamespace(e.ns)))
	require.Len(t, list.Items, 2, "one replacement for a member whose label drifted")

	// A failed member with old inputs is deleted, not counted.
	outdated := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.Spec.TemplateVersionID = e.fake.addVersion(e.tplID, "v0", codersdk.ProvisionerJobSucceeded).String()
	})
	e.setMemberStatus(t, outdated, e.failedStatus("BuildFailed"))
	pool = e.reconcilePool(t, 1)
	require.Zero(t, pool.Status.ConsecutiveFailures)
	require.Nil(t, e.reconcileMember(t, outdated, 0))

	// The longest pool name: the API server cuts the generated name to 63
	// characters, so the members still get valid names.
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	require.Len(t, long, 253)
	other := &coderv1alpha1.CoderWorkspacePool{ObjectMeta: metav1.ObjectMeta{Name: long, Namespace: e.ns}, Spec: *e.pool.Spec.DeepCopy()}
	require.NoError(t, k8sClient.Create(e.ctx, other))
	r := &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	for range 2 {
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: long}})
		require.NoError(t, err)
	}
	created := e.poolMembers(t, long)
	require.Len(t, created, 1)
	require.Equal(t, long, created[0].Spec.PoolName)
	require.LessOrEqual(t, len(created[0].Name), 63)
}
