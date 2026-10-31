package controller_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		// Coder never answers 408 or 499 on create. A proxy can, after it
		// forwarded the request, so both stay uncertain, never rejected.
		{name: "408 after commit", fault: &fakeFault{Status: 408, AfterCommit: true}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", message: "Coder answered 408", confirmed: "WaitingForBuild"},
		{name: "499 after commit", fault: &fakeFault{Status: 499, AfterCommit: true}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", message: "Coder answered 499", confirmed: "WaitingForBuild"},
		// Coder answers create with 201. Since codersdk v2.37, a 201 without
		// the workspace's JSON is a *codersdk.Error with status 201. Another
		// 2xx always was one. Both stay uncertain, never rejected.
		{name: "200 proxy page", fault: &fakeFault{Status: 200, HTML: "<html>proxy</html>"}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", message: "Coder answered 200", confirmed: "ConfirmingCreate"},
		{name: "201 proxy page after commit", fault: &fakeFault{Status: 201, AfterCommit: true, HTML: "<html>proxy</html>"}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", message: "Coder answered 201: Received an HTML response", confirmed: "WaitingForBuild"},
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

// Two create answers without effect in a row must both clear the stored
// marker. Before #318 the second round compared the cleared status with the
// snapshot taken before the marker write, found no change, skipped the write,
// and left the stored marker behind: the test then confirmed forever and
// never sent the create again.
func TestTemplateTestCreateNoEffectTwiceInARow(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	for _, tc := range []struct {
		name   string
		fault  fakeFault
		reason string
	}{
		{name: "429", fault: fakeFault{Status: 429}, reason: "CreateRetrying"},
		{name: "401", fault: fakeFault{Status: 401}, reason: "CreateRetrying"},
		{name: "406", fault: fakeFault{Status: 406}, reason: "TemplateVersionImporting"},
	} {
		t.Run(tc.name, func(t *testing.T) { // Not parallel: the cases share e.
			creates := e.fake.requestCount(routeCreateWorkspace)
			key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
			e.reconcile(t, key, 2) // Finalizer and initialization.
			for round := 1; round <= 2; round++ {
				e.fake.failNext(routeCreateWorkspace, tc.fault)
				tt := e.reconcile(t, key, 1) // Lookups and create.
				require.Equal(t, creates+round, e.fake.requestCount(routeCreateWorkspace), "%s round %d", tc.name, round)
				// The stored object, not the in-memory one, must hold no marker.
				require.Nil(t, tt.Status.CreateAttemptTime, "%s round %d: a result without effect clears the stored marker", tc.name, round)
				require.Equal(t, coderv1alpha1.CoderTemplateTestPhasePending, tt.Status.Phase, "%s round %d", tc.name, round)
				require.Equal(t, tc.reason, tt.Status.Reason, "%s round %d", tc.name, round)
			}
			requireTemplateTestRunning(t, e.settle(t, key), "WaitingForBuild", "")
			require.Equal(t, creates+3, e.fake.requestCount(routeCreateWorkspace), "%s: the create is sent again after both answers", tc.name)
		})
	}
}

// markerConflictClient fails every status write that sets the create marker
// of a test or a pool member, like a write from a stale object after a leader
// handover.
type markerConflictClient struct{ client.Client }

func (c markerConflictClient) Status() client.SubResourceWriter {
	return markerConflictWriter{c.Client.Status()}
}

type markerConflictWriter struct{ client.SubResourceWriter }

func (w markerConflictWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if tt, ok := obj.(*coderv1alpha1.CoderTemplateTest); ok && tt.Status.CreateAttemptTime != nil {
		return apierrors.NewConflict(schema.GroupResource{Group: "coder.com", Resource: "codertemplatetests"}, tt.Name, errors.New("stale object"))
	}
	if m, ok := obj.(*coderv1alpha1.CoderWorkspacePoolMember); ok && m.Status.CreateAttemptTime != nil {
		return apierrors.NewConflict(schema.GroupResource{Group: "coder.com", Resource: "coderworkspacepoolmembers"}, m.Name, errors.New("stale object"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestTemplateTestMarkerConflictSendsNoRequest(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	e.reconcile(t, key, 2) // Finalizer and initialization.

	r := &controller.CoderTemplateTestReconciler{Client: markerConflictClient{k8sClient}, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.True(t, apierrors.IsConflict(err), "the marker write conflict ends the reconcile: %v", err)
	require.Zero(t, e.fake.requestCount(routeCreateWorkspace), "no create request without a stored marker")
	tt := &coderv1alpha1.CoderTemplateTest{}
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	require.Nil(t, tt.Status.CreateAttemptTime)
}

// After an uncertain create answer, every later wait starts with the answer's
// HTTP status, also after a failed confirm read, so the final DeadlineExceeded
// message still says why the test waited (#342). Coder's own text in a later
// error never counts, and a long error never cuts the note off.
func TestTemplateTestConfirmKeepsCreateAnswer(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	// A confirm read fails with a long message that quotes a create answer.
	fakeAnswer := "The create result is uncertain (Coder answered 418 " + strings.Repeat("x", 300)
	for _, tc := range []struct {
		name  string
		fault fakeFault
		note  string // Empty: an answer without HTTP status has no note.
	}{
		{name: "408", fault: fakeFault{Status: 408}, note: "The create request got HTTP 408 from Coder or a proxy. "},
		{name: "499", fault: fakeFault{Status: 499}, note: "The create request got HTTP 499 from Coder or a proxy. "},
		{name: "502", fault: fakeFault{Status: 502}, note: "The create request got HTTP 502 from Coder or a proxy. "},
		{name: "connection reset", fault: fakeFault{Reset: true}},
	} {
		t.Run(tc.name, func(t *testing.T) { // Not parallel: the cases share e and its clock.
			creates := e.fake.requestCount(routeCreateWorkspace)
			key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
			e.fake.failNext(routeCreateWorkspace, tc.fault)
			requireTemplateTestRunning(t, e.reconcile(t, key, 3), "ConfirmingCreate", "The create result is uncertain") // Finalizer, initialization, then lookups and create.

			tt := e.reconcile(t, key, 1)
			requireTemplateTestRunning(t, tt, "ConfirmingCreate", "Reading again.")
			require.True(t, strings.HasPrefix(tt.Status.Message, tc.note), tt.Status.Message)
			e.fake.failNext(routeWorkspaceByName, fakeFault{Status: 503, Message: fakeAnswer})
			tt = e.reconcile(t, key, 1)
			requireTemplateTestRunning(t, tt, "CoderUnavailable", "Coder answered 503")
			require.True(t, strings.HasPrefix(tt.Status.Message, tc.note), tt.Status.Message)
			tt = e.reconcile(t, key, 1)
			require.Equal(t, tc.note+fmt.Sprintf("No workspace %s from this test found yet. Reading again.", tt.Status.WorkspaceName), tt.Status.Message,
				"the note comes once, only with the create answer's status")

			e.clock.SetTime(e.clock.Now().Add(900 * time.Second))
			tt = e.reconcile(t, key, 1)
			require.Equal(t, "DeadlineExceeded", tt.Status.Reason, tt.Status.Message)
			require.Contains(t, tt.Status.Message, "Last wait: ConfirmingCreate: "+tc.note)
			require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace), "never a second create request")
		})
	}
}

// nonCoderNotFounds are 404 answers that Coder itself never gives on the
// workspace routes: a proxy's page, and Coder's own answer to a wrong route
// (coderd/httpapi/httpapi.go:180 in v2.37.3).
var nonCoderNotFounds = []struct {
	name  string
	fault fakeFault
}{
	{name: "HTML 404", fault: fakeFault{Status: 404, HTML: "<html>404 Not Found</html>"}},
	{name: "Route not found", fault: fakeFault{Status: 404, Message: "Route not found."}},
}

// Before the create, only Coder's own 404 proves that the workspace name is
// free. Another 404 waits and sends no create request (#351).
func TestTemplateTestLookupTrustsOnlyCodersNotFound(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	for _, tc := range nonCoderNotFounds {
		t.Run(tc.name, func(t *testing.T) { // Not parallel: the cases share e.
			creates := e.fake.requestCount(routeCreateWorkspace)
			key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
			e.fake.failNext(routeWorkspaceByName, tc.fault)
			tt := e.reconcile(t, key, 3) // Finalizer, initialization, then lookups.
			requireTemplateTestWaiting(t, tt, "CoderUnavailable", "get workspace by name: Coder answered 404")
			require.Nil(t, tt.Status.CreateAttemptTime, "no marker without a create request")
			require.Equal(t, creates, e.fake.requestCount(routeCreateWorkspace), "no create request")
			requireTemplateTestRunning(t, e.settle(t, key), "WaitingForBuild", "")
			require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace))
		})
	}
}

func TestTemplateTestDeadlineAfterCreate(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	tt := e.reconcile(t, key, 2) // Finalizer and initialization only.
	deadline := tt.Status.StartTime.Add(900 * time.Second)

	// The create request returns after the deadline.
	clock := &lateClock{times: []time.Time{deadline.Add(-3 * time.Second), deadline.Add(-2 * time.Second), deadline.Add(-time.Second), deadline}}
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: clock}
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
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: clock}
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
	r := &controller.CoderTemplateTestReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
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
