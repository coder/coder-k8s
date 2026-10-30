package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// mcpSweep is a sweeper for the records of an mcpServerEnv.
type mcpSweep struct {
	env      mcpServerEnv
	clock    *clocktesting.FakeClock
	recorder *events.FakeRecorder
	sweeper  *controller.CleanupRecordReconciler
}

func newMCPSweep(t *testing.T, env mcpServerEnv) mcpSweep {
	t.Helper()
	s := mcpSweep{env: env, clock: clocktesting.NewFakeClock(time.Now()), recorder: events.NewFakeRecorder(10)}
	s.sweeper = &controller.CleanupRecordReconciler{Client: watchClient(t), APIReader: k8sClient, OperatorNamespace: env.namespace, Clock: s.clock, Recorder: s.recorder}
	return s
}

func (s mcpSweep) sweep(ctx context.Context, t *testing.T, uid types.UID) ctrl.Result {
	t.Helper()
	result, err := s.sweeper.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: s.env.namespace, Name: "ck8s-cleanup-" + string(uid)}})
	require.NoError(t, err)
	return result
}

// leaveMCPServer deletes the control plane, then the object, as a namespace
// deletion can: the finalizer goes without a delete, and the record stays.
func (env mcpServerEnv) leaveMCPServer(ctx context.Context, t *testing.T, server *coderv1alpha1.CoderMCPServer) {
	t.Helper()
	require.NoError(t, k8sClient.Delete(ctx, env.controlPlane))
	gone, _ := env.deleteMCPServer(ctx, t, server)
	require.Nil(t, gone)
	require.NotNil(t, env.mcpServerRecord(ctx, t, server.UID))
}

// #353 (#384 follow-up 3): after a namespace deletion, a new control plane
// that reaches the same Coder deployment deletes the leftover MCP server
// config, once the settle window has passed. Not parallel: it reads
// process-wide metrics.
func TestCleanupSweepMCPServer(t *testing.T) {
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	env.leaveMCPServer(ctx, t, got)
	require.Len(t, env.fake.mcpServers(), 1, "the config is left in Coder")
	s := newMCPSweep(t, env)
	swept := kindMetric(t, "coder_k8s_cleanup_records_swept_total", "CoderMCPServer")

	requests := env.fake.totalRequests()
	result := s.sweep(ctx, t, got.UID)
	require.Equal(t, requests, env.fake.totalRequests(), "no Coder call inside the settle window")
	require.Greater(t, result.RequeueAfter, 4*time.Minute)
	s.clock.Step(6 * time.Minute)
	require.Equal(t, 10*time.Minute, s.sweep(ctx, t, got.UID).RequeueAfter, "no control plane reaches the deployment")
	require.Contains(t, <-s.recorder.Events, "NoMatchingDeployment")
	require.Equal(t, 1.0, kindMetric(t, "coder_k8s_cleanup_records_unmatched", "CoderMCPServer"))
	require.Len(t, env.fake.mcpServers(), 1)

	createTestControlPlane(ctx, t, env.namespace, "coder-b", env.fake.server.URL)
	require.Zero(t, s.sweep(ctx, t, got.UID).RequeueAfter)
	require.Empty(t, env.fake.mcpServers(), "the sweep deleted the config")
	require.Equal(t, 1, env.fake.requestCount(routeDeleteMCPServer))
	require.Nil(t, env.mcpServerRecord(ctx, t, got.UID), "Coder confirmed the delete, so the record goes")
	require.Equal(t, swept+1, kindMetric(t, "coder_k8s_cleanup_records_swept_total", "CoderMCPServer"))
	require.Zero(t, kindMetric(t, "coder_k8s_cleanup_records_unmatched", "CoderMCPServer"))
}

// #353 invariant 6: a sweep never touches the config of a living object.
func TestCleanupSweepMCPServerLivingObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	s := newMCPSweep(t, env)
	s.clock.Step(time.Hour)
	requests := env.fake.totalRequests()
	require.Equal(t, 30*time.Minute, s.sweep(ctx, t, got.UID).RequeueAfter)
	require.Equal(t, requests, env.fake.totalRequests(), "no Coder call for a living object")
	require.NotNil(t, env.mcpServerRecord(ctx, t, got.UID))
}

