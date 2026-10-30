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

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func requireUsed(t *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, used bool) {
	t.Helper()
	require.Equal(t, used, meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed), member.Status.Message)
}

// TestPoolMemberWatchesUse is mutation check 9: Coder sets last_used_at when
// it serves a create from one of its prebuilds, so only a value later than
// the one at ready is use. A used member never fails and is never extended.
func TestPoolMemberWatchesUse(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 0)
	started := e.clock.Now()
	key, workspaceID, buildID := e.createdMember(t, nil)
	// A claimed prebuild: last_used_at is the claim, after created_at.
	e.fake.markUsed(workspaceID, started.Add(2*time.Minute))
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	member := e.reconcileMember(t, key, 1)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, member.Status.Phase, member.Status.Message)
	e.clock.SetTime(started.Add(10 * time.Minute))
	requireUsed(t, e.reconcileMember(t, key, 2), false)

	// Use after ready: Used=True, and the later rules never fail the member.
	e.fake.markUsed(workspaceID, started.Add(11*time.Minute+500*time.Millisecond))
	e.fake.addExternalBuild(workspaceID, uuid.New(), codersdk.WorkspaceTransitionStop)
	e.clock.SetTime(started.Add(50 * time.Minute))
	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Used", "without a claim")
	requireUsed(t, member, true)
	require.Zero(t, e.lastStep.RequeueAfter)
	reads := e.fake.requestCount(routeWorkspace)
	requireMember(t, e.reconcileMember(t, key, 2), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Used", "without a claim")
	require.Equal(t, reads, e.fake.requestCount(routeWorkspace), "a used member reads nothing")
	require.Zero(t, e.fake.requestCount(routeExtendWorkspace))
}

// TestPoolMemberTemplateWideWrite (#349): a template schedule update with
// update_workspace_last_used_at writes one last_used_at to every workspace of
// the template. Coder shows nothing that tells it apart from a usage flush
// that reached every workspace of the template, so it counts as use: every
// unclaimed member becomes Used.
func TestPoolMemberTemplateWideWrite(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	started := e.clock.Now()
	first, _ := e.readyMember(t)
	second, _ := e.readyMember(t)
	e.clock.SetTime(started.Add(10 * time.Minute))
	e.fake.touchTemplateWorkspaces(e.tplID, e.clock.Now())
	for _, key := range []types.NamespacedName{first, second} {
		member := e.reconcileMember(t, key, 1)
		requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Used", "without a claim")
		requireUsed(t, member, true)
	}
}

// TestPoolMemberUseWithNewReadySiblings (#349): after each usage flush, a new
// sibling becomes ready with that flush's value as its baseline. The member
// is Used from the first flush on, and its workspace is never deleted.
func TestPoolMemberUseWithNewReadySiblings(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	started := e.clock.Now()
	key, workspaceID := e.readyMember(t)
	used := []uuid.UUID{workspaceID}
	for tick := range 3 {
		siblingKey, siblingID, siblingBuild := e.createdMember(t, nil)
		used = append(used, siblingID)
		flush := started.Add(time.Duration(10+tick)*time.Minute + 300*time.Millisecond)
		for _, id := range used {
			e.fake.markUsed(id, flush)
		}
		e.fake.setBuildJob(siblingBuild, codersdk.ProvisionerJobSucceeded)
		e.fake.setAgents(siblingBuild, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
		e.clock.SetTime(flush.Add(10 * time.Second))
		sibling := e.reconcileMember(t, siblingKey, 1)
		require.True(t, sibling.Status.ReadyLastUsedAt.Time.Equal(flush.Truncate(time.Microsecond)), "the sibling's baseline is the flush")
		requireUsed(t, e.reconcileMember(t, key, 1), true)
	}
	e.deleteMember(t, key)
	require.Nil(t, e.reconcileMember(t, key, 1), "the member goes without deleting its workspace")
	require.Zero(t, e.fake.requestCount(routeCreateBuild), "no delete build")
}

// TestPoolMemberUseInBaselineSecond (#349): use later in the same second as
// the baseline value is still use, and the workspace is never deleted.
func TestPoolMemberUseInBaselineSecond(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key, workspaceID, buildID := e.createdMember(t, nil)
	second := e.clock.Now().Add(10 * time.Minute).Truncate(time.Second)
	e.fake.markUsed(workspaceID, second.Add(300*time.Millisecond))
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	e.clock.SetTime(second.Add(500 * time.Millisecond))
	member := e.reconcileMember(t, key, 1)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, member.Status.Phase, member.Status.Message)
	requireUsed(t, member, false)
	e.fake.markUsed(workspaceID, second.Add(800*time.Millisecond))
	e.clock.SetTime(second.Add(time.Minute))
	requireUsed(t, e.reconcileMember(t, key, 1), true)
	e.deleteMember(t, key)
	require.Nil(t, e.reconcileMember(t, key, 1), "the member goes without deleting its workspace")
	require.Zero(t, e.fake.requestCount(routeCreateBuild), "no delete build")
}

// TestPoolMemberWatchRules: an agent that stays not ready for 5 minutes, and
// a dormant workspace, fail an unclaimed ready member.
func TestPoolMemberWatchRules(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	started := e.clock.Now()
	key, _ := e.readyMember(t)
	buildID := uuid.MustParse(e.reconcileMember(t, key, 0).Status.StartBuildID)
	ready := agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady)
	lost := agent("main", codersdk.WorkspaceAgentDisconnected, codersdk.WorkspaceAgentLifecycleReady)
	requireNotReady := func(t *testing.T, at time.Duration) {
		t.Helper()
		e.clock.SetTime(started.Add(at))
		member := e.reconcileMember(t, key, 1)
		requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "AgentNotReady", lost.ID.String())
		require.False(t, meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady))
	}

	// A short disconnect only makes the member not ready.
	e.fake.setAgents(buildID, lost)
	requireNotReady(t, time.Minute)
	e.fake.setAgents(buildID, ready)
	e.clock.SetTime(started.Add(3 * time.Minute))
	member := e.reconcileMember(t, key, 1)
	requireReady(t, member, "is ready")
	require.True(t, meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady))

	// The 5 minutes count from the first read that found the agent lost.
	e.fake.setAgents(buildID, lost)
	requireNotReady(t, 4*time.Minute)
	requireNotReady(t, 9*time.Minute-time.Second)
	e.clock.SetTime(started.Add(9 * time.Minute))
	requireMemberFailed(t, e.reconcileMember(t, key, 1), "AgentLost", "has not been ready since", metav1.ConditionFalse)

	// Dormancy fails the member.
	e.clock.SetTime(started)
	key, workspaceID := e.readyMember(t)
	dormant := started.Add(time.Minute)
	e.fake.setDormant(workspaceID, &dormant, nil)
	requireMemberFailed(t, e.reconcileMember(t, key, 1), "Dormant", "is dormant", metav1.ConditionFalse)
}
