package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

func (env mcpServerEnv) posts() int { return env.fake.requestCount(routeCreateMCPServer) }

// requireCreated checks that Coder holds exactly one config with the spec's
// slug, that status records it, that no create marker is left, and that every
// create request carried enabled false and the nonce. It returns the config.
func (env mcpServerEnv) requireCreated(t *testing.T, server *coderv1alpha1.CoderMCPServer) fakeMCPServer {
	t.Helper()
	var own []fakeMCPServer
	for _, s := range env.fake.mcpServers() {
		if s.view.Slug == server.Spec.Slug {
			own = append(own, s)
		}
	}
	require.Len(t, own, 1, "exactly one config with the slug")
	require.Equal(t, own[0].view.ID.String(), server.Status.ConfigID)
	require.Nil(t, server.Status.CreateAttemptTime)
	for _, body := range env.fake.mcpRequestBodies(routeCreateMCPServer) {
		var req codersdk.CreateMCPServerConfigRequest
		require.NoError(t, json.Unmarshal(body, &req))
		if req.Slug == server.Spec.Slug {
			require.False(t, req.Enabled, "created disabled")
			require.Equal(t, "coder-k8s:"+string(server.UID), req.DisplayName)
		}
	}
	return own[0]
}

// requireMCPServerCreatedStep checks the reason after a create in this
// version, which does not apply the spec yet.
func requireMCPServerCreatedStep(t *testing.T, server *coderv1alpha1.CoderMCPServer) {
	t.Helper()
	require.Contains(t, requireMCPServerStep(t, server, false, "Pending"), "exists in Coder, disabled.")
}

// Plan 4.5 create steps 1 to 3 for every auth type: one create, disabled with
// the nonce, every spec field and the credentials, and status records the ID
// and the change tokens. The next reconcile sends nothing.
func TestCoderMCPServerCreate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		spec    func(*coderv1alpha1.CoderMCPServerSpec)
		secrets func(context.Context, *testing.T, mcpServerEnv)
		check   func(*testing.T, fakeMCPServer)
		fields  []string
	}{
		{
			name: "api key", secrets: func(ctx context.Context, t *testing.T, env mcpServerEnv) { env.keySecret(ctx, t, testMCPKey) },
			fields: []string{"apiKey"}, check: func(t *testing.T, s fakeMCPServer) {
				require.Equal(t, "Authorization", s.view.APIKeyHeader)
				require.Equal(t, testMCPKey, s.apiKeyValue)
			},
		},
		{name: "slug taken in another case", secrets: func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.keySecret(ctx, t, testMCPKey)
			_, err := env.fake.client(t, 2*time.Second).CreateMCPServerConfig(ctx, env.orgID, apiKeyMCPRequest("GITHUB")) // Slugs are case-sensitive (plan F3).
			require.NoError(t, err)
		}, fields: []string{"apiKey"}, check: func(t *testing.T, s fakeMCPServer) { require.Equal(t, testMCPKey, s.apiKeyValue) }},
		{name: "custom headers", spec: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useCustomHeaders(s, "X-Api-Key", "X-Tenant")
			s.Auth.CustomHeaders[1].SecretRef.Name = "tenant-mcp"
		}, secrets: func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.secret(ctx, t, "tracker-mcp", map[string]string{"x-api-key": testMCPKey}, testMCPOrigin)
			env.secret(ctx, t, "tenant-mcp", map[string]string{"x-tenant": testMCPTenant}, testMCPOrigin)
		}, fields: []string{"header:X-Api-Key", "header:X-Tenant"}, check: func(t *testing.T, s fakeMCPServer) {
			require.Equal(t, map[string]string{"X-Api-Key": testMCPKey, "X-Tenant": testMCPTenant}, s.customHeaders)
		}},
		{name: "oauth2", spec: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) {
				o.RevocationURL, o.Scopes = "https://auth.example.com/oauth/revoke", []string{"read", "write"}
			})
		}, secrets: func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.secret(ctx, t, "tracker-oauth", map[string]string{"client-secret": testMCPClientSecret}, testMCPOrigin, testMCPAuthOrigin)
		}, fields: []string{"oauth2ClientSecret"}, check: func(t *testing.T, s fakeMCPServer) {
			require.Equal(t, testMCPClientSecret, s.oauth2ClientSecret)
			require.Equal(t, "coder-agents", s.view.OAuth2ClientID)
			require.Equal(t, "https://auth.example.com/oauth/authorize", s.view.OAuth2AuthURL)
			require.Equal(t, "https://auth.example.com/oauth/token", s.view.OAuth2TokenURL)
			require.Equal(t, "https://auth.example.com/oauth/revoke", s.view.OAuth2RevocationURL)
			require.Equal(t, "read write", s.view.OAuth2Scopes)
		}},
		{name: "no auth", spec: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "none"}
			s.Description, s.IconURL, s.Availability, s.Transport = "Issues", "/icon/github.svg", "default_on", "sse"
			s.ModelIntent, s.AllowInPlanMode, s.ForwardCoderHeaders = true, true, true
		}, check: func(t *testing.T, s fakeMCPServer) {
			require.Equal(t, "Issues", s.view.Description)
			require.Equal(t, "/icon/github.svg", s.view.IconURL)
			require.Equal(t, "sse", s.view.Transport)
			require.Equal(t, "default_on", s.view.Availability)
			require.True(t, s.view.ModelIntent && s.view.AllowInPlanMode && s.view.ForwardCoderHeaders)
			require.False(t, s.view.HasAPIKey || s.view.HasCustomHeaders || s.view.HasOAuth2Secret)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			if tc.secrets != nil {
				tc.secrets(ctx, t, env)
			}
			base := env.posts() // Creates of the test setup.
			got, result := env.reconcile(ctx, t, env.create(ctx, t, tc.spec))
			stored := env.requireCreated(t, got)
			requireMCPServerCreatedStep(t, got)
			require.Equal(t, 1, env.posts()-base)
			require.Equal(t, 10*time.Minute, result.RequeueAfter)
			require.Equal(t, "github", stored.view.Slug)
			require.Equal(t, "https://mcp.example.com/github/mcp", stored.view.URL)
			require.Equal(t, got.Spec.Auth.Type, stored.view.AuthType)
			tc.check(t, stored)

			require.Len(t, got.Status.Credentials, len(tc.fields))
			for i, field := range tc.fields {
				c := got.Status.Credentials[i]
				require.Equal(t, field, c.Field)
				s := &corev1.Secret{}
				require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: c.SecretName}, s))
				require.Equal(t, string(s.UID), c.SecretUID)
				require.Equal(t, s.ResourceVersion, c.SecretResourceVersion)
			}

			before := env.mcpRequests()
			again, _ := env.reconcile(ctx, t, got)
			require.Equal(t, before, env.mcpRequests(), "a recorded config needs no MCP request in this version")
			require.Equal(t, got.ResourceVersion, again.ResourceVersion)
		})
	}
}

