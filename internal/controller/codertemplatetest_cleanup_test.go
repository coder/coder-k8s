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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func (e *templateTestEnv) deleteTest(t *testing.T, key types.NamespacedName) {
	t.Helper()
	require.NoError(t, k8sClient.Delete(e.ctx, &coderv1alpha1.CoderTemplateTest{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}))
}

func (e *templateTestEnv) annotate(t *testing.T, key types.NamespacedName, policy string) {
	t.Helper()
	tt := &coderv1alpha1.CoderTemplateTest{}
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	tt.Annotations = map[string]string{"coder.com/deletion-policy": policy}
	require.NoError(t, k8sClient.Update(e.ctx, tt))
}

// allowRetain sets spec.templateTests.allowRetain on control plane coder.
func (e *templateTestEnv) allowRetain(t *testing.T) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	require.NotNil(t, cp.Spec.TemplateTests, "assertion failed: control plane coder has no templateTests")
	cp.Spec.TemplateTests.AllowRetain = true
	require.NoError(t, k8sClient.Update(e.ctx, cp))
}

// failedWithWorkspace ends a test as BuildFailed while its workspace exists.
func (e *templateTestEnv) failedWithWorkspace(t *testing.T) types.NamespacedName {
	t.Helper()
	key, _, buildID := e.startedTest(t)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobFailed)
	requireTemplateTestFailedWith(t, e.settle(t, key), "BuildFailed", metav1.ConditionFalse, "Deleting")
	return key
}

// ownershipUnknown ends a test as OwnershipUnknown: its workspace's builds
// name another initiator on the confirming read.
func (e *templateTestEnv) ownershipUnknown(t *testing.T) types.NamespacedName {
	t.Helper()
	key := e.uncertainCreate(t, fakeFault{Status: 504, AfterCommit: true})
	e.fake.failNext(routeWorkspaceBuilds, fakeFault{Rewrite: func(a any) any {
		builds := a.([]codersdk.WorkspaceBuild)
		builds[len(builds)-1].InitiatorID = uuid.New()
		return builds
	}})
	requireTemplateTestFailedWith(t, e.settle(t, key), "WorkspaceNameConflict", metav1.ConditionUnknown, "OwnershipUnknown")
	return key
}

func requireDeleted(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted)
	require.NotNil(t, c)
	require.Equal(t, status, c.Status, c.Message)
	require.Equal(t, reason, c.Reason, c.Message)
}

func TestTemplateTestDeletionComesFirst(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	e.reconcile(t, key, 2) // Finalizer and initialization: the next step would create.
	e.deleteTest(t, key)
	require.Nil(t, e.settle(t, key), "a test without a create request is released at once")
	require.Zero(t, e.fake.totalRequests(), "deletion never starts new work")
}

func TestTemplateTestDeletionStates(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)

	// A create request may exist: confirming reads, finalizer kept.
	key := e.uncertainCreate(t, fakeFault{Status: 502}, 3600)
	e.deleteTest(t, key)
	tt := e.reconcile(t, key, 2)
	require.Equal(t, coderv1alpha1.CoderTemplateTestPhaseRunning, tt.Status.Phase, "deletion never changes the phase")
	requireDeleted(t, tt, metav1.ConditionUnknown, "CreateOutcomeUnknown")
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
	require.Equal(t, 5*time.Second, e.lastStep.RequeueAfter)
	e.clock.SetTime(e.clock.Now().Add(15 * time.Minute))
	require.Nil(t, e.reconcile(t, key, 1), "nothing found after the settle window: released (A3)")
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace))

	// A committed create found during deletion is adopted, then kept.
	key = e.uncertainCreate(t, fakeFault{Status: 504, AfterCommit: true})
	e.deleteTest(t, key)
	tt = e.reconcile(t, key, 2)
	require.NotEmpty(t, tt.Status.WorkspaceID)
	requireDeleted(t, tt, metav1.ConditionFalse, "Deleting")
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))

	// A live workspace without provenance is never read again.
	key = e.ownershipUnknown(t)
	reads := e.fake.totalRequests()
	e.deleteTest(t, key)
	tt = e.settle(t, key)
	requireDeleted(t, tt, metav1.ConditionUnknown, "OwnershipUnknown")
	require.Equal(t, reads, e.fake.totalRequests(), "no reads for a workspace of unknown ownership")

	// Only retain releases it, without a Coder call (A2).
	e.allowRetain(t)
	e.annotate(t, key, "retain")
	require.Nil(t, e.reconcile(t, key, 1))
	require.Equal(t, reads, e.fake.totalRequests())
}

