package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

// writeRoutes returns the routes of the fake's recorded write requests, in
// request order.
func (env mcpServerEnv) writeRoutes() []string {
	env.fake.mu.Lock()
	defer env.fake.mu.Unlock()
	var out []string
	for _, b := range env.fake.mcp.bodies {
		out = append(out, b.route)
	}
	return out
}

func (env mcpServerEnv) lastUpdate(t *testing.T) codersdk.UpdateMCPServerConfigRequest {
	t.Helper()
	bodies := env.fake.mcpRequestBodies(routeUpdateMCPServer)
	require.NotEmpty(t, bodies)
	var req codersdk.UpdateMCPServerConfigRequest
	require.NoError(t, json.Unmarshal(bodies[len(bodies)-1], &req))
	return req
}

// Plan S20 and 4.5 steps 5 and 6: the config is created disabled with
// Coder's default Everyone entry. The entries that spec.access does not list
// go first, then one PATCH enables the config with the spec values, and only
// then are the missing groups added. So the enabled config never has a group
// that the spec does not list. The PATCH sets url, so it carries the
// credential too.
func TestCoderMCPServerApplyOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	data := env.fake.addGroup(env.orgID, "data", "")
	got, result := env.reconcile(ctx, t, env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
		s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{"data"}}
	}))
	requireMCPServerApplied(t, got)
	require.Equal(t, 10*time.Minute, result.RequeueAfter)
	require.Equal(t, []string{routeCreateMCPServer, routeUpdateMCPServerACL, routeUpdateMCPServer, routeUpdateMCPServerACL}, env.writeRoutes(),
		"create, remove Everyone, enable, then add data")
	acls := env.fake.mcpRequestBodies(routeUpdateMCPServerACL)
	require.JSONEq(t, `{"group_roles":{"`+env.orgID.String()+`":""}}`, string(acls[0]), "the first ACL write only removes")
	require.JSONEq(t, `{"group_roles":{"`+data.String()+`":"read"}}`, string(acls[1]))
	stored := env.requireCreated(t, got)
	require.True(t, stored.view.Enabled)
	require.Equal(t, "GitHub", stored.view.DisplayName)
	require.Equal(t, testMCPKey, stored.apiKeyValue)
	groups, users := env.fake.mcpACL(stored.view.ID)
	require.Equal(t, []uuid.UUID{data}, groups)
	require.Empty(t, users)
	req := env.lastUpdate(t)
	require.Equal(t, testMCPKey, *req.APIKeyValue, "a PATCH that sets url carries the credential")
	require.Equal(t, "https://mcp.example.com/github/mcp", *req.URL)
	require.Len(t, got.Status.Credentials, 1)

	before := env.mcpRequests()
	again, _ := env.reconcile(ctx, t, got)
	require.Equal(t, before, env.mcpRequests(), "an applied generation sends nothing")
	require.Equal(t, got.ResourceVersion, again.ResourceVersion)
}

// A new generation that moves an enabled config to other groups and also
// disables or changes it: when the PATCH fails, the ACL holds only groups
// that both generations allow, so no newly listed group can use the old
// configuration.
func TestCoderMCPServerApplyNeverBroadensBeforeTheChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		edit  func(*coderv1alpha1.CoderMCPServerSpec)
		fault fakeFault
	}{
		{name: "disable rejected", edit: func(s *coderv1alpha1.CoderMCPServerSpec) { s.Enabled = ptrTo(false) }, fault: fakeFault{Status: http.StatusBadRequest}},
		{name: "URL change fails", edit: func(s *coderv1alpha1.CoderMCPServerSpec) { s.URL = "https://mcp.example.com/v2/mcp" }, fault: fakeFault{Status: http.StatusBadGateway}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			data := env.fake.addGroup(env.orgID, "data", "")
			env.fake.addGroup(env.orgID, "ops", "")
			got, _ := env.reconcile(ctx, t, env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
				s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{"Everyone", "data"}}
			}))
			requireMCPServerApplied(t, got)

			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
			got.Spec.Access.Groups = []string{"data", "ops"}
			tc.edit(&got.Spec)
			require.NoError(t, k8sClient.Update(ctx, got))
			env.fake.failNext(routeUpdateMCPServer, tc.fault)
			got, _ = env.reconcile(ctx, t, got)
			require.NotEqual(t, got.Generation, got.Status.AppliedGeneration)
			groups, _ := env.fake.mcpACL(env.requireCreated(t, got).view.ID)
			require.Equal(t, []uuid.UUID{data}, groups, "Everyone is gone, ops is not added yet")

			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerApplied(t, got)
		})
	}
}

