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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// Plan S7, #422 item 1: with only a create marker, the controller deletes
// nothing. A person can edit the display name of a provider, so not even the
// temporary display name proves that this object created it.
func TestCoderAIProviderDeletePendingCreate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		displayName string // of a provider that holds the name in Coder; empty for none
	}{
		{name: "a person's provider with the temporary display name", displayName: "nonce"},
		{name: "nothing created"},
		{name: "someone else's provider", displayName: "Team OpenAI"},
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
			provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-3 * time.Minute)}
			require.NoError(t, k8sClient.Status().Update(ctx, provider))
			got, _ := env.deleteAIProvider(ctx, t, provider)
			require.Nil(t, got, "the finalizer went")
			require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
			_, found := env.fake.aiProvider(provider.Name)
			require.Equal(t, tc.displayName != "", found, "the provider stays")
		})
	}
}

// #422: when a person edits the display name of the provider of a lost
// create answer, the controller reports NameConflict and drops its create
// marker. A delete then keeps the record, so a sweep can report the provider
// for manual cleanup.
func TestCoderAIProviderDeleteKeepsRecordAfterNameConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{AfterCommit: true, Reset: true})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "ConfirmingCreate")
	_, err := env.fake.client(t, 2*time.Second).UpdateAIProvider(ctx, "openai", codersdk.UpdateAIProviderRequest{DisplayName: ptr.To("Renamed by a person")})
	require.NoError(t, err)
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, true, "NameConflict")
	require.Nil(t, got.Status.CreateAttemptTime)

	gone, _ := env.deleteAIProvider(ctx, t, got)
	require.Nil(t, gone)
	_, ok := env.fake.aiProvider("openai")
	require.True(t, ok, "the controller never deletes a provider that it cannot prove its own")
	require.NotNil(t, cleanupRecord(ctx, t, env.namespace, got.UID), "the record stays for a sweep")
}

// A 404 proves that no create of this object committed only when the
// operator user is a site owner. Otherwise the finalizer stays.
func TestCoderAIProviderDeletePendingCreateNeedsSiteOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	provider.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, provider))
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-3 * time.Minute)}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))
	env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{} })
	got, result := env.deleteAIProvider(ctx, t, provider)
	require.NotNil(t, got)
	require.Contains(t, requireAIProviderStep(t, got, false, "ConfirmingCreate"), "not a site owner")
	require.Equal(t, 60*time.Second, result.RequeueAfter, "a delete waits with the delete poll")
	require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
}

// A 404 right after a create request does not prove absence: the request
// can still commit. The finalizer stays until the settle window ends. A
// create that commits in the meantime stays in Coder: without its ID, nothing
// proves that it is this object's (#422 item 1).
func TestCoderAIProviderDeleteRecentCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	provider.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, provider))
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-30 * time.Second)}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))
	got, result := env.deleteAIProvider(ctx, t, provider)
	require.NotNil(t, got)
	require.Contains(t, requireAIProviderStep(t, got, false, "ConfirmingCreate"), "can still commit")
	require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
	require.Greater(t, result.RequeueAfter, time.Duration(0))
	require.LessOrEqual(t, result.RequeueAfter, 90*time.Second)

	// The create commits late. The next read after the window finds it.
	_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: "coder-k8s:" + string(provider.UID),
		BaseURL: provider.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
	})
	require.NoError(t, err)
	got.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-3 * time.Minute)}
	require.NoError(t, k8sClient.Status().Update(ctx, got))
	got, _ = env.reconcileDeleting(ctx, t, got)
	require.Nil(t, got, "the finalizer went")
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
	_, found := env.fake.aiProvider(provider.Name)
	require.True(t, found, "the late create stays")
}

// A name lookup that answers this object's provider without an ID is
// impossible. The reconcile fails fast, deletes nothing and keeps the
// finalizer. While the control plane is being deleted, the one attempt is
// over, so the finalizer goes.
func TestCoderAIProviderDeletePendingCreateAnswerWithoutID(t *testing.T) {
	t.Parallel()
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "control plane ready", true: "control plane being deleted"}[deleting], func(t *testing.T) {
			t.Parallel()
			testAIProviderDeleteAnswerWithoutID(t, deleting)
		})
	}
}