// #353 invariant 5: without a recorded config ID, a sweep deletes nothing. A
// person can edit the slug and the display name of a config, so neither
// proves that the object created it. The record goes only when the
// organization holds no config with the recorded slug and none with the
// temporary display name. Otherwise it stays for manual cleanup.
func TestCleanupSweepMCPServerWithoutConfigID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	editConfig := func(req codersdk.UpdateMCPServerConfigRequest) func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer) {
		return func(ctx context.Context, t *testing.T, env mcpServerEnv, _ *coderv1alpha1.CoderMCPServer) {
			t.Helper()
			configs := env.fake.mcpServers()
			require.Len(t, configs, 1)
			_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, configs[0].view.ID, req)
			require.NoError(t, err)
		}
	}
	for name, tc := range map[string]struct {
		committed bool                                                                           // The create commits, and its answer is lost.
		edit      func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer) // A person's change in Coder after the object went.
		kept      bool
	}{
		"create never committed":                            {},
		"lost create answer, display name edited":           {committed: true, edit: editConfig(codersdk.UpdateMCPServerConfigRequest{DisplayName: ptr.To("Renamed by a person")}), kept: true},
		"lost create answer, slug edited":                   {committed: true, edit: editConfig(codersdk.UpdateMCPServerConfigRequest{Slug: ptr.To("renamed")}), kept: true},
		"a person's config with the slug":                   {edit: createOther, kept: true},
		"a person's config with the temporary display name": {edit: createWithNonce, kept: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newMCPServerEnv(ctx, t, true)
			got := env.leaveWithoutConfigID(ctx, t, tc.committed)
			if tc.edit != nil {
				tc.edit(ctx, t, env, got)
			}
			configs, deletes := len(env.fake.mcpServers()), env.fake.requestCount(routeDeleteMCPServer)
			createTestControlPlane(ctx, t, env.namespace, "coder-b", env.fake.server.URL)
			s := newMCPSweep(t, env)
			s.clock.Step(6 * time.Minute)

			result := s.sweep(ctx, t, got.UID)
			require.Equal(t, deletes, env.fake.requestCount(routeDeleteMCPServer), "a record without a config ID deletes nothing")
			require.Len(t, env.fake.mcpServers(), configs)
			if !tc.kept {
				require.Zero(t, result.RequeueAfter)
				require.Nil(t, env.mcpServerRecord(ctx, t, got.UID), "Coder holds no config that can be the object's")
				require.Empty(t, s.recorder.Events)
				return
			}
			require.Equal(t, 10*time.Minute, result.RequeueAfter)
			require.NotNil(t, env.mcpServerRecord(ctx, t, got.UID), "a config can still be the object's, so the record stays")
			require.Len(t, s.recorder.Events, 1)
			require.Contains(t, <-s.recorder.Events, "ManualCleanupNeeded")
		})
	}
}

// #353: a record kept for manual cleanup counts in the unmatched gauge until
// a person deletes the config in Coder. The next sweep then removes it. Not
// parallel: it reads process-wide metrics.
func TestCleanupSweepMCPServerManualCleanup(t *testing.T) {
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	got := env.leaveWithoutConfigID(ctx, t, true)
	createTestControlPlane(ctx, t, env.namespace, "coder-b", env.fake.server.URL)
	s := newMCPSweep(t, env)
	s.clock.Step(6 * time.Minute)

	require.Equal(t, 10*time.Minute, s.sweep(ctx, t, got.UID).RequeueAfter)
	require.Len(t, env.fake.mcpServers(), 1, "the config of the lost create answer stays")
	require.NotNil(t, env.mcpServerRecord(ctx, t, got.UID))
	require.Equal(t, 1.0, kindMetric(t, "coder_k8s_cleanup_records_unmatched", "CoderMCPServer"))

	for _, c := range env.fake.mcpServers() {
		require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, c.view.ID))
	}
	require.Zero(t, s.sweep(ctx, t, got.UID).RequeueAfter)
	require.Nil(t, env.mcpServerRecord(ctx, t, got.UID), "the config is gone, so the record goes")
	require.Zero(t, kindMetric(t, "coder_k8s_cleanup_records_unmatched", "CoderMCPServer"))
}

// leaveWithoutConfigID leaves the record of an object whose create answer was
// lost, so the record has no config ID. With committed, the create committed
// in Coder. Otherwise Coder holds no config.
func (env mcpServerEnv) leaveWithoutConfigID(ctx context.Context, t *testing.T, committed bool) *coderv1alpha1.CoderMCPServer {
	t.Helper()
	env.keySecret(ctx, t, testMCPKey)
	env.fake.failNext(routeCreateMCPServer, fakeFault{AfterCommit: committed, Reset: true})
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, false, "ConfirmingCreate")
	env.leaveMCPServer(ctx, t, got)
	require.Empty(t, env.mcpServerRecord(ctx, t, got.UID)["coderID"])
	if committed {
		require.Len(t, env.fake.mcpServers(), 1)
	} else {
		require.Empty(t, env.fake.mcpServers())
	}
	return got
}
