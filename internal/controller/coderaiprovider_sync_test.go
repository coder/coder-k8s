package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

const testAIProviderKeyRotated = "sk-ai-provider-rotated-4c8e2b6f" // #nosec G101 -- test fixture, not a credential.

// newReadyAIProvider returns a provider that reached Ready.
func newReadyAIProvider(ctx context.Context, t *testing.T) (aiProviderEnv, *corev1.Secret, *coderv1alpha1.CoderAIProvider) {
	t.Helper()
	env, secret, provider := newAIProviderCreateEnv(ctx, t)
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderReady(t, got)
	return env, secret, got
}

func (env aiProviderEnv) writes() int {
	return env.fake.requestCount(routeCreateAIProvider) + env.fake.requestCount(routeUpdateAIProvider) + env.fake.requestCount(routeDeleteAIProvider)
}

func setSecretValue(ctx context.Context, t *testing.T, secret *corev1.Secret, value string) {
	t.Helper()
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	secret.Data["api-key"] = []byte(value)
	require.NoError(t, k8sClient.Update(ctx, secret))
}

func editSpec(ctx context.Context, t *testing.T, provider *coderv1alpha1.CoderAIProvider, edit func(*coderv1alpha1.CoderAIProviderSpec)) *coderv1alpha1.CoderAIProvider {
	t.Helper()
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), provider))
	edit(&provider.Spec)
	require.NoError(t, k8sClient.Update(ctx, provider))
	return provider
}

// Plan S13: a reconcile without a spec, Secret or Coder difference sends no
// write, also for an empty display name, which Coder shows as the name.
func TestCoderAIProviderSyncNoDifference(t *testing.T) {
	t.Parallel()
	for _, displayName := range []string{"OpenAI", ""} {
		t.Run("display name "+displayName, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newAIProviderCreateEnv(ctx, t)
			provider = editSpec(ctx, t, provider, func(s *coderv1alpha1.CoderAIProviderSpec) { s.DisplayName = displayName })
			got, _ := env.reconcile(ctx, t, provider)
			requireAIProviderReady(t, got)
			writes := env.writes()
			for range 3 {
				env.restart()
				again, _ := env.reconcile(ctx, t, got)
				require.Equal(t, got.ResourceVersion, again.ResourceVersion)
			}
			require.Equal(t, writes, env.writes())
		})
	}
}

// Dashboard edits of the display name and icon are drift, not kill switches:
// one sparse PATCH restores them and keeps the key.
func TestCoderAIProviderSyncDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	_, err := env.fake.client(t, 2*time.Second).UpdateAIProvider(ctx, provider.Status.ProviderID, codersdk.UpdateAIProviderRequest{
		DisplayName: ptrTo("Renamed"), Icon: ptrTo("/icon/other.svg"),
	})
	require.NoError(t, err)
	writes := env.writes()
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderReady(t, got)
	require.Equal(t, writes+1, env.writes())
	stored, _ := env.fake.aiProvider("openai")
	require.Equal(t, "OpenAI", stored.displayName)
	require.Equal(t, "/icon/openai.svg", stored.icon)
	require.Equal(t, provider.Status.Credential.KeyID, stored.keys[0].id.String(), "the key was not sent again")
}

