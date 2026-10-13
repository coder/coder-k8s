package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// and no URL. The restored consent enables it again.
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
			require.Zero(t, got.Status.AppliedGeneration, "the re-arm is written before the PATCH")

			writes := env.writes()
			got, result = env.reconcile(ctx, t, got)
			require.Contains(t, requireAIProviderStep(t, got, true, tc.reason), "The controller disabled provider openai in Coder")
			require.Zero(t, result.RequeueAfter, "the Secret watch requeues the provider")
			require.Equal(t, writes+1, env.writes(), "one PATCH")
			stored, _ := env.fake.aiProvider("openai")
			require.False(t, stored.enabled)
			require.Equal(t, before.baseURL, stored.baseURL)
			require.Equal(t, before.keys, stored.keys, "the PATCH held no key")

			got, _ = env.reconcile(ctx, t, got)
			require.Contains(t, requireAIProviderStep(t, got, true, tc.reason), "Provider openai is disabled in Coder.")
			require.Equal(t, writes+1, env.writes(), "a disabled provider needs no write")

			editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Labels, s.Annotations = consented.Labels, consented.Annotations })
			got, _ = env.reconcile(ctx, t, got)
			requireAIProviderReady(t, got)
			stored, _ = env.fake.aiProvider("openai")
			require.True(t, stored.enabled)
			require.Equal(t, before.keys, stored.keys, "the key was not sent again")
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
