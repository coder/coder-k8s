package controller

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// mcpServerEveryoneGroup is the group of all organization members. Its ID
	// is the organization ID (coderd/database/queries/groups.sql:169-176 at
	// Coder v2.37.3).
	mcpServerEveryoneGroup = "Everyone"
	// mcpServerGroupNameMaxLength is the MaxLength marker of access.groups.
	mcpServerGroupNameMaxLength = 255
)

// mcpServerGroupNamePattern is the Pattern marker of access.groups. A name
// that matches it holds none of the characters that Coder reads in a q
// search query as syntax: no colon, space, quote, %, _ or \.
var mcpServerGroupNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]+(?:-[a-zA-Z0-9]+)*$`)

// syncMCPServerACL makes the ACL of the config configID exactly
// access.groups with the read role (plan 4.8). It resolves every name before
// it reads the ACL, and sends one sparse PATCH only when the ACL differs. A
// nil access means the controller does not own the ACL, so it sends nothing.
// It returns nil when the ACL matches.
func syncMCPServerACL(
	ctx context.Context, sdk *codersdk.Client, orgID, configID uuid.UUID, access *coderv1alpha1.CoderMCPServerAccess,
) (*mcpServerStep, error) {
	return syncMCPServerACLPass(ctx, sdk, orgID, configID, access, false)
}

// narrowMCPServerACL removes every ACL entry that access does not list, and
// adds nothing. A caller that changes the config runs it before the change
// and syncMCPServerACL after it. Then no principal outside both the old and
// the new access list can reach the config at any point, also when the
// change fails. It returns nil when nothing is left to remove.
func narrowMCPServerACL(
	ctx context.Context, sdk *codersdk.Client, orgID, configID uuid.UUID, access *coderv1alpha1.CoderMCPServerAccess,
) (*mcpServerStep, error) {
	return syncMCPServerACLPass(ctx, sdk, orgID, configID, access, true)
}

func syncMCPServerACLPass(
	ctx context.Context, sdk *codersdk.Client, orgID, configID uuid.UUID, access *coderv1alpha1.CoderMCPServerAccess, removeOnly bool,
) (*mcpServerStep, error) {
	if sdk == nil || orgID == uuid.Nil || configID == uuid.Nil {
		return nil, fmt.Errorf("assertion failed: MCP server ACL sync needs a Coder client, an organization ID, and a config ID")
	}
	if access == nil {
		return nil, nil
	}
	want, step := resolveMCPServerGroups(ctx, sdk, orgID, configID, access.Groups)
	if step != nil {
		return step, nil
	}
	acl, err := sdk.MCPServerConfigACL(ctx, orgID, configID)
	if err != nil {
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "get the ACL of MCP server config %s: %s", configID, coderErrorSummary(err)), nil
	}
	patch := codersdk.UpdateMCPServerConfigACLRequest{GroupRoles: map[string]codersdk.MCPServerConfigRole{}, UserRoles: map[string]codersdk.MCPServerConfigRole{}}
	for _, g := range acl.Groups {
		if g.ID == uuid.Nil || g.OrganizationID != orgID {
			return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch",
				"assertion failed: Coder answered an ACL group %s of organization %s for MCP server config %s", g.ID, g.OrganizationID, configID), nil
		}
		if _, ok := want[g.ID]; !ok {
			patch.GroupRoles[g.ID.String()] = codersdk.MCPServerConfigRoleDeleted
		} else if g.Role != codersdk.MCPServerConfigRoleRead {
			patch.GroupRoles[g.ID.String()] = codersdk.MCPServerConfigRoleRead
		}
		delete(want, g.ID)
	}
	if !removeOnly {
		for id := range want {
			patch.GroupRoles[id.String()] = codersdk.MCPServerConfigRoleRead
		}
	}
	// access lists groups only, so every user entry goes.
	for _, u := range acl.Users {
		if u.ID == uuid.Nil {
			return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch", "assertion failed: Coder answered an ACL user without an ID for MCP server config %s", configID), nil
		}
		patch.UserRoles[u.ID.String()] = codersdk.MCPServerConfigRoleDeleted
	}
	if len(patch.GroupRoles) == 0 && len(patch.UserRoles) == 0 {
		return nil, nil
	}
	err = sdk.UpdateMCPServerConfigACL(ctx, orgID, configID, patch)
	switch {
	case coderStatus(err) == http.StatusBadRequest:
		// For example a group deleted after the lookup. The resync resolves
		// the names again.
		return mcpServerStalled(mcpServerResync, "CoderRejected", "Coder rejected the ACL update: %s", coderErrorSummary(err)), nil
	case err != nil:
		// A sparse PATCH sets absolute roles, so sending it again is safe.
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "update the ACL of MCP server config %s: %s", configID, coderErrorSummary(err)), nil
	}
	return nil, nil
}

// resolveMCPServerGroups maps group names to IDs. Everyone needs no call.
// Other names go to acl/available as q with no limit, because Coder applies
// the limit to groups but no offset (coderd/mcp_acl.go:127-132). Coder
// matches q loosely, so only a group with exactly the name counts. Group names
// are unique per organization with case (coderd/database/dump.sql:4502).
func resolveMCPServerGroups(
	ctx context.Context, sdk *codersdk.Client, orgID, configID uuid.UUID, names []string,
) (map[uuid.UUID]struct{}, *mcpServerStep) {
	ids := make(map[uuid.UUID]struct{}, len(names))
	var missing []string
	for _, name := range names {
		if name == mcpServerEveryoneGroup {
			ids[orgID] = struct{}{}
			continue
		}
		// Admission checks the same rule. Repeat it, so that q never carries
		// search syntax when admission was bypassed.
		if len(name) > mcpServerGroupNameMaxLength || !mcpServerGroupNamePattern.MatchString(name) {
			return nil, mcpServerStalled(0, "GroupNotFound", "access.groups name %q is not a Coder group name.", name)
		}
		available, err := sdk.MCPServerConfigACLAvailable(ctx, orgID, configID, codersdk.UsersRequest{SearchQuery: name})
		switch {
		case coderStatus(err) == http.StatusBadRequest:
			return nil, mcpServerStalled(mcpServerResync, "GroupNotFound", "Coder rejected the group search for %q: %s", name, coderErrorSummary(err))
		case err != nil:
			return nil, mcpServerWait(mcpServerRetry, "CoderUnavailable", "search group %q: %s", name, coderErrorSummary(err))
		}
		var found []uuid.UUID
		for _, g := range available.Groups {
			if g.OrganizationID != orgID {
				return nil, mcpServerStalled(mcpServerResync, "CoderAnswerMismatch",
					"assertion failed: Coder answered group %s of organization %s when asked for organization %s", g.ID, g.OrganizationID, orgID)
			}
			if g.Name == name {
				found = append(found, g.ID)
			}
		}
		switch {
		case len(found) == 0:
			missing = append(missing, name)
		case len(found) > 1 || found[0] == uuid.Nil:
			return nil, mcpServerStalled(mcpServerResync, "CoderAnswerMismatch",
				"assertion failed: Coder answered %d groups named %q, or one without an ID", len(found), name)
		default:
			ids[found[0]] = struct{}{}
		}
	}
	if len(missing) > 0 {
		return nil, mcpServerStalled(mcpServerResync, "GroupNotFound", "Coder organization has no group named %s.", strings.Join(missing, ", "))
	}
	return ids, nil
}
