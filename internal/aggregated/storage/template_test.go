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

func TestTemplateStorageCRUDLifecycle(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	createdObj, err := templateStorage.Create(ctx, &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "unit-template"},
		Spec:       aggregationv1alpha1.CoderTemplateSpec{Running: true},
	}, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatalf("create template: %v", err)
	}

	created, ok := createdObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected *CoderTemplate from create, got %T", createdObj)
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
	updatedObj, createdOnUpdate, err := templateStorage.Update(
		ctx,
		toUpdate.Name,
		rest.DefaultUpdatedObjectInfo(toUpdate),
		nil,
		nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatalf("update template: %v", err)
	}
	if createdOnUpdate {
		t.Fatal("expected update of existing template, got createdOnUpdate=true")
	}

	updated, ok := updatedObj.(*aggregationv1alpha1.CoderTemplate)
	if !ok {
		t.Fatalf("expected *CoderTemplate from update, got %T", updatedObj)
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

	deletedObj, deletedNow, err := templateStorage.Delete(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatalf("delete template: %v", err)
	}
	if !deletedNow {
		t.Fatal("expected immediate delete")
	}
	if _, ok := deletedObj.(*aggregationv1alpha1.CoderTemplate); !ok {
		t.Fatalf("expected *CoderTemplate from delete, got %T", deletedObj)
	}

	_, err = templateStorage.Get(ctx, created.Name, nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}
}

func TestTemplateStorageCreateAlreadyExists(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	_, err := templateStorage.Create(ctx, &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "starter-template"},
		Spec:       aggregationv1alpha1.CoderTemplateSpec{Running: true},
	}, nil, nil)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected AlreadyExists error, got %v", err)
	}
}

func TestTemplateStorageUpdateRejectsNamespaceChange(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	currentObj, err := templateStorage.Get(ctx, "starter-template", nil)
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	current := currentObj.(*aggregationv1alpha1.CoderTemplate)

	modified := current.DeepCopy()
	modified.Namespace = "sandbox"
	modified.ResourceVersion = current.ResourceVersion

	_, _, err = templateStorage.Update(
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

func TestTemplateStorageUpdateIgnoresStatusWrites(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	currentObj, err := templateStorage.Get(ctx, "starter-template", nil)
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	current := currentObj.(*aggregationv1alpha1.CoderTemplate)
	if current.Status.AutoShutdown == nil {
		t.Fatal("expected seeded template status autoShutdown")
	}

	modified := current.DeepCopy()
	modified.Spec.Running = !current.Spec.Running
	modified.ResourceVersion = current.ResourceVersion
	overrideDeadline := metav1.NewTime(time.Date(2040, time.January, 1, 0, 0, 0, 0, time.UTC))
	modified.Status.AutoShutdown = &overrideDeadline

	updatedObj, _, err := templateStorage.Update(
		ctx,
		modified.Name,
		rest.DefaultUpdatedObjectInfo(modified),
		nil,
		nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatalf("update template: %v", err)
	}

	updated := updatedObj.(*aggregationv1alpha1.CoderTemplate)
	if updated.Status.AutoShutdown == nil {
		t.Fatal("expected status autoShutdown to remain present")
	}
	if !updated.Status.AutoShutdown.Equal(current.Status.AutoShutdown) {
		t.Fatalf("expected status to remain unchanged, got %s want %s", updated.Status.AutoShutdown, current.Status.AutoShutdown)
	}
}

func TestTemplateStorageDeleteAmbiguousWithoutNamespace(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()

	_, err := templateStorage.Create(
		genericapirequest.WithNamespace(context.Background(), "sandbox"),
		&aggregationv1alpha1.CoderTemplate{ObjectMeta: metav1.ObjectMeta{Name: "starter-template"}},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("seed same-name template in sandbox namespace: %v", err)
	}

	_, _, err = templateStorage.Delete(context.Background(), "starter-template", nil, nil)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for ambiguous delete, got %v", err)
	}
}

func TestTemplateStorageUpdateRequiresResourceVersion(t *testing.T) {
	t.Helper()

	templateStorage := NewTemplateStorage()
	ctx := genericapirequest.WithNamespace(context.Background(), "default")

	currentObj, err := templateStorage.Get(ctx, "starter-template", nil)
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	current := currentObj.(*aggregationv1alpha1.CoderTemplate)

	modified := current.DeepCopy()
	modified.Spec.Running = !current.Spec.Running
	modified.ResourceVersion = ""

	_, _, err = templateStorage.Update(
		ctx,
		modified.Name,
		rest.DefaultUpdatedObjectInfo(modified),
		nil,
		nil,
		false,
		nil,
	)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest when resourceVersion is missing, got %v", err)
	}
}
