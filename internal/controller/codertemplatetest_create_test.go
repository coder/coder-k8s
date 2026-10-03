package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

func TestTemplateTestCreateResults(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	cases := []struct {
		name    string
		fault   *fakeFault
		phase   string
		reason  string
		message string
		// confirmed is the reason once confirming reads ran after an
		// uncertain result.
		confirmed string
	}{
		{name: "created", phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "WaitingForBuild"},
		{name: "429", fault: &fakeFault{Status: 429}, phase: coderv1alpha1.CoderTemplateTestPhasePending, reason: "CreateRetrying", message: "Coder answered 429"},
		{name: "401", fault: &fakeFault{Status: 401}, phase: coderv1alpha1.CoderTemplateTestPhasePending, reason: "CreateRetrying", message: "Coder answered 401"},
		{name: "406", fault: &fakeFault{Status: 406}, phase: coderv1alpha1.CoderTemplateTestPhasePending, reason: "TemplateVersionImporting"},
		{name: "409", fault: &fakeFault{Status: 409}, phase: coderv1alpha1.CoderTemplateTestPhaseFailed, reason: "WorkspaceNameConflict"},
		{name: "400", fault: &fakeFault{Status: 400}, phase: coderv1alpha1.CoderTemplateTestPhaseFailed, reason: "CreateRejected", message: "Coder answered 400: fake fault 400"},
		{name: "502 before commit", fault: &fakeFault{Status: 502}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", message: "Coder answered 502", confirmed: "ConfirmingCreate"},
		{name: "504 after commit", fault: &fakeFault{Status: 504, AfterCommit: true}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", confirmed: "WaitingForBuild"},
		{name: "connection reset", fault: &fakeFault{Reset: true}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", confirmed: "ConfirmingCreate"},
		{name: "wrong workspace in the answer", fault: &fakeFault{Rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.Name = "other"
			return ws
		}}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "CoderAnswerMismatch", message: "Coder answered workspace ", confirmed: "WaitingForBuild"},
		{name: "answer without a build ID", fault: &fakeFault{Rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.LatestBuild.ID = uuid.Nil
			return ws
		}}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "CoderAnswerMismatch", message: "Coder answered start build ", confirmed: "WaitingForBuild"},
		{name: "answer with another workspace's build", fault: &fakeFault{Rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.LatestBuild.WorkspaceID = uuid.New()
			return ws
		}}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "CoderAnswerMismatch", message: "Coder answered start build workspace ", confirmed: "WaitingForBuild"},
		{name: "answer with a delete build", fault: &fakeFault{Rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.LatestBuild.Transition = codersdk.WorkspaceTransitionDelete
			return ws
		}}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "CoderAnswerMismatch", message: "Coder answered start build transition ", confirmed: "WaitingForBuild"},
	}
	for _, tc := range cases {
		creates := e.fake.requestCount(routeCreateWorkspace)
		key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
		if tc.fault != nil {
			e.fake.failNext(routeCreateWorkspace, *tc.fault)
		}
		tt := e.reconcile(t, key, 3) // Finalizer, initialization, then lookups and create.
		require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace), tc.name)
		require.Equal(t, tc.phase, tt.Status.Phase, tc.name)
		require.Equal(t, tc.reason, tt.Status.Reason, tc.name)
		require.Contains(t, tt.Status.Message, tc.message, tc.name)

		switch tc.phase {
		case coderv1alpha1.CoderTemplateTestPhaseFailed:
			if tc.name == "409" {
				// Deleted before its finalizer is released: the marker is set,
				// but WorkspaceDeleted=True proves nothing exists.
				require.NoError(t, k8sClient.Delete(e.ctx, tt))
				require.Nil(t, e.reconcile(t, key, 1), "a proven-clean test is released on deletion")
				continue
			}
			requireTemplateTestFailed(t, e.settle(t, key), tc.reason)
		case coderv1alpha1.CoderTemplateTestPhasePending:
			require.Nil(t, tt.Status.CreateAttemptTime, "%s: a result without effect clears the marker", tc.name)
			if tc.name == "429" {
				require.InDelta(t, 2*time.Second, e.lastStep.RequeueAfter, float64(400*time.Millisecond), "429 backs off with jitter")
			}
			requireTemplateTestRunning(t, e.settle(t, key), "WaitingForBuild", "")
			require.Equal(t, creates+2, e.fake.requestCount(routeCreateWorkspace), "%s: one more request after the retry", tc.name)
		case coderv1alpha1.CoderTemplateTestPhaseRunning:
			require.NotNil(t, tt.Status.CreateAttemptTime, tc.name)
			if tc.reason != "WaitingForBuild" {
				require.Empty(t, tt.Status.WorkspaceID, tc.name)
				tt = e.settle(t, key)
				requireTemplateTestRunning(t, tt, tc.confirmed, "")
				require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace), "%s: never a second create request", tc.name)
				if tc.confirmed == "ConfirmingCreate" {
					require.Empty(t, tt.Status.WorkspaceID, tc.name)
					continue
				}
			}
			ws, err := e.fake.client(t, 5*time.Second).Workspace(e.ctx, uuid.MustParse(tt.Status.WorkspaceID))
			require.NoError(t, err)
			require.Equal(t, tt.Status.StartBuildID, ws.LatestBuild.ID.String())
			require.Equal(t, tt.Status.WorkspaceName, ws.Name)
			require.Equal(t, codersdk.AutomaticUpdatesNever, ws.AutomaticUpdates)
		}
	}
}

