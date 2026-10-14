package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// sweepEnv is a provider that went with its control plane, as in a
// namespace deletion: Coder keeps the provider and the record stays.
type sweepEnv struct {
	aiProviderEnv
	provider *coderv1alpha1.CoderAIProvider
	clock    *clocktesting.FakeClock
	recorder *events.FakeRecorder
	sweeper  *controller.CleanupRecordReconciler
}

func newSweepEnv(ctx context.Context, t *testing.T) sweepEnv {
	t.Helper()
	env, _, provider := newReadyAIProvider(ctx, t)
	require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "coder"}}))
	gone, _ := env.deleteAIProvider(ctx, t, provider)
	require.Nil(t, gone)
	require.NotNil(t, cleanupRecord(ctx, t, env.namespace, provider.UID))
	return newSweeper(t, env, provider)
}

// newSweeper returns a sweeper for the record of provider in env.
func newSweeper(t *testing.T, env aiProviderEnv, provider *coderv1alpha1.CoderAIProvider) sweepEnv {
	t.Helper()
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	// Other tests run their own control planes: the sweeper of this test
	// lists only the control planes of its namespace.
	scoped := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return c.List(ctx, list, append(opts, client.InNamespace(env.namespace))...)
		},
	})
	s := sweepEnv{aiProviderEnv: env, provider: provider, clock: clocktesting.NewFakeClock(time.Now()), recorder: events.NewFakeRecorder(10)}
	s.sweeper = &controller.CleanupRecordReconciler{Client: scoped, APIReader: k8sClient, OperatorNamespace: env.namespace, Clock: s.clock, Recorder: s.recorder}
	return s
}

// providerMetric reads a controller metric with the label kind=CoderAIProvider.
func providerMetric(t *testing.T, name string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if f.GetName() == name && l.GetName() == "kind" && l.GetValue() == "CoderAIProvider" {
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

func (s sweepEnv) sweep(ctx context.Context, t *testing.T) ctrl.Result {
	t.Helper()
	result, err := s.sweeper.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: s.namespace, Name: "ck8s-cleanup-" + string(s.provider.UID)}})
	require.NoError(t, err)
	return result
}

// #353: after a namespace deletion, a new control plane that reaches the
// same Coder deployment deletes the leftover provider, once the settle
// window has passed. A different deployment at the same URL never does.
// Not parallel: it reads process-wide metrics.
func TestCleanupSweepAIProvider(t *testing.T) {
	ctx := context.Background()
	s := newSweepEnv(ctx, t)
	_, ok := s.fake.aiProvider("openai")
	require.True(t, ok, "the provider is left in Coder")
	swept := providerMetric(t, "coder_k8s_cleanup_records_swept_total")

	requests := s.fake.totalRequests()
	result := s.sweep(ctx, t)
	require.Equal(t, requests, s.fake.totalRequests(), "no Coder call inside the settle window")
	require.Greater(t, result.RequeueAfter, 4*time.Minute, "the settle window has not passed")
	require.LessOrEqual(t, result.RequeueAfter, 5*time.Minute, "it waits for the end of the settle window")
	deletes := s.fake.requestCount(routeDeleteAIProvider)
	s.clock.Step(6 * time.Minute)
	result = s.sweep(ctx, t)
	require.Equal(t, 10*time.Minute, result.RequeueAfter, "no control plane reaches the deployment")
	require.Equal(t, deletes, s.fake.requestCount(routeDeleteAIProvider))

	// A new control plane at the same URL, but another deployment answers:
	// for example a new Coder database behind the old address.
	createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
	recorded := s.fake.deploymentID
	s.fake.mu.Lock()
	s.fake.deploymentID = uuid.NewString()
	s.fake.mu.Unlock()
	result = s.sweep(ctx, t)
	require.Equal(t, 10*time.Minute, result.RequeueAfter)
	require.Equal(t, deletes, s.fake.requestCount(routeDeleteAIProvider), "a different deployment is never swept")
	require.NotNil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID))
	require.Contains(t, <-s.recorder.Events, "NoMatchingDeployment")
	require.Equal(t, 1.0, providerMetric(t, "coder_k8s_cleanup_records_unmatched"))

	// The same deployment comes back.
	s.fake.mu.Lock()
	s.fake.deploymentID = recorded
	s.fake.mu.Unlock()
	result = s.sweep(ctx, t)
	require.Zero(t, result.RequeueAfter)
	_, ok = s.fake.aiProvider("openai")
	require.False(t, ok, "the sweep deleted the provider")
	require.Equal(t, deletes+1, s.fake.requestCount(routeDeleteAIProvider))
	require.Nil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID), "Coder confirmed the delete, so the record goes")
	require.Equal(t, swept+1, providerMetric(t, "coder_k8s_cleanup_records_swept_total"))
	require.Zero(t, providerMetric(t, "coder_k8s_cleanup_records_unmatched"))
}

