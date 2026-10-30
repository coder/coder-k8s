package controller_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Route names for the workspace pool routes of fakeCoder (#153).
const (
	routeExtendWorkspace    = "extendWorkspace"
	routeWorkspaceACL       = "workspaceACL"
	routeUpdateWorkspaceACL = "updateWorkspaceACL"
	routeSharingSettings    = "workspaceSharingSettings"
	routeVersionPresets     = "templateVersionPresets"
	routeWorkspaces         = "workspaces"
)

// fakePoolState is the fake's state for the routes that CoderWorkspacePool
// controllers use. Every handler and helper here mirrors Coder v2.37.3, whose
// handlers for these routes are the same as in v2.37.2 (the Kind E2E image).
type fakePoolState struct {
	presets     map[uuid.UUID][]codersdk.Preset                    // Version -> presets.
	maxDeadline map[uuid.UUID]time.Duration                        // Template -> autostop requirement after build completion.
	acls        map[uuid.UUID]map[uuid.UUID]codersdk.WorkspaceRole // Workspace -> user -> role.
	sharing     map[uuid.UUID]codersdk.ShareableWorkspaceOwners    // Organization -> setting. Missing means everyone.
}

func (f *fakeCoder) registerPoolRoutes(mux *http.ServeMux) {
	f.pool = fakePoolState{
		presets: map[uuid.UUID][]codersdk.Preset{}, maxDeadline: map[uuid.UUID]time.Duration{},
		acls: map[uuid.UUID]map[uuid.UUID]codersdk.WorkspaceRole{}, sharing: map[uuid.UUID]codersdk.ShareableWorkspaceOwners{},
	}
	f.route(mux, "GET /api/v2/workspaces", routeWorkspaces, f.listWorkspaces)
	f.route(mux, "PUT /api/v2/workspaces/{id}/extend", routeExtendWorkspace, f.extendWorkspace)
	f.route(mux, "GET /api/v2/workspaces/{id}/acl", routeWorkspaceACL, f.getWorkspaceACL)
	f.route(mux, "PATCH /api/v2/workspaces/{id}/acl", routeUpdateWorkspaceACL, f.patchWorkspaceACL)
	f.route(mux, "GET /api/v2/organizations/{org}/settings/workspace-sharing", routeSharingSettings, f.getSharingSettings)
	f.route(mux, "GET /api/v2/templateversions/{id}/presets", routeVersionPresets, func(r *http.Request) (int, any) {
		id, err := uuid.Parse(r.PathValue("id"))
		if _, ok := f.versions[id]; err != nil || !ok {
			return fakeError(http.StatusNotFound, "Resource not found.")
		}
		return http.StatusOK, f.pool.presets[id] // Coder answers null for a version without presets.
	})
}

// Setup helpers. Like the ones in fakecoder_test.go, they lock the fake.

// setTemplateSchedule sets the template's default TTL, and the autostop
// requirement (a Premium feature) that caps every build deadline at
// maxDeadline after the build completes. Zero turns either off.
func (f *fakeCoder) setTemplateSchedule(templateID uuid.UUID, defaultTTL, maxDeadline time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tpl, ok := f.templates[templateID]
	require.True(f.t, ok, "assertion failed: unknown template %s", templateID)
	tpl.DefaultTTLMillis, tpl.AllowUserAutostop = defaultTTL.Milliseconds(), true
	f.templates[templateID] = tpl
	f.pool.maxDeadline[templateID] = maxDeadline
}

// addPreset adds a preset to a version and returns its ID.
func (f *fakeCoder) addPreset(versionID uuid.UUID, name string, params ...codersdk.PresetParameter) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Contains(f.t, f.versions, versionID, "assertion failed: unknown version")
	p := codersdk.Preset{ID: uuid.New(), Name: name, Parameters: append([]codersdk.PresetParameter{}, params...)}
	f.pool.presets[versionID] = append(f.pool.presets[versionID], p)
	return p.ID
}

