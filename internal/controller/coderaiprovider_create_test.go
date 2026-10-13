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
	"github.com/coder/coder-k8s/internal/controller"
)

// newAIProviderCreateEnv returns a namespace with a ready control plane, a
// consented key Secret and a provider object named openai.
func newAIProviderCreateEnv(ctx context.Context, t *testing.T) (aiProviderEnv, *corev1.Secret, *coderv1alpha1.CoderAIProvider) {
	t.Helper()
	env := newAIProviderEnv(ctx, t, true)
	secret := createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKey))
	provider := env.createProvider(ctx, t, func(s *coderv1alpha1.CoderAIProviderSpec) {
		s.DisplayName, s.Icon = "OpenAI", "/icon/openai.svg"
	})
	return env, secret, provider
}

// restart gives env a new reconciler, as a controller restart does. The
// reconciler keeps no state between calls, so every step resumes from status.
func (env *aiProviderEnv) restart() {
	env.r = &controller.CoderAIProviderReconciler{Client: k8sClient, Scheme: scheme}
}

func TestCoderAIProviderCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newAIProviderCreateEnv(ctx, t)

	// Stop after the create to see what it sent.
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
	got, result := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	require.Equal(t, 30*time.Second, result.RequeueAfter)
	stored, ok := env.fake.aiProvider("openai")
	require.True(t, ok)
	require.False(t, stored.enabled, "the provider starts disabled")
	require.Equal(t, "coder-k8s:"+string(got.UID), stored.displayName, "the nonce proves who created it")
	require.Equal(t, codersdk.AIProviderTypeOpenAI, stored.providerType)
	require.Equal(t, "https://api.openai.com/v1", stored.baseURL)
	require.Len(t, stored.keys, 1)
	require.Equal(t, testAIProviderKey, stored.keys[0].plaintext)
	require.Equal(t, stored.id.String(), got.Status.ProviderID)
	require.Nil(t, got.Status.CreateAttemptTime)
	require.Equal(t, &coderv1alpha1.CoderAIProviderCredentialStatus{
		SecretName: secret.Name, SecretKey: "api-key", SecretUID: string(secret.UID), SecretResourceVersion: secret.ResourceVersion,
		KeyID: stored.keys[0].id.String(),
	}, got.Status.Credential)
	require.Zero(t, got.Status.AppliedGeneration)

	env.restart()
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	stored, _ = env.fake.aiProvider("openai")
	require.True(t, stored.enabled)
	require.Equal(t, "OpenAI", stored.displayName)
	require.Equal(t, "/icon/openai.svg", stored.icon)
	require.Equal(t, testAIProviderKey, stored.keys[0].plaintext, "the apply step does not touch the key")

	// A ready provider needs no Coder call and no status write.
	before := env.fake.totalRequests()
	again, _ := env.reconcile(ctx, t, got)
	require.Equal(t, before, env.fake.totalRequests())
	require.Equal(t, got.ResourceVersion, again.ResourceVersion)
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))
	require.Equal(t, 2, env.fake.requestCount(routeUpdateAIProvider))
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
}

