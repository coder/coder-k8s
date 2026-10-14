package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// deleteAIProvider deletes the object and runs one reconcile. It returns
// the object, or nil when the finalizer went and the object is gone.
func (env aiProviderEnv) deleteAIProvider(ctx context.Context, t *testing.T, provider *coderv1alpha1.CoderAIProvider) (*coderv1alpha1.CoderAIProvider, ctrl.Result) {
	t.Helper()
	require.NoError(t, client.IgnoreNotFound(k8sClient.Delete(ctx, provider)))
	return env.reconcileDeleting(ctx, t, provider)
}

func (env aiProviderEnv) reconcileDeleting(ctx context.Context, t *testing.T, provider *coderv1alpha1.CoderAIProvider) (*coderv1alpha1.CoderAIProvider, ctrl.Result) {
	t.Helper()
	result, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.NoError(t, err)
	got := &coderv1alpha1.CoderAIProvider{}
	err = k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got)
	if apierrors.IsNotFound(err) {
		return nil, result
	}
	require.NoError(t, err)
	return got, result
}

// holdControlPlane puts a finalizer on the control plane and deletes it, so
// it stays Terminating like a control plane whose own cleanup still runs.
func holdControlPlane(ctx context.Context, t *testing.T, namespace string) {
	t.Helper()
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	key := client.ObjectKey{Namespace: namespace, Name: "coder"}
	require.NoError(t, k8sClient.Get(ctx, key, controlPlane))
	controlPlane.Finalizers = append(controlPlane.Finalizers, "example.com/hold")
	require.NoError(t, k8sClient.Update(ctx, controlPlane))
	require.NoError(t, k8sClient.Delete(ctx, controlPlane))
	t.Cleanup(func() {
		if k8sClient.Get(context.Background(), key, controlPlane) == nil {
			controlPlane.Finalizers = nil
			_ = k8sClient.Update(context.Background(), controlPlane)
		}
	})
}

// Plan S7 and 4.6: the controller deletes by the recorded ID, and 204 or 404
// releases the finalizer. A failed delete keeps the finalizer and retries
// every 60 seconds with no deadline.
func TestCoderAIProviderDelete(t *testing.T) {
	t.Parallel()
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		got, _ := env.deleteAIProvider(ctx, t, provider)
		require.Nil(t, got, "the finalizer went")
		require.Equal(t, 1, env.fake.requestCount(routeDeleteAIProvider))
		_, found := env.fake.aiProvider("openai")
		require.False(t, found)
	})
	t.Run("already gone in Coder", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		require.NoError(t, env.fake.client(t, 2*time.Second).DeleteAIProvider(ctx, provider.Status.ProviderID))
		got, _ := env.deleteAIProvider(ctx, t, provider)
		require.Nil(t, got, "a 404 releases the finalizer")
		require.Equal(t, 2, env.fake.requestCount(routeDeleteAIProvider))
	})
	t.Run("Coder unreachable", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		env.fake.failNext(routeDeleteAIProvider, fakeFault{Status: http.StatusBadGateway})
		got, result := env.deleteAIProvider(ctx, t, provider)
		require.NotNil(t, got)
		require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
		require.Contains(t, requireAIProviderStep(t, got, false, "CoderUnavailable"), "delete provider openai")
		require.Equal(t, 60*time.Second, result.RequeueAfter)
		_, found := env.fake.aiProvider("openai")
		require.True(t, found)

		got, _ = env.reconcileDeleting(ctx, t, got)
		require.Nil(t, got)
		require.Equal(t, 2, env.fake.requestCount(routeDeleteAIProvider))
	})
	t.Run("control plane not ready", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		controlPlane := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, controlPlane))
		controlPlane.Status.OperatorAccessReady = false
		require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
		requests := env.fake.totalRequests()
		got, result := env.deleteAIProvider(ctx, t, provider)
		require.NotNil(t, got)
		requireAIProviderStep(t, got, false, "OperatorAccessNotReady")
		require.Equal(t, 60*time.Second, result.RequeueAfter)
		require.Equal(t, requests, env.fake.totalRequests(), "no Coder request")
	})
}

