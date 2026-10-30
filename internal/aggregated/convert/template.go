// Package convert maps codersdk models to aggregated API resources and request payloads.
package convert

import (
	"fmt"
	"strconv"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// nameField is a field of a Coder answer that becomes a segment of the Kubernetes name.
type nameField struct{ segment, value string }

// requireNameFields returns an error when a field cannot be a segment of the name, for example in
// the {} that a proxy answers instead of Coder.
func requireNameFields(object string, fields ...nameField) error {
	for _, field := range fields {
		if err := coder.NameSegmentError(field.segment, field.value); err != nil {
			return fmt.Errorf("the Coder API answered a %s without a valid name: %w", object, err)
		}
	}
	return nil
}

// TemplateToK8s converts a codersdk.Template to an aggregated API CoderTemplate. It returns an
// error when t lacks the fields of the name.
func TemplateToK8s(namespace string, t codersdk.Template) (*aggregationv1alpha1.CoderTemplate, error) {
	if namespace == "" {
		panic("assertion failed: namespace must not be empty")
	}
	if err := requireNameFields("template", nameField{"organization", t.OrganizationName}, nameField{"template", t.Name}); err != nil {
		return nil, err
	}

	updatedAt := metav1.NewTime(t.UpdatedAt)

	return &aggregationv1alpha1.CoderTemplate{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderTemplate",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              coder.BuildTemplateName(t.OrganizationName, t.Name),
			Namespace:         namespace,
			UID:               types.UID(t.ID.String()),
			ResourceVersion:   strconv.FormatInt(t.UpdatedAt.UnixNano(), 10),
			CreationTimestamp: metav1.NewTime(t.CreatedAt),
		},
		Spec: aggregationv1alpha1.CoderTemplateSpec{
			Organization: t.OrganizationName,
			VersionID:    t.ActiveVersionID.String(),
			DisplayName:  t.DisplayName,
			Description:  t.Description,
			Icon:         t.Icon,
		},
		Status: aggregationv1alpha1.CoderTemplateStatus{
			ID:               t.ID.String(),
			OrganizationName: t.OrganizationName,
			ActiveVersionID:  t.ActiveVersionID.String(),
			Deprecated:       t.Deprecated,
			UpdatedAt:        &updatedAt,
		},
	}, nil
}

// TemplateCreateRequestFromK8s builds a codersdk.CreateTemplateRequest from a K8s CoderTemplate.
func TemplateCreateRequestFromK8s(obj *aggregationv1alpha1.CoderTemplate, templateName string) (codersdk.CreateTemplateRequest, error) {
	if obj == nil {
		return codersdk.CreateTemplateRequest{}, fmt.Errorf("assertion failed: template object must not be nil")
	}
	if templateName == "" {
		return codersdk.CreateTemplateRequest{}, fmt.Errorf("assertion failed: template name must not be empty")
	}

	versionID, err := uuid.Parse(obj.Spec.VersionID)
	if err != nil {
		return codersdk.CreateTemplateRequest{}, fmt.Errorf("parse template spec.versionID %q: %w", obj.Spec.VersionID, err)
	}

	return codersdk.CreateTemplateRequest{
		Name:        templateName,
		VersionID:   versionID,
		DisplayName: obj.Spec.DisplayName,
		Description: obj.Spec.Description,
		Icon:        obj.Spec.Icon,
	}, nil
}

// TemplateUpdateMetaRequestFromK8s builds a codersdk.UpdateTemplateMeta request.
func TemplateUpdateMetaRequestFromK8s(obj *aggregationv1alpha1.CoderTemplate) codersdk.UpdateTemplateMeta {
	if obj == nil {
		panic("assertion failed: template object must not be nil")
	}

	displayName := obj.Spec.DisplayName
	description := obj.Spec.Description
	icon := obj.Spec.Icon

	return codersdk.UpdateTemplateMeta{
		DisplayName: &displayName,
		Description: &description,
		Icon:        &icon,
	}
}
