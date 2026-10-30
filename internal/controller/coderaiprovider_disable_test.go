package controller_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

func editSecret(ctx context.Context, t *testing.T, secret *corev1.Secret, edit func(*corev1.Secret)) {
	t.Helper()
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	edit(secret)
	require.NoError(t, k8sClient.Update(ctx, secret))
}

// #267: when the key Secret stops allowing the origin that Coder uses, the
// controller disables the provider in Coder with one PATCH that holds no key
// and no URL. The disable is a kill switch: the restored consent does not
// undo it or a dashboard key change, and a spec edit enables the provider.
func TestCoderAIProviderDisablesWithoutConsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason string
		edit   func(*corev1.Secret)
	}{
		{name: "origin removed", reason: credentialsecret.ReasonDestinationNotAllowed, edit: func(s *corev1.Secret) {
			s.Annotations[credentialsecret.DestinationsAnnotation] = "https://llm.example.com"
		}},
		{name: "annotation removed", reason: credentialsecret.ReasonNotOptedIn, edit: func(s *corev1.Secret) {
			delete(s.Annotations, credentialsecret.DestinationsAnnotation)
		}},
		{name: "label removed", reason: credentialsecret.ReasonNotOptedIn, edit: func(s *corev1.Secret) {
			delete(s.Labels, credentialsecret.LabelKey)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, secret, provider := newReadyAIProvider(ctx, t)
			before, _ := env.fake.aiProvider("openai")
			require.True(t, before.enabled)
			consented := secret.DeepCopy()
			editSecret(ctx, t, secret, tc.edit)

			// A failed disable is retried, and the message says that Coder
			// can still send the key.
			env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusBadGateway})
			got, result := env.reconcile(ctx, t, provider)
			require.Contains(t, requireAIProviderStep(t, got, false, tc.reason), "Coder can still send the key")
			require.Equal(t, 30*time.Second, result.RequeueAfter)

			writes := env.writes()
			got, result = env.reconcile(ctx, t, got)
			require.Contains(t, requireAIProviderStep(t, got, true, tc.reason), "The controller disabled provider openai in Coder")
			requireAIProviderRequeue(t, 10*time.Minute, result.RequeueAfter)
			require.Equal(t, got.Generation, got.Status.AppliedGeneration)
			require.Equal(t, writes+1, env.writes(), "one PATCH")
			stored, _ := env.fake.aiProvider("openai")
			require.False(t, stored.enabled)
			require.Equal(t, before.baseURL, stored.baseURL)
			require.Equal(t, before.keys, stored.keys, "the PATCH held no key")

			got, result = env.reconcile(ctx, t, got)
			require.Contains(t, requireAIProviderStep(t, got, true, tc.reason), "Provider openai is disabled in Coder.")
			requireAIProviderRequeue(t, 10*time.Minute, result.RequeueAfter)
			require.Equal(t, writes+1, env.writes(), "a disabled provider needs no write")

			// A dashboard enable while consent is withdrawn is undone by the
			// next resync.
			dashboard := env.fake.client(t, 2*time.Second)
			_, err := dashboard.UpdateAIProvider(ctx, got.Status.ProviderID, codersdk.UpdateAIProviderRequest{Enabled: ptrTo(true)})
			require.NoError(t, err)
			got, _ = env.reconcile(ctx, t, got)
			require.Contains(t, requireAIProviderStep(t, got, true, tc.reason), "The controller disabled provider openai in Coder")
			stored, _ = env.fake.aiProvider("openai")
			require.False(t, stored.enabled)

			// A dashboard key change and the restored consent stay kill
			// switches.
			_, err = dashboard.UpdateAIProvider(ctx, got.Status.ProviderID, codersdk.UpdateAIProviderRequest{
				APIKeys: &[]codersdk.AIProviderKeyMutation{{APIKey: ptrTo("sk-dashboard-0123456789abcdef")}},
			})
			require.NoError(t, err)
			editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Labels, s.Annotations = consented.Labels, consented.Annotations })
			writes = env.writes()
			got, _ = env.reconcile(ctx, t, got)
			// #321: the controller disabled the provider, so the message
			// names the kill switch, not only a Coder admin.
			require.Contains(t, requireAIProviderStep(t, got, true, "DisabledExternally"), "by a Coder admin or by the controller's credential kill switch")
			require.Equal(t, writes, env.writes())
			stored, _ = env.fake.aiProvider("openai")
			require.Equal(t, "sk-dashboard-0123456789abcdef", stored.keys[0].plaintext)

			got = editSpec(ctx, t, got, func(s *coderv1alpha1.CoderAIProviderSpec) { s.DisplayName = "OpenAI v2" })
			got, _ = env.reconcile(ctx, t, got)
			requireAIProviderReady(t, got)
			stored, _ = env.fake.aiProvider("openai")
			require.True(t, stored.enabled)
			require.Equal(t, testAIProviderKey, stored.keys[0].plaintext)
		})
	}
}

