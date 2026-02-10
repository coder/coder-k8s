package controller_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	replicas := int32(2)

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cp",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:     "test-image:latest",
			Replicas:  &replicas,
			ExtraArgs: []string{"--prometheus-enable=false"},
			Service: coderv1alpha1.ServiceSpec{
				Port: 8080,
			},
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

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment); err != nil {
		t.Fatalf("expected deployment to be reconciled: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != replicas {
		t.Fatalf("expected deployment replicas %d, got %#v", replicas, deployment.Spec.Replicas)
	}

	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected one container in deployment pod spec, got %d", len(deployment.Spec.Template.Spec.Containers))
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	expectedArgs := []string{"--http-address=0.0.0.0:3000", "--prometheus-enable=false"}
	if !reflect.DeepEqual(container.Args, expectedArgs) {
		t.Fatalf("expected container args %v, got %v", expectedArgs, container.Args)
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, service); err != nil {
		t.Fatalf("expected service to be reconciled: %v", err)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 8080 {
		t.Fatalf("expected service port 8080, got %+v", service.Spec.Ports)
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
