package controller_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
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
		"deploymentID": env.fake.deploymentID, "nonce": "coder-k8s:" + string(got.UID), "coderID": stored.id.String(),
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
