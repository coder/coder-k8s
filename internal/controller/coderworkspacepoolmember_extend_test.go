package controller_test

import (
	"context"
	"errors"
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
	// A failed read shows in status, and the next good read clears it.
	e.fake.failNext(routeWorkspace, fakeFault{Status: 503})
	member := e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "CoderUnavailable", "Coder answered 503")
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "WorkspaceReady", "is ready")

	// 14 minutes before the deadline, the controller moves it to now + TTL.
	e.clock.SetTime(started.Add(46 * time.Minute))
	member = e.reconcileMember(t, key, 1)
	requireReady(t, member, "extended the deadline")
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace))
	require.True(t, member.Status.LastExtendTime.Time.Equal(e.clock.Now()))
	require.Equal(t, e.clock.Now().Add(time.Hour).UTC(), e.deadline(t, workspaceID).Time.UTC())
	requireReady(t, e.reconcileMember(t, key, 2), "")
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace), "the new deadline is an hour away")

	// The template's max deadline bounds the extension, then ends it: the
	// member never sends a request that Coder must refuse.
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 90*time.Minute)
	e.clock.SetTime(started)
	key, workspaceID = e.readyMember(t)
	extends := e.fake.requestCount(routeExtendWorkspace)
	e.clock.SetTime(started.Add(46 * time.Minute))
	requireReady(t, e.reconcileMember(t, key, 1), "extended the deadline")
	require.True(t, started.Add(90*time.Minute).Equal(e.deadline(t, workspaceID).Time))
	for _, at := range []time.Duration{76 * time.Minute, 77 * time.Minute} {
		e.clock.SetTime(started.Add(at))
		requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "MaxDeadline", "allows no extension")
	}
	require.Equal(t, extends+1, e.fake.requestCount(routeExtendWorkspace), "no request after the max deadline")

	// Any other refusal, for example after clock skew, is a wait that names it.
	e.clock.SetTime(started)
	key, workspaceID = e.readyMember(t)
	before := e.deadline(t, workspaceID)
	e.fake.failNext(routeExtendWorkspace, fakeFault{Status: 400})
	e.clock.SetTime(started.Add(46 * time.Minute))
	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ExtendRefused", "Coder refused to extend")
	require.Nil(t, member.Status.LastExtendTime)
	require.Equal(t, before, e.deadline(t, workspaceID))

	// Without a deadline, nothing is extended.
	e.fake.setTemplateSchedule(e.tplID, 0, 0)
	e.clock.SetTime(started)
	key, _ = e.readyMember(t)
	extends = e.fake.requestCount(routeExtendWorkspace)
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
	started := e.clock.Now()
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 0)
	key, workspaceID := e.readyMember(t)
	e.clock.SetTime(started.Add(10 * time.Minute)) // 50 minutes left.
	bind(t, key)
	// A 409 (the workspace changed after the read) is read again, not the end.
	e.fake.failNext(routeExtendWorkspace, fakeFault{Status: 409})
	member := e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "could not extend")
	require.False(t, member.Status.BindExtended)
	require.True(t, member.Status.BindExtendDeadline.Time.Equal(e.clock.Now().Add(time.Hour)))
	require.Equal(t, time.Minute, e.lastStep.RequeueAfter)
	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "bound to claim build-1")
	require.True(t, member.Status.BindExtended)
	require.Equal(t, 2, e.fake.requestCount(routeExtendWorkspace), "the bind extends even with 50 minutes left")
	require.True(t, e.deadline(t, workspaceID).Time.Equal(e.clock.Now().Add(time.Hour)))
	require.Zero(t, e.lastStep.RequeueAfter)

	e.clock.SetTime(e.clock.Now().Add(55 * time.Minute)) // 5 minutes left.
	requireMember(t, e.reconcileMember(t, key, 2), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "")
	require.Equal(t, 2, e.fake.requestCount(routeExtendWorkspace), "never a second extension after bind")

	// A deadline later than now + TTL (the TTL shrank) is never moved earlier.
	e.clock.SetTime(started)
	e.fake.setTemplateSchedule(e.tplID, 2*time.Hour, 0)
	key, workspaceID = e.readyMember(t)
	before := e.deadline(t, workspaceID)
	e.fake.setTemplateSchedule(e.tplID, 30*time.Minute, 0)
	bind(t, key)
	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "needs no extension")
	require.True(t, member.Status.BindExtended)
	require.Equal(t, before, e.deadline(t, workspaceID))
	require.Equal(t, 2, e.fake.requestCount(routeExtendWorkspace))

	// The new deadline counts from a clock reading after the status write
	// that stores the target, not from the start of a slow reconcile.
	e.clock.SetTime(started)
	e.fake.setTemplateSchedule(e.tplID, 10*time.Minute, 0)
	key, workspaceID = e.readyMember(t)
	bind(t, key)
	late := started.Add(90 * time.Second)
	e.clock.SetTime(late) // The fake checks the 29-minute rule against this time.
	r := &controller.CoderWorkspacePoolMemberReconciler{Client: k8sClient, Scheme: scheme, Clock: &lateClock{times: []time.Time{started, started, late}}}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	member = e.reconcileMember(t, key, 0)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "bound to claim build-1")
	require.True(t, member.Status.BindExtendDeadline.Time.Equal(started.Add(30*time.Minute)))
	require.True(t, e.deadline(t, workspaceID).Time.Equal(late.Add(30*time.Minute)))

	// Without a deadline, the bind needs no extension.
	e.fake.setTemplateSchedule(e.tplID, 0, 0)
	e.clock.SetTime(time.Now().Truncate(time.Second))
	key, _ = e.readyMember(t)
	bind(t, key)
	extends := e.fake.requestCount(routeExtendWorkspace)
	member = e.reconcileMember(t, key, 1)
	requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "no autostop deadline")
	require.True(t, member.Status.BindExtended)
	require.Equal(t, extends, e.fake.requestCount(routeExtendWorkspace))
}

