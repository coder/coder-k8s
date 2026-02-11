package convert

import (
	"strconv"
	"strings"
	"testing"
	"time"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

func TestTemplateToK8s(t *testing.T) {
	t.Parallel()

	templateID := uuid.New()
	activeVersionID := uuid.New()
	createdAt := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	updatedAt := createdAt.Add(2 * time.Hour)

	template := codersdk.Template{
		ID:               templateID,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
		OrganizationName: "acme",
		Name:             "starter-template",
		DisplayName:      "Starter Template",
		Description:      "Base development template",
		Icon:             "/icons/starter.png",
		ActiveVersionID:  activeVersionID,
		Deprecated:       true,
	}

	converted := TemplateToK8s("control-plane", template)
	if converted == nil {
		t.Fatal("expected non-nil converted template")
	}
	if converted.Name != "acme.starter-template" {
		t.Fatalf("expected name acme.starter-template, got %q", converted.Name)
	}
	if converted.Namespace != "control-plane" {
		t.Fatalf("expected namespace control-plane, got %q", converted.Namespace)
	}
	expectedResourceVersion := strconv.FormatInt(updatedAt.UnixNano(), 10)
	if converted.ResourceVersion != expectedResourceVersion {
		t.Fatalf(
			"expected resource version %q from updated timestamp, got %q",
			expectedResourceVersion,
			converted.ResourceVersion,
		)
	}
	if converted.Spec.Organization != "acme" {
		t.Fatalf("expected spec organization acme, got %q", converted.Spec.Organization)
	}
	if converted.Spec.VersionID != activeVersionID.String() {
		t.Fatalf("expected spec version ID %q, got %q", activeVersionID.String(), converted.Spec.VersionID)
	}
	if converted.Spec.DisplayName != "Starter Template" {
		t.Fatalf("expected spec display name Starter Template, got %q", converted.Spec.DisplayName)
	}
	if converted.Spec.Description != "Base development template" {
		t.Fatalf("expected spec description Base development template, got %q", converted.Spec.Description)
	}
	if converted.Spec.Icon != "/icons/starter.png" {
		t.Fatalf("expected spec icon /icons/starter.png, got %q", converted.Spec.Icon)
	}
	if converted.Status.ID != templateID.String() {
		t.Fatalf("expected status ID %q, got %q", templateID.String(), converted.Status.ID)
	}
	if converted.Status.OrganizationName != "acme" {
		t.Fatalf("expected status organization name acme, got %q", converted.Status.OrganizationName)
	}
	if converted.Status.ActiveVersionID != activeVersionID.String() {
		t.Fatalf("expected status active version ID %q, got %q", activeVersionID.String(), converted.Status.ActiveVersionID)
	}
	if !converted.Status.Deprecated {
		t.Fatal("expected status deprecated true")
	}
	if converted.Status.UpdatedAt == nil {
		t.Fatal("expected status updatedAt to be set")
	}
	if !converted.Status.UpdatedAt.Time.Equal(updatedAt) {
		t.Fatalf("expected status updatedAt %s, got %s", updatedAt, converted.Status.UpdatedAt.Time)
	}
}

func TestTemplateCreateRequestFromK8s(t *testing.T) {
	t.Parallel()

	versionID := uuid.New()
	obj := &aggregationv1alpha1.CoderTemplate{
		Spec: aggregationv1alpha1.CoderTemplateSpec{
			VersionID:   versionID.String(),
			DisplayName: "Starter Template",
			Description: "Base development template",
			Icon:        "/icons/starter.png",
		},
	}

	request, err := TemplateCreateRequestFromK8s(obj, "starter-template")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if request.Name != "starter-template" {
		t.Fatalf("expected request name starter-template, got %q", request.Name)
	}
	if request.VersionID != versionID {
		t.Fatalf("expected request version ID %q, got %q", versionID, request.VersionID)
	}
	if request.DisplayName != "Starter Template" {
		t.Fatalf("expected request display name Starter Template, got %q", request.DisplayName)
	}
	if request.Description != "Base development template" {
		t.Fatalf("expected request description Base development template, got %q", request.Description)
	}
	if request.Icon != "/icons/starter.png" {
		t.Fatalf("expected request icon /icons/starter.png, got %q", request.Icon)
	}
}

func TestTemplateCreateRequestFromK8sRejectsInvalidVersionID(t *testing.T) {
	t.Parallel()

	obj := &aggregationv1alpha1.CoderTemplate{
		Spec: aggregationv1alpha1.CoderTemplateSpec{VersionID: "not-a-uuid"},
	}

	_, err := TemplateCreateRequestFromK8s(obj, "starter-template")
	if err == nil {
		t.Fatal("expected error for invalid spec.versionID, got nil")
	}
	if !strings.Contains(err.Error(), "parse template spec.versionID") {
		t.Fatalf("expected parse error, got %v", err)
	}
}
