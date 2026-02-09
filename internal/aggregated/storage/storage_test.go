// Package storage provides hardcoded in-memory storage implementations for aggregated API resources.
package storage

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

func TestWorkspaceStorageList(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	obj, err := workspaceStorage.List(ctx, nil)
	if err != nil {
		t.Fatalf("expected workspace list to succeed: %v", err)
	}

	list, ok := obj.(*aggregationv1alpha1.CoderWorkspaceList)
	if !ok {
		t.Fatalf("expected *CoderWorkspaceList, got %T", obj)
	}
	if len(list.Items) == 0 {
		t.Fatal("expected non-empty workspace list")
	}
}

func TestWorkspaceStorageGet(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	obj, err := workspaceStorage.Get(ctx, "dev-workspace", nil)
	if err != nil {
		t.Fatalf("expected workspace get to succeed: %v", err)
	}

	workspace, ok := obj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected *CoderWorkspace, got %T", obj)
	}
	if workspace.Name != "dev-workspace" {
		t.Fatalf("expected dev-workspace, got %q", workspace.Name)
	}
}

func TestWorkspaceStorageGetNotFound(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	_, err := workspaceStorage.Get(ctx, "does-not-exist", nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound error, got %v", err)
	}
}

func TestTemplateStorageList(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	obj, err := templateStorage.List(ctx, nil)
	if err != nil {
		t.Fatalf("expected template list to succeed: %v", err)
	}

	list, ok := obj.(*aggregationv1alpha1.CoderTemplateList)
	if !ok {
		t.Fatalf("expected *CoderTemplateList, got %T", obj)
	}
	if len(list.Items) == 0 {
		t.Fatal("expected non-empty template list")
	}
}

func TestTemplateStorageGet(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	obj, err := templateStorage.Get(ctx, "starter-template", nil)
	if err != nil {
		t.Fatalf("expected template get to succeed: %v", err)
	}

	template, ok := obj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected *CoderTemplate, got %T", obj)
	}
	if template.Name != "starter-template" {
		t.Fatalf("expected starter-template, got %q", template.Name)
	}
}

func TestTemplateStorageGetNotFound(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	_, err := templateStorage.Get(ctx, "does-not-exist", nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound error, got %v", err)
	}
}
