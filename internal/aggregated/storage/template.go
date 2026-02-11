package storage

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

var (
	_ rest.Storage              = (*TemplateStorage)(nil)
	_ rest.Getter               = (*TemplateStorage)(nil)
	_ rest.Lister               = (*TemplateStorage)(nil)
	_ rest.Updater              = (*TemplateStorage)(nil)
	_ rest.GracefulDeleter      = (*TemplateStorage)(nil)
	_ rest.Scoper               = (*TemplateStorage)(nil)
	_ rest.SingularNameProvider = (*TemplateStorage)(nil)
)

// TemplateStorage provides hardcoded CoderTemplate objects.
type TemplateStorage struct {
	mu             sync.RWMutex
	tableConvertor rest.TableConvertor
	templates      map[string]*aggregationv1alpha1.CoderTemplate
}

// NewTemplateStorage builds hardcoded storage for CoderTemplate resources.
func NewTemplateStorage() *TemplateStorage {
	starterDeadline := metav1.NewTime(time.Date(2030, time.January, 4, 18, 0, 0, 0, time.UTC))
	platformDeadline := metav1.NewTime(time.Date(2030, time.January, 5, 18, 0, 0, 0, time.UTC))
	docsDeadline := metav1.NewTime(time.Date(2030, time.January, 6, 18, 0, 0, 0, time.UTC))

	return &TemplateStorage{
		tableConvertor: rest.NewDefaultTableConvertor(aggregationv1alpha1.Resource("codertemplates")),
		templates: map[string]*aggregationv1alpha1.CoderTemplate{
			templateKey("default", "starter-template"): {
				TypeMeta: metav1.TypeMeta{
					Kind:       "CoderTemplate",
					APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:            "starter-template",
					Namespace:       "default",
					ResourceVersion: "1",
					Generation:      1,
				},
				Spec: aggregationv1alpha1.CoderTemplateSpec{Running: true},
				Status: aggregationv1alpha1.CoderTemplateStatus{
					AutoShutdown: &starterDeadline,
				},
			},
			templateKey("default", "platform-template"): {
				TypeMeta: metav1.TypeMeta{
					Kind:       "CoderTemplate",
					APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:            "platform-template",
					Namespace:       "default",
					ResourceVersion: "1",
					Generation:      1,
				},
				Spec: aggregationv1alpha1.CoderTemplateSpec{Running: false},
				Status: aggregationv1alpha1.CoderTemplateStatus{
					AutoShutdown: &platformDeadline,
				},
			},
			templateKey("sandbox", "docs-template"): {
				TypeMeta: metav1.TypeMeta{
					Kind:       "CoderTemplate",
					APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:            "docs-template",
					Namespace:       "sandbox",
					ResourceVersion: "1",
					Generation:      1,
				},
				Spec: aggregationv1alpha1.CoderTemplateSpec{Running: true},
				Status: aggregationv1alpha1.CoderTemplateStatus{
					AutoShutdown: &docsDeadline,
				},
			},
		},
	}
}

func templateKey(namespace, name string) string {
	return namespace + "/" + name
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

// Get returns a hardcoded CoderTemplate by name.
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

	namespace := genericapirequest.NamespaceValue(ctx)

	s.mu.RLock()
	defer s.mu.RUnlock()

	if namespace != "" {
		template, ok := s.templates[templateKey(namespace, name)]
		if !ok {
			return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), name)
		}
		return template.DeepCopy(), nil
	}

	template, found, ambiguous := s.findTemplateByNameLocked(name)
	if ambiguous {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("template name %q is ambiguous across namespaces; specify namespace", name))
	}
	if !found {
		return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), name)
	}
	return template.DeepCopy(), nil
}

// List returns hardcoded CoderTemplate objects.
func (s *TemplateStorage) List(ctx context.Context, _ *metainternalversion.ListOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}

	namespace := genericapirequest.NamespaceValue(ctx)
	list := &aggregationv1alpha1.CoderTemplateList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderTemplateList",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		Items: make([]aggregationv1alpha1.CoderTemplate, 0),
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]string, 0, len(s.templates))
	for key := range s.templates {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		template := s.templates[key]
		if namespace != "" && template.Namespace != namespace {
			continue
		}
		list.Items = append(list.Items, *template.DeepCopy())
	}

	return list, nil
}

// Create inserts a CoderTemplate into the in-memory store.
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

	template, ok := obj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderTemplate, got %T", obj))
	}

	candidate := template.DeepCopy()
	if candidate.Name == "" {
		return nil, apierrors.NewBadRequest("metadata.name is required")
	}

	namespace, err := resolveWriteNamespace(ctx, candidate.Namespace)
	if err != nil {
		return nil, err
	}
	candidate.Namespace = namespace

	ensureTemplateTypeMeta(candidate)
	if candidate.Generation == 0 {
		candidate.Generation = 1
	}
	if candidate.CreationTimestamp.IsZero() {
		candidate.CreationTimestamp = metav1.Now()
	}
	candidate.ResourceVersion = "1"

	if createValidation != nil {
		if err := createValidation(ctx, candidate); err != nil {
			return nil, err
		}
	}

	key := templateKey(candidate.Namespace, candidate.Name)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.templates[key]; exists {
		return nil, apierrors.NewAlreadyExists(aggregationv1alpha1.Resource("codertemplates"), candidate.Name)
	}

	s.templates[key] = candidate.DeepCopy()
	return candidate.DeepCopy(), nil
}

