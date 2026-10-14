package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

const workspaceRBACFinalizer = "coder.com/workspace-rbac-cleanup"

// deleteFinalizedControlPlane gives the control plane its own finalizer, as
// its reconciler does, and deletes it, so it stays Terminating.
func deleteFinalizedControlPlane(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	key := client.ObjectKey{Namespace: namespace, Name: name}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, key, controlPlane))
	controlPlane.Finalizers = append(controlPlane.Finalizers, workspaceRBACFinalizer)
	require.NoError(t, k8sClient.Update(ctx, controlPlane))
	require.NoError(t, k8sClient.Delete(ctx, controlPlane))
	t.Cleanup(func() {
		if k8sClient.Get(context.Background(), key, controlPlane) == nil {
			controlPlane.Finalizers = nil
			_ = k8sClient.Update(context.Background(), controlPlane)
		}
	})
}

func releasedWithoutDelete(t *testing.T, kind string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "coder_k8s_cleanup_released_without_delete_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "kind" && l.GetValue() == kind {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// #353: a control plane that is being deleted keeps its finalizer, and so its
// Coder Deployment, while a CoderAIGatewayKey or CoderAIProvider uses it. A
// dependent of another control plane does not hold it. A namespace deletion
// does not wait: it removes Coder's pods anyway.
func TestControlPlaneWaitsForDependents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reconcileGone := func(t *testing.T, namespace string) (ctrl.Result, bool) {
		t.Helper()
		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: "coder"}})
		require.NoError(t, err)
		err = k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "coder"}, &coderv1alpha1.CoderControlPlane{})
		return result, apierrors.IsNotFound(err)
	}
	t.Run("dependents", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
		other := env.createNamedKey(ctx, t, "other", "another-control-plane", "other-secret")
		provider := &coderv1alpha1.CoderAIProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: env.namespace},
			Spec: coderv1alpha1.CoderAIProviderSpec{
				ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"}, Type: "openai", BaseURL: "https://api.openai.com/v1",
				APIKeySecretRef: coderv1alpha1.SecretKeySelector{Name: "openai-key", Key: "api-key"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, provider))
		deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")

		result, gone := reconcileGone(t, env.namespace)
		require.False(t, gone, "the control plane waits for its key and provider")
		require.Equal(t, 10*time.Second, result.RequeueAfter)
		require.NoError(t, k8sClient.Delete(ctx, key))
		_, gone = reconcileGone(t, env.namespace)
		require.False(t, gone, "the control plane waits for its provider")
		require.NoError(t, k8sClient.Delete(ctx, provider))
		_, gone = reconcileGone(t, env.namespace)
		require.True(t, gone, "a key of another control plane does not hold this one")
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(other), other))
	})
	t.Run("namespace deletion", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		env.createKey(ctx, t, "coder", testAIGatewaySecret)
		deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")
		require.NoError(t, k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.namespace}}))
		_, gone := reconcileGone(t, env.namespace)
		require.True(t, gone, "a namespace deletion does not wait")
	})
}

// #353: while the control plane holds for it, a deleted key takes the normal
// path: it waits for the settle window, then deletes every key, instead of
// one bounded pass. A control plane without operator access makes it wait
// and retry (item q), not release.
func TestCoderAIGatewayKeyDeleteUnderControlPlaneHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got := rotatedKey(ctx, t, &env)
	deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")
	require.NoError(t, k8sClient.Delete(ctx, got))

	got, result := env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingKeys")
	require.Equal(t, 5*time.Minute, result.RequeueAfter, "the settle window, not the bounded pass")
	require.Len(t, env.fake.aiGatewayKeys(), 2)

	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, controlPlane))
	controlPlane.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
	env.clock.Step(5 * time.Minute)
	got, result = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "OperatorAccessNotReady")
	require.Equal(t, time.Minute, result.RequeueAfter)
	require.Len(t, env.fake.aiGatewayKeys(), 2, "no release without a delete")

	controlPlane.Status.OperatorAccessReady = true
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
	gone, _ := env.reconcile(ctx, t, got)
	require.Nil(t, gone)
	require.Empty(t, env.fake.aiGatewayKeys())
}

// #353: while the control plane holds for it, a failed provider delete
// retries instead of releasing after one attempt.
func TestCoderAIProviderDeleteUnderControlPlaneHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")
	env.fake.failNext(routeDeleteAIProvider, fakeFault{Status: http.StatusBadGateway})
	got, result := env.deleteAIProvider(ctx, t, provider)
	require.NotNil(t, got, "a failed delete keeps the finalizer while the control plane waits")
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	require.Equal(t, 60*time.Second, result.RequeueAfter)
	got, _ = env.reconcileDeleting(ctx, t, got)
	require.Nil(t, got)
	_, found := env.fake.aiProvider("openai")
	require.False(t, found)
	require.Equal(t, 2, env.fake.requestCount(routeDeleteAIProvider))
}