// Plan S15: access with no groups and enabled false reach Coder as an empty
// ACL on a disabled config. Without access, the controller never touches the
// ACL, so Coder's default Everyone entry stays.
func TestCoderMCPServerApplyAccess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		access  *coderv1alpha1.CoderMCPServerAccess
		enabled bool
		acl     bool // The ACL routes are called.
	}{
		{name: "no groups, disabled", access: &coderv1alpha1.CoderMCPServerAccess{Groups: []string{}}, acl: true},
		{name: "access omitted", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, _ := env.reconcile(ctx, t, env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
				s.Access, s.Enabled = tc.access, ptrTo(tc.enabled)
			}))
			requireMCPServerApplied(t, got)
			stored := env.requireCreated(t, got)
			require.Equal(t, tc.enabled, stored.view.Enabled)
			groups, _ := env.fake.mcpACL(stored.view.ID)
			aclCalls := env.fake.requestCount(routeMCPServerACL) + env.fake.requestCount(routeUpdateMCPServerACL)
			if tc.acl {
				require.Empty(t, groups, "no Everyone entry")
				require.NotZero(t, aclCalls)
			} else {
				require.Equal(t, []uuid.UUID{env.orgID}, groups)
				require.Zero(t, aclCalls)
			}
		})
	}
}

// A group that does not resolve keeps the new config disabled with the nonce,
// and sends no PATCH. Once the group exists, the next reconcile applies.
func TestCoderMCPServerApplyGroupNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
		s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{"data"}}
	}))
	requireMCPServerStep(t, got, true, "GroupNotFound")
	stored := env.requireCreated(t, got)
	require.False(t, stored.view.Enabled)
	require.Equal(t, "coder-k8s:"+string(got.UID), stored.view.DisplayName)
	require.Zero(t, env.fake.requestCount(routeUpdateMCPServer)+env.fake.requestCount(routeUpdateMCPServerACL))
	require.Zero(t, got.Status.AppliedGeneration)

	env.fake.addGroup(env.orgID, "data", "")
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.True(t, env.requireCreated(t, got).view.Enabled)
}

// rewriteUpdate changes the update answer after Coder applied the update.
func rewriteUpdate(edit func(*codersdk.MCPServerConfig)) fakeFault {
	return fakeFault{AfterCommit: true, Rewrite: func(answer any) any {
		c := answer.(codersdk.MCPServerConfig)
		edit(&c)
		return c
	}}
}

// Each failure of the apply leaves the generation unapplied, sends no other
// write, and never puts a credential into status or logs.
func TestCoderMCPServerApplyFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		route   string
		fault   fakeFault
		reason  string
		stalled bool
		retry   bool // The next reconcile applies the spec.
	}{
		{name: "config gone", route: routeMCPServer, fault: fakeFault{Status: http.StatusNotFound}, reason: "DeletedExternally", stalled: true},
		{name: "read fails", route: routeMCPServer, fault: fakeFault{Status: http.StatusServiceUnavailable}, reason: "CoderUnavailable", retry: true},
		{name: "read answers another slug", route: routeMCPServer, reason: "CoderAnswerMismatch", stalled: true, fault: fakeFault{Rewrite: func(answer any) any {
			c := answer.(codersdk.MCPServerConfig)
			c.Slug = "other"
			return c
		}}},
		{name: "update fails", route: routeUpdateMCPServer, fault: fakeFault{Status: http.StatusInternalServerError, Detail: "upstream " + testMCPKey}, reason: "CoderUnavailable", retry: true},
		{name: "update rejected", route: routeUpdateMCPServer, fault: fakeFault{Status: http.StatusBadRequest, Detail: "rejected " + testMCPKey}, reason: "CoderRejected", stalled: true},
		{name: "update answers 404", route: routeUpdateMCPServer, fault: fakeFault{Status: http.StatusNotFound}, reason: "CoderUnavailable", retry: true},
		{name: "update answers another enabled", route: routeUpdateMCPServer, reason: "CoderAnswerMismatch", stalled: true, fault: rewriteUpdate(func(c *codersdk.MCPServerConfig) { c.Enabled = false })},
		{name: "update answers another description", route: routeUpdateMCPServer, reason: "CoderAnswerMismatch", stalled: true, fault: rewriteUpdate(func(c *codersdk.MCPServerConfig) { c.Description = "x" })},
		{name: "update answers another availability", route: routeUpdateMCPServer, reason: "CoderAnswerMismatch", stalled: true, fault: rewriteUpdate(func(c *codersdk.MCPServerConfig) { c.Availability = "force_on" })},
		{name: "update answers another flag", route: routeUpdateMCPServer, reason: "CoderAnswerMismatch", stalled: true, fault: rewriteUpdate(func(c *codersdk.MCPServerConfig) { c.ForwardCoderHeaders = true })},
		{name: "update answers another header", route: routeUpdateMCPServer, reason: "CoderAnswerMismatch", stalled: true, fault: rewriteUpdate(func(c *codersdk.MCPServerConfig) { c.APIKeyHeader = "X-Other" })},
		{name: "update answers no key", route: routeUpdateMCPServer, reason: "CoderAnswerMismatch", stalled: true, fault: rewriteUpdate(func(c *codersdk.MCPServerConfig) { c.HasAPIKey = false })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			env.fake.failNext(tc.route, tc.fault)
			got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
			requireMCPServerStep(t, got, tc.stalled, tc.reason)
			require.Zero(t, got.Status.AppliedGeneration)
			require.NotEmpty(t, got.Status.ConfigID, "the ID is stored before the apply")
			require.Equal(t, 1, env.posts())
			if tc.retry {
				got, _ = env.reconcile(ctx, t, got)
				requireMCPServerApplied(t, got)
				require.True(t, env.requireCreated(t, got).view.Enabled)
			}
		})
	}
}

