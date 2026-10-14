package controller_test

import (
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

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// withWorkspace marks a member as one that sent its create request.
func withWorkspace(edit func(*coderv1alpha1.CoderWorkspacePoolMemberStatus)) func(*coderv1alpha1.CoderWorkspacePoolMemberStatus) {
	return func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
		if edit != nil {
			edit(s)
		}
		s.WorkspaceID = uuid.NewString()
	}
}

// usedStatus is a ready member that someone used without a claim.
var usedStatus = memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseReady,
	memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionReady, metav1.ConditionTrue, "WorkspaceReady"),
	memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionUsed, metav1.ConditionTrue, "Used"))

// gone reports whether the member no longer exists.
func (e *poolMemberEnv) gone(t *testing.T, key types.NamespacedName) bool {
	t.Helper()
	return e.reconcileMember(t, key, 0) == nil
}

// TestPoolDeleteOrder: on a scale-down the pool deletes current members
// without a create request first, then members that are not ready (newest
// first), then ready members (oldest first).
func TestPoolDeleteOrder(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(5)) })
	now := e.clock.Now()
	readyNew := e.createMember(t, nil)
	e.setMemberStatus(t, readyNew, withWorkspace(e.readyAt(now.Add(-time.Minute))))
	readyOld := e.createMember(t, nil)
	e.setMemberStatus(t, readyOld, withWorkspace(e.readyAt(now.Add(-time.Hour))))
	building := e.createMember(t, nil)
	e.setMemberStatus(t, building, withWorkspace(nil))
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has second precision.
	buildingNew := e.createMember(t, nil)
	e.setMemberStatus(t, buildingNew, withWorkspace(nil))
	pending := e.createMember(t, nil)
	e.reconcilePool(t, 1)
	require.Len(t, e.poolMembers(t, e.pool.Name), 5)

	for _, step := range []struct {
		replicas int32
		deleted  types.NamespacedName
	}{{4, pending}, {3, buildingNew}, {2, building}, {1, readyOld}} {
		e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(step.replicas) })
		e.reconcilePool(t, 1)
		require.True(t, e.gone(t, step.deleted), "replicas %d deletes %s", step.replicas, step.deleted.Name)
		require.Len(t, e.poolMembers(t, e.pool.Name), int(step.replicas))
	}
	require.False(t, e.gone(t, readyNew))
}

// TestPoolNewBeforeOld: after a version change, outdated members that are
// not ready go at once, and outdated ready members stay claimable until
// current members are ready, oldest first.
func TestPoolNewBeforeOld(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Replicas, s.MaxInFlight = new(int32(2)), new(int32(2))
	})
	v0 := e.fake.addVersion(e.tplID, "v0", codersdk.ProvisionerJobSucceeded).String()
	outdated := func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Spec.TemplateVersionID = v0 }
	now := e.clock.Now()
	oldest := e.createMember(t, outdated)
	e.setMemberStatus(t, oldest, withWorkspace(e.readyAt(now.Add(-2*time.Hour))))
	older := e.createMember(t, outdated)
	e.setMemberStatus(t, older, withWorkspace(e.readyAt(now.Add(-time.Hour))))
	building := e.createMember(t, outdated)
	e.setMemberStatus(t, building, withWorkspace(nil))

	e.reconcilePool(t, 2)
	require.True(t, e.gone(t, building), "an outdated member that is not ready goes at once")
	require.False(t, e.gone(t, oldest))
	require.False(t, e.gone(t, older))
	var current []types.NamespacedName
	for _, m := range e.poolMembers(t, e.pool.Name) {
		if m.Spec.TemplateVersionID != v0 {
			current = append(current, memberKey(m))
		}
	}
	require.Len(t, current, 2)

	e.setMemberStatus(t, current[0], withWorkspace(e.readyAt(now)))
	e.reconcilePool(t, 1)
	require.True(t, e.gone(t, oldest), "one current ready member replaces the oldest outdated one")
	require.False(t, e.gone(t, older))
	e.setMemberStatus(t, current[1], withWorkspace(e.readyAt(now)))
	e.reconcilePool(t, 1)
	require.True(t, e.gone(t, older))
	requirePool(t, e.reconcilePool(t, 1), "Ready", true, false)
}

