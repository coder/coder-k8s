package controller_test

import (
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// reconcilePool runs n pool reconciles, each with a new reconciler, and
// returns the stored pool.
func (e *poolMemberEnv) reconcilePool(t *testing.T, n int) *coderv1alpha1.CoderWorkspacePool {
	t.Helper()
	key := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	for range n {
		r := &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
		var err error
		e.lastStep, err = r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	pool := &coderv1alpha1.CoderWorkspacePool{}
	require.NoError(t, k8sClient.Get(e.ctx, key, pool))
	return pool
}

func (e *poolMemberEnv) editPool(t *testing.T, edit func(*coderv1alpha1.CoderWorkspacePoolSpec)) {
	t.Helper()
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}, e.pool))
	edit(&e.pool.Spec)
	require.NoError(t, k8sClient.Update(e.ctx, e.pool))
}

// setMemberStatus edits a member's status as its reconciler would.
func (e *poolMemberEnv) setMemberStatus(t *testing.T, key types.NamespacedName, edit func(*coderv1alpha1.CoderWorkspacePoolMemberStatus)) {
	t.Helper()
	member := e.reconcileMember(t, key, 0)
	edit(&member.Status)
	require.NoError(t, k8sClient.Status().Update(e.ctx, member))
}

func memberPhase(phase string, conditions ...metav1.Condition) func(*coderv1alpha1.CoderWorkspacePoolMemberStatus) {
	return func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
		s.Phase = phase
		for _, c := range conditions {
			c.LastTransitionTime = metav1.Now()
			meta.SetStatusCondition(&s.Conditions, c)
		}
	}
}

func memberCondition(conditionType string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: conditionType, Status: status, Reason: reason, Message: reason}
}

var readyStatus = memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseReady,
	memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionReady, metav1.ConditionTrue, "WorkspaceReady"))

func requirePool(t *testing.T, pool *coderv1alpha1.CoderWorkspacePool, reason string, ready, stalled bool) {
	t.Helper()
	require.Equal(t, reason, pool.Status.Reason, pool.Status.Message)
	require.Equal(t, pool.Generation, pool.Status.ObservedGeneration)
	for conditionType, want := range map[string]bool{
		coderv1alpha1.CoderWorkspacePoolConditionReady:       ready,
		coderv1alpha1.CoderWorkspacePoolConditionStalled:     stalled,
		coderv1alpha1.CoderWorkspacePoolConditionReconciling: !ready && !stalled,
	} {
		require.Equal(t, want, meta.IsStatusConditionTrue(pool.Status.Conditions, conditionType), "%s: %s", conditionType, pool.Status.Message)
	}
}

// TestPoolResolvesInputs: the pool resolves the version, the preset, and the
// parameters hash, and publishes the selector (plan mutation 7).
func TestPoolResolvesInputs(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	presetID := e.fake.addPreset(e.v1, "small")
	e.pool.Status = coderv1alpha1.CoderWorkspacePoolStatus{}
	require.NoError(t, k8sClient.Status().Update(e.ctx, e.pool))
	e.setPoolSelectors(t, "small", coderv1alpha1.CoderWorkspacePoolParameter{Name: "region", Value: "eu"})

	pool := e.reconcilePool(t, 1)
	requirePool(t, pool, "WaitingForMembers", false, false)
	require.Equal(t, 15*time.Second, e.lastStep.RequeueAfter)
	require.Equal(t, "coder.com/workspace-pool-uid="+string(pool.UID), pool.Status.Selector, "HPA refuses an empty selector")
	resolved := pool.Status.Resolved
	require.NotNil(t, resolved)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolResolved{
		OrganizationID: e.orgID.String(), TemplateID: e.tplID.String(), TemplateVersionID: e.v1.String(), TemplateVersionName: "v1",
		PresetID: presetID.String(), OwnerID: e.tester.String(),
		ParametersHash: parametersHash(coderv1alpha1.CoderWorkspacePoolMemberParameter{Name: "region", Value: "eu"}),
		ResolvedTime:   resolved.ResolvedTime,
	}, *resolved)
	require.True(t, resolved.ResolvedTime.Time.Equal(e.clock.Now()))
	// The same inputs keep the time.
	e.clock.SetTime(e.clock.Now().Add(time.Minute))
	require.Equal(t, resolved.ResolvedTime, e.reconcilePool(t, 1).Status.Resolved.ResolvedTime)

	// With version.active, the pool follows a promotion.
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) {
		s.Version, s.PresetName = coderv1alpha1.CoderWorkspacePoolVersion{Active: new(true)}, ""
	})
	v2 := e.fake.addVersion(e.tplID, "v2", codersdk.ProvisionerJobSucceeded)
	e.fake.promoteVersion(v2)
	pool = e.reconcilePool(t, 1)
	requirePool(t, pool, "WaitingForMembers", false, false)
	require.Equal(t, v2.String(), pool.Status.Resolved.TemplateVersionID)
	require.Equal(t, "v2", pool.Status.Resolved.TemplateVersionName)
	require.Empty(t, pool.Status.Resolved.PresetID)
	require.True(t, pool.Status.Resolved.ResolvedTime.Time.Equal(e.clock.Now()))
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter, "the pool reads the active version again every minute")
}

