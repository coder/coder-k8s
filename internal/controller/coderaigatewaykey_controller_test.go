package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

const testAIGatewaySecret = "coder-ai-gateway-key"

// aiGatewayKeyEnv is one namespace with a fake Coder and a ready control
// plane named coder that points at it.
type aiGatewayKeyEnv struct {
	namespace string
	fake      *fakeCoder
	client    client.Client // The reconciler's client. Tests can wrap it.
}

func newAIGatewayKeyEnv(ctx context.Context, t *testing.T, allowKeys *bool) aiGatewayKeyEnv {
	t.Helper()
	env := aiGatewayKeyEnv{namespace: createTestNamespace(ctx, t, "aigk"), fake: newFakeCoder(t), client: k8sClient}
	cp := createTestControlPlane(ctx, t, env.namespace, "coder", env.fake.server.URL)
	if allowKeys != nil {
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(cp), cp))
		cp.Spec.AIGateway = &coderv1alpha1.AIGatewaySpec{AllowKeys: *allowKeys}
		require.NoError(t, k8sClient.Update(ctx, cp))
	}
	return env
}

func (env aiGatewayKeyEnv) createKey(ctx context.Context, t *testing.T, controlPlane, secretName string) *coderv1alpha1.CoderAIGatewayKey {
	t.Helper()
	key := &coderv1alpha1.CoderAIGatewayKey{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: env.namespace},
		Spec: coderv1alpha1.CoderAIGatewayKeySpec{
			ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: controlPlane}, SecretName: secretName,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, key))
	t.Cleanup(func() {
		_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(key), key)
		key.Finalizers = nil
		_ = k8sClient.Update(context.Background(), key)
		_ = k8sClient.Delete(context.Background(), key)
	})
	return key
}

// reconcile runs one reconcile with a new reconciler, as after a restart, and
// returns the stored object (nil once it is gone).
func (env aiGatewayKeyEnv) reconcile(ctx context.Context, t *testing.T, key *coderv1alpha1.CoderAIGatewayKey) (*coderv1alpha1.CoderAIGatewayKey, ctrl.Result) {
	t.Helper()
	r := &controller.CoderAIGatewayKeyReconciler{
		Client: env.client, APIReader: env.client, Scheme: scheme, Clock: clocktesting.NewFakePassiveClock(time.Now()),
	}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(key)})
	require.NoError(t, err)
	got := &coderv1alpha1.CoderAIGatewayKey{}
	err = k8sClient.Get(ctx, client.ObjectKeyFromObject(key), got)
	if apierrors.IsNotFound(err) {
		return nil, result
	}
	require.NoError(t, err)
	return got, result
}

// requireAIGatewayKeyStep checks the kstatus conditions of a wait or a stall
// and returns the message.
func requireAIGatewayKeyStep(t *testing.T, key *coderv1alpha1.CoderAIGatewayKey, stalled bool, reason string) string {
	t.Helper()
	want := map[string]metav1.ConditionStatus{"Ready": metav1.ConditionFalse, "Reconciling": metav1.ConditionTrue, "Stalled": metav1.ConditionFalse}
	if stalled {
		want["Reconciling"], want["Stalled"] = metav1.ConditionFalse, metav1.ConditionTrue
	}
	for conditionType, status := range want {
		condition := meta.FindStatusCondition(key.Status.Conditions, conditionType)
		require.NotNil(t, condition, conditionType)
		require.Equal(t, status, condition.Status, conditionType)
		require.Equal(t, reason, condition.Reason, conditionType)
		require.LessOrEqual(t, len(condition.Message), 256)
	}
	require.Equal(t, key.Generation, key.Status.ObservedGeneration)
	return meta.FindStatusCondition(key.Status.Conditions, "Ready").Message
}

func requireNoOutputSecret(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &corev1.Secret{})
	require.True(t, apierrors.IsNotFound(err), "the controller must not create Secret %s: %v", name, err)
}

// A missing or not ready control plane, and a control plane that does not
// allow keys (plan S5), send zero Coder requests and create no Secret.
func TestCoderAIGatewayKeyWaitsWithoutCoderCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		allowKeys    *bool
		controlPlane string
		stalled      bool
		reason       string
	}{
		"missing control plane":   {allowKeys: ptrTo(true), controlPlane: "missing", reason: "ControlPlaneNotReady"},
		"control plane not ready": {allowKeys: ptrTo(true), controlPlane: "starting", reason: "ControlPlaneNotReady"},
		"aiGateway absent":        {controlPlane: "coder", stalled: true, reason: "NotAllowed"},
		"allowKeys false":         {allowKeys: ptrTo(false), controlPlane: "coder", stalled: true, reason: "NotAllowed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, tc.allowKeys)
			createTestControlPlane(ctx, t, env.namespace, "starting", "")
			got, result := env.reconcile(ctx, t, env.createKey(ctx, t, tc.controlPlane, testAIGatewaySecret))
			require.Equal(t, []string{coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer}, got.Finalizers)
			requireAIGatewayKeyStep(t, got, tc.stalled, tc.reason)
			require.Positive(t, result.RequeueAfter)
			require.Zero(t, env.fake.totalRequests())
			requireNoOutputSecret(ctx, t, env.namespace, testAIGatewaySecret)
		})
	}
}

