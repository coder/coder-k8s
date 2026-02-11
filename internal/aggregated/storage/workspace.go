package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/aggregated/convert"
	"github.com/coder/coder/v2/codersdk"
)

var (
	_ rest.Storage              = (*WorkspaceStorage)(nil)
	_ rest.Getter               = (*WorkspaceStorage)(nil)
	_ rest.Lister               = (*WorkspaceStorage)(nil)
	_ rest.Creater              = (*WorkspaceStorage)(nil) //nolint:misspell // Kubernetes rest interface name is Creater.
	_ rest.Updater              = (*WorkspaceStorage)(nil)
	_ rest.GracefulDeleter      = (*WorkspaceStorage)(nil)
	_ rest.Scoper               = (*WorkspaceStorage)(nil)
	_ rest.SingularNameProvider = (*WorkspaceStorage)(nil)
)

// WorkspaceStorage provides codersdk-backed CoderWorkspace objects.
type WorkspaceStorage struct {
	provider       coder.ClientProvider
	tableConvertor rest.TableConvertor
}

// NewWorkspaceStorage builds codersdk-backed storage for CoderWorkspace resources.
func NewWorkspaceStorage(provider coder.ClientProvider) *WorkspaceStorage {
	if provider == nil {
		panic("assertion failed: workspace client provider must not be nil")
	}

	return &WorkspaceStorage{
		provider:       provider,
		tableConvertor: rest.NewDefaultTableConvertor(aggregationv1alpha1.Resource("coderworkspaces")),
	}
}

// New returns an empty CoderWorkspace object.
func (s *WorkspaceStorage) New() runtime.Object {
	return &aggregationv1alpha1.CoderWorkspace{}
}

// Destroy cleans up storage resources.
func (s *WorkspaceStorage) Destroy() {}

// NamespaceScoped returns true because CoderWorkspace is namespaced.
func (s *WorkspaceStorage) NamespaceScoped() bool {
	return true
}

// GetSingularName returns the singular name of the CoderWorkspace resource.
func (s *WorkspaceStorage) GetSingularName() string {
	return "coderworkspace"
}

// NewList returns an empty CoderWorkspaceList object.
func (s *WorkspaceStorage) NewList() runtime.Object {
	return &aggregationv1alpha1.CoderWorkspaceList{}
}

// Get fetches a CoderWorkspace by organization, owner, and workspace name.
func (s *WorkspaceStorage) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: workspace storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, fmt.Errorf("assertion failed: workspace name must not be empty")
	}

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}

	orgName, userName, workspaceName, err := coder.ParseWorkspaceName(name)
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid workspace name %q: %v", name, err))
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}

	workspace, err := sdk.WorkspaceByOwnerAndName(ctx, userName, workspaceName, codersdk.WorkspaceOptions{})
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
	}
	if workspace.OrganizationName != orgName {
		return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	return convert.WorkspaceToK8s(namespace, workspace), nil
}

// List fetches CoderWorkspace objects from codersdk.
func (s *WorkspaceStorage) List(ctx context.Context, _ *metainternalversion.ListOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: workspace storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}

	namespace, badNamespaceErr := namespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}

	responseNamespace, responseNamespaceErr := namespaceForListConversion(namespace, s.provider)
	if responseNamespaceErr != nil {
		return nil, responseNamespaceErr
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}

	workspacesResponse, err := sdk.Workspaces(ctx, codersdk.WorkspaceFilter{})
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), "<list>")
	}

	list := &aggregationv1alpha1.CoderWorkspaceList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderWorkspaceList",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		Items: make([]aggregationv1alpha1.CoderWorkspace, 0, len(workspacesResponse.Workspaces)),
	}

	for _, workspace := range workspacesResponse.Workspaces {
		list.Items = append(list.Items, *convert.WorkspaceToK8s(responseNamespace, workspace))
	}

	return list, nil
}

