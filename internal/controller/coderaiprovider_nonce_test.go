package controller_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// createOldCleanupRecord writes a cleanup record as an operator version
// before #426 wrote it, with the nonce coder-k8s:<UID>.
func createOldCleanupRecord(ctx context.Context, t *testing.T, namespace, kind string, uid types.UID, data map[string]string) {
	t.Helper()
	data["kind"], data["uid"], data["nonce"] = kind, string(uid), "coder-k8s:"+string(uid)
	require.NoError(t, k8sClient.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "ck8s-cleanup-" + string(uid), Labels: map[string]string{"coder.com/cleanup-record": kind}},
		Data:       data,
	}))
}

// requireRandomNonce checks that a recorded nonce is the temporary display
// name prefix and a value that nobody can derive from the object.
func requireRandomNonce(t *testing.T, nonce string, uid types.UID) {
	t.Helper()
	require.True(t, strings.HasPrefix(nonce, "coder-k8s:"), nonce)
	require.Greater(t, len(nonce), len("coder-k8s:")+20, nonce)
	require.NotContains(t, nonce, string(uid), "the nonce is not the object UID")
}

// #426: anyone who can read the object sees its UID, so a provider that a
// person creates with the object's name, its type and the display name
// coder-k8s:<UID> is not this object's. The controller does not adopt it,
// does not change it, and the delete of the object leaves it.
func TestCoderAIProviderUIDDisplayNameNotAdopted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	const personsKey = "sk-person-0123456789abcdef" // #nosec G101 -- test fixture, not a credential.
	_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: "coder-k8s:" + string(provider.UID),
		BaseURL: provider.Spec.BaseURL, APIKeys: []string{personsKey},
	})
	require.NoError(t, err)

	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, true, "NameConflict")
	require.Empty(t, got.Status.ProviderID, "the person's provider is not adopted")
	stored, _ := env.fake.aiProvider(provider.Name)
	require.Equal(t, "coder-k8s:"+string(provider.UID), stored.displayName, "the controller did not change it")
	require.Equal(t, personsKey, stored.keys[0].plaintext)

	gone, _ := env.deleteAIProvider(ctx, t, got)
	require.Nil(t, gone)
	require.Zero(t, env.fake.requestCount(routeDeleteAIProvider))
	_, ok := env.fake.aiProvider(provider.Name)
	require.True(t, ok, "the person's provider stays")
}

// #426: the create sends a random nonce from the cleanup record as the
// display name, never the object UID. After a lost create answer, the next
// reconcile adopts the provider that carries that nonce.
func TestCoderAIProviderLostCreateAnswerRecoversByNonce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{AfterCommit: true, Reset: true})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "ConfirmingCreate")
	stored, ok := env.fake.aiProvider(provider.Name)
	require.True(t, ok, "the create committed")
	nonce := cleanupRecord(ctx, t, env.namespace, got.UID)["nonce"]
	require.Equal(t, nonce, stored.displayName, "the create sent the recorded nonce")
	requireRandomNonce(t, nonce, got.UID)

	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, stored.id.String(), got.Status.ProviderID, "the provider with the nonce is adopted")
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIProvider))
}

// #426: while a create can still commit, the next create sends the same
// nonce, so a late commit is still recognized. Once the operator knows the
// provider ID, Coder has shown the nonce as the display name, so the next
// create gets a new one.
func TestCoderAIProviderNonceReuse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeCreateAIProvider, fakeFault{Status: http.StatusTooManyRequests})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "CreateRetrying")
	first := cleanupRecord(ctx, t, env.namespace, got.UID)["nonce"]
	requireRandomNonce(t, first, got.UID)

	env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	stored, _ := env.fake.aiProvider(provider.Name)
	require.Equal(t, first, stored.displayName, "the second create sent the nonce of the first")

	require.NoError(t, env.fake.client(t, 2*time.Second).DeleteAIProvider(ctx, stored.id.String()))
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, 3, env.fake.requestCount(routeCreateAIProvider))
	second := cleanupRecord(ctx, t, env.namespace, got.UID)["nonce"]
	requireRandomNonce(t, second, got.UID)
	require.NotEqual(t, first, second, "a create after a known provider ID gets a new nonce")
}

// #426 migration: a record of an operator version before #426 keeps its
// nonce coder-k8s:<UID> until the next create, so a create of that version
// whose answer was lost is still adopted. The next create gets a random
// nonce.
func TestCoderAIProviderOldNonceAdoptedUntilNextCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, secret, provider := newAIProviderCreateEnv(ctx, t)
	_, err := env.fake.client(t, 2*time.Second).CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: "coder-k8s:" + string(provider.UID),
		BaseURL: provider.Spec.BaseURL, APIKeys: []string{testAIProviderKey},
	})
	require.NoError(t, err)
	attempt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	createOldCleanupRecord(ctx, t, env.namespace, "CoderAIProvider", provider.UID, map[string]string{
		"namespace": env.namespace, "name": provider.Name, "deploymentID": env.fake.deploymentID, "coderID": "", "type": "openai",
		"createAttemptTime": attempt.Format(time.RFC3339),
	})
	provider.Status.CreateAttemptTime = &metav1.Time{Time: attempt}
	provider.Status.Credential = &coderv1alpha1.CoderAIProviderCredentialStatus{SecretName: secret.Name, SecretKey: "api-key"}
	require.NoError(t, k8sClient.Status().Update(ctx, provider))

	env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	stored, _ := env.fake.aiProvider(provider.Name)
	require.Equal(t, stored.id.String(), got.Status.ProviderID, "the create of the older version is adopted")
	require.Equal(t, "coder-k8s:"+string(provider.UID), cleanupRecord(ctx, t, env.namespace, got.UID)["nonce"])

	require.NoError(t, env.fake.client(t, 2*time.Second).DeleteAIProvider(ctx, stored.id.String()))
	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderReady(t, got)
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIProvider), "the setup create and one create of the controller")
	requireRandomNonce(t, cleanupRecord(ctx, t, env.namespace, got.UID)["nonce"], got.UID)
}

// #426: after the operator learned the provider ID, Coder has shown the nonce
// as the display name. A person who saw it can delete the provider and create
// one of the same name with that display name. It is not this object's.
func TestCoderAIProviderShownNonceNotAdopted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _, provider := newAIProviderCreateEnv(ctx, t)
	env.fake.failNext(routeUpdateAIProvider, fakeFault{Status: http.StatusInternalServerError})
	got, _ := env.reconcile(ctx, t, provider)
	requireAIProviderStep(t, got, false, "CoderUnavailable")
	stored, _ := env.fake.aiProvider(provider.Name)
	nonce := stored.displayName
	require.Equal(t, cleanupRecord(ctx, t, env.namespace, got.UID)["nonce"], nonce)

	person := env.fake.client(t, 2*time.Second)
	require.NoError(t, person.DeleteAIProvider(ctx, stored.id.String()))
	_, err := person.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderTypeOpenAI, Name: provider.Name, DisplayName: nonce,
		BaseURL: provider.Spec.BaseURL, APIKeys: []string{"sk-person-0123456789abcdef"},
	})
	require.NoError(t, err)

	got, _ = env.reconcile(ctx, t, got)
	requireAIProviderStep(t, got, true, "NameConflict")
	require.Empty(t, got.Status.ProviderID, "the person's provider is not adopted")
	stored, _ = env.fake.aiProvider(provider.Name)
	require.Equal(t, nonce, stored.displayName, "the controller did not change it")
}
