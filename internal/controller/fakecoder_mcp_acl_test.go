package controller_test

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

// MCP server config ACL routes of the fake Coder (coderd/mcp_acl.go at tag
// v2.37.3). The fake's one token is a site owner, so it always holds the
// share permission these routes check. Coder sets no order on the groups and
// users it returns, so tests must not depend on the order of either list.
const (
	routeMCPServerACL          = "mcpServerACL"
	routeUpdateMCPServerACL    = "updateMCPServerACL"
	routeMCPServerACLAvailable = "mcpServerACLAvailable"
)

// fakeGroup is one groups row (coderd/database/dump.sql). The fake does not
// store the Everyone group: every organization has one, and its ID is the
// organization ID (coderd/database/migrations/000201_ensure_default_everyone_group.up.sql).
type fakeGroup struct {
	id, orgID         uuid.UUID
	name, displayName string
	members           []uuid.UUID
}

func (f *fakeCoder) registerMCPServerACLRoutes(mux *http.ServeMux) {
	const base = "/api/v2/organizations/{org}/mcp-servers/{id}"
	f.route(mux, "GET "+base+"/acl", routeMCPServerACL, f.withMCPServer(f.getMCPServerACL))
	f.route(mux, "PATCH "+base+"/acl", routeUpdateMCPServerACL, f.withMCPServer(f.patchMCPServerACL))
	f.route(mux, "GET "+base+"/acl/available", routeMCPServerACLAvailable, f.withMCPServer(f.mcpServerACLAvailable))
}

// mcpGroup mirrors a groups lookup by ID in any organization.
func (f *fakeCoder) mcpGroup(id uuid.UUID) (fakeGroup, bool) {
	if org, ok := f.orgs[id]; ok {
		return fakeGroup{id: org.ID, orgID: org.ID, name: "Everyone"}, true
	}
	i := slices.IndexFunc(f.mcp.groups, func(g *fakeGroup) bool { return g.id == id })
	if i < 0 {
		return fakeGroup{}, false
	}
	return *f.mcp.groups[i], true
}

// mcpOrgGroups returns the Everyone group, then the organization's other
// groups in insert order.
func (f *fakeCoder) mcpOrgGroups(orgID uuid.UUID) []fakeGroup {
	out := []fakeGroup{{id: orgID, orgID: orgID, name: "Everyone"}}
	for _, g := range f.mcp.groups {
		if g.orgID == orgID {
			out = append(out, *g)
		}
	}
	return out
}

// mcpGroupAnswer mirrors db2sdk.Group (coderd/database/db2sdk/db2sdk.go:364-378)
// with no member list, as both ACL routes call it. The Everyone group counts
// every member of its organization.
func (f *fakeCoder) mcpGroupAnswer(g fakeGroup) codersdk.Group {
	org := f.orgs[g.orgID]
	count := len(g.members)
	if g.id == g.orgID {
		count = len(f.orgRoles[g.orgID])
	}
	return codersdk.Group{
		ID: g.id, Name: g.name, DisplayName: g.displayName, OrganizationID: g.orgID, Members: []codersdk.ReducedUser{},
		TotalMemberCount: count, Source: codersdk.GroupSourceUser, OrganizationName: org.Name, OrganizationDisplayName: org.DisplayName,
	}
}

// getMCPServerACL mirrors mcpServerConfigACL (coderd/mcp_acl.go:36-59, 252-299):
// entries whose user or group no longer exists drop out.
func (f *fakeCoder) getMCPServerACL(s *fakeMCPServer, _ *http.Request) (int, any) {
	out := codersdk.MCPServerConfigACL{Users: []codersdk.MCPServerConfigUser{}, Groups: []codersdk.MCPServerConfigGroup{}}
	for _, id := range fakeSortedIDs(s.userACL) {
		if u, ok := f.users[id]; ok {
			out.Users = append(out.Users, codersdk.MCPServerConfigUser{MinimalUser: u.MinimalUser, Role: codersdk.MCPServerConfigRoleRead})
		}
	}
	for _, id := range fakeSortedIDs(s.groupACL) {
		if g, ok := f.mcpGroup(id); ok {
			out.Groups = append(out.Groups, codersdk.MCPServerConfigGroup{Group: f.mcpGroupAnswer(g), Role: codersdk.MCPServerConfigRoleRead})
		}
	}
	return http.StatusOK, out
}

