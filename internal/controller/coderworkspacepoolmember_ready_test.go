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

// createdMember runs a member up to its created workspace and returns the
// member key, the workspace ID, and the start build ID.
func (e *poolMemberEnv) createdMember(t *testing.T, edit func(*coderv1alpha1.CoderWorkspacePoolMember)) (types.NamespacedName, uuid.UUID, uuid.UUID) {
	t.Helper()
	key := e.createMember(t, edit)
	member := e.reconcileMember(t, key, 2)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForBuild", "was created")
	return key, uuid.MustParse(member.Status.WorkspaceID), uuid.MustParse(member.Status.StartBuildID)
}

// uncertainMember runs a member up to a create request that fails with fault.
func (e *poolMemberEnv) uncertainMember(t *testing.T, fault fakeFault, edit func(*coderv1alpha1.CoderWorkspacePoolMember)) types.NamespacedName {
	t.Helper()
	key := e.createMember(t, edit)
	e.reconcileMember(t, key, 1) // Finalizer.
	e.fake.failNext(routeCreateWorkspace, fault)
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "ConfirmingCreate", "")
	return key
}

func requireDeletedCondition(t *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted)
	require.NotNil(t, condition)
	require.Equal(t, status, condition.Status)
	require.Equal(t, reason, condition.Reason)
}

func TestPoolMemberBecomesReady(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key, workspaceID, buildID := e.createdMember(t, nil)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	starting := agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleStarting)
	e.fake.setAgents(buildID, starting)
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForAgents", starting.ID.String())
	// The same wait, when the read ends at the deadline, fails the member.
	other, _, _ := e.createdMember(t, nil)
	otherDeadline := e.reconcileMember(t, other, 0).CreationTimestamp.Add(900 * time.Second)
	late := &controller.CoderWorkspacePoolMemberReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: &lateClock{times: []time.Time{otherDeadline.Add(-time.Second), otherDeadline}}}
	_, err := late.Reconcile(e.ctx, ctrl.Request{NamespacedName: other})
	require.NoError(t, err)
	requireMemberFailed(t, e.reconcileMember(t, other, 0), "DeadlineExceeded", "Last wait: WaitingForBuild", metav1.ConditionFalse)

	lastUsed := e.clock.Now().Add(-time.Hour)
	e.fake.markUsed(workspaceID, lastUsed)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	// The read that finds the workspace ready ends at the deadline: it stays ready.
	member := e.reconcileMember(t, key, 0)
	deadline := member.CreationTimestamp.Add(900 * time.Second)
	r := &controller.CoderWorkspacePoolMemberReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: &lateClock{times: []time.Time{deadline.Add(-time.Second), deadline}}}
	result, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Equal(t, time.Minute, result.RequeueAfter, "a ready member reads its workspace every minute")
	member = e.reconcileMember(t, key, 0)
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, member.Status.Phase, member.Status.Message)
	require.Equal(t, "WorkspaceReady", member.Status.Reason)
	require.True(t, meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady))
	require.True(t, member.Status.ReadyTime.Time.Equal(deadline.Add(-time.Second)))
	require.True(t, member.Status.ReadyLastUsedAt.Time.Equal(lastUsed.Truncate(time.Microsecond)), "the baseline is last_used_at at ready")

	// Past the deadline, a ready member stays ready.
	e.clock.SetTime(member.CreationTimestamp.Add(time.Hour))
	require.Equal(t, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, e.reconcileMember(t, key, 2).Status.Phase)
}

