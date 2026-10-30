package controller_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

// hangTimeout ends a reconcile whose Coder request hangs. It leaves room for
// the requests before the hang on a loaded test host.
const hangTimeout = 10 * time.Second

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
	env.r = &controller.CoderAIProviderReconciler{Client: k8sClient, Scheme: scheme, OperatorNamespace: env.namespace, APIReader: k8sClient}
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
	require.Equal(t, cleanupRecord(ctx, t, env.namespace, got.UID)["nonce"], stored.displayName, "the nonce proves who created it")
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
	require.Equal(t, testAIProviderKey, stored.keys[0].plaintext, "the sync step does not resend an unchanged key")
	require.Equal(t, got.Status.Credential.KeyID, stored.keys[0].id.String())

	// A ready provider costs one read and no write or status update.
	before := env.fake.totalRequests()
	again, _ := env.reconcile(ctx, t, got)
	require.Equal(t, before+1, env.fake.totalRequests())
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

// #332: a read that confirms a create does not prove which key Coder holds.
// A key changed in the dashboard between the lost create answer and that
// read is replaced by the Secret's key before the object reports Ready.
func TestCoderAIProviderConfirmedCreateResendsKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{AfterCommit: true, Reset: true})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "ConfirmingCreate")

	stored, ok := env.fake.aiProvider("openai")
	require.True(t, ok, "the create committed")
	_, err := env.fake.client(t, 2*time.Second).UpdateAIProvider(ctx, stored.id.String(), codersdk.UpdateAIProviderRequest{
		APIKeys: &[]codersdk.AIProviderKeyMutation{{APIKey: ptrTo("sk-dashboard-0123456789abcdef")}},
	})
	require.NoError(t, err)

	env.restart()
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	stored, _ = env.fake.aiProvider("openai")
	require.Len(t, stored.keys, 1)
	require.Equal(t, testAIProviderKey, stored.keys[0].plaintext, "the Secret's key replaced the dashboard key")
	require.Equal(t, stored.keys[0].id.String(), got.Status.Credential.KeyID)
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))
}

// #332: the in-memory record of a sent key does not survive a read that
// confirms the create. When the status write after a create answer fails and
// the key changes in the dashboard before the retry, the Secret's key
// replaces the dashboard key in one PATCH, and the steady state writes nothing.
func TestCoderAIProviderConfirmedCreateAfterStatusFailureResendsKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newAIProviderCreateEnv(ctx, t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	statusErr := errors.New("injected status write failure")
	var failed atomic.Bool
	env.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if p, ok := obj.(*coderv1alpha1.CoderAIProvider); ok && p.Status.ProviderID != "" && failed.CompareAndSwap(false, true) {
				return statusErr
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	_, err = env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.ErrorIs(t, err, statusErr)
	got := &coderv1alpha1.CoderAIProvider{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), got))
	require.Empty(t, got.Status.ProviderID, "the create answer was not stored")

	stored, ok := env.fake.aiProvider("openai")
	require.True(t, ok, "the create committed")
	_, err = env.fake.client(t, 2*time.Second).UpdateAIProvider(ctx, stored.id.String(), codersdk.UpdateAIProviderRequest{
		APIKeys: &[]codersdk.AIProviderKeyMutation{{APIKey: ptrTo("sk-dashboard-0123456789abcdef")}},
	})
	require.NoError(t, err)

	updates := env.fake.requestCount(routeUpdateAIProvider)
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	stored, _ = env.fake.aiProvider("openai")
	require.Len(t, stored.keys, 1)
	require.Equal(t, testAIProviderKey, stored.keys[0].plaintext, "the Secret's key replaced the dashboard key")
	require.Equal(t, stored.keys[0].id.String(), got.Status.Credential.KeyID)
	require.Equal(t, secret.ResourceVersion, got.Status.Credential.SecretResourceVersion)
	require.Equal(t, updates+1, env.fake.requestCount(routeUpdateAIProvider), "one PATCH applies the spec and the key")
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))

	writes, resourceVersion := env.writes(), got.ResourceVersion
	for i := range 3 {
		if i == 2 {
			env.restart()
		}
		got, _ = env.reconcile(ctx, t, got)
		requireAIProviderReady(t, got)
		require.Equal(t, resourceVersion, got.ResourceVersion, "the steady state writes no status")
		require.Equal(t, writes, env.writes(), "the steady state sends no write to Coder")
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

			short, cancel := context.WithTimeout(ctx, hangTimeout)
			defer cancel()
			_, err := env.r.Reconcile(short, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
			require.Error(t, err, "the status write after the hang fails with the expired context")
			stored := &coderv1alpha1.CoderAIProvider{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), stored))
			require.NotNil(t, stored.Status.CreateAttemptTime, "the marker was written before the request")
			credentialsecrettest.RequireNoLeak(t, []string{testAIProviderKey}, map[string]any{"error": err, "status": stored.Status})

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
	short, cancel := context.WithTimeout(ctx, hangTimeout)
	defer cancel()
	_, err := env.r.Reconcile(short, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.Error(t, err, "the status write after the hang fails with the expired context")
	credentialsecrettest.RequireNoLeak(t, []string{testAIProviderKey}, map[string]any{"error": err})
	stored, ok := env.fake.aiProvider("openai")
	require.True(t, ok, "the create committed")
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

// #253 review round 3: a 4xx that replaces a committed create answer, which
// Coder itself never sends after a commit, is confirmed by a read. The
// object then owns the provider instead of failing an assertion.
func TestCoderAIProviderCreateRejectedAfterCommit(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotAcceptable, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newAIProviderCreateEnv(ctx, t)
			env.fake.failNext(routeCreateAIProvider, fakeFault{AfterCommit: true, Status: status, Detail: "echo " + testAIProviderKey})
			got, _ := env.reconcile(ctx, t, provider)
			requireAIProviderReady(t, got)
			require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))
			require.Equal(t, 1, env.fake.aiProviderCount())
			stored, _ := env.fake.aiProvider("openai")
			require.Equal(t, stored.id.String(), got.Status.ProviderID)
		})
	}
}