// TestPoolResolutionWaitsAndStalls: a refused or missing input stops new
// members and keeps the resolved inputs. Inputs that only a person can fix
// stall the pool.
func TestPoolResolutionWaitsAndStalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, e *poolMemberEnv)
		reason  string
		stalled bool
	}{
		{"owner not allowed", func(t *testing.T, e *poolMemberEnv) { e.allowOwner(t, uuid.NewString(), "default.docker") }, "OwnerNotAllowed", false},
		{"template not allowed", func(t *testing.T, e *poolMemberEnv) { e.allowOwner(t, e.tester.String(), "default.other") }, "TemplateNotAllowed", false},
		{"owner not eligible", func(_ *testing.T, e *poolMemberEnv) {
			e.fake.updateUser(e.tester, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner}} })
		}, "OwnerNotEligible", false},
		{"version not found", func(t *testing.T, e *poolMemberEnv) {
			e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Version.Name = "nope" })
		}, "TemplateVersionNotFound", false},
		{"version importing", func(t *testing.T, e *poolMemberEnv) {
			e.fake.addVersion(e.tplID, "v3", codersdk.ProvisionerJobRunning)
			e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Version.Name = "v3" })
		}, "TemplateVersionImporting", false},
		{"preset not found", func(t *testing.T, e *poolMemberEnv) { e.setPoolSelectors(t, "missing") }, "PresetNotFound", false},
		{"version archived", func(_ *testing.T, e *poolMemberEnv) { e.fake.archiveVersion(e.v1) }, "TemplateVersionArchived", true},
		{"version import failed", func(t *testing.T, e *poolMemberEnv) {
			e.fake.addVersion(e.tplID, "bad", codersdk.ProvisionerJobFailed)
			e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Version.Name = "bad" })
		}, "TemplateVersionImportFailed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPoolMemberEnv(t)
			before := *e.pool.Status.Resolved
			tc.setup(t, e)
			pool := e.reconcilePool(t, 1)
			requirePool(t, pool, tc.reason, false, tc.stalled)
			require.Equal(t, before, *pool.Status.Resolved, "a refused input keeps the resolved inputs")
			if tc.stalled {
				require.Zero(t, e.lastStep.RequeueAfter)
			} else {
				require.Equal(t, time.Minute, e.lastStep.RequeueAfter)
			}
		})
	}
}

// TestPoolCountsMembers: the counts follow plan 5.3. Members that someone
// used and members whose agents are not ready never count as ready, so no
// claim or count relies on them.
func TestPoolCountsMembers(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(2)) })
	v2 := e.fake.addVersion(e.tplID, "v2", codersdk.ProvisionerJobSucceeded)
	notReady := memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseReady,
		memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionReady, metav1.ConditionFalse, "AgentNotReady"))
	used := memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseReady,
		memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionReady, metav1.ConditionTrue, "Used"),
		memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionUsed, metav1.ConditionTrue, "Used"))
	outdated := func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Spec.TemplateVersionID = v2.String() }
	bound := func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1", UID: uuid.NewString()}
	}
	foreign := func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.OwnerReferences = nil }
	held := func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Finalizers = []string{"test.coder.com/hold"} }
	for _, m := range []struct {
		edit   func(*coderv1alpha1.CoderWorkspacePoolMember)
		status func(*coderv1alpha1.CoderWorkspacePoolMemberStatus)
	}{
		{nil, readyStatus}, // Ready and current.
		{nil, notReady},    // An agent is not ready: not ready.
		{nil, used},        // Someone used it: not in the pool.
		{bound, readyStatus},
		{nil, memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed)},
		{nil, memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhasePending)},
		{outdated, readyStatus},
		{foreign, readyStatus}, // Not controlled by the pool.
		{held, readyStatus},    // Deleted below.
	} {
		key := e.createMember(t, m.edit)
		e.setMemberStatus(t, key, m.status)
	}
	members := &coderv1alpha1.CoderWorkspacePoolMemberList{}
	require.NoError(t, k8sClient.List(e.ctx, members))
	for i := range members.Items {
		if m := &members.Items[i]; len(m.Finalizers) > 0 && m.Namespace == e.ns {
			require.NoError(t, k8sClient.Delete(e.ctx, m))
			t.Cleanup(func() {
				m := e.reconcileMember(t, types.NamespacedName{Namespace: m.Namespace, Name: m.Name}, 0)
				m.Finalizers = nil
				_ = k8sClient.Update(e.ctx, m)
			})
		}
	}

	pool := e.reconcilePool(t, 1)
	requirePool(t, pool, "WaitingForMembers", false, false)
	st := pool.Status
	require.Equal(t, int32(4), st.Replicas, "ready, not ready, pending, and outdated")
	require.Equal(t, int32(2), st.ReadyReplicas, "ready and outdated ready, never Ready=False")
	require.Equal(t, int32(3), st.UpdatedReplicas, "ready, not ready, and pending")
	require.Equal(t, int32(1), st.BoundReplicas)
	require.Equal(t, int32(1), st.UsedReplicas)
}

// TestPoolReady: Ready=True needs spec.replicas current ready members and no
// outdated unclaimed member.
func TestPoolReady(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key := e.createMember(t, nil)
	e.setMemberStatus(t, key, readyStatus)
	pool := e.reconcilePool(t, 1)
	requirePool(t, pool, "Ready", true, false)
	require.Zero(t, e.lastStep.RequeueAfter)

	// An outdated member makes the pool reconcile until it is replaced.
	outdated := e.createMember(t, func(m *coderv1alpha1.CoderWorkspacePoolMember) {
		m.Spec.TemplateVersionID = e.fake.addVersion(e.tplID, "v2", codersdk.ProvisionerJobSucceeded).String()
	})
	e.setMemberStatus(t, outdated, readyStatus)
	requirePool(t, e.reconcilePool(t, 1), "WaitingForMembers", false, false)
}
