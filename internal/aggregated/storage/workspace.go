// Package storage provides hardcoded in-memory storage implementations for aggregated API resources.
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
	_ rest.Storage              = (*WorkspaceStorage)(nil)
	_ rest.Getter               = (*WorkspaceStorage)(nil)
	_ rest.Lister               = (*WorkspaceStorage)(nil)
	_ rest.Updater              = (*WorkspaceStorage)(nil)
	_ rest.GracefulDeleter      = (*WorkspaceStorage)(nil)
	_ rest.Scoper               = (*WorkspaceStorage)(nil)
	_ rest.SingularNameProvider = (*WorkspaceStorage)(nil)
)

// WorkspaceStorage provides hardcoded CoderWorkspace objects.
type WorkspaceStorage struct {
	mu             sync.RWMutex
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
					Name:            "dev-workspace",
					Namespace:       "default",
					ResourceVersion: "1",
					Generation:      1,
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
					Name:            "staging-workspace",
					Namespace:       "default",
					ResourceVersion: "1",
					Generation:      1,
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
					Name:            "sandbox-workspace",
					Namespace:       "sandbox",
					ResourceVersion: "1",
					Generation:      1,
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	if namespace != "" {
		workspace, ok := s.workspaces[workspaceKey(namespace, name)]
		if !ok {
			return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
		}
		return workspace.DeepCopy(), nil
	}

	workspace, found, ambiguous := s.findWorkspaceByNameLocked(name)
	if ambiguous {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("workspace name %q is ambiguous across namespaces; specify namespace", name))
	}
	if !found {
		return nil, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
	}
	return workspace.DeepCopy(), nil
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
		Items: make([]aggregationv1alpha1.CoderWorkspace, 0),
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

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

// Create inserts a CoderWorkspace into the in-memory store.
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

	workspace, ok := obj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderWorkspace, got %T", obj))
	}

	candidate := workspace.DeepCopy()
	if candidate.Name == "" {
		return nil, apierrors.NewBadRequest("metadata.name is required")
	}

	namespace, err := resolveWriteNamespace(ctx, candidate.Namespace)
	if err != nil {
		return nil, err
	}
	candidate.Namespace = namespace

	ensureWorkspaceTypeMeta(candidate)
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

	key := workspaceKey(candidate.Namespace, candidate.Name)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.workspaces[key]; exists {
		return nil, apierrors.NewAlreadyExists(aggregationv1alpha1.Resource("coderworkspaces"), candidate.Name)
	}

	s.workspaces[key] = candidate.DeepCopy()
	return candidate.DeepCopy(), nil
}

// Update modifies an existing CoderWorkspace in the in-memory store.
func (s *WorkspaceStorage) Update(
	ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc,
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

	namespace := genericapirequest.NamespaceValue(ctx)
	if namespace == "" {
		return nil, false, apierrors.NewBadRequest("namespace is required")
	}

	key := workspaceKey(namespace, name)

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.workspaces[key]
	if !exists {
		if !forceAllowCreate {
			return nil, false, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
		}

		createdObj, err := objInfo.UpdatedObject(ctx, &aggregationv1alpha1.CoderWorkspace{})
		if err != nil {
			return nil, false, err
		}
		createdWorkspace, ok := createdObj.(*aggregationv1alpha1.CoderWorkspace)
		if !ok {
			return nil, false, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderWorkspace, got %T", createdObj))
		}

		candidate := createdWorkspace.DeepCopy()
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

		ensureWorkspaceTypeMeta(candidate)
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

		s.workspaces[key] = candidate.DeepCopy()
		return candidate.DeepCopy(), true, nil
	}

	updatedObj, err := objInfo.UpdatedObject(ctx, existing.DeepCopy())
	if err != nil {
		return nil, false, err
	}
	updatedWorkspace, ok := updatedObj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("expected *CoderWorkspace, got %T", updatedObj))
	}

	candidate := updatedWorkspace.DeepCopy()
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

	if candidate.ResourceVersion != "" && candidate.ResourceVersion != existing.ResourceVersion {
		return nil, false, apierrors.NewConflict(
			aggregationv1alpha1.Resource("coderworkspaces"),
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
	ensureWorkspaceTypeMeta(candidate)

	if updateValidation != nil {
		if err := updateValidation(ctx, candidate, existing); err != nil {
			return nil, false, err
		}
	}

	s.workspaces[key] = candidate.DeepCopy()
	return candidate.DeepCopy(), false, nil
}

// Delete removes a CoderWorkspace from the in-memory store.
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

	namespace := genericapirequest.NamespaceValue(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		key       string
		workspace *aggregationv1alpha1.CoderWorkspace
	)
	if namespace != "" {
		key = workspaceKey(namespace, name)
		workspace = s.workspaces[key]
	} else {
		matchedKeys := make([]string, 0)
		for candidateKey, candidateWorkspace := range s.workspaces {
			if candidateWorkspace.Name == name {
				matchedKeys = append(matchedKeys, candidateKey)
			}
		}
		if len(matchedKeys) > 1 {
			return nil, false, apierrors.NewBadRequest(
				fmt.Sprintf("workspace name %q is ambiguous across namespaces; specify namespace", name),
			)
		}
		if len(matchedKeys) == 1 {
			key = matchedKeys[0]
			workspace = s.workspaces[key]
		}
	}

	if workspace == nil {
		return nil, false, apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	if deleteValidation != nil {
		if err := deleteValidation(ctx, workspace.DeepCopy()); err != nil {
			return nil, false, err
		}
	}

	deleted := workspace.DeepCopy()
	delete(s.workspaces, key)
	return deleted, true, nil
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

func ensureWorkspaceTypeMeta(workspace *aggregationv1alpha1.CoderWorkspace) {
	if workspace == nil {
		panic("assertion failed: workspace must not be nil")
	}
	workspace.TypeMeta = metav1.TypeMeta{
		Kind:       "CoderWorkspace",
		APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
	}
}

func (s *WorkspaceStorage) findWorkspaceByNameLocked(name string) (*aggregationv1alpha1.CoderWorkspace, bool, bool) {
	matchedKeys := make([]string, 0)
	for key, workspace := range s.workspaces {
		if workspace.Name == name {
			matchedKeys = append(matchedKeys, key)
		}
	}
	if len(matchedKeys) == 0 {
		return nil, false, false
	}
	if len(matchedKeys) > 1 {
		return nil, false, true
	}
	workspace := s.workspaces[matchedKeys[0]]
	if workspace == nil {
		return nil, false, false
	}
	return workspace, true, false
}