// A rejected create is not sent again when the read that confirms absence
// fails once.
func TestCoderAIProviderRejectedCreateConfirmFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeAIProvider, fakeFault{Rewrite: func(answer any) any { return answer }}) // The name lookup passes.
	env.fake.failNext(routeAIProvider, fakeFault{Status: http.StatusBadGateway})                   // The confirmation read fails.
	env.fake.failNext(routeCreateAIProvider, fakeFault{Status: http.StatusBadRequest})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "ConfirmingRejectedCreate")
	require.NotNil(t, got.Status.CreateAttemptTime)
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, true, "CoderRejected")
	require.Nil(t, got.Status.CreateAttemptTime)
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider), "the rejected create was not sent again")
}

// A create answer without a provider ID is impossible, so it fails fast.
// The marker stays, and a read decides on the next reconcile.
func TestCoderAIProviderCreateAnswerWithoutID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{Rewrite: func(answer any) any {
		p := answer.(codersdk.AIProvider)
		p.ID = uuid.Nil
		return p
	}})
	_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)})
	require.ErrorContains(t, err, "assertion failed: Coder answered AI provider openai without an ID")
	stored := &coderv1alpha1.CoderAIProvider{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), stored))
	require.NotNil(t, stored.Status.CreateAttemptTime)
	got, _ := env.reconcile(ctx, t, stored)
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
	env.fake.failNext(routeAIProvider, fakeFault{Status: http.StatusNotFound, Message: coderResourceNotFound})

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

// #268 item 3: a rejected create stays remembered across a failed site-owner
// check and a failed read, so it is not sent again.
func TestCoderAIProviderRejectedCreateOwnerCheckFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{Status: http.StatusBadRequest})
	env.fake.failNext(routeUser, fakeFault{Status: http.StatusBadGateway})
	got, _ := env.reconcile(ctx, t, provider)
	require.Contains(t, requireAIProviderStep(t, got, false, "ConfirmingRejectedCreate"), "get operator user")

	env.fake.failNext(routeAIProvider, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireAIProviderStep(t, got, false, "ConfirmingRejectedCreate"), "get provider openai")

	env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{} })
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireAIProviderStep(t, got, false, "ConfirmingRejectedCreate"), "not a site owner")

	env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) { u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner}} })
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, true, "CoderRejected")
	require.Nil(t, got.Status.CreateAttemptTime)
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider), "the rejected create was not sent again")
}

// #338: a 404 that Coder did not send on the name lookup proves nothing. A
// pending create keeps its marker and sends no create, a new create is not
// sent, and the next reconcile with Coder's own answer decides.
func TestCoderAIProviderCreateLookupNotCoder404(t *testing.T) {
	t.Parallel()
	for name, fault := range notCoder404s {
		for _, pending := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pending=%v", name, pending), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				env, secret, provider := newAIProviderCreateEnv(ctx, t)
				if pending {
					// A create that committed, but whose answer was lost.
					_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
						Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: "coder-k8s:" + string(provider.UID),
						BaseURL: provider.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
					})
					require.NoError(t, err)
					provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now()}
					provider.Status.Credential = &coderv1alpha1.CoderAIProviderCredentialStatus{SecretName: secret.Name, SecretKey: "api-key"}
					require.NoError(t, k8sClient.Status().Update(ctx, provider))
				}
				posts := env.fake.requestCount(routeCreateAIProvider)
				env.fake.failNext(routeAIProvider, fault)
				got, result := env.reconcile(ctx, t, provider)
				requireAIProviderStep(t, got, false, "CoderUnavailable")
				require.Equal(t, 30*time.Second, result.RequeueAfter)
				require.Equal(t, posts, env.fake.requestCount(routeCreateAIProvider), "no create is sent")
				require.Equal(t, pending, got.Status.CreateAttemptTime != nil, "a pending create keeps its marker")

				got, _ = env.reconcile(ctx, t, got)
				requireAIProviderReady(t, got)
				require.Equal(t, 1, env.fake.aiProviderCount(), "one provider in Coder")
			})
		}
	}
}

// #338: after a rejected create, the read that confirms absence also needs
// Coder's own 404. Otherwise the marker stays and the create is not sent
// again.
func TestCoderAIProviderRejectedCreateNotCoder404(t *testing.T) {
	t.Parallel()
	for name, fault := range notCoder404s {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env, _, provider := newAIProviderCreateEnv(ctx, t)
			env.fake.failNext(routeAIProvider, fakeFault{Rewrite: func(answer any) any { return answer }}) // The name lookup passes.
			env.fake.failNext(routeAIProvider, fault)                                                      // The confirmation read.
			env.fake.failNext(routeCreateAIProvider, fakeFault{Status: http.StatusBadRequest})
			got, _ := env.reconcile(ctx, t, provider)
			requireAIProviderStep(t, got, false, "ConfirmingRejectedCreate")
			require.NotNil(t, got.Status.CreateAttemptTime, "the marker stays")
			got, _ = env.reconcile(ctx, t, got)
			requireAIProviderStep(t, got, true, "CoderRejected")
			require.Nil(t, got.Status.CreateAttemptTime)
			require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider), "the rejected create was not sent again")
		})
	}
}
