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

func TestReconcile_StatusPersistence(t *testing.T) {
	ctx := context.Background()
	replicas := int32(1)

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-status-persistence",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "test-status-image:latest",
			Replicas: &replicas,
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

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}

	if reconciled.Status.ObservedGeneration != reconciled.Generation {
		t.Fatalf("expected observed generation %d, got %d", reconciled.Generation, reconciled.Status.ObservedGeneration)
	}
	expectedURL := "http://" + cp.Name + "." + cp.Namespace + ".svc.cluster.local:8080"
	if reconciled.Status.URL != expectedURL {
		t.Fatalf("expected status URL %q, got %q", expectedURL, reconciled.Status.URL)
	}
	if reconciled.Status.ReadyReplicas != 0 {
		t.Fatalf("expected ready replicas 0, got %d", reconciled.Status.ReadyReplicas)
	}
	if reconciled.Status.Phase != coderv1alpha1.CoderControlPlanePhasePending {
		t.Fatalf("expected phase %q, got %q", coderv1alpha1.CoderControlPlanePhasePending, reconciled.Status.Phase)
	}
}

func TestReconcile_OwnerReferences(t *testing.T) {
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner-references",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-owner-image:latest",
		},
	}

	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment); err != nil {
		t.Fatalf("get reconciled deployment: %v", err)
	}
	assertSingleControllerOwnerReference(t, deployment.OwnerReferences, cp.Name)

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, service); err != nil {
		t.Fatalf("get reconciled service: %v", err)
	}
	assertSingleControllerOwnerReference(t, service.OwnerReferences, cp.Name)
}

func TestReconcile_SpecUpdatePropagates(t *testing.T) {
	ctx := context.Background()
	initialReplicas := int32(1)

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-spec-update-propagates",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "img:v1",
			Replicas: &initialReplicas,
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

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}

	updatedReplicas := int32(3)
	reconciled.Spec.Replicas = &updatedReplicas
	reconciled.Spec.Image = "img:v2"
	reconciled.Spec.Service.Port = 9090
	if err := k8sClient.Update(ctx, reconciled); err != nil {
		t.Fatalf("update control plane spec: %v", err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("second reconcile control plane: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment); err != nil {
		t.Fatalf("get reconciled deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != updatedReplicas {
		t.Fatalf("expected deployment replicas %d, got %#v", updatedReplicas, deployment.Spec.Replicas)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected one container in deployment pod spec, got %d", len(deployment.Spec.Template.Spec.Containers))
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != "img:v2" {
		t.Fatalf("expected container image %q, got %q", "img:v2", deployment.Spec.Template.Spec.Containers[0].Image)
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, service); err != nil {
		t.Fatalf("get reconciled service: %v", err)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 9090 {
		t.Fatalf("expected service port 9090, got %+v", service.Spec.Ports)
	}
}

func TestReconcile_PhaseTransitionToReady(t *testing.T) {
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-phase-transition-ready",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-phase-image:latest",
		},
	}

	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.Phase != coderv1alpha1.CoderControlPlanePhasePending {
		t.Fatalf("expected phase %q before deployment ready, got %q", coderv1alpha1.CoderControlPlanePhasePending, reconciled.Status.Phase)
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment); err != nil {
		t.Fatalf("get reconciled deployment: %v", err)
	}
	deployment.Status.ReadyReplicas = 1
	deployment.Status.Replicas = 1
	if err := k8sClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("second reconcile control plane: %v", err)
	}

	reconciledAfterReady := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciledAfterReady); err != nil {
		t.Fatalf("get reconciled control plane after deployment ready: %v", err)
	}
	if reconciledAfterReady.Status.Phase != coderv1alpha1.CoderControlPlanePhaseReady {
		t.Fatalf("expected phase %q after deployment ready, got %q", coderv1alpha1.CoderControlPlanePhaseReady, reconciledAfterReady.Status.Phase)
	}
	if reconciledAfterReady.Status.ReadyReplicas != 1 {
		t.Fatalf("expected ready replicas 1 after deployment ready, got %d", reconciledAfterReady.Status.ReadyReplicas)
	}
}

func TestReconcile_DefaultsApplied(t *testing.T) {
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-defaults-applied",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{},
	}

	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment); err != nil {
		t.Fatalf("get reconciled deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("expected default deployment replicas 1, got %#v", deployment.Spec.Replicas)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected one container in deployment pod spec, got %d", len(deployment.Spec.Template.Spec.Containers))
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != "ghcr.io/coder/coder:latest" {
		t.Fatalf("expected default image %q, got %q", "ghcr.io/coder/coder:latest", deployment.Spec.Template.Spec.Containers[0].Image)
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, service); err != nil {
		t.Fatalf("get reconciled service: %v", err)
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("expected default service type %q, got %q", corev1.ServiceTypeClusterIP, service.Spec.Type)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 80 {
		t.Fatalf("expected default service port 80, got %+v", service.Spec.Ports)
	}
}

func assertSingleControllerOwnerReference(t *testing.T, ownerReferences []metav1.OwnerReference, ownerName string) {
	t.Helper()

	if len(ownerReferences) != 1 {
		t.Fatalf("expected one owner reference, got %d", len(ownerReferences))
	}
	ownerReference := ownerReferences[0]
	if ownerReference.Name != ownerName {
		t.Fatalf("expected owner reference name %q, got %q", ownerName, ownerReference.Name)
	}
	if ownerReference.Kind != "CoderControlPlane" {
		t.Fatalf("expected owner reference kind %q, got %q", "CoderControlPlane", ownerReference.Kind)
	}
	if ownerReference.Controller == nil || !*ownerReference.Controller {
		t.Fatalf("expected owner reference controller=true, got %#v", ownerReference.Controller)
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