// Each create answer class of plan 4.5 step 4, and the reconcile after it.
// Every case ends with exactly one config of this object, and a create is
// sent again only when a read proved that the first one did not commit.
func TestCoderMCPServerCreateAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fault   fakeFault
		reason  string
		requeue time.Duration
		marker  bool // The first reconcile keeps the create marker.
		posts   int  // Creates sent until the config is recorded.
	}{
		{name: "reset before commit", fault: fakeFault{Reset: true}, reason: "ConfirmingCreate", requeue: 5 * time.Second, marker: true, posts: 2},
		{name: "reset after commit", fault: fakeFault{Reset: true, AfterCommit: true}, reason: "ConfirmingCreate", requeue: 5 * time.Second, marker: true, posts: 1},
		{name: "504 after commit", fault: fakeFault{Status: http.StatusGatewayTimeout, AfterCommit: true}, reason: "ConfirmingCreate", requeue: 5 * time.Second, marker: true, posts: 1},
		{name: "proxy page after commit", fault: fakeFault{Status: http.StatusOK, HTML: "<html>sign in</html>", AfterCommit: true}, reason: "ConfirmingCreate", requeue: 5 * time.Second, marker: true, posts: 1},
		{name: "answer for another slug", fault: fakeFault{Rewrite: func(answer any) any {
			c := answer.(codersdk.MCPServerConfig)
			c.Slug = "other"
			return c
		}}, reason: "ConfirmingCreate", requeue: 5 * time.Second, marker: true, posts: 1},
		{name: "429", fault: fakeFault{Status: http.StatusTooManyRequests}, reason: "CreateRetrying", requeue: 30 * time.Second, posts: 2},
		{name: "401", fault: fakeFault{Status: http.StatusUnauthorized}, reason: "CreateRetrying", requeue: 30 * time.Second, posts: 2},
		{name: "400 after commit", fault: fakeFault{Status: http.StatusBadRequest, AfterCommit: true}, reason: "Pending", requeue: 10 * time.Minute, posts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			server := env.create(ctx, t, nil)
			env.fake.failNext(routeCreateMCPServer, tc.fault)
			got, result := env.reconcile(ctx, t, server)
			requireMCPServerStep(t, got, false, tc.reason)
			require.Equal(t, tc.requeue, result.RequeueAfter)
			require.Equal(t, 1, env.posts())
			require.Equal(t, tc.marker, got.Status.CreateAttemptTime != nil, "create marker")
			if tc.reason != "Pending" {
				require.Empty(t, got.Status.ConfigID)
				got, _ = env.reconcile(ctx, t, got)
			}
			env.requireCreated(t, got)
			requireMCPServerCreatedStep(t, got)
			require.Equal(t, tc.posts, env.posts())
		})
	}
}

