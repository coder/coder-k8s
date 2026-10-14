package controller_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// pendingMCPServer applies a server, then makes a spec edit whose PATCH
// fails, so the object has a generation that is not applied yet.
func (env mcpServerEnv) pendingMCPServer(ctx context.Context, t *testing.T) (*coderv1alpha1.CoderMCPServer, *corev1.Secret) {
	t.Helper()
	secret := env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.Description = "pending"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, false, "CoderUnavailable")
	require.NotEqual(t, got.Generation, got.Status.AppliedGeneration)
	return got, secret
}

// restoreSecret creates the key Secret again with its consent.
func restoreSecret(ctx context.Context, t *testing.T, consented *corev1.Secret) {
	t.Helper()
	s := consented.DeepCopy()
	s.ResourceVersion, s.UID = "", ""
	createSecret(ctx, t, s)
}

// #311: the disable PATCH commits in Coder but its answer is lost, while a
// generation is pending, and consent comes back before any other reconcile.
// The controller wrote appliedGeneration before the PATCH, so the next sync
// finds DisabledExternally and does not apply the pending generation. Only a
// spec edit after the disable enables the config.
func TestCoderMCPServerLostDisableAnswerHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	got, secret := env.pendingMCPServer(ctx, t)
	consented := secret.DeepCopy()
	require.NoError(t, k8sClient.Delete(ctx, secret))

	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, false, credentialsecret.ReasonSecretNotFound), "the disable failed")
	require.False(t, env.stored(t, got).view.Enabled, "the PATCH committed")
	require.Equal(t, got.Generation, got.Status.AppliedGeneration, "written before the PATCH")

	restoreSecret(ctx, t, consented)
	mark := len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "DisabledExternally")
	require.Empty(t, env.writesSince(mark))
	stored := env.stored(t, got)
	require.False(t, stored.view.Enabled)
	require.Empty(t, stored.view.Description, "the pending generation is not applied")

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.Description = "after the disable"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.True(t, env.stored(t, got).view.Enabled)
}

// #311: when the status write before the disable PATCH fails, the
// controller sends no PATCH, and the reconcile returns the error.
func TestCoderMCPServerDisableNeedsTheMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	got, secret := env.pendingMCPServer(ctx, t)
	require.NoError(t, k8sClient.Delete(ctx, secret))

	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	r := &controller.CoderMCPServerReconciler{Client: interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("lost status write")
		},
	}), Scheme: scheme}
	mark := len(env.writeRoutes())
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
	require.ErrorContains(t, err, "lost status write")
	require.Empty(t, env.writesSince(mark), "no PATCH without the marker")
	require.True(t, env.stored(t, got).view.Enabled)
}

// #311: the disable answer and the status write after it are both lost, so
// the next disable reconcile finds the config disabled. It records the
// generation as applied, so consent that comes back later does not apply
// the pending generation either.
func TestCoderMCPServerFoundDisabledHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	got, secret := env.pendingMCPServer(ctx, t)
	consented := secret.DeepCopy()
	require.NoError(t, k8sClient.Delete(ctx, secret))
	// Coder holds the disable, but status does not know it.
	_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, env.stored(t, got).view.ID, codersdk.UpdateMCPServerConfigRequest{Enabled: ptrTo(false)})
	require.NoError(t, err)

	mark := len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, credentialsecret.ReasonSecretNotFound), "is disabled in Coder")
	require.Equal(t, got.Generation, got.Status.AppliedGeneration)
	require.Empty(t, env.writesSince(mark))

	restoreSecret(ctx, t, consented)
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "DisabledExternally")
	require.Empty(t, env.writesSince(mark))
	require.Empty(t, env.stored(t, got).view.Description)
}

// #334: the consent check requires https for the revocation URL that Coder
// holds, as the spec does. Coder accepts an http loopback revocation URL,
// so a Secret that lists that origin still does not prove consent for it.
func TestCoderMCPServerConsentCheckNeedsHTTPSRevocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	const loopback = "http://127.0.0.1:8080"
	env.secret(ctx, t, "tracker-oauth", map[string]string{"client-secret": testMCPClientSecret}, testMCPOrigin, testMCPAuthOrigin, loopback)
	got, id := env.applied(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) { useOAuth2(s, nil) })
	_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, id, codersdk.UpdateMCPServerConfigRequest{OAuth2RevocationURL: ptrTo(loopback + "/revoke")})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.URL = "https://other.example.com/mcp"
	require.NoError(t, k8sClient.Update(ctx, got))

	mark := len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, credentialsecret.ReasonDestinationNotAllowed), "The controller disabled MCP server config")
	writes := env.writesSince(mark)
	require.Len(t, writes, 1)
	require.JSONEq(t, `{"enabled":false}`, string(writes[0].body))
}