// #353: a release without a delete in Coder counts once in
// coder_k8s_cleanup_released_without_delete_total, also when the finalizer
// patch fails and the release runs again. Not parallel: other tests release
// keys too.
func TestReleasedWithoutDeleteMetric(t *testing.T) {
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got := rotatedKey(ctx, t, &env)
	before := releasedWithoutDelete(t, "CoderAIGatewayKey")
	require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "coder"}}))
	require.NoError(t, k8sClient.Delete(ctx, got))
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	failed := false
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*coderv1alpha1.CoderAIGatewayKey); ok && !failed {
				failed = true
				return apierrors.NewConflict(schema.GroupResource{Resource: "coderaigatewaykeys"}, obj.GetName(), nil)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	_, _, err = env.reconcileErr(ctx, t, got)
	require.Error(t, err, "the finalizer patch fails once")
	require.Equal(t, before, releasedWithoutDelete(t, "CoderAIGatewayKey"), "no count before the finalizer is gone")
	gone, _ := env.reconcile(ctx, t, got)
	require.Nil(t, gone)
	require.Equal(t, before+1, releasedWithoutDelete(t, "CoderAIGatewayKey"))
}

// #353: a foreground deletion removes the Coder Deployment before the
// control plane, so the control plane does not hold for its dependents.
func TestControlPlaneForegroundDeletionDoesNotHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	env.createKey(ctx, t, "coder", testAIGatewaySecret)
	key := client.ObjectKey{Namespace: env.namespace, Name: "coder"}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, key, controlPlane))
	controlPlane.Finalizers = append(controlPlane.Finalizers, workspaceRBACFinalizer)
	require.NoError(t, k8sClient.Update(ctx, controlPlane))
	require.NoError(t, k8sClient.Delete(ctx, controlPlane, client.PropagationPolicy(metav1.DeletePropagationForeground)))
	t.Cleanup(func() {
		if k8sClient.Get(context.Background(), key, controlPlane) == nil {
			controlPlane.Finalizers = nil
			_ = k8sClient.Update(context.Background(), controlPlane)
		}
	})
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, key, controlPlane))
	require.NotContains(t, controlPlane.Finalizers, workspaceRBACFinalizer, "no hold under foreground deletion")
}

// #353: when the hold ends, the control plane stays for a short grace period
// while dependents remain, and a dependent under the hold requeues before the
// hold ends, so it runs its bounded pass while Coder still runs.
func TestControlPlaneHoldEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got := rotatedKey(ctx, t, &env)
	deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, controlPlane))
	end := controlPlane.DeletionTimestamp.Add(10 * time.Minute)

	controlPlane.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
	require.NoError(t, k8sClient.Delete(ctx, got))
	env.clock.SetTime(end.Add(-30 * time.Second))
	got, result := env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "OperatorAccessNotReady")
	require.Equal(t, 31*time.Second, result.RequeueAfter, "the dependent looks again right after the hold ends")

	ccpClock := clocktesting.NewFakeClock(end.Add(10 * time.Second))
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, Clock: ccpClock}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(controlPlane)})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(controlPlane), controlPlane), "the control plane stays in the grace period")
	ccpClock.SetTime(end.Add(31 * time.Second))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(controlPlane)})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(controlPlane), controlPlane), "each dependent adds one bounded pass to the grace period")
	ccpClock.SetTime(end.Add(41 * time.Second))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(controlPlane)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(controlPlane), controlPlane)), "after the grace period it goes")
}

