package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// dashboard is a Coder client for the edits that an admin makes in the
// dashboard. The fake records its writes like the controller's, so take a
// write mark after the edit.
func (env mcpServerEnv) dashboard(t *testing.T) *codersdk.Client {
	t.Helper()
	return env.fake.client(t, 2*time.Second)
}

// applied creates the server of validMCPServerSpec with mutate applied and
// applies it.
func (env mcpServerEnv) applied(ctx context.Context, t *testing.T, mutate func(*coderv1alpha1.CoderMCPServerSpec)) (*coderv1alpha1.CoderMCPServer, uuid.UUID) {
	t.Helper()
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, mutate))
	requireMCPServerApplied(t, got)
	return got, env.requireCreated(t, got).view.ID
}

// writesSince returns the recorded writes after the first mark writes.
func (env mcpServerEnv) writesSince(mark int) []fakeMCPBody {
	env.fake.mu.Lock()
	defer env.fake.mu.Unlock()
	return slices.Clone(env.fake.mcp.bodies[mark:])
}

// stored returns what Coder holds for the server, without updated_at, which
// every PATCH moves (plan F11).
func (env mcpServerEnv) stored(t *testing.T, server *coderv1alpha1.CoderMCPServer) fakeMCPServer {
	t.Helper()
	s := env.requireCreated(t, server)
	s.view.UpdatedAt = time.Time{}
	return s
}

func headerMCPSecret(ctx context.Context, t *testing.T, env mcpServerEnv) {
	env.secret(ctx, t, "tracker-mcp", map[string]string{"x-api-key": testMCPKey}, testMCPOrigin)
}

func oauth2MCPSecret(ctx context.Context, t *testing.T, env mcpServerEnv) {
	env.secret(ctx, t, "tracker-oauth", map[string]string{"client-secret": testMCPClientSecret}, testMCPOrigin, testMCPAuthOrigin)
}

// Plan 4.7 item 5 and S13: the controller reads the config on every
// reconcile, so a dashboard edit is set back by one sparse PATCH without a
// spec change. A PATCH that sets url carries the credential (plan 4.6 item
// 5). A credential of another auth type is cleared (#314). Scopes compare as
// a set, so a reordered list gets no write.
func TestCoderMCPServerSyncDrift(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		spec    func(*coderv1alpha1.CoderMCPServerSpec)
		secrets func(context.Context, *testing.T, mcpServerEnv)
		edit    codersdk.UpdateMCPServerConfigRequest
		want    string // The body of the controller's PATCH, or "" for no write.
	}{
		{name: "description", edit: codersdk.UpdateMCPServerConfigRequest{Description: ptrTo("edited")}, want: `{"description":""}`},
		{
			name: "availability and a flag", edit: codersdk.UpdateMCPServerConfigRequest{Availability: ptrTo("default_on"), ForwardCoderHeaders: ptrTo(true)},
			want: `{"availability":"default_off","forward_coder_headers":false}`,
		},
		{name: "url", edit: codersdk.UpdateMCPServerConfigRequest{URL: ptrTo("https://evil.example.net/mcp")}, want: `{"url":"https://mcp.example.com/github/mcp","api_key_value":"` + testMCPKey + `"}`},
		{name: "api key header", edit: codersdk.UpdateMCPServerConfigRequest{APIKeyHeader: ptrTo("X-Other")}, want: `{"api_key_header":"Authorization"}`},
		{name: "credential of another auth type", edit: codersdk.UpdateMCPServerConfigRequest{CustomHeaders: &map[string]string{"X-Extra": "v"}}, want: `{"custom_headers":{}}`},
		{
			name: "api key on a none config", spec: func(s *coderv1alpha1.CoderMCPServerSpec) { s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "none"} },
			edit: codersdk.UpdateMCPServerConfigRequest{APIKeyValue: ptrTo("dashboard-key")}, want: `{"api_key_value":""}`,
		},
		{
			name: "scopes reordered", secrets: oauth2MCPSecret,
			spec: func(s *coderv1alpha1.CoderMCPServerSpec) {
				useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.Scopes = []string{"read", "write"} })
			},
			edit: codersdk.UpdateMCPServerConfigRequest{OAuth2Scopes: ptrTo("write  read")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			if tc.secrets == nil {
				tc.secrets = func(ctx context.Context, t *testing.T, env mcpServerEnv) { env.keySecret(ctx, t, testMCPKey) }
			}
			tc.secrets(ctx, t, env)
			got, id := env.applied(ctx, t, tc.spec)
			before := env.stored(t, got)

			_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, id, tc.edit)
			require.NoError(t, err)
			mark := len(env.writeRoutes())
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerApplied(t, got)
			writes := env.writesSince(mark)
			if tc.want == "" {
				require.Empty(t, writes)
				return
			}
			require.Len(t, writes, 1)
			require.Equal(t, routeUpdateMCPServer, writes[0].route)
			require.JSONEq(t, tc.want, string(writes[0].body))
			require.Equal(t, before, env.stored(t, got), "Coder holds the applied state again")

			mark = len(env.writeRoutes())
			env.reconcile(ctx, t, got)
			require.Empty(t, env.writesSince(mark))
		})
	}
}

