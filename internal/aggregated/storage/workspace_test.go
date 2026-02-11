package storage

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

func TestWorkspaceStorageCRUDLifecycle(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	createdObj, err := workspaceStorage.Create(ctx, &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "unit-workspace"},
		Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Running: true},
	}, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	created, ok := createdObj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected *CoderWorkspace from create, got %T", createdObj)
	}
	if created.Namespace != "default" {
		t.Fatalf("expected namespace default, got %q", created.Namespace)
	}
	if created.ResourceVersion != "1" {
		t.Fatalf("expected resourceVersion 1, got %q", created.ResourceVersion)
	}
	if created.Generation != 1 {
		t.Fatalf("expected generation 1, got %d", created.Generation)
	}

	toUpdate := created.DeepCopy()
	toUpdate.Spec.Running = false
	toUpdate.ResourceVersion = created.ResourceVersion
	updatedObj, createdOnUpdate, err := workspaceStorage.Update(
		ctx,
		toUpdate.Name,
		rest.DefaultUpdatedObjectInfo(toUpdate),
		nil,
		nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatalf("update workspace: %v", err)
	}
	if createdOnUpdate {
		t.Fatal("expected update of existing workspace, got createdOnUpdate=true")
	}

	updated, ok := updatedObj.(*aggregationv1alpha1.CoderWorkspace)
	if !ok {
		t.Fatalf("expected *CoderWorkspace from update, got %T", updatedObj)
	}
	if updated.Spec.Running {
		t.Fatalf("expected running=false after update, got %+v", updated.Spec)
	}
	if updated.ResourceVersion == created.ResourceVersion {
		t.Fatalf("expected resourceVersion to change, got %q", updated.ResourceVersion)
	}
	if updated.Generation != created.Generation+1 {
		t.Fatalf("expected generation increment to %d, got %d", created.Generation+1, updated.Generation)
	}

	deletedObj, deletedNow, err := workspaceStorage.Delete(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	if !deletedNow {
		t.Fatal("expected immediate delete")
	}
	if _, ok := deletedObj.(*aggregationv1alpha1.CoderWorkspace); !ok {
		t.Fatalf("expected *CoderWorkspace from delete, got %T", deletedObj)
	}

	_, err = workspaceStorage.Get(ctx, created.Name, nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}
}

func TestWorkspaceStorageCreateAlreadyExists(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	_, err := workspaceStorage.Create(ctx, &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-workspace"},
		Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Running: true},
	}, nil, nil)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected AlreadyExists error, got %v", err)
	}
}

func TestWorkspaceStorageUpdateRejectsNamespaceChange(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	currentObj, err := workspaceStorage.Get(ctx, "dev-workspace", nil)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	current := currentObj.(*aggregationv1alpha1.CoderWorkspace)

	modified := current.DeepCopy()
	modified.Namespace = "sandbox"
	modified.ResourceVersion = current.ResourceVersion

	_, _, err = workspaceStorage.Update(
		ctx,
		modified.Name,
		rest.DefaultUpdatedObjectInfo(modified),
		nil,
		nil,
		false,
		nil,
	)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for namespace mismatch, got %v", err)
	}
}

func TestWorkspaceStorageUpdateIgnoresStatusWrites(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	currentObj, err := workspaceStorage.Get(ctx, "dev-workspace", nil)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	current := currentObj.(*aggregationv1alpha1.CoderWorkspace)
	if current.Status.AutoShutdown == nil {
		t.Fatal("expected seeded workspace status autoShutdown")
	}

	modified := current.DeepCopy()
	modified.Spec.Running = !current.Spec.Running
	modified.ResourceVersion = current.ResourceVersion
	overrideDeadline := metav1.NewTime(time.Date(2040, time.January, 1, 0, 0, 0, 0, time.UTC))
	modified.Status.AutoShutdown = &overrideDeadline

	updatedObj, _, err := workspaceStorage.Update(
		ctx,
		modified.Name,
		rest.DefaultUpdatedObjectInfo(modified),
		nil,
		nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatalf("update workspace: %v", err)
	}

	updated := updatedObj.(*aggregationv1alpha1.CoderWorkspace)
	if updated.Status.AutoShutdown == nil {
		t.Fatal("expected status autoShutdown to remain present")
	}
	if !updated.Status.AutoShutdown.Equal(current.Status.AutoShutdown) {
		t.Fatalf("expected status to remain unchanged, got %s want %s", updated.Status.AutoShutdown, current.Status.AutoShutdown)
	}
}

func TestWorkspaceStorageDeleteAmbiguousWithoutNamespace(t *testing.T) {
	t.Helper()

	workspaceStorage := NewWorkspaceStorage()

	_, err := workspaceStorage.Create(
		genericapirequest.WithNamespace(context.Background(), "sandbox"),
		&aggregationv1alpha1.CoderWorkspace{ObjectMeta: metav1.ObjectMeta{Name: "dev-workspace"}},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("seed same-name workspace in sandbox namespace: %v", err)
	}

	_, _, err = workspaceStorage.Delete(context.Background(), "dev-workspace", nil, nil)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for ambiguous delete, got %v", err)
	}
}
