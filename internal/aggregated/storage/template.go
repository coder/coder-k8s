package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
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

const (
	templateVersionBuildWaitTimeoutEnv         = "CODER_K8S_TEMPLATE_BUILD_WAIT_TIMEOUT"
	templateVersionBuildBackoffAfterEnv        = "CODER_K8S_TEMPLATE_BUILD_BACKOFF_AFTER"
	templateVersionBuildInitialPollIntervalEnv = "CODER_K8S_TEMPLATE_BUILD_INITIAL_POLL_INTERVAL"
	templateVersionBuildMaxPollIntervalEnv     = "CODER_K8S_TEMPLATE_BUILD_MAX_POLL_INTERVAL"
	defaultTemplateVersionBuildWaitTimeout     = 25 * time.Minute
	defaultTemplateVersionBuildBackoffAfter    = 2 * time.Minute
	defaultTemplateVersionBuildInitialPoll     = 2 * time.Second
	defaultTemplateVersionBuildMaxPollInterval = 10 * time.Second
	// MaxTemplateVersionBuildWaitTimeout keeps template build waits within aggregated API request deadlines.
	MaxTemplateVersionBuildWaitTimeout = 30 * time.Minute
)

type templateVersionBuildWaitConfig struct {
	waitTimeout     time.Duration
	backoffAfter    time.Duration
	initialPoll     time.Duration
	maxPollInterval time.Duration
}

var (
	_ rest.Storage              = (*TemplateStorage)(nil)
	_ rest.Getter               = (*TemplateStorage)(nil)
	_ rest.Lister               = (*TemplateStorage)(nil)
	_ rest.Creater              = (*TemplateStorage)(nil) //nolint:misspell // Kubernetes rest interface name is Creater.
	_ rest.Updater              = (*TemplateStorage)(nil)
	_ rest.GracefulDeleter      = (*TemplateStorage)(nil)
	_ rest.Scoper               = (*TemplateStorage)(nil)
	_ rest.SingularNameProvider = (*TemplateStorage)(nil)

	errTemplateVersionBuildWaitTimeoutExceeded = errors.New("template version build wait timeout exceeded")
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

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
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

	obj := convert.TemplateToK8s(namespace, template)

	files, err := fetchTemplateSourceFiles(ctx, sdk, template.ActiveVersionID)
	if err != nil {
		return nil, fmt.Errorf("fetch template source files: %w", err)
	}
	obj.Spec.Files = files

	return obj, nil
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

	responseNamespace, responseNamespaceErr := namespaceForListConversion(ctx, namespace, s.provider)
	if responseNamespaceErr != nil {
		return nil, responseNamespaceErr
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
		list.Items = append(list.Items, *convert.TemplateToK8s(responseNamespace, template))
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

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
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

	if templateObj.Spec.Files != nil {
		zipBytes, err := buildSourceZip(templateObj.Spec.Files)
		if err != nil {
			return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid template spec.files: %v", err))
		}

		uploadResponse, err := sdk.Upload(ctx, codersdk.ContentTypeZip, bytes.NewReader(zipBytes))
		if err != nil {
			return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), templateObj.Name)
		}

		templateVersion, err := sdk.CreateTemplateVersion(ctx, org.ID, codersdk.CreateTemplateVersionRequest{
			StorageMethod: codersdk.ProvisionerStorageMethodFile,
			FileID:        uploadResponse.ID,
			Provisioner:   codersdk.ProvisionerTypeTerraform,
		})
		if err != nil {
			return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), templateObj.Name)
		}

		createdTemplate, err := sdk.CreateTemplate(ctx, org.ID, codersdk.CreateTemplateRequest{
			Name:        templateName,
			VersionID:   templateVersion.ID,
			DisplayName: templateObj.Spec.DisplayName,
			Description: templateObj.Spec.Description,
			Icon:        templateObj.Spec.Icon,
		})
		if err != nil {
			return nil, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), templateObj.Name)
		}

		return convert.TemplateToK8s(namespace, createdTemplate), nil
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

