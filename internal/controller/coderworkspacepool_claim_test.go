package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// createClaim creates a claim on the env's pool.
func (e *poolMemberEnv) createClaim(t *testing.T, name string) types.NamespacedName {
	t.Helper()
	claim := &coderv1alpha1.CoderWorkspaceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec:       coderv1alpha1.CoderWorkspaceClaimSpec{PoolName: e.pool.Name},
	}
	require.NoError(t, k8sClient.Create(e.ctx, claim))
	return types.NamespacedName{Namespace: e.ns, Name: name}
}

func (e *poolMemberEnv) claim(t *testing.T, key types.NamespacedName) *coderv1alpha1.CoderWorkspaceClaim {
	t.Helper()
	claim := &coderv1alpha1.CoderWorkspaceClaim{}
	require.NoError(t, k8sClient.Get(e.ctx, key, claim))
	return claim
}

// readyWithWorkspace is a ready member with a workspace ID and name.
func readyWithWorkspace(name string) func(*coderv1alpha1.CoderWorkspacePoolMemberStatus) {
	return withWorkspace(func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
		readyStatus(s)
		s.WorkspaceName = name
	})
}

// requireBound checks that the member holds the claim: claimRef, the claim
// as the only controller, no reference to the pool, and the pool label.
func (e *poolMemberEnv) requireBound(t *testing.T, member, claimKey types.NamespacedName) {
	t.Helper()
	m, claim := e.reconcileMember(t, member, 0), e.claim(t, claimKey)
	require.NotNil(t, m.Spec.ClaimRef, "member %s is not bound", member.Name)
	require.Equal(t, coderv1alpha1.CoderWorkspaceClaimReference{Name: claim.Name, UID: string(claim.UID)}, *m.Spec.ClaimRef)
	controllerRef := metav1.GetControllerOf(m)
	require.NotNil(t, controllerRef)
	require.Equal(t, claim.UID, controllerRef.UID)
	require.Equal(t, "CoderWorkspaceClaim", controllerRef.Kind)
	require.True(t, *controllerRef.BlockOwnerDeletion, "a claim delete waits for the member's cleanup")
	for _, o := range m.OwnerReferences {
		require.NotEqual(t, e.pool.UID, o.UID, "garbage collection of the pool never reaches a bound member")
	}
	require.Equal(t, string(e.pool.UID), m.Labels[coderv1alpha1.CoderWorkspacePoolSelectorLabel])
}

func requireClaim(t *testing.T, claim *coderv1alpha1.CoderWorkspaceClaim, phase, reason string, ready bool) {
	t.Helper()
	require.Equal(t, phase, claim.Status.Phase, claim.Status.Message)
	require.Equal(t, reason, claim.Status.Reason, claim.Status.Message)
	require.Equal(t, ready, meta.IsStatusConditionTrue(claim.Status.Conditions, coderv1alpha1.CoderWorkspaceClaimConditionReady))
	require.Equal(t, !ready, meta.IsStatusConditionTrue(claim.Status.Conditions, coderv1alpha1.CoderWorkspaceClaimConditionReconciling))
	require.False(t, meta.IsStatusConditionTrue(claim.Status.Conditions, coderv1alpha1.CoderWorkspaceClaimConditionStalled))
}

// TestPoolBindsOldestClaimFirst: with one ready member and two claims, the
// older claim gets the member. Its status names the workspace, and the
// other claim waits.
func TestPoolBindsOldestClaimFirst(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	member := e.createMember(t, nil)
	e.setMemberStatus(t, member, readyWithWorkspace("kwp-ready"))
	older := e.createClaim(t, "older")
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has second precision.
	newer := e.createClaim(t, "newer")

	pool := e.reconcilePool(t, 1)
	e.requireBound(t, member, older)
	claim := e.claim(t, older)
	requireClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	m := e.reconcileMember(t, member, 0)
	require.Equal(t, m.Name, claim.Status.MemberName)
	require.Equal(t, m.Status.WorkspaceID, claim.Status.WorkspaceID)
	require.Equal(t, "kwp-ready", claim.Status.WorkspaceName)
	require.Equal(t, "tester", claim.Status.OwnerName, "coder ssh tester/kwp-ready reaches it")
	require.NotNil(t, claim.Status.BoundTime)
	requireClaim(t, e.claim(t, newer), coderv1alpha1.CoderWorkspaceClaimPhasePending, "WaitingForReadyWorkspace", false)
	require.Equal(t, int32(1), pool.Status.BoundReplicas)
	require.Zero(t, pool.Status.ReadyReplicas, "a bound member is not ready to claim")
	require.Len(t, e.poolMembers(t, e.pool.Name), 3, "the pool replaces the bound member and adds one for the waiting claim")
}

