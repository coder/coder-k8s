package controller_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

const testAIGatewaySecret = "coder-ai-gateway-key"

// aiGatewayKeyEnv is one namespace with a fake Coder and a ready control
// plane named coder that points at it.
type aiGatewayKeyEnv struct {
	namespace string
	fake      *fakeCoder
	client    client.Client // The reconciler's client. Tests can wrap it.
	clock     *clocktesting.FakeClock
}

func newAIGatewayKeyEnv(ctx context.Context, t *testing.T, allowKeys *bool) aiGatewayKeyEnv {
	t.Helper()
	// Status times have second precision, so the fake clock starts on a second.
	env := aiGatewayKeyEnv{
		namespace: createTestNamespace(ctx, t, "aigk"), fake: newFakeCoder(t), client: k8sClient,
		clock: clocktesting.NewFakeClock(time.Now().Truncate(time.Second)),
	}
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
	return env.createNamedKey(ctx, t, "gateway", controlPlane, secretName)
}

func (env aiGatewayKeyEnv) createNamedKey(ctx context.Context, t *testing.T, name, controlPlane, secretName string) *coderv1alpha1.CoderAIGatewayKey {
	t.Helper()
	key := &coderv1alpha1.CoderAIGatewayKey{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env.namespace},
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
	got, result, err := env.reconcileErr(ctx, t, key)
	require.NoError(t, err)
	return got, result
}

// reconcileErr runs one reconcile and returns its error. It checks that no
// key that the fake Coder holds appears in the status, the output Secret's
// metadata, the logs or the error (plan S3).
func (env aiGatewayKeyEnv) reconcileErr(ctx context.Context, t *testing.T, key *coderv1alpha1.CoderAIGatewayKey) (*coderv1alpha1.CoderAIGatewayKey, ctrl.Result, error) {
	t.Helper()
	r := &controller.CoderAIGatewayKeyReconciler{Client: env.client, APIReader: env.client, Scheme: scheme, Clock: env.clock}
	logCtx, logs := capturingContext(ctx)
	result, reconcileErr := r.Reconcile(logCtx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(key)})
	got := &coderv1alpha1.CoderAIGatewayKey{}
	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(key), got)
	if apierrors.IsNotFound(err) {
		got = nil
	} else {
		require.NoError(t, err)
	}
	haystacks := map[string]any{"logs": logs.String(), "error": fmt.Sprint(reconcileErr)}
	if got != nil {
		haystacks["status"] = got.Status
		secret := &corev1.Secret{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: got.Namespace, Name: got.Spec.SecretName}, secret); err == nil {
			haystacks["secret metadata"] = secret.ObjectMeta
		}
	}
	var plaintexts []string
	for _, k := range env.fake.aiGatewayKeys() {
		plaintexts = append(plaintexts, k.plaintext)
	}
	if len(plaintexts) > 0 {
		credentialsecrettest.RequireNoLeak(t, plaintexts, haystacks)
	}
	return got, result, reconcileErr
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
		"immutable": func(s *corev1.Secret, k *coderv1alpha1.CoderAIGatewayKey) {
			s.OwnerReferences = []metav1.OwnerReference{gatewayKeyOwner(k)}
			s.Immutable = ptrTo(true)
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

// The issue step of a usable object (plan 4.3): one LIST, a dry run, the
// marker, one POST, the key written into the claimed Secret, and the key
// recorded in status. A second reconcile sends one LIST and no write (plan
// S9).
func TestCoderAIGatewayKeyIssuesFirstKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	got, _ := env.reconcile(ctx, t, key)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, 1, env.fake.requestCount(routeAIGatewayKeys))
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey))

	keys := env.fake.aiGatewayKeys()
	require.Len(t, keys, 1)
	name := "ck8s-" + strings.ReplaceAll(string(got.UID), "-", "") + "-0"
	require.Equal(t, name, keys[0].name)
	require.Equal(t, int64(1), got.Status.NextSerial)
	require.Equal(t, got.Generation, got.Status.AppliedGeneration)
	require.Nil(t, got.Status.PendingKey)
	require.Equal(t, &coderv1alpha1.CoderAIGatewayKeyCurrent{
		Serial: 0, ID: keys[0].id.String(), Name: name, IssuedAt: metav1.NewTime(env.clock.Now()),
	}, got.Status.CurrentKey)
	require.Equal(t, env.clock.Now(), got.Status.LastCreateAttemptTime.Time)
	require.Equal(t, env.clock.Now().Add(30*24*time.Hour), got.Status.NextRotationTime.Time)

	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
	require.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	require.Equal(t, map[string][]byte{"key": []byte(keys[0].plaintext)}, secret.Data)
	require.Equal(t, map[string]string{
		"app.kubernetes.io/managed-by": "coder-k8s", "app.kubernetes.io/name": "coder-ai-gateway-key", "app.kubernetes.io/instance": "gateway",
	}, secret.Labels)
	require.Equal(t, map[string]string{"coder.com/ai-gateway-key-id": keys[0].id.String(), "coder.com/ai-gateway-key-name": name}, secret.Annotations)
	require.Len(t, secret.OwnerReferences, 1)
	require.Equal(t, got.UID, secret.OwnerReferences[0].UID)
	require.True(t, *secret.OwnerReferences[0].Controller)

	// A reconcile with no change sends no write and no dry run.
	writes := 0
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	counting := env
	counting.client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			return c.Patch(ctx, obj, patch, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return c.Update(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	})
	again, _ := counting.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, again)
	require.Zero(t, writes)
	require.Equal(t, got.ResourceVersion, again.ResourceVersion)
	require.Equal(t, 2, env.fake.requestCount(routeAIGatewayKeys))
	require.Equal(t, 3, env.fake.totalRequests())
}