// Update applies a template metadata/source reconcile.
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
	updatedTemplate, ok := updatedObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		return nil, false, fmt.Errorf("assertion failed: expected *CoderTemplate, got %T", updatedObj)
	}
	currentTemplate, ok := currentObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		return nil, false, fmt.Errorf("assertion failed: expected *CoderTemplate, got %T", currentObj)
	}

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, false, badNamespaceErr
	}
	if updatedTemplate.Name != "" && updatedTemplate.Name != name {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf("updated object metadata.name %q must match request name %q", updatedTemplate.Name, name),
		)
	}
	if updatedTemplate.Namespace != "" && updatedTemplate.Namespace != namespace {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf("metadata.namespace %q does not match request namespace %q", updatedTemplate.Namespace, namespace),
		)
	}
	if updatedTemplate.ResourceVersion == "" {
		return nil, false, apierrors.NewBadRequest("metadata.resourceVersion is required for update")
	}
	if updatedTemplate.ResourceVersion != currentTemplate.ResourceVersion {
		return nil, false, apierrors.NewConflict(
			aggregationv1alpha1.Resource("codertemplates"),
			name,
			fmt.Errorf(
				"resource version mismatch: got %q, current is %q",
				updatedTemplate.ResourceVersion,
				currentTemplate.ResourceVersion,
			),
		)
	}
	if updateValidation != nil {
		if err := updateValidation(ctx, updatedTemplate, currentTemplate); err != nil {
			return nil, false, err
		}
	}
	if updatedTemplate.Spec.Organization != currentTemplate.Spec.Organization {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf(
				"spec.organization %q must match existing organization %q",
				updatedTemplate.Spec.Organization,
				currentTemplate.Spec.Organization,
			),
		)
	}

	// spec.versionID is informational (populated from the backend active version).
	// Reject explicit non-empty mutations to avoid silent drift when the active version remains unchanged.
	// Allow empty desired values so GitOps clients can omit this informational field on updates.
	if updatedTemplate.Spec.VersionID != "" && updatedTemplate.Spec.VersionID != currentTemplate.Spec.VersionID {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf(
				"spec.versionID is read-only; to change the active version, update spec.files instead (current: %q, requested: %q)",
				currentTemplate.Spec.VersionID,
				updatedTemplate.Spec.VersionID,
			),
		)
	}

	templateID, err := uuid.Parse(currentTemplate.Status.ID)
	if err != nil {
		return nil, false, fmt.Errorf("parse current template status.id %q: %w", currentTemplate.Status.ID, err)
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, false, wrapClientError(err)
	}

	// Pre-validate spec.files before any mutations to avoid partial updates.
	var normalizedDesiredFiles map[string]string
	if updatedTemplate.Spec.Files != nil {
		var normalizeErr error
		normalizedDesiredFiles, normalizeErr = normalizeFileKeys(updatedTemplate.Spec.Files)
		if normalizeErr != nil {
			return nil, false, apierrors.NewBadRequest(fmt.Sprintf("invalid template spec.files: %v", normalizeErr))
		}
		// Validate that files can be built into a zip (path/size/UTF-8 checks).
		if _, buildErr := buildSourceZip(normalizedDesiredFiles); buildErr != nil {
			return nil, false, apierrors.NewBadRequest(fmt.Sprintf("invalid template spec.files: %v", buildErr))
		}
	}

	metadataChanged := updatedTemplate.Spec.DisplayName != currentTemplate.Spec.DisplayName ||
		updatedTemplate.Spec.Description != currentTemplate.Spec.Description ||
		updatedTemplate.Spec.Icon != currentTemplate.Spec.Icon
	if metadataChanged {
		_, err := sdk.UpdateTemplateMeta(ctx, templateID, convert.TemplateUpdateMetaRequestFromK8s(updatedTemplate))
		if err != nil {
			return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
		}
	}

	if updatedTemplate.Spec.Files != nil {
		if normalizedDesiredFiles == nil {
			return nil, false, fmt.Errorf("assertion failed: normalized desired template files must not be nil when spec.files is provided")
		}

		currentActiveVersionID, err := uuid.Parse(currentTemplate.Status.ActiveVersionID)
		if err != nil {
			return nil, false, fmt.Errorf(
				"parse current template status.activeVersionID %q: %w",
				currentTemplate.Status.ActiveVersionID,
				err,
			)
		}

		currentFiles, err := fetchTemplateSourceFiles(ctx, sdk, currentActiveVersionID)
		if err != nil {
			return nil, false, fmt.Errorf("fetch current template source files: %w", err)
		}

		if !reflect.DeepEqual(normalizedDesiredFiles, currentFiles) {
			currentRawSourceZip, err := fetchRawTemplateSourceZip(ctx, sdk, currentActiveVersionID)
			if err != nil {
				return nil, false, fmt.Errorf("fetch current template source zip: %w", err)
			}

			zipBytes, err := buildMergedSourceZip(currentRawSourceZip, normalizedDesiredFiles)
			if err != nil {
				return nil, false, apierrors.NewBadRequest(fmt.Sprintf("invalid template spec.files: %v", err))
			}

			uploadResponse, err := sdk.Upload(ctx, codersdk.ContentTypeZip, bytes.NewReader(zipBytes))
			if err != nil {
				return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
			}
			if uploadResponse.ID == uuid.Nil {
				return nil, false, fmt.Errorf("assertion failed: uploaded file ID must not be nil")
			}

			org, err := sdk.OrganizationByName(ctx, currentTemplate.Spec.Organization)
			if err != nil {
				return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
			}

			newVersion, err := sdk.CreateTemplateVersion(ctx, org.ID, codersdk.CreateTemplateVersionRequest{
				TemplateID:    templateID,
				StorageMethod: codersdk.ProvisionerStorageMethodFile,
				FileID:        uploadResponse.ID,
				Provisioner:   codersdk.ProvisionerTypeTerraform,
			})
			if err != nil {
				return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
			}
			if newVersion.ID == uuid.Nil {
				return nil, false, fmt.Errorf("assertion failed: new template version ID must not be nil")
			}

			if waitErr := waitForTemplateVersionBuild(ctx, sdk, newVersion.ID); waitErr != nil {
				return nil, false, mapTemplateVersionBuildWaitError(waitErr, name)
			}

			if err := sdk.UpdateActiveTemplateVersion(ctx, templateID, codersdk.UpdateActiveTemplateVersion{ID: newVersion.ID}); err != nil {
				return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
			}

			// Post-condition: verify promotion succeeded. The vendored SDK silently
			// swallows transport errors in UpdateActiveTemplateVersion, so we must
			// confirm the active version actually changed.
			verifyTemplate, err := sdk.Template(ctx, templateID)
			if err != nil {
				return nil, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
			}
			if verifyTemplate.ActiveVersionID != newVersion.ID {
				return nil, false, fmt.Errorf(
					"assertion failed: active version promotion did not take effect: expected %q, got %q",
					newVersion.ID.String(),
					verifyTemplate.ActiveVersionID.String(),
				)
			}
		}
	}

	refreshedObj, err := s.Get(ctx, name, nil)
	if err != nil {
		return nil, false, err
	}

	return refreshedObj, false, nil
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

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
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