// Create creates a CoderWorkspace through codersdk.
func (s *WorkspaceStorage) Create(
	ctx context.Context,
	obj runtime.Object,
	createValidation rest.ValidateObjectFunc,
	_ *metav1.CreateOptions,
) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: workspace storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if obj == nil {
		return nil, fmt.Errorf("assertion failed: object must not be nil")
	}

	workspaceObj, ok := obj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderWorkspace, got %T", obj))
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}
	if workspaceObj.Name == "" {
		return nil, apierrors.NewBadRequest("metadata.name must not be empty")
	}

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}
	if workspaceObj.Namespace != "" && workspaceObj.Namespace != namespace {
		return nil, apierrors.NewBadRequest(
			fmt.Sprintf("metadata.namespace %q must match request namespace %q", workspaceObj.Namespace, namespace),
		)
	}

	orgName, userName, workspaceName, err := coder.ParseWorkspaceName(workspaceObj.Name)
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid workspace name %q: %v", workspaceObj.Name, err))
	}
	if workspaceObj.Spec.Organization != orgName {
		return nil, apierrors.NewBadRequest(
			fmt.Sprintf(
				"spec.organization %q must match organization %q parsed from metadata.name",
				workspaceObj.Spec.Organization,
				orgName,
			),
		)
	}
	if workspaceObj.Spec.TemplateName == "" {
		return nil, apierrors.NewBadRequest("spec.templateName must not be empty")
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}

	org, err := sdk.OrganizationByName(ctx, orgName)
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), workspaceObj.Name)
	}

	template, err := sdk.TemplateByName(ctx, org.ID, workspaceObj.Spec.TemplateName)
	if err != nil {
		return nil, coder.MapCoderError(
			err,
			aggregationv1alpha1.Resource("codertemplates"),
			coder.BuildTemplateName(orgName, workspaceObj.Spec.TemplateName),
		)
	}

	if workspaceObj.Spec.TemplateVersionID != "" {
		parsedTemplateVersionID, parseErr := uuid.Parse(workspaceObj.Spec.TemplateVersionID)
		if parseErr != nil {
			return nil, apierrors.NewBadRequest(
				fmt.Sprintf(
					"invalid workspace spec: invalid templateVersionID %q: %v",
					workspaceObj.Spec.TemplateVersionID,
					parseErr,
				),
			)
		}

		templateVersion, templateVersionErr := sdk.TemplateVersion(ctx, parsedTemplateVersionID)
		if templateVersionErr != nil {
			return nil, coder.MapCoderError(
				templateVersionErr,
				aggregationv1alpha1.Resource("coderworkspaces"),
				workspaceObj.Name,
			)
		}

		if templateVersion.TemplateID == nil || *templateVersion.TemplateID != template.ID {
			return nil, apierrors.NewBadRequest(
				fmt.Sprintf(
					"spec.templateVersionID %q does not belong to template %q",
					workspaceObj.Spec.TemplateVersionID,
					workspaceObj.Spec.TemplateName,
				),
			)
		}
	}

	request, err := convert.WorkspaceCreateRequestFromK8s(workspaceObj, workspaceName, template.ID)
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid workspace spec: %v", err))
	}

	createdWorkspace, err := sdk.CreateUserWorkspace(ctx, userName, request)
	if err != nil {
		return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), workspaceObj.Name)
	}

	if !workspaceObj.Spec.Running {
		stopBuild, stopErr := sdk.CreateWorkspaceBuild(ctx, createdWorkspace.ID, codersdk.CreateWorkspaceBuildRequest{
			Transition: codersdk.WorkspaceTransitionStop,
		})
		if stopErr == nil {
			createdWorkspace.LatestBuild = stopBuild
			if !stopBuild.UpdatedAt.IsZero() {
				createdWorkspace.UpdatedAt = stopBuild.UpdatedAt
			}
		}
		// The workspace creation already succeeded. Returning a stop transition error here
		// would cause client retries to fail with AlreadyExists, while the desired stop
		// transition can be retried safely via a subsequent Update.
	}

	return convert.WorkspaceToK8s(namespace, createdWorkspace), nil
}