func TestPoolMemberReadinessFailures(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	sdk := e.fake.client(t, 5*time.Second)
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, key types.NamespacedName, workspaceID, buildID uuid.UUID)
		reason  string
		message string
		deleted metav1.ConditionStatus
	}{
		{name: "build failed without an error code", setup: func(_ *testing.T, _ types.NamespacedName, _, buildID uuid.UUID) {
			e.fake.setBuildJob(buildID, codersdk.ProvisionerJobFailed)
		}, reason: "BuildFailed", message: "failed.", deleted: metav1.ConditionFalse},
		{name: "external stop", setup: func(_ *testing.T, _ types.NamespacedName, workspaceID, buildID uuid.UUID) {
			e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
			e.fake.addExternalBuild(workspaceID, uuid.New(), codersdk.WorkspaceTransitionStop)
		}, reason: "ChangedExternally", message: "Someone else started build 2", deleted: metav1.ConditionFalse},
		{name: "deleted externally", setup: func(_ *testing.T, _ types.NamespacedName, workspaceID, _ uuid.UUID) {
			e.fake.markDeleted(workspaceID)
		}, reason: "WorkspaceDeletedExternally", message: "deleted by someone else", deleted: metav1.ConditionTrue},
		// A status writer points the member at another workspace of the owner.
		{name: "another workspace in status", setup: func(t *testing.T, key types.NamespacedName, _, _ uuid.UUID) {
			other, err := sdk.CreateUserWorkspace(e.ctx, e.tester.String(), codersdk.CreateWorkspaceRequest{TemplateVersionID: e.v1, Name: "someone-elses"})
			require.NoError(t, err)
			member := e.reconcileMember(t, key, 0)
			member.Status.WorkspaceID, member.Status.StartBuildID = other.ID.String(), other.LatestBuild.ID.String()
			require.NoError(t, k8sClient.Status().Update(e.ctx, member))
		}, reason: "OwnershipUnknown", message: "is not the workspace of this member", deleted: metav1.ConditionUnknown},
		// A status writer names a start build that the owner, not the
		// operator, started on the member's own kwp- workspace.
		{name: "start build by someone else in status", setup: func(t *testing.T, key types.NamespacedName, workspaceID, _ uuid.UUID) {
			build := e.fake.addExternalBuild(workspaceID, e.tester, codersdk.WorkspaceTransitionStart)
			e.fake.setBuildJob(build.ID, codersdk.ProvisionerJobSucceeded)
			e.fake.setAgents(build.ID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
			member := e.reconcileMember(t, key, 0)
			member.Status.StartBuildID = build.ID.String()
			require.NoError(t, k8sClient.Status().Update(e.ctx, member))
		}, reason: "OwnershipUnknown", message: "Nothing proves that the controller started build 2", deleted: metav1.ConditionUnknown},
	} {
		key, workspaceID, buildID := e.createdMember(t, nil)
		tc.setup(t, key, workspaceID, buildID)
		writes := e.fake.requestCount(routeCreateBuild) + e.fake.requestCount(routeCancelBuild)
		member := e.reconcileMember(t, key, 1)
		requireMemberFailed(t, member, tc.reason, tc.message, tc.deleted)
		if tc.reason == "BuildFailed" {
			// Without an error code, the message names no code (a mutation survivor of #255).
			require.Equal(t, "The start build of workspace "+member.Status.WorkspaceName+" failed.", member.Status.Message)
		}
		requireDeletedCondition(t, member, tc.deleted, map[metav1.ConditionStatus]string{
			metav1.ConditionFalse: "CleanupPending", metav1.ConditionTrue: "DeletedExternally", metav1.ConditionUnknown: "OwnershipUnknown",
		}[tc.deleted])
		require.Equal(t, writes, e.fake.requestCount(routeCreateBuild)+e.fake.requestCount(routeCancelBuild), "%s: no Coder write", tc.name)
	}
}

// Only Coder's own 404 proves that no workspace of the member exists. A
// proxy's 404 page, or "Route not found.", keeps the marker and waits, also
// after the settle window (#351).
func TestPoolMemberConfirmTrustsOnlyCodersNotFound(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	long := func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Spec.ReadyTimeoutSeconds = 7200 }
	for _, tc := range nonCoderNotFounds {
		t.Run(tc.name, func(t *testing.T) { // Not parallel: the cases share e and its clock.
			creates := e.fake.requestCount(routeCreateWorkspace)
			key := e.uncertainMember(t, fakeFault{Status: 502}, long)
			marker := e.reconcileMember(t, key, 0).Status.CreateAttemptTime.Time
			e.clock.SetTime(marker.Add(15 * time.Minute))
			e.fake.failNext(routeWorkspaceByName, tc.fault)
			member := e.reconcileMember(t, key, 1)
			requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "CoderUnavailable", "get workspace by name: Coder answered 404")
			require.NotNil(t, member.Status.CreateAttemptTime, "the marker stays")
			// Coder's own 404 after the window proves that no workspace exists.
			requireMemberFailed(t, e.reconcileMember(t, key, 1), "CreateOutcomeUnknown", "appeared within 15m0s", metav1.ConditionTrue)
			require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace), "never a second create request")
		})
	}
}