// TestPoolReleasesUsedMember: the pool releases a member that someone used
// and never deletes it, also not on a scale to zero (mutation check 5). A
// member that its reconciler marks used after the pool's list answers 409
// to the delete and stays.
func TestPoolReleasesUsedMember(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(2)) })
	used := e.createMember(t, nil)
	e.setMemberStatus(t, used, withWorkspace(usedStatus))
	ready := e.createMember(t, nil)
	e.setMemberStatus(t, ready, withWorkspace(readyStatus))
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })

	pool := e.reconcilePool(t, 2)
	require.True(t, e.gone(t, ready))
	released := e.reconcileMember(t, used, 0)
	require.NotNil(t, released, "a used member is never deleted")
	require.Nil(t, metav1.GetControllerOf(released), "the pool released it")
	require.Equal(t, string(pool.UID), released.Labels[coderv1alpha1.CoderWorkspacePoolSelectorLabel])
	require.Equal(t, int32(1), pool.Status.UsedReplicas)
	require.Zero(t, pool.Status.Replicas)

	// The member reconciler marks the member used between the list and the
	// delete: the resourceVersion precondition refuses the delete.
	late := e.createMember(t, nil)
	e.setMemberStatus(t, late, withWorkspace(readyStatus))
	hook := deleteHookClient{Client: k8sClient, deleted: new(false), hook: func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		e.setMemberStatus(t, memberKey(*m), usedStatus)
	}}
	r := &controller.CoderWorkspacePoolReconciler{Client: hook, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.True(t, apierrors.IsConflict(err), "the delete conflicts: %v", err)
	e.reconcilePool(t, 1)
	require.Nil(t, metav1.GetControllerOf(e.reconcileMember(t, late, 0)), "the next reconcile releases it")
}

// TestPoolConsentLossDeletesUnclaimed: when the admin removes the owner or
// the template, or the owner gains a role, the pool deletes every unclaimed
// member, because each one holds the owner's token. Bound and used members
// stay.
func TestPoolConsentLossDeletesUnclaimed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		lose         func(t *testing.T, e *poolMemberEnv)
	}{
		{"template removed", "TemplateNotAllowed", func(t *testing.T, e *poolMemberEnv) { e.allowOwner(t, e.tester.String(), "default.other") }},
		{"owner removed", "OwnerNotAllowed", func(t *testing.T, e *poolMemberEnv) { e.allowOwner(t, uuid.NewString(), "default.docker") }},
		{"owner gains an admin role", "OwnerNotEligible", func(_ *testing.T, e *poolMemberEnv) {
			e.fake.updateUser(e.tester, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner}} })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPoolMemberEnv(t)
			e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(4)) })
			ready := e.createMember(t, nil)
			e.setMemberStatus(t, ready, withWorkspace(readyStatus))
			pending := e.createMember(t, nil)
			used := e.createMember(t, nil)
			e.setMemberStatus(t, used, withWorkspace(usedStatus))
			bound := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
				m.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1", UID: uuid.NewString()}
			})
			e.setMemberStatus(t, bound, withWorkspace(readyStatus))
			tc.lose(t, e)

			pool := e.reconcilePool(t, 1)
			require.Equal(t, tc.reason, pool.Status.Reason, pool.Status.Message)
			require.True(t, e.gone(t, ready))
			require.True(t, e.gone(t, pending))
			require.False(t, e.gone(t, used))
			require.False(t, e.gone(t, bound))
			require.False(t, meta.IsStatusConditionTrue(pool.Status.Conditions, coderv1alpha1.CoderWorkspacePoolConditionReady))
		})
	}
}