// Update modifies an existing CoderTemplate in the in-memory store.
func (s *TemplateStorage) Update(
	ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc,
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

	namespace := genericapirequest.NamespaceValue(ctx)
	if namespace == "" {
		return nil, false, apierrors.NewBadRequest("namespace is required")
	}

	key := templateKey(namespace, name)

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.templates[key]
	if !exists {
		if !forceAllowCreate {
			return nil, false, apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), name)
		}

		createdObj, err := objInfo.UpdatedObject(ctx, &aggregationv1alpha1.CoderTemplate{})
		if err != nil {
			return nil, false, err
		}
		createdTemplate, ok := createdObj.(*aggregationv1alpha1.CoderTemplate)
		if !ok {
			return nil, false, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderTemplate, got %T", createdObj))
		}

		candidate := createdTemplate.DeepCopy()
		if candidate.Name == "" {
			candidate.Name = name
		}
		if candidate.Name != name {
			return nil, false, apierrors.NewBadRequest(fmt.Sprintf("metadata.name %q must match request name %q", candidate.Name, name))
		}
		if candidate.Namespace == "" {
			candidate.Namespace = namespace
		}
		if candidate.Namespace != namespace {
			return nil, false, apierrors.NewBadRequest(
				fmt.Sprintf("metadata.namespace %q must match request namespace %q", candidate.Namespace, namespace),
			)
		}

		ensureTemplateTypeMeta(candidate)
		if candidate.Generation == 0 {
			candidate.Generation = 1
		}
		if candidate.CreationTimestamp.IsZero() {
			candidate.CreationTimestamp = metav1.Now()
		}
		candidate.ResourceVersion = "1"

		if createValidation != nil {
			if err := createValidation(ctx, candidate); err != nil {
				return nil, false, err
			}
		}

		s.templates[key] = candidate.DeepCopy()
		return candidate.DeepCopy(), true, nil
	}

	updatedObj, err := objInfo.UpdatedObject(ctx, existing.DeepCopy())
	if err != nil {
		return nil, false, err
	}
	updatedTemplate, ok := updatedObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderTemplate, got %T", updatedObj))
	}

	candidate := updatedTemplate.DeepCopy()
	if candidate.Name == "" {
		candidate.Name = name
	}
	if candidate.Name != name {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("metadata.name %q must match request name %q", candidate.Name, name))
	}
	if candidate.Namespace == "" {
		candidate.Namespace = namespace
	}
	if candidate.Namespace != namespace {
		return nil, false, apierrors.NewBadRequest(
			fmt.Sprintf("metadata.namespace %q must match request namespace %q", candidate.Namespace, namespace),
		)
	}

	if candidate.ResourceVersion == "" {
		return nil, false, apierrors.NewBadRequest("metadata.resourceVersion is required for update")
	}
	if candidate.ResourceVersion != existing.ResourceVersion {
		return nil, false, apierrors.NewConflict(
			aggregationv1alpha1.Resource("codertemplates"),
			name,
			fmt.Errorf("resourceVersion %q does not match current value %q", candidate.ResourceVersion, existing.ResourceVersion),
		)
	}

	candidate.Status = existing.Status
	candidate.CreationTimestamp = existing.CreationTimestamp
	candidate.Generation = existing.Generation + 1
	candidateFinalResourceVersion, err := incrementResourceVersion(existing.ResourceVersion)
	if err != nil {
		return nil, false, err
	}
	candidate.ResourceVersion = candidateFinalResourceVersion
	ensureTemplateTypeMeta(candidate)

	if updateValidation != nil {
		if err := updateValidation(ctx, candidate, existing); err != nil {
			return nil, false, err
		}
	}

	s.templates[key] = candidate.DeepCopy()
	return candidate.DeepCopy(), false, nil
}

// Delete removes a CoderTemplate from the in-memory store.
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

	namespace := genericapirequest.NamespaceValue(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		key      string
		template *aggregationv1alpha1.CoderTemplate
	)
	if namespace != "" {
		key = templateKey(namespace, name)
		template = s.templates[key]
	} else {
		matchedKeys := make([]string, 0)
		for candidateKey, candidateTemplate := range s.templates {
			if candidateTemplate.Name == name {
				matchedKeys = append(matchedKeys, candidateKey)
			}
		}
		if len(matchedKeys) > 1 {
			return nil, false, apierrors.NewBadRequest(
				fmt.Sprintf("template name %q is ambiguous across namespaces; specify namespace", name),
			)
		}
		if len(matchedKeys) == 1 {
			key = matchedKeys[0]
			template = s.templates[key]
		}
	}

	if template == nil {
		return nil, false, apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), name)
	}

	if deleteValidation != nil {
		if err := deleteValidation(ctx, template.DeepCopy()); err != nil {
			return nil, false, err
		}
	}

	deleted := template.DeepCopy()
	delete(s.templates, key)
	return deleted, true, nil
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

func ensureTemplateTypeMeta(template *aggregationv1alpha1.CoderTemplate) {
	if template == nil {
		panic("assertion failed: template must not be nil")
	}
	template.TypeMeta = metav1.TypeMeta{
		Kind:       "CoderTemplate",
		APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
	}
}

func (s *TemplateStorage) findTemplateByNameLocked(name string) (*aggregationv1alpha1.CoderTemplate, bool, bool) {
	matchedKeys := make([]string, 0)
	for key, template := range s.templates {
		if template.Name == name {
			matchedKeys = append(matchedKeys, key)
		}
	}
	if len(matchedKeys) == 0 {
		return nil, false, false
	}
	if len(matchedKeys) > 1 {
		return nil, false, true
	}
	template := s.templates[matchedKeys[0]]
	if template == nil {
		return nil, false, false
	}
	return template, true, false
}
