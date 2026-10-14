package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	e.reconcilePool(t, 1)
	members := e.poolMembers(t, e.pool.Name)
	require.Len(t, members, 1)
	// The member accepts its inputs: it passes the check and creates.
	member := e.reconcileMember(t, memberKey(members[0]), 3)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, member.Status.Phase, member.Status.Message)
}

// TestPoolMemberDiscovery: a member that the pool controls counts even
// after its label drifts (as outdated), and a pool with a 253-character name
// still creates members.
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

// TestPoolMemberRechecksConsentUncached: the member reads the control
// plane's consent without the cache right before its create request, so a
// template that an admin just removed is never built, whatever the cache
// still says.
func TestPoolMemberRechecksConsentUncached(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	e.allowOwner(t, e.tester.String(), "default.other")
	stale := staleControlPlaneClient{k8sClient, func(o *coderv1alpha1.WorkspacePoolOwner) { o.Templates = []string{"default.docker"} }}
	r := &controller.CoderWorkspacePoolMemberReconciler{Client: stale, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	for range 3 {
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	member := e.reconcileMember(t, key, 0)
	require.Equal(t, "TemplateNotAllowed", member.Status.Reason, member.Status.Message)
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace))
}

// stalePoolClient answers pool reads from a cache that has not seen the
// pool's resolved inputs yet.
type stalePoolClient struct{ client.Client }

func (c stalePoolClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if pool, ok := obj.(*coderv1alpha1.CoderWorkspacePool); ok {
		pool.Status.Resolved = nil
	}
	return nil
}

// TestPoolMemberReadsPoolUncached: the member compares its inputs with the
// pool read without the cache, so a cache that lags behind the pool's
// status never fails a valid member.
func TestPoolMemberReadsPoolUncached(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	r := &controller.CoderWorkspacePoolMemberReconciler{Client: stalePoolClient{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	for range 3 {
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	member := e.reconcileMember(t, key, 0)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, member.Status.Phase, member.Status.Message)
}

// staleDeletingControlPlaneClient answers control plane reads from a cache
// that has not seen the start of its deletion.
type staleDeletingControlPlaneClient struct{ client.Client }

func (c staleDeletingControlPlaneClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if cp, ok := obj.(*coderv1alpha1.CoderControlPlane); ok {
		cp.DeletionTimestamp, cp.Finalizers = nil, nil
	}
	return nil
}

// TestPoolNoCreateWhileControlPlaneDeletes: once the control plane's
// deletion starts, neither the pool nor a member creates anything, whatever
// the cache still says. The cleanup releases members of a deleting control
// plane without a Coder call, so a new workspace would be left behind.
func TestPoolNoCreateWhileControlPlaneDeletes(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	member := e.createMember(t, nil)
	cp := &coderv1alpha1.CoderControlPlane{}
	cpKey := types.NamespacedName{Namespace: e.ns, Name: "coder"}
	require.NoError(t, k8sClient.Get(e.ctx, cpKey, cp))
	cp.Finalizers = append(cp.Finalizers, "test.coder.com/hold")
	require.NoError(t, k8sClient.Update(e.ctx, cp))
	require.NoError(t, k8sClient.Delete(e.ctx, cp))
	t.Cleanup(func() {
		cp := &coderv1alpha1.CoderControlPlane{}
		if k8sClient.Get(e.ctx, cpKey, cp) == nil {
			cp.Finalizers = nil
			_ = k8sClient.Update(e.ctx, cp)
		}
	})
	stale := staleDeletingControlPlaneClient{k8sClient}
	m := &controller.CoderWorkspacePoolMemberReconciler{Client: stale, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	p := &controller.CoderWorkspacePoolReconciler{Client: stale, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	for range 3 {
		_, err := m.Reconcile(e.ctx, ctrl.Request{NamespacedName: member})
		require.NoError(t, err)
		_, err = p.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
		require.NoError(t, err)
	}
	require.Equal(t, "ControlPlaneDeleting", e.reconcileMember(t, member, 0).Status.Reason)
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request")
	require.Len(t, e.poolMembers(t, e.pool.Name), 1, "no new member")
}
