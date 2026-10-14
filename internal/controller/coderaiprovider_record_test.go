package controller_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// cleanupRecord returns the data of the cleanup record of uid in namespace,
// or nil when there is none.
func cleanupRecord(ctx context.Context, t *testing.T, namespace string, uid types.UID) map[string]string {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ck8s-cleanup-" + string(uid)}, configMap)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	require.Equal(t, "CoderAIProvider", configMap.Labels["coder.com/cleanup-record"])
	return configMap.Data
}

// #353 part 2: before its create, the controller records which Coder
// deployment holds the provider, and the provider ID before the sync replaces
// the nonce. The record holds no key. A confirmed delete in Coder removes it.
func TestCoderAIProviderCleanupRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)

	// Stop after the create, before the sync replaces the nonce.
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	stored, ok := env.fake.aiProvider("openai")
	require.True(t, ok)
	record := cleanupRecord(ctx, t, env.namespace, got.UID)
	require.NotEmpty(t, record["createAttemptTime"])
	delete(record, "createAttemptTime")
	require.Equal(t, map[string]string{
		"kind": "CoderAIProvider", "uid": string(got.UID), "namespace": env.namespace, "name": "openai",
		"deploymentID": env.fake.deploymentID, "nonce": "coder-k8s:" + string(got.UID), "coderID": stored.id.String(), "type": "openai",
	}, record, "the provider ID is recorded before the sync replaces the nonce")

	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	for _, v := range cleanupRecord(ctx, t, env.namespace, got.UID) {
		require.NotContains(t, v, testAIProviderKey, "a record holds no key")
	}

	gone, _ := env.deleteAIProvider(ctx, t, got)
	require.Nil(t, gone)
	_, ok = env.fake.aiProvider("openai")
	require.False(t, ok)
	require.Nil(t, cleanupRecord(ctx, t, env.namespace, got.UID), "Coder confirmed the delete, so the record goes")
}

// #353 invariant 1: without a record, the controller sends no create.
func TestCoderAIProviderNoCreateWithoutRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("record write fails", func(t *testing.T) {
		t.Parallel()
		env, _, provider := newAIProviderCreateEnv(ctx, t)
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
		require.NoError(t, err)
		env.r.Client = interceptor.NewClient(base, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return errors.New("injected record write failure")
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		_, err = env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
		require.ErrorContains(t, err, "injected record write failure")
		require.Zero(t, env.fake.requestCount(routeCreateAIProvider), "no create without a record")
		got := &coderv1alpha1.CoderAIProvider{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
		require.Nil(t, got.Status.CreateAttemptTime, "no create marker either")
	})
	t.Run("deployment ID unknown", func(t *testing.T) {
		t.Parallel()
		env, _, provider := newAIProviderCreateEnv(ctx, t)
		env.fake.failNext(routeBuildInfo, fakeFault{Status: http.StatusServiceUnavailable})
		got, _ := env.reconcile(ctx, t, provider)
		requireAIProviderStep(t, got, false, "CoderUnavailable")
		require.Zero(t, env.fake.requestCount(routeCreateAIProvider))
		require.Nil(t, cleanupRecord(ctx, t, env.namespace, got.UID))
	})
	t.Run("deployment ID not a UUID", func(t *testing.T) {
		t.Parallel()
		env, _, provider := newAIProviderCreateEnv(ctx, t)
		env.fake.mu.Lock()
		env.fake.deploymentID = "https://coder.example.com"
		env.fake.mu.Unlock()
		got, _ := env.reconcile(ctx, t, provider)
		requireAIProviderStep(t, got, false, "CoderUnavailable")
		require.Zero(t, env.fake.requestCount(routeCreateAIProvider))
	})
}

// #353: a release without a delete in Coder keeps the record, so a later
// sweep can delete the provider.
func TestCoderAIProviderRecordKeptWithoutDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "coder"}}))
	gone, _ := env.deleteAIProvider(ctx, t, provider)
	require.Nil(t, gone, "ControlPlaneGone releases the finalizer")
	_, ok := env.fake.aiProvider("openai")
	require.True(t, ok, "the provider stays in Coder")
	record := cleanupRecord(ctx, t, env.namespace, provider.UID)
	require.Equal(t, provider.Status.ProviderID, record["coderID"], "the record stays for a sweep")
}

// #353: a provider from before records existed gets one, with the deployment
// ID that Coder answers now.
func TestCoderAIProviderRecordBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	require.NoError(t, k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(provider.UID)}}))
	builds := env.fake.requestCount(routeBuildInfo)
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderReady(t, got)
	record := cleanupRecord(ctx, t, env.namespace, provider.UID)
	require.Equal(t, provider.Status.ProviderID, record["coderID"])
	require.Equal(t, env.fake.deploymentID, record["deploymentID"])
	require.Equal(t, builds+1, env.fake.requestCount(routeBuildInfo))
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider), "no new create")

	// A ConfigMap of that name that is not a record is never written over.
	configMap := &corev1.ConfigMap{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(provider.UID)}, configMap))
	configMap.Data["uid"] = strings.ToUpper(string(provider.UID))
	require.NoError(t, k8sClient.Update(ctx, configMap))
	_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.ErrorContains(t, err, "is not a valid cleanup record")
}