// The spec.access field doc says that every sync removes user grants: a
// grant added in the dashboard goes on the next reconcile, without a spec
// change, and the config itself gets no PATCH.
func TestCoderMCPServerSyncRemovesUserGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	data := env.fake.addGroup(env.orgID, "data", "")
	alice := env.fake.addUser("alice", codersdk.LoginTypePassword)
	env.fake.addMember(env.orgID, alice)
	got, id := env.applied(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
		s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{"data"}}
	})

	require.NoError(t, env.dashboard(t).UpdateMCPServerConfigACL(ctx, env.orgID, id, codersdk.UpdateMCPServerConfigACLRequest{
		UserRoles: map[string]codersdk.MCPServerConfigRole{alice.String(): codersdk.MCPServerConfigRoleRead},
	}))
	mark := len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	groups, users := env.fake.mcpACL(id)
	require.Equal(t, []uuid.UUID{data}, groups)
	require.Empty(t, users)
	writes := env.writesSince(mark)
	require.Len(t, writes, 1)
	require.Equal(t, routeUpdateMCPServerACL, writes[0].route)
}

// Plan 4.7 items 2 and 3: a dashboard delete after the apply is a kill
// switch with no write. After a spec edit, the controller creates the config
// again, because the slug is free.
func TestCoderMCPServerSyncDeletedConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, gone := env.applied(ctx, t, nil)
	require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, gone))
	mark := len(env.writeRoutes())
	got, result := env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "DeletedExternally")
	require.Equal(t, 10*time.Minute, result.RequeueAfter, "no Kubernetes event follows a dashboard delete")
	require.Empty(t, env.writesSince(mark))

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.Description = "re-armed"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.Equal(t, 2, env.posts())
	require.NotEqual(t, gone.String(), got.Status.ConfigID)
	stored := env.stored(t, got)
	require.True(t, stored.view.Enabled)
	require.Equal(t, testMCPKey, stored.apiKeyValue)
}

// Plan S14: after a full apply, a dashboard disable, credential clear or
// auth type change is a kill switch, like the delete of
// TestCoderMCPServerSyncDeletedConfig. It gives Stalled=True and no write
// until the spec changes. A spec edit re-arms the object, and an apply that
// fails keeps the old applied generation.
func TestCoderMCPServerSyncKillSwitches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		edit         codersdk.UpdateMCPServerConfigRequest
	}{
		{name: "disable", reason: "DisabledExternally", edit: codersdk.UpdateMCPServerConfigRequest{Enabled: ptrTo(false)}},
		{name: "credential clear", reason: "CredentialClearedExternally", edit: codersdk.UpdateMCPServerConfigRequest{APIKeyValue: ptrTo("")}},
		{name: "auth type change", reason: "CredentialClearedExternally", edit: codersdk.UpdateMCPServerConfigRequest{AuthType: ptrTo("none")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, id := env.applied(ctx, t, nil)
			_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, id, tc.edit)
			require.NoError(t, err)
			mark := len(env.writeRoutes())
			for range 2 {
				var result ctrl.Result
				got, result = env.reconcile(ctx, t, got)
				requireMCPServerStep(t, got, true, tc.reason)
				require.Equal(t, 10*time.Minute, result.RequeueAfter, "no Kubernetes event follows a dashboard edit")
			}
			require.Empty(t, env.writesSince(mark), "a kill switch holds every write")

			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
			got.Spec.Description = "re-armed"
			require.NoError(t, k8sClient.Update(ctx, got))
			env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerStep(t, got, false, "CoderUnavailable")
			require.Equal(t, got.Generation-1, got.Status.AppliedGeneration)

			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerApplied(t, got)
			stored := env.stored(t, got)
			require.True(t, stored.view.Enabled)
			require.Equal(t, "api_key", stored.view.AuthType)
			require.Equal(t, testMCPKey, stored.apiKeyValue)
			require.Equal(t, "re-armed", stored.view.Description)
		})
	}
}