// Update updates workspace run state through codersdk build transitions.
func (s *WorkspaceStorage) Update(
	ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	_ rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool,
	_ *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("assertion failed: workspace storage must not be nil")
	}
	if ctx == nil {
		return nil, false, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, false, fmt.Errorf("assertion failed: workspace name must not be empty")
	}
	if objInfo == nil {
		return nil, false, fmt.Errorf("assertion failed: updated object info must not be nil")
	}
	if forceAllowCreate {
		return nil, false, apierrors.NewMethodNotSupported(
			aggregationv1alpha1.Resource("coderworkspaces"),
			"create on update",
		)
	}

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, false, badNamespaceErr
	}

	orgName, userName, workspaceName, err := coder.ParseWorkspaceName(name)
	if err != nil {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("invalid workspace name %q: %v", name, err))
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, false, wrapClientError(err)
	}

	currentWorkspace, err := sdk.WorkspaceByOwnerAndName(ctx, userName, workspaceName, codersdk.WorkspaceOptions{})
	if err != nil {
		return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
	}
	if currentWorkspace.OrganizationName != orgName {
		return nil, false, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	currentK8sObj := convert.WorkspaceToK8s(namespace, currentWorkspace)
	desiredObjRuntime, err := objInfo.UpdatedObject(ctx, currentK8sObj.DeepCopy())
	if err != nil {
		return nil, false, err
	}

	desiredObj, ok := desiredObjRuntime.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf("updated object must be *CoderWorkspace, got %T", desiredObjRuntime),
		)
	}
	if desiredObj.Name != "" && desiredObj.Name != name {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf("updated object metadata.name %q must match request name %q", desiredObj.Name, name),
		)
	}
	if desiredObj.Spec.Organization != "" && desiredObj.Spec.Organization != orgName {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf(
				"updated object spec.organization %q must match organization %q parsed from metadata.name",
				desiredObj.Spec.Organization,
				orgName,
			),
		)
	}
	if desiredObj.Namespace != "" && desiredObj.Namespace != namespace {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf("metadata.namespace %q does not match request namespace %q", desiredObj.Namespace, namespace),
		)
	}
	if desiredObj.ResourceVersion == "" {
		return nil, false, apierrors.NewBadRequest("metadata.resourceVersion is required for update")
	}
	if desiredObj.ResourceVersion != currentK8sObj.ResourceVersion {
		return nil, false, apierrors.NewConflict(
			aggregationv1alpha1.Resource("coderworkspaces"),
			name,
			fmt.Errorf(
				"resource version mismatch: got %q, current is %q",
				desiredObj.ResourceVersion,
				currentK8sObj.ResourceVersion,
			),
		)
	}

	if updateValidation != nil {
		if err := updateValidation(ctx, desiredObj, currentK8sObj); err != nil {
			return nil, false, err
		}
	}

	// Workspace updates via codersdk are currently limited to workspace build
	// transitions, which map only to spec.running toggles in this API.
	if desiredObj.Spec.Organization != currentK8sObj.Spec.Organization ||
		desiredObj.Spec.TemplateName != currentK8sObj.Spec.TemplateName ||
		(desiredObj.Spec.TemplateVersionID != "" && desiredObj.Spec.TemplateVersionID != currentK8sObj.Spec.TemplateVersionID) ||
		(desiredObj.Spec.TTLMillis != nil && !equalInt64Ptr(desiredObj.Spec.TTLMillis, currentK8sObj.Spec.TTLMillis)) ||
		(desiredObj.Spec.AutostartSchedule != nil && !equalStringPtr(desiredObj.Spec.AutostartSchedule, currentK8sObj.Spec.AutostartSchedule)) {
		return nil, false, apierrors.NewBadRequest(
			"workspace update only supports changing spec.running; other spec fields are immutable",
		)
	}

	if desiredObj.Spec.Running == currentK8sObj.Spec.Running {
		return currentK8sObj, false, nil
	}

	transition := codersdk.WorkspaceTransitionStop
	if desiredObj.Spec.Running {
		transition = codersdk.WorkspaceTransitionStart
	}

	build, err := sdk.CreateWorkspaceBuild(ctx, currentWorkspace.ID, codersdk.CreateWorkspaceBuildRequest{
		Transition: transition,
	})
	if err != nil {
		return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	currentWorkspace.LatestBuild = build
	if !build.UpdatedAt.IsZero() {
		currentWorkspace.UpdatedAt = build.UpdatedAt
	}

	return convert.WorkspaceToK8s(namespace, currentWorkspace), false, nil
}

