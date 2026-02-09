// Package storage provides hardcoded in-memory storage implementations for aggregated API resources.
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

var (
	_ rest.Storage              = (*WorkspaceStorage)(nil)
	_ rest.Getter               = (*WorkspaceStorage)(nil)
	_ rest.Lister               = (*WorkspaceStorage)(nil)
	_ rest.Scoper               = (*WorkspaceStorage)(nil)
	_ rest.SingularNameProvider = (*WorkspaceStorage)(nil)
)

// WorkspaceStorage provides hardcoded CoderWorkspace objects.
type WorkspaceStorage struct {
	tableConvertor rest.TableConvertor
	workspaces     map[string]*aggregationv1alpha1.CoderWorkspace
}

// NewWorkspaceStorage builds hardcoded storage for CoderWorkspace resources.
func NewWorkspaceStorage() *WorkspaceStorage {
	workspaceDeadline := metav1.NewTime(time.Date(2030, time.January, 1, 18, 0, 0, 0, time.UTC))
	stagingDeadline := metav1.NewTime(time.Date(2030, time.January, 2, 18, 0, 0, 0, time.UTC))
	sandboxDeadline := metav1.NewTime(time.Date(2030, time.January, 3, 18, 0, 0, 0, time.UTC))

	return &WorkspaceStorage{
		tableConvertor: rest.NewDefaultTableConvertor(aggregationv1alpha1.Resource("coderworkspaces")),
		workspaces: map[string]*aggregationv1alpha1.CoderWorkspace{
			workspaceKey("default", "dev-workspace"): {
				TypeMeta: metav1.TypeMeta{
					Kind:       "CoderWorkspace",
					APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dev-workspace",
					Namespace: "default",
				},
				Spec: aggregationv1alpha1.CoderWorkspaceSpec{Running: true},
				Status: aggregationv1alpha1.CoderWorkspaceStatus{
					AutoShutdown: &workspaceDeadline,
				},
			},
			workspaceKey("default", "staging-workspace"): {
				TypeMeta: metav1.TypeMeta{
					Kind:       "CoderWorkspace",
					APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "staging-workspace",
					Namespace: "default",
				},
				Spec: aggregationv1alpha1.CoderWorkspaceSpec{Running: false},
				Status: aggregationv1alpha1.CoderWorkspaceStatus{
					AutoShutdown: &stagingDeadline,
				},
			},
			workspaceKey("sandbox", "sandbox-workspace"): {
				TypeMeta: metav1.TypeMeta{
					Kind:       "CoderWorkspace",
					APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-workspace",
					Namespace: "sandbox",
				},
				Spec: aggregationv1alpha1.CoderWorkspaceSpec{Running: true},
				Status: aggregationv1alpha1.CoderWorkspaceStatus{
					AutoShutdown: &sandboxDeadline,
				},
			},
		},
	}
}

func workspaceKey(namespace, name string) string {
	return namespace + "/" + name
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

// Get returns a hardcoded CoderWorkspace by name.
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

	namespace := genericapirequest.NamespaceValue(ctx)
	if namespace != "" {
		if workspace, ok := s.workspaces[workspaceKey(namespace, name)]; ok {
			return workspace.DeepCopy(), nil
		}
		return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	for _, workspace := range s.workspaces {
		if workspace.Name == name {
			return workspace.DeepCopy(), nil
		}
	}

	return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
}

// List returns hardcoded CoderWorkspace objects.
func (s *WorkspaceStorage) List(ctx context.Context, _ *metainternalversion.ListOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: workspace storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}

	namespace := genericapirequest.NamespaceValue(ctx)
	list := &aggregationv1alpha1.CoderWorkspaceList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderWorkspaceList",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		Items: make([]aggregationv1alpha1.CoderWorkspace, 0, len(s.workspaces)),
	}

	keys := make([]string, 0, len(s.workspaces))
	for key := range s.workspaces {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		workspace := s.workspaces[key]
		if namespace != "" && workspace.Namespace != namespace {
			continue
		}
		list.Items = append(list.Items, *workspace.DeepCopy())
	}

	return list, nil
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
