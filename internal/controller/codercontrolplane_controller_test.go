package controller_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

func TestReconcile_NotFound(t *testing.T) {
	r := &controller.CoderControlPlaneReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nonexistent",
			Namespace: "default",
		},
	})

	if err != nil {
		t.Fatalf("expected no error for not-found resource, got: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got: %+v", result)
	}
}

func TestReconcile_ExistingResource(t *testing.T) {
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cp",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-image:latest",
		},
	}

	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	r := &controller.CoderControlPlaneReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      cp.Name,
			Namespace: cp.Namespace,
		},
	})

	if err != nil {
		t.Fatalf("expected no error for existing resource, got: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got: %+v", result)
	}
}

func TestReconcile_NilClient(t *testing.T) {
	r := &controller.CoderControlPlaneReconciler{
		Client: nil,
		Scheme: scheme,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test",
			Namespace: "default",
		},
	})

	if err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
	expected := "assertion failed: reconciler client must not be nil"
	if !strings.Contains(err.Error(), expected) {
		t.Fatalf("expected error containing %q, got %q", expected, err.Error())
	}
}

func TestReconcile_NilScheme(t *testing.T) {
	r := &controller.CoderControlPlaneReconciler{
		Client: k8sClient,
		Scheme: nil,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test",
			Namespace: "default",
		},
	})

	if err == nil {
		t.Fatal("expected error for nil scheme, got nil")
	}
	expected := "assertion failed: reconciler scheme must not be nil"
	if !strings.Contains(err.Error(), expected) {
		t.Fatalf("expected error containing %q, got %q", expected, err.Error())
	}
}
