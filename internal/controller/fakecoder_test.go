package controller_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// fakeCoderToken is the only session token the fake Coder accepts. It equals
// the token that createTestControlPlane stores.
const fakeCoderToken = "operator-session-token"

// Route names for fakeCoder.failNext and fakeCoder.requestCount.
const (
	routeUser            = "user"
	routeOrganization    = "organization"
	routeOrgMember       = "organizationMember"
	routeTemplateByName  = "templateByName"
	routeTemplate        = "template"
	routeVersionByName   = "templateVersionByName"
	routeVersion         = "templateVersion"
	routeCreateWorkspace = "createWorkspace"
	routeWorkspaceByName = "workspaceByOwnerAndName"
	routeWorkspace       = "workspace"
	routeWorkspaceBuilds = "workspaceBuilds"
	routeCreateBuild     = "createWorkspaceBuild"
	routeCancelBuild     = "cancelWorkspaceBuild"
)

// fakeFault is one injected failure for the next request on a route.
type fakeFault struct {
	Status      int  // HTTP status of the error answer.
	AfterCommit bool // Apply the route's state change before the fault fires.
	Hang        bool // Block until the client gives up (its own timeout).
	Reset       bool // Close the TCP connection without an answer.
}

type fakeWorkspace struct {
	workspace codersdk.Workspace
	builds    []codersdk.WorkspaceBuild // Oldest first.
	deleted   bool
}

// fakeCoder is a stateful in-memory Coder API for CoderTemplateTest controller
// tests. It serves only the routes the controller uses, through the real
// codersdk client, and mirrors the Coder v2.37.2 answers that the controller's
// crash-safety rules depend on. It leaves WorkspaceBuild.Status empty because
// the controller reads only Job.Status.
type fakeCoder struct {
	t      *testing.T
	server *httptest.Server
	done   chan struct{}

	mu         sync.Mutex
	now        func() time.Time
	operatorID uuid.UUID
	users      map[uuid.UUID]codersdk.User
	orgs       map[uuid.UUID]codersdk.Organization
	orgRoles   map[uuid.UUID]map[uuid.UUID][]codersdk.SlimRole // Organization -> user -> roles.
	templates  map[uuid.UUID]codersdk.Template
	versions   map[uuid.UUID]codersdk.TemplateVersion
	workspaces map[uuid.UUID]*fakeWorkspace
	faults     map[string][]fakeFault
	requests   map[string]int // Route -> requests received.
}