// Plan S5 and each outcome class of section 5.6: an uncertain create keeps
// the marker and is never sent again before a read, and every class ends
// with exactly one provider that this object owns.
func TestCoderAIProviderCreateOutcomes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		fault      fakeFault
		firstStep  string
		posts      int // Create requests in all.
		keepMarker bool
	}{
		{name: "answer lost after commit", fault: fakeFault{AfterCommit: true, Reset: true}, firstStep: "ConfirmingCreate", posts: 1, keepMarker: true},
		{name: "connection reset before commit", fault: fakeFault{Reset: true}, firstStep: "ConfirmingCreate", posts: 2, keepMarker: true},
		{name: "HTML 200 after commit", fault: fakeFault{AfterCommit: true, Status: http.StatusOK, HTML: "<html>proxy</html>"}, firstStep: "ConfirmingCreate", posts: 1, keepMarker: true},
		{name: "HTML 502 before commit", fault: fakeFault{Status: http.StatusBadGateway, HTML: "<html>bad gateway</html>"}, firstStep: "ConfirmingCreate", posts: 2, keepMarker: true},
		{name: "500 after commit", fault: fakeFault{AfterCommit: true, Status: http.StatusInternalServerError}, firstStep: "ConfirmingCreate", posts: 1, keepMarker: true},
		{name: "answer for another name", fault: fakeFault{AfterCommit: true, Rewrite: func(answer any) any {
			p := answer.(codersdk.AIProvider)
			p.Name = "other"
			return p
		}}, firstStep: "ConfirmingCreate", posts: 1, keepMarker: true},
		{name: "answer without the key", fault: fakeFault{AfterCommit: true, Rewrite: func(answer any) any {
			p := answer.(codersdk.AIProvider)
			p.APIKeys = nil
			return p
		}}, firstStep: "ConfirmingCreate", posts: 1, keepMarker: true},
		{name: "rate limited", fault: fakeFault{Status: http.StatusTooManyRequests}, firstStep: "CreateRetrying", posts: 2},
		{name: "forbidden", fault: fakeFault{Status: http.StatusForbidden}, firstStep: "CreateRetrying", posts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newAIProviderCreateEnv(ctx, t)
			env.fake.failNext(routeCreateAIProvider, tc.fault)

			got, result := env.reconcile(ctx, t, provider)
			requireAIProviderStep(t, got, false, tc.firstStep)
			require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))
			require.Empty(t, got.Status.ProviderID)
			if tc.keepMarker {
				require.NotNil(t, got.Status.CreateAttemptTime, "an uncertain create keeps the marker")
				require.NotNil(t, got.Status.Credential)
				require.Equal(t, 5*time.Second, result.RequeueAfter)
			} else {
				require.Nil(t, got.Status.CreateAttemptTime, "a create without effect clears the marker")
				require.Nil(t, got.Status.Credential)
			}

			for range 2 {
				env.restart()
				got, _ = env.reconcile(ctx, t, got)
			}
			requireAIProviderReady(t, got)
			require.Equal(t, tc.posts, env.fake.requestCount(routeCreateAIProvider))
			require.Equal(t, 1, env.fake.aiProviderCount(), "exactly one provider")
			stored, _ := env.fake.aiProvider("openai")
			require.Equal(t, stored.id.String(), got.Status.ProviderID)
			require.Equal(t, stored.keys[0].id.String(), got.Status.Credential.KeyID)
			require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
		})
	}
}

// A create that hangs ends with the reconcile's context. The marker is
// already stored, so the next reconcile reads Coder first.
func TestCoderAIProviderCreateHangs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		committed bool
	}{{name: "before commit"}, {name: "after commit", committed: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newAIProviderCreateEnv(ctx, t)
			env.fake.failNext(routeCreateAIProvider, fakeFault{Hang: true, AfterCommit: tc.committed})

			short, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			_, err := env.r.Reconcile(short, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
			require.Error(t, err, "the status write after the hang fails with the expired context")
			stored := &coderv1alpha1.CoderAIProvider{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), stored))
			require.NotNil(t, stored.Status.CreateAttemptTime, "the marker was written before the request")

			env.restart()
			got, _ := env.reconcile(ctx, t, stored)
			requireAIProviderReady(t, got)
			posts := 2
			if tc.committed {
				posts = 1
			}
			require.Equal(t, posts, env.fake.requestCount(routeCreateAIProvider))
			require.Equal(t, 1, env.fake.aiProviderCount())
		})
	}
}

// The provider ID is stored before the apply replaces the nonce, so a crash
// between the apply and the next status write keeps ownership.
func TestCoderAIProviderCrashAfterApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Hang: true, AfterCommit: true})
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := env.r.Reconcile(short, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.Error(t, err, "the status write after the hang fails with the expired context")
	stored, _ := env.fake.aiProvider("openai")
	require.Equal(t, "OpenAI", stored.displayName, "the apply committed and replaced the nonce")
	got := &coderv1alpha1.CoderAIProvider{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
	require.Equal(t, stored.id.String(), got.Status.ProviderID, "the ID was stored before the apply")
	require.Nil(t, got.Status.CreateAttemptTime)

	env.restart()
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))
}

// Plan S4: a provider that someone else creates between the lookup and the
// create gives NameConflict, and the controller never changes it.
func TestCoderAIProviderCreateConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	foreign, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: "openai", DisplayName: "Team key", BaseURL: "https://api.openai.com/v1", Enabled: true,
	})
	require.NoError(t, err)
	setupPosts := env.fake.requestCount(routeCreateAIProvider)
	// The lookup does not see it yet.
	env.fake.failNext(routeAIProvider, fakeFault{Status: http.StatusNotFound})

	got, _ := env.reconcile(ctx, t, provider)
	require.Contains(t, requireAIProviderStep(t, got, true, "NameConflict"), foreign.ID.String())
	require.Nil(t, got.Status.CreateAttemptTime)
	require.Nil(t, got.Status.Credential)
	require.Empty(t, got.Status.ProviderID)
	env.restart()
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, true, "NameConflict")

	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider)-setupPosts, "one create, then never again")
	require.Zero(t, env.fake.requestCount(routeUpdateAIProvider))
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
	stored, _ := env.fake.aiProvider("openai")
	require.Equal(t, foreign.ID, stored.id)
	require.Equal(t, "Team key", stored.displayName)
}

