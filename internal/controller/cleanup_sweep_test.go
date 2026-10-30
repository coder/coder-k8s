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
	"k8s.io/utils/ptr"
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
	s := sweepEnv{aiProviderEnv: env, provider: provider, clock: clocktesting.NewFakeClock(time.Now()), recorder: events.NewFakeRecorder(10)}
	s.sweeper = &controller.CleanupRecordReconciler{Client: base, APIReader: k8sClient, OperatorNamespace: env.namespace, Clock: s.clock, Recorder: s.recorder}
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
// object, and deletes only through a control plane of the record's
// deployment.
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
	t.Run("control plane in another namespace", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		// Another namespace runs a control plane that answers the record's
		// deployment ID, as a spoofing Coder could.
		createTestControlPlane(ctx, t, createTestNamespace(ctx, t, "aip-other"), "coder", s.fake.server.URL)
		s.clock.Step(6 * time.Minute)
		require.Equal(t, 10*time.Minute, s.sweep(ctx, t).RequeueAfter)
		require.Zero(t, s.fake.requestCount(routeDeleteAIProvider), "only a control plane in the object's namespace sweeps")
		require.NotNil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID))
	})
	t.Run("first matching control plane has no site owner", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		other := newFakeCoder(t) // A second Coder on the same database: same deployment, its own users.
		other.mu.Lock()
		other.deploymentID = s.fake.deploymentID
		other.mu.Unlock()
		other.updateUser(other.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{} })
		createTestControlPlane(ctx, t, s.namespace, "coder-a", other.server.URL)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		s.clock.Step(6 * time.Minute)
		require.Zero(t, s.sweep(ctx, t).RequeueAfter, "the next matching control plane sweeps")
		_, ok := s.fake.aiProvider("openai")
		require.False(t, ok)
	})
	t.Run("stale cache of the record", func(t *testing.T) {
		t.Parallel()
		s := newSweepEnv(ctx, t)
		createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
		// A newer create attempt, written right before; a stale cache would
		// still show the old time.
		configMap := &corev1.ConfigMap{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: "ck8s-cleanup-" + string(s.provider.UID)}, configMap))
		old := configMap.DeepCopy()
		configMap.Data["createAttemptTime"] = s.clock.Now().Add(6 * time.Minute).UTC().Format(time.RFC3339)
		require.NoError(t, k8sClient.Update(ctx, configMap))
		cached := s.sweeper.Client
		s.sweeper.Client = interceptor.NewClient(cached.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok {
					old.DeepCopyInto(cm)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		s.clock.Step(6 * time.Minute)
		require.Greater(t, s.sweep(ctx, t).RequeueAfter, time.Duration(0), "the sweep waits for the settle window after the newest create")
		_, ok := s.fake.aiProvider("openai")
		require.True(t, ok)
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

// #422 item 1: without a recorded provider ID, a sweep deletes nothing. A
// person can edit the display name of a provider, so the temporary display
// name proves nothing. Coder cannot change a provider's name or type, so the
// record goes only when Coder holds no provider of that name and type.
// Otherwise it stays for manual cleanup.
func TestCleanupSweepAIProviderWithoutProviderID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		committed bool                                                                           // The create commits, and its answer is lost.
		person    func(context.Context, *testing.T, sweepEnv, *codersdk.CreateAIProviderRequest) // A person's change in Coder after the object went.
		kept      bool
	}{
		"create never committed": {},
		"lost create answer":     {committed: true, kept: true},
		"lost create answer, display name edited": {committed: true, kept: true, person: func(ctx context.Context, t *testing.T, s sweepEnv, _ *codersdk.CreateAIProviderRequest) {
			_, err := s.fake.client(t, 2*time.Second).UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{DisplayName: ptr.To("Renamed by a person")})
			require.NoError(t, err)
		}},
		"a person's provider with the temporary display name": {kept: true, person: createPersonsAIProvider},
		"a person's provider of another type": {person: func(ctx context.Context, t *testing.T, s sweepEnv, req *codersdk.CreateAIProviderRequest) {
			req.Type, req.DisplayName = codersdk.AIProviderTypeAnthropic, "Team Anthropic"
			createPersonsAIProvider(ctx, t, s, req)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := leaveAIProviderWithoutID(ctx, t, tc.committed)
			if tc.person != nil {
				tc.person(ctx, t, s, &codersdk.CreateAIProviderRequest{
					Type: codersdk.AIProviderTypeOpenAI, Name: "openai", DisplayName: "coder-k8s:" + string(s.provider.UID),
					BaseURL: s.provider.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
				})
			}
			_, before := s.fake.aiProvider("openai")
			deletes := s.fake.requestCount(routeDeleteAIProvider)
			createTestControlPlane(ctx, t, s.namespace, "coder-b", s.fake.server.URL)
			s.clock.Step(6 * time.Minute)

			result := s.sweep(ctx, t)
			require.Equal(t, deletes, s.fake.requestCount(routeDeleteAIProvider), "a record without a provider ID deletes nothing")
			_, after := s.fake.aiProvider("openai")
			require.Equal(t, before, after)
			if !tc.kept {
				require.Zero(t, result.RequeueAfter)
				require.Nil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID), "Coder holds no provider that can be the object's")
				require.Empty(t, s.recorder.Events)
				return
			}
			require.Equal(t, 10*time.Minute, result.RequeueAfter)
			require.NotNil(t, cleanupRecord(ctx, t, s.namespace, s.provider.UID), "a provider can still be the object's, so the record stays")
			require.Len(t, s.recorder.Events, 1)
			require.Contains(t, <-s.recorder.Events, "ManualCleanupNeeded")
		})
	}
}