// Delete requests workspace deletion through a codersdk build transition.
func (s *WorkspaceStorage) Delete(
	ctx context.Context,
	name string,
	deleteValidation rest.ValidateObjectFunc,
	_ *metav1.DeleteOptions,
) (runtime.Object, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("assertion failed: workspace storage must not be nil")
	}
	if ctx == nil {
		return nil, false, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, false, fmt.Errorf("assertion failed: workspace name must not be empty")
	}

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, false, badNamespaceErr
	}

	orgName, userName, workspaceName, err := coder.ParseWorkspaceName(name)
	if err != nil {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("invalid workspace name %q: %v", name, err))
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, false, wrapClientError(err)
	}

	workspace, err := sdk.WorkspaceByOwnerAndName(ctx, userName, workspaceName, codersdk.WorkspaceOptions{})
	if err != nil {
		return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
	}
	if workspace.OrganizationName != orgName {
		return nil, false, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	if deleteValidation != nil {
		if validationErr := deleteValidation(ctx, convert.WorkspaceToK8s(namespace, workspace)); validationErr != nil {
			return nil, false, validationErr
		}
	}

	_, err = sdk.CreateWorkspaceBuild(ctx, workspace.ID, codersdk.CreateWorkspaceBuildRequest{
		Transition: codersdk.WorkspaceTransitionDelete,
	})
	if err != nil {
		return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	return &metav1.Status{Status: metav1.StatusSuccess}, true, nil
}

// ConvertToTable converts a workspace object or list into kubectl table output.
func (s *WorkspaceStorage) ConvertToTable(ctx context.Context, object, tableOptions runtime.Object) (*metav1.Table, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: workspace storage must not be nil")
	}
	if s.tableConvertor == nil {
		return nil, fmt.Errorf("assertion failed: workspace table convertor must not be nil")
	}

	return s.tableConvertor.ConvertToTable(ctx, object, tableOptions)
}

func (s *WorkspaceStorage) clientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("assertion failed: workspace client provider must not be nil")
	}

	sdk, err := s.provider.ClientForNamespace(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("resolve codersdk client for namespace %q: %w", namespace, err)
	}
	if sdk == nil {
		return nil, fmt.Errorf("assertion failed: workspace client provider returned nil codersdk client")
	}

	return sdk, nil
}

func namespaceFromRequestContext(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("assertion failed: context must not be nil")
	}

	return genericapirequest.NamespaceValue(ctx), nil
}

func requiredNamespaceFromRequestContext(ctx context.Context) (string, error) {
	namespace, err := namespaceFromRequestContext(ctx)
	if err != nil {
		return "", err
	}
	if namespace == "" {
		return "", apierrors.NewBadRequest("namespace is required")
	}

	return namespace, nil
}

func namespaceForListConversion(requestNamespace string, provider coder.ClientProvider) (string, error) {
	if requestNamespace != "" {
		return requestNamespace, nil
	}
	if provider == nil {
		return "", fmt.Errorf("assertion failed: client provider must not be nil")
	}

	staticProvider, ok := provider.(*coder.StaticClientProvider)
	if !ok || staticProvider.Namespace == "" {
		return "", apierrors.NewServiceUnavailable(
			"all-namespaces list requires a namespace-pinned static provider; configure --coder-namespace",
		)
	}

	return staticProvider.Namespace, nil
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	return *a == *b
}

func equalStringPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	return *a == *b
}