func TestPoolMemberConfirmsCreate(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	sdk := e.fake.client(t, 5*time.Second)
	workspaceOf := func(t *testing.T, key types.NamespacedName) codersdk.Workspace {
		t.Helper()
		ws, err := sdk.WorkspaceByOwnerAndName(e.ctx, e.tester.String(), e.reconcileMember(t, key, 0).Status.WorkspaceName, codersdk.WorkspaceOptions{})
		require.NoError(t, err)
		return ws
	}
	afterCommit := fakeFault{Status: 504, AfterCommit: true}

	// A create that Coder served from one of its prebuilds: the claim build counts.
	key := e.uncertainMember(t, afterCommit, nil)
	ws := workspaceOf(t, key)
	claim := e.fake.claimPrebuild(ws.ID)
	member := e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "WaitingForBuild", "was created")
	require.Equal(t, claim.ID.String(), member.Status.StartBuildID)
	require.Equal(t, ws.ID.String(), member.Status.WorkspaceID)

	// Deleted by someone else before the member saw it.
	key = e.uncertainMember(t, afterCommit, nil)
	e.fake.markDeleted(workspaceOf(t, key).ID)
	member = e.reconcileMember(t, key, 1)
	requireMemberFailed(t, member, "WorkspaceDeletedExternally", "", metav1.ConditionTrue)
	requireDeletedCondition(t, member, metav1.ConditionTrue, "DeletedExternally")

	// Without provenance the member never adopts the workspace.
	key = e.uncertainMember(t, afterCommit, nil)
	e.fake.failNext(routeWorkspaceBuilds, fakeFault{Rewrite: func(a any) any {
		builds := a.([]codersdk.WorkspaceBuild)
		for i := range builds {
			builds[i].InitiatorID = uuid.New()
		}
		return builds
	}})
	member = e.reconcileMember(t, key, 1)
	requireMemberFailed(t, member, "WorkspaceNameConflict", "nothing proves", metav1.ConditionUnknown)
	requireDeletedCondition(t, member, metav1.ConditionUnknown, "OwnershipUnknown")
	require.Empty(t, member.Status.WorkspaceID)

	// A start build without the pinned preset is not the requested build.
	preset := e.fake.addPreset(e.v1, "small")
	e.setPoolSelectors(t, "small")
	e.resolve(t, func(r *coderv1alpha1.CoderWorkspacePoolResolved) { r.PresetID = preset.String() })
	key = e.uncertainMember(t, afterCommit, nil)
	e.fake.failNext(routeWorkspaceBuilds, fakeFault{Rewrite: func(a any) any {
		builds := a.([]codersdk.WorkspaceBuild)
		for i := range builds {
			builds[i].TemplateVersionPresetID = nil
		}
		return builds
	}})
	requireMemberFailed(t, e.reconcileMember(t, key, 1), "WorkspaceNameConflict", "nothing proves", metav1.ConditionUnknown)
	e.setPoolSelectors(t, "")
	e.resolve(t, nil)

	// Nothing appears: the member reads again until the settle window ends.
	long := func(m *coderv1alpha1.CoderWorkspacePoolMember) { m.Spec.ReadyTimeoutSeconds = 7200 }
	key = e.uncertainMember(t, fakeFault{Status: 502}, long)
	marker := e.reconcileMember(t, key, 0).Status.CreateAttemptTime.Time
	e.clock.SetTime(marker.Add(15*time.Minute - time.Second))
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "ConfirmingCreate", "Reading again")
	// Only a site owner's 404 proves that no workspace exists.
	e.clock.SetTime(marker.Add(15 * time.Minute))
	e.fake.updateUser(e.fake.operatorID, func(u *codersdk.User) { u.Roles = nil })
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating, "ConfirmingCreate", "not a site owner")
	e.fake.updateUser(e.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner}} })
	member = e.reconcileMember(t, key, 1)
	requireMemberFailed(t, member, "CreateOutcomeUnknown", "appeared within 15m0s", metav1.ConditionTrue)
	require.NoError(t, k8sClient.Delete(e.ctx, member))
	require.Nil(t, e.reconcileMember(t, key, 1), "a member proven to have no workspace is released")
	require.Equal(t, 5, e.fake.requestCount(routeCreateWorkspace), "one create request per member, never a second")
}