type templateVersionBuildTerminalError struct {
	versionID uuid.UUID
	status    codersdk.ProvisionerJobStatus
	detail    string
}

func (e *templateVersionBuildTerminalError) Error() string {
	if e == nil {
		panic("assertion failed: template version build terminal error must not be nil")
	}
	if e.versionID == uuid.Nil {
		panic("assertion failed: template version build terminal error must include version ID")
	}
	if e.status == "" {
		panic("assertion failed: template version build terminal error must include status")
	}

	detail := e.detail
	if detail == "" {
		detail = "template version build did not succeed"
	}

	return fmt.Sprintf(
		"template version %q build ended with status %q: %s",
		e.versionID.String(),
		e.status,
		detail,
	)
}

type templateVersionBuildWaitTimeoutError struct {
	versionID   uuid.UUID
	waitTimeout time.Duration
	lastStatus  codersdk.ProvisionerJobStatus
	lastError   string
	cause       error
}

func (e *templateVersionBuildWaitTimeoutError) Error() string {
	if e == nil {
		panic("assertion failed: template version build timeout error must not be nil")
	}
	if e.versionID == uuid.Nil {
		panic("assertion failed: template version build timeout error must include version ID")
	}
	if e.waitTimeout <= 0 {
		panic("assertion failed: template version build timeout error must include positive timeout")
	}
	if e.lastStatus == "" {
		panic("assertion failed: template version build timeout error must include last status")
	}
	if e.cause == nil {
		panic("assertion failed: template version build timeout error must include cause")
	}

	if e.lastError == "" {
		return fmt.Sprintf(
			"template version %q did not succeed within %s (last status: %q): %v",
			e.versionID.String(),
			e.waitTimeout,
			e.lastStatus,
			e.cause,
		)
	}

	return fmt.Sprintf(
		"template version %q did not succeed within %s (last status: %q, last error: %s): %v",
		e.versionID.String(),
		e.waitTimeout,
		e.lastStatus,
		e.lastError,
		e.cause,
	)
}