// TestPoolBindsCurrentBeforeOutdated: a claim takes a current member before
// an outdated one, even an older one. A used member and a member whose
// agent is not ready are never bound (plan 16, PR 9 advisor item (a)), nor
// is a member whose spec.poolName names another pool.
func TestPoolBindsCurrentBeforeOutdated(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(4)) })
	v0 := e.fake.addVersion(e.tplID, "v0", codersdk.ProvisionerJobSucceeded).String()
	now := e.clock.Now()
	// The outdated member's label drifted: the pool finds it as its
	// controller, and the bind sets the label again.
	outdated := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Spec.TemplateVersionID, m.Labels = v0, nil })
	e.setMemberStatus(t, outdated, withWorkspace(e.readyAt(now.Add(-time.Hour))))
	current := e.createMember(t, nil)
	e.setMemberStatus(t, current, withWorkspace(e.readyAt(now)))
	used := e.createMember(t, nil)
	e.setMemberStatus(t, used, withWorkspace(usedStatus))
	notReady := e.createMember(t, nil)
	e.setMemberStatus(t, notReady, withWorkspace(memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseReady,
		memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionReady, metav1.ConditionFalse, "AgentNotReady"))))
	// The pool controls this member, but it names another pool.
	misnamed := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Spec.PoolName = "other" })
	e.setMemberStatus(t, misnamed, withWorkspace(e.readyAt(now.Add(-2*time.Hour))))
	first := e.createClaim(t, "first")
	time.Sleep(1100 * time.Millisecond)
	second := e.createClaim(t, "second")
	time.Sleep(1100 * time.Millisecond)
	third := e.createClaim(t, "third")

	e.reconcilePool(t, 1)
	e.requireBound(t, current, first)
	e.requireBound(t, outdated, second)
	requireClaim(t, e.claim(t, third), coderv1alpha1.CoderWorkspaceClaimPhasePending, "WaitingForReadyWorkspace", false)
	require.Nil(t, e.reconcileMember(t, used, 0).Spec.ClaimRef)
	require.Nil(t, e.reconcileMember(t, notReady, 0).Spec.ClaimRef)
	require.Nil(t, e.reconcileMember(t, misnamed, 0).Spec.ClaimRef)
}

// TestPoolBindSkipsUseAfterLastRead (#409): a member reads last_used_at once
// a minute, so its status does not show use since that read. The bind reads
// Coder again: the claim takes the next member, and the used member's next
// read marks it Used, so the pool releases it.
func TestPoolBindSkipsUseAfterLastRead(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	used, usedID := e.readyMember(t)
	e.tick() // The used member is the older one, so the bind tries it first.
	unused, _ := e.readyMember(t)
	e.fake.markUsed(usedID, e.clock.Now().Add(30*time.Second))
	claim := e.createClaim(t, "build")

	e.reconcilePool(t, 1)
	e.requireBound(t, unused, claim)
	require.Nil(t, e.reconcileMember(t, used, 0).Spec.ClaimRef, "a workspace used after the member's last read is never bound")

	requireMember(t, e.reconcileMember(t, used, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Used", "")
	e.reconcilePool(t, 1)
	m := e.reconcileMember(t, used, 0)
	require.Nil(t, metav1.GetControllerOf(m), "the pool released the used member")
	require.Nil(t, m.Spec.ClaimRef)
}

// bindHookClient runs hook once, right before the first member update.
type bindHookClient struct {
	client.Client
	hook func(m *coderv1alpha1.CoderWorkspacePoolMember)
	done *bool
}

func (c bindHookClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if m, ok := obj.(*coderv1alpha1.CoderWorkspacePoolMember); ok && !*c.done {
		*c.done = true
		c.hook(m)
	}
	return c.Client.Update(ctx, obj, opts...)
}

// afterMemberList runs hook once, right after the first member list.
type afterMemberList struct {
	client.Reader
	hook func()
	done *bool
}

func (c afterMemberList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if _, ok := list.(*coderv1alpha1.CoderWorkspacePoolMemberList); ok && !*c.done {
		*c.done = true
		c.hook()
	}
	return nil
}

// onlyClaim lists only the named claim, as a cache that has not seen the
// others yet.
type onlyClaim struct {
	client.Client
	name string
}

func (c onlyClaim) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if claims, ok := list.(*coderv1alpha1.CoderWorkspaceClaimList); ok {
		var keep []coderv1alpha1.CoderWorkspaceClaim
		for _, c2 := range claims.Items {
			if c2.Name == c.name {
				keep = append(keep, c2)
			}
		}
		claims.Items = keep
	}
	return nil
}

// TestPoolBindRace is mutation check 4: a bind carries the listed
// resourceVersion. Two reconciles that race for one member (for example two
// operator replicas during a leader change, one with a cache that has not
// seen the older claim) bind exactly one claim. A member that its
// reconciler marks used between the list and the bind is not bound.
func TestPoolBindRace(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	member := e.createMember(t, nil)
	e.setMemberStatus(t, member, readyWithWorkspace("kwp-race"))
	a := e.createClaim(t, "a")
	time.Sleep(1100 * time.Millisecond)
	b := e.createClaim(t, "b")
	other := &controller.CoderWorkspacePoolReconciler{Client: onlyClaim{k8sClient, "b"}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	racing := bindHookClient{Client: k8sClient, done: new(false), hook: func(*coderv1alpha1.CoderWorkspacePoolMember) {
		_, err := other.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}}
	r := &controller.CoderWorkspacePoolReconciler{Client: racing, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.Error(t, err, "the second bind of the member must fail")
	e.requireBound(t, member, b)
	e.reconcilePool(t, 1)
	requireClaim(t, e.claim(t, a), coderv1alpha1.CoderWorkspaceClaimPhasePending, "WaitingForReadyWorkspace", false)
	bound := 0
	for _, m := range e.poolMembers(t, e.pool.Name) {
		if m.Spec.ClaimRef != nil {
			bound++
		}
	}
	require.Equal(t, 1, bound, "exactly one bind")

	// Someone uses a ready member right after the pool's member list.
	e2 := newPoolMemberEnv(t)
	used := e2.createMember(t, nil)
	e2.setMemberStatus(t, used, readyWithWorkspace("kwp-used"))
	e2.createClaim(t, "c")
	marking := afterMemberList{Reader: k8sClient, done: new(false), hook: func() { e2.setMemberStatus(t, used, usedStatus) }}
	r = &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: marking, Scheme: scheme, Clock: e2.clock}
	_, err = r.Reconcile(e2.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e2.ns, Name: e2.pool.Name}})
	require.True(t, apierrors.IsConflict(err), "the resourceVersion refuses the bind: %v", err)
	require.Nil(t, e2.reconcileMember(t, used, 0).Spec.ClaimRef, "a used member is never bound")
}