// #267: a spec that names an origin the Secret does not allow stalls with
// zero writes while Coder still uses an allowed origin, and a provider that
// an admin disabled stays a kill switch.
func TestCoderAIProviderConsentWithdrawnWithoutDisable(t *testing.T) {
	t.Parallel()
	t.Run("Coder origin still allowed", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		got := editSpec(ctx, t, provider, func(s *coderv1alpha1.CoderAIProviderSpec) { s.BaseURL = "https://llm.example.com/v1" })
		writes := env.writes()
		got, _ = env.reconcile(ctx, t, got)
		require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonDestinationNotAllowed), "Coder still sends the key of provider openai to an origin that the Secret allows.")
		require.Equal(t, writes, env.writes())
		stored, _ := env.fake.aiProvider("openai")
		require.True(t, stored.enabled)
		require.Equal(t, "https://api.openai.com/v1", stored.baseURL)
	})
	t.Run("another Secret in the spec", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newReadyAIProvider(ctx, t)
		other := env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKeyRotated)
		other.Name = "llm-api-key"
		createSecret(ctx, t, other)
		got := editSpec(ctx, t, provider, func(s *coderv1alpha1.CoderAIProviderSpec) {
			s.BaseURL, s.APIKeySecretRef = "https://llm.example.com/v1", coderv1alpha1.SecretKeySelector{Name: "llm-api-key", Key: "api-key"}
		})
		got, _ = env.reconcile(ctx, t, got)
		require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonDestinationNotAllowed), "The controller disabled provider openai in Coder")
		stored, _ := env.fake.aiProvider("openai")
		require.False(t, stored.enabled, "the new Secret cannot prove consent for the key that Coder holds")
	})
	t.Run("Secret recreated under the same name", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, secret, provider := newReadyAIProvider(ctx, t)
		require.NoError(t, k8sClient.Delete(ctx, secret))
		createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKeyRotated))
		got := editSpec(ctx, t, provider, func(s *coderv1alpha1.CoderAIProviderSpec) { s.BaseURL = "https://llm.example.com/v1" })
		got, _ = env.reconcile(ctx, t, got)
		require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonDestinationNotAllowed), "The controller disabled provider openai in Coder")
		stored, _ := env.fake.aiProvider("openai")
		require.False(t, stored.enabled, "the new Secret did not consent for the key that Coder holds")
	})
	t.Run("disabled in the dashboard", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, secret, provider := newReadyAIProvider(ctx, t)
		_, err := env.fake.client(t, 2*time.Second).UpdateAIProvider(ctx, provider.Status.ProviderID, codersdk.UpdateAIProviderRequest{Enabled: ptrTo(false)})
		require.NoError(t, err)
		consented := secret.DeepCopy()
		editSecret(ctx, t, secret, func(s *corev1.Secret) { delete(s.Labels, credentialsecret.LabelKey) })
		writes := env.writes()
		got, _ := env.reconcile(ctx, t, provider)
		require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonNotOptedIn), "is disabled in Coder")
		require.Equal(t, writes, env.writes())

		editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Labels = consented.Labels })
		got, _ = env.reconcile(ctx, t, got)
		requireAIProviderStep(t, got, true, "DisabledExternally")
		require.Equal(t, writes, env.writes())
	})
}

// A disable answer that shows the provider enabled is retried.
func TestCoderAIProviderDisableAnswerMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newReadyAIProvider(ctx, t)
	editSecret(ctx, t, secret, func(s *corev1.Secret) { delete(s.Labels, credentialsecret.LabelKey) })
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Rewrite: func(answer any) any {
		p := answer.(codersdk.AIProvider)
		p.Enabled = true
		return p
	}})
	got, result := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "CoderAnswerMismatch")
	require.Equal(t, 30*time.Second, result.RequeueAfter)
}

