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

// TemplateToK8s converts a codersdk.Template to an aggregated API CoderTemplate.
func TemplateToK8s(namespace string, t codersdk.Template) *aggregationv1alpha1.CoderTemplate {
	if namespace == "" {
		panic("assertion failed: namespace must not be empty")
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
	}
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