// #353 invariants 5 and 6: a sweep never touches the provider of a living
// object, finds a provider without a recorded ID only by its nonce, and
// leaves a provider of the same name that the object did not create.
func TestCleanupSweepAIProviderCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("living object", func(t *testing.T) {
		t.Parallel()
		env, _, provider := newReadyAIProvider(ctx, t)
		s := newSweeper(t, env, provider)
		s.clock.Step(time.Hour)
		requests := s.fake.totalRequests()
		require.Equal(t, 30*time.Minute, s.sweep(ctx, t).RequeueAfter)
		require.Equal(t, requests, s.fake.totalRequests(), "no Coder call for a living object")
		require.NotNil(t, cleanupRecord(ctx, t, s.namespace, provider.UID))
	})
	t.Run("same name, another object", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.createProvider(ctx, t, nil) // A new object with the old name and a new UID.
		s.clock.Step(6 * time.Minute)
		s.sweep(ctx, t)
		_, ok := s.fake.aiProvider("openai")
		require.False(t, ok, "only the UID decides that the object is gone")
	})
	for name, nonce := range map[string]bool{"nonce matches": true, "nonce differs": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSweepEnv(ctx, t)
			createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
			configMap := &corev1.ConfigMap{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: "ck8s-cleanup-" + string(s.provider.UID)}, configMap))
			configMap.Data["coderID"] = "" // The create committed, but its ID was never recorded.
			require.NoError(t, k8sClient.Update(ctx, configMap))
			if nonce {
				s.fake.mu.Lock()
				for _, p := range s.fake.aiProviders {
					p.displayName = "coder-k8s:" + string(s.provider.UID)
				}
				s.fake.mu.Unlock()
			}
			s.clock.Step(6 * time.Minute)
			require.Zero(t, s.sweep(ctx, t).RequeueAfter)
			_, ok := s.fake.aiProvider("openai")
			require.Equal(t, !nonce, ok, "only the provider with the record's nonce is deleted")
			require.Nil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID), "Coder holds no provider of the record")
		})
	}
	t.Run("delete fails", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.fake.failNext(routeDeleteAIProvider, fakeFault{Status: http.StatusBadGateway, HTML: "<html>bad gateway</html>"})
		s.clock.Step(6 * time.Minute)
		require.Equal(t, time.Minute, s.sweep(ctx, t).RequeueAfter)
		require.NotNil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID), "the record stays until Coder confirms the delete")
		s.fake.failNext(routeDeleteAIProvider, fakeFault{Status: http.StatusNotFound, HTML: "<html>not found</html>"})
		require.Equal(t, time.Minute, s.sweep(ctx, t).RequeueAfter, "only Coder's own 404 confirms")
		require.NotNil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID))
		require.Zero(t, s.sweep(ctx, t).RequeueAfter)
		require.Nil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID))
	})
	t.Run("deployment ID not answered yet", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.fake.failNext(routeBuildInfo, fakeFault{Status: http.StatusBadGateway, HTML: "<html>starting</html>"})
		s.clock.Step(6 * time.Minute)
		require.Equal(t, time.Minute, s.sweep(ctx, t).RequeueAfter, "a control plane that does not answer yet can still match")
		require.Empty(t, s.recorder.Events, "no NoMatchingDeployment Event for an unknown deployment")
		require.Zero(t, s.sweep(ctx, t).RequeueAfter)
		require.Nil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID))
	})
	t.Run("stale cache of the token Secret", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		cached := s.sweeper.Client
		s.sweeper.Client = interceptor.NewClient(cached.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name) // Not in the cache yet.
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		s.clock.Step(6 * time.Minute)
		require.Zero(t, s.sweep(ctx, t).RequeueAfter, "the sweep reads the operator access live")
		require.Nil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID))
	})
	t.Run("setup needs a manager", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		require.ErrorContains(t, s.sweeper.SetupWithManager(nil), "assertion failed: the cleanup record reconciler needs a manager")
	})
	t.Run("operator not a site owner", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.fake.updateUser(s.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{} })
		s.clock.Step(6 * time.Minute)
		require.Equal(t, time.Minute, s.sweep(ctx, t).RequeueAfter)
		require.Zero(t, s.fake.requestCount(routeDeleteAIProvider), "only a site owner can trust Coder's 404")
		require.NotNil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID))
	})
	t.Run("not a record", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		configMap := &corev1.ConfigMap{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: "ck8s-cleanup-" + string(s.provider.UID)}, configMap))
		configMap.Labels["coder.com/cleanup-record"] = "CoderAIGatewayKey"
		require.NoError(t, k8sClient.Update(ctx, configMap))
		s.clock.Step(6 * time.Minute)
		s.sweep(ctx, t)
		_, ok := s.fake.aiProvider("openai")
		require.True(t, ok, "a ConfigMap that is not a valid record is never acted on")
	})
}