// A Secret value change gives exactly one PATCH and a new change token. A
// metadata-only change gives none, until a restart empties the in-memory
// hash.
func TestCoderAIProviderKeyRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newReadyAIProvider(ctx, t)

	setSecretValue(ctx, t, secret, testAIProviderKeyRotated)
	writes := env.writes()
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderReady(t, got)
	require.Equal(t, writes+1, env.writes())
	stored, _ := env.fake.aiProvider("openai")
	require.Len(t, stored.keys, 1)
	require.Equal(t, testAIProviderKeyRotated, stored.keys[0].plaintext)
	require.Equal(t, stored.keys[0].id.String(), got.Status.Credential.KeyID)
	require.NotEqual(t, provider.Status.Credential.KeyID, got.Status.Credential.KeyID)
	require.Equal(t, secret.ResourceVersion, got.Status.Credential.SecretResourceVersion)
	require.Nil(t, got.Status.Credential.ResendAttemptTime)
	credentialsecrettest.RequireNoLeak(t, []string{testAIProviderKey, testAIProviderKeyRotated}, map[string]any{"status": got.Status})

	// A label change only: the token moves, Coder is not written.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	secret.Labels["team"] = "ml"
	require.NoError(t, k8sClient.Update(ctx, secret))
	writes = env.writes()
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, writes, env.writes())
	require.Equal(t, secret.ResourceVersion, got.Status.Credential.SecretResourceVersion)

	// After a restart the hash is gone, so one metadata-only change resends.
	env.restart()
	secret.Labels["team"] = "research"
	require.NoError(t, k8sClient.Update(ctx, secret))
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, writes+1, env.writes())
	got, _ = env.reconcile(ctx, t, got)
	require.Equal(t, writes+1, env.writes(), "the resend refills the hash")

	// Another key in the same Secret changes neither UID nor resourceVersion
	// of the reference, but it is another value.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	secret.Data["api-key-v2"] = []byte(testAIProviderKey + "-v2")
	require.NoError(t, k8sClient.Update(ctx, secret))
	got = editSpec(ctx, t, got, func(s *coderv1alpha1.CoderAIProviderSpec) { s.APIKeySecretRef.Key = "api-key-v2" })
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	stored, _ = env.fake.aiProvider("openai")
	require.Equal(t, testAIProviderKey+"-v2", stored.keys[0].plaintext)
	require.Equal(t, "api-key-v2", got.Status.Credential.SecretKey)
}

// #253 review round 3: a provider whose first apply failed is never enabled
// at its old base URL. URL and enabled flag travel in one PATCH.
func TestCoderAIProviderBaseURLChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "CoderUnavailable")

	got = editSpec(ctx, t, got, func(s *coderv1alpha1.CoderAIProviderSpec) { s.BaseURL = "https://api.openai.com/v2" })
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	stored, _ := env.fake.aiProvider("openai")
	require.False(t, stored.enabled, "a failed PATCH leaves the provider disabled")
	require.Equal(t, "https://api.openai.com/v1", stored.baseURL)

	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	stored, _ = env.fake.aiProvider("openai")
	require.True(t, stored.enabled)
	require.Equal(t, "https://api.openai.com/v2", stored.baseURL)

	// A later URL change keeps the key.
	keyID := got.Status.Credential.KeyID
	got = editSpec(ctx, t, got, func(s *coderv1alpha1.CoderAIProviderSpec) { s.BaseURL = "https://api.openai.com/v3" })
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	stored, _ = env.fake.aiProvider("openai")
	require.Equal(t, "https://api.openai.com/v3", stored.baseURL)
	require.Equal(t, keyID, stored.keys[0].id.String())
}

// Plan S12, combined edit: a new key Secret and a new origin go in one PATCH,
// so Coder never pairs the old key with the new URL.
func TestCoderAIProviderKeyAndURLTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newReadyAIProvider(ctx, t)
	other := env.keySecret(consentedObjectMeta("", "", "https://llm.example.com"), testAIProviderKeyRotated)
	other.Name = "llm-api-key"
	createSecret(ctx, t, other)
	got := editSpec(ctx, t, provider, func(s *coderv1alpha1.CoderAIProviderSpec) {
		s.BaseURL, s.APIKeySecretRef = "https://llm.example.com/v1", coderv1alpha1.SecretKeySelector{Name: "llm-api-key", Key: "api-key"}
	})
	writes := env.writes()
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, writes+1, env.writes(), "one PATCH")
	stored, _ := env.fake.aiProvider("openai")
	require.Equal(t, "https://llm.example.com/v1", stored.baseURL)
	require.Equal(t, testAIProviderKeyRotated, stored.keys[0].plaintext)
	require.Equal(t, "llm-api-key", got.Status.Credential.SecretName)
}

