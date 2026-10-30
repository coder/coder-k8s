package convert

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
)

func mustTemplateToK8s(t *testing.T, namespace string, template codersdk.Template) *aggregationv1alpha1.CoderTemplate {
	t.Helper()
	obj, err := TemplateToK8s(namespace, template)
	if err != nil {
		t.Fatalf("convert template: %v", err)
	}
	return obj
}

func mustWorkspaceToK8s(t *testing.T, namespace string, workspace codersdk.Workspace) *aggregationv1alpha1.CoderWorkspace {
	t.Helper()
	obj, err := WorkspaceToK8s(namespace, workspace)
	if err != nil {
		t.Fatalf("convert workspace: %v", err)
	}
	return obj
}

func mustTemplateVersionToK8s(t *testing.T, namespace string, template codersdk.Template, version codersdk.TemplateVersion) *aggregationv1alpha1.CoderTemplateVersion {
	t.Helper()
	obj, err := TemplateVersionToK8s(namespace, template, version)
	if err != nil {
		t.Fatalf("convert template version: %v", err)
	}
	return obj
}

// TestConvertersRejectAnswersWithoutNames checks that a Coder answer without the fields of the
// Kubernetes name, for example the {} of a proxy (#242), is an error and not a panic.
func TestConvertersRejectAnswersWithoutNames(t *testing.T) {
	t.Parallel()

	templateID := uuid.New()
	for _, tc := range []struct {
		name    string
		convert func() error
		want    string
	}{
		{"empty template", func() error { _, err := TemplateToK8s("ns", codersdk.Template{}); return err }, "organization must not be empty"},
		{"template name with a dot", func() error {
			_, err := TemplateToK8s("ns", codersdk.Template{OrganizationName: "acme", Name: "a.b"})
			return err
		}, `template must not contain "."`},
		{"empty workspace", func() error { _, err := WorkspaceToK8s("ns", codersdk.Workspace{}); return err }, "organization must not be empty"},
		{"workspace without an owner", func() error {
			_, err := WorkspaceToK8s("ns", codersdk.Workspace{OrganizationName: "acme", Name: "dev"})
			return err
		}, "user must not be empty"},
		{"template version without a name", func() error {
			_, err := TemplateVersionToK8s("ns", codersdk.Template{ID: templateID, OrganizationName: "acme", Name: "docker"}, codersdk.TemplateVersion{TemplateID: &templateID})
			return err
		}, "version must not be empty"},
		{"template version of a template without an organization", func() error {
			_, err := TemplateVersionToK8s("ns", codersdk.Template{ID: templateID, Name: "docker"}, codersdk.TemplateVersion{TemplateID: &templateID, Name: "v1"})
			return err
		}, "organization must not be empty"},
	} {
		err := tc.convert()
		if err == nil || !strings.Contains(err.Error(), "the Coder API answered") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: expected an error with %q, got %v", tc.name, tc.want, err)
		}
	}
}