// setSharing sets an organization's shareable_workspace_owners setting.
func (f *fakeCoder) setSharing(orgID uuid.UUID, owners codersdk.ShareableWorkspaceOwners) {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Contains(f.t, f.orgs, orgID, "assertion failed: unknown organization")
	f.pool.sharing[orgID] = owners
}

// markUsed sets last_used_at as Coder's usage tracker does after a session,
// app traffic, or POST /workspaces/{id}/usage.
func (f *fakeCoder) markUsed(workspaceID uuid.UUID, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustWorkspace(workspaceID).workspace.LastUsedAt = at
}

// touchTemplateWorkspaces models a template schedule update with
// update_workspace_last_used_at: one last_used_at on every workspace of the
// template (UpdateTemplateWorkspacesLastUsedAt), and a new template updated_at.
func (f *fakeCoder) touchTemplateWorkspaces(templateID uuid.UUID, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tpl, ok := f.templates[templateID]
	require.True(f.t, ok, "assertion failed: unknown template %s", templateID)
	tpl.UpdatedAt = at
	f.templates[templateID] = tpl
	for _, fw := range f.workspaces {
		if fw.workspace.TemplateID == templateID {
			fw.workspace.LastUsedAt = at
		}
	}
}

// editTemplate models a template edit that writes no last_used_at, for
// example a version promote: only updated_at moves.
func (f *fakeCoder) editTemplate(templateID uuid.UUID, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tpl, ok := f.templates[templateID]
	require.True(f.t, ok, "assertion failed: unknown template %s", templateID)
	tpl.UpdatedAt = at
	f.templates[templateID] = tpl
}

// addTemplateWorkspace adds a workspace of the version's template outside
// any pool, for example a user's own workspace. Its only build is a
// succeeded start or stop, and it was last used at lastUsed.
func (f *fakeCoder) addTemplateWorkspace(versionID uuid.UUID, transition codersdk.WorkspaceTransition, lastUsed time.Time) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[versionID]
	require.True(f.t, ok, "assertion failed: unknown version %s", versionID)
	tpl, owner, now := f.templates[*v.TemplateID], f.users[f.operatorID], f.now()
	fw := &fakeWorkspace{workspace: codersdk.Workspace{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, OwnerID: owner.ID, OwnerName: owner.Username,
		OrganizationID: tpl.OrganizationID, OrganizationName: tpl.OrganizationName,
		TemplateID: tpl.ID, TemplateName: tpl.Name, AutomaticUpdates: codersdk.AutomaticUpdatesNever, LastUsedAt: lastUsed,
	}}
	fw.workspace.Name = "dev-" + fw.workspace.ID.String()[:8]
	f.appendBuild(fw, versionID, transition)
	f.setJob(&fw.builds[0], codersdk.ProvisionerJobSucceeded)
	f.workspaces[fw.workspace.ID] = fw
	return fw.workspace.ID
}

// setDormant sets dormant_at and deleting_at. Leaving dormancy (nil
// dormantAt) also sets last_used_at to now, like UpdateWorkspaceDormantDeletingAt.
func (f *fakeCoder) setDormant(workspaceID uuid.UUID, dormantAt, deletingAt *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fw := f.mustWorkspace(workspaceID)
	fw.workspace.DormantAt, fw.workspace.DeletingAt = dormantAt, deletingAt
	if dormantAt == nil {
		fw.workspace.LastUsedAt = f.now()
	}
}

// addExternalBuild appends a pending build that someone other than the
// operator started, for example an admin's stop or an autostop.
func (f *fakeCoder) addExternalBuild(workspaceID, initiatorID uuid.UUID, transition codersdk.WorkspaceTransition) codersdk.WorkspaceBuild {
	f.mu.Lock()
	defer f.mu.Unlock()
	fw := f.mustWorkspace(workspaceID)
	b := f.appendBuild(fw, fw.builds[len(fw.builds)-1].TemplateVersionID, transition)
	fw.builds[len(fw.builds)-1].InitiatorID, b.InitiatorID = initiatorID, initiatorID
	return b
}