// TestPoolFinishesClaimAfterCrash: the pool crashed after the member bind,
// before the claim status. The next reconcile finds the member that holds
// the claim, finishes the claim status, and binds no second member.
func TestPoolFinishesClaimAfterCrash(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(2)) })
	claimKey := e.createClaim(t, "build-1")
	claim := e.claim(t, claimKey)
	held := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: claim.Name, UID: string(claim.UID)}
		m.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: coderv1alpha1.GroupVersion.String(), Kind: "CoderWorkspaceClaim", Name: claim.Name, UID: claim.UID,
			Controller: new(true), BlockOwnerDeletion: new(true),
		}}
	})
	e.setMemberStatus(t, held, readyWithWorkspace("kwp-held"))
	spare := e.createMember(t, nil)
	e.setMemberStatus(t, spare, readyWithWorkspace("kwp-spare"))

	e.reconcilePool(t, 1)
	claim = e.claim(t, claimKey)
	requireClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
	require.Equal(t, held.Name, claim.Status.MemberName)
	require.Equal(t, "kwp-held", claim.Status.WorkspaceName)
	require.Equal(t, "tester", claim.Status.OwnerName)
	require.Nil(t, e.reconcileMember(t, spare, 0).Spec.ClaimRef, "no second member for the claim")
}

// TestPoolClaimDeleteDeletesWorkspace: the claim controls its member, with
// blockOwnerDeletion, so a claim delete makes garbage collection delete the
// member. envtest runs no garbage collector, so the test deletes the member
// as the collector would. The member's cleanup then deletes the workspace,
// also after the claimant used it.
func TestPoolClaimDeleteDeletesWorkspace(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	member, workspaceID := e.readyMember(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(1)) })
	claimKey := e.createClaim(t, "build-1")
	e.reconcilePool(t, 1)
	e.requireBound(t, member, claimKey)

	e.fake.markUsed(workspaceID, e.clock.Now().Add(time.Minute))
	require.NoError(t, k8sClient.Delete(e.ctx, e.claim(t, claimKey)))
	e.deleteMember(t, member)
	m := e.reconcileMember(t, member, 1)
	requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "Deleting", "started a delete build")
	e.fake.setBuildJob(uuid.MustParse(m.Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	require.Nil(t, e.reconcileMember(t, member, 1), "the workspace is deleted, then the finalizer goes")
}

// staleClaims answers claim lists with the claims as the cache first saw
// them: without status.
type staleClaims struct{ client.Client }

func (c staleClaims) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if claims, ok := list.(*coderv1alpha1.CoderWorkspaceClaimList); ok {
		for i := range claims.Items {
			claims.Items[i].Status = coderv1alpha1.CoderWorkspaceClaimStatus{}
		}
	}
	return nil
}

