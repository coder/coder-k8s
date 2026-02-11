package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

func TestTemplateStorageCRUDWithCoderSDK(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	listObj, err := templateStorage.List(ctx, nil)
	if err != nil {
		t.Fatalf("expected template list to succeed: %v", err)
	}

	list, ok := listObj.(*aggregationv1alpha1.CoderTemplateList)
	if !ok {
		t.Fatalf("expected *CoderTemplateList, got %T", listObj)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected one template in list, got %d", len(list.Items))
	}
	if list.Items[0].Name != "acme.starter-template" {
		t.Fatalf("expected template name acme.starter-template, got %q", list.Items[0].Name)
	}

	obj, err := templateStorage.Get(ctx, "acme.starter-template", nil)
	if err != nil {
		t.Fatalf("expected template get to succeed: %v", err)
	}

	template, ok := obj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected *CoderTemplate, got %T", obj)
	}
	if template.Spec.Organization != "acme" {
		t.Fatalf("expected organization acme, got %q", template.Spec.Organization)
	}

	versionID := uuid.New()
	createObj := &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "acme.ops-template"},
		Spec: aggregationv1alpha1.CoderTemplateSpec{
			Organization: "acme",
			VersionID:    versionID.String(),
			DisplayName:  "Ops Template",
			Description:  "Operations tooling",
			Icon:         "/icons/ops.png",
		},
	}

	createdObj, err := templateStorage.Create(ctx, createObj, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("expected template create to succeed: %v", err)
	}

	createdTemplate, ok := createdObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected *CoderTemplate from create, got %T", createdObj)
	}
	if createdTemplate.Name != "acme.ops-template" {
		t.Fatalf("expected created template name acme.ops-template, got %q", createdTemplate.Name)
	}
	if createdTemplate.Spec.DisplayName != "Ops Template" {
		t.Fatalf("expected created display name Ops Template, got %q", createdTemplate.Spec.DisplayName)
	}

	if !state.hasTemplate("acme", "ops-template") {
		t.Fatal("expected template to be persisted in mock server state")
	}

	_, deleted, err := templateStorage.Delete(ctx, "acme.ops-template", rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("expected template delete to succeed: %v", err)
	}
	if !deleted {
		t.Fatal("expected delete to report deleted=true")
	}

	_, err = templateStorage.Get(ctx, "acme.ops-template", nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}
}

func TestTemplateStorageListRequiresNamespace(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	templateStorage := NewTemplateStorage(newTestClientProvider(t, server.URL))

	_, err := templateStorage.List(context.Background(), nil)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for missing namespace, got %v", err)
	}
}

func TestWorkspaceStorageCRUDWithCoderSDK(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()

	workspaceStorage := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	listObj, err := workspaceStorage.List(ctx, nil)
	if err != nil {
		t.Fatalf("expected workspace list to succeed: %v", err)
	}

	list, ok := listObj.(*aggregationv1alpha1.CoderWorkspaceList)
	if !ok {
		t.Fatalf("expected *CoderWorkspaceList, got %T", listObj)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected one workspace in list, got %d", len(list.Items))
	}
	if list.Items[0].Name != "acme.alice.dev-workspace" {
		t.Fatalf("expected workspace name acme.alice.dev-workspace, got %q", list.Items[0].Name)
	}

	obj, err := workspaceStorage.Get(ctx, "acme.alice.dev-workspace", nil)
	if err != nil {
		t.Fatalf("expected workspace get to succeed: %v", err)
	}

	workspace, ok := obj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected *CoderWorkspace, got %T", obj)
	}
	if !workspace.Spec.Running {
		t.Fatal("expected initial workspace to be running")
	}

	ttlMillis := int64(7200000)
	autostartSchedule := "CRON_TZ=UTC 0 10 * * 1-5"
	createObj := &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.ops-workspace"},
		Spec: aggregationv1alpha1.CoderWorkspaceSpec{
			Organization:      "acme",
			TemplateName:      "starter-template",
			Running:           false,
			TTLMillis:         &ttlMillis,
			AutostartSchedule: &autostartSchedule,
		},
	}

	createdObj, err := workspaceStorage.Create(ctx, createObj, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("expected workspace create to succeed: %v", err)
	}

	createdWorkspace, ok := createdObj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected *CoderWorkspace from create, got %T", createdObj)
	}
	if createdWorkspace.Spec.Running {
		t.Fatal("expected created workspace to be stopped when spec.running=false")
	}
	if !state.hasWorkspace("alice", "ops-workspace") {
		t.Fatal("expected workspace to be persisted in mock server state")
	}
	if !containsTransition(state.buildTransitionsSnapshot(), codersdk.WorkspaceTransitionStop) {
		t.Fatal("expected create to queue stop transition when running=false")
	}

	desiredWorkspace := createdWorkspace.DeepCopy()
	desiredWorkspace.Spec.Running = true

	updatedObj, created, err := workspaceStorage.Update(
		ctx,
		desiredWorkspace.Name,
		testUpdatedObjectInfo{obj: desiredWorkspace},
		nil,
		rest.ValidateAllObjectUpdateFunc,
		false,
		nil,
	)
	if err != nil {
		t.Fatalf("expected workspace update to succeed: %v", err)
	}
	if created {
		t.Fatal("expected update created=false")
	}

	updatedWorkspace, ok := updatedObj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected *CoderWorkspace from update, got %T", updatedObj)
	}
	if !updatedWorkspace.Spec.Running {
		t.Fatal("expected updated workspace to be running")
	}
	if !containsTransition(state.buildTransitionsSnapshot(), codersdk.WorkspaceTransitionStart) {
		t.Fatal("expected update to queue start transition")
	}

	_, deleted, err := workspaceStorage.Delete(ctx, desiredWorkspace.Name, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("expected workspace delete to succeed: %v", err)
	}
	if !deleted {
		t.Fatal("expected delete to report deleted=true")
	}
	if !containsTransition(state.buildTransitionsSnapshot(), codersdk.WorkspaceTransitionDelete) {
		t.Fatal("expected delete to queue delete transition")
	}
}