// #290: the controller fails closed on every credential failure of the
// recorded key Secret, also when a second read for Coder's base URL fails the
// same way. The disable PATCH body holds only the enabled flag.
func TestCoderAIProviderDisablesOnEveryCredentialFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason string
		edit   func(ctx context.Context, t *testing.T, env aiProviderEnv, secret *corev1.Secret)
	}{
		{name: "Secret deleted", reason: credentialsecret.ReasonSecretNotFound, edit: func(ctx context.Context, t *testing.T, _ aiProviderEnv, secret *corev1.Secret) {
			require.NoError(t, k8sClient.Delete(ctx, secret))
		}},
		{name: "control plane references the Secret", reason: credentialsecret.ReasonRefused, edit: func(ctx context.Context, t *testing.T, env aiProviderEnv, secret *corev1.Secret) {
			controlPlane := &coderv1alpha1.CoderControlPlane{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, controlPlane))
			controlPlane.Spec.LicenseSecretRef = &coderv1alpha1.SecretKeySelector{Name: secret.Name, Key: "api-key"}
			require.NoError(t, k8sClient.Update(ctx, controlPlane))
		}},
		{name: "malformed annotation", reason: credentialsecret.ReasonMalformed, edit: func(ctx context.Context, t *testing.T, _ aiProviderEnv, secret *corev1.Secret) {
			editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Annotations[credentialsecret.DestinationsAnnotation] = "api.openai.com" })
		}},
		{name: "key removed", reason: credentialsecret.ReasonKeyMissing, edit: func(ctx context.Context, t *testing.T, _ aiProviderEnv, secret *corev1.Secret) {
			editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Data = map[string][]byte{"other": []byte("x")} })
		}},
		{name: "malformed key value", reason: credentialsecret.ReasonMalformed, edit: func(ctx context.Context, t *testing.T, _ aiProviderEnv, secret *corev1.Secret) {
			editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Data["api-key"] = []byte(testAIProviderKey + "\n") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, secret, provider := newReadyAIProvider(ctx, t)
			before, _ := env.fake.aiProvider("openai")
			tc.edit(ctx, t, env, secret)

			var bodies []string
			env.fake.failNext(routeUpdateAIProvider, fakeFault{Inspect: func(body []byte) { bodies = append(bodies, string(body)) }})
			writes := env.writes()
			got, result := env.reconcile(ctx, t, provider)
			require.Contains(t, requireAIProviderStep(t, got, true, tc.reason), "The controller disabled provider openai in Coder")
			requireAIProviderRequeue(t, 10*time.Minute, result.RequeueAfter)
			require.Equal(t, writes+1, env.writes(), "one PATCH")
			require.Len(t, bodies, 1)
			require.JSONEq(t, `{"enabled":false}`, bodies[0], "the PATCH body holds only the enabled flag")
			stored, _ := env.fake.aiProvider("openai")
			require.False(t, stored.enabled)
			require.Equal(t, before.baseURL, stored.baseURL)
			require.Equal(t, before.keys, stored.keys)
		})
	}
}

// The disable needs a Coder provider ID. Without one, a credential failure
// sends nothing to Coder, also while a create can be in flight: the
// controller creates providers disabled.
func TestCoderAIProviderNoDisableWithoutProviderID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newAIProviderCreateEnv(ctx, t)
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now()}
	provider.Status.Credential = &coderv1alpha1.CoderAIProviderCredentialStatus{SecretName: secret.Name, SecretKey: "api-key"}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))
	require.NoError(t, k8sClient.Delete(ctx, secret))
	got, result := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, true, credentialsecret.ReasonSecretNotFound)
	require.Zero(t, result.RequeueAfter)
	require.Zero(t, env.fake.totalRequests())
}

// #277: a key that is not valid UTF-8 would reach Coder as another value, so
// it is a malformed credential: no create, and a provider that the
// controller created is disabled (the second read refuses it too).
func TestCoderAIProviderKeyNotUTF8(t *testing.T) {
	t.Parallel()
	invalid := testAIProviderKey + "\xff"
	t.Run("before the create", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env := newAIProviderEnv(ctx, t, true)
		createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), invalid))
		got, _ := env.reconcile(ctx, t, env.createProvider(ctx, t, nil))
		require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonMalformed), "is not valid UTF-8")
		require.Zero(t, env.fake.totalRequests())
	})
	t.Run("after the create", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, secret, provider := newReadyAIProvider(ctx, t)
		setSecretValue(ctx, t, secret, invalid)
		got, _ := env.reconcile(ctx, t, provider)
		require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonMalformed), "The controller disabled provider openai in Coder")
		stored, _ := env.fake.aiProvider("openai")
		require.False(t, stored.enabled)
		require.Equal(t, testAIProviderKey, stored.keys[0].plaintext)
	})
}

