package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

const (
	// TemplateVersionOrganizationLabel holds the organization name of a CoderTemplateVersion.
	TemplateVersionOrganizationLabel = "aggregation.coder.com/organization"
	// TemplateVersionTemplateLabel holds the template name of a CoderTemplateVersion.
	TemplateVersionTemplateLabel = "aggregation.coder.com/template"
)

// TemplateVersionToK8s converts a Coder template version of tpl to a CoderTemplateVersion.
//
// metadata.resourceVersion is a fingerprint (hex SHA-256) of the returned object. It changes when
// any returned field changes, including the active flag, which Coder does not reflect in the
// version's own updated_at. Compare it only for equality.
func TemplateVersionToK8s(namespace string, tpl codersdk.Template, v codersdk.TemplateVersion) *aggregationv1alpha1.CoderTemplateVersion {
	if namespace == "" {
		panic("assertion failed: namespace must not be empty")
	}
	if v.TemplateID == nil || *v.TemplateID != tpl.ID {
		panic(fmt.Sprintf("assertion failed: template version %s must belong to template %s", v.ID, tpl.ID))
	}
	for _, value := range []string{tpl.OrganizationName, tpl.Name} {
		if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
			panic(fmt.Sprintf("assertion failed: %q must be a valid label value: %s", value, strings.Join(problems, "; ")))
		}
	}

	obj := &aggregationv1alpha1.CoderTemplateVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderTemplateVersion",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              coder.BuildTemplateVersionName(tpl.OrganizationName, tpl.Name, v.Name),
			Namespace:         namespace,
			UID:               types.UID(v.ID.String()),
			CreationTimestamp: metav1.NewTime(v.CreatedAt),
			Labels: map[string]string{
				TemplateVersionOrganizationLabel: tpl.OrganizationName,
				TemplateVersionTemplateLabel:     tpl.Name,
			},
		},
		Spec: aggregationv1alpha1.CoderTemplateVersionSpec{
			Organization: tpl.OrganizationName,
			TemplateName: tpl.Name,
			Message:      v.Message,
		},
		Status: aggregationv1alpha1.CoderTemplateVersionStatus{
			ID:         v.ID.String(),
			TemplateID: tpl.ID.String(),
			Active:     tpl.ActiveVersionID == v.ID,
			Archived:   v.Archived,
			CreatedBy:  v.CreatedBy.Username,
			UpdatedAt:  optionalTime(&v.UpdatedAt),
			Job: aggregationv1alpha1.CoderTemplateVersionJob{
				Status:      string(v.Job.Status),
				ErrorCode:   string(v.Job.ErrorCode),
				StartedAt:   optionalTime(v.Job.StartedAt),
				CompletedAt: optionalTime(v.Job.CompletedAt),
			},
		},
	}

	// Serialization of the generated API type cannot fail for well-formed input, so an error is an
	// impossible state.
	serialized, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("assertion failed: serialize template version representation: %v", err))
	}
	sum := sha256.Sum256(serialized)
	obj.ResourceVersion = hex.EncodeToString(sum[:])

	return obj
}

func optionalTime(value *time.Time) *metav1.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	converted := metav1.NewTime(*value)
	return &converted
}