// A rejected create is not sent again until the spec changes.
func TestCoderAIProviderCreateRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{Status: http.StatusBadRequest})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, true, "CoderRejected")
	require.Nil(t, got.Status.CreateAttemptTime)
	before := env.fake.totalRequests()
	env.restart()
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, true, "CoderRejected")
	require.Equal(t, before, env.fake.totalRequests())

	got.Spec.DisplayName = "OpenAI v2"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIProvider))
}

// Plan 5.6 rule 4: a missing provider proves nothing unless the operator
// user is a site owner, so the create is not sent again.
func TestCoderAIProviderConfirmNeedsSiteOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{Reset: true})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "ConfirmingCreate")

	env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{} })
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireAIProviderStep(t, got, false, "ConfirmingCreate"), "not a site owner")
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))

	env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner}} })
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIProvider))
}

// The apply step only sets display name, icon and enabled. A changed key or
// a provider deleted in Coder does not count as applied.
func TestCoderAIProviderApplyGuards(t *testing.T) {
	t.Parallel()
	t.Run("key Secret changed after the create", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, secret, provider := newAIProviderCreateEnv(ctx, t)
		env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
		got, _ := env.reconcile(ctx, t, provider)
		require.NotEmpty(t, got.Status.ProviderID)

		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
		secret.Data["api-key"] = []byte(testAIProviderKey + "-rotated")
		require.NoError(t, k8sClient.Update(ctx, secret))
		got, _ = env.reconcile(ctx, t, got)
		require.Contains(t, requireAIProviderStep(t, got, false, "Pending"), "key Secret changed")
		require.Equal(t, 1, env.fake.requestCount(routeUpdateAIProvider), "no update with a changed key")
		stored, _ := env.fake.aiProvider("openai")
		require.Equal(t, testAIProviderKey, stored.keys[0].plaintext)
	})
	t.Run("base URL changed after the create", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newAIProviderCreateEnv(ctx, t)
		env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
		got, _ := env.reconcile(ctx, t, provider)
		require.NotEmpty(t, got.Status.ProviderID)

		got.Spec.BaseURL = "https://api.openai.com/v2" // Same origin, so the key is still allowed.
		require.NoError(t, k8sClient.Update(ctx, got))
		got, _ = env.reconcile(ctx, t, got)
		require.Contains(t, requireAIProviderStep(t, got, false, "Pending"), "base URL changed")
		require.Zero(t, got.Status.AppliedGeneration)
		stored, _ := env.fake.aiProvider("openai")
		require.Equal(t, "https://api.openai.com/v1", stored.baseURL)
	})
	t.Run("update answer without the requested values", func(t *testing.T) {
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
	t.Run("key Secret rotated after Ready", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, secret, provider := newAIProviderCreateEnv(ctx, t)
		got, _ := env.reconcile(ctx, t, provider)
		requireAIProviderReady(t, got)

		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
		secret.Data["api-key"] = []byte(testAIProviderKey + "-rotated")
		require.NoError(t, k8sClient.Update(ctx, secret))
		got, _ = env.reconcile(ctx, t, got)
		require.Contains(t, requireAIProviderStep(t, got, false, "Pending"), "key Secret changed")
		require.Equal(t, got.Generation, got.Status.AppliedGeneration, "the spec is still applied")
	})
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status)+" on the apply is retried", func(t *testing.T) {
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
	t.Run("deleted in Coder before the apply", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		env, _, provider := newAIProviderCreateEnv(ctx, t)
		env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
		got, _ := env.reconcile(ctx, t, provider)
		require.NoError(t, env.fake.client(t, 2*time.Second).DeleteAIProvider(ctx, got.Status.ProviderID))

		got, _ = env.reconcile(ctx, t, got)
		requireAIProviderStep(t, got, true, "DeletedExternally")
		require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider), "no new create")
		require.Zero(t, got.Status.AppliedGeneration)
	})
}