// #374 review: a provider from before records gets one also while its key
// Secret fails the credential contract and the controller disables it.
func TestCoderAIProviderRecordBackfillOnDisable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newReadyAIProvider(ctx, t)
	require.NoError(t, k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(provider.UID)}}))
	editSecret(ctx, t, secret, func(s *corev1.Secret) { delete(s.Labels, "coder.com/credential") })
	got, _ := env.reconcile(ctx, t, provider)
	stored, _ := env.fake.aiProvider("openai")
	require.False(t, stored.enabled, "the disable still runs")
	require.Equal(t, got.Status.ProviderID, cleanupRecord(ctx, t, env.namespace, provider.UID)["coderID"])
}

// #374 review: the controller reads records live. A cache that does not
// show the record yet must not make it create the record twice.
func TestCoderAIProviderRecordReadLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return apierrors.NewNotFound(corev1.Resource("configmaps"), key.Name) // A stale cache.
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderReady(t, got)
	require.Equal(t, got.Status.ProviderID, cleanupRecord(ctx, t, env.namespace, provider.UID)["coderID"])
}

// #374 review: a ConfigMap of the record's name that is not this object's
// record is never deleted.
func TestCoderAIProviderDeleteKeepsForeignConfigMap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	provider.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, provider))
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(provider.UID)}, Data: map[string]string{"owner": "someone else"}}
	require.NoError(t, k8sClient.Create(ctx, foreign))
	gone, _ := env.deleteAIProvider(ctx, t, provider)
	require.Nil(t, gone, "nothing of the object exists in Coder, so the finalizer goes")
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign), "the foreign ConfigMap stays")
}

// #374 review: when the status write after a create failed, the record holds
// the only provider ID. A delete uses it even after the nonce is gone.
func TestCoderAIProviderDeleteUsesRecordedID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	provider.Status.ProviderID = ""
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))
	env.fake.mu.Lock()
	for _, p := range env.fake.aiProviders {
		p.displayName = "Renamed in the dashboard"
	}
	env.fake.mu.Unlock()
	gone, _ := env.deleteAIProvider(ctx, t, provider)
	require.Nil(t, gone)
	_, ok := env.fake.aiProvider("openai")
	require.False(t, ok, "the recorded ID proves ownership")
	require.Nil(t, cleanupRecord(ctx, t, env.namespace, provider.UID))
}

// #374 review round 2: when the control plane now reaches another Coder
// deployment (a new database), a delete there proves nothing. The finalizer
// goes without a delete, and the record stays for a sweep.
func TestCoderAIProviderDeleteAfterDeploymentSwitch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	env.fake.mu.Lock()
	env.fake.deploymentID = uuid.NewString()
	env.fake.mu.Unlock()
	gone, _ := env.deleteAIProvider(ctx, t, provider)
	require.Nil(t, gone)
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider), "no delete in another deployment")
	require.Equal(t, provider.Status.ProviderID, cleanupRecord(ctx, t, env.namespace, provider.UID)["coderID"], "the record stays")
}

// The reconciler refuses to start without its cleanup record dependencies.
func TestCoderAIProviderSetupNeedsRecordDependencies(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]*controller.CoderAIProviderReconciler{
		"no operator namespace": {Client: k8sClient, Scheme: scheme, APIReader: k8sClient},
		"no API reader":         {Client: k8sClient, Scheme: scheme, OperatorNamespace: "coder-system"},
	} {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		require.NoError(t, err)
		require.ErrorContains(t, r.SetupWithManager(mgr), "assertion failed: AI provider reconciler needs", name)
	}
}

// #374 review round 3: when the control plane reaches another Coder
// deployment while the object lives, the provider looks gone, but the
// record still locates it in the old deployment. The controller keeps the
// record and creates nothing in the new deployment.
func TestCoderAIProviderDeploymentSwitchKeepsRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	before := cleanupRecord(ctx, t, env.namespace, provider.UID)
	env.fake.mu.Lock()
	env.fake.deploymentID = uuid.NewString() // A new database behind the same control plane.
	for id := range env.fake.aiProviders {
		delete(env.fake.aiProviders, id)
	}
	env.fake.mu.Unlock()
	provider.Spec.DisplayName = "OpenAI (edited)"
	require.NoError(t, k8sClient.Update(ctx, provider))
	got, _ := env.reconcile(ctx, t, provider)
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireAIProviderStep(t, got, true, "CoderDeploymentChanged"), before["deploymentID"])
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider), "no create in the new deployment")
	require.Equal(t, before, cleanupRecord(ctx, t, env.namespace, provider.UID), "the record keeps the old locator")

	// #374 review round 4: deleting the object in this state keeps the
	// record too, so a sweep in the old deployment can still find it.
	require.Empty(t, got.Status.ProviderID)
	gone, _ := env.deleteAIProvider(ctx, t, got)
	require.Nil(t, gone, "the finalizer goes without a delete")
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider), "no delete in the new deployment")
	require.Equal(t, before, cleanupRecord(ctx, t, env.namespace, provider.UID), "the record stays for a sweep")
}