// markerConflictClient fails every status write that sets the create marker,
// like a write from a stale object after a leader handover.
type markerConflictClient struct{ client.Client }

func (c markerConflictClient) Status() client.SubResourceWriter {
	return markerConflictWriter{c.Client.Status()}
}

type markerConflictWriter struct{ client.SubResourceWriter }

func (w markerConflictWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if tt, ok := obj.(*coderv1alpha1.CoderTemplateTest); ok && tt.Status.CreateAttemptTime != nil {
		return apierrors.NewConflict(schema.GroupResource{Group: "coder.com", Resource: "codertemplatetests"}, tt.Name, errors.New("stale object"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestTemplateTestMarkerConflictSendsNoRequest(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	e.reconcile(t, key, 2) // Finalizer and initialization.

	r := &controller.CoderTemplateTestReconciler{Client: markerConflictClient{k8sClient}, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.True(t, apierrors.IsConflict(err), "the marker write conflict ends the reconcile: %v", err)
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request without a stored marker")
	tt := &coderv1alpha1.CoderTemplateTest{}
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	require.Nil(t, tt.Status.CreateAttemptTime)
}

func TestTemplateTestDeadlineAfterCreate(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	tt := e.reconcile(t, key, 2) // Finalizer and initialization only.
	deadline := tt.Status.StartTime.Add(900 * time.Second)

	// The create request returns after the deadline.
	clock := &lateClock{times: []time.Time{deadline.Add(-3 * time.Second), deadline.Add(-2 * time.Second), deadline.Add(-time.Second), deadline}}
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	requireTemplateTestFailedWith(t, tt, "DeadlineExceeded", metav1.ConditionFalse, "CleanupPending")
	require.Contains(t, tt.Status.Message, "Last wait: WaitingForBuild")
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace))
}

func TestTemplateTestDeadlineAfterMarkerWrite(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	tt := e.reconcile(t, key, 2) // Finalizer and initialization only.
	deadline := tt.Status.StartTime.Add(900 * time.Second)

	// The marker write returns at the deadline: no request leaves.
	clock := &lateClock{times: []time.Time{deadline.Add(-2 * time.Second), deadline.Add(-time.Second), deadline}}
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request after the deadline")
	tt = e.settle(t, key)
	requireTemplateTestFailed(t, tt, "DeadlineExceeded")
	require.Nil(t, tt.Status.CreateAttemptTime, "an unsent request leaves no marker")
	require.Contains(t, tt.Status.Message, "Last wait: CreatingWorkspace")
}

func TestTemplateTestBackoffForgetsDeletedTests(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: e.clock}
	run := func(key types.NamespacedName) ctrl.Result {
		t.Helper()
		result, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		return result
	}
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	run(key) // Finalizer.
	run(key) // Initialization.
	e.fake.failNext(routeUser, fakeFault{Status: 429})
	e.fake.failNext(routeUser, fakeFault{Status: 429})
	require.InDelta(t, 2*time.Second, run(key).RequeueAfter, float64(400*time.Millisecond))
	require.InDelta(t, 4*time.Second, run(key).RequeueAfter, float64(800*time.Millisecond), "the backoff grows")

	// Deleting the test forgets its backoff, so a new test with the same
	// name starts at 2 s again.
	require.NoError(t, k8sClient.Delete(e.ctx, &coderv1alpha1.CoderTemplateTest{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}))
	run(key)
	run(key) // Not found.
	again := &coderv1alpha1.CoderTemplateTest{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Spec: coderv1alpha1.CoderTemplateTestSpec{
		ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"}, Template: "default.docker", Version: coderv1alpha1.CoderTemplateTestVersion{Name: "v1"},
	}}
	require.NoError(t, k8sClient.Create(e.ctx, again))
	run(key)
	run(key)
	e.fake.failNext(routeUser, fakeFault{Status: 429})
	require.InDelta(t, 2*time.Second, run(key).RequeueAfter, float64(400*time.Millisecond))

	// The jittered delay never passes the 2 m cap.
	for range 16 {
		e.fake.failNext(routeUser, fakeFault{Status: 429})
		require.LessOrEqual(t, run(key).RequeueAfter, 2*time.Minute)
	}
}
