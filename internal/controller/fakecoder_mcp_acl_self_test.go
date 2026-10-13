package controller_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These self-tests pin the fake's MCP server config ACL routes to the Coder
// facts that the CoderMCPServer controller depends on (plan facts F5, F11 and
// F13).

func aclGroupNames(acl codersdk.MCPServerConfigACL) []string {
	names := []string{}
	for _, g := range acl.Groups {
		names = append(names, g.Name)
	}
	return names
}

// F5: a new config's ACL lets the Everyone group read it. That group's ID is
// the organization ID, and each organization has its own.
func TestFakeCoderMCPServerACLDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	orgID, otherOrg := f.addOrganization("coder"), f.addOrganization("other")
	f.addMember(orgID, f.addUser("alice", codersdk.LoginTypePassword))
	c := f.client(t, 2*time.Second)

	created, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	acl, err := c.MCPServerConfigACL(ctx, orgID, created.ID)
	require.NoError(t, err)
	require.Empty(t, acl.Users)
	require.Len(t, acl.Groups, 1)
	everyone := acl.Groups[0]
	require.Equal(t, orgID, everyone.ID)
	require.Equal(t, orgID, everyone.OrganizationID)
	require.Equal(t, "Everyone", everyone.Name)
	require.Equal(t, "coder", everyone.OrganizationName)
	require.Equal(t, codersdk.MCPServerConfigRoleRead, everyone.Role)
	require.Equal(t, 1, everyone.TotalMemberCount, "Everyone counts every organization member")
	status, body := fakeRawRequest(t, f, http.MethodGet, "/api/v2/organizations/coder/mcp-servers/"+created.ID.String()+"/acl", "")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"users":[]`)
	require.Contains(t, body, `"members":[]`)

	other, err := c.CreateMCPServerConfig(ctx, otherOrg, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	groups, users := f.mcpACL(other.ID)
	require.Equal(t, []uuid.UUID{otherOrg}, groups)
	require.Empty(t, users)
	_, err = c.MCPServerConfigACL(ctx, otherOrg, created.ID)
	requireCoderStatus(t, err, http.StatusNotFound)
	requireCoderStatus(t, c.UpdateMCPServerConfigACL(ctx, orgID, uuid.New(), codersdk.UpdateMCPServerConfigACLRequest{}), http.StatusNotFound)
}

// F11, F13: an ACL PATCH is sparse, "" removes an entry, every UUID spelling
// names one entry, and a valid PATCH, also an empty one, moves updated_at. A
// rejected PATCH changes nothing.
func TestFakeCoderMCPServerACLPatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	f.now = steppingClock(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC))
	orgID, otherOrg := f.addOrganization("coder"), f.addOrganization("other")
	alice := f.addUser("alice", codersdk.LoginTypePassword)
	f.addMember(orgID, alice)
	outsider := f.addUser("outsider", codersdk.LoginTypePassword)
	data := f.addGroup(orgID, "data", "Data team", alice)
	foreign := f.addGroup(otherOrg, "data", "")
	c := f.client(t, 2*time.Second)
	created, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	patch := func(groups, users map[string]codersdk.MCPServerConfigRole) error {
		return c.UpdateMCPServerConfigACL(ctx, orgID, created.ID, codersdk.UpdateMCPServerConfigACLRequest{GroupRoles: groups, UserRoles: users})
	}
	requireACL := func(groups, users []uuid.UUID) {
		t.Helper()
		gotGroups, gotUsers := f.mcpACL(created.ID)
		require.ElementsMatch(t, groups, gotGroups)
		require.ElementsMatch(t, users, gotUsers)
	}
	updatedAt := func() time.Time {
		cfg, err := c.MCPServerConfigByID(ctx, orgID, created.ID)
		require.NoError(t, err)
		return cfg.UpdatedAt
	}

	require.NoError(t, patch(map[string]codersdk.MCPServerConfigRole{data.String(): "read"}, nil))
	requireACL([]uuid.UUID{orgID, data}, nil)
	acl, err := c.MCPServerConfigACL(ctx, orgID, created.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"Everyone", "data"}, aclGroupNames(acl))
	require.NoError(t, patch(
		map[string]codersdk.MCPServerConfigRole{strings.ToUpper(orgID.String()): ""},
		map[string]codersdk.MCPServerConfigRole{alice.String(): "read", uuid.NewString(): ""},
	), `"" removes an entry, also an unknown one, and an upper-case UUID names the same entry`)
	requireACL([]uuid.UUID{data}, []uuid.UUID{alice})
	acl, err = c.MCPServerConfigACL(ctx, orgID, created.ID)
	require.NoError(t, err)
	require.Len(t, acl.Users, 1)
	require.Equal(t, "alice", acl.Users[0].Username)
	require.Equal(t, codersdk.MCPServerConfigRoleRead, acl.Users[0].Role)

	before := updatedAt()
	require.NoError(t, patch(nil, nil))
	require.True(t, updatedAt().After(before), "an empty PATCH moves updated_at")
	requireACL([]uuid.UUID{data}, []uuid.UUID{alice})

	unknownGroup, unknownUser := uuid.New(), uuid.New()
	before = updatedAt()
	err = patch(
		map[string]codersdk.MCPServerConfigRole{data.String(): "write", unknownGroup.String(): "read", foreign.String(): "read", "not-a-uuid": "read"},
		map[string]codersdk.MCPServerConfigRole{outsider.String(): "read", unknownUser.String(): "read"},
	)
	validations := requireCoderMessage(t, err, http.StatusBadRequest, "Invalid request to update MCP server config ACL.")
	require.ElementsMatch(t, []codersdk.ValidationError{
		{Field: "group_roles", Detail: `role "write" is not a valid MCP server config role`},
		{Field: "group_roles", Detail: "not-a-uuid is not a valid UUID."},
		{Field: "group_roles", Detail: "group with ID " + unknownGroup.String() + " does not exist"},
		{Field: "user_roles", Detail: "user with ID " + unknownUser.String() + " does not exist"},
		{Field: "user_roles", Detail: "user " + outsider.String() + " does not belong to organization " + orgID.String()},
		{Field: "user_roles", Detail: "user " + unknownUser.String() + " does not belong to organization " + orgID.String()},
		{Field: "group_roles", Detail: "group " + foreign.String() + " does not belong to organization " + orgID.String()},
	}, validations)
	err = patch(map[string]codersdk.MCPServerConfigRole{data.String(): "read", "urn:uuid:" + data.String(): ""}, nil)
	require.Equal(t, []codersdk.ValidationError{{Field: "group_roles", Detail: "duplicate entries for ID " + data.String()}},
		requireCoderMessage(t, err, http.StatusBadRequest, "Invalid request to update MCP server config ACL."))
	requireACL([]uuid.UUID{data}, []uuid.UUID{alice})
	require.Equal(t, before, updatedAt(), "a rejected PATCH changes nothing")

	status, body := fakeRawRequest(t, f, http.MethodPatch, "/api/v2/organizations/coder/mcp-servers/"+created.ID.String()+"/acl", "{")
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, body, "Request body must be valid JSON.")
	bodies := f.mcpRequestBodies(routeUpdateMCPServerACL)
	require.Len(t, bodies, 6, "the fake records every ACL PATCH body")
	require.JSONEq(t, `{"group_roles":{"`+data.String()+`":"read"}}`, string(bodies[0]))
}

// F13: acl/available matches q loosely, so the caller must keep only exact
// names. Users page with limit, offset and after_id. Groups take only limit.
func TestFakeCoderMCPServerACLAvailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	orgID, otherOrg := f.addOrganization("coder"), f.addOrganization("other")
	for _, name := range []string{"carol", "Bob", "alice", "dana"} {
		f.addMember(orgID, f.addUser(name, codersdk.LoginTypePassword))
	}
	stranger := f.addUser("aaron", codersdk.LoginTypePassword) // In no organization.
	f.addMember(otherOrg, f.addUser("datafan", codersdk.LoginTypePassword))
	f.addGroup(orgID, "data", "")
	f.addGroup(orgID, "data-science", "Data science")
	f.addGroup(orgID, "analytics", "Big DATA")
	f.addGroup(orgID, "datax", "")
	f.addGroup(otherOrg, "data", "")
	c := f.client(t, 2*time.Second)
	created, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	available := func(q string, page codersdk.Pagination) codersdk.ACLAvailable {
		t.Helper()
		got, err := c.MCPServerConfigACLAvailable(ctx, orgID, created.ID, codersdk.UsersRequest{SearchQuery: q, Pagination: page})
		require.NoError(t, err)
		return got
	}
	groupNames := func(a codersdk.ACLAvailable) []string {
		names := []string{}
		for _, g := range a.Groups {
			names = append(names, g.Name)
		}
		return names
	}
	userNames := func(a codersdk.ACLAvailable) []string {
		names := []string{}
		for _, u := range a.Users {
			names = append(names, u.Username)
		}
		return names
	}

	all := available("", codersdk.Pagination{})
	require.ElementsMatch(t, []string{"Everyone", "data", "data-science", "analytics", "datax"}, groupNames(all))
	require.Equal(t, []string{"alice", "Bob", "carol", "dana"}, userNames(all), "users are ordered by lower-case username")
	require.ElementsMatch(t, []string{"data", "data-science", "analytics", "datax"}, groupNames(available("DATA", codersdk.Pagination{})),
		"q matches a substring of the name or display name, in any case")
	require.Equal(t, []string{"dana"}, userNames(available("search:da", codersdk.Pagination{})))
	require.ElementsMatch(t, []string{"data-science"}, groupNames(available("data_science", codersdk.Pagination{})), "_ matches any one character")
	require.ElementsMatch(t, []string{"datax"}, groupNames(available(`data\x`, codersdk.Pagination{})))
	require.Empty(t, groupNames(available(`data\`, codersdk.Pagination{})), "a trailing \\ quotes the closing %")
	f.addGroup(orgID, "50%-data%", "")
	require.Equal(t, []string{"50%-data%"}, groupNames(available(`data\`, codersdk.Pagination{})))
	require.Equal(t, []string{"50%-data%"}, groupNames(available(`0\%`, codersdk.Pagination{})), "\\% is a literal %")
	require.ElementsMatch(t, []string{"Everyone", "data-science"}, groupNames(available("e%n", codersdk.Pagination{})), "% matches any run")
	require.Empty(t, groupNames(available(`"data"`, codersdk.Pagination{})), "a bare term keeps its quotes")
	require.Equal(t, []string{"data-science"}, groupNames(available("data   science", codersdk.Pagination{})), "terms are joined with one space")

	page := available("", codersdk.Pagination{Limit: 2, Offset: 2})
	require.Equal(t, []string{"carol", "dana"}, userNames(page))
	require.Len(t, page.Groups, 2, "limit applies to groups")
	require.Equal(t, groupNames(available("", codersdk.Pagination{Limit: 2})), groupNames(page), "offset does not apply to groups")
	bob := all.Users[1].ID
	require.Equal(t, []string{"carol", "dana"}, userNames(available("", codersdk.Pagination{AfterID: bob})))
	require.Empty(t, userNames(available("", codersdk.Pagination{AfterID: stranger})), "a cursor user in no organization matches nothing")

	for q, detail := range map[string]string{
		":data":            `Query element ":data" cannot start or end with ':'`,
		"a:b:c":            `Query element "a:b:c" can only contain 1 ':'`,
		"data search:data": `Query param "search" provided more than once, found 2 times. Only provide 1 instance of this query param.`,
	} {
		_, err := c.MCPServerConfigACLAvailable(ctx, orgID, created.ID, codersdk.UsersRequest{SearchQuery: q})
		validations := requireCoderMessage(t, err, http.StatusBadRequest, "Invalid user search query.")
		require.Len(t, validations, 1, q)
		require.Equal(t, detail, validations[0].Detail, q)
	}
	path := "/api/v2/organizations/coder/mcp-servers/" + created.ID.String() + "/acl/available?"
	for _, query := range []string{"limit=-1", "offset=x", "after_id=bob"} {
		status, body := fakeRawRequest(t, f, http.MethodGet, path+query, "")
		require.Equal(t, http.StatusBadRequest, status, query)
		require.Contains(t, body, "Query parameters have invalid values.", query)
	}
}
