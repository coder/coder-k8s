package controller_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// keySweepEnv is a CoderAIGatewayKey that went with its control plane, as in
// a namespace deletion: Coder keeps its keys and the record stays. Coder also
// holds keys that are not the object's: a person's key, and a key of another
// object.
type keySweepEnv struct {
	aiGatewayKeyEnv
	key      *coderv1alpha1.CoderAIGatewayKey
	clock    *clocktesting.FakeClock
	recorder *events.FakeRecorder
	sweeper  *controller.CleanupRecordReconciler
}

func newKeySweepEnv(ctx context.Context, t *testing.T) keySweepEnv {
	t.Helper()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got := rotatedKey(ctx, t, &env)
	sdk := env.fake.client(t, 5*time.Second)
	for _, name := range []string{
		aiGatewayKeyPrefix(got) + "9", // An orphan of a create whose answer was lost.
		"personal-key",
		"ck8s-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-0",
	} {
		_, err := sdk.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: name})
		require.NoError(t, err)
	}
	require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "coder"}}))
	require.NoError(t, k8sClient.Delete(ctx, got))
	gone, _ := env.reconcile(ctx, t, got)
	require.Nil(t, gone, "ControlPlaneGone releases the finalizer")
	require.NotNil(t, env.aiGatewayKeyRecord(ctx, t, got.UID))
	require.Len(t, env.fake.aiGatewayKeys(), 5)
	return newKeySweeper(t, env, got)
}

func newKeySweeper(t *testing.T, env aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey) keySweepEnv {
	t.Helper()
	s := keySweepEnv{aiGatewayKeyEnv: env, key: key, clock: clocktesting.NewFakeClock(env.clock.Now()), recorder: events.NewFakeRecorder(10)}
	s.sweeper = &controller.CleanupRecordReconciler{Client: watchClient(t), APIReader: k8sClient, OperatorNamespace: env.records, Clock: s.clock, Recorder: s.recorder}
	return s
}

func (s keySweepEnv) sweep(ctx context.Context, t *testing.T) ctrl.Result {
	t.Helper()
	result, err := s.sweeper.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: s.records, Name: "ck8s-cleanup-" + string(s.key.UID)}})
	require.NoError(t, err)
	return result
}

// keysLeft returns the names of the keys that Coder holds.
func (s keySweepEnv) keysLeft() []string {
	var names []string
	for _, k := range s.fake.aiGatewayKeys() {
		names = append(names, k.name)
	}
	return names
}

