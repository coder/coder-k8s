package controller_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// mcpACLEnv is a fake Coder with the organization coder, the groups data,
// data-science, analytics (display name "Big DATA") and datax, and one MCP
// server config whose ACL holds the Everyone group and the member alice.
type mcpACLEnv struct {
	fake                    *fakeCoder
	sdk                     *codersdk.Client
	orgID, configID, alice  uuid.UUID
	data, science, analytic uuid.UUID
}

func newMCPACLEnv(t *testing.T) mcpACLEnv {
	t.Helper()
	ctx := context.Background()
	env := mcpACLEnv{fake: newFakeCoder(t)}
	env.orgID = env.fake.addOrganization("coder")
	env.alice = env.fake.addUser("alice", codersdk.LoginTypePassword)
	env.fake.addMember(env.orgID, env.alice)
	env.data = env.fake.addGroup(env.orgID, "data", "")
	env.science = env.fake.addGroup(env.orgID, "data-science", "")
	env.analytic = env.fake.addGroup(env.orgID, "analytics", "Big DATA")
	env.fake.addGroup(env.orgID, "datax", "")
	env.sdk = env.fake.client(t, 2*time.Second)
	config, err := env.sdk.CreateMCPServerConfig(ctx, env.orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	env.configID = config.ID
	require.NoError(t, env.sdk.UpdateMCPServerConfigACL(ctx, env.orgID, env.configID, codersdk.UpdateMCPServerConfigACLRequest{
		UserRoles: map[string]codersdk.MCPServerConfigRole{env.alice.String(): codersdk.MCPServerConfigRoleRead},
	}))
	return env
}

// mcpACLCalls counts the ACL route requests since a mark.
type mcpACLCalls struct{ available, get, patch int }

func (env mcpACLEnv) calls() mcpACLCalls {
	return mcpACLCalls{
		available: env.fake.requestCount(routeMCPServerACLAvailable),
		get:       env.fake.requestCount(routeMCPServerACL),
		patch:     env.fake.requestCount(routeUpdateMCPServerACL),
	}
}

func (env mcpACLEnv) sync(t *testing.T, groups ...string) (controller.MCPServerACLResult, mcpACLCalls) {
	t.Helper()
	before := env.calls()
	got, err := controller.SyncMCPServerACL(context.Background(), env.sdk, env.orgID, env.configID, &coderv1alpha1.CoderMCPServerAccess{Groups: groups})
	require.NoError(t, err)
	after := env.calls()
	return got, mcpACLCalls{available: after.available - before.available, get: after.get - before.get, patch: after.patch - before.patch}
}

func (env mcpACLEnv) requireACL(t *testing.T, groups ...uuid.UUID) {
	t.Helper()
	gotGroups, gotUsers := env.fake.mcpACL(env.configID)
	require.ElementsMatch(t, groups, gotGroups)
	require.Empty(t, gotUsers, "access lists groups only, so no user entry stays")
}

// Plan S23: names resolve to exactly the group with that name, the ACL
// becomes exactly access.groups, user entries go, and a second run sends no
// write.
func TestMCPServerACLExactGroups(t *testing.T) {
	t.Parallel()
	env := newMCPACLEnv(t)
	got, calls := env.sync(t, "data")
	require.Empty(t, got.Reason, got.Message)
	require.Equal(t, mcpACLCalls{available: 1, get: 1, patch: 1}, calls)
	env.requireACL(t, env.data)
	require.JSONEq(t, `{"group_roles":{"`+env.orgID.String()+`":"","`+env.data.String()+`":"read"},"user_roles":{"`+env.alice.String()+`":""}}`,
		string(env.fake.mcpRequestBodies(routeUpdateMCPServerACL)[1]), "one sparse PATCH removes Everyone and alice")

	got, calls = env.sync(t, "data")
	require.Empty(t, got.Reason)
	require.Equal(t, mcpACLCalls{available: 1, get: 1}, calls, "a second run sends nothing")

	// data and data-science resolve to different groups. Everyone needs no
	// search.
	got, calls = env.sync(t, "Everyone", "data-science")
	require.Empty(t, got.Reason, got.Message)
	require.Equal(t, mcpACLCalls{available: 1, get: 1, patch: 1}, calls)
	env.requireACL(t, env.orgID, env.science)

	// Plan S15: no groups leaves the server to organization admins.
	got, _ = env.sync(t)
	require.Empty(t, got.Reason)
	env.requireACL(t)
}

// A nil access means the controller does not own the ACL.
func TestMCPServerACLNotOwned(t *testing.T) {
	t.Parallel()
	env := newMCPACLEnv(t)
	before := env.fake.totalRequests()
	got, err := controller.SyncMCPServerACL(context.Background(), env.sdk, env.orgID, env.configID, nil)
	require.NoError(t, err)
	require.Empty(t, got.Reason)
	require.Equal(t, before, env.fake.totalRequests(), "no read and no write")
}

// A name that resolves to no group, or that Coder's search rejects, stops
// the sync before the ACL read, so nothing is written.
func TestMCPServerACLGroupNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		groups    []string
		fault     *fakeFault
		requeue   time.Duration
		available int
		message   string
	}{
		{
			name: "only loose matches", groups: []string{"science", "Data", "big-data"}, requeue: 10 * time.Minute, available: 3,
			message: "Coder organization has no group named science, Data, big-data.",
		},
		{
			name: "one name missing", groups: []string{"data", "missing"}, requeue: 10 * time.Minute, available: 2,
			message: "Coder organization has no group named missing.",
		},
		{name: "lowercase everyone is a normal name", groups: []string{"everyone"}, requeue: 10 * time.Minute, available: 1},
		{
			name: "search rejected", groups: []string{"data"}, fault: &fakeFault{Status: http.StatusBadRequest}, requeue: 10 * time.Minute, available: 1,
			message: `Coder rejected the group search for "data": Coder answered 400: fake fault 400`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newMCPACLEnv(t)
			if tc.fault != nil {
				env.fake.failNext(routeMCPServerACLAvailable, *tc.fault)
			}
			got, calls := env.sync(t, tc.groups...)
			require.Equal(t, "GroupNotFound", got.Reason)
			require.True(t, got.Stalled)
			require.Equal(t, tc.requeue, got.Requeue)
			require.Contains(t, got.Message, tc.message)
			require.Equal(t, mcpACLCalls{available: tc.available}, calls, "no ACL read and no write")
		})
	}
}

