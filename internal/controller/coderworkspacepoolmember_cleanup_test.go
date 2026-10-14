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

func (e *poolMemberEnv) deleteMember(t *testing.T, key types.NamespacedName) {
	t.Helper()
	require.NoError(t, k8sClient.Delete(e.ctx, e.reconcileMember(t, key, 0)))
}

func (e *poolMemberEnv) coderWrites() int {
	return e.fake.requestCount(routeCreateBuild) + e.fake.requestCount(routeCancelBuild) + e.fake.requestCount(routeExtendWorkspace)
}

// TestPoolMemberCleanupDeletesWorkspace: deleting a member deletes its
// workspace with the delete steps, then releases the finalizer.
func TestPoolMemberCleanupDeletesWorkspace(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key, _ := e.readyMember(t)
	e.deleteMember(t, key)
	member := e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "Deleting", "started a delete build")
	requireDeletedCondition(t, member, metav1.ConditionFalse, "Deleting")
	require.Equal(t, 1, e.fake.requestCount(routeCreateBuild))
	e.fake.setBuildJob(uuid.MustParse(member.Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	require.Nil(t, e.reconcileMember(t, key, 1), "the finalizer goes once the delete build succeeded")

	// A bound member is deleted too, even after the claimant used it.
	key, workspaceID := e.readyMember(t)
	member = e.reconcileMember(t, key, 0)
	member.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1", UID: uuid.NewString()}
	require.NoError(t, k8sClient.Update(e.ctx, member))
	e.fake.markUsed(workspaceID, e.clock.Now().Add(time.Minute))
	e.deleteMember(t, key)
	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "Deleting", "started a delete build")
}

// TestPoolMemberCleanupGuardsDelete is mutation check 3: before any delete
// write, the member proves that the workspace and its start build are its
// own. Otherwise it keeps the finalizer and never touches the workspace.
func TestPoolMemberCleanupGuardsDelete(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	sdk := e.fake.client(t, 5*time.Second)
	other, err := sdk.CreateUserWorkspace(e.ctx, e.tester.String(), codersdk.CreateWorkspaceRequest{TemplateVersionID: e.v1, Name: "someone-elses"})
	require.NoError(t, err)
	e.fake.setBuildJob(other.LatestBuild.ID, codersdk.ProvisionerJobSucceeded)
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, workspaceID uuid.UUID)
	}{
		{name: "another workspace of the owner", edit: func(_ *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, _ uuid.UUID) {
			member.Status.WorkspaceID, member.Status.StartBuildID = other.ID.String(), other.LatestBuild.ID.String()
		}},
		{name: "a start build by someone else", edit: func(_ *testing.T, member *coderv1alpha1.CoderWorkspacePoolMember, workspaceID uuid.UUID) {
			build := e.fake.addExternalBuild(workspaceID, e.tester, codersdk.WorkspaceTransitionStart)
			e.fake.setBuildJob(build.ID, codersdk.ProvisionerJobSucceeded)
			member.Status.StartBuildID = build.ID.String()
		}},
	} {
		key, workspaceID := e.readyMember(t)
		member := e.reconcileMember(t, key, 0)
		tc.edit(t, member, workspaceID)
		require.NoError(t, k8sClient.Status().Update(e.ctx, member))
		e.deleteMember(t, key)
		writes := e.coderWrites()
		member = e.reconcileMember(t, key, 1)
		requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "OwnershipUnknown", "never deletes it")
		requireDeletedCondition(t, member, metav1.ConditionUnknown, "OwnershipUnknown")
		require.Zero(t, e.lastStep.RequeueAfter, tc.name)
		reads := e.fake.totalRequests()
		require.NotNil(t, e.reconcileMember(t, key, 2), "%s: the finalizer stays", tc.name)
		require.Equal(t, reads, e.fake.totalRequests(), "%s: no Coder call after OwnershipUnknown", tc.name)
		require.Equal(t, writes, e.coderWrites(), "%s: no Coder write", tc.name)
	}

	// A person releases a member by removing its finalizer. While another
	// finalizer keeps the object, the controller still never touches the
	// workspace.
	key, _ := e.readyMember(t)
	member := e.reconcileMember(t, key, 0)
	member.Finalizers = append(member.Finalizers, "test.coder.com/hold")
	require.NoError(t, k8sClient.Update(e.ctx, member))
	e.deleteMember(t, key)
	member = e.reconcileMember(t, key, 0)
	member.Finalizers = []string{"test.coder.com/hold"}
	require.NoError(t, k8sClient.Update(e.ctx, member))
	requests := e.fake.totalRequests()
	member = e.reconcileMember(t, key, 2)
	require.Equal(t, requests, e.fake.totalRequests(), "no Coder call after the finalizer is gone")
	member.Finalizers = nil
	require.NoError(t, k8sClient.Update(e.ctx, member))
}

// TestPoolMemberCleanupLastUseCheck is mutation check 11: before the first
// delete build of an unclaimed member, for example after its pool was
// deleted, the member reads last_used_at again. A used workspace stays in
// Coder, and the member releases its finalizer without a Coder write.
func TestPoolMemberCleanupLastUseCheck(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key, workspaceID := e.readyMember(t)
	e.fake.markUsed(workspaceID, e.clock.Now().Add(time.Minute))
	e.deleteMember(t, key)
	require.Nil(t, e.reconcileMember(t, key, 1))
	require.Zero(t, e.coderWrites(), "a used workspace is never deleted")

	// A template-wide write is not use: the workspace is deleted.
	key, _ = e.readyMember(t)
	e.clock.SetTime(e.clock.Now().Add(2 * time.Minute))
	e.fake.touchTemplateWorkspaces(e.tplID, e.clock.Now())
	e.deleteMember(t, key)
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "Deleting", "started a delete build")
}

// TestPoolMemberCleanupWithoutWorkspaceID: a member deleted while its create
// outcome is unknown confirms it by reads first, and a member whose control
// plane is gone is released without a Coder call.
func TestPoolMemberCleanupWithoutWorkspaceID(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	// The create took effect, but the answer was lost: the reads adopt the
	// workspace, then the member deletes it.
	key := e.uncertainMember(t, fakeFault{Status: 502, AfterCommit: true}, nil)
	e.deleteMember(t, key)
	member := e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "Deleting", "adopted workspace")
	require.NotEmpty(t, member.Status.WorkspaceID)
	e.reconcileMember(t, key, 1)
	require.Equal(t, 1, e.fake.requestCount(routeCancelBuild), "the start build is canceled before the delete build")

	// No workspace appears: within the settle window the member reads again,
	// after it the site owner's 404 proves that none exists.
	key = e.uncertainMember(t, fakeFault{Status: 502}, nil)
	marker := e.reconcileMember(t, key, 0).Status.CreateAttemptTime.Time
	e.deleteMember(t, key)
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, "ConfirmingCreate", "")
	require.Equal(t, 5*time.Second, e.lastStep.RequeueAfter)
	e.clock.SetTime(marker.Add(15 * time.Minute))
	require.Nil(t, e.reconcileMember(t, key, 1))

	// The control plane is gone: nothing can reach Coder.
	e.clock.SetTime(time.Now().Truncate(time.Second))
	key, _ = e.readyMember(t)
	e.deleteMember(t, key)
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	require.NoError(t, k8sClient.Delete(e.ctx, cp))
	requests := e.fake.totalRequests()
	require.Nil(t, e.reconcileMember(t, key, 1))
	require.Equal(t, requests, e.fake.totalRequests())
}
