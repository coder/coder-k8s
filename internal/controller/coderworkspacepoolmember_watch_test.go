package controller_test

import (
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// TestPoolMemberTemplateWideWrite is mutation check 10: a template schedule
// update sets one last_used_at on every workspace of the template. That is
// not use, even after the template changed again.
func TestPoolMemberTemplateWideWrite(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	started := e.clock.Now()
	first, _ := e.readyMember(t)
	second, secondWorkspace := e.readyMember(t)
	e.clock.SetTime(started.Add(10 * time.Minute))
	e.fake.touchTemplateWorkspaces(e.tplID, e.clock.Now())
	member := e.reconcileMember(t, first, 1)
	requireUsed(t, member, false)
	require.True(t, member.Status.ReadyLastUsedAt.Time.Equal(e.clock.Now()), "the baseline moves to the template-wide value")

	// The template changes again: the other member's baseline shows the write.
	e.fake.editTemplate(e.tplID, started.Add(time.Hour))
	requireUsed(t, e.reconcileMember(t, second, 1), false)

	// Real use is still use.
	e.fake.markUsed(secondWorkspace, started.Add(12*time.Minute))
	requireUsed(t, e.reconcileMember(t, second, 1), true)
	requireUsed(t, e.reconcileMember(t, first, 1), false)
}

// TestPoolMemberTemplateWideWriteWithoutSibling (#349): in a one-member pool
// no other member sees a template-wide write. After the template changes
// again, a stopped workspace of the template with the same last_used_at
// still shows the write: no usage flush can reach it.
func TestPoolMemberTemplateWideWriteWithoutSibling(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	started := e.clock.Now()
	key, workspaceID := e.readyMember(t)
	e.fake.addTemplateWorkspace(e.v1, codersdk.WorkspaceTransitionStop, started.Add(-time.Hour))
	written := started.Add(10*time.Minute + 250*time.Millisecond)
	e.fake.touchTemplateWorkspaces(e.tplID, written)
	e.fake.editTemplate(e.tplID, written.Add(30*time.Second))
	e.clock.SetTime(started.Add(11 * time.Minute))
	member := e.reconcileMember(t, key, 1)
	requireUsed(t, member, false)
	require.True(t, member.Status.ReadyLastUsedAt.Time.Equal(written.Truncate(time.Second)), "the baseline moves to the template-wide value")

	e.fake.markUsed(workspaceID, started.Add(12*time.Minute))
	requireUsed(t, e.reconcileMember(t, key, 1), true)
}

// TestPoolMemberUseNearTemplateEdit (#349): every template edit moves the
// template's updated_at, for example a version promote. Use right after an
// edit is still use when an idle workspace of the template stays below it,
// or when it is not in the second after the edit.
func TestPoolMemberUseNearTemplateEdit(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	started := e.clock.Now()
	key, workspaceID := e.readyMember(t)
	e.readyMember(t)
	edited := started.Add(10 * time.Minute)
	e.fake.editTemplate(e.tplID, edited)
	e.fake.markUsed(workspaceID, edited.Add(500*time.Millisecond))
	e.clock.SetTime(started.Add(11 * time.Minute))
	requireUsed(t, e.reconcileMember(t, key, 1), true)

	// Without another workspace of the template, only the time tells.
	for _, offset := range []time.Duration{-2 * time.Second, 2 * time.Second} {
		e := newPoolMemberEnv(t)
		started := e.clock.Now()
		key, workspaceID := e.readyMember(t)
		edited := started.Add(10 * time.Minute)
		e.fake.editTemplate(e.tplID, edited)
		e.fake.markUsed(workspaceID, edited.Add(offset))
		e.clock.SetTime(started.Add(11 * time.Minute))
		requireUsed(t, e.reconcileMember(t, key, 1), true)
	}
}

// TestPoolMemberUseSharedWithOtherWorkspace (#349): Coder's usage tracker
// writes one value to every workspace used since its last flush, and status
// keeps whole seconds. Another workspace with the same value is no witness
// of a template-wide write just for that.
func TestPoolMemberUseSharedWithOtherWorkspace(t *testing.T) {
	t.Parallel()
	// A sibling that became ready in the same second as the use.
	e := newPoolMemberEnv(t)
	started := e.clock.Now()
	key, workspaceID := e.readyMember(t)
	e.clock.SetTime(started.Add(10*time.Minute + 100*time.Millisecond))
	e.readyMember(t)
	e.fake.markUsed(workspaceID, started.Add(10*time.Minute+300*time.Millisecond))
	e.clock.SetTime(started.Add(11 * time.Minute))
	requireUsed(t, e.reconcileMember(t, key, 1), true)

	// A sibling whose baseline came from the same flush: someone used it
	// before it was ready. An idle member stays below the flush.
	e = newPoolMemberEnv(t)
	started = e.clock.Now()
	key, workspaceID = e.readyMember(t)
	e.readyMember(t)
	siblingKey, siblingWorkspace, siblingBuild := e.createdMember(t, nil)
	flush := started.Add(10*time.Minute + 300*time.Millisecond)
	e.fake.markUsed(siblingWorkspace, flush)
	e.fake.markUsed(workspaceID, flush)
	e.fake.setBuildJob(siblingBuild, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(siblingBuild, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	require.True(t, e.reconcileMember(t, siblingKey, 1).Status.ReadyLastUsedAt.Time.Equal(flush.Truncate(time.Second)))
	e.clock.SetTime(started.Add(11 * time.Minute))
	requireUsed(t, e.reconcileMember(t, key, 1), true)

	// A running workspace in use can share every flush with this member.
	e = newPoolMemberEnv(t)
	started = e.clock.Now()
	key, workspaceID = e.readyMember(t)
	flush = started.Add(10*time.Minute + 300*time.Millisecond)
	e.fake.addTemplateWorkspace(e.v1, codersdk.WorkspaceTransitionStart, flush)
	e.fake.markUsed(workspaceID, flush)
	e.clock.SetTime(started.Add(11 * time.Minute))
	requireUsed(t, e.reconcileMember(t, key, 1), true)
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