// The helpers and handlers below run with f.mu held.

func (f *fakeCoder) mustWorkspace(workspaceID uuid.UUID) *fakeWorkspace {
	fw, ok := f.workspaces[workspaceID]
	require.True(f.t, ok, "assertion failed: unknown workspace %s", workspaceID)
	return fw
}

func (f *fakeCoder) hasPreset(versionID, presetID uuid.UUID) bool {
	return slices.ContainsFunc(f.pool.presets[versionID], func(p codersdk.Preset) bool { return p.ID == presetID })
}

// ttlError mirrors the checks of validWorkspaceTTLMillis on an explicit
// ttl_ms: after truncation to whole minutes, at least one minute and at most
// 30 days. Coder runs them before the name check.
func ttlError(millis *int64) string {
	if millis == nil || *millis == 0 {
		return ""
	}
	switch d := (time.Duration(*millis) * time.Millisecond).Truncate(time.Minute); {
	case d < time.Minute:
		return "time until shutdown must be at least one minute"
	case d > 30*24*time.Hour:
		return "time until shutdown must be less than 30 days"
	}
	return ""
}

func fakeTTLError(millis *int64) (int, any) {
	return http.StatusBadRequest, codersdk.Response{
		Message:     "Invalid Workspace Time to Shutdown.",
		Validations: []codersdk.ValidationError{{Field: "ttl_ms", Detail: ttlError(millis)}},
	}
}

// createTTL mirrors validWorkspaceTTLMillis for a valid request: an explicit
// ttl_ms is truncated to whole minutes. No ttl_ms, or 0, takes the template's
// default TTL, and a zero default leaves autostop off.
func createTTL(req codersdk.CreateWorkspaceRequest, tpl codersdk.Template) *int64 {
	switch {
	case req.TTLMillis != nil && *req.TTLMillis != 0:
		ttl := (time.Duration(*req.TTLMillis) * time.Millisecond).Truncate(time.Minute).Milliseconds()
		return &ttl
	case tpl.DefaultTTLMillis > 0:
		return &tpl.DefaultTTLMillis
	}
	return nil
}

// applyAutostop sets the deadlines of a succeeded start build like
// schedule.CalculateAutostop: completion plus the workspace TTL, then capped
// at (or, without a TTL, set to) the template's max deadline.
func (f *fakeCoder) applyAutostop(fw *fakeWorkspace, b *codersdk.WorkspaceBuild) {
	completed := *b.Job.CompletedAt
	var deadline, maxDeadline time.Time
	if ttl := fw.workspace.TTLMillis; ttl != nil && *ttl > 0 {
		deadline = completed.Add(time.Duration(*ttl) * time.Millisecond)
	}
	if after := f.pool.maxDeadline[fw.workspace.TemplateID]; after > 0 {
		maxDeadline = completed.Add(after)
		if deadline.IsZero() || maxDeadline.Before(deadline) {
			deadline = maxDeadline
		}
	}
	b.Deadline = codersdk.NewNullTime(deadline, !deadline.IsZero())
	b.MaxDeadline = codersdk.NewNullTime(maxDeadline, !maxDeadline.IsZero())
}

// writableWorkspace returns the workspace for an extend or ACL write. Coder
// finds deleted workspaces too, so a workspace that the fake deleted out of
// band (markDeleted, no delete build) is refused loudly: the fake does not
// model writes to it.
func (f *fakeCoder) writableWorkspace(r *http.Request, aclWrite bool) (*fakeWorkspace, int, any) {
	fw := f.workspaceByID(r)
	if fw == nil {
		status, resp := fakeError(http.StatusNotFound, "Resource not found.")
		return nil, status, resp
	}
	if fw.deleted && (aclWrite || fw.builds[len(fw.builds)-1].Transition != codersdk.WorkspaceTransitionDelete) {
		status, resp := fakeError(http.StatusBadRequest, "fake coder does not model this write to a deleted workspace")
		return nil, status, resp
	}
	return fw, 0, nil
}