// A key with a NUL, CR, LF or other control byte cannot reach the provider
// in an HTTP header, so it is a malformed credential with no Coder request.
func TestCoderAIProviderKeyNotHeaderValue(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]string{"NUL": testAIProviderKey + "\x00x", "CR": testAIProviderKey + "\rx", "DEL": testAIProviderKey + "\x7fx"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newAIProviderEnv(ctx, t, true)
			createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), key))
			got, _ := env.reconcile(ctx, t, env.createProvider(ctx, t, nil))
			require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonMalformed), "is not a valid HTTP header value")
			require.Zero(t, env.fake.totalRequests())
		})
	}
}

// #154 A7: a transient error of the key Secret read, such as an API server
// timeout, is not a credential failure. The reconcile returns the error, so
// the object retries with backoff. The controller does not disable the
// provider, and a failed first read sends no request to Coder. A failed
// second read in the disable path sends no write.
func TestCoderAIProviderTransientSecretError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		failGet         int32
		withdrawConsent bool
	}{
		{name: "first read", failGet: 1},
		{name: "second read in the disable path", failGet: 2, withdrawConsent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, secret, provider := newReadyAIProvider(ctx, t)
			if tc.withdrawConsent {
				editSecret(ctx, t, secret, func(s *corev1.Secret) { delete(s.Labels, credentialsecret.LabelKey) })
			}
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
			require.NoError(t, err)
			var gets atomic.Int32
			env.r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Secret); ok && key == client.ObjectKeyFromObject(secret) && gets.Add(1) == tc.failGet {
						return apierrors.NewTimeoutError("the server was unable to return a response in the time allotted", 1)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})

			requests, writes := env.fake.totalRequests(), env.writes()
			_, err = env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
			require.True(t, apierrors.IsTimeout(err), "the reconcile returns the error, so the object retries: %v", err)
			require.Equal(t, tc.failGet, gets.Load())
			require.Equal(t, writes, env.writes(), "no disable")
			if !tc.withdrawConsent {
				require.Equal(t, requests, env.fake.totalRequests(), "no Coder request")
			}
			got := &coderv1alpha1.CoderAIProvider{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
			requireAIProviderReady(t, got)
			stored, _ := env.fake.aiProvider("openai")
			require.True(t, stored.enabled)

			// The retry reads the Secret again.
			got, _ = env.reconcile(ctx, t, got)
			if tc.withdrawConsent {
				require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonNotOptedIn), "The controller disabled provider openai in Coder")
				require.Equal(t, writes+1, env.writes())
				return
			}
			requireAIProviderReady(t, got)
			require.Equal(t, writes, env.writes())
		})
	}
}

// #327: a 404 that Coder did not send, on the read before the disable, does
// not prove that nothing uses the key. The object waits and says that Coder
// can still send the key, and the next reconcile disables the provider.
func TestCoderAIProviderDisableNotCoder404(t *testing.T) {
	t.Parallel()
	for name, fault := range notCoder404s {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, secret, provider := newReadyAIProvider(ctx, t)
			editSecret(ctx, t, secret, func(s *corev1.Secret) { delete(s.Labels, credentialsecret.LabelKey) })
			writes := env.writes()
			env.fake.failNext(routeAIProvider, fault)
			got, result := env.reconcile(ctx, t, provider)
			require.Contains(t, requireAIProviderStep(t, got, false, credentialsecret.ReasonNotOptedIn), "Coder can still send the key")
			require.Equal(t, 30*time.Second, result.RequeueAfter)
			require.Equal(t, writes, env.writes())
			stored, _ := env.fake.aiProvider("openai")
			require.True(t, stored.enabled)

			got, _ = env.reconcile(ctx, t, got)
			require.Contains(t, requireAIProviderStep(t, got, true, credentialsecret.ReasonNotOptedIn), "The controller disabled provider openai in Coder")
			stored, _ = env.fake.aiProvider("openai")
			require.False(t, stored.enabled)
		})
	}
}