// patchMCPServerACL mirrors patchMCPServerConfigACL (coderd/mcp_acl.go:173-250):
// a sparse update. Only the listed IDs change, and the role "" removes an
// entry without checking that its user or group exists. Any validation error
// rejects the whole request. A valid request, also an empty one, moves
// updated_at (coderd/database/queries/mcpserverconfigs.sql:182-190). Coder
// reports validation errors in map order. The fake sorts the keys.
func (f *fakeCoder) patchMCPServerACL(s *fakeMCPServer, r *http.Request) (int, any) {
	var req codersdk.UpdateMCPServerConfigACLRequest
	if status, resp, ok := f.readMCPBody(routeUpdateMCPServerACL, r, &req); !ok {
		return status, resp
	}
	groupExists := func(id uuid.UUID) bool { _, ok := f.mcpGroup(id); return ok }
	userExists := func(id uuid.UUID) bool { _, ok := f.users[id]; return ok }
	// acl.Validate checks groups, then users (coderd/rbac/acl/updatevalidator.go:28-130).
	v := fakeACLValidate("group_roles", "group", req.GroupRoles, groupExists)
	v = append(v, fakeACLValidate("user_roles", "user", req.UserRoles, userExists)...)
	// validateMCPServerConfigACLOrganization (coderd/mcp_acl.go:385-425).
	orgID := s.view.OrganizationID
	for _, id := range fakeActiveACLIDs(req.UserRoles) {
		if _, member := f.orgRoles[orgID][id]; !member {
			v = append(v, codersdk.ValidationError{Field: "user_roles", Detail: "user " + id.String() + " does not belong to organization " + orgID.String()})
		}
	}
	for _, id := range fakeActiveACLIDs(req.GroupRoles) {
		if g, ok := f.mcpGroup(id); ok && g.orgID != orgID {
			v = append(v, codersdk.ValidationError{Field: "group_roles", Detail: "group " + id.String() + " does not belong to organization " + orgID.String()})
		}
	}
	users, dup := fakeCanonicalACLRoles("user_roles", req.UserRoles)
	v = append(v, dup...)
	groups, dup := fakeCanonicalACLRoles("group_roles", req.GroupRoles)
	v = append(v, dup...)
	if len(v) > 0 {
		return http.StatusBadRequest, codersdk.Response{Message: "Invalid request to update MCP server config ACL.", Validations: v}
	}
	fakeApplyACLRoles(s.userACL, users)
	fakeApplyACLRoles(s.groupACL, groups)
	s.view.UpdatedAt = f.now().UTC().Truncate(time.Microsecond)
	return http.StatusNoContent, nil
}

