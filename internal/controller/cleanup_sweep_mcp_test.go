package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
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

// #353 invariants 5 and 6: a sweep never touches the config of a living
// object, finds a config without a recorded ID only by its slug and nonce,
// and leaves a config with the slug that the object did not create.
func TestCleanupSweepMCPServerCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("living object", func(t *testing.T) {
		t.Parallel()
		env := newMCPServerEnv(ctx, t, true)
		env.keySecret(ctx, t, testMCPKey)
		got, _ := env.applied(ctx, t, nil)
		s := newMCPSweep(t, env)
		s.clock.Step(time.Hour)
		requests := env.fake.totalRequests()
		require.Equal(t, 30*time.Minute, s.sweep(ctx, t, got.UID).RequeueAfter)
		require.Equal(t, requests, env.fake.totalRequests(), "no Coder call for a living object")
		require.NotNil(t, env.mcpServerRecord(ctx, t, got.UID))
	})
	for name, tc := range map[string]struct {
		replace bool // Someone else's config takes the slug.
	}{
		"pending create, found by its nonce": {},
		"slug taken by another config":       {replace: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			env.fake.failNext(routeCreateMCPServer, fakeFault{AfterCommit: true, Reset: true})
			got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
			requireMCPServerStep(t, got, false, "ConfirmingCreate")
			env.leaveMCPServer(ctx, t, got)
			require.Empty(t, env.mcpServerRecord(ctx, t, got.UID)["coderID"])
			if tc.replace {
				for _, c := range env.fake.mcpServers() {
					require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, c.view.ID))
				}
				createOther(ctx, t, env, got)
			}
			deletes := env.fake.requestCount(routeDeleteMCPServer)
			createTestControlPlane(ctx, t, env.namespace, "coder-b", env.fake.server.URL)
			s := newMCPSweep(t, env)
			s.clock.Step(6 * time.Minute)
			require.Zero(t, s.sweep(ctx, t, got.UID).RequeueAfter)
			require.Nil(t, env.mcpServerRecord(ctx, t, got.UID))
			if tc.replace {
				require.Equal(t, deletes, env.fake.requestCount(routeDeleteMCPServer), "the other config is never deleted")
				require.Len(t, env.fake.mcpServers(), 1)
				return
			}
			require.Equal(t, deletes+1, env.fake.requestCount(routeDeleteMCPServer))
			require.Empty(t, env.fake.mcpServers(), "the config with the nonce is deleted")
		})
	}
}
