package convert

import (
	"testing"
	"time"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

func TestWorkspaceToK8s(t *testing.T) {
	t.Parallel()

	workspaceID := uuid.New()
	buildID := uuid.New()
	createdAt := time.Date(2025, time.February, 2, 3, 4, 5, 0, time.UTC)
	updatedAt := createdAt.Add(4 * time.Hour)
	lastUsedAt := createdAt.Add(3 * time.Hour)
	ttlMillis := int64(3600000)
	autostartSchedule := "CRON_TZ=UTC 0 9 * * 1-5"

	workspace := codersdk.Workspace{
		ID:                workspaceID,
		CreatedAt:         createdAt,
		UpdatedAt:         updatedAt,
		OwnerName:         "alice",
		OrganizationName:  "acme",
		TemplateName:      "starter-template",
		Name:              "dev-workspace",
		TTLMillis:         &ttlMillis,
		AutostartSchedule: &autostartSchedule,
		LastUsedAt:        lastUsedAt,
		LatestBuild: codersdk.WorkspaceBuild{
			ID:         buildID,
			Transition: codersdk.WorkspaceTransitionStart,
			Status:     codersdk.WorkspaceStatusStarting,
		},
	}

	converted := WorkspaceToK8s("control-plane", workspace)
	if converted == nil {
		t.Fatal("expected non-nil converted workspace")
	}
	if converted.Name != "acme.alice.dev-workspace" {
		t.Fatalf("expected name acme.alice.dev-workspace, got %q", converted.Name)
	}
	if converted.Namespace != "control-plane" {
		t.Fatalf("expected namespace control-plane, got %q", converted.Namespace)
	}
	if converted.Spec.Organization != "acme" {
		t.Fatalf("expected spec organization acme, got %q", converted.Spec.Organization)
	}
	if converted.Spec.TemplateName != "starter-template" {
		t.Fatalf("expected spec template name starter-template, got %q", converted.Spec.TemplateName)
	}
	if !converted.Spec.Running {
		t.Fatal("expected running=true when latest build transition is start")
	}
	if converted.Spec.TTLMillis == nil || *converted.Spec.TTLMillis != ttlMillis {
		t.Fatalf("expected TTL millis %d, got %+v", ttlMillis, converted.Spec.TTLMillis)
	}
	if converted.Spec.AutostartSchedule == nil || *converted.Spec.AutostartSchedule != autostartSchedule {
		t.Fatalf("expected autostart schedule %q, got %+v", autostartSchedule, converted.Spec.AutostartSchedule)
	}
	if converted.Status.ID != workspaceID.String() {
		t.Fatalf("expected status ID %q, got %q", workspaceID.String(), converted.Status.ID)
	}
	if converted.Status.OwnerName != "alice" {
		t.Fatalf("expected status owner name alice, got %q", converted.Status.OwnerName)
	}
	if converted.Status.OrganizationName != "acme" {
		t.Fatalf("expected status organization name acme, got %q", converted.Status.OrganizationName)
	}
	if converted.Status.TemplateName != "starter-template" {
		t.Fatalf("expected status template name starter-template, got %q", converted.Status.TemplateName)
	}
	if converted.Status.LatestBuildID != buildID.String() {
		t.Fatalf("expected status latest build ID %q, got %q", buildID.String(), converted.Status.LatestBuildID)
	}
	if converted.Status.LatestBuildStatus != string(codersdk.WorkspaceStatusStarting) {
		t.Fatalf("expected status latest build status %q, got %q", codersdk.WorkspaceStatusStarting, converted.Status.LatestBuildStatus)
	}
	if converted.Status.LastUsedAt == nil {
		t.Fatal("expected status lastUsedAt to be set")
	}
	if !converted.Status.LastUsedAt.Time.Equal(lastUsedAt) {
		t.Fatalf("expected status lastUsedAt %s, got %s", lastUsedAt, converted.Status.LastUsedAt.Time)
	}
}

func TestWorkspaceToK8sInfersRunningFromBuildStatus(t *testing.T) {
	t.Parallel()

	workspace := codersdk.Workspace{
		ID:               uuid.New(),
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		OwnerName:        "alice",
		OrganizationName: "acme",
		TemplateName:     "starter-template",
		Name:             "dev-workspace",
		LastUsedAt:       time.Now().UTC(),
		LatestBuild: codersdk.WorkspaceBuild{
			ID:         uuid.New(),
			Transition: codersdk.WorkspaceTransitionStop,
			Status:     codersdk.WorkspaceStatusRunning,
		},
	}

	converted := WorkspaceToK8s("control-plane", workspace)
	if !converted.Spec.Running {
		t.Fatal("expected running=true when latest build status is running")
	}
}

func TestWorkspaceCreateRequestFromK8s(t *testing.T) {
	t.Parallel()

	templateID := uuid.New()
	ttlMillis := int64(3600000)
	autostartSchedule := "CRON_TZ=UTC 0 9 * * 1-5"

	obj := &aggregationv1alpha1.CoderWorkspace{
		Spec: aggregationv1alpha1.CoderWorkspaceSpec{
			TTLMillis:         &ttlMillis,
			AutostartSchedule: &autostartSchedule,
		},
	}

	request := WorkspaceCreateRequestFromK8s(obj, "dev-workspace", templateID)
	if request.Name != "dev-workspace" {
		t.Fatalf("expected request name dev-workspace, got %q", request.Name)
	}
	if request.TemplateID != templateID {
		t.Fatalf("expected request template ID %q, got %q", templateID, request.TemplateID)
	}
	if request.TTLMillis == nil || *request.TTLMillis != ttlMillis {
		t.Fatalf("expected request TTL millis %d, got %+v", ttlMillis, request.TTLMillis)
	}
	if request.AutostartSchedule == nil || *request.AutostartSchedule != autostartSchedule {
		t.Fatalf("expected request autostart schedule %q, got %+v", autostartSchedule, request.AutostartSchedule)
	}
}