// fakeWorkspaceQueryTerm is one term of a workspace search query as codersdk
// sends it: key:"value".
var fakeWorkspaceQueryTerm = regexp.MustCompile(`^([a-z_]+):"([^"]*)"$`)

// listWorkspaces mirrors the workspaces handler without a limit for the terms
// the controllers send: template (a template name in any organization) and
// organization (an ID, or a name that must exist). Coder lowercases the
// query, answers live workspaces only, and counts every match.
func (f *fakeCoder) listWorkspaces(r *http.Request) (int, any) {
	query := r.URL.Query()
	if query.Has("limit") || query.Has("offset") || query.Has("after_id") {
		return fakeError(http.StatusBadRequest, "fake coder does not model workspace pages")
	}
	templateName, orgID := "", uuid.Nil
	for _, term := range strings.Fields(strings.ToLower(query.Get("q"))) {
		m := fakeWorkspaceQueryTerm.FindStringSubmatch(term)
		switch {
		case m != nil && m[1] == "template":
			templateName = m[2]
		case m != nil && m[1] == "organization":
			id, err := uuid.Parse(m[2])
			if err != nil {
				o, ok := f.orgByIdent(m[2])
				if !ok {
					return fakeError(http.StatusBadRequest, "Invalid workspace search query.")
				}
				id = o.ID
			}
			orgID = id
		default:
			return fakeError(http.StatusBadRequest, "fake coder does not model the workspace query term %q", term)
		}
	}
	res := codersdk.WorkspacesResponse{Workspaces: []codersdk.Workspace{}}
	for _, fw := range f.workspaces {
		if !fw.deleted && (templateName == "" || strings.EqualFold(f.templates[fw.workspace.TemplateID].Name, templateName)) &&
			(orgID == uuid.Nil || fw.workspace.OrganizationID == orgID) {
			res.Workspaces = append(res.Workspaces, fw.view())
		}
	}
	res.Count = len(res.Workspaces)
	return http.StatusOK, res
}

// extendWorkspace mirrors putExtendWorkspace.
func (f *fakeCoder) extendWorkspace(r *http.Request) (int, any) {
	fw, status, resp := f.writableWorkspace(r, false)
	if fw == nil {
		return status, resp
	}
	var req codersdk.PutExtendWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fakeError(http.StatusBadRequest, "decode request: %v", err)
	}
	b, newDeadline := &fw.builds[len(fw.builds)-1], req.Deadline.UTC()
	switch {
	case b.Transition != codersdk.WorkspaceTransitionStart:
		return fakeError(http.StatusConflict, "Workspace must be started, current status: %s", b.Transition)
	case b.Job.CompletedAt == nil:
		return fakeError(http.StatusConflict, "Workspace is still building!")
	case !b.Deadline.Valid:
		return fakeError(http.StatusConflict, "Workspace shutdown is manual.")
	case newDeadline.Before(f.now().Add(29 * time.Minute)):
		return fakeError(http.StatusBadRequest, "Cannot extend workspace: new deadline must be at least 30 minutes in the future")
	case newDeadline.Before(*b.Job.CompletedAt):
		return fakeError(http.StatusBadRequest, "Cannot extend workspace: new deadline must be before workspace start time")
	case b.MaxDeadline.Valid && newDeadline.After(b.MaxDeadline.Time):
		return fakeError(http.StatusBadRequest, "Cannot extend workspace beyond max deadline.")
	}
	b.Deadline = codersdk.NewNullTime(newDeadline, true)
	return http.StatusOK, codersdk.Response{Message: "Deadline updated to " + newDeadline.Format(time.RFC3339) + "."}
}