// #422 item 1: an object with an unconfirmed create is deleted while a
// person's provider of its name carries its temporary display name. The
// delete and the sweep both leave that provider, the record waits for manual
// cleanup, and it goes once the person deletes the provider. Not parallel: it
// reads process-wide metrics.
func TestCoderAIProviderTemporaryDisplayNameDeletesNothing(t *testing.T) {
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{Reset: true})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "ConfirmingCreate")
	_, committed := env.fake.aiProvider("openai")
	require.False(t, committed, "the create never committed")
	s := newSweeper(t, env, got)
	createPersonsAIProvider(ctx, t, s, &codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: "openai", DisplayName: "coder-k8s:" + string(got.UID),
		BaseURL: got.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
	})

	gone, _ := env.deleteAIProvider(ctx, t, got)
	require.Nil(t, gone, "the finalizer goes without a delete")
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider), "a display name never proves ownership")
	_, ok := env.fake.aiProvider("openai")
	require.True(t, ok, "the person's provider stays")
	require.NotNil(t, cleanupRecord(ctx, t, env.namespace, got.UID), "the record stays for a sweep")

	s.clock.Step(6 * time.Minute)
	require.Equal(t, 10*time.Minute, s.sweep(ctx, t).RequeueAfter)
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
	require.Contains(t, <-s.recorder.Events, "ManualCleanupNeeded")
	require.Equal(t, 1.0, providerMetric(t, "coder_k8s_cleanup_records_unmatched"))

	stored, _ := env.fake.aiProvider("openai")
	require.NoError(t, env.fake.client(t, 2*time.Second).DeleteAIProvider(ctx, stored.id.String()))
	require.Zero(t, s.sweep(ctx, t).RequeueAfter)
	require.Nil(t, cleanupRecord(ctx, t, env.namespace, got.UID), "the provider is gone, so the record goes")
	require.Zero(t, providerMetric(t, "coder_k8s_cleanup_records_unmatched"))
}

// leaveAIProviderWithoutID leaves the record of an object whose create answer
// was lost, so the record has no provider ID: the control plane goes, then
// the object. With committed, the create committed in Coder. Otherwise Coder
// holds no provider.
func leaveAIProviderWithoutID(ctx context.Context, t *testing.T, committed bool) sweepEnv {
	t.Helper()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{AfterCommit: committed, Reset: true})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "ConfirmingCreate")
	require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "coder"}}))
	gone, _ := env.deleteAIProvider(ctx, t, got)
	require.Nil(t, gone)
	require.Empty(t, cleanupRecord(ctx, t, env.namespace, got.UID)["coderID"])
	_, ok := env.fake.aiProvider("openai")
	require.Equal(t, committed, ok)
	return newSweeper(t, env, got)
}

func createPersonsAIProvider(ctx context.Context, t *testing.T, s sweepEnv, req *codersdk.CreateAIProviderRequest) {
	t.Helper()
	_, err := s.fake.client(t, 2*time.Second).CreateAIProvider(ctx, *req)
	require.NoError(t, err)
}
