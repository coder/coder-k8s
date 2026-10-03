package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// templateTestEnv is one namespace with a control plane that talks to a fake
// Coder. The control plane's test owner is the fake user "tester".
type templateTestEnv struct {
	ctx      context.Context
	fake     *fakeCoder
	clock    *clocktesting.FakePassiveClock
	ns       string
	tester   uuid.UUID
	lastStep ctrl.Result
}

func newTemplateTestEnv(t *testing.T) *templateTestEnv {
	t.Helper()
	ctx := context.Background()
	// Status times have second precision, so the fake clock starts on a second.
	e := &templateTestEnv{ctx: ctx, fake: newFakeCoder(t), clock: clocktesting.NewFakePassiveClock(time.Now().Truncate(time.Second))}
	e.ns = createTestNamespace(ctx, t, "ktt-ctrl")
	e.tester = e.fake.addUser("tester", codersdk.LoginTypePassword)
	createTestControlPlane(ctx, t, e.ns, "coder", e.fake.server.URL)
	e.setOwner(t, "coder", e.tester.String())
	return e
}

func (e *templateTestEnv) setOwner(t *testing.T, controlPlane, ownerID string) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: controlPlane}, cp))
	cp.Spec.TemplateTests = &coderv1alpha1.TemplateTestsSpec{OwnerUserID: ownerID}
	if ownerID == "" {
		cp.Spec.TemplateTests = nil
	}
	require.NoError(t, k8sClient.Update(e.ctx, cp))
}

func (e *templateTestEnv) createTest(t *testing.T, template string, version coderv1alpha1.CoderTemplateTestVersion) types.NamespacedName {
	t.Helper()
	tt := &coderv1alpha1.CoderTemplateTest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "test-", Namespace: e.ns},
		Spec: coderv1alpha1.CoderTemplateTestSpec{
			ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
			Template:        template, Version: version,
		},
	}
	require.NoError(t, k8sClient.Create(e.ctx, tt))
	return types.NamespacedName{Namespace: tt.Namespace, Name: tt.Name}
}

// reconcile runs n reconciles, each with a new reconciler as after a restart,
// and returns the stored object (nil once it is gone).
func (e *templateTestEnv) reconcile(t *testing.T, key types.NamespacedName, n int) *coderv1alpha1.CoderTemplateTest {
	t.Helper()
	for range n {
		r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: e.clock}
		var err error
		e.lastStep, err = r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	tt := &coderv1alpha1.CoderTemplateTest{}
	err := k8sClient.Get(e.ctx, key, tt)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return tt
}

// settle reconciles until a waiting test is stable and a failed test has
// released its finalizer.
func (e *templateTestEnv) settle(t *testing.T, key types.NamespacedName) *coderv1alpha1.CoderTemplateTest {
	t.Helper()
	return e.reconcile(t, key, 4)
}

func requireTemplateTestWaiting(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, reason, messagePart string) {
	t.Helper()
	require.Equal(t, coderv1alpha1.CoderTemplateTestPhasePending, tt.Status.Phase, tt.Status.Message)
	require.Equal(t, reason, tt.Status.Reason, tt.Status.Message)
	require.Contains(t, tt.Status.Message, messagePart)
	require.True(t, meta.IsStatusConditionTrue(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReconciling))
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
}

func requireTemplateTestFailed(t *testing.T, tt *coderv1alpha1.CoderTemplateTest, reason string) {
	t.Helper()
	require.Equal(t, coderv1alpha1.CoderTemplateTestPhaseFailed, tt.Status.Phase, tt.Status.Message)
	require.Equal(t, reason, tt.Status.Reason, tt.Status.Message)
	require.NotNil(t, tt.Status.CompletionTime)
	require.True(t, meta.IsStatusConditionTrue(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionStalled))
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReady))
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReconciling))
	deleted := meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted)
	require.NotNil(t, deleted)
	require.Equal(t, metav1.ConditionTrue, deleted.Status)
	require.Equal(t, "NotCreated", deleted.Reason)
	require.False(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer), "a finished test without a workspace keeps no finalizer")
}