func TestWorkspaceStorageGetOrgMismatchReturnsNotFound(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	workspaceStorage := NewWorkspaceStorage(newTestClientProvider(t, server.URL))
	ctx := namespacedContext("control-plane")

	_, err := workspaceStorage.Get(ctx, "otherorg.alice.dev-workspace", nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound when organization segment mismatches workspace org, got %v", err)
	}
}

func TestWorkspaceStorageListRequiresNamespace(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()

	workspaceStorage := NewWorkspaceStorage(newTestClientProvider(t, server.URL))

	_, err := workspaceStorage.List(context.Background(), nil)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for missing namespace, got %v", err)
	}
}

type testUpdatedObjectInfo struct {
	obj runtime.Object
	err error
}

func (i testUpdatedObjectInfo) Preconditions() *metav1.Preconditions {
	return nil
}

func (i testUpdatedObjectInfo) UpdatedObject(context.Context, runtime.Object) (runtime.Object, error) {
	if i.err != nil {
		return nil, i.err
	}
	if i.obj == nil {
		return nil, fmt.Errorf("assertion failed: updated object must not be nil")
	}

	return i.obj, nil
}

type mockCoderServerState struct {
	mu sync.Mutex

	organization codersdk.Organization

	templatesByID      map[uuid.UUID]codersdk.Template
	templateIDsByOrg   map[string]map[string]uuid.UUID
	workspacesByID     map[uuid.UUID]codersdk.Workspace
	workspaceIDsByUser map[string]map[string]uuid.UUID

	buildTransitions []codersdk.WorkspaceTransition
}