func TestTemplateTestRetain(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)

	// While the test runs, retain changes nothing.
	key, _, _ := e.startedTest(t)
	e.annotate(t, key, "retain")
	tt := e.reconcile(t, key, 1)
	requireTemplateTestRunning(t, tt, "WaitingForBuild", "")
	require.Nil(t, meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted))

	// Without the control plane's opt-in, retain is ignored: cleanup
	// deletes the workspace and keeps the finalizer until it is gone.
	key = e.failedWithWorkspace(t)
	e.annotate(t, key, "retain")
	tt = e.reconcile(t, key, 1)
	requireDeleted(t, tt, metav1.ConditionFalse, "Deleting")
	require.Contains(t, meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted).Message, "allowRetain")
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))

	// With the opt-in, a final test with a workspace is released without a
	// Coder call, and its IDs stay.
	e.allowRetain(t)
	key = e.failedWithWorkspace(t)
	requests := e.fake.totalRequests()
	e.annotate(t, key, "retain")
	tt = e.reconcile(t, key, 1)
	require.Equal(t, "BuildFailed", tt.Status.Reason, "retain never changes the result")
	requireDeleted(t, tt, metav1.ConditionFalse, "Retained")
	require.False(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
	require.NotEmpty(t, tt.Status.WorkspaceID)
	require.Equal(t, requests, e.fake.totalRequests(), "retain needs no Coder call")

	// Back to delete after the release: the finalizer returns, and cleanup
	// deletes the workspace.
	e.annotate(t, key, "delete")
	tt = e.reconcile(t, key, 2)
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
	requireDeleted(t, tt, metav1.ConditionFalse, "Deleting")

	// Any other value means delete, and the condition names it.
	key = e.failedWithWorkspace(t)
	e.annotate(t, key, "keep")
	tt = e.reconcile(t, key, 1)
	requireDeleted(t, tt, metav1.ConditionFalse, "Deleting")
	require.Contains(t, meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted).Message, `value "keep"`)
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
}

func TestTemplateTestControlPlaneGone(t *testing.T) {
	t.Parallel()
	controlPlane := func(e *templateTestEnv) *coderv1alpha1.CoderControlPlane {
		cp := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
		return cp
	}
	cases := []struct {
		name string
		gone func(e *templateTestEnv)
	}{
		{name: "not found", gone: func(e *templateTestEnv) { require.NoError(t, k8sClient.Delete(e.ctx, controlPlane(e))) }},
		{name: "being deleted", gone: func(e *templateTestEnv) {
			cp := controlPlane(e)
			cp.Finalizers = append(cp.Finalizers, "test.coder.com/hold")
			require.NoError(t, k8sClient.Update(e.ctx, cp))
			require.NoError(t, k8sClient.Delete(e.ctx, cp))
			t.Cleanup(func() {
				cp := controlPlane(e)
				cp.Finalizers = nil
				_ = k8sClient.Update(e.ctx, cp)
			})
		}},
	}
	for _, tc := range cases {
		e := newTemplateTestEnv(t)
		running, _, _ := e.startedTest(t)
		final := e.failedWithWorkspace(t)
		tc.gone(e)

		tt := e.reconcile(t, running, 1)
		requireTemplateTestFailedWith(t, tt, "ControlPlaneGone", metav1.ConditionUnknown, "ControlPlaneGone")
		require.False(t, controllerutil.ContainsFinalizer(e.settle(t, running), coderv1alpha1.CoderTemplateTestCleanupFinalizer), tc.name)

		// Namespace teardown: the control plane and a test go together.
		e.deleteTest(t, final)
		require.Nil(t, e.settle(t, final), "%s: deletion never waits for a gone control plane", tc.name)
	}
}

func TestTemplateTestControlPlaneUnavailable(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.failedWithWorkspace(t)
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))

	e.deleteTest(t, key)
	tt := e.reconcile(t, key, 2)
	requireDeleted(t, tt, metav1.ConditionFalse, "ControlPlaneUnavailable")
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer), "an unreachable Coder never counts as deleted")
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter)

	// Without the opt-in, retain keeps the finalizer while Coder is
	// unreachable.
	e.annotate(t, key, "retain")
	tt = e.reconcile(t, key, 1)
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer), "retain needs the control plane's opt-in")

	// Namespace teardown while Coder is unreachable: with the opt-in, retain
	// releases the test without a Coder call (A2, A5.3).
	e.allowRetain(t)
	requests := e.fake.totalRequests()
	require.Nil(t, e.reconcile(t, key, 1))
	require.Equal(t, requests, e.fake.totalRequests())
}

func TestTemplateTestCleanupRechecks(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)

	// Retained was stored, but the finalizer update failed, and the admin
	// switched back to delete: the finalizer stays.
	key := e.failedWithWorkspace(t)
	tt := &coderv1alpha1.CoderTemplateTest{}
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	meta.SetStatusCondition(&tt.Status.Conditions, metav1.Condition{
		Type:   coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted,
		Status: metav1.ConditionFalse, Reason: "Retained", Message: "Stored before the finalizer update failed.",
	})
	require.NoError(t, k8sClient.Status().Update(e.ctx, tt))
	e.annotate(t, key, "delete")
	tt = e.reconcile(t, key, 1)
	requireDeleted(t, tt, metav1.ConditionFalse, "Deleting")
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))

	// An ignored value is named for a workspace of unknown ownership too,
	// without reads and without a message that grows.
	key = e.ownershipUnknown(t)
	reads := e.fake.totalRequests()
	e.annotate(t, key, "keep")
	tt = e.reconcile(t, key, 1)
	requireDeleted(t, tt, metav1.ConditionUnknown, "OwnershipUnknown")
	message := meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted).Message
	require.Contains(t, message, `value "keep"`)
	require.Contains(t, message, "removing the finalizer releases the test")
	tt = e.reconcile(t, key, 1)
	require.Equal(t, message, meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted).Message)
	require.Equal(t, reads, e.fake.totalRequests())

	// A gone control plane releases it as well (A1).
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	require.NoError(t, k8sClient.Delete(e.ctx, cp))
	tt = e.reconcile(t, key, 1)
	requireDeleted(t, tt, metav1.ConditionUnknown, "ControlPlaneGone")
	require.False(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
}
