package controller_test

import (
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// readyMember runs a member up to a ready workspace. The start build
// finishes at the test clock's current time.
func (e *poolMemberEnv) readyMember(t *testing.T) (types.NamespacedName, uuid.UUID) {
	t.Helper()
	key, workspaceID, buildID := e.createdMember(t, nil)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	member := e.reconcileMember(t, key, 1)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, member.Status.Phase, member.Status.Message)
	return key, workspaceID
}

func (e *poolMemberEnv) deadline(t *testing.T, workspaceID uuid.UUID) codersdk.NullTime {
	t.Helper()
	ws, err := e.fake.client(t, 5*time.Second).Workspace(e.ctx, workspaceID)
	require.NoError(t, err)
	return ws.LatestBuild.Deadline
}

func requireReady(t *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, messagePart string) {
	t.Helper()
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, member.Status.Reason, messagePart)
}

func TestPoolMemberKeepsUnclaimedWorkspaceWarm(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 0)
	key, workspaceID := e.readyMember(t)
	started := e.clock.Now()

	requireReady(t, e.reconcileMember(t, key, 1), "")
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter)
	require.Zero(t, e.fake.requestCount(routeExtendWorkspace), "a deadline an hour away needs nothing")

	// 14 minutes before the deadline, the controller moves it to now + TTL.
	e.clock.SetTime(started.Add(46 * time.Minute))
	member := e.reconcileMember(t, key, 1)
	requireReady(t, member, "extended the deadline")
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace))
	require.True(t, member.Status.LastExtendTime.Time.Equal(e.clock.Now()))
	require.Equal(t, e.clock.Now().Add(time.Hour).UTC(), e.deadline(t, workspaceID).Time.UTC())
	requireReady(t, e.reconcileMember(t, key, 2), "")
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace), "the new deadline is an hour away")

	// The template's max deadline refuses the extension: the deadline stays.
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 70*time.Minute)
	e.clock.SetTime(started)
	key, workspaceID = e.readyMember(t)
	before := e.deadline(t, workspaceID)
	e.clock.SetTime(started.Add(46 * time.Minute))
	member = e.reconcileMember(t, key, 1)
	requireReady(t, member, "Coder refused to extend")
	require.Nil(t, member.Status.LastExtendTime)
	require.Equal(t, before, e.deadline(t, workspaceID))

	// Without a deadline, nothing is extended.
	e.fake.setTemplateSchedule(e.tplID, 0, 0)
	e.clock.SetTime(started)
	key, _ = e.readyMember(t)
	extends := e.fake.requestCount(routeExtendWorkspace)
	e.clock.SetTime(started.Add(10 * time.Hour))
	requireReady(t, e.reconcileMember(t, key, 1), "no autostop deadline")
	require.Equal(t, extends, e.fake.requestCount(routeExtendWorkspace))
}

// TestPoolMemberExtendsOnceAtBind is mutation check 12: after the one
// extension at bind, the member never extends again.
func TestPoolMemberExtendsOnceAtBind(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	bind := func(t *testing.T, key types.NamespacedName) {
		t.Helper()
		member := e.reconcileMember(t, key, 0)
		member.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1", UID: uuid.NewString()}
		require.NoError(t, k8sClient.Update(e.ctx, member))
	}
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 0)
	key, workspaceID := e.readyMember(t)
	e.clock.SetTime(e.clock.Now().Add(10 * time.Minute)) // 50 minutes left.
	bind(t, key)
	member := e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "bound to claim build-1")
	require.True(t, member.Status.BindExtended)
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace), "the bind extends even with 50 minutes left")
	require.Equal(t, e.clock.Now().Add(time.Hour).UTC(), e.deadline(t, workspaceID).Time.UTC())
	require.Zero(t, e.lastStep.RequeueAfter)

	e.clock.SetTime(e.clock.Now().Add(55 * time.Minute)) // 5 minutes left.
	requireMember(t, e.reconcileMember(t, key, 2), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "")
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace), "never a second extension after bind")

	// Without a deadline, the bind needs no extension.
	e.fake.setTemplateSchedule(e.tplID, 0, 0)
	e.clock.SetTime(time.Now().Truncate(time.Second))
	key, _ = e.readyMember(t)
	bind(t, key)
	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "no autostop deadline")
	require.True(t, member.Status.BindExtended)
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace))
}

// TestPoolMemberGuardsEveryExtension: the read right before an extension
// proves that the workspace is the member's own and still runs its start
// build. Otherwise the member never writes to it.
func TestPoolMemberGuardsEveryExtension(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 0)
	started := e.clock.Now()
	sdk := e.fake.client(t, 5*time.Second)
	other, err := sdk.CreateUserWorkspace(e.ctx, e.tester.String(), codersdk.CreateWorkspaceRequest{TemplateVersionID: e.v1, Name: "someone-elses"})
	require.NoError(t, err)
	e.fake.setBuildJob(other.LatestBuild.ID, codersdk.ProvisionerJobSucceeded)

	// A status writer points the member at another workspace of the owner.
	key, _ := e.readyMember(t)
	member := e.reconcileMember(t, key, 0)
	member.Status.WorkspaceID, member.Status.StartBuildID = other.ID.String(), other.LatestBuild.ID.String()
	require.NoError(t, k8sClient.Status().Update(e.ctx, member))
	e.clock.SetTime(started.Add(50 * time.Minute))
	member = e.reconcileMember(t, key, 1)
	requireMemberFailed(t, member, "OwnershipUnknown", "is not the workspace of this member", metav1.ConditionUnknown)

	// Someone stopped the workspace: its latest build is not the start build.
	e.clock.SetTime(started)
	key, workspaceID := e.readyMember(t)
	e.fake.addExternalBuild(workspaceID, uuid.New(), codersdk.WorkspaceTransitionStop)
	e.clock.SetTime(started.Add(50 * time.Minute))
	requireMemberFailed(t, e.reconcileMember(t, key, 1), "ChangedExternally", "Someone else started build 2", metav1.ConditionFalse)
	require.Zero(t, e.fake.requestCount(routeExtendWorkspace))
}