func newFakeCoder(t *testing.T) *fakeCoder {
	t.Helper()
	f := &fakeCoder{
		t: t, done: make(chan struct{}), now: time.Now,
		users: map[uuid.UUID]codersdk.User{}, orgs: map[uuid.UUID]codersdk.Organization{},
		orgRoles: map[uuid.UUID]map[uuid.UUID][]codersdk.SlimRole{}, templates: map[uuid.UUID]codersdk.Template{},
		versions: map[uuid.UUID]codersdk.TemplateVersion{}, workspaces: map[uuid.UUID]*fakeWorkspace{},
		faults: map[string][]fakeFault{}, requests: map[string]int{},
	}
	f.operatorID = f.addUser("operator", codersdk.LoginTypePassword, codersdk.RoleOwner)

	mux := http.NewServeMux()
	f.route(mux, "GET /api/v2/users/{user}", routeUser, func(r *http.Request) (int, any) {
		return found(f.userByIdent(r.PathValue("user")))
	})
	f.route(mux, "GET /api/v2/organizations/{org}", routeOrganization, func(r *http.Request) (int, any) {
		return found(f.orgByIdent(r.PathValue("org")))
	})
	f.route(mux, "GET /api/v2/organizations/{org}/members/{user}", routeOrgMember, f.getOrgMember)
	f.route(mux, "GET /api/v2/organizations/{org}/templates/{name}", routeTemplateByName, func(r *http.Request) (int, any) {
		return found(findIn(f.templates, func(t codersdk.Template) bool {
			return t.OrganizationID.String() == r.PathValue("org") && strings.EqualFold(t.Name, r.PathValue("name"))
		}))
	})
	f.route(mux, "GET /api/v2/templates/{id}", routeTemplate, func(r *http.Request) (int, any) {
		return found(findIn(f.templates, func(t codersdk.Template) bool { return t.ID.String() == r.PathValue("id") }))
	})
	f.route(mux, "GET /api/v2/templates/{id}/versions/{name}", routeVersionByName, func(r *http.Request) (int, any) {
		return found(findIn(f.versions, func(v codersdk.TemplateVersion) bool {
			return v.TemplateID.String() == r.PathValue("id") && v.Name == r.PathValue("name")
		}))
	})
	f.route(mux, "GET /api/v2/templateversions/{id}", routeVersion, func(r *http.Request) (int, any) {
		return found(findIn(f.versions, func(v codersdk.TemplateVersion) bool { return v.ID.String() == r.PathValue("id") }))
	})
	f.route(mux, "POST /api/v2/users/{user}/workspaces", routeCreateWorkspace, f.createWorkspace)
	f.route(mux, "GET /api/v2/users/{user}/workspace/{name}", routeWorkspaceByName, f.getWorkspaceByOwnerAndName)
	f.route(mux, "GET /api/v2/workspaces/{id}", routeWorkspace, f.getWorkspace)
	f.route(mux, "GET /api/v2/workspaces/{id}/builds", routeWorkspaceBuilds, f.listBuilds)
	f.route(mux, "POST /api/v2/workspaces/{id}/builds", routeCreateBuild, f.createBuild)
	f.route(mux, "PATCH /api/v2/workspacebuilds/{id}/cancel", routeCancelBuild, f.cancelBuild)

	f.server = httptest.NewServer(mux)
	t.Cleanup(func() {
		close(f.done) // Releases hanging handlers before Close waits for them.
		f.server.Close()
	})
	return f
}

// client returns an SDK client built the way the controller builds it.
func (f *fakeCoder) client(t *testing.T, timeout time.Duration) *codersdk.Client {
	t.Helper()
	u, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	c, err := coder.NewSDKClient(coder.Config{CoderURL: u, SessionToken: fakeCoderToken, RequestTimeout: timeout})
	require.NoError(t, err)
	return c
}

func (f *fakeCoder) route(mux *http.ServeMux, pattern, route string, h func(*http.Request) (int, any)) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests[route]++
		if r.Header.Get(codersdk.SessionTokenHeader) != fakeCoderToken {
			f.mu.Unlock()
			writeFakeJSON(w, http.StatusUnauthorized, codersdk.Response{Message: "You must be logged in."})
			return
		}
		var fault *fakeFault
		if queue := f.faults[route]; len(queue) > 0 {
			fault, f.faults[route] = &queue[0], queue[1:]
		}
		status, resp := 0, any(nil)
		if fault == nil || fault.AfterCommit {
			status, resp = h(r)
		}
		f.mu.Unlock()

		switch {
		case fault == nil:
			writeFakeJSON(w, status, resp)
		case fault.Hang:
			select {
			case <-r.Context().Done():
			case <-f.done:
			}
		case fault.Reset:
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				f.t.Errorf("fake coder: hijack for reset: %v", err)
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0) // Send RST instead of FIN.
			}
			_ = conn.Close()
		default:
			writeFakeJSON(w, fault.Status, codersdk.Response{Message: fmt.Sprintf("fake fault %d", fault.Status)})
		}
	})
}

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fakeError(status int, format string, args ...any) (int, any) {
	return status, codersdk.Response{Message: fmt.Sprintf(format, args...)}
}