// A spec change after the apply sends one PATCH with the new values. It sets
// url, so it carries the current credential (plan 4.6 item 5), and status
// records that credential's change token.
func TestCoderMCPServerApplySpecChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	secret := env.keySecret(ctx, t, testMCPKey)
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerApplied(t, got)

	rotated := testMCPKey + "-rotated"
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	secret.Data["token"] = []byte(rotated)
	require.NoError(t, k8sClient.Update(ctx, secret))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.URL, got.Spec.Description = "https://mcp.example.com/v2/mcp", "Issues"
	require.NoError(t, k8sClient.Update(ctx, got))
	before := env.fake.requestCount(routeUpdateMCPServer)
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.Equal(t, 1, env.fake.requestCount(routeUpdateMCPServer)-before)
	require.Equal(t, 1, env.posts())
	stored := env.requireCreated(t, got)
	require.Equal(t, "https://mcp.example.com/v2/mcp", stored.view.URL)
	require.Equal(t, "Issues", stored.view.Description)
	require.Equal(t, rotated, *env.lastUpdate(t).APIKeyValue)
	require.Equal(t, rotated, stored.apiKeyValue)
	require.Len(t, got.Status.Credentials, 1)
	require.Equal(t, secret.ResourceVersion, got.Status.Credentials[0].SecretResourceVersion)
}

// appliedStatusFailClient fails the status write that records an applied
// generation, like a crash after the PATCH.
type appliedStatusFailClient struct{ client.Client }

func (c appliedStatusFailClient) Status() client.SubResourceWriter {
	return appliedStatusFailWriter{c.Client.Status()}
}

type appliedStatusFailWriter struct{ client.SubResourceWriter }

func (w appliedStatusFailWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if s, ok := obj.(*coderv1alpha1.CoderMCPServer); ok && s.Status.AppliedGeneration != 0 {
		return errors.New("lost status write")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// The PATCH replaces the nonce, so the config ID is stored before it. When
// the status write after the PATCH is lost, the next reconcile still knows
// the config and applies again, instead of reporting its own config as a
// slug conflict.
func TestCoderMCPServerApplyLostStatusWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	r := &controller.CoderMCPServerReconciler{Client: appliedStatusFailClient{k8sClient}, Scheme: scheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)})
	require.ErrorContains(t, err, "lost status write")
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), server))
	require.NotEmpty(t, server.Status.ConfigID)
	require.Zero(t, server.Status.AppliedGeneration)
	require.Equal(t, "GitHub", env.requireCreated(t, server).view.DisplayName, "the PATCH replaced the nonce")

	got, _ := env.reconcile(ctx, t, server)
	requireMCPServerApplied(t, got)
	require.Equal(t, 1, env.posts())
	require.Equal(t, 2, env.fake.requestCount(routeUpdateMCPServer))
}
