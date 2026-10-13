package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/credentialsecret"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

const (
	testAIProviderKey    = "sk-ai-provider-7f3c9a1e5b2d" // #nosec G101 -- test fixture, not a credential.
	testAIProviderOrigin = "https://api.openai.com"
)

// aiProviderEnv is one namespace with a fake Coder and, optionally, a ready
// control plane named coder that points at it.
type aiProviderEnv struct {
	namespace string
	fake      *fakeCoder
	r         *controller.CoderAIProviderReconciler
}

func newAIProviderEnv(ctx context.Context, t *testing.T, withControlPlane bool) aiProviderEnv {
	t.Helper()
	env := aiProviderEnv{namespace: createTestNamespace(ctx, t, "aip"), fake: newFakeCoder(t)}
	env.r = &controller.CoderAIProviderReconciler{Client: k8sClient, Scheme: scheme}
	if withControlPlane {
		createTestControlPlane(ctx, t, env.namespace, "coder", env.fake.server.URL)
	}
	return env
}

func (env aiProviderEnv) keySecret(meta metav1.ObjectMeta, value string) *corev1.Secret {
	meta.Name, meta.Namespace = "openai-api-key", env.namespace
	return tokenSecret(meta, "api-key", value)
}

func (env aiProviderEnv) createProvider(ctx context.Context, t *testing.T, mutate func(*coderv1alpha1.CoderAIProviderSpec)) *coderv1alpha1.CoderAIProvider {
	t.Helper()
	spec := validAIProviderSpec()
	if mutate != nil {
		mutate(&spec)
	}
	provider := &coderv1alpha1.CoderAIProvider{ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: env.namespace}, Spec: spec}
	require.NoError(t, k8sClient.Create(ctx, provider))
	t.Cleanup(func() {
		_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(provider), provider)
		provider.Finalizers = nil
		_ = k8sClient.Update(context.Background(), provider)
		_ = k8sClient.Delete(context.Background(), provider)
	})
	return provider
}

