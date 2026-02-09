package storage

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

var _ rest.Storage = (*TemplateStorage)(nil)
var _ rest.Getter = (*TemplateStorage)(nil)
var _ rest.Lister = (*TemplateStorage)(nil)
var _ rest.Scoper = (*TemplateStorage)(nil)
var _ rest.SingularNameProvider = (*TemplateStorage)(nil)

// TemplateStorage provides hardcoded CoderTemplate objects.
type TemplateStorage struct {
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
					Name:      "starter-template",
					Namespace: "default",
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
					Name:      "platform-template",
					Namespace: "default",
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
					Name:      "docs-template",
					Namespace: "sandbox",
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
	if namespace != "" {
		if template, ok := s.templates[templateKey(namespace, name)]; ok {
			return template.DeepCopy(), nil
		}
		return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), name)
	}

	for _, template := range s.templates {
		if template.Name == name {
			return template.DeepCopy(), nil
		}
	}

	return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), name)
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
		Items: make([]aggregationv1alpha1.CoderTemplate, 0, len(s.templates)),
	}

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

// ConvertToTable converts a template object or list into kubectl table output.
func (s *TemplateStorage) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template storage must not be nil")
	}
	if s.tableConvertor == nil {
		return nil, fmt.Errorf("assertion failed: template table convertor must not be nil")
	}

	return s.tableConvertor.ConvertToTable(ctx, object, tableOptions)
}