// A 400 that a read confirms clears the marker and stalls. The same spec is
// never sent again, also when the confirming read failed first. A spec change
// creates the config.
func TestCoderMCPServerCreateRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	env.fake.failNext(routeCreateMCPServer, fakeFault{Status: http.StatusBadRequest, Detail: "rejected " + testMCPKey})
	env.fake.failNext(routeMCPServers, fakeFault{Rewrite: func(answer any) any { return answer }}) // The lookup before the create.
	env.fake.failNext(routeMCPServers, fakeFault{Status: http.StatusInternalServerError})          // The confirming read.
	got, result := env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, false, "ConfirmingRejectedCreate")
	require.Equal(t, 30*time.Second, result.RequeueAfter)
	require.NotNil(t, got.Status.CreateAttemptTime)

	got, result = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, "CoderRejected"), "a read confirmed that nothing was created")
	require.Zero(t, result.RequeueAfter)
	require.Nil(t, got.Status.CreateAttemptTime)
	require.Equal(t, 1, env.posts())

	before := env.mcpRequests()
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "CoderRejected")
	require.Equal(t, before, env.mcpRequests(), "a rejected spec sends nothing")

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.DisplayName = "GitHub tools"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	env.requireCreated(t, got)
	requireMCPServerCreatedStep(t, got)
	require.Equal(t, 2, env.posts())
}

// Plan S4: a config that another client created between the lookup and the
// create answers 409. The confirming read finds it, and the controller never
// changes it.
func TestCoderMCPServerCreateSlugRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	_, err := env.fake.client(t, 2*time.Second).CreateMCPServerConfig(ctx, env.orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	env.fake.failNext(routeMCPServers, fakeFault{Rewrite: func(any) any { return []codersdk.MCPServerConfig{} }})
	got, _ := env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, true, "SlugConflict")
	require.Nil(t, got.Status.CreateAttemptTime)
	require.Empty(t, got.Status.ConfigID)
	require.Equal(t, 2, env.posts(), "the setup create and the controller's create")
	require.Len(t, env.fake.mcpServers(), 1)
	require.Zero(t, env.fake.requestCount(routeUpdateMCPServer)+env.fake.requestCount(routeDeleteMCPServer))
}

// A marker left by an earlier reconcile: the controller reads Coder before it
// sends anything. A reconcile after a restart uses a new reconciler.
func TestCoderMCPServerCreateRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		existing bool // Coder holds a config with this object's nonce.
		notOwner bool // The operator user is not a site owner.
		reason   string
		posts    int
	}{
		{name: "create committed", existing: true, reason: "Pending"},
		{name: "nothing committed", reason: "Pending", posts: 1},
		{name: "nothing visible to a non-owner", notOwner: true, reason: "ConfirmingCreate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			server := env.create(ctx, t, nil)
			server.Status.CreateAttemptTime = &metav1.Time{Time: time.Now()}
			require.NoError(t, k8sClient.Status().Update(ctx, server))
			if tc.existing {
				req := apiKeyMCPRequest("github")
				req.DisplayName = "coder-k8s:" + string(server.UID)
				_, err := env.fake.client(t, 2*time.Second).CreateMCPServerConfig(ctx, env.orgID, req)
				require.NoError(t, err)
			}
			if tc.notOwner {
				// An organization role named owner is not the site owner role.
				env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) {
					u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner, OrganizationID: env.orgID.String()}, {Name: codersdk.RoleMember}}
				})
			}
			env.r = &controller.CoderMCPServerReconciler{Client: k8sClient, Scheme: scheme}
			base := env.posts() // Creates of the test setup.
			got, _ := env.reconcile(ctx, t, server)
			if tc.reason == "Pending" {
				env.requireCreated(t, got)
				requireMCPServerCreatedStep(t, got)
			} else {
				requireMCPServerStep(t, got, false, tc.reason)
				require.NotNil(t, got.Status.CreateAttemptTime)
			}
			require.Equal(t, tc.posts, env.posts()-base)
		})
	}
}

// markerConflictMCPClient fails the status write that sets the create marker,
// like a write from a stale object after a leader handover.
type markerConflictMCPClient struct{ client.Client }

func (c markerConflictMCPClient) Status() client.SubResourceWriter {
	return markerConflictMCPWriter{c.Client.Status()}
}

type markerConflictMCPWriter struct{ client.SubResourceWriter }

func (w markerConflictMCPWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if s, ok := obj.(*coderv1alpha1.CoderMCPServer); ok && s.Status.CreateAttemptTime != nil {
		return apierrors.NewConflict(schema.GroupResource{Group: "coder.com", Resource: "codermcpservers"}, s.Name, errors.New("stale object"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// A conflict on the marker write ends the reconcile before the create.
func TestCoderMCPServerMarkerConflictSendsNoCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	r := &controller.CoderMCPServerReconciler{Client: markerConflictMCPClient{k8sClient}, Scheme: scheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)})
	require.True(t, apierrors.IsConflict(err), "%v", err)
	require.Zero(t, env.posts())
}
