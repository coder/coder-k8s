package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// Plan F9: a name that the control plane's spec.extraEnv seeds in coderd
// stalls with zero Coder requests.
func TestCoderAIProviderNameReservedByEnv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIProviderEnv(ctx, t, true)
	createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKey))
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	key := client.ObjectKey{Namespace: env.namespace, Name: "coder"}
	require.NoError(t, k8sClient.Get(ctx, key, controlPlane))
	controlPlane.Spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_OPENAI_KEY", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "legacy-openai"}, Key: "key"},
	}}}
	require.NoError(t, k8sClient.Update(ctx, controlPlane))

	got, result := env.reconcile(ctx, t, env.createProvider(ctx, t, nil))
	require.Contains(t, requireAIProviderStep(t, got, true, "NameReservedByEnv"), "sets CODER_AI_GATEWAY_OPENAI_KEY")
	require.Zero(t, result.RequeueAfter, "the control plane watch requeues the provider")
	require.Zero(t, env.fake.totalRequests())

	// An indexed provider variable can seed any name, so it blocks every
	// create. A seeding flag in spec.extraArgs reserves its name (#287), and
	// the message names the flag but not its value.
	for _, edit := range []func(*coderv1alpha1.CoderControlPlaneSpec){
		func(s *coderv1alpha1.CoderControlPlaneSpec) {
			s.ExtraEnv = []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_PROVIDER_0_TYPE", Value: "openai"}, {Name: "CODER_AI_GATEWAY_PROVIDER_0_NAME", Value: "team-openai"}}
		},
		func(s *coderv1alpha1.CoderControlPlaneSpec) {
			s.ExtraEnv, s.ExtraArgs = nil, []string{"--ai-gateway-openai-key=sk-legacy-flag-value"}
		},
	} {
		require.NoError(t, k8sClient.Get(ctx, key, controlPlane))
		edit(&controlPlane.Spec)
		require.NoError(t, k8sClient.Update(ctx, controlPlane))
		got, _ = env.reconcile(ctx, t, got)
		require.NotContains(t, requireAIProviderStep(t, got, true, "NameReservedByEnv"), "sk-legacy-flag-value")
		require.Zero(t, env.fake.totalRequests())
	}

	require.NoError(t, k8sClient.Get(ctx, key, controlPlane))
	controlPlane.Spec.ExtraEnv, controlPlane.Spec.ExtraArgs = []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_OPENAI_BASE_URL", Value: "https://llm.example.com"}}, nil
	require.NoError(t, k8sClient.Update(ctx, controlPlane))
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
}

// #289: a create that can be in flight is confirmed before the env-name
// check. A committed create is recorded. A confirmed absence clears the
// marker, and the check then refuses a new create request.
func TestCoderAIProviderPendingCreateBeforeEnvCheck(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "not committed"}[committed], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, secret, provider := newAIProviderCreateEnv(ctx, t)
			if committed {
				_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
					Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: "coder-k8s:" + string(provider.UID),
					BaseURL: provider.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
				})
				require.NoError(t, err)
			}
			provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now()}
			provider.Status.Credential = &coderv1alpha1.CoderAIProviderCredentialStatus{SecretName: secret.Name, SecretKey: "api-key"}
			require.NoError(t, k8sClient.Status().Update(ctx, provider))
			controlPlane := &coderv1alpha1.CoderControlPlane{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, controlPlane))
			controlPlane.Spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_AI_GATEWAY_OPENAI_KEY", Value: "sk-seeded"}}
			require.NoError(t, k8sClient.Update(ctx, controlPlane))
			posts := env.fake.requestCount(routeCreateAIProvider)

			got, _ := env.reconcile(ctx, t, provider)
			require.Equal(t, posts, env.fake.requestCount(routeCreateAIProvider), "no create is sent")
			if committed {
				stored, _ := env.fake.aiProvider("openai")
				require.Equal(t, stored.id.String(), got.Status.ProviderID, "the committed create is recorded")
				return
			}
			requireAIProviderStep(t, got, true, "NameReservedByEnv")
			require.Nil(t, got.Status.CreateAttemptTime)
			require.Empty(t, got.Status.ProviderID)
		})
	}
}
