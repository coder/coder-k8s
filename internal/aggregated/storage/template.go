package storage

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/aggregated/convert"
	"github.com/coder/coder/v2/codersdk"
)

var (
	_ rest.Storage              = (*TemplateStorage)(nil)
	_ rest.Getter               = (*TemplateStorage)(nil)
	_ rest.Lister               = (*TemplateStorage)(nil)
	_ rest.Creater              = (*TemplateStorage)(nil) //nolint:misspell // Kubernetes rest interface name is Creater.
	_ rest.Updater              = (*TemplateStorage)(nil)
	_ rest.GracefulDeleter      = (*TemplateStorage)(nil)
	_ rest.Scoper               = (*TemplateStorage)(nil)
	_ rest.SingularNameProvider = (*TemplateStorage)(nil)
)

// TemplateStorage provides codersdk-backed CoderTemplate objects.
type TemplateStorage struct {
	provider       coder.ClientProvider
	tableConvertor rest.TableConvertor
}

// NewTemplateStorage builds codersdk-backed storage for CoderTemplate resources.
func NewTemplateStorage(provider coder.ClientProvider) *TemplateStorage {
	if provider == nil {
		panic("assertion failed: template client provider must not be nil")
	}

	return &TemplateStorage{
		provider:       provider,
		tableConvertor: rest.NewDefaultTableConvertor(aggregationv1alpha1.Resource("codertemplates")),
	}
}

// New returns an empty CoderTemplate object.
func (s *TemplateStorage) New() runtime.Object {
	return &aggregationv1alpha1.CoderTemplate{}
}

// Destroy cleans up storage resources.
func (s *TemplateStorage) Destroy() {}

// NamespaceScoped returns true because CoderTemplate is namespaced.
func (s *TemplateStorage) NamespaceScoped() bool {
	return true
}

// GetSingularName returns the singular name of the CoderTemplate resource.
func (s *TemplateStorage) GetSingularName() string {
	return "codertemplate"
}

// NewList returns an empty CoderTemplateList object.
func (s *TemplateStorage) NewList() runtime.Object {
	return &aggregationv1alpha1.CoderTemplateList{}
}

// Get fetches a CoderTemplate by organization and template name.
func (s *TemplateStorage) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, fmt.Errorf("assertion failed: template name must not be empty")
	}

	namespace, badNamespaceErr := namespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}

	orgName, templateName, err := coder.ParseTemplateName(name)
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid template name %q: %v", name, err))
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}

	org, err := sdk.OrganizationByName(ctx, orgName)
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
	}

	template, err := sdk.TemplateByName(ctx, org.ID, templateName)
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
	}

	return convert.TemplateToK8s(namespace, template), nil
}

// List fetches CoderTemplate objects from codersdk.
func (s *TemplateStorage) List(ctx context.Context, _ *metainternalversion.ListOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}

	namespace, badNamespaceErr := namespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}

	templates, err := sdk.Templates(ctx, codersdk.TemplateFilter{})
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), "<list>")
	}

	list := &aggregationv1alpha1.CoderTemplateList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderTemplateList",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		Items: make([]aggregationv1alpha1.CoderTemplate, 0, len(templates)),
	}

	for _, template := range templates {
		list.Items = append(list.Items, *convert.TemplateToK8s(namespace, template))
	}

	return list, nil
}

// Create creates a CoderTemplate through codersdk.
func (s *TemplateStorage) Create(
	ctx context.Context,
	obj runtime.Object,
	createValidation rest.ValidateObjectFunc,
	_ *metav1.CreateOptions,
) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if obj == nil {
		return nil, fmt.Errorf("assertion failed: object must not be nil")
	}

	templateObj, ok := obj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderTemplate, got %T", obj))
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}
	if templateObj.Name == "" {
		return nil, apierrors.NewBadRequest("metadata.name must not be empty")
	}

	namespace, badNamespaceErr := namespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}
	if templateObj.Namespace != "" && templateObj.Namespace != namespace {
		return nil, apierrors.NewBadRequest(
			fmt.Sprintf("metadata.namespace %q must match request namespace %q", templateObj.Namespace, namespace),
		)
	}

	orgName, templateName, err := coder.ParseTemplateName(templateObj.Name)
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid template name %q: %v", templateObj.Name, err))
	}
	if templateObj.Spec.Organization != orgName {
		return nil, apierrors.NewBadRequest(
			fmt.Sprintf(
				"spec.organization %q must match organization %q parsed from metadata.name",
				templateObj.Spec.Organization,
				orgName,
			),
		)
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}

	org, err := sdk.OrganizationByName(ctx, orgName)
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), templateObj.Name)
	}

	request, err := convert.TemplateCreateRequestFromK8s(templateObj, templateName)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}

	createdTemplate, err := sdk.CreateTemplate(ctx, org.ID, request)
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), templateObj.Name)
	}

	return convert.TemplateToK8s(namespace, createdTemplate), nil
}