// Plan S6 and the other LIST failure classes: no Coder write and no Secret.
func TestCoderAIGatewayKeyListFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		setup   func(*fakeCoder)
		stalled bool
		reason  string
	}{
		"no license":     {setup: func(f *fakeCoder) { f.setAIGatewayFeature(false) }, stalled: true, reason: "LicenseRequired"},
		"route missing":  {setup: func(f *fakeCoder) { f.setAIGatewayRoutesMissing(true) }, stalled: true, reason: "NotSupported"},
		"not site owner": {setup: func(f *fakeCoder) { f.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusForbidden}) }, stalled: true, reason: "CoderRejected"},
		"rate limited":   {setup: func(f *fakeCoder) { f.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusTooManyRequests}) }, reason: "CoderUnavailable"},
		"server error": {setup: func(f *fakeCoder) {
			f.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusBadGateway, Detail: "detail-must-not-leak"})
		}, reason: "CoderUnavailable"},
		"connection reset": {setup: func(f *fakeCoder) { f.failNext(routeAIGatewayKeys, fakeFault{Reset: true}) }, reason: "CoderUnavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			tc.setup(env.fake)
			got, result := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
			message := requireAIGatewayKeyStep(t, got, tc.stalled, tc.reason)
			require.NotContains(t, message, "detail-must-not-leak")
			require.Positive(t, result.RequeueAfter)
			require.Equal(t, 1, env.fake.requestCount(routeAIGatewayKeys))
			require.Equal(t, 1, env.fake.totalRequests())
			requireNoOutputSecret(ctx, t, env.namespace, testAIGatewaySecret)
		})
	}
}

// Plan S1 at reconciler level: an existing Secret that this object does not
// control gives SecretNotOwned, zero Coder requests, and an unchanged Secret.
func TestCoderAIGatewayKeySecretNotOwned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, mutate := range map[string]func(*corev1.Secret, *coderv1alpha1.CoderAIGatewayKey){
		"no owner": func(*corev1.Secret, *coderv1alpha1.CoderAIGatewayKey) {},
		"another CR's UID": func(s *corev1.Secret, k *coderv1alpha1.CoderAIGatewayKey) {
			s.OwnerReferences = []metav1.OwnerReference{gatewayKeyOwner(k)}
			s.OwnerReferences[0].UID = "6b1f0e2a-0000-4000-8000-000000000000"
		},
		"another owner kind": func(s *corev1.Secret, k *coderv1alpha1.CoderAIGatewayKey) {
			s.OwnerReferences = []metav1.OwnerReference{gatewayKeyOwner(k)}
			s.OwnerReferences[0].Kind = "CoderAIProvider"
		},
		"credential label": func(s *corev1.Secret, k *coderv1alpha1.CoderAIGatewayKey) {
			s.OwnerReferences = []metav1.OwnerReference{gatewayKeyOwner(k)}
			s.Labels = map[string]string{"coder.com/credential": "true"}
		},
		"not Opaque": func(s *corev1.Secret, k *coderv1alpha1.CoderAIGatewayKey) {
			s.OwnerReferences = []metav1.OwnerReference{gatewayKeyOwner(k)}
			s.Type = corev1.SecretTypeBasicAuth
			s.Data = map[string][]byte{corev1.BasicAuthUsernameKey: []byte("u")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testAIGatewaySecret, Namespace: env.namespace},
				Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{"key": []byte("someone-else")},
			}
			mutate(secret, key)
			secret = createSecret(ctx, t, secret)
			got, _ := env.reconcile(ctx, t, key)
			requireAIGatewayKeyStep(t, got, true, "SecretNotOwned")
			require.Zero(t, env.fake.totalRequests())
			after := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), after))
			require.Equal(t, secret.ResourceVersion, after.ResourceVersion, "the Secret must stay unchanged")
		})
	}
}

func gatewayKeyOwner(key *coderv1alpha1.CoderAIGatewayKey) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: coderv1alpha1.GroupVersion.String(), Kind: "CoderAIGatewayKey", Name: key.Name, UID: key.UID, Controller: ptrTo(true),
	}
}