// Plan 5.3 rule 5: a crash after the key PATCH leaves the resend marker, and
// the next reconcile sends the key once more. Coder ends with that key only.
func TestCoderAIProviderResendCrash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newReadyAIProvider(ctx, t)
	setSecretValue(ctx, t, secret, testAIProviderKeyRotated)
	updates := env.fake.requestCount(routeUpdateAIProvider)
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Hang: true, AfterCommit: true})
	short, cancel := context.WithTimeout(ctx, hangTimeout)
	defer cancel()
	_, err := env.r.Reconcile(short, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.Error(t, err)
	stored := &coderv1alpha1.CoderAIProvider{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), stored))
	require.NotNil(t, stored.Status.Credential.ResendAttemptTime, "the marker was written before the PATCH")
	credentialsecrettest.RequireNoLeak(t, []string{testAIProviderKey, testAIProviderKeyRotated}, map[string]any{"error": err, "status": stored.Status})

	env.restart()
	got, _ := env.reconcile(ctx, t, stored)
	requireAIProviderReady(t, got)
	require.Nil(t, got.Status.Credential.ResendAttemptTime)
	require.Equal(t, updates+2, env.fake.requestCount(routeUpdateAIProvider), "the key was sent twice")
	coder, _ := env.fake.aiProvider("openai")
	require.Len(t, coder.keys, 1)
	require.Equal(t, testAIProviderKeyRotated, coder.keys[0].plaintext)
	require.Equal(t, coder.keys[0].id.String(), got.Status.Credential.KeyID)
}

// Plan S14: a dashboard delete, disable or key change after a full apply is a
// kill switch: Stalled, and zero writes until a spec edit re-arms the object.
func TestCoderAIProviderKillSwitches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason string
		act    func(ctx context.Context, c *codersdk.Client, id string) error
		check  func(t *testing.T, before coderv1alpha1.CoderAIProviderStatus, after fakeAIProvider)
	}{
		{reason: "DeletedExternally", act: func(ctx context.Context, c *codersdk.Client, id string) error {
			return c.DeleteAIProvider(ctx, id)
		}, check: func(t *testing.T, before coderv1alpha1.CoderAIProviderStatus, after fakeAIProvider) {
			require.NotEqual(t, before.ProviderID, after.id.String(), "a spec edit creates the provider again")
		}},
		{reason: "DisabledExternally", act: func(ctx context.Context, c *codersdk.Client, id string) error {
			_, err := c.UpdateAIProvider(ctx, id, codersdk.UpdateAIProviderRequest{Enabled: ptrTo(false)})
			return err
		}, check: func(t *testing.T, _ coderv1alpha1.CoderAIProviderStatus, after fakeAIProvider) {
			require.True(t, after.enabled)
		}},
		{reason: "KeyChangedExternally", act: func(ctx context.Context, c *codersdk.Client, id string) error {
			_, err := c.UpdateAIProvider(ctx, id, codersdk.UpdateAIProviderRequest{
				APIKeys: &[]codersdk.AIProviderKeyMutation{{APIKey: ptrTo("sk-dashboard-0123456789abcdef")}},
			})
			return err
		}, check: func(t *testing.T, _ coderv1alpha1.CoderAIProviderStatus, after fakeAIProvider) {
			require.Len(t, after.keys, 1)
			require.Equal(t, testAIProviderKey, after.keys[0].plaintext, "a spec edit sends the key again")
		}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newReadyAIProvider(ctx, t)
			require.NoError(t, tc.act(ctx, env.fake.client(t, 2*time.Second), provider.Status.ProviderID))
			writes := env.writes()
			got, _ := env.reconcile(ctx, t, provider)
			requireAIProviderStep(t, got, true, tc.reason)
			for range 2 {
				env.restart()
				got, _ = env.reconcile(ctx, t, got)
				requireAIProviderStep(t, got, true, tc.reason)
			}
			require.Equal(t, writes, env.writes(), "a kill switch sends nothing")

			// A spec edit whose apply fails does not count as applied, so the
			// next reconcile applies again instead of reporting a kill switch.
			got = editSpec(ctx, t, got, func(s *coderv1alpha1.CoderAIProviderSpec) { s.DisplayName = "OpenAI v2" })
			env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusBadGateway})
			env.fake.failNext(routeCreateAIProvider, fakeFault{Status: http.StatusBadGateway})
			got, _ = env.reconcile(ctx, t, got)
			require.NotEqual(t, got.Generation, got.Status.AppliedGeneration)
			for range 3 {
				if got, _ = env.reconcile(ctx, t, got); got.Status.AppliedGeneration == got.Generation {
					break
				}
			}
			requireAIProviderReady(t, got)
			stored, _ := env.fake.aiProvider("openai")
			require.Equal(t, "OpenAI v2", stored.displayName)
			tc.check(t, provider.Status, stored)
		})
	}
}

