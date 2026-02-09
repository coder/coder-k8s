package controller_test

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
)

type fakeBootstrapClient struct {
	response coderbootstrap.RegisterWorkspaceProxyResponse
	err      error
	calls    int
}

func (f *fakeBootstrapClient) EnsureWorkspaceProxy(_ context.Context, _ coderbootstrap.RegisterWorkspaceProxyRequest) (coderbootstrap.RegisterWorkspaceProxyResponse, error) {
	f.calls++
	return f.response, f.err
}

func TestWorkspaceProxyReconcile_UsingDirectTokenSecret(t *testing.T) {
	ctx := context.Background()
	secretName := "proxy-session-token"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultTokenSecretKey: []byte("token-value"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create proxy secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, secret)
	})

	workspaceProxy := &coderv1alpha1.WorkspaceProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-direct", Namespace: "default"},
		Spec: coderv1alpha1.WorkspaceProxySpec{
			Image:            "proxy-image:latest",
			PrimaryAccessURL: "https://coder.example.com",
			ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{
				Name: secretName,
				Key:  coderv1alpha1.DefaultTokenSecretKey,
			},
		},
	}
	if err := k8sClient.Create(ctx, workspaceProxy); err != nil {
		t.Fatalf("create workspace proxy resource: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, workspaceProxy)
	})

	reconciler := &controller.WorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}})
	if err != nil {
		t.Fatalf("reconcile workspace proxy: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %+v", result)
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}, deployment); err != nil {
		t.Fatalf("expected deployment to be reconciled: %v", err)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected one container, got %d", len(deployment.Spec.Template.Spec.Containers))
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Name != "workspace-proxy" {
		t.Fatalf("expected workspace-proxy container, got %q", container.Name)
	}
	if len(container.Env) < 2 {
		t.Fatalf("expected at least two environment variables, got %d", len(container.Env))
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}, service); err != nil {
		t.Fatalf("expected service to be reconciled: %v", err)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 80 {
		t.Fatalf("expected default service port 80, got %+v", service.Spec.Ports)
	}
}

func TestWorkspaceProxyReconcile_WithBootstrap(t *testing.T) {
	ctx := context.Background()
	credentialsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-bootstrap-credentials", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultTokenSecretKey: []byte("coder-session-token"),
		},
	}
	if err := k8sClient.Create(ctx, credentialsSecret); err != nil {
		t.Fatalf("create bootstrap credentials secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, credentialsSecret)
	})

	workspaceProxy := &coderv1alpha1.WorkspaceProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-bootstrap", Namespace: "default"},
		Spec: coderv1alpha1.WorkspaceProxySpec{
			Image: "proxy-image:latest",
			Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{
				CoderURL: "https://coder.example.com",
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{
					Name: credentialsSecret.Name,
					Key:  coderv1alpha1.DefaultTokenSecretKey,
				},
				ProxyName:                "proxy-bootstrap",
				GeneratedTokenSecretName: "proxy-bootstrap-token",
			},
		},
	}
	if err := k8sClient.Create(ctx, workspaceProxy); err != nil {
		t.Fatalf("create workspace proxy resource: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, workspaceProxy)
	})

	bootstrapClient := &fakeBootstrapClient{
		response: coderbootstrap.RegisterWorkspaceProxyResponse{ProxyName: workspaceProxy.Name, ProxyToken: "generated-proxy-token"},
	}
	reconciler := &controller.WorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}})
	if err != nil {
		t.Fatalf("reconcile workspace proxy with bootstrap: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %+v", result)
	}
	if bootstrapClient.calls != 1 {
		t.Fatalf("expected bootstrap client to be called once, got %d", bootstrapClient.calls)
	}

	tokenSecret := &corev1.Secret{}
	tokenSecretName := fmt.Sprintf("%s-token", workspaceProxy.Name)
	if workspaceProxy.Spec.Bootstrap.GeneratedTokenSecretName != "" {
		tokenSecretName = workspaceProxy.Spec.Bootstrap.GeneratedTokenSecretName
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: tokenSecretName, Namespace: workspaceProxy.Namespace}, tokenSecret); err != nil {
		t.Fatalf("expected generated token secret: %v", err)
	}
	if got := string(tokenSecret.Data[coderv1alpha1.DefaultTokenSecretKey]); got != "generated-proxy-token" {
		t.Fatalf("expected generated token value, got %q", got)
	}
}
