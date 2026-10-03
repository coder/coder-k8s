package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

func (e *templateTestEnv) get(t *testing.T, key types.NamespacedName) *coderv1alpha1.CoderTemplateTest {
	t.Helper()
	tt := &coderv1alpha1.CoderTemplateTest{}
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	return tt
}

func TestTemplateTestDeleteCancelsFirst(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)

	// A pending start build is canceled at once, then deleted.
	key, _, _ := e.startedTest(t)
	e.deleteTest(t, key)
	tt := e.reconcile(t, key, 2)
	requireDeleted(t, tt, metav1.ConditionFalse, "Deleting")
	require.Equal(t, 1, e.fake.requestCount(routeCancelBuild))
	require.Equal(t, 1, e.fake.requestCount(routeCreateBuild))
	require.Equal(t, coderv1alpha1.CoderTemplateTestPhaseRunning, tt.Status.Phase, "deletion never changes the phase")

	// At the deadline, a running start build moves to canceling. The
	// controller waits without a second cancel, then deletes.
	key, _, buildID := e.startedTest(t)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobRunning)
	e.clock.SetTime(e.clock.Now().Add(900 * time.Second))
	cancels, deletes := e.fake.requestCount(routeCancelBuild), e.fake.requestCount(routeCreateBuild)
	e.fake.failNext(routeCancelBuild, fakeFault{Status: 412}) // The job moved on: read again.
	requireTemplateTestFailedWith(t, e.settle(t, key), "DeadlineExceeded", metav1.ConditionFalse, "Deleting")
	require.Equal(t, cancels+2, e.fake.requestCount(routeCancelBuild), "a refused cancel is sent once more, then never again while canceling")
	require.Equal(t, deletes, e.fake.requestCount(routeCreateBuild), "no delete while the build cancels")
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobCanceled)
	e.fake.failNext(routeCreateBuild, fakeFault{Status: 409}) // Another build started: read again.
	e.settle(t, key)
	require.Equal(t, deletes+2, e.fake.requestCount(routeCreateBuild))
	require.NotEmpty(t, e.get(t, key).Status.DeleteBuildID)
}