// Plan 4.7 item 4 and S19: a dashboard switch to user_oidc is always set
// back, also when it cleared the credentials. The one PATCH carries only
// auth fields, so a config that is also disabled stays disabled.
func TestCoderMCPServerSyncUserOIDC(t *testing.T) {
	t.Parallel()
	authFields := []string{
		"auth_type", "api_key_header", "api_key_value", "custom_headers", "oauth2_client_id", "oauth2_client_secret",
		"oauth2_auth_url", "oauth2_token_url", "oauth2_revocation_url", "oauth2_scopes",
	}
	for _, tc := range []struct {
		name     string
		spec     func(*coderv1alpha1.CoderMCPServerSpec)
		secrets  func(context.Context, *testing.T, mcpServerEnv)
		disabled bool
	}{
		{name: "none", spec: func(s *coderv1alpha1.CoderMCPServerSpec) { s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "none"} }},
		{name: "api key"},
		{name: "api key, also disabled", disabled: true},
		{name: "custom headers", secrets: headerMCPSecret, spec: func(s *coderv1alpha1.CoderMCPServerSpec) { useCustomHeaders(s, "X-Api-Key") }},
		{name: "oauth2", secrets: oauth2MCPSecret, spec: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.Scopes = []string{"read"} })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			if tc.secrets == nil {
				tc.secrets = func(ctx context.Context, t *testing.T, env mcpServerEnv) { env.keySecret(ctx, t, testMCPKey) }
			}
			tc.secrets(ctx, t, env)
			got, id := env.applied(ctx, t, tc.spec)
			want := env.stored(t, got)

			edit := codersdk.UpdateMCPServerConfigRequest{AuthType: ptrTo("user_oidc")}
			if tc.disabled {
				edit.Enabled, want.view.Enabled = ptrTo(false), false
			}
			_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, id, edit)
			require.NoError(t, err)
			mark := len(env.writeRoutes())
			got, _ = env.reconcile(ctx, t, got)
			writes := env.writesSince(mark)
			require.Len(t, writes, 1)
			require.Equal(t, routeUpdateMCPServer, writes[0].route)
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(writes[0].body, &body))
			for field := range body {
				require.Contains(t, authFields, field, "the PATCH carries only auth fields")
			}
			require.JSONEq(t, `"`+got.Spec.Auth.Type+`"`, string(body["auth_type"]))
			require.Equal(t, want, env.stored(t, got))
			if tc.disabled {
				require.Contains(t, requireMCPServerStep(t, got, true, "DisabledExternally"), "set the auth type back from user_oidc")
			} else {
				requireMCPServerApplied(t, got)
			}

			mark = len(env.writeRoutes())
			env.reconcile(ctx, t, got)
			require.Empty(t, env.writesSince(mark))
		})
	}
}

// Plan 4.6 item 4 and S13: a metadata-only Secret change moves the change
// token. Status records the new token and nothing is sent. A restart empties
// the in-memory hashes, so the next such change resends the value once.
func TestCoderMCPServerSyncSecretMetadataChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	secret := env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	touch := func(note string) {
		t.Helper()
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
		secret.Annotations["example.com/note"] = note
		require.NoError(t, k8sClient.Update(ctx, secret))
	}

	touch("one")
	mark := len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.Empty(t, env.writesSince(mark))
	require.Equal(t, secret.ResourceVersion, got.Status.Credentials[0].SecretResourceVersion)

	env.r = &controller.CoderMCPServerReconciler{Client: k8sClient, Scheme: scheme}
	touch("two")
	mark = len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	writes := env.writesSince(mark)
	require.Len(t, writes, 1)
	require.JSONEq(t, `{"api_key_value":"`+testMCPKey+`"}`, string(writes[0].body))
	require.Equal(t, secret.ResourceVersion, got.Status.Credentials[0].SecretResourceVersion)
}