func newMockCoderServer(t *testing.T) (*httptest.Server, *mockCoderServerState) {
	t.Helper()

	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	orgID := uuid.New()
	templateID := uuid.New()
	activeVersionID := uuid.New()
	workspaceID := uuid.New()
	workspaceBuildID := uuid.New()
	ttlMillis := int64(3600000)
	autostartSchedule := "CRON_TZ=UTC 0 9 * * 1-5"

	organization := codersdk.Organization{
		MinimalOrganization: codersdk.MinimalOrganization{
			ID:          orgID,
			Name:        "acme",
			DisplayName: "Acme",
		},
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now.Add(-1 * time.Hour),
	}

	template := codersdk.Template{
		ID:               templateID,
		CreatedAt:        now.Add(-12 * time.Hour),
		UpdatedAt:        now.Add(-2 * time.Hour),
		OrganizationID:   orgID,
		OrganizationName: "acme",
		Name:             "starter-template",
		DisplayName:      "Starter Template",
		Description:      "Default development template",
		Icon:             "/icons/starter.png",
		ActiveVersionID:  activeVersionID,
	}

	workspace := codersdk.Workspace{
		ID:                workspaceID,
		CreatedAt:         now.Add(-8 * time.Hour),
		UpdatedAt:         now.Add(-30 * time.Minute),
		OwnerName:         "alice",
		OrganizationID:    orgID,
		OrganizationName:  "acme",
		TemplateID:        templateID,
		TemplateName:      "starter-template",
		Name:              "dev-workspace",
		TTLMillis:         &ttlMillis,
		AutostartSchedule: &autostartSchedule,
		LastUsedAt:        now.Add(-10 * time.Minute),
		LatestBuild: codersdk.WorkspaceBuild{
			ID:                 workspaceBuildID,
			WorkspaceID:        workspaceID,
			WorkspaceName:      "dev-workspace",
			WorkspaceOwnerName: "alice",
			TemplateVersionID:  activeVersionID,
			Transition:         codersdk.WorkspaceTransitionStart,
			Status:             codersdk.WorkspaceStatusRunning,
			CreatedAt:          now.Add(-30 * time.Minute),
			UpdatedAt:          now.Add(-30 * time.Minute),
		},
	}

	state := &mockCoderServerState{
		organization: organization,
		templatesByID: map[uuid.UUID]codersdk.Template{
			template.ID: template,
		},
		templateIDsByOrg: map[string]map[string]uuid.UUID{
			"acme": {
				template.Name: template.ID,
			},
		},
		workspacesByID: map[uuid.UUID]codersdk.Workspace{
			workspace.ID: workspace,
		},
		workspaceIDsByUser: map[string]map[string]uuid.UUID{
			"alice": {
				workspace.Name: workspace.ID,
			},
		},
		buildTransitions: []codersdk.WorkspaceTransition{},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handleRequest(t, w, r)
	}))

	return server, state
}

func (s *mockCoderServerState) handleRequest(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	segments := splitPath(r.URL.Path)

	switch {
	case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "organizations") && len(segments) == 4:
		s.handleGetOrganization(w, segments[3])
		return
	case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "templates") && len(segments) == 3:
		s.handleListTemplates(w)
		return
	case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "organizations") && len(segments) == 6 && segments[4] == "templates":
		s.handleGetTemplateByName(w, segments[3], segments[5])
		return
	case r.Method == http.MethodPost && hasSegments(segments, "api", "v2", "organizations") && len(segments) == 5 && segments[4] == "templates":
		s.handleCreateTemplate(w, r, segments[3])
		return
	case r.Method == http.MethodDelete && hasSegments(segments, "api", "v2", "templates") && len(segments) == 4:
		s.handleDeleteTemplate(w, segments[3])
		return
	case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "workspaces") && len(segments) == 3:
		s.handleListWorkspaces(w)
		return
	case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "users") && len(segments) == 6 && segments[4] == "workspace":
		s.handleGetWorkspace(w, segments[3], segments[5])
		return
	case r.Method == http.MethodPost && hasSegments(segments, "api", "v2", "users") && len(segments) == 5 && segments[4] == "workspaces":
		s.handleCreateWorkspace(w, r, segments[3])
		return
	case r.Method == http.MethodPost && hasSegments(segments, "api", "v2", "workspaces") && len(segments) == 5 && segments[4] == "builds":
		s.handleCreateWorkspaceBuild(w, r, segments[3])
		return
	default:
		writeCoderError(w, http.StatusNotFound, fmt.Sprintf("unexpected route: %s %s", r.Method, r.URL.Path))
		return
	}
}

func (s *mockCoderServerState) handleGetOrganization(w http.ResponseWriter, orgSegment string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if orgSegment != s.organization.Name && orgSegment != s.organization.ID.String() {
		writeCoderError(w, http.StatusNotFound, "organization not found")
		return
	}

	writeJSON(w, http.StatusOK, s.organization)
}

func (s *mockCoderServerState) handleListTemplates(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()

	templates := make([]codersdk.Template, 0, len(s.templatesByID))
	for _, template := range s.templatesByID {
		templates = append(templates, template)
	}
	sort.Slice(templates, func(i, j int) bool {
		if templates[i].OrganizationName == templates[j].OrganizationName {
			return templates[i].Name < templates[j].Name
		}
		return templates[i].OrganizationName < templates[j].OrganizationName
	})

	writeJSON(w, http.StatusOK, templates)
}

func (s *mockCoderServerState) handleGetTemplateByName(w http.ResponseWriter, orgSegment, templateName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if orgSegment != s.organization.Name && orgSegment != s.organization.ID.String() {
		writeCoderError(w, http.StatusNotFound, "organization not found")
		return
	}

	orgTemplates, ok := s.templateIDsByOrg[s.organization.Name]
	if !ok {
		writeCoderError(w, http.StatusNotFound, "template not found")
		return
	}
	templateID, ok := orgTemplates[templateName]
	if !ok {
		writeCoderError(w, http.StatusNotFound, "template not found")
		return
	}
	template := s.templatesByID[templateID]

	writeJSON(w, http.StatusOK, template)
}