func fakeACLValidate(field, kind string, roles map[string]codersdk.MCPServerConfigRole, exists func(uuid.UUID) bool) []codersdk.ValidationError {
	var out []codersdk.ValidationError
	var ids []uuid.UUID
	for _, raw := range fakeSortedKeys(roles) {
		role := roles[raw]
		if role != codersdk.MCPServerConfigRoleRead && role != codersdk.MCPServerConfigRoleDeleted {
			out = append(out, codersdk.ValidationError{Field: field, Detail: fmt.Sprintf("role %q is not a valid MCP server config role", role)})
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			out = append(out, codersdk.ValidationError{Field: field, Detail: fmt.Sprintf("%v is not a valid UUID.", raw)})
			continue
		}
		if role != codersdk.MCPServerConfigRoleDeleted {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if !exists(id) {
			out = append(out, codersdk.ValidationError{Field: field, Detail: fmt.Sprintf("%s with ID %v does not exist", kind, id)})
		}
	}
	return out
}

func fakeActiveACLIDs(roles map[string]codersdk.MCPServerConfigRole) []uuid.UUID {
	var out []uuid.UUID
	for _, raw := range fakeSortedKeys(roles) {
		if id, err := uuid.Parse(raw); err == nil && roles[raw] != codersdk.MCPServerConfigRoleDeleted {
			out = append(out, id)
		}
	}
	return out
}

// fakeCanonicalACLRoles mirrors canonicalMCPServerConfigACLRoles
// (coderd/mcp_acl.go:344-363): every spelling uuid.Parse accepts names the same
// entry, and two spellings of one ID reject the request.
func fakeCanonicalACLRoles(field string, roles map[string]codersdk.MCPServerConfigRole) (map[uuid.UUID]codersdk.MCPServerConfigRole, []codersdk.ValidationError) {
	out := map[uuid.UUID]codersdk.MCPServerConfigRole{}
	var dup []codersdk.ValidationError
	for _, raw := range fakeSortedKeys(roles) {
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		if _, ok := out[id]; ok {
			dup = append(dup, codersdk.ValidationError{Field: field, Detail: fmt.Sprintf("duplicate entries for ID %s", id)})
			continue
		}
		out[id] = roles[raw]
	}
	return out, dup
}

func fakeApplyACLRoles(entries map[uuid.UUID]struct{}, roles map[uuid.UUID]codersdk.MCPServerConfigRole) {
	for id, role := range roles {
		if role == codersdk.MCPServerConfigRoleDeleted {
			delete(entries, id)
			continue
		}
		entries[id] = struct{}{}
	}
}

// mcpServerACLAvailable mirrors mcpServerConfigACLAvailable
// (coderd/mcp_acl.go:75-160). One q search term filters users and groups
// loosely. Users page with limit, offset and after_id
// (coderd/database/queries/organizationmembers.sql:84-243). Groups take only
// limit, never offset or after_id, so every page holds the same groups, and
// a group past the limit never appears (coderd/database/queries/groups.sql:43-92).
// A limit of 0 means no limit.
func (f *fakeCoder) mcpServerACLAvailable(s *fakeMCPServer, r *http.Request) (int, any) {
	query := r.URL.Query()
	search, v := f.fakeUserSearch(query.Get("q"))
	if len(v) > 0 {
		return http.StatusBadRequest, codersdk.Response{Message: "Invalid user search query.", Validations: v}
	}
	// ParsePagination (coderd/pagination.go:14-34).
	var afterID uuid.UUID
	if raw := query.Get("after_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			v = append(v, codersdk.ValidationError{Field: "after_id", Detail: `Query param "after_id" must be a valid uuid`})
		}
		afterID = id
	}
	limit, offset := fakePositiveInt32(query, "limit", &v), fakePositiveInt32(query, "offset", &v)
	if len(v) > 0 {
		return http.StatusBadRequest, codersdk.Response{Message: "Query parameters have invalid values.", Validations: v}
	}
	match := fakeILikeContains(search)

	orgID := s.view.OrganizationID
	// The after_id cursor reads the user's membership in any organization. A
	// user with none gives a NULL comparison, so no user matches.
	afterUser, hasCursorRow := f.users[afterID]
	hasCursorRow = hasCursorRow && len(afterUser.OrganizationIDs) > 0
	afterCursor := func(u codersdk.User) bool {
		return afterID == uuid.Nil || (hasCursorRow && strings.ToLower(u.Username) > strings.ToLower(afterUser.Username))
	}
	users := []codersdk.ReducedUser{}
	for id := range f.orgRoles[orgID] {
		u := f.users[id]
		if (match(u.Email) || match(u.Username) || match(u.Name)) && afterCursor(u) {
			// mcpServerConfigACLReducedUser (coderd/mcp_acl.go:321-337).
			users = append(users, codersdk.ReducedUser{
				MinimalUser: u.MinimalUser, Email: u.Email, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt, LastSeenAt: u.LastSeenAt,
				Status: u.Status, LoginType: u.LoginType, IsServiceAccount: u.IsServiceAccount,
			})
		}
	}
	// ORDER BY LOWER(username). The fake compares bytes, Postgres uses its collation.
	slices.SortFunc(users, func(a, b codersdk.ReducedUser) int {
		return strings.Compare(strings.ToLower(a.Username), strings.ToLower(b.Username))
	})
	users = users[min(offset, len(users)):]
	groups := []codersdk.Group{}
	for _, g := range f.mcpOrgGroups(orgID) {
		if match(g.name) || match(g.displayName) {
			groups = append(groups, f.mcpGroupAnswer(g))
		}
	}
	if limit > 0 {
		users, groups = users[:min(limit, len(users))], groups[:min(limit, len(groups))]
	}
	return http.StatusOK, codersdk.ACLAvailable{Users: users, Groups: groups}
}