func (f *fakeCoder) getWorkspaceACL(r *http.Request) (int, any) {
	fw := f.workspaceByID(r)
	if fw == nil {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	acl := codersdk.WorkspaceACL{Users: []codersdk.WorkspaceUser{}, Groups: []codersdk.WorkspaceGroup{}}
	for userID, role := range f.pool.acls[fw.workspace.ID] {
		u := f.users[userID]
		acl.Users = append(acl.Users, codersdk.WorkspaceUser{MinimalUser: codersdk.MinimalUser{ID: u.ID, Username: u.Username}, Role: role})
	}
	slices.SortFunc(acl.Users, func(a, b codersdk.WorkspaceUser) int { return strings.Compare(a.Username, b.Username) })
	return http.StatusOK, acl
}

// patchWorkspaceACL mirrors patchWorkspaceACL for the operator, a site owner:
// 403 when the organization's setting is none, 400 for an unknown role or
// user, and 204 otherwise. Site owners pass Coder's own share checks under
// every other setting, so the controller enforces the setting itself.
func (f *fakeCoder) patchWorkspaceACL(r *http.Request) (int, any) {
	fw, status, resp := f.writableWorkspace(r, true)
	if fw == nil {
		return status, resp
	}
	if f.sharingSetting(fw.workspace.OrganizationID) == codersdk.ShareableWorkspaceOwnersNone {
		return fakeError(http.StatusForbidden, "Workspace sharing is disabled for this organization.")
	}
	var req codersdk.UpdateWorkspaceACL
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fakeError(http.StatusBadRequest, "decode request: %v", err)
	}
	if len(req.GroupRoles) > 0 {
		return fakeError(http.StatusBadRequest, "fake coder does not model group ACLs")
	}
	var invalid []codersdk.ValidationError
	for idText, role := range req.UserRoles {
		id, err := uuid.Parse(idText)
		_, known := f.users[id]
		switch {
		case role != codersdk.WorkspaceRoleAdmin && role != codersdk.WorkspaceRoleUse && role != codersdk.WorkspaceRoleDeleted:
			invalid = append(invalid, codersdk.ValidationError{Field: "user_roles", Detail: "role \"" + string(role) + "\" is not a valid workspace role"})
		case err != nil:
			invalid = append(invalid, codersdk.ValidationError{Field: "user_roles", Detail: idText + " is not a valid UUID."})
		case role != codersdk.WorkspaceRoleDeleted && !known:
			invalid = append(invalid, codersdk.ValidationError{Field: "user_roles", Detail: "user with ID " + idText + " does not exist"})
		}
	}
	if len(invalid) > 0 {
		return http.StatusBadRequest, codersdk.Response{Message: "Invalid request to update workspace ACL", Validations: invalid}
	}
	acl := f.pool.acls[fw.workspace.ID]
	if acl == nil {
		acl = map[uuid.UUID]codersdk.WorkspaceRole{}
		f.pool.acls[fw.workspace.ID] = acl
	}
	for idText, role := range req.UserRoles {
		if role == codersdk.WorkspaceRoleDeleted {
			delete(acl, uuid.MustParse(idText))
		} else {
			acl[uuid.MustParse(idText)] = role
		}
	}
	return http.StatusNoContent, nil
}

func (f *fakeCoder) sharingSetting(orgID uuid.UUID) codersdk.ShareableWorkspaceOwners {
	if owners, ok := f.pool.sharing[orgID]; ok {
		return owners
	}
	return codersdk.ShareableWorkspaceOwnersEveryone // The Coder default.
}

func (f *fakeCoder) getSharingSettings(r *http.Request) (int, any) {
	o, ok := f.orgByIdent(r.PathValue("org"))
	if !ok {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	owners := f.sharingSetting(o.ID)
	return http.StatusOK, codersdk.WorkspaceSharingSettings{
		SharingDisabled: owners == codersdk.ShareableWorkspaceOwnersNone, ShareableWorkspaceOwners: owners,
	}
}