// reconcile runs one reconcile with captured logs, checks that neither the
// status, the logs, nor the error holds the key, and returns the stored object.
func (env aiProviderEnv) reconcile(ctx context.Context, t *testing.T, provider *coderv1alpha1.CoderAIProvider) (*coderv1alpha1.CoderAIProvider, ctrl.Result) {
	t.Helper()
	logCtx, logs := capturingContext(ctx)
	result, err := env.r.Reconcile(logCtx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.NoError(t, err)
	got := &coderv1alpha1.CoderAIProvider{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
	credentialsecrettest.RequireNoLeak(t, []string{testAIProviderKey}, map[string]any{"status": got.Status, "annotations": got.Annotations, "logs": logs.String()})
	return got, result
}

// requireAIProviderStep checks the kstatus conditions of a wait or a stall.
func requireAIProviderStep(t *testing.T, provider *coderv1alpha1.CoderAIProvider, stalled bool, reason string) string {
	t.Helper()
	want := map[string]metav1.ConditionStatus{"Ready": metav1.ConditionFalse, "Reconciling": metav1.ConditionTrue, "Stalled": metav1.ConditionFalse}
	if stalled {
		want["Reconciling"], want["Stalled"] = metav1.ConditionFalse, metav1.ConditionTrue
	}
	return requireAIProviderConditions(t, provider, want, reason)
}

// requireAIProviderReady checks that the provider reports Ready for its
// current generation.
func requireAIProviderReady(t *testing.T, provider *coderv1alpha1.CoderAIProvider) {
	t.Helper()
	want := map[string]metav1.ConditionStatus{"Ready": metav1.ConditionTrue, "Reconciling": metav1.ConditionFalse, "Stalled": metav1.ConditionFalse}
	requireAIProviderConditions(t, provider, want, "Applied")
	require.Equal(t, provider.Generation, provider.Status.AppliedGeneration)
	require.Nil(t, provider.Status.CreateAttemptTime)
}

func requireAIProviderConditions(t *testing.T, provider *coderv1alpha1.CoderAIProvider, want map[string]metav1.ConditionStatus, reason string) string {
	t.Helper()
	for conditionType, status := range want {
		condition := meta.FindStatusCondition(provider.Status.Conditions, conditionType)
		require.NotNil(t, condition, conditionType)
		require.Equal(t, status, condition.Status, conditionType)
		require.Equal(t, reason, condition.Reason, conditionType)
		require.Equal(t, provider.Generation, condition.ObservedGeneration)
	}
	require.Equal(t, provider.Generation, provider.Status.ObservedGeneration)
	return meta.FindStatusCondition(provider.Status.Conditions, "Ready").Message
}

// Plan tests S1, S2, S10 and S12 at reconciler level: every credential
// failure stalls with zero Coder requests, and a usable key reaches Coder.
func TestCoderAIProviderCredentialChecks(t *testing.T) {
	t.Parallel()
	consented := consentedObjectMeta("", "", testAIProviderOrigin)
	for _, tc := range []struct {
		name    string
		baseURL string // Default: https://api.openai.com/v1.
		secret  func(env aiProviderEnv) *corev1.Secret
		setup   func(ctx context.Context, t *testing.T, env aiProviderEnv)
		reason  string // Empty: the key is usable and the name lookup runs.
	}{
		{name: "consented", secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey) }},
		{name: "uppercase host", baseURL: "https://API.OpenAI.com/v1", secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey) }},
		{name: "explicit default port", baseURL: "https://api.openai.com:443/v1", secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey) }},
		{name: "IPv6 host", baseURL: "https://[FD00::1]:8443/v1", secret: func(env aiProviderEnv) *corev1.Secret {
			return env.keySecret(consentedObjectMeta("", "", "https://[fd00::1]:8443"), testAIProviderKey)
		}},
		{name: "IPv6 host, other port", baseURL: "https://[fd00::1]/v1", reason: credentialsecret.ReasonDestinationNotAllowed, secret: func(env aiProviderEnv) *corev1.Secret {
			return env.keySecret(consentedObjectMeta("", "", "https://[fd00::1]:8443"), testAIProviderKey)
		}},
		{name: "missing Secret", reason: credentialsecret.ReasonSecretNotFound},
		{name: "missing key", reason: credentialsecret.ReasonKeyMissing, secret: func(env aiProviderEnv) *corev1.Secret {
			s := env.keySecret(consented, testAIProviderKey)
			s.Data = map[string][]byte{"other": []byte(testAIProviderKey)}
			return s
		}},
		{name: "no label", reason: credentialsecret.ReasonNotOptedIn, secret: func(env aiProviderEnv) *corev1.Secret {
			m := consentedObjectMeta("", "", testAIProviderOrigin)
			m.Labels = nil
			return env.keySecret(m, testAIProviderKey)
		}},
		{name: "no destinations", reason: credentialsecret.ReasonNotOptedIn, secret: func(env aiProviderEnv) *corev1.Secret {
			m := consentedObjectMeta("", "", testAIProviderOrigin)
			m.Annotations = nil
			return env.keySecret(m, testAIProviderKey)
		}},
		{name: "other port", baseURL: "https://api.openai.com:8443/v1", reason: credentialsecret.ReasonDestinationNotAllowed, secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey) }},
		{name: "other scheme", baseURL: "http://api.openai.com/v1", reason: credentialsecret.ReasonDestinationNotAllowed, secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey) }},
		{name: "subdomain", baseURL: "https://eu.api.openai.com/v1", reason: credentialsecret.ReasonDestinationNotAllowed, secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey) }},
		{name: "trailing dot", baseURL: "https://api.openai.com./v1", reason: credentialsecret.ReasonDestinationNotAllowed, secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey) }},
		{name: "not Opaque", reason: credentialsecret.ReasonRefused, secret: func(env aiProviderEnv) *corev1.Secret {
			s := env.keySecret(consented, testAIProviderKey)
			s.Type = "example.com/api-key"
			return s
		}},
		{name: "owned by a coder.com object", reason: credentialsecret.ReasonRefused, secret: func(env aiProviderEnv) *corev1.Secret {
			m := consentedObjectMeta("", "", testAIProviderOrigin)
			m.OwnerReferences = []metav1.OwnerReference{{APIVersion: "coder.com/v1alpha1", Kind: "CoderControlPlane", Name: "coder", UID: "0b6f6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"}}
			return env.keySecret(m, testAIProviderKey)
		}},
		{name: "empty value", reason: credentialsecret.ReasonMalformed, secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, "") }},
		{name: "whitespace", reason: credentialsecret.ReasonMalformed, secret: func(env aiProviderEnv) *corev1.Secret { return env.keySecret(consented, testAIProviderKey+"\n") }},
		{name: "oversized", reason: credentialsecret.ReasonMalformed, secret: func(env aiProviderEnv) *corev1.Secret {
			return env.keySecret(consented, testAIProviderKey+strings.Repeat("k", credentialsecret.MaxValueBytes))
		}},
		{name: "operator token Secret, labeled", reason: credentialsecret.ReasonRefused, setup: func(ctx context.Context, t *testing.T, env aiProviderEnv) {
			operatorToken := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder-operator-token"}, operatorToken))
			operatorToken.Labels = map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue}
			operatorToken.Annotations = map[string]string{credentialsecret.DestinationsAnnotation: testAIProviderOrigin}
			require.NoError(t, k8sClient.Update(ctx, operatorToken))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newAIProviderEnv(ctx, t, true)
			if tc.secret != nil {
				createSecret(ctx, t, tc.secret(env))
			}
			if tc.setup != nil {
				tc.setup(ctx, t, env)
			}
			provider := env.createProvider(ctx, t, func(s *coderv1alpha1.CoderAIProviderSpec) {
				if tc.baseURL != "" {
					s.BaseURL = tc.baseURL
				}
				if tc.setup != nil {
					s.APIKeySecretRef = coderv1alpha1.SecretKeySelector{Name: "coder-operator-token", Key: coderv1alpha1.DefaultTokenSecretKey}
				}
			})
			got, result := env.reconcile(ctx, t, provider)
			if tc.reason == "" {
				requireAIProviderReady(t, got)
				require.Equal(t, 1, env.fake.requestCount(routeAIProvider), "a usable key reaches the name lookup")
				require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))
				require.Equal(t, 10*time.Minute, result.RequeueAfter)
				return
			}
			requireAIProviderStep(t, got, true, tc.reason)
			require.Zero(t, env.fake.totalRequests(), "a credential failure sends nothing to Coder")
			require.Zero(t, result.RequeueAfter, "the Secret watch requeues the provider")
		})
	}
}

func TestCoderAIProviderControlPlaneNotReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIProviderEnv(ctx, t, false)
	createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKey))
	provider := env.createProvider(ctx, t, nil)
	got, result := env.reconcile(ctx, t, provider)
	require.Equal(t, "CoderControlPlane coder does not exist.", requireAIProviderStep(t, got, false, "ControlPlaneNotReady"))
	require.Equal(t, 30*time.Second, result.RequeueAfter)

	// A ready control plane whose operator token Secret is missing: no watch
	// sees the Secret appear, so the wait retries on a timer.
	createTestControlPlane(ctx, t, env.namespace, "coder", env.fake.server.URL)
	operatorToken := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder-operator-token"}, operatorToken))
	require.NoError(t, k8sClient.Delete(ctx, operatorToken))
	got, result = env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "OperatorAccessNotReady")
	require.Equal(t, 30*time.Second, result.RequeueAfter)
	require.Zero(t, env.fake.totalRequests())

	operatorToken.ObjectMeta = metav1.ObjectMeta{Name: operatorToken.Name, Namespace: env.namespace}
	require.NoError(t, k8sClient.Create(ctx, operatorToken))
	got, _ = env.reconcile(ctx, t, provider)
	requireAIProviderReady(t, got)
}

// The name lookup only reads. A provider that this object did not create is a
// conflict (plan S4, read-only part), and Coder errors never put Detail or the
// key into status or logs (the redaction half of S3).
func TestCoderAIProviderNameLookup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(ctx context.Context, t *testing.T, f *fakeCoder)
		stalled bool
		reason  string
		message string
		requeue time.Duration
	}{
		{
			name: "name taken", stalled: true, reason: "NameConflict", requeue: 10 * time.Minute, message: "Coder already has a provider named openai (ID ",
			prepare: func(ctx context.Context, t *testing.T, f *fakeCoder) {
				_, err := f.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
					Type: codersdk.AIProviderTypeOpenAI, Name: "openai", DisplayName: "Team key", BaseURL: "https://api.openai.com/v1", Enabled: true,
				})
				require.NoError(t, err)
			},
		},
		{
			name: "Coder error with the key in its detail", reason: "CoderUnavailable", requeue: 30 * time.Second, message: "get provider openai: Coder answered 500: fake fault 500",
			prepare: func(_ context.Context, _ *testing.T, f *fakeCoder) {
				for range 2 { // One fault per reconcile.
					f.failNext(routeAIProvider, fakeFault{Status: 500, Detail: "upstream rejected " + testAIProviderKey})
				}
			},
		},
		{
			name: "answer for another provider", stalled: true, reason: "CoderAnswerMismatch", requeue: 10 * time.Minute, message: `assertion failed: Coder answered AI provider "other" when asked for "openai"`,
			prepare: func(ctx context.Context, t *testing.T, f *fakeCoder) {
				_, err := f.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{Type: codersdk.AIProviderTypeOpenAI, Name: "openai", BaseURL: "https://api.openai.com/v1"})
				require.NoError(t, err)
				for range 2 { // One fault per reconcile.
					f.failNext(routeAIProvider, fakeFault{Rewrite: func(answer any) any {
						p := answer.(codersdk.AIProvider)
						p.Name = "other"
						return p
					}})
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newAIProviderEnv(ctx, t, true)
			createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKey))
			tc.prepare(ctx, t, env.fake)
			before := env.fake.totalRequests()
			got, result := env.reconcile(ctx, t, env.createProvider(ctx, t, nil))
			require.Contains(t, requireAIProviderStep(t, got, tc.stalled, tc.reason), tc.message)
			require.Equal(t, tc.requeue, result.RequeueAfter)
			require.Equal(t, 1, env.fake.totalRequests()-before, "one GET and no write")

			// A reconcile without a change sends no Coder write and leaves the
			// stored status unchanged (no condition time flaps).
			again, _ := env.reconcile(ctx, t, got)
			require.Equal(t, got.ResourceVersion, again.ResourceVersion)
		})
	}
}

