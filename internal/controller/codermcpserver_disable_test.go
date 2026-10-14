package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// Plan G8 and S12 (#311): when a credential Secret withdraws consent, the
// controller disables the config in Coder with one PATCH that holds no
// credential and no destination field. The disable is a kill switch: the
// restored consent does not undo it, and a spec edit enables the config.
func TestCoderMCPServerDisablesWithoutConsent(t *testing.T) {
	t.Parallel()
	oauth2 := func(s *coderv1alpha1.CoderMCPServerSpec) { useOAuth2(s, nil) }
	for _, tc := range []struct {
		name   string
		spec   func(*coderv1alpha1.CoderMCPServerSpec)
		secret func(context.Context, *testing.T, mcpServerEnv) *corev1.Secret
		edit   func(*corev1.Secret) // nil deletes the Secret.
		reason string
	}{
		{name: "Secret deleted", reason: credentialsecret.ReasonSecretNotFound},
		{name: "label removed", reason: credentialsecret.ReasonNotOptedIn, edit: func(s *corev1.Secret) { delete(s.Labels, credentialsecret.LabelKey) }},
		{name: "origin removed", reason: credentialsecret.ReasonDestinationNotAllowed, edit: func(s *corev1.Secret) {
			s.Annotations[credentialsecret.DestinationsAnnotation] = "https://other.example.com"
		}},
		{
			name: "OAuth2 token origin removed", reason: credentialsecret.ReasonDestinationNotAllowed, spec: oauth2,
			secret: func(ctx context.Context, t *testing.T, env mcpServerEnv) *corev1.Secret {
				return env.secret(ctx, t, "tracker-oauth", map[string]string{"client-secret": testMCPClientSecret}, testMCPOrigin, testMCPAuthOrigin)
			},
			edit: func(s *corev1.Secret) { s.Annotations[credentialsecret.DestinationsAnnotation] = testMCPOrigin },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			if tc.secret == nil {
				tc.secret = func(ctx context.Context, t *testing.T, env mcpServerEnv) *corev1.Secret {
					return env.keySecret(ctx, t, testMCPKey)
				}
			}
			secret := tc.secret(ctx, t, env)
			consented := secret.DeepCopy()
			got, _ := env.applied(ctx, t, tc.spec)
			before := env.stored(t, got)
			if tc.edit == nil {
				require.NoError(t, k8sClient.Delete(ctx, secret))
			} else {
				editSecret(ctx, t, secret, tc.edit)
			}

			// A failed disable is retried, and the message says that Coder
			// can still send the credentials.
			env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
			got, result := env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, false, tc.reason), "Coder can still send the credentials")
			require.Equal(t, 30*time.Second, result.RequeueAfter)

			mark := len(env.writeRoutes())
			got, result = env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, true, tc.reason), "The controller disabled MCP server config")
			require.Equal(t, 10*time.Minute, result.RequeueAfter)
			writes := env.writesSince(mark)
			require.Len(t, writes, 1)
			require.Equal(t, routeUpdateMCPServer, writes[0].route)
			require.JSONEq(t, `{"enabled":false}`, string(writes[0].body))
			before.view.Enabled = false
			require.Equal(t, before, env.stored(t, got), "only enabled changed")

			mark = len(env.writeRoutes())
			got, _ = env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, true, tc.reason), "is disabled in Coder")
			require.Empty(t, env.writesSince(mark))

			// Restored consent does not enable the config again.
			if tc.edit == nil {
				consented.ResourceVersion, consented.UID = "", ""
				createSecret(ctx, t, consented)
			} else {
				editSecret(ctx, t, secret, func(s *corev1.Secret) { s.Labels, s.Annotations = consented.Labels, consented.Annotations })
			}
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerStep(t, got, true, "DisabledExternally")
			require.Empty(t, env.writesSince(mark))

			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
			got.Spec.Description = "re-armed"
			require.NoError(t, k8sClient.Update(ctx, got))
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerApplied(t, got)
			require.True(t, env.stored(t, got).view.Enabled)
		})
	}
}

// Plan S12: a spec change to an origin that the Secret does not list sends
// nothing. The consent check uses what Coder holds: Coder keeps the old URL,
// which the recorded Secret still allows, so the config stays enabled. A
// credential that Coder holds without a recorded Secret, or a recorded
// Secret replaced by another one with the same name, has no proof of consent,
// so the config is disabled.
func TestCoderMCPServerConsentCheckUsesCoderState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		prepare  func(context.Context, *testing.T, mcpServerEnv, *corev1.Secret, uuid.UUID)
		disabled bool
	}{
		{name: "Coder keeps an allowed URL"},
		{name: "Coder holds an unrecorded credential", disabled: true, prepare: func(ctx context.Context, t *testing.T, env mcpServerEnv, _ *corev1.Secret, id uuid.UUID) {
			_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, id, codersdk.UpdateMCPServerConfigRequest{CustomHeaders: &map[string]string{"X-Extra": "v"}})
			require.NoError(t, err)
		}},
		{name: "Secret replaced", disabled: true, prepare: func(ctx context.Context, t *testing.T, env mcpServerEnv, secret *corev1.Secret, _ uuid.UUID) {
			require.NoError(t, k8sClient.Delete(ctx, secret))
			env.keySecret(ctx, t, testMCPKey)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			secret := env.keySecret(ctx, t, testMCPKey)
			got, id := env.applied(ctx, t, nil)
			if tc.prepare != nil {
				tc.prepare(ctx, t, env, secret, id)
			}
			before := env.stored(t, got)

			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
			got.Spec.URL = "https://other.example.com/mcp"
			require.NoError(t, k8sClient.Update(ctx, got))
			mark := len(env.writeRoutes())
			got, _ = env.reconcile(ctx, t, got)
			message := requireMCPServerStep(t, got, true, credentialsecret.ReasonDestinationNotAllowed)
			if tc.disabled {
				require.Contains(t, message, "The controller disabled MCP server config")
				require.Len(t, env.writesSince(mark), 1)
				before.view.Enabled = false
			} else {
				require.Contains(t, message, "only to origins that its recorded Secret allows")
				require.Empty(t, env.writesSince(mark))
			}
			require.Equal(t, before, env.stored(t, got))
		})
	}
}