func requireAIGatewayKeyReady(t *testing.T, key *coderv1alpha1.CoderAIGatewayKey) {
	t.Helper()
	for conditionType, status := range map[string]metav1.ConditionStatus{"Ready": metav1.ConditionTrue, "Reconciling": metav1.ConditionFalse, "Stalled": metav1.ConditionFalse} {
		condition := meta.FindStatusCondition(key.Status.Conditions, conditionType)
		require.NotNil(t, condition, conditionType)
		require.Equal(t, status, condition.Status, conditionType)
		require.Equal(t, "Ready", condition.Reason, conditionType)
	}
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

// The object that claimed a shared Secret keeps it. An object created later
// in the same second with a lower name ranks older, but it must not stall
// the claimant: it reports SecretNotOwned, and the claimant goes on. The
// client reports one creation time for every listed key, so the tie does
// not depend on timing.
func TestCoderAIGatewayKeyClaimantKeepsSharedSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	first := env.createNamedKey(ctx, t, "z-gateway", "coder", testAIGatewaySecret)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if keys, ok := list.(*coderv1alpha1.CoderAIGatewayKeyList); ok {
				for i := range keys.Items {
					keys.Items[i].CreationTimestamp = first.CreationTimestamp
				}
			}
			return nil
		},
	})
	claimant, _ := env.reconcile(ctx, t, first)
	requireAIGatewayKeyReady(t, claimant)
	later, _ := env.reconcile(ctx, t, env.createNamedKey(ctx, t, "a-gateway", "coder", testAIGatewaySecret))
	requireAIGatewayKeyStep(t, later, true, "SecretNotOwned")
	claimant, _ = env.reconcile(ctx, t, claimant)
	requireAIGatewayKeyReady(t, claimant)
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

func isDryRunUpdate(opts []client.UpdateOption) bool {
	update := &client.UpdateOptions{}
	update.ApplyOptions(opts)
	return len(update.DryRun) == 1 && update.DryRun[0] == metav1.DryRunAll
}

// Plan 4.3 step 2 and S11: a dry-run key write that admission refuses stalls
// with SecretWriteRejected, zero POSTs, and an unchanged empty Secret. The
// status holds the reason and code, never the API server's message.
func TestCoderAIGatewayKeyDryRunRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gr := schema.GroupResource{Resource: "secrets"}
	for name, rejection := range map[string]error{
		"invalid":   apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, testAIGatewaySecret, field.ErrorList{field.Forbidden(field.NewPath("data"), "server-text-must-not-leak")}),
		"forbidden": apierrors.NewForbidden(gr, testAIGatewaySecret, errors.New("server-text-must-not-leak")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
			require.NoError(t, err)
			env.client = interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*corev1.Secret); ok && isDryRunUpdate(opts) {
						return rejection
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
			message := requireAIGatewayKeyStep(t, got, true, "SecretWriteRejected")
			require.NotContains(t, message, "server-text-must-not-leak")
			require.Contains(t, message, string(apierrors.ReasonForError(rejection)))
			require.Zero(t, env.fake.requestCount(routeCreateAIGatewayKey))
			secret := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
			require.Empty(t, secret.Data)
		})
	}
}

// The API server refuses a data change of an immutable Secret, also in a
// dry run. That is why checkOwnedSecret refuses an immutable output Secret
// before any key exists. A dry run of a writable Secret stores nothing.
func TestSecretDryRunWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "dryrun")
	for name, immutable := range map[string]bool{"immutable": true, "writable": false} {
		secret := createSecret(ctx, t, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Type: corev1.SecretTypeOpaque, Immutable: ptrTo(immutable),
		})
		update := secret.DeepCopy()
		update.Data = map[string][]byte{"key": []byte("placeholder")}
		err := k8sClient.Update(ctx, update, client.DryRunAll)
		if immutable {
			require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
		} else {
			require.NoError(t, err)
		}
		stored := &corev1.Secret{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), stored))
		require.Equal(t, secret.ResourceVersion, stored.ResourceVersion)
		require.Empty(t, stored.Data)
	}
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
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Clock: env.clock,
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
	requireReason("Ready")

	// Deleting the output Secret requeues the object at once. The drift
	// re-issue arrives in K7, so it reports that it waits.
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
	require.NoError(t, k8sClient.Delete(ctx, secret))
	requireReason("IssuingKey")
}