func TestTemplateTestWaitsForControlPlaneAndOwner(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	requireStep := func(reason, messagePart string) *coderv1alpha1.CoderTemplateTest {
		t.Helper()
		tt := e.settle(t, key)
		requireTemplateTestWaiting(t, tt, reason, messagePart)
		require.Positive(t, e.lastStep.RequeueAfter)
		return tt
	}

	tt := e.reconcile(t, key, 1)
	require.True(t, controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer))
	require.Empty(t, tt.Status.Phase, "the first reconcile only adds the finalizer")
	require.Zero(t, e.fake.requestCount(routeUser), "no Coder call before the finalizer is stored")

	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	tt = requireStep("OperatorAccessNotReady", "operator access")
	require.NotNil(t, tt.Status.StartTime)
	require.Equal(t, tt.Generation, tt.Status.ObservedGeneration)
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionReady))
	require.True(t, meta.IsStatusConditionFalse(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionStalled))
	cp.Status.OperatorAccessReady, cp.Status.URL = true, ""
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	requireStep("ControlPlaneNotReady", "status.url")
	require.NoError(t, k8sClient.Delete(e.ctx, cp))
	requireStep("ControlPlaneNotReady", "does not exist")

	// A recreated control plane starts without an owner.
	require.NoError(t, k8sClient.Delete(e.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "coder-operator-token"}}))
	createTestControlPlane(e.ctx, t, e.ns, "coder", e.fake.server.URL)
	requireStep("OwnerNotConfigured", "spec.templateTests.ownerUserID")
	e.setOwner(t, "coder", uuid.NewString())
	requireStep("OwnerNotEligible", "does not exist")
	e.setOwner(t, "coder", e.tester.String())
	e.fake.failNext(routeUser, fakeFault{Status: 503})
	requireTemplateTestWaiting(t, e.reconcile(t, key, 1), "CoderUnavailable", "get owner: Coder answered 503")
	requireStep("ReadyToCreate", "Inputs are resolved")
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace))

	require.NoError(t, k8sClient.Delete(e.ctx, tt))
	require.Nil(t, e.reconcile(t, key, 1), "deleting a test that never created a workspace releases it at once")
}

func TestTemplateTestDeadlineWhilePending(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	e.setOwner(t, "coder", "")
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	requireTemplateTestWaiting(t, e.settle(t, key), "OwnerNotConfigured", "")

	e.clock.SetTime(e.clock.Now().Add(899 * time.Second))
	requireTemplateTestWaiting(t, e.settle(t, key), "OwnerNotConfigured", "")
	require.Equal(t, time.Second, e.lastStep.RequeueAfter, "the requeue never passes the deadline")

	e.clock.SetTime(e.clock.Now().Add(time.Second))
	tt := e.settle(t, key)
	requireTemplateTestFailed(t, tt, "DeadlineExceeded")
	require.Contains(t, tt.Status.Message, "Last wait: OwnerNotConfigured")
	require.Equal(t, tt.Status.CompletionTime, e.settle(t, key).Status.CompletionTime, "a final test is never evaluated again")
}

// lateClock answers first on the first call and later afterwards, like a
// reconcile whose Coder lookup takes a while.
type lateClock struct {
	first, later time.Time
	calls        int
}

func (c *lateClock) Now() time.Time {
	c.calls++
	if c.calls == 1 {
		return c.first
	}
	return c.later
}

func (c *lateClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

func TestTemplateTestDeadlineAfterSlowLookup(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	tt := e.settle(t, key)
	requireTemplateTestWaiting(t, tt, "ReadyToCreate", "")

	deadline := tt.Status.StartTime.Add(900 * time.Second)
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: &lateClock{first: deadline.Add(-time.Second), later: deadline}}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	tt = e.settle(t, key)
	requireTemplateTestFailed(t, tt, "DeadlineExceeded")
	require.Contains(t, tt.Status.Message, "Last wait: ReadyToCreate")
}