func TestCoderAIProviderFinalizer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIProviderEnv(ctx, t, false)
	provider := env.createProvider(ctx, t, func(s *coderv1alpha1.CoderAIProviderSpec) { s.Enabled = ptrTo(false) })
	got, _ := env.reconcile(ctx, t, provider)
	require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, got.Finalizers)
	require.Equal(t, ptrTo(false), got.Spec.Enabled, "the finalizer patch keeps enabled: false")

	// With nothing in Coder, deletion releases the finalizer.
	require.NoError(t, k8sClient.Delete(ctx, got))
	_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got)))

	// A provider with a Coder ID keeps its finalizer: this version deletes nothing.
	kept := env.createProvider(ctx, t, nil)
	kept, _ = env.reconcile(ctx, t, kept)
	kept.Status.ProviderID = "5d0c6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"
	require.NoError(t, k8sClient.Status().Update(ctx, kept))
	require.NoError(t, k8sClient.Delete(ctx, kept))
	kept, _ = env.reconcile(ctx, t, kept)
	require.Equal(t, []string{coderv1alpha1.CoderAIProviderCleanupFinalizer}, kept.Finalizers)
	requireAIProviderStep(t, kept, false, "DeletePending")
	require.Zero(t, env.fake.totalRequests())
}

// Plan S2 through the manager: a Secret update that removes the label, a
// Secret delete, and control plane changes each requeue the provider at once.
func TestCoderAIProviderWatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIProviderEnv(ctx, t, false)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{env.namespace: {}}},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	require.NoError(t, (&controller.CoderAIProviderReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr))
	mgrCtx, cancel := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-mgrDone)
	})

	secret := createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKey))
	provider := env.createProvider(ctx, t, nil)
	requireReason := func(reason string) {
		t.Helper()
		require.Eventually(t, func() bool {
			got := &coderv1alpha1.CoderAIProvider{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
			condition := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			return condition != nil && condition.Reason == reason
		}, 30*time.Second, 100*time.Millisecond, reason)
	}
	requireReason("ControlPlaneNotReady")

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	secret.Labels = nil
	require.NoError(t, k8sClient.Update(ctx, secret))
	requireReason(credentialsecret.ReasonNotOptedIn)

	secret.Labels = map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue}
	require.NoError(t, k8sClient.Update(ctx, secret))
	requireReason("ControlPlaneNotReady")

	createTestControlPlane(ctx, t, env.namespace, "coder", env.fake.server.URL)
	requireReason("Applied")

	// Another control plane that names the key Secret as its operator token
	// Secret makes the key unusable. Removing it makes the key usable again.
	other := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: env.namespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "coder-control-plane:test", OperatorAccess: coderv1alpha1.OperatorAccessSpec{GeneratedTokenSecretName: secret.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, other))
	requireReason(credentialsecret.ReasonRefused)
	// Renaming its operator token Secret frees the key Secret. The update
	// event maps the old object too, so the provider is checked again.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(other), other))
	other.Spec.OperatorAccess.GeneratedTokenSecretName = "other-operator-token"
	require.NoError(t, k8sClient.Update(ctx, other))
	requireReason("Applied")
	other.Spec.OperatorAccess.GeneratedTokenSecretName = secret.Name
	require.NoError(t, k8sClient.Update(ctx, other))
	requireReason(credentialsecret.ReasonRefused)
	// The same holds when the control plane spec references the key Secret in
	// any other field, and stops referencing it (#249, #260).
	for _, role := range controlPlaneSecretRoles {
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(other), other))
		other.Spec = coderv1alpha1.CoderControlPlaneSpec{Image: "coder-control-plane:test"}
		require.NoError(t, k8sClient.Update(ctx, other))
		requireReason("Applied")

		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(other), other))
		setControlPlaneSecret(&other.Spec, role, secret.Name)
		require.NoError(t, k8sClient.Update(ctx, other))
		requireReason(credentialsecret.ReasonRefused)
	}
	require.NoError(t, k8sClient.Delete(ctx, other))
	requireReason("Applied")

	require.NoError(t, k8sClient.Delete(ctx, secret))
	requireReason(credentialsecret.ReasonSecretNotFound)
}