func (s *mockCoderServerState) handleCreateTemplate(w http.ResponseWriter, r *http.Request, orgSegment string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if orgSegment != s.organization.Name && orgSegment != s.organization.ID.String() {
		writeCoderError(w, http.StatusNotFound, "organization not found")
		return
	}

	var request codersdk.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeCoderError(w, http.StatusBadRequest, fmt.Sprintf("decode create template request: %v", err))
		return
	}

	now := time.Now().UTC()
	template := codersdk.Template{
		ID:               uuid.New(),
		CreatedAt:        now,
		UpdatedAt:        now,
		OrganizationID:   s.organization.ID,
		OrganizationName: s.organization.Name,
		Name:             request.Name,
		DisplayName:      request.DisplayName,
		Description:      request.Description,
		Icon:             request.Icon,
		ActiveVersionID:  request.VersionID,
	}

	s.templatesByID[template.ID] = template
	orgTemplates, ok := s.templateIDsByOrg[s.organization.Name]
	if !ok {
		orgTemplates = map[string]uuid.UUID{}
		s.templateIDsByOrg[s.organization.Name] = orgTemplates
	}
	orgTemplates[template.Name] = template.ID

	writeJSON(w, http.StatusCreated, template)
}

func (s *mockCoderServerState) handleDeleteTemplate(w http.ResponseWriter, templateIDSegment string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	templateID, err := uuid.Parse(templateIDSegment)
	if err != nil {
		writeCoderError(w, http.StatusBadRequest, fmt.Sprintf("invalid template id %q", templateIDSegment))
		return
	}

	template, ok := s.templatesByID[templateID]
	if !ok {
		writeCoderError(w, http.StatusNotFound, "template not found")
		return
	}

	delete(s.templatesByID, templateID)
	orgTemplates := s.templateIDsByOrg[template.OrganizationName]
	delete(orgTemplates, template.Name)

	writeJSON(w, http.StatusOK, map[string]string{"message": "template deleted"})
}

func (s *mockCoderServerState) handleListWorkspaces(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()

	workspaces := make([]codersdk.Workspace, 0, len(s.workspacesByID))
	for _, workspace := range s.workspacesByID {
		workspaces = append(workspaces, workspace)
	}
	sort.Slice(workspaces, func(i, j int) bool {
		if workspaces[i].OrganizationName == workspaces[j].OrganizationName {
			if workspaces[i].OwnerName == workspaces[j].OwnerName {
				return workspaces[i].Name < workspaces[j].Name
			}
			return workspaces[i].OwnerName < workspaces[j].OwnerName
		}
		return workspaces[i].OrganizationName < workspaces[j].OrganizationName
	})

	writeJSON(w, http.StatusOK, codersdk.WorkspacesResponse{Workspaces: workspaces, Count: len(workspaces)})
}

func (s *mockCoderServerState) handleGetWorkspace(w http.ResponseWriter, owner, workspaceName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	userWorkspaces, ok := s.workspaceIDsByUser[owner]
	if !ok {
		writeCoderError(w, http.StatusNotFound, "workspace not found")
		return
	}
	workspaceID, ok := userWorkspaces[workspaceName]
	if !ok {
		writeCoderError(w, http.StatusNotFound, "workspace not found")
		return
	}
	workspace := s.workspacesByID[workspaceID]

	writeJSON(w, http.StatusOK, workspace)
}

func (s *mockCoderServerState) handleCreateWorkspace(w http.ResponseWriter, r *http.Request, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var request codersdk.CreateWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeCoderError(w, http.StatusBadRequest, fmt.Sprintf("decode create workspace request: %v", err))
		return
	}

	template, ok := s.templatesByID[request.TemplateID]
	if !ok {
		writeCoderError(w, http.StatusNotFound, "template not found")
		return
	}

	now := time.Now().UTC()
	workspaceID := uuid.New()
	build := codersdk.WorkspaceBuild{
		ID:                 uuid.New(),
		CreatedAt:          now,
		UpdatedAt:          now,
		WorkspaceID:        workspaceID,
		WorkspaceName:      request.Name,
		WorkspaceOwnerName: user,
		TemplateVersionID:  template.ActiveVersionID,
		Transition:         codersdk.WorkspaceTransitionStart,
		Status:             codersdk.WorkspaceStatusRunning,
	}
	workspace := codersdk.Workspace{
		ID:                workspaceID,
		CreatedAt:         now,
		UpdatedAt:         now,
		OwnerName:         user,
		OrganizationID:    template.OrganizationID,
		OrganizationName:  template.OrganizationName,
		TemplateID:        template.ID,
		TemplateName:      template.Name,
		Name:              request.Name,
		TTLMillis:         request.TTLMillis,
		AutostartSchedule: request.AutostartSchedule,
		LastUsedAt:        now,
		LatestBuild:       build,
	}

	s.workspacesByID[workspace.ID] = workspace
	userWorkspaces, ok := s.workspaceIDsByUser[user]
	if !ok {
		userWorkspaces = map[string]uuid.UUID{}
		s.workspaceIDsByUser[user] = userWorkspaces
	}
	userWorkspaces[workspace.Name] = workspace.ID

	writeJSON(w, http.StatusCreated, workspace)
}

