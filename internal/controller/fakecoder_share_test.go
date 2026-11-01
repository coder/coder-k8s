package controller_test

import (
	"maps"
	"net/http"
	"slices"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

// Route names for the Share mode routes of fakeCoder (#153 PR 14). The
// workspace ACL and sharing settings reads are pool routes, see
// fakecoder_pool_test.go. Groups come from addGroup, see
// fakecoder_mcp_acl_test.go.
const (
	routeGroup                 = "group"
	routeUpdateSharingSettings = "updateWorkspaceSharingSettings"
)

// fakeShareState mirrors Coder v2.37.2 for the routes that only Share mode
// uses.
type fakeShareState struct {
	licensed bool // Premium: every /groups route needs it (templateRBACEnabledMW).
}

func (f *fakeCoder) registerShareRoutes(mux *http.ServeMux) {
	f.route(mux, "GET /api/v2/groups/{id}", routeGroup, func(r *http.Request) (int, any) {
		if !f.share.licensed {
			return fakeError(http.StatusForbidden, "Template RBAC is a Premium feature. Contact sales!")
		}
		id, err := uuid.Parse(r.PathValue("id"))
		g, ok := f.mcpGroup(id)
		if err != nil || !ok {
			return http.StatusNotFound, fakeResourceNotFound
		}
		members := g.members
		if g.id == g.orgID { // The Everyone group: every member of its organization.
			members = slices.Collect(maps.Keys(f.orgRoles[g.orgID]))
		}
		group := f.mcpGroupAnswer(g)
		for _, id := range members {
			u := f.users[id]
			group.Members = append(group.Members, codersdk.ReducedUser{
				MinimalUser: codersdk.MinimalUser{ID: u.ID, Username: u.Username}, Email: u.Email, Status: u.Status, LoginType: u.LoginType,
			})
		}
		return http.StatusOK, group
	})
	// The controller never changes sharing settings (plan T6). The route
	// exists so that such a request is counted and fails the test.
	f.route(mux, "PATCH /api/v2/organizations/{org}/settings/workspace-sharing", routeUpdateSharingSettings, func(*http.Request) (int, any) {
		f.t.Errorf("fake coder: the controller changed the workspace sharing settings of an organization")
		return fakeError(http.StatusInternalServerError, "The controller must never change sharing settings.")
	})
}

// Setup helpers. Like the ones in fakecoder_test.go, they lock the fake.

// setLicensed turns the Premium license on or off.
func (f *fakeCoder) setLicensed(licensed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share.licensed = licensed
}

// setGroupMembers replaces the members of a group from addGroup.
func (f *fakeCoder) setGroupMembers(groupID uuid.UUID, members ...uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.mcp.groups, func(g *fakeGroup) bool { return g.id == groupID })
	if i < 0 {
		f.t.Fatalf("assertion failed: fake coder has no group %s", groupID)
	}
	f.mcp.groups[i].members = slices.Clone(members)
}

// addUserWithID adds a user with a fixed ID, such as Coder's prebuilds
// system user.
func (f *fakeCoder) addUserWithID(id uuid.UUID, username string) {
	added := f.addUser(username, codersdk.LoginTypeNone) //nolint:staticcheck // Coder's system users have login type none.
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[added]
	delete(f.users, added)
	u.ID = id
	f.users[id] = u
}
