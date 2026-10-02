package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

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

// The API server cuts every create request at 34 seconds and then answers 504 itself, whatever the
// storage did. A promotion therefore splits its deadline: the activation request gets at most
// TemplatePromotePatchBudget, and TemplatePromoteRereadReserve stays for the confirming re-read and
// the answer. The re-read ends a fifth of the reserve before the deadline, so after an activation the
// caller gets Promoted, 409 or 503 from this storage, never the API server's 504.
const (
	TemplatePromotePatchBudget   = 10 * time.Second
	TemplatePromoteRereadReserve = 5 * time.Second
)

// TemplatePromoteStorage serves the codertemplates/promote subresource. It activates an existing
// version of a template. It never downloads template source and never retries the activation.
type TemplatePromoteStorage struct {
	provider      coder.ClientProvider
	patchBudget   time.Duration
	rereadReserve time.Duration
}

// NewTemplatePromoteStorage builds codersdk-backed storage for the codertemplates/promote subresource.
func NewTemplatePromoteStorage(provider coder.ClientProvider) *TemplatePromoteStorage {
	if provider == nil {
		panic("assertion failed: template promote client provider must not be nil")
	}

	return &TemplatePromoteStorage{provider: provider, patchBudget: TemplatePromotePatchBudget, rereadReserve: TemplatePromoteRereadReserve}
}

// New returns an empty CoderTemplateVersionPromotion object.
func (s *TemplatePromoteStorage) New() runtime.Object {
	return &aggregationv1alpha1.CoderTemplateVersionPromotion{}
}

// Destroy is a no-op: the storage holds no background resources.
func (s *TemplatePromoteStorage) Destroy() {}

var promotionKind = schema.GroupKind{Group: aggregationv1alpha1.SchemeGroupVersion.Group, Kind: "CoderTemplateVersionPromotion"}

// Create activates spec.versionID on the template named name.
//
// Every outcome reports observed state. AlreadyActive and dry-run send no write to Coder. A real
// activation is confirmed by re-reading the template: it returns 409 when another version became
// active instead, and 503 when the outcome cannot be confirmed. A 503 has no Retry-After, because
// the activation may already have been applied.
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
	if s.patchBudget <= 0 || s.rereadReserve <= 0 {
		return nil, fmt.Errorf("assertion failed: template promote patch budget and re-read reserve must be positive")
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

	// Send the activation only if the activation and the confirming re-read both still fit.
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline && time.Until(deadline) < s.patchBudget+s.rereadReserve {
		return nil, apierrors.NewTimeoutError(fmt.Sprintf(
			"the promotion of version %s on template %q was not attempted: too little of the request time was left after the lookups; nothing was sent to Coder, so it is safe to try again",
			versionID, name,
		), 0)
	}
	patchCtx, cancel := context.WithTimeout(ctx, s.patchBudget)
	patchErr := patchActiveTemplateVersion(patchCtx, sdk, template.ID, versionID)
	cancel()
	var coderErr *codersdk.Error
	if errors.As(patchErr, &coderErr) && coderErr.StatusCode() >= 400 && coderErr.StatusCode() < 500 {
		// Coder answered with a rejection, so nothing was applied. The messages are fixed: Coder's
		// error text is not passed on.
		switch coderErr.StatusCode() {
		case http.StatusNotFound:
			// The checks above found both, so the template or the version changed meanwhile.
			return nil, apierrors.NewConflict(resource, name, fmt.Errorf(
				"the template or version %s was not found in Coder during the activation; nothing was changed: re-read before retrying", versionID,
			))
		case http.StatusTooManyRequests:
			return nil, apierrors.NewTooManyRequests("Coder rate-limited the activation; nothing was changed", 0)
		default:
			// A Coder 403 means that the version cannot be promoted, not that the caller lacks access.
			return nil, apierrors.NewBadRequest(fmt.Sprintf(
				"Coder refused to activate version %s on template %q (status %d); nothing was changed: check that its import succeeded and that it is not archived",
				versionID, name, coderErr.StatusCode(),
			))
		}
	}

	// A success, a transport error, a timeout or a 5xx: re-read the template to learn the outcome.
	rereadCtx := ctx
	if hasDeadline {
		var cancelReread context.CancelFunc
		rereadCtx, cancelReread = context.WithDeadline(ctx, deadline.Add(-s.rereadReserve/5))
		defer cancelReread()
	}
	confirmed, err := sdk.Template(rereadCtx, template.ID)
	if err != nil {
		return nil, couldNotConfirmPromotionError(name, versionID)
	}
	switch {
	case confirmed.ActiveVersionID == versionID:
		return result(aggregationv1alpha1.PromotionResultPromoted, versionID), nil
	case patchErr != nil && confirmed.ActiveVersionID == previous:
		return nil, couldNotConfirmPromotionError(name, versionID)
	default:
		return nil, apierrors.NewConflict(resource, name, fmt.Errorf(
			"the promotion of version %s was superseded by a concurrent change: version %s is active; re-read before retrying",
			versionID, confirmed.ActiveVersionID,
		))
	}
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

// patchActiveTemplateVersion activates versionID. Unlike codersdk.UpdateActiveTemplateVersion,
// which returns nil on a transport error, it returns every transport error and non-200 answer.
func patchActiveTemplateVersion(ctx context.Context, sdk *codersdk.Client, templateID, versionID uuid.UUID) error {
	res, err := sdk.Request(ctx, http.MethodPatch, fmt.Sprintf("/api/v2/templates/%s/versions", templateID),
		codersdk.UpdateActiveTemplateVersion{ID: versionID})
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return codersdk.ReadBodyAsError(res)
	}

	return nil
}

func notVersionOfTemplateError(name string) error {
	return apierrors.NewBadRequest(fmt.Sprintf("spec.versionID is not a version of template %q", name))
}

func couldNotConfirmPromotionError(name string, versionID uuid.UUID) error {
	return apierrors.NewServiceUnavailable(fmt.Sprintf(
		"could not confirm the promotion of version %s on template %q; it may or may not have been applied: re-read the template before retrying",
		versionID, name,
	))
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