func mapTemplateVersionBuildWaitError(waitErr error, templateName string) error {
	if waitErr == nil {
		return fmt.Errorf("assertion failed: wait error must not be nil")
	}
	if templateName == "" {
		return fmt.Errorf("assertion failed: template name must not be empty")
	}

	var terminalErr *templateVersionBuildTerminalError
	if errors.As(waitErr, &terminalErr) {
		return apierrors.NewBadRequest(terminalErr.Error())
	}

	var timeoutErr *templateVersionBuildWaitTimeoutError
	if errors.As(waitErr, &timeoutErr) {
		switch {
		case errors.Is(timeoutErr.cause, errTemplateVersionBuildWaitTimeoutExceeded):
			return apierrors.NewBadRequest(timeoutErr.Error())
		case errors.Is(timeoutErr.cause, context.DeadlineExceeded), errors.Is(timeoutErr.cause, context.Canceled):
			return apierrors.NewTimeoutError(timeoutErr.Error(), 0)
		default:
			return apierrors.NewInternalError(timeoutErr)
		}
	}

	return coder.MapCoderError(waitErr, aggregationv1alpha1.Resource("codertemplates"), templateName)
}

func waitForTemplateVersionBuild(ctx context.Context, sdk *codersdk.Client, versionID uuid.UUID) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}
	if sdk == nil {
		return fmt.Errorf("assertion failed: codersdk client must not be nil")
	}
	if versionID == uuid.Nil {
		return fmt.Errorf("assertion failed: template version ID must not be nil")
	}

	waitConfig, waitConfigErr := loadTemplateVersionBuildWaitConfigFromEnv()
	if waitConfigErr != nil {
		return waitConfigErr
	}

	effectiveWaitTimeout := waitConfig.waitTimeout
	if requestDeadline, hasRequestDeadline := ctx.Deadline(); hasRequestDeadline {
		remaining := time.Until(requestDeadline)
		if remaining > 0 && remaining < effectiveWaitTimeout {
			effectiveWaitTimeout = remaining
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, effectiveWaitTimeout)
	defer cancel()

	pollInterval := waitConfig.initialPoll
	pollStartTime := time.Now()

	lastStatus := codersdk.ProvisionerJobUnknown
	lastError := ""
	for {
		version, err := sdk.TemplateVersion(waitCtx, versionID)
		if err != nil {
			if waitCtx.Err() != nil {
				timeoutCause, timeoutCauseErr := templateVersionBuildWaitTimeoutCause(ctx, waitCtx)
				if timeoutCauseErr != nil {
					return timeoutCauseErr
				}

				return &templateVersionBuildWaitTimeoutError{
					versionID:   versionID,
					waitTimeout: effectiveWaitTimeout,
					lastStatus:  lastStatus,
					lastError:   lastError,
					cause:       timeoutCause,
				}
			}
			return fmt.Errorf("fetch template version %q: %w", versionID.String(), err)
		}

		status := version.Job.Status
		if status == "" {
			status = codersdk.ProvisionerJobUnknown
		}
		lastStatus = status
		lastError = version.Job.Error

		switch status {
		case codersdk.ProvisionerJobSucceeded:
			return nil
		case codersdk.ProvisionerJobFailed, codersdk.ProvisionerJobCanceled:
			if lastError == "" {
				lastError = "template version build did not succeed"
			}
			return &templateVersionBuildTerminalError{
				versionID: versionID,
				status:    status,
				detail:    lastError,
			}
		case codersdk.ProvisionerJobPending, codersdk.ProvisionerJobRunning, codersdk.ProvisionerJobCanceling, codersdk.ProvisionerJobUnknown:
			// Keep polling below.
		default:
			return fmt.Errorf(
				"assertion failed: unexpected template version build status %q for version %q",
				status,
				versionID.String(),
			)
		}

		if waitConfig.backoffAfter > 0 && time.Since(pollStartTime) >= waitConfig.backoffAfter && pollInterval < waitConfig.maxPollInterval {
			nextPollInterval := pollInterval * 2
			if nextPollInterval <= 0 || nextPollInterval > waitConfig.maxPollInterval {
				nextPollInterval = waitConfig.maxPollInterval
			}
			pollInterval = nextPollInterval
		}

		select {
		case <-waitCtx.Done():
			timeoutCause, timeoutCauseErr := templateVersionBuildWaitTimeoutCause(ctx, waitCtx)
			if timeoutCauseErr != nil {
				return timeoutCauseErr
			}

			return &templateVersionBuildWaitTimeoutError{
				versionID:   versionID,
				waitTimeout: effectiveWaitTimeout,
				lastStatus:  lastStatus,
				lastError:   lastError,
				cause:       timeoutCause,
			}
		case <-time.After(pollInterval):
		}
	}
}

func templateVersionBuildWaitTimeoutCause(requestCtx, waitCtx context.Context) (error, error) {
	if requestCtx == nil {
		return nil, fmt.Errorf("assertion failed: request context must not be nil")
	}
	if waitCtx == nil {
		return nil, fmt.Errorf("assertion failed: wait context must not be nil")
	}

	waitErr := waitCtx.Err()
	if waitErr == nil {
		return nil, fmt.Errorf("assertion failed: wait context finished without an error")
	}

	if requestErr := requestCtx.Err(); requestErr != nil {
		return requestErr, nil
	}

	if errors.Is(waitErr, context.DeadlineExceeded) {
		return errTemplateVersionBuildWaitTimeoutExceeded, nil
	}

	return waitErr, nil
}