// kindMetric reads a controller metric with the label kind.
func kindMetric(t *testing.T, name, kind string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if f.GetName() == name && l.GetName() == "kind" && l.GetValue() == kind {
					if m.GetGauge() != nil {
						return m.GetGauge().GetValue()
					}
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// #353, key half: after a namespace deletion, a new control plane that
// reaches the same Coder deployment deletes every key with the object's
// prefix, once the settle window has passed, and leaves every other key. A
// different deployment at the same URL never does. Not parallel: it reads
// process-wide metrics.
func TestCleanupSweepAIGatewayKeys(t *testing.T) {
	ctx := context.Background()
	s := newKeySweepEnv(ctx, t)
	const kind = "CoderAIGatewayKey"
	swept := kindMetric(t, "coder_k8s_cleanup_records_swept_total", kind)

	requests := s.fake.totalRequests()
	result := s.sweep(ctx, t)
	require.Equal(t, requests, s.fake.totalRequests(), "no Coder call inside the settle window")
	require.Greater(t, result.RequeueAfter, 4*time.Minute)
	require.LessOrEqual(t, result.RequeueAfter, 5*time.Minute, "it waits for the end of the settle window after the last create")
	s.clock.Step(6 * time.Minute)
	require.Equal(t, 10*time.Minute, s.sweep(ctx, t).RequeueAfter, "no control plane reaches the deployment")
	event := <-s.recorder.Events
	require.Contains(t, event, "NoMatchingDeployment")
	require.Contains(t, event, "the AI Gateway keys")
	require.Equal(t, 1.0, kindMetric(t, "coder_k8s_cleanup_records_unmatched", kind))

	// A new control plane at the same URL, but a new Coder database answers.
	createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
	recorded := s.fake.deploymentID
	s.fake.mu.Lock()
	s.fake.deploymentID = uuid.NewString()
	s.fake.mu.Unlock()
	require.Equal(t, 10*time.Minute, s.sweep(ctx, t).RequeueAfter)
	require.Zero(t, s.fake.requestCount(routeDeleteAIGatewayKey), "a different deployment is never swept")
	require.Len(t, s.fake.aiGatewayKeys(), 5)

	// The same deployment comes back.
	s.fake.mu.Lock()
	s.fake.deploymentID = recorded
	s.fake.mu.Unlock()
	require.Zero(t, s.sweep(ctx, t).RequeueAfter)
	left := s.keysLeft()
	require.Len(t, left, 2, "the person's key and the other object's key stay")
	require.Contains(t, left, "personal-key")
	for _, name := range left {
		require.False(t, strings.HasPrefix(name, aiGatewayKeyPrefix(s.key)), "every key of the object is gone: %s", name)
	}
	require.Equal(t, 3, s.fake.requestCount(routeDeleteAIGatewayKey), "the current, the retiring and the orphan key")
	require.Nil(t, s.aiGatewayKeyRecord(ctx, t, s.key.UID), "a LIST confirmed the delete, so the record goes")
	require.Equal(t, swept+1, kindMetric(t, "coder_k8s_cleanup_records_swept_total", kind))
	require.Zero(t, kindMetric(t, "coder_k8s_cleanup_records_unmatched", kind))
}

// #353 invariants 5 to 7 for keys: a sweep never touches the keys of a
// living object, keeps the record until a site owner's LIST shows no key of
// the object, and decides by UID that the object is gone.
func TestCleanupSweepAIGatewayKeyCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("living object", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
		s := newKeySweeper(t, env, got)
		s.clock.Step(time.Hour)
		requests := s.fake.totalRequests()
		require.Equal(t, 30*time.Minute, s.sweep(ctx, t).RequeueAfter)
		require.Equal(t, requests, s.fake.totalRequests(), "no Coder call for a living object")
		require.NotNil(t, s.aiGatewayKeyRecord(ctx, t, got.UID))
	})
	t.Run("same name, another object", func(t *testing.T) {
		t.Parallel()
		s := newKeySweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.createKey(ctx, t, "coder", testAIGatewaySecret) // The old name, a new UID.
		s.clock.Step(6 * time.Minute)
		require.Zero(t, s.sweep(ctx, t).RequeueAfter, "only the UID decides that the object is gone")
		require.Len(t, s.keysLeft(), 2)
	})
	t.Run("delete fails", func(t *testing.T) {
		t.Parallel()
		s := newKeySweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.fake.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusNotFound, HTML: "<html>not found</html>"})
		s.clock.Step(6 * time.Minute)
		require.Equal(t, time.Minute, s.sweep(ctx, t).RequeueAfter, "only Coder's own 404 confirms a delete")
		require.NotNil(t, s.aiGatewayKeyRecord(ctx, t, s.key.UID), "the record stays until Coder confirms")
		require.Len(t, s.keysLeft(), 5, "the sweep stops at the failed DELETE")
		require.Zero(t, s.sweep(ctx, t).RequeueAfter)
		require.Nil(t, s.aiGatewayKeyRecord(ctx, t, s.key.UID))
		require.Len(t, s.keysLeft(), 2)
	})
	t.Run("LIST still shows a key", func(t *testing.T) {
		t.Parallel()
		s := newKeySweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		// Both LISTs of the pass show a key that each DELETE answers 404 for,
		// as a Coder that lists a key it cannot delete would.
		ghost := codersdk.AIGatewayKey{ID: uuid.New(), Name: aiGatewayKeyPrefix(s.key) + "1"}
		for range 2 {
			s.fake.failNext(routeAIGatewayKeys, fakeFault{Rewrite: func(a any) any { return append(a.([]codersdk.AIGatewayKey), ghost) }})
		}
		s.clock.Step(6 * time.Minute)
		require.Equal(t, time.Minute, s.sweep(ctx, t).RequeueAfter, "only a LIST that shows no key of the object confirms")
		require.NotNil(t, s.aiGatewayKeyRecord(ctx, t, s.key.UID))
		require.Zero(t, s.sweep(ctx, t).RequeueAfter)
		require.Nil(t, s.aiGatewayKeyRecord(ctx, t, s.key.UID))
	})
	t.Run("operator not a site owner", func(t *testing.T) {
		t.Parallel()
		s := newKeySweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.fake.updateUser(s.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{} })
		s.clock.Step(6 * time.Minute)
		require.Equal(t, time.Minute, s.sweep(ctx, t).RequeueAfter)
		require.Zero(t, s.fake.requestCount(routeDeleteAIGatewayKey), "only a site owner's LIST shows every key")
		require.NotNil(t, s.aiGatewayKeyRecord(ctx, t, s.key.UID))
	})
	t.Run("control plane in another namespace", func(t *testing.T) {
		t.Parallel()
		s := newKeySweepEnv(ctx, t)
		createTestControlPlane(ctx, t, createTestNamespace(ctx, t, "aigk-other"), "coder", s.fake.server.URL)
		s.clock.Step(6 * time.Minute)
		require.Equal(t, 10*time.Minute, s.sweep(ctx, t).RequeueAfter)
		require.Zero(t, s.fake.requestCount(routeDeleteAIGatewayKey), "only a control plane in the object's namespace sweeps")
	})
}