func found[T any](v T, ok bool) (int, any) {
	if !ok {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	return http.StatusOK, v
}

func findIn[T any](m map[uuid.UUID]T, match func(T) bool) (T, bool) {
	for _, v := range m {
		if match(v) {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// Setup helpers lock the fake, so tests can call them while a controller
// talks to the server.

func (f *fakeCoder) addUser(username string, loginType codersdk.LoginType, siteRoles ...string) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := codersdk.User{Roles: []codersdk.SlimRole{}, OrganizationIDs: []uuid.UUID{}}
	u.ID, u.Username, u.Email = uuid.New(), username, username+"@example.com"
	u.Status, u.LoginType = codersdk.UserStatusActive, loginType
	for _, role := range siteRoles {
		u.Roles = append(u.Roles, codersdk.SlimRole{Name: role})
	}
	f.users[u.ID] = u
	return u.ID
}

func (f *fakeCoder) addOrganization(name string) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := codersdk.Organization{}
	o.ID, o.Name = uuid.New(), name
	f.orgs[o.ID] = o
	f.orgRoles[o.ID] = map[uuid.UUID][]codersdk.SlimRole{}
	return o.ID
}

// addMember makes a user a member of an organization with roles.
func (f *fakeCoder) addMember(orgID, userID uuid.UUID, roles ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	require.True(f.t, ok, "assertion failed: unknown user %s", userID)
	require.Contains(f.t, f.orgRoles, orgID, "assertion failed: unknown organization")
	slim := []codersdk.SlimRole{}
	for _, role := range roles {
		slim = append(slim, codersdk.SlimRole{Name: role, OrganizationID: orgID.String()})
	}
	f.orgRoles[orgID][userID] = slim
	u.OrganizationIDs = append(u.OrganizationIDs, orgID)
	f.users[userID] = u
}

func (f *fakeCoder) addTemplate(orgID uuid.UUID, name string) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	org, ok := f.orgs[orgID]
	require.True(f.t, ok, "assertion failed: unknown organization %s", orgID)
	tpl := codersdk.Template{ID: uuid.New(), Name: name, OrganizationID: orgID, OrganizationName: org.Name}
	f.templates[tpl.ID] = tpl
	return tpl.ID
}

// addVersion adds a version whose import job has status job. The first
// version of a template becomes the active version.
func (f *fakeCoder) addVersion(templateID uuid.UUID, name string, job codersdk.ProvisionerJobStatus) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	tpl, ok := f.templates[templateID]
	require.True(f.t, ok, "assertion failed: unknown template %s", templateID)
	v := codersdk.TemplateVersion{
		ID: uuid.New(), TemplateID: &templateID, OrganizationID: tpl.OrganizationID, Name: name,
		Job: codersdk.ProvisionerJob{ID: uuid.New(), Status: job},
	}
	f.versions[v.ID] = v
	if tpl.ActiveVersionID == uuid.Nil {
		tpl.ActiveVersionID = v.ID
		f.templates[templateID] = tpl
	}
	return v.ID
}

func (f *fakeCoder) archiveVersion(versionID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[versionID]
	require.True(f.t, ok, "assertion failed: unknown version %s", versionID)
	v.Archived = true
	f.versions[versionID] = v
}

// updateUser edits a user, for example to suspend it.
func (f *fakeCoder) updateUser(userID uuid.UUID, edit func(*codersdk.User)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	require.True(f.t, ok, "assertion failed: unknown user %s", userID)
	edit(&u)
	f.users[userID] = u
}

// setVersionJob moves a version's import job to status.
func (f *fakeCoder) setVersionJob(versionID uuid.UUID, status codersdk.ProvisionerJobStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[versionID]
	require.True(f.t, ok, "assertion failed: unknown version %s", versionID)
	v.Job.Status = status
	f.versions[versionID] = v
}

// promoteVersion makes a version the active version of its template.
func (f *fakeCoder) promoteVersion(versionID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[versionID]
	require.True(f.t, ok, "assertion failed: unknown version %s", versionID)
	tpl := f.templates[*v.TemplateID]
	tpl.ActiveVersionID = versionID
	f.templates[tpl.ID] = tpl
}