// Plan S7: with only a create marker, a read decides. The controller deletes
// a provider that carries this object's nonce, and never one that does not.
func TestCoderAIProviderDeletePendingCreate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		displayName string // of a provider that holds the name in Coder; empty for none
		deletes     int
	}{
		{name: "create committed", displayName: "nonce", deletes: 1},
		{name: "nothing created", deletes: 0},
		{name: "someone else's provider", displayName: "Team OpenAI", deletes: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newAIProviderCreateEnv(ctx, t)
			if tc.displayName != "" {
				displayName := tc.displayName
				if displayName == "nonce" {
					displayName = "coder-k8s:" + string(provider.UID)
				}
				_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
					Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: displayName,
					BaseURL: provider.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
				})
				require.NoError(t, err)
			}
			provider.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
			require.NoError(t, k8sClient.Update(ctx, provider))
			provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now()}
			require.NoError(t, k8sClient.Status().Update(ctx, provider))
			got, _ := env.deleteAIProvider(ctx, t, provider)
			require.Nil(t, got, "the finalizer went")
			require.Equal(t, tc.deletes, env.fake.requestCount(routeDeleteAIProvider))
			_, found := env.fake.aiProvider(provider.Name)
			require.Equal(t, tc.displayName != "" && tc.deletes == 0, found, "only this object's provider is deleted")
		})
	}
}

// A 404 proves that no create of this object committed only when the
// operator user is a site owner. Otherwise the finalizer stays.
func TestCoderAIProviderDeletePendingCreateNeedsSiteOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	provider.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, provider))
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now()}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))
	env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{} })
	got, _ := env.deleteAIProvider(ctx, t, provider)
	require.NotNil(t, got)
	require.Contains(t, requireAIProviderStep(t, got, false, "ConfirmingCreate"), "not a site owner")
	require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
}

// Plan D12 and S7: when the control plane is gone, nothing can delete the
// provider, so the finalizer goes at once with zero Coder requests. While the
// control plane is being deleted with working operator access, the
// controller sends exactly one bounded delete, and then releases the
// finalizer whatever the answer. A namespace deletion therefore finishes.
func TestCoderAIProviderDeleteWithControlPlaneGoing(t *testing.T) {
	t.Parallel()
	t.Run("control plane gone", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		// The namespace controller deletes every object in the namespace.
		require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "coder"}}))
		requests := env.fake.totalRequests()
		got, _ := env.deleteAIProvider(ctx, t, provider)
		require.Nil(t, got, "the finalizer went")
		require.Equal(t, requests, env.fake.totalRequests(), "no Coder request")
		_, found := env.fake.aiProvider("openai")
		require.True(t, found, "the provider stays in Coder")
	})
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "control plane being deleted", true: "control plane being deleted, delete fails"}[fail], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newReadyAIProvider(ctx, t)
			holdControlPlane(ctx, t, env.namespace)
			if fail {
				env.fake.failNext(routeDeleteAIProvider, fakeFault{Status: http.StatusBadGateway})
			}
			got, _ := env.deleteAIProvider(ctx, t, provider)
			require.Nil(t, got, "the finalizer went")
			require.Equal(t, 1, env.fake.requestCount(routeDeleteAIProvider), "exactly one delete attempt")
			_, found := env.fake.aiProvider("openai")
			require.Equal(t, fail, found)
		})
	}
	t.Run("control plane being deleted without operator access", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		controlPlane := &coderv1alpha1.CoderControlPlane{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, controlPlane))
		controlPlane.Status.OperatorAccessReady = false
		require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
		holdControlPlane(ctx, t, env.namespace)
		requests := env.fake.totalRequests()
		got, _ := env.deleteAIProvider(ctx, t, provider)
		require.Nil(t, got, "the finalizer went")
		require.Equal(t, requests, env.fake.totalRequests(), "no Coder request")
	})
}