func loadTemplateVersionBuildWaitConfigFromEnv() (templateVersionBuildWaitConfig, error) {
	waitTimeout, waitTimeoutErr := parseDurationEnvOrDefault(templateVersionBuildWaitTimeoutEnv, defaultTemplateVersionBuildWaitTimeout)
	if waitTimeoutErr != nil {
		return templateVersionBuildWaitConfig{}, waitTimeoutErr
	}
	if waitTimeout <= 0 {
		return templateVersionBuildWaitConfig{}, fmt.Errorf(
			"assertion failed: %s must be > 0, got %s",
			templateVersionBuildWaitTimeoutEnv,
			waitTimeout,
		)
	}

	if waitTimeout > MaxTemplateVersionBuildWaitTimeout {
		return templateVersionBuildWaitConfig{}, fmt.Errorf(
			"assertion failed: %s (%s) must be <= %s (%s)",
			templateVersionBuildWaitTimeoutEnv,
			waitTimeout,
			"aggregated API request timeout",
			MaxTemplateVersionBuildWaitTimeout,
		)
	}

	backoffAfter, backoffAfterErr := parseDurationEnvOrDefault(templateVersionBuildBackoffAfterEnv, defaultTemplateVersionBuildBackoffAfter)
	if backoffAfterErr != nil {
		return templateVersionBuildWaitConfig{}, backoffAfterErr
	}
	if backoffAfter < 0 {
		return templateVersionBuildWaitConfig{}, fmt.Errorf(
			"assertion failed: %s must be >= 0, got %s",
			templateVersionBuildBackoffAfterEnv,
			backoffAfter,
		)
	}
	if backoffAfter > waitTimeout {
		return templateVersionBuildWaitConfig{}, fmt.Errorf(
			"assertion failed: %s (%s) must be <= %s (%s)",
			templateVersionBuildBackoffAfterEnv,
			backoffAfter,
			templateVersionBuildWaitTimeoutEnv,
			waitTimeout,
		)
	}

	initialPollInterval, initialPollIntervalErr := parseDurationEnvOrDefault(templateVersionBuildInitialPollIntervalEnv, defaultTemplateVersionBuildInitialPoll)
	if initialPollIntervalErr != nil {
		return templateVersionBuildWaitConfig{}, initialPollIntervalErr
	}
	if initialPollInterval <= 0 {
		return templateVersionBuildWaitConfig{}, fmt.Errorf(
			"assertion failed: %s must be > 0, got %s",
			templateVersionBuildInitialPollIntervalEnv,
			initialPollInterval,
		)
	}

	maxPollInterval, maxPollIntervalErr := parseDurationEnvOrDefault(templateVersionBuildMaxPollIntervalEnv, defaultTemplateVersionBuildMaxPollInterval)
	if maxPollIntervalErr != nil {
		return templateVersionBuildWaitConfig{}, maxPollIntervalErr
	}
	if maxPollInterval <= 0 {
		return templateVersionBuildWaitConfig{}, fmt.Errorf(
			"assertion failed: %s must be > 0, got %s",
			templateVersionBuildMaxPollIntervalEnv,
			maxPollInterval,
		)
	}
	if maxPollInterval < initialPollInterval {
		return templateVersionBuildWaitConfig{}, fmt.Errorf(
			"assertion failed: %s (%s) must be >= %s (%s)",
			templateVersionBuildMaxPollIntervalEnv,
			maxPollInterval,
			templateVersionBuildInitialPollIntervalEnv,
			initialPollInterval,
		)
	}

	return templateVersionBuildWaitConfig{
		waitTimeout:     waitTimeout,
		backoffAfter:    backoffAfter,
		initialPoll:     initialPollInterval,
		maxPollInterval: maxPollInterval,
	}, nil
}

func parseDurationEnvOrDefault(envName string, defaultValue time.Duration) (time.Duration, error) {
	if envName == "" {
		return 0, fmt.Errorf("assertion failed: environment variable name must not be empty")
	}

	rawValue := strings.TrimSpace(os.Getenv(envName))
	if rawValue == "" {
		return defaultValue, nil
	}

	parsedValue, parseErr := time.ParseDuration(rawValue)
	if parseErr != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", envName, rawValue, parseErr)
	}

	return parsedValue, nil
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