// The controller repeats the access.groups name rule, so q never carries
// search syntax. A name past admission sends no request at all.
func TestMCPServerACLRejectsNamesOutsideThePattern(t *testing.T) {
	t.Parallel()
	env := newMCPACLEnv(t)
	for _, name := range []string{"data science", "role:admin", "50%", "data_science", `data\`, `"data"`, "-data", "data-", strings.Repeat("a", 256), ""} {
		before := env.fake.totalRequests()
		got, err := controller.SyncMCPServerACL(context.Background(), env.sdk, env.orgID, env.configID, &coderv1alpha1.CoderMCPServerAccess{Groups: []string{"data", name}})
		require.NoError(t, err)
		require.Equal(t, "GroupNotFound", got.Reason, name)
		require.True(t, got.Stalled)
		require.Zero(t, got.Requeue, "only a spec change helps")
		require.Equal(t, 1, env.fake.totalRequests()-before, "only the search for data ran: %q", name)
	}
	got, _ := env.sync(t, strings.Repeat("a", 255))
	require.Equal(t, "GroupNotFound", got.Reason)
	require.NotZero(t, got.Requeue, "255 characters is a valid name, searched in Coder")
}

// Answers that cannot be right stop the sync, and Coder errors wait.
func TestMCPServerACLCoderAnswers(t *testing.T) {
	t.Parallel()
	rewriteAvailable := func(edit func(*codersdk.ACLAvailable)) fakeFault {
		return fakeFault{Rewrite: func(answer any) any {
			a := answer.(codersdk.ACLAvailable)
			edit(&a)
			return a
		}}
	}
	for _, tc := range []struct {
		name    string
		route   string
		fault   fakeFault
		reason  string
		stalled bool
		calls   mcpACLCalls
	}{
		{
			name: "search answers another organization", route: routeMCPServerACLAvailable, reason: "CoderAnswerMismatch", stalled: true, calls: mcpACLCalls{available: 1},
			fault: rewriteAvailable(func(a *codersdk.ACLAvailable) { a.Groups[0].OrganizationID = uuid.New() }),
		},
		{
			name: "two groups with the name", route: routeMCPServerACLAvailable, reason: "CoderAnswerMismatch", stalled: true, calls: mcpACLCalls{available: 1},
			fault: rewriteAvailable(func(a *codersdk.ACLAvailable) {
				i := slices.IndexFunc(a.Groups, func(g codersdk.Group) bool { return g.Name == "data" })
				a.Groups = append(a.Groups, a.Groups[i])
			}),
		},
		{
			name: "group without an ID", route: routeMCPServerACLAvailable, reason: "CoderAnswerMismatch", stalled: true, calls: mcpACLCalls{available: 1},
			fault: rewriteAvailable(func(a *codersdk.ACLAvailable) {
				a.Groups[slices.IndexFunc(a.Groups, func(g codersdk.Group) bool { return g.Name == "data" })].ID = uuid.Nil
			}),
		},
		{name: "search fails", route: routeMCPServerACLAvailable, fault: fakeFault{Status: http.StatusBadGateway}, reason: "CoderUnavailable", calls: mcpACLCalls{available: 1}},
		{name: "ACL read fails", route: routeMCPServerACL, fault: fakeFault{Status: http.StatusServiceUnavailable}, reason: "CoderUnavailable", calls: mcpACLCalls{available: 1, get: 1}},
		{
			name: "ACL answers another organization", route: routeMCPServerACL, reason: "CoderAnswerMismatch", stalled: true, calls: mcpACLCalls{available: 1, get: 1},
			fault: fakeFault{Rewrite: func(answer any) any {
				acl := answer.(codersdk.MCPServerConfigACL)
				acl.Groups[0].OrganizationID = uuid.New()
				return acl
			}},
		},
		{
			name: "ACL user without an ID", route: routeMCPServerACL, reason: "CoderAnswerMismatch", stalled: true, calls: mcpACLCalls{available: 1, get: 1},
			fault: fakeFault{Rewrite: func(answer any) any {
				acl := answer.(codersdk.MCPServerConfigACL)
				acl.Users[0].ID = uuid.Nil
				return acl
			}},
		},
		{name: "update rejected", route: routeUpdateMCPServerACL, fault: fakeFault{Status: http.StatusBadRequest}, reason: "CoderRejected", stalled: true, calls: mcpACLCalls{available: 1, get: 1, patch: 1}},
		{name: "update fails", route: routeUpdateMCPServerACL, fault: fakeFault{Status: http.StatusInternalServerError}, reason: "CoderUnavailable", calls: mcpACLCalls{available: 1, get: 1, patch: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newMCPACLEnv(t)
			env.fake.failNext(tc.route, tc.fault)
			got, calls := env.sync(t, "data")
			require.Equal(t, tc.reason, got.Reason, got.Message)
			require.Equal(t, tc.stalled, got.Stalled)
			require.Equal(t, tc.calls, calls)
			want := map[bool]time.Duration{true: 10 * time.Minute, false: 30 * time.Second}[tc.stalled]
			require.Equal(t, want, got.Requeue)
			groups, users := env.fake.mcpACL(env.configID)
			require.Equal(t, []uuid.UUID{env.orgID}, groups, "the ACL did not change")
			require.Equal(t, []uuid.UUID{env.alice}, users)

			// The next run converges.
			got, _ = env.sync(t, "data")
			require.Empty(t, got.Reason, got.Message)
			env.requireACL(t, env.data)
		})
	}
}

// A PATCH whose answer is lost after Coder applied it is safe to repeat: the
// next run finds the ACL in place and sends nothing.
func TestMCPServerACLLostAnswer(t *testing.T) {
	t.Parallel()
	env := newMCPACLEnv(t)
	env.fake.failNext(routeUpdateMCPServerACL, fakeFault{AfterCommit: true, Reset: true})
	got, calls := env.sync(t, "data", "analytics")
	require.Equal(t, "CoderUnavailable", got.Reason)
	require.Equal(t, mcpACLCalls{available: 2, get: 1, patch: 1}, calls)
	env.requireACL(t, env.data, env.analytic)
	got, calls = env.sync(t, "data", "analytics")
	require.Empty(t, got.Reason)
	require.Zero(t, calls.patch)
}

// The comparison does not depend on the order of Coder's lists: the fake
// sorts bytewise, while Coder sorts with the database collation or not at
// all. A role other than read is set back.
func TestMCPServerACLComparesSets(t *testing.T) {
	t.Parallel()
	env := newMCPACLEnv(t)
	got, _ := env.sync(t, "data", "data-science", "Everyone")
	require.Empty(t, got.Reason)
	reverse := func(answer any) any {
		acl := answer.(codersdk.MCPServerConfigACL)
		slices.Reverse(acl.Groups)
		return acl
	}
	env.fake.failNext(routeMCPServerACL, fakeFault{Rewrite: reverse})
	env.fake.failNext(routeMCPServerACLAvailable, fakeFault{Rewrite: func(answer any) any {
		a := answer.(codersdk.ACLAvailable)
		slices.Reverse(a.Groups)
		return a
	}})
	got, calls := env.sync(t, "Everyone", "data-science", "data")
	require.Empty(t, got.Reason)
	require.Zero(t, calls.patch, "another order is no difference")

	env.fake.failNext(routeMCPServerACL, fakeFault{Rewrite: func(answer any) any {
		acl := answer.(codersdk.MCPServerConfigACL)
		acl.Groups[0].Role = "write"
		return acl
	}})
	_, calls = env.sync(t, "Everyone", "data-science", "data")
	require.Equal(t, 1, calls.patch)
	bodies := env.fake.mcpRequestBodies(routeUpdateMCPServerACL)
	require.NotContains(t, string(bodies[len(bodies)-1]), `""`, "the PATCH sets read and removes nothing")
}