func TestTemplateTestDeleteRetries(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.failedWithWorkspace(t)
	for _, tc := range []struct {
		attempt int32
		backoff time.Duration
	}{{1, time.Minute}, {2, 2 * time.Minute}} {
		attempt, backoff := tc.attempt, tc.backoff
		deletes := e.fake.requestCount(routeCreateBuild)
		e.fake.setBuildJob(uuid.MustParse(e.get(t, key).Status.DeleteBuildID), codersdk.ProvisionerJobFailed)
		tt := e.settle(t, key)
		requireDeleted(t, tt, metav1.ConditionFalse, "DeleteRetrying")
		require.Equal(t, attempt, tt.Status.DeleteAttempts, "each failed delete build counts once")
		require.Equal(t, backoff, e.lastStep.RequeueAfter)
		require.Equal(t, deletes, e.fake.requestCount(routeCreateBuild), "no new delete build before the backoff")
		e.clock.SetTime(e.clock.Now().Add(backoff))
		requireDeleted(t, e.reconcile(t, key, 1), metav1.ConditionFalse, "Deleting")
		require.Equal(t, deletes+1, e.fake.requestCount(routeCreateBuild), "a failed build is never adopted again")
	}
	e.fake.setBuildJob(uuid.MustParse(e.get(t, key).Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	tt := e.settle(t, key)
	requireTemplateTestFailedWith(t, tt, "BuildFailed", metav1.ConditionTrue, "Deleted")
}

func TestTemplateTestFailedDeleteFailsThePass(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key, _, buildID := e.startedTest(t)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	tt := e.settle(t, key)
	requireTemplateTestRunning(t, tt, "DeletingWorkspace", "")
	require.Equal(t, 1, e.fake.requestCount(routeCreateBuild))

	e.fake.setBuildJob(uuid.MustParse(tt.Status.DeleteBuildID), codersdk.ProvisionerJobFailed)
	tt = e.reconcile(t, key, 1)
	requireTemplateTestFailedWith(t, tt, "DeleteBuildFailed", metav1.ConditionFalse, "DeleteRetrying")
	require.NotNil(t, tt.Status.AgentsReadyTime, "the agents were ready")

	// Cleanup after a failure never turns it into a success.
	e.clock.SetTime(e.clock.Now().Add(time.Minute))
	e.reconcile(t, key, 1)
	e.fake.setBuildJob(uuid.MustParse(e.get(t, key).Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	requireTemplateTestFailedWith(t, e.settle(t, key), "DeleteBuildFailed", metav1.ConditionTrue, "Deleted")
}

// passedAndDeleting runs a test to a pass whose delete build is pending.
func (e *templateTestEnv) passedAndDeleting(t *testing.T) (types.NamespacedName, *coderv1alpha1.CoderTemplateTest) {
	t.Helper()
	key, _, buildID := e.startedTest(t)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	tt := e.settle(t, key)
	requireTemplateTestRunning(t, tt, "DeletingWorkspace", "")
	require.NotEmpty(t, tt.Status.DeleteBuildID)
	return key, tt
}

// TestTemplateTestDeleteAfterPassFailures checks the delete after a pass: a
// 429 keeps the rate-limit backoff (r4173060957), every failed delete build
// fails the test (r4173060960), and the deadline still applies.
func TestTemplateTestDeleteAfterPassFailures(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)

	// A 429: the jittered 2 s backoff, not the 5 s poll.
	key, _ := e.passedAndDeleting(t)
	e.fake.failNext(routeWorkspace, fakeFault{Status: 429})
	requireTemplateTestRunning(t, e.reconcile(t, key, 1), "CoderUnavailable", "")
	require.InDelta(t, 2*time.Second, e.lastStep.RequeueAfter, float64(400*time.Millisecond))

	// A status write failed after the delete build was created: the stored
	// status has no delete build ID, and the build then fails.
	tt := e.get(t, key)
	deleteBuildID := uuid.MustParse(tt.Status.DeleteBuildID)
	tt.Status.DeleteBuildID = ""
	require.NoError(t, k8sClient.Status().Update(e.ctx, tt))
	e.fake.setBuildJob(deleteBuildID, codersdk.ProvisionerJobFailed)
	tt = e.reconcile(t, key, 1)
	requireTemplateTestFailedWith(t, tt, "DeleteBuildFailed", metav1.ConditionFalse, "DeleteRetrying")
	require.Equal(t, int32(1), tt.Status.DeleteAttempts, "counted once")
	tt = e.reconcile(t, key, 2)
	require.Equal(t, int32(1), tt.Status.DeleteAttempts, "cleanup does not count it again")

	// Someone else's delete build fails after the pass.
	key, tt = e.passedAndDeleting(t)
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.LatestBuild.ID, ws.LatestBuild.Job.Status = uuid.New(), codersdk.ProvisionerJobCanceled
		return ws
	}})
	require.NotEqual(t, "", tt.Status.DeleteBuildID)
	requireTemplateTestFailedWith(t, e.reconcile(t, key, 1), "DeleteBuildFailed", metav1.ConditionFalse, "DeleteRetrying")

	// The deadline passes during the delete after a pass (plan A12.5): the
	// test fails as DeadlineExceeded, the delete build is never canceled,
	// and a later success keeps the failure.
	key, tt = e.passedAndDeleting(t)
	cancels := e.fake.requestCount(routeCancelBuild)
	e.clock.SetTime(tt.Status.StartTime.Add(900 * time.Second))
	requireTemplateTestFailedWith(t, e.settle(t, key), "DeadlineExceeded", metav1.ConditionFalse, "Deleting")
	require.Equal(t, cancels, e.fake.requestCount(routeCancelBuild), "the controller never cancels a delete build")
	e.fake.setBuildJob(uuid.MustParse(tt.Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	requireTemplateTestFailedWith(t, e.settle(t, key), "DeadlineExceeded", metav1.ConditionTrue, "Deleted")
}

func TestTemplateTestCleanupWrongAnswer(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.failedWithWorkspace(t)
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.ID = uuid.New()
		return ws
	}})
	tt := e.reconcile(t, key, 1)
	requireDeleted(t, tt, metav1.ConditionFalse, "CoderAnswerMismatch")
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
}