// setBuildJob moves a build's job to status. A succeeded delete build deletes
// the workspace, which frees its name.
func (f *fakeCoder) setBuildJob(buildID uuid.UUID, status codersdk.ProvisionerJobStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fw, b := f.findBuild(buildID)
	require.NotNil(f.t, b, "assertion failed: unknown build %s", buildID)
	f.setJob(b, status)
	if status == codersdk.ProvisionerJobSucceeded && b.Transition == codersdk.WorkspaceTransitionDelete {
		fw.deleted = true
	}
}

// setAgents gives a build one resource that holds agents.
func (f *fakeCoder) setAgents(buildID uuid.UUID, agents ...codersdk.WorkspaceAgent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, b := f.findBuild(buildID)
	require.NotNil(f.t, b, "assertion failed: unknown build %s", buildID)
	b.Resources = []codersdk.WorkspaceResource{{
		ID: uuid.New(), Name: "main",
		Agents: append([]codersdk.WorkspaceAgent(nil), agents...), // Later caller edits must not change the fake.
	}}
}

// markDeleted deletes a workspace out of band, which frees its name.
func (f *fakeCoder) markDeleted(workspaceID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fw, ok := f.workspaces[workspaceID]
	require.True(f.t, ok, "assertion failed: unknown workspace %s", workspaceID)
	fw.deleted = true
}

// failNext queues fault for the next authenticated request on route.
func (f *fakeCoder) failNext(route string, fault fakeFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[route] = append(f.faults[route], fault)
}

func (f *fakeCoder) requestCount(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[route]
}

func (f *fakeCoder) workspaceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.workspaces)
}

// The helpers and handlers below run with f.mu held.

func (f *fakeCoder) userByIdent(ident string) (codersdk.User, bool) {
	if ident == codersdk.Me {
		ident = f.operatorID.String()
	}
	return findIn(f.users, func(u codersdk.User) bool { return u.ID.String() == ident || strings.EqualFold(u.Username, ident) })
}

// orgByIdent matches names case-insensitively, like Coder v2.37.2
// (GetOrganizationByName compares LOWER(name)).
func (f *fakeCoder) orgByIdent(ident string) (codersdk.Organization, bool) {
	return findIn(f.orgs, func(o codersdk.Organization) bool { return o.ID.String() == ident || strings.EqualFold(o.Name, ident) })
}

func (f *fakeCoder) findBuild(buildID uuid.UUID) (*fakeWorkspace, *codersdk.WorkspaceBuild) {
	for _, fw := range f.workspaces {
		for i := range fw.builds {
			if fw.builds[i].ID == buildID {
				return fw, &fw.builds[i]
			}
		}
	}
	return nil, nil
}

func (f *fakeCoder) setJob(b *codersdk.WorkspaceBuild, status codersdk.ProvisionerJobStatus) {
	now := f.now()
	b.Job.Status = status
	if !status.Active() {
		b.Job.CompletedAt = &now
	}
	// Coder v2.37.2 sets CanceledAt on every cancel. CompletedAt waits until
	// the worker releases a running job (patchCancelWorkspaceBuild).
	if status == codersdk.ProvisionerJobCanceling || (status == codersdk.ProvisionerJobCanceled && b.Job.CanceledAt == nil) {
		b.Job.CanceledAt = &now
	}
}

func (f *fakeCoder) appendBuild(fw *fakeWorkspace, versionID uuid.UUID, transition codersdk.WorkspaceTransition) codersdk.WorkspaceBuild {
	now := f.now()
	b := codersdk.WorkspaceBuild{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, WorkspaceID: fw.workspace.ID,
		WorkspaceName: fw.workspace.Name, WorkspaceOwnerID: fw.workspace.OwnerID, WorkspaceOwnerName: fw.workspace.OwnerName,
		TemplateVersionID: versionID, TemplateVersionName: f.versions[versionID].Name,
		BuildNumber: int32(len(fw.builds) + 1), //nolint:gosec // Tests create a handful of builds.
		Transition:  transition,
		InitiatorID: f.operatorID, // The operator token makes every request.
		Job:         codersdk.ProvisionerJob{ID: uuid.New(), CreatedAt: now, Status: codersdk.ProvisionerJobPending},
		Resources:   []codersdk.WorkspaceResource{},
	}
	fw.builds = append(fw.builds, b)
	return b
}