func (s *mockCoderServerState) handleCreateWorkspaceBuild(w http.ResponseWriter, r *http.Request, workspaceIDSegment string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	workspaceID, err := uuid.Parse(workspaceIDSegment)
	if err != nil {
		writeCoderError(w, http.StatusBadRequest, fmt.Sprintf("invalid workspace id %q", workspaceIDSegment))
		return
	}

	workspace, ok := s.workspacesByID[workspaceID]
	if !ok {
		writeCoderError(w, http.StatusNotFound, "workspace not found")
		return
	}

	var request codersdk.CreateWorkspaceBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeCoderError(w, http.StatusBadRequest, fmt.Sprintf("decode create workspace build request: %v", err))
		return
	}

	now := time.Now().UTC()
	build := codersdk.WorkspaceBuild{
		ID:                 uuid.New(),
		CreatedAt:          now,
		UpdatedAt:          now,
		WorkspaceID:        workspace.ID,
		WorkspaceName:      workspace.Name,
		WorkspaceOwnerName: workspace.OwnerName,
		TemplateVersionID:  workspace.LatestBuild.TemplateVersionID,
		Transition:         request.Transition,
		Status:             statusFromTransition(request.Transition),
	}

	workspace.LatestBuild = build
	workspace.UpdatedAt = now
	s.workspacesByID[workspace.ID] = workspace
	s.buildTransitions = append(s.buildTransitions, request.Transition)

	writeJSON(w, http.StatusCreated, build)
}

func (s *mockCoderServerState) hasTemplate(organization, templateName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	organizationTemplates, ok := s.templateIDsByOrg[organization]
	if !ok {
		return false
	}
	_, ok = organizationTemplates[templateName]
	return ok
}

func (s *mockCoderServerState) hasWorkspace(owner, workspaceName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	userWorkspaces, ok := s.workspaceIDsByUser[owner]
	if !ok {
		return false
	}
	_, ok = userWorkspaces[workspaceName]
	return ok
}

func (s *mockCoderServerState) buildTransitionsSnapshot() []codersdk.WorkspaceTransition {
	s.mu.Lock()
	defer s.mu.Unlock()

	transitions := make([]codersdk.WorkspaceTransition, len(s.buildTransitions))
	copy(transitions, s.buildTransitions)
	return transitions
}

func newTestClientProvider(t *testing.T, serverURL string) coder.ClientProvider {
	t.Helper()

	parsedURL, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse mock server URL %q: %v", serverURL, err)
	}

	client := codersdk.New(parsedURL)
	client.SetSessionToken("test-session-token")

	return &coder.StaticClientProvider{Client: client}
}

func namespacedContext(namespace string) context.Context {
	return genericapirequest.WithNamespace(context.Background(), namespace)
}

func containsTransition(transitions []codersdk.WorkspaceTransition, transition codersdk.WorkspaceTransition) bool {
	for _, got := range transitions {
		if got == transition {
			return true
		}
	}
	return false
}

func statusFromTransition(transition codersdk.WorkspaceTransition) codersdk.WorkspaceStatus {
	switch transition {
	case codersdk.WorkspaceTransitionStart:
		return codersdk.WorkspaceStatusRunning
	case codersdk.WorkspaceTransitionStop:
		return codersdk.WorkspaceStatusStopped
	case codersdk.WorkspaceTransitionDelete:
		return codersdk.WorkspaceStatusDeleted
	default:
		return codersdk.WorkspaceStatusPending
	}
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "/")
}

func hasSegments(segments []string, expected ...string) bool {
	if len(segments) < len(expected) {
		return false
	}

	for i, segment := range expected {
		if segments[i] != segment {
			return false
		}
	}

	return true
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeCoderError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, codersdk.Response{Message: message})
}
