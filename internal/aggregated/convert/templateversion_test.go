package convert

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coder/coder/v2/codersdk"
)

func templateVersionFixture() (codersdk.Template, codersdk.TemplateVersion) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	templateID := uuid.New()
	tpl := codersdk.Template{ID: templateID, OrganizationName: "acme", Name: "docker", ActiveVersionID: uuid.New()}
	v := codersdk.TemplateVersion{
		ID:         uuid.New(),
		TemplateID: &templateID,
		CreatedAt:  now,
		UpdatedAt:  now.Add(time.Minute),
		Name:       "v1.2.3",
		Message:    "first",
		Readme:     "README-SECRET",
		CreatedBy:  codersdk.MinimalUser{Username: "alice", Name: "Alice Example"},
		Job: codersdk.ProvisionerJob{
			Status:    codersdk.ProvisionerJobFailed,
			Error:     "JOB-ERROR-SECRET",
			ErrorCode: codersdk.RequiredTemplateVariables,
			StartedAt: &now,
			FileID:    uuid.New(),
		},
	}
	return tpl, v
}

func TestTemplateVersionToK8s(t *testing.T) {
	t.Parallel()

	tpl, v := templateVersionFixture()
	obj := TemplateVersionToK8s("coder", tpl, v)

	if obj.Name != "acme.docker.v1.2.3" || obj.Namespace != "coder" || string(obj.UID) != v.ID.String() {
		t.Fatalf("unexpected identity: name=%q namespace=%q uid=%q", obj.Name, obj.Namespace, obj.UID)
	}
	if obj.Labels[TemplateVersionOrganizationLabel] != "acme" || obj.Labels[TemplateVersionTemplateLabel] != "docker" {
		t.Fatalf("unexpected labels: %v", obj.Labels)
	}
	if obj.Spec.Organization != "acme" || obj.Spec.TemplateName != "docker" || obj.Spec.Message != "first" {
		t.Fatalf("unexpected spec: %+v", obj.Spec)
	}
	if obj.Status.Active || obj.Status.Archived || obj.Status.CreatedBy != "alice" || obj.Status.TemplateID != tpl.ID.String() {
		t.Fatalf("unexpected status: %+v", obj.Status)
	}
	if obj.Status.Job.Status != "failed" || obj.Status.Job.ErrorCode != "REQUIRED_TEMPLATE_VARIABLES" ||
		obj.Status.Job.StartedAt == nil || obj.Status.Job.CompletedAt != nil {
		t.Fatalf("unexpected job: %+v", obj.Status.Job)
	}
	if len(obj.ResourceVersion) != 64 {
		t.Fatalf("expected a hex SHA-256 resourceVersion, got %q", obj.ResourceVersion)
	}

	// Excluded fields never appear in the object.
	serialized, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, excluded := range []string{"JOB-ERROR-SECRET", "README-SECRET", "Alice Example", v.Job.FileID.String()} {
		if strings.Contains(string(serialized), excluded) {
			t.Fatalf("serialized object contains excluded value %q: %s", excluded, serialized)
		}
	}
}

func TestTemplateVersionToK8sFingerprint(t *testing.T) {
	t.Parallel()

	tpl, v := templateVersionFixture()
	base := TemplateVersionToK8s("coder", tpl, v).ResourceVersion
	if again := TemplateVersionToK8s("coder", tpl, v).ResourceVersion; again != base {
		t.Fatalf("fingerprint is not stable: %q != %q", again, base)
	}

	activeTemplate := tpl
	activeTemplate.ActiveVersionID = v.ID
	active := TemplateVersionToK8s("coder", activeTemplate, v)
	if !active.Status.Active {
		t.Fatal("expected the active version to report active=true")
	}

	archived := v
	archived.Archived = true
	succeeded := v
	succeeded.Job.Status = codersdk.ProvisionerJobSucceeded
	renamed := v
	renamed.Name = "v2"

	seen := map[string]string{base: "base"}
	for label, rv := range map[string]string{
		"active":    active.ResourceVersion,
		"archived":  TemplateVersionToK8s("coder", tpl, archived).ResourceVersion,
		"succeeded": TemplateVersionToK8s("coder", tpl, succeeded).ResourceVersion,
		"renamed":   TemplateVersionToK8s("coder", tpl, renamed).ResourceVersion,
	} {
		if previous, ok := seen[rv]; ok {
			t.Fatalf("fingerprint for %s equals the one for %s", label, previous)
		}
		seen[rv] = label
	}
}

func TestTemplateVersionToK8sPanicsOnForeignVersion(t *testing.T) {
	t.Parallel()

	tpl, v := templateVersionFixture()
	otherTemplateID := uuid.New()
	v.TemplateID = &otherTemplateID

	defer func() {
		if recovered := recover(); recovered == nil || !strings.Contains(recovered.(string), "assertion failed") {
			t.Fatalf("expected an assertion panic, got %v", recovered)
		}
	}()
	TemplateVersionToK8s("coder", tpl, v)
}