// Update applies a legacy-compatible template update.
func (s *TemplateStorage) Update(
	ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	_ rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool,
	_ *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if ctx == nil {
		return nil, false, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, false, fmt.Errorf("assertion failed: template name must not be empty")
	}
	if objInfo == nil {
		return nil, false, fmt.Errorf("assertion failed: updated object info must not be nil")
	}
	if forceAllowCreate {
		return nil, false, apierrors.NewMethodNotSupported(
			aggregationv1alpha1.Resource("codertemplates"),
			"create on update",
		)
	}

	currentObj, err := s.Get(ctx, name, nil)
	if err != nil {
		return nil, false, err
	}

	currentObjForUpdate := currentObj.DeepCopyObject()
	if currentObjForUpdate == nil {
		return nil, false, fmt.Errorf("assertion failed: current template object deep copy must not be nil")
	}

	updatedObj, err := objInfo.UpdatedObject(ctx, currentObjForUpdate)
	if err != nil {
		return nil, false, err
	}
	if updatedObj == nil {
		return nil, false, fmt.Errorf("assertion failed: updated template object must not be nil")
	}
	if updateValidation != nil {
		if err := updateValidation(ctx, updatedObj, currentObj); err != nil {
			return nil, false, err
		}
	}

	updatedTemplate, ok := updatedObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		return nil, false, fmt.Errorf("assertion failed: expected *CoderTemplate, got %T", updatedObj)
	}
	currentTemplate, ok := currentObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		return nil, false, fmt.Errorf("assertion failed: expected *CoderTemplate, got %T", currentObj)
	}

	// Template updates via codersdk are currently limited. The legacy spec.running
	// field remains for compatibility with in-repo callers and is a no-op in the
	// Coder backend. Reject updates to all other spec fields to avoid drift between
	// accepted update payloads and persisted backend state.
	if updatedTemplate.Spec.Organization != currentTemplate.Spec.Organization ||
		updatedTemplate.Spec.VersionID != currentTemplate.Spec.VersionID ||
		updatedTemplate.Spec.DisplayName != currentTemplate.Spec.DisplayName ||
		updatedTemplate.Spec.Description != currentTemplate.Spec.Description ||
		updatedTemplate.Spec.Icon != currentTemplate.Spec.Icon {
		return nil, false, apierrors.NewBadRequest(
			"template update only supports changing spec.running; other spec fields are immutable",
		)
	}

	return updatedObj, false, nil
}

// Delete deletes a CoderTemplate through codersdk.
func (s *TemplateStorage) Delete(
	ctx context.Context,
	name string,
	deleteValidation rest.ValidateObjectFunc,
	_ *metav1.DeleteOptions,
) (runtime.Object, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if ctx == nil {
		return nil, false, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, false, fmt.Errorf("assertion failed: template name must not be empty")
	}

	namespace, badNamespaceErr := namespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, false, badNamespaceErr
	}

	orgName, templateName, err := coder.ParseTemplateName(name)
	if err != nil {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("invalid template name %q: %v", name, err))
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, false, wrapClientError(err)
	}

	org, err := sdk.OrganizationByName(ctx, orgName)
	if err != nil {
		return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
	}

	template, err := sdk.TemplateByName(ctx, org.ID, templateName)
	if err != nil {
		return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
	}

	if deleteValidation != nil {
		if validationErr := deleteValidation(ctx, convert.TemplateToK8s(namespace, template)); validationErr != nil {
			return nil, false, validationErr
		}
	}

	if err := sdk.DeleteTemplate(ctx, template.ID); err != nil {
		return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
	}

	return &metav1.Status{Status: metav1.StatusSuccess}, true, nil
}

// ConvertToTable converts a template object or list into kubectl table output.
func (s *TemplateStorage) ConvertToTable(ctx context.Context, object, tableOptions runtime.Object) (*metav1.Table, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if s.tableConvertor == nil {
		return nil, fmt.Errorf("assertion failed: template table convertor must not be nil")
	}

	return s.tableConvertor.ConvertToTable(ctx, object, tableOptions)
}

func (s *TemplateStorage) clientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("assertion failed: template client provider must not be nil")
	}

	sdk, err := s.provider.ClientForNamespace(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("resolve codersdk client for namespace %q: %w", namespace, err)
	}
	if sdk == nil {
		return nil, fmt.Errorf("assertion failed: template client provider returned nil codersdk client")
	}

	return sdk, nil
}