// TestTemplateTestSettleNeedsSiteOwner checks the release after the settle
// window: Coder also answers 404 when the caller may not read the
// workspace, so only a site owner's 404 proves that none exists.
func TestTemplateTestSettleNeedsSiteOwner(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.uncertainCreate(t, fakeFault{Status: 502}, 3600)
	e.fake.updateUser(e.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleMember}} })
	e.clock.SetTime(e.clock.Now().Add(15 * time.Minute))
	requireTemplateTestRunning(t, e.settle(t, key), "ConfirmingCreate", "not a site owner")
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter, "only a role change helps: poll slowly")

	// An organization role named owner is not the site owner role.
	e.fake.updateUser(e.fake.operatorID, func(u *codersdk.User) {
		u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner, OrganizationID: e.orgID.String()}}
	})
	requireTemplateTestRunning(t, e.settle(t, key), "ConfirmingCreate", "not a site owner")

	// Cleanup has no deadline, so it polls slowly too, not every 5 s.
	other := e.uncertainCreate(t, fakeFault{Status: 502}, 3600)
	e.deleteTest(t, other)
	e.clock.SetTime(e.clock.Now().Add(15 * time.Minute))
	requireDeleted(t, e.settle(t, other), metav1.ConditionUnknown, "CreateOutcomeUnknown")
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter)

	e.fake.updateUser(e.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner}} })
	requireTemplateTestFailed(t, e.settle(t, key), "CreateOutcomeUnknown")
}

// TestTemplateTestWatchesControlPlane runs the reconciler in a manager: a
// control plane change requeues its tests before their poll.
func TestTemplateTestWatchesControlPlane(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{e.ns: {}}},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	r := &controller.CoderTemplateTestReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Clock: clock.RealClock{}}
	require.NoError(t, r.SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })

	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	require.Eventually(t, func() bool { return e.get(t, key).Status.Reason == "OperatorAccessNotReady" }, 10*time.Second, 50*time.Millisecond)
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = true
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	// The Pending poll is 15 s, so only the watch is this fast.
	require.Eventually(t, func() bool { return e.get(t, key).Status.WorkspaceID != "" }, 8*time.Second, 50*time.Millisecond)
}

func TestTemplateTestDeleteEdgeCases(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)

	// Coder unavailable during cleanup: the slow 60 s poll, not 5 s.
	key := e.failedWithWorkspace(t)
	e.fake.failNext(routeWorkspace, fakeFault{Status: 500})
	requireDeleted(t, e.reconcile(t, key, 1), metav1.ConditionFalse, "ControlPlaneUnavailable")
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter)

	// A failed delete build seen late: the backoff counts from its
	// completion, so the retry follows at once.
	deletes := e.fake.requestCount(routeCreateBuild)
	e.fake.setBuildJob(uuid.MustParse(e.get(t, key).Status.DeleteBuildID), codersdk.ProvisionerJobFailed)
	e.clock.SetTime(e.clock.Now().Add(5 * time.Minute))
	requireDeleted(t, e.reconcile(t, key, 1), metav1.ConditionFalse, "DeleteRetrying")
	require.Equal(t, time.Second, e.lastStep.RequeueAfter)
	e.reconcile(t, key, 1)
	require.Equal(t, deletes+1, e.fake.requestCount(routeCreateBuild))

	// Without a completion time, the retry counts from the build's own time,
	// so it still comes after one backoff.
	deletes = e.fake.requestCount(routeCreateBuild)
	e.fake.setBuildJob(uuid.MustParse(e.get(t, key).Status.DeleteBuildID), codersdk.ProvisionerJobFailed)
	noCompletion := fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.LatestBuild.Job.CompletedAt = nil
		return ws
	}}
	e.fake.failNext(routeWorkspace, noCompletion)
	e.fake.failNext(routeWorkspace, noCompletion)
	requireDeleted(t, e.reconcile(t, key, 1), metav1.ConditionFalse, "DeleteRetrying")
	require.Equal(t, 2*time.Minute, e.lastStep.RequeueAfter)
	e.clock.SetTime(e.clock.Now().Add(2 * time.Minute))
	e.reconcile(t, key, 1)
	require.Equal(t, deletes+1, e.fake.requestCount(routeCreateBuild), "a stable retry time")

	// An unknown delete build status proves no failure: a passed test waits.
	key, _, buildID := e.startedTest(t)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	requireTemplateTestRunning(t, e.settle(t, key), "DeletingWorkspace", "")
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.LatestBuild.Job.Status = codersdk.ProvisionerJobUnknown
		return ws
	}})
	tt := e.reconcile(t, key, 1)
	requireTemplateTestRunning(t, tt, "DeletingWorkspace", "is unknown")
	require.Zero(t, tt.Status.DeleteAttempts)
}