// TestPoolNeverRebindsClaim: a claim that a member held once never binds
// again, also when its member is gone and the cache still shows the claim
// without status: the pool reads the claim again without the cache right
// before a bind. The claim becomes Lost with MemberDeleted. A member of
// another pool that names the claim never counts as its holder.
func TestPoolNeverRebindsClaim(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(2)) })
	first := e.createMember(t, nil)
	e.setMemberStatus(t, first, readyWithWorkspace("kwp-first"))
	claimKey := e.createClaim(t, "build-1")
	claim := e.claim(t, claimKey)
	// A member of another pool with a claimRef to this claim.
	e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.Spec.PoolName, m.OwnerReferences, m.Labels = "other", nil, nil
		m.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: claim.Name, UID: string(claim.UID)}
	})
	e.reconcilePool(t, 1)
	e.requireBound(t, first, claimKey)
	require.Equal(t, first.Name, e.claim(t, claimKey).Status.MemberName)

	require.NoError(t, k8sClient.Delete(e.ctx, e.reconcileMember(t, first, 0)))
	spare := e.createMember(t, nil)
	e.setMemberStatus(t, spare, readyWithWorkspace("kwp-spare"))
	stale := &controller.CoderWorkspacePoolReconciler{Client: staleClaims{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := stale.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.NoError(t, err)
	require.Nil(t, e.reconcileMember(t, spare, 0).Spec.ClaimRef, "no second member for the claim")
	claim = e.claim(t, claimKey)
	require.Equal(t, coderv1alpha1.CoderWorkspaceClaimPhaseLost, claim.Status.Phase)
	require.Equal(t, "MemberDeleted", claim.Status.Reason)
	require.True(t, meta.IsStatusConditionTrue(claim.Status.Conditions, coderv1alpha1.CoderWorkspaceClaimConditionStalled))
}

// fixedClaims answers claim lists with a saved copy, as a cache that still
// holds a claim that is gone.
type fixedClaims struct {
	client.Client
	items []coderv1alpha1.CoderWorkspaceClaim
}

func (c fixedClaims) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if claims, ok := list.(*coderv1alpha1.CoderWorkspaceClaimList); ok {
		claims.Items = append([]coderv1alpha1.CoderWorkspaceClaim(nil), c.items...)
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

// TestPoolSkipsReplacedClaim: the cache still holds a deleted claim, and a
// new claim of another pool took its name. The pool neither binds the new
// claim nor writes its status.
func TestPoolSkipsReplacedClaim(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	member := e.createMember(t, nil)
	e.setMemberStatus(t, member, readyWithWorkspace("kwp-ready"))
	key := e.createClaim(t, "build-1")
	old := e.claim(t, key)
	require.NoError(t, k8sClient.Delete(e.ctx, old))
	other := &coderv1alpha1.CoderWorkspaceClaim{}
	other.Name, other.Namespace, other.Spec.PoolName = key.Name, key.Namespace, "other"
	require.NoError(t, k8sClient.Create(e.ctx, other))

	r := &controller.CoderWorkspacePoolReconciler{Client: fixedClaims{k8sClient, []coderv1alpha1.CoderWorkspaceClaim{*old}}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.NoError(t, err)
	require.Nil(t, e.reconcileMember(t, member, 0).Spec.ClaimRef)
	require.Empty(t, e.claim(t, key).Status.Phase, "the other pool's claim has no status from this pool")
}

// TestPoolClaimNotLostOnOverlap: another reconcile binds the claim after
// this reconcile listed members. This reconcile sees the claim Bound
// without a holder in its own list, checks the holder again without the
// cache, and keeps the claim Bound instead of marking it Lost.
func TestPoolClaimNotLostOnOverlap(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	member := e.createMember(t, nil)
	e.setMemberStatus(t, member, readyWithWorkspace("kwp-ready"))
	claimKey := e.createClaim(t, "build-1")
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	inner := &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	overlap := afterMemberList{Reader: k8sClient, done: new(false), hook: func() {
		_, err := inner.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}}
	r := &controller.CoderWorkspacePoolReconciler{Client: staleClaims{k8sClient}, APIReader: overlap, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		require.True(t, apierrors.IsConflict(err), "only a conflict may end the overlapping reconcile: %v", err)
	}
	e.requireBound(t, member, claimKey)
	requireClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true)
}