func (fw *fakeWorkspace) view() codersdk.Workspace {
	ws := fw.workspace
	ws.LatestBuild = fw.builds[len(fw.builds)-1]
	return ws
}

func (f *fakeCoder) liveWorkspace(ownerID uuid.UUID, name string) *fakeWorkspace {
	for _, fw := range f.workspaces {
		if !fw.deleted && fw.workspace.OwnerID == ownerID && strings.EqualFold(fw.workspace.Name, name) {
			return fw
		}
	}
	return nil
}

func (f *fakeCoder) workspaceByID(r *http.Request) *fakeWorkspace {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return nil
	}
	return f.workspaces[id]
}

func (f *fakeCoder) getOrgMember(r *http.Request) (int, any) {
	o, okOrg := f.orgByIdent(r.PathValue("org"))
	u, okUser := f.userByIdent(r.PathValue("user"))
	roles, okMember := f.orgRoles[o.ID][u.ID]
	if !okOrg || !okUser || !okMember {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	m := codersdk.OrganizationMemberWithUserData{
		Username: u.Username, Email: u.Email, Status: u.Status, LoginType: u.LoginType,
		IsServiceAccount: u.IsServiceAccount, GlobalRoles: u.Roles,
	}
	m.UserID, m.OrganizationID, m.Roles = u.ID, o.ID, roles
	return http.StatusOK, m
}

// createWorkspace mirrors Coder v2.37.2 for a request that sends only
// template_version_id: archived 500, import running 406, import failed 400,
// live name taken 409 (case-insensitive).
func (f *fakeCoder) createWorkspace(r *http.Request) (int, any) {
	owner, ok := f.userByIdent(r.PathValue("user"))
	if !ok {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	var req codersdk.CreateWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fakeError(http.StatusBadRequest, "decode request: %v", err)
	}
	v, ok := f.versions[req.TemplateVersionID]
	switch {
	case req.TemplateID != uuid.Nil || !ok:
		return fakeError(http.StatusBadRequest, "fake coder needs a known template_version_id only")
	case v.Archived:
		return fakeError(http.StatusInternalServerError, "template version is archived")
	case v.Job.Status == codersdk.ProvisionerJobPending || v.Job.Status == codersdk.ProvisionerJobRunning:
		return fakeError(http.StatusNotAcceptable, "template version import is still running")
	case v.Job.Status != codersdk.ProvisionerJobSucceeded:
		return fakeError(http.StatusBadRequest, "template version import failed")
	case f.liveWorkspace(owner.ID, req.Name) != nil:
		return fakeError(http.StatusConflict, "workspace %q already exists", req.Name)
	}
	tpl, now := f.templates[*v.TemplateID], f.now()
	fw := &fakeWorkspace{workspace: codersdk.Workspace{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, OwnerID: owner.ID, OwnerName: owner.Username,
		OrganizationID: tpl.OrganizationID, OrganizationName: tpl.OrganizationName,
		TemplateID: tpl.ID, TemplateName: tpl.Name, Name: req.Name, AutomaticUpdates: req.AutomaticUpdates,
	}}
	f.appendBuild(fw, v.ID, codersdk.WorkspaceTransitionStart)
	f.workspaces[fw.workspace.ID] = fw
	return http.StatusCreated, fw.view()
}

// getWorkspaceByOwnerAndName prefers the live workspace. With
// include_deleted=true it falls back to the newest deleted one.
func (f *fakeCoder) getWorkspaceByOwnerAndName(r *http.Request) (int, any) {
	owner, ok := f.userByIdent(r.PathValue("user"))
	if !ok {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	name := r.PathValue("name")
	if fw := f.liveWorkspace(owner.ID, name); fw != nil {
		return http.StatusOK, fw.view()
	}
	var newest *fakeWorkspace
	for _, fw := range f.workspaces {
		if r.URL.Query().Get("include_deleted") == "true" && fw.workspace.OwnerID == owner.ID &&
			strings.EqualFold(fw.workspace.Name, name) &&
			(newest == nil || fw.workspace.CreatedAt.After(newest.workspace.CreatedAt)) {
			newest = fw
		}
	}
	if newest == nil {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	return http.StatusOK, newest.view()
}

func (f *fakeCoder) getWorkspace(r *http.Request) (int, any) {
	fw := f.workspaceByID(r)
	switch {
	case fw == nil:
		return fakeError(http.StatusNotFound, "Resource not found.")
	case fw.deleted && r.URL.Query().Get("include_deleted") != "true":
		return fakeError(http.StatusGone, "Workspace was deleted.")
	}
	return http.StatusOK, fw.view()
}

// listBuilds answers newest first. Whether Coder serves the build list of a
// deleted workspace is not verified yet (plan risk R2).
func (f *fakeCoder) listBuilds(r *http.Request) (int, any) {
	fw := f.workspaceByID(r)
	if fw == nil {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	out := make([]codersdk.WorkspaceBuild, 0, len(fw.builds))
	for i := len(fw.builds) - 1; i >= 0; i-- {
		out = append(out, fw.builds[i])
	}
	return http.StatusOK, out
}

// createBuild accepts only delete builds and refuses orphan deletes, so a
// controller that would leak resources fails loudly. Like Coder v2.37.2
// (postWorkspaceBuilds), it accepts a delete build on a deleted workspace.
func (f *fakeCoder) createBuild(r *http.Request) (int, any) {
	fw := f.workspaceByID(r)
	if fw == nil {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	var req codersdk.CreateWorkspaceBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fakeError(http.StatusBadRequest, "decode request: %v", err)
	}
	latest := fw.builds[len(fw.builds)-1]
	switch {
	case req.Transition != codersdk.WorkspaceTransitionDelete || req.Orphan:
		return fakeError(http.StatusBadRequest, "fake coder accepts only non-orphan delete builds")
	case latest.Job.Status.Active():
		return fakeError(http.StatusConflict, "A build is already active.")
	}
	return http.StatusCreated, f.appendBuild(fw, latest.TemplateVersionID, codersdk.WorkspaceTransitionDelete)
}

// cancelBuild mirrors Coder v2.37.2 (coderd/workspacebuilds.go): a completed
// or already canceled job answers 400 before the expect_status check, a
// mismatch answers 412, a pending job is canceled at once, and a running job
// moves to canceling.
func (f *fakeCoder) cancelBuild(r *http.Request) (int, any) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return fakeError(http.StatusBadRequest, "Invalid build ID.")
	}
	_, b := f.findBuild(id)
	if b == nil {
		return fakeError(http.StatusNotFound, "Resource not found.")
	}
	expect := codersdk.ProvisionerJobStatus(r.URL.Query().Get("expect_status"))
	switch {
	case b.Job.Status != codersdk.ProvisionerJobPending && b.Job.Status != codersdk.ProvisionerJobRunning:
		return fakeError(http.StatusBadRequest, "Job is not cancelable.")
	case expect != "" && expect != b.Job.Status:
		return fakeError(http.StatusPreconditionFailed, "Job status is %s.", b.Job.Status)
	case b.Job.Status == codersdk.ProvisionerJobPending:
		f.setJob(b, codersdk.ProvisionerJobCanceled)
	default:
		f.setJob(b, codersdk.ProvisionerJobCanceling)
	}
	return http.StatusOK, codersdk.Response{Message: "Job has been marked as canceled."}
}
