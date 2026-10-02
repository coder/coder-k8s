package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

var (
	_ rest.Storage      = (*TemplatePromoteStorage)(nil)
	_ rest.NamedCreater = (*TemplatePromoteStorage)(nil)
)

// TemplatePromoteStorage serves the codertemplates/promote subresource. It evaluates the activation
// of an existing version of a template and never downloads template source.
//
// For now only a dry-run request (a preview) is served; a real request that would change the active
// version is refused before any write to Coder. The activation itself ships separately.
type TemplatePromoteStorage struct {
	provider coder.ClientProvider
}

// NewTemplatePromoteStorage builds codersdk-backed storage for the codertemplates/promote subresource.
func NewTemplatePromoteStorage(provider coder.ClientProvider) *TemplatePromoteStorage {
	if provider == nil {
		panic("assertion failed: template promote client provider must not be nil")
	}

	return &TemplatePromoteStorage{provider: provider}
}

// New returns an empty CoderTemplateVersionPromotion object.
func (s *TemplatePromoteStorage) New() runtime.Object {
	return &aggregationv1alpha1.CoderTemplateVersionPromotion{}
}

// Destroy is a no-op: the storage holds no background resources.
func (s *TemplatePromoteStorage) Destroy() {}

var promotionKind = schema.GroupKind{Group: aggregationv1alpha1.SchemeGroupVersion.Group, Kind: "CoderTemplateVersionPromotion"}

// Create evaluates the activation of spec.versionID on the template named name. Every outcome reports
// observed state, and no request sends a write to Coder: AlreadyActive needs none, a dry-run request
// reports WouldPromote, and a real request that needs a change is refused with 400 for now.
func (s *TemplatePromoteStorage) Create(
	ctx context.Context,
	name string,
	obj runtime.Object,
	createValidation rest.ValidateObjectFunc,
	options *metav1.CreateOptions,
) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template promote storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, fmt.Errorf("assertion failed: template name must not be empty")
	}

	request, ok := obj.(*aggregationv1alpha1.CoderTemplateVersionPromotion)
	if !ok || request == nil {
		return nil, fmt.Errorf("assertion failed: expected *CoderTemplateVersionPromotion, got %T", obj)
	}

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}
	orgName, templateName, err := coder.ParseTemplateName(name)
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid template name %q: %v", name, err))
	}
	if request.Name != "" && request.Name != name {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("metadata.name %q must be empty or match the template name %q", request.Name, name))
	}
	versionID, err := uuid.Parse(request.Spec.VersionID)
	if err != nil || versionID == uuid.Nil {
		return nil, apierrors.NewInvalid(promotionKind, name, field.ErrorList{field.Invalid(
			field.NewPath("spec", "versionID"), request.Spec.VersionID, "must be the Coder ID (a UUID) of a template version",
		)})
	}

	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}
	dryRun := options != nil && len(options.DryRun) > 0

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}
	resource := aggregationv1alpha1.Resource("codertemplates")

	org, err := sdk.OrganizationByName(ctx, orgName)
	if err != nil {
		return nil, coder.MapCoderError(err, resource, name)
	}
	if err := requireCanonicalTemplateName(name, orgName, templateName, org); err != nil {
		return nil, err
	}
	template, err := sdk.TemplateByName(ctx, org.ID, templateName)
	if err != nil {
		return nil, coder.MapCoderError(err, resource, name)
	}
	if err := requireCanonicalTemplateLeaf(name, orgName, templateName, template); err != nil {
		return nil, err
	}
	if template.ID == uuid.Nil || template.OrganizationID != org.ID {
		return nil, fmt.Errorf("assertion failed: template %q must have an ID and belong to organization %s", name, org.ID)
	}

	if err := s.requirePromotableVersion(ctx, sdk, name, template, versionID); err != nil {
		return nil, err
	}

	previous := template.ActiveVersionID
	result := func(outcome aggregationv1alpha1.CoderTemplateVersionPromotionResult, active uuid.UUID) runtime.Object {
		return &aggregationv1alpha1.CoderTemplateVersionPromotion{
			TypeMeta:   metav1.TypeMeta{Kind: promotionKind.Kind, APIVersion: aggregationv1alpha1.SchemeGroupVersion.String()},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       aggregationv1alpha1.CoderTemplateVersionPromotionSpec{VersionID: versionID.String()},
			Status: aggregationv1alpha1.CoderTemplateVersionPromotionStatus{
				Result: outcome, PreviousActiveVersionID: previous.String(), ActiveVersionID: active.String(),
			},
		}
	}
	// Coder accepts a repeated activation but still bumps the template's updated_at and writes an
	// audit entry, so an already active version gets no request at all.
	if previous == versionID {
		return result(aggregationv1alpha1.PromotionResultAlreadyActive, previous), nil
	}
	if dryRun {
		return result(aggregationv1alpha1.PromotionResultWouldPromote, previous), nil
	}

	return nil, apierrors.NewBadRequest(fmt.Sprintf(
		"promotion is not enabled yet; use dryRun=All to preview it: version %s is not active on template %q", versionID, name,
	))
}

// requirePromotableVersion checks that versionID is a version of template that Coder can activate.
// Unknown versions, orphan versions and versions of other templates get one identical error, so the
// error does not reveal whether a version of another template exists.
func (s *TemplatePromoteStorage) requirePromotableVersion(
	ctx context.Context, sdk *codersdk.Client, name string, template codersdk.Template, versionID uuid.UUID,
) error {
	version, err := sdk.TemplateVersion(ctx, versionID)
	if err != nil {
		var coderErr *codersdk.Error
		if errors.As(err, &coderErr) && coderErr.StatusCode() == http.StatusNotFound {
			return notVersionOfTemplateError(name)
		}
		return coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
	}
	if version.TemplateID == nil || *version.TemplateID != template.ID {
		return notVersionOfTemplateError(name)
	}
	if version.ID != versionID || version.OrganizationID != template.OrganizationID {
		return fmt.Errorf("assertion failed: version %s of template %q must have the requested ID and the template's organization", versionID, name)
	}
	if version.Archived {
		return apierrors.NewBadRequest(fmt.Sprintf("version %s of template %q is archived; unarchive it in Coder before promoting it", versionID, name))
	}
	if version.Job.Status != codersdk.ProvisionerJobSucceeded {
		return apierrors.NewBadRequest(fmt.Sprintf(
			"version %s of template %q cannot be promoted: its import job is %q; only versions whose import succeeded can be promoted",
			versionID, name, version.Job.Status,
		))
	}

	return nil
}

func notVersionOfTemplateError(name string) error {
	return apierrors.NewBadRequest(fmt.Sprintf("spec.versionID is not a version of template %q", name))
}

func (s *TemplatePromoteStorage) clientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("assertion failed: template promote client provider must not be nil")
	}

	sdk, err := s.provider.ClientForNamespace(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("resolve codersdk client for namespace %q: %w", namespace, err)
	}
	if sdk == nil {
		return nil, fmt.Errorf("assertion failed: template promote client provider returned nil codersdk client")
	}

	return sdk, nil
}