func testAIProviderDeleteAnswerWithoutID(t *testing.T, deleting bool) {
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: "coder-k8s:" + string(provider.UID),
		BaseURL: provider.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
	})
	require.NoError(t, err)
	provider.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, provider))
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-3 * time.Minute)}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))
	env.fake.failNext(routeAIProvider, fakeFault{Rewrite: func(answer any) any {
		p := answer.(codersdk.AIProvider)
		p.ID = uuid.Nil
		return p
	}})
	if deleting {
		holdControlPlane(ctx, t, env.namespace)
		got, _ := env.deleteAIProvider(ctx, t, provider)
		require.Nil(t, got, "the finalizer went")
		require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
		return
	}
	require.NoError(t, k8sClient.Delete(ctx, provider))
	_, err = env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.ErrorContains(t, err, "assertion failed: Coder answered AI provider openai without an ID")
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), provider))
	require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, provider.Finalizers)
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
	t.Run("control plane being deleted, operator token read fails", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		holdControlPlane(ctx, t, env.namespace)
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
		require.NoError(t, err)
		env.r.Client = interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == "coder-operator-token" {
					return apierrors.NewTimeoutError("the server was unable to return a response in the time allotted", 1)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		requests := env.fake.totalRequests()
		got, _ := env.deleteAIProvider(ctx, t, provider)
		require.Nil(t, got, "the finalizer went")
		require.Equal(t, requests, env.fake.totalRequests(), "no Coder request")
	})
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

// #323: status.providerID goes into /api/v2/ai/providers/{id}, which also
// takes a name, and the SDK resolves "..". A value that is not a canonical
// UUID fails an assertion before any Coder request, on the delete, sync and
// disable paths.
func TestCoderAIProviderForgedProviderID(t *testing.T) {
	t.Parallel()
	forged := map[string]string{
		"path traversal": "../../users/5d0c6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11",
		"name":           "openai",
		"braces":         "{5d0c6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11}",
		"upper case":     "5D0C6C2E-3A4D-4C55-9D2F-3F1B1F0C9A11",
		"nil UUID":       "00000000-0000-0000-0000-000000000000",
	}
	for _, path := range []string{"delete", "sync", "disable"} {
		for name, id := range forged {
			t.Run(path+"/"+name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				env, secret, provider := newReadyAIProvider(ctx, t)
				provider.Status.ProviderID = id
				require.NoError(t, k8sClient.Status().Update(ctx, provider))
				switch path {
				case "delete":
					require.NoError(t, k8sClient.Delete(ctx, provider))
				case "disable":
					editSecret(ctx, t, secret, func(s *corev1.Secret) { delete(s.Labels, "coder.com/credential") })
				}
				requests := env.fake.totalRequests()
				_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
				require.ErrorContains(t, err, "assertion failed: AI provider "+env.namespace+"/openai has status.providerID")
				require.ErrorContains(t, err, "which is not a canonical UUID")
				require.Equal(t, requests, env.fake.totalRequests(), "no Coder request")
				got := &coderv1alpha1.CoderAIProvider{}
				require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
				require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
			})
		}
	}
}

// #323: a delete answered 404 without Coder's JSON error, for example an
// HTML page of a proxy, does not prove that the provider is gone. The
// finalizer stays and the delete is retried.
func TestCoderAIProviderDeleteNotFoundFromProxy(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]fakeFault{
		"HTML page":     {Status: http.StatusNotFound, HTML: "<html>404 Not Found</html>"},
		"other message": {Status: http.StatusNotFound, Message: "Route not found."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newReadyAIProvider(ctx, t)
			env.fake.failNext(routeDeleteAIProvider, fault)
			got, result := env.deleteAIProvider(ctx, t, provider)
			require.NotNil(t, got)
			requireAIProviderStep(t, got, false, "CoderUnavailable")
			require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
			require.Equal(t, 60*time.Second, result.RequeueAfter)
			_, found := env.fake.aiProvider("openai")
			require.True(t, found)

			got, _ = env.reconcileDeleting(ctx, t, got)
			require.Nil(t, got, "Coder's own answer releases the finalizer")
			_, found = env.fake.aiProvider("openai")
			require.False(t, found)
		})
	}
}

// #323: with only a create marker, a lookup 404 that Coder did not answer
// proves nothing either.
func TestCoderAIProviderDeletePendingCreateNotFoundFromProxy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	provider.Finalizers = []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, provider))
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-3 * time.Minute)}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))
	env.fake.failNext(routeAIProvider, fakeFault{Status: http.StatusNotFound, HTML: "<html>404 Not Found</html>"})
	got, _ := env.deleteAIProvider(ctx, t, provider)
	require.NotNil(t, got)
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
	got, _ = env.reconcileDeleting(ctx, t, got)
	require.Nil(t, got, "Coder's own 404 releases the finalizer")
}