// Plan S3 end to end: Coder echoes the key in Message, Detail and
// validations of failed writes. The key appears in no status, log or error.
func TestCoderAIProviderErrorsHoldNoKey(t *testing.T) {
	t.Parallel()
	echo := fakeFault{Status: http.StatusBadRequest, Message: "bad key " + testAIProviderKeyRotated, Detail: "key " + testAIProviderKeyRotated}
	ctx := context.Background()
	env, secret, provider := newReadyAIProvider(ctx, t)
	setSecretValue(ctx, t, secret, testAIProviderKeyRotated)
	env.fake.failNext(routeUpdateAIProvider, echo)
	got, _ := env.reconcile(ctx, t, provider)
	require.Contains(t, requireAIProviderStep(t, got, true, "CoderRejected"), "[REDACTED]")

	other, _, fresh := newAIProviderCreateEnv(ctx, t)
	echo.Message, echo.Detail = "bad key "+testAIProviderKey, "key "+testAIProviderKey
	other.fake.failNext(routeCreateAIProvider, echo)
	got, _ = other.reconcile(ctx, t, fresh)
	require.Contains(t, requireAIProviderStep(t, got, true, "CoderRejected"), "[REDACTED]")
}

// The 401, 403 and answer checks of the sync PATCH.
func TestCoderAIProviderSyncAnswers(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status)+" is retried", func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newAIProviderCreateEnv(ctx, t)
			env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: status})
			got, result := env.reconcile(ctx, t, provider)
			requireAIProviderStep(t, got, false, "CoderUnavailable")
			require.Equal(t, 30*time.Second, result.RequeueAfter)
			got, _ = env.reconcile(ctx, t, got)
			requireAIProviderReady(t, got)
		})
	}
	t.Run("answer without the requested values", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newAIProviderCreateEnv(ctx, t)
		env.fake.failNext(routeUpdateAIProvider, fakeFault{Rewrite: func(answer any) any {
			p := answer.(codersdk.AIProvider)
			p.Enabled = false
			return p
		}})
		got, _ := env.reconcile(ctx, t, provider)
		requireAIProviderStep(t, got, true, "CoderAnswerMismatch")
		require.Zero(t, got.Status.AppliedGeneration)
	})
	t.Run("deleted before a full apply is created again", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newAIProviderCreateEnv(ctx, t)
		env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
		got, _ := env.reconcile(ctx, t, provider)
		oldID := got.Status.ProviderID
		require.NoError(t, env.fake.client(t, 2*time.Second).DeleteAIProvider(ctx, oldID))
		got, _ = env.reconcile(ctx, t, got)
		requireAIProviderReady(t, got)
		require.NotEqual(t, oldID, got.Status.ProviderID)
		require.Equal(t, 2, env.fake.requestCount(routeCreateAIProvider))
	})
}

// A nonce provider found without a recorded credential (a create that
// committed after its marker was cleared) is ours: the controller records it
// and sends the current key, instead of failing an assertion.
func TestCoderAIProviderNonceWithoutCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	got := &coderv1alpha1.CoderAIProvider{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
	_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: "openai", DisplayName: "coder-k8s:" + string(got.UID),
		BaseURL: "https://api.openai.com/v1", APIKeys: []string{"sk-late-commit-0123456789ab"},
	})
	require.NoError(t, err)
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	stored, _ := env.fake.aiProvider("openai")
	require.Equal(t, stored.id.String(), got.Status.ProviderID)
	require.Equal(t, testAIProviderKey, stored.keys[0].plaintext, "the current key replaced the unknown one")
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider), "only the late commit")
}
