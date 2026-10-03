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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	}{
		{name: "created", phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "WaitingForBuild"},
		{name: "429", fault: &fakeFault{Status: 429}, phase: coderv1alpha1.CoderTemplateTestPhasePending, reason: "CreateRetrying", message: "Coder answered 429"},
		{name: "401", fault: &fakeFault{Status: 401}, phase: coderv1alpha1.CoderTemplateTestPhasePending, reason: "CreateRetrying", message: "Coder answered 401"},
		{name: "406", fault: &fakeFault{Status: 406}, phase: coderv1alpha1.CoderTemplateTestPhasePending, reason: "TemplateVersionImporting"},
		{name: "409", fault: &fakeFault{Status: 409}, phase: coderv1alpha1.CoderTemplateTestPhaseFailed, reason: "WorkspaceNameConflict"},
		{name: "400", fault: &fakeFault{Status: 400}, phase: coderv1alpha1.CoderTemplateTestPhaseFailed, reason: "CreateRejected", message: "Coder answered 400: fake fault 400"},
		{name: "502 before commit", fault: &fakeFault{Status: 502}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate", message: "Coder answered 502"},
		{name: "504 after commit", fault: &fakeFault{Status: 504, AfterCommit: true}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate"},
		{name: "connection reset", fault: &fakeFault{Reset: true}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "ConfirmingCreate"},
		{name: "wrong workspace in the answer", fault: &fakeFault{Rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.Name = "other"
			return ws
		}}, phase: coderv1alpha1.CoderTemplateTestPhaseRunning, reason: "CoderAnswerMismatch", message: "Coder answered workspace "},
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
				requireTemplateTestRunning(t, e.settle(t, key), "ConfirmingCreate", "")
				require.Equal(t, creates+1, e.fake.requestCount(routeCreateWorkspace), "%s: never a second create request", tc.name)
				continue
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