// #353: a dependent that is not being deleted does no cleanup, so it holds a
// deleted control plane only for 30 s, which covers one command that deletes
// both. A dependent that is being deleted holds it for the whole hold.
func TestControlPlaneLiveDependentWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	env.createKey(ctx, t, "coder", testAIGatewaySecret)
	deleting := env.createNamedKey(ctx, t, "deleting", "coder", "deleting-secret")
	deleting.Finalizers = []string{coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, deleting))
	removeFinalizer := func() {
		if k8sClient.Get(context.Background(), client.ObjectKeyFromObject(deleting), deleting) == nil {
			deleting.Finalizers = nil
			require.NoError(t, k8sClient.Update(context.Background(), deleting))
		}
	}
	t.Cleanup(removeFinalizer)
	// A deleted key whose cleanup finalizer is gone, held only by another
	// finalizer, does not hold the control plane.
	foreign := env.createNamedKey(ctx, t, "foreign", "coder", "foreign-secret")
	foreign.Finalizers = []string{"example.com/keep"}
	require.NoError(t, k8sClient.Update(ctx, foreign))
	t.Cleanup(func() {
		if k8sClient.Get(context.Background(), client.ObjectKeyFromObject(foreign), foreign) == nil {
			foreign.Finalizers = nil
			_ = k8sClient.Update(context.Background(), foreign)
		}
	})
	require.NoError(t, k8sClient.Delete(ctx, foreign))
	deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")
	ref := client.ObjectKey{Namespace: env.namespace, Name: "coder"}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, ref, controlPlane))
	ccpClock := clocktesting.NewFakeClock(controlPlane.DeletionTimestamp.Add(29 * time.Second))
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, Clock: ccpClock}
	gone := func() bool {
		t.Helper()
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ref})
		require.NoError(t, err)
		return apierrors.IsNotFound(k8sClient.Get(ctx, ref, &coderv1alpha1.CoderControlPlane{}))
	}
	require.False(t, gone(), "live keys hold the control plane for 30 s")
	require.NoError(t, k8sClient.Delete(ctx, deleting))
	ccpClock.SetTime(controlPlane.DeletionTimestamp.Add(31 * time.Second))
	require.False(t, gone(), "a key that is being deleted still holds it")
	removeFinalizer()
	require.True(t, gone(), "after 30 s a live key, or a deleted key without its cleanup finalizer, does not hold it")
}

// #353: a release that a failed finalizer patch left recorded does not count
// when the next reconcile deletes the keys in Coder. Not parallel: other
// tests release keys too.
func TestReleasedWithoutDeleteMetricRecovered(t *testing.T) {
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got := rotatedKey(ctx, t, &env)
	before := releasedWithoutDelete(t, "CoderAIGatewayKey")
	deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, controlPlane))
	controlPlane.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
	require.NoError(t, k8sClient.Delete(ctx, got))
	env.clock.SetTime(controlPlane.DeletionTimestamp.Add(10*time.Minute + time.Second)) // The hold has ended.
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	failed := false
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*coderv1alpha1.CoderAIGatewayKey); ok && !failed {
				failed = true
				return apierrors.NewConflict(schema.GroupResource{Resource: "coderaigatewaykeys"}, obj.GetName(), nil)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	_, _, err = env.reconcileErr(ctx, t, got)
	require.Error(t, err, "the release without a delete fails its finalizer patch")
	require.Len(t, env.fake.aiGatewayKeys(), 2)

	controlPlane.Status.OperatorAccessReady = true
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
	gone, _ := env.reconcile(ctx, t, got)
	require.Nil(t, gone)
	require.Empty(t, env.fake.aiGatewayKeys(), "the retry deletes the keys in Coder")
	require.Equal(t, before, releasedWithoutDelete(t, "CoderAIGatewayKey"), "a release that deleted in Coder does not count")
}

// #353: the hold keeps the Coder Deployment and its service account, so the
// control plane revokes the managed workspace RBAC before it waits.
func TestControlPlaneHoldRevokesWorkspaceRBAC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "hold-rbac")
	workspaceNamespace := createTestNamespace(ctx, t, "hold-rbac-ws")
	cp := createCoderControlPlaneUnstructured(ctx, t, "coder", namespace, map[string]any{
		"image":          "test-workspace-rbac:latest",
		"serviceAccount": map[string]any{"name": "coder-hold-sa"},
		"rbac":           map[string]any{"workspacePerms": true, "workspaceNamespaces": []any{workspaceNamespace}},
	})
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	ref := types.NamespacedName{Namespace: namespace, Name: cp.Name}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ref})
	require.NoError(t, err)
	bindingRef := types.NamespacedName{Namespace: workspaceNamespace, Name: expectedWorkspaceRoleBindingName(t, cp, "coder-hold-sa")}
	require.NoError(t, k8sClient.Get(ctx, bindingRef, &rbacv1.RoleBinding{}), "the reconcile grants workspace RBAC")

	key := &coderv1alpha1.CoderAIGatewayKey{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: namespace, Finalizers: []string{coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer}},
		Spec:       coderv1alpha1.CoderAIGatewayKeySpec{ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: cp.Name}, SecretName: "gateway-key"},
	}
	require.NoError(t, k8sClient.Create(ctx, key))
	t.Cleanup(func() {
		if k8sClient.Get(context.Background(), client.ObjectKeyFromObject(key), key) == nil {
			key.Finalizers = nil
			_ = k8sClient.Update(context.Background(), key)
		}
	})
	require.NoError(t, k8sClient.Delete(ctx, key))
	require.NoError(t, k8sClient.Delete(ctx, cp))
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ref})
	require.NoError(t, err)
	require.Equal(t, 10*time.Second, result.RequeueAfter, "the control plane holds for the key")
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, bindingRef, &rbacv1.RoleBinding{})), "the hold does not keep workspace RBAC")
}