// failStatusWriteClient fails every member status write that match accepts,
// after the write landed when committed is true (like a crash right after
// it), or instead of it (like a conflict).
type failStatusWriteClient struct {
	client.Client
	match     func(*coderv1alpha1.CoderWorkspacePoolMember) bool
	committed bool
}

func (c failStatusWriteClient) Status() client.SubResourceWriter {
	return failStatusWriter{c.Client.Status(), c}
}

type failStatusWriter struct {
	client.SubResourceWriter
	c failStatusWriteClient
}

func (w failStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	m, ok := obj.(*coderv1alpha1.CoderWorkspacePoolMember)
	if !ok || !w.c.match(m) {
		return w.SubResourceWriter.Update(ctx, obj, opts...)
	}
	if w.c.committed {
		if err := w.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
			return err
		}
	}
	return apierrors.NewConflict(schema.GroupResource{Group: "coder.com", Resource: "coderworkspacepoolmembers"}, m.Name, errors.New("stale object"))
}

// TestPoolMemberBindExtensionConfirmed: the member stores the target deadline
// before the extend request, and only Coder's deadline proves the extension.
// So no lost status write, crash, or uncertain answer leads to a second
// extension, or to a bind without one.
func TestPoolMemberBindExtensionConfirmed(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.fake.setTemplateSchedule(e.tplID, time.Hour, 0)
	started := e.clock.Now()
	bound := func(t *testing.T) (types.NamespacedName, uuid.UUID) {
		t.Helper()
		e.clock.SetTime(started)
		key, workspaceID := e.readyMember(t)
		member := e.reconcileMember(t, key, 0)
		member.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: "build-1", UID: uuid.NewString()}
		require.NoError(t, k8sClient.Update(e.ctx, member))
		e.clock.SetTime(started.Add(10 * time.Minute))
		return key, workspaceID
	}
	failWrite := func(t *testing.T, key types.NamespacedName, c failStatusWriteClient) {
		t.Helper()
		c.Client = k8sClient
		r := &controller.CoderWorkspacePoolMemberReconciler{Client: c, Scheme: scheme, Clock: e.clock}
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.True(t, apierrors.IsConflict(err), "the failed write ends the reconcile: %v", err)
	}
	// extendedAfter: a minute later, the next reconciles send wantRequests
	// extend requests and end with the bind extended to the stored target
	// or later.
	extendedAfter := func(t *testing.T, key types.NamespacedName, workspaceID uuid.UUID, wantRequests int) {
		t.Helper()
		requests := e.fake.requestCount(routeExtendWorkspace)
		e.clock.SetTime(e.clock.Now().Add(time.Minute))
		member := e.reconcileMember(t, key, 2)
		requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "Its deadline is now")
		require.True(t, member.Status.BindExtended)
		require.Equal(t, requests+wantRequests, e.fake.requestCount(routeExtendWorkspace))
		require.False(t, e.deadline(t, workspaceID).Time.Before(member.Status.BindExtendDeadline.Time))
	}

	// The status write after a successful request is lost: the read proves
	// the extension, so no second request.
	key, workspaceID := bound(t)
	failWrite(t, key, failStatusWriteClient{match: func(m *coderv1alpha1.CoderWorkspacePoolMember) bool { return m.Status.BindExtended }})
	require.Equal(t, 1, e.fake.requestCount(routeExtendWorkspace))
	extendedAfter(t, key, workspaceID, 0)

	// A crash right after the target is stored, before the request: the
	// next reconcile still extends.
	key, workspaceID = bound(t)
	requests := e.fake.requestCount(routeExtendWorkspace)
	failWrite(t, key, failStatusWriteClient{committed: true, match: func(m *coderv1alpha1.CoderWorkspacePoolMember) bool {
		return m.Status.BindExtendDeadline != nil && !m.Status.BindExtended
	}})
	require.Equal(t, requests, e.fake.requestCount(routeExtendWorkspace))
	extendedAfter(t, key, workspaceID, 1)

	// A 409, then a lost status write: nothing claims the extension.
	key, workspaceID = bound(t)
	e.fake.failNext(routeExtendWorkspace, fakeFault{Status: 409})
	failWrite(t, key, failStatusWriteClient{match: func(m *coderv1alpha1.CoderWorkspacePoolMember) bool {
		return strings.Contains(m.Status.Message, "could not extend")
	}})
	extendedAfter(t, key, workspaceID, 1)

	// The next read decides an uncertain answer: the member sends the
	// request again when it had no effect, never when it had one.
	for _, c := range []struct {
		fault        fakeFault
		wantRequests int
	}{{fakeFault{Status: 502}, 1}, {fakeFault{Status: 502, AfterCommit: true}, 0}, {fakeFault{Status: 429}, 1}} {
		key, workspaceID = bound(t)
		e.fake.failNext(routeExtendWorkspace, c.fault)
		member := e.reconcileMember(t, key, 1)
		requireMember(t, member, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "CoderUnavailable", "extend workspace")
		require.False(t, member.Status.BindExtended)
		extendedAfter(t, key, workspaceID, c.wantRequests)
	}

	// Coder refuses the bind extension: the member does not try again.
	key, _ = bound(t)
	e.fake.failNext(routeExtendWorkspace, fakeFault{Status: 400})
	requireMember(t, e.reconcileMember(t, key, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Bound", "Coder refused to extend")
	requests = e.fake.requestCount(routeExtendWorkspace)
	e.clock.SetTime(e.clock.Now().Add(time.Minute))
	require.True(t, e.reconcileMember(t, key, 2).Status.BindExtended)
	require.Equal(t, requests, e.fake.requestCount(routeExtendWorkspace))
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

	// A status writer names a start build that the owner, not the operator,
	// started on the member's own kwp- workspace.
	e.clock.SetTime(started)
	key, workspaceID = e.readyMember(t)
	build := e.fake.addExternalBuild(workspaceID, e.tester, codersdk.WorkspaceTransitionStart)
	e.fake.setBuildJob(build.ID, codersdk.ProvisionerJobSucceeded)
	member = e.reconcileMember(t, key, 0)
	member.Status.StartBuildID = build.ID.String()
	require.NoError(t, k8sClient.Status().Update(e.ctx, member))
	e.clock.SetTime(started.Add(50 * time.Minute))
	requireMemberFailed(t, e.reconcileMember(t, key, 1), "OwnershipUnknown", "Nothing proves that the controller started build 2", metav1.ConditionUnknown)
	require.Zero(t, e.fake.requestCount(routeExtendWorkspace))
}