// fakeUserSearch mirrors searchquery.Users (coderd/searchquery/search.go:148-177,
// 686-726, 857-893) for the search term only. Coder lowercases q and splits it
// at spaces outside double quotes. It joins the terms without a colon into one
// search term, with one space between them and their quotes kept. A key:value
// term sets that key. The fake models only the "search" key and fails the
// test for any other key.
func (f *fakeCoder) fakeUserSearch(q string) (string, []codersdk.ValidationError) {
	var elements, bare []string
	for _, token := range fakeSplitQuery(strings.ToLower(q), ' ', true) {
		if strings.Contains(token, ":") {
			elements = append(elements, token)
		} else {
			bare = append(bare, token)
		}
	}
	if len(bare) > 0 {
		elements = append(elements, strings.Join(bare, " "))
	}
	var searches []string
	for _, e := range elements {
		if strings.HasPrefix(e, ":") || strings.HasSuffix(e, ":") {
			return "", []codersdk.ValidationError{{Field: "q", Detail: fmt.Sprintf("Query element %q cannot start or end with ':'", e)}}
		}
		parts := fakeSplitQuery(e, ':', false)
		switch {
		case len(parts) == 1:
			searches = append(searches, e)
		case len(parts) == 2 && parts[0] == "search":
			searches = append(searches, parts[1])
		case len(parts) == 2:
			f.t.Errorf("fake coder: the q key %q is not modeled", parts[0])
			return "", []codersdk.ValidationError{{Field: parts[0], Detail: "fake coder: key not modeled"}}
		default:
			return "", []codersdk.ValidationError{{Field: "q", Detail: fmt.Sprintf("Query element %q can only contain 1 ':'", e)}}
		}
	}
	if len(searches) > 1 {
		return "", []codersdk.ValidationError{{Field: "search", Detail: fmt.Sprintf("Query param %q provided more than once, found %d times. Only provide 1 instance of this query param.", "search", len(searches))}}
	}
	if len(searches) == 0 {
		return "", nil
	}
	return searches[0], nil
}

// fakeSplitQuery mirrors splitQueryParameterByDelimiter
// (coderd/searchquery/search.go:857-872).
func fakeSplitQuery(query string, delimiter rune, maintainQuotes bool) []string {
	quoted := false
	parts := strings.FieldsFunc(query, func(r rune) bool {
		if r == '"' {
			quoted = !quoted
		}
		return !quoted && r == delimiter
	})
	if !maintainQuotes {
		for i, part := range parts {
			parts[i] = strings.Trim(part, `"`)
		}
	}
	return parts
}

// fakeILikeContains returns a matcher for Postgres `value ILIKE concat('%', search, '%')`.
// Coder does not escape the search term, so % and _ in it are wildcards and \
// quotes the next character. A trailing \ quotes the closing %, so the value
// must then end with a literal %. An empty search matches everything.
func fakeILikeContains(search string) func(string) bool {
	var b strings.Builder
	b.WriteString(`(?is)^`)
	escaped := false
	for _, r := range "%" + search + "%" {
		switch {
		case escaped:
			b.WriteString(regexp.QuoteMeta(string(r)))
			escaped = false
		case r == '\\':
			escaped = true
		case r == '%':
			b.WriteString(`.*`)
		case r == '_':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if escaped {
		panic("assertion failed: a LIKE pattern that ends with % cannot end with an escape")
	}
	b.WriteString(`$`)
	return regexp.MustCompile(b.String()).MatchString
}

// fakePositiveInt32 mirrors QueryParamParser.PositiveInt32
// (coderd/httpapi/queryparams.go:103-121) with a default of 0.
func fakePositiveInt32(query map[string][]string, param string, v *[]codersdk.ValidationError) int {
	raw := ""
	if vals := query[param]; len(vals) > 0 {
		raw = vals[0]
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err == nil && n < 0 {
		err = fmt.Errorf("value is negative")
	}
	if err != nil {
		*v = append(*v, codersdk.ValidationError{Field: param, Detail: fmt.Sprintf("Query param %q must be a valid 32-bit positive integer: %s", param, err.Error())})
		return 0
	}
	return int(n)
}

func fakeSortedKeys(m map[string]codersdk.MCPServerConfigRole) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func fakeSortedIDs(m map[uuid.UUID]struct{}) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return ids
}

// Setup helpers and assertions. They lock the fake.

// addGroup adds a group whose members must be members of the organization.
func (f *fakeCoder) addGroup(orgID uuid.UUID, name, displayName string, members ...uuid.UUID) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.orgs[orgID]; !ok {
		f.t.Fatalf("assertion failed: fake coder has no organization %s", orgID)
	}
	for _, m := range members {
		if _, ok := f.orgRoles[orgID][m]; !ok {
			f.t.Fatalf("assertion failed: user %s is not a member of organization %s", m, orgID)
		}
	}
	g := &fakeGroup{id: uuid.New(), orgID: orgID, name: name, displayName: displayName, members: slices.Clone(members)}
	f.mcp.groups = append(f.mcp.groups, g)
	return g.id
}

// mcpACL returns the stored group and user IDs of a config's ACL, sorted.
func (f *fakeCoder) mcpACL(id uuid.UUID) (groups, users []uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.mcp.servers {
		if s.view.ID == id {
			return fakeSortedIDs(s.groupACL), fakeSortedIDs(s.userACL)
		}
	}
	f.t.Fatalf("assertion failed: fake coder has no MCP server config %s", id)
	return nil, nil
}