// Plan S2 at reconciler level: a secretName that a control plane uses gives
// SecretNameReserved, zero Coder requests, and no Secret.
func TestCoderAIGatewayKeySecretNameReserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	other := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: env.namespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:            "coder-control-plane:test",
			OperatorAccess:   coderv1alpha1.OperatorAccessSpec{GeneratedTokenSecretName: "custom-token"},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: "license", Key: "license"},
			Database:         &coderv1alpha1.DatabaseSpec{ConnectionSecretRef: coderv1alpha1.SecretKeySelector{Name: "database", Key: "url"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, other))
	other.Status.OperatorTokenSecretRef = &coderv1alpha1.SecretKeySelector{Name: "recorded-token", Key: "token"}
	require.NoError(t, k8sClient.Status().Update(ctx, other))

	for _, name := range []string{"custom-token", "recorded-token", "license", "database", "coder-k8s-apiserver-tls"} {
		t.Run(name, func(t *testing.T) {
			key := env.createKey(ctx, t, "coder", name)
			got, _ := env.reconcile(ctx, t, key)
			message := requireAIGatewayKeyStep(t, got, true, "SecretNameReserved")
			require.Contains(t, message, `"`+name+`"`)
			require.Zero(t, env.fake.totalRequests())
			requireNoOutputSecret(ctx, t, env.namespace, name)
			require.NoError(t, k8sClient.Delete(ctx, got))
			gone, _ := env.reconcile(ctx, t, got)
			require.Nil(t, gone, "an object that never sent a create releases its finalizer")
		})
	}
}

// The first steps of a usable object: one LIST, then the Secret is claimed
// empty, with the owner reference and labels, before any key exists. A
// second reconcile changes nothing. Deletion of an object that never sent a
// create releases the finalizer with zero Coder calls.
func TestCoderAIGatewayKeyClaimsSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	got, _ := env.reconcile(ctx, t, key)
	requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	require.Equal(t, 1, env.fake.requestCount(routeAIGatewayKeys))
	require.Zero(t, env.fake.requestCount(routeCreateAIGatewayKey))

	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
	require.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	require.Empty(t, secret.Data)
	require.Equal(t, map[string]string{
		"app.kubernetes.io/managed-by": "coder-k8s", "app.kubernetes.io/name": "coder-ai-gateway-key", "app.kubernetes.io/instance": "gateway",
	}, secret.Labels)
	require.Len(t, secret.OwnerReferences, 1)
	require.Equal(t, got.UID, secret.OwnerReferences[0].UID)
	require.True(t, *secret.OwnerReferences[0].Controller)

	again, _ := env.reconcile(ctx, t, got)
	require.Equal(t, got.ResourceVersion, again.ResourceVersion, "a reconcile with no change writes no status")
	after := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), after))
	require.Equal(t, secret.ResourceVersion, after.ResourceVersion)
	require.Equal(t, 2, env.fake.totalRequests())

	require.NoError(t, k8sClient.Delete(ctx, again))
	gone, _ := env.reconcile(ctx, t, again)
	require.Nil(t, gone)
	require.Equal(t, 2, env.fake.totalRequests(), "deletion of an object that never sent a create calls Coder zero times")
}

// An object that may have sent a create keeps its finalizer: this version
// has no deletion path (K9), so it neither deletes nor forgets keys.
func TestCoderAIGatewayKeyKeepsFinalizerAfterCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	got.Status.NextSerial = 1
	require.NoError(t, k8sClient.Status().Update(ctx, got))
	require.NoError(t, k8sClient.Delete(ctx, got))
	requests := env.fake.totalRequests()
	kept, _ := env.reconcile(ctx, t, got)
	require.Equal(t, []string{coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer}, kept.Finalizers)
	requireAIGatewayKeyStep(t, kept, false, "DeletePending")
	require.Equal(t, requests, env.fake.totalRequests())
}

// Plan 5.1: a Secret create that the API server rejects as Invalid stalls
// with SecretWriteRejected before any key exists.
func TestCoderAIGatewayKeySecretCreateRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, obj.GetName(),
					field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), obj.GetName(), "label too long")})
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyStep(t, got, true, "SecretWriteRejected")
	require.Zero(t, env.fake.requestCount(routeCreateAIGatewayKey))
	requireNoOutputSecret(ctx, t, env.namespace, testAIGatewaySecret)
}

// Through the manager: the control plane watch and Owns(Secret) requeue the
// object at once.
func TestCoderAIGatewayKeyWatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(false))
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{env.namespace: {}}},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	require.NoError(t, (&controller.CoderAIGatewayKeyReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Clock: clocktesting.NewFakePassiveClock(time.Now()),
	}).SetupWithManager(mgr))
	mgrCtx, cancel := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-mgrDone)
	})

	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	requireReason := func(reason string) {
		t.Helper()
		require.Eventually(t, func() bool {
			got := &coderv1alpha1.CoderAIGatewayKey{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(key), got))
			condition := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			return condition != nil && condition.Reason == reason
		}, 30*time.Second, 100*time.Millisecond, reason)
	}
	requireReason("NotAllowed")

	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, cp))
	cp.Spec.AIGateway.AllowKeys = true
	require.NoError(t, k8sClient.Update(ctx, cp))
	requireReason("IssuingKey")

	// Deleting the claimed Secret requeues the object, which claims it again.
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
	require.NoError(t, k8sClient.Delete(ctx, secret))
	require.Eventually(t, func() bool {
		again := &corev1.Secret{}
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), again)
		return err == nil && again.UID != secret.UID
	}, 30*time.Second, 100*time.Millisecond)
}
