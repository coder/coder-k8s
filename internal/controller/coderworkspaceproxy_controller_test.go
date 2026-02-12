package controller_test

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/coder/coder/v2/codersdk"
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

	// Provisioner key support.
	provisionerKeyResponses []coderbootstrap.EnsureProvisionerKeyResponse
	provisionerKeyErr       error
	provisionerKeyCalls     int
	provisionerKeyRequests  []coderbootstrap.EnsureProvisionerKeyRequest
	deleteKeyErr            error
	deleteKeyCalls          int
	deleteKeyRequests       []deleteKeyRequest

	// Entitlements support.
	entitlementsResponse codersdk.Entitlements
	entitlementsErr      error
	entitlementsCalls    int
	entitlementsRequests []entitlementsRequest
}

type deleteKeyRequest struct {
	CoderURL         string
	SessionToken     string
	OrganizationName string
	KeyName          string
}

type entitlementsRequest struct {
	CoderURL     string
	SessionToken string
}

func (f *fakeBootstrapClient) EnsureWorkspaceProxy(_ context.Context, _ coderbootstrap.RegisterWorkspaceProxyRequest) (coderbootstrap.RegisterWorkspaceProxyResponse, error) {
	f.calls++
	return f.response, f.err
}

func (f *fakeBootstrapClient) EnsureProvisionerKey(_ context.Context, req coderbootstrap.EnsureProvisionerKeyRequest) (coderbootstrap.EnsureProvisionerKeyResponse, error) {
	f.provisionerKeyCalls++
	f.provisionerKeyRequests = append(f.provisionerKeyRequests, req)

	if f.provisionerKeyErr != nil {
		return coderbootstrap.EnsureProvisionerKeyResponse{}, f.provisionerKeyErr
	}
	if len(f.provisionerKeyResponses) == 0 {
		return coderbootstrap.EnsureProvisionerKeyResponse{}, nil
	}
	idx := f.provisionerKeyCalls - 1
	if idx >= len(f.provisionerKeyResponses) {
		idx = len(f.provisionerKeyResponses) - 1
	}

	return f.provisionerKeyResponses[idx], nil
}

func (f *fakeBootstrapClient) DeleteProvisionerKey(_ context.Context, coderURL, sessionToken, orgName, keyName string) error {
	f.deleteKeyCalls++
	f.deleteKeyRequests = append(f.deleteKeyRequests, deleteKeyRequest{
		CoderURL:         coderURL,
		SessionToken:     sessionToken,
		OrganizationName: orgName,
		KeyName:          keyName,
	})
	return f.deleteKeyErr
}

func (f *fakeBootstrapClient) Entitlements(_ context.Context, coderURL, sessionToken string) (codersdk.Entitlements, error) {
	f.entitlementsCalls++
	f.entitlementsRequests = append(f.entitlementsRequests, entitlementsRequest{
		CoderURL:     coderURL,
		SessionToken: sessionToken,
	})
	if f.entitlementsErr != nil {
		return codersdk.Entitlements{}, f.entitlementsErr
	}
	if f.entitlementsResponse.Features == nil {
		f.entitlementsResponse.Features = map[codersdk.FeatureName]codersdk.Feature{
			codersdk.FeatureExternalProvisionerDaemons: {Entitlement: codersdk.EntitlementEntitled},
		}
	}
	return f.entitlementsResponse, nil
}

func workspaceProxyResourceName(name string) string {
	const prefix = "wsproxy-"
	candidate := prefix + name
	if len(candidate) <= 63 {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(prefix) - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s%s-%s", prefix, name[:available], suffix)
}

func workspaceProxyInstanceLabelValue(name string) string {
	if len(name) <= 63 {
		return name
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s", name[:available], suffix)
}

func TestCoderWorkspaceProxyReconcile_UsingDirectTokenSecret(t *testing.T) {
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

	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{
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

	reconciler := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}})
	if err != nil {
		t.Fatalf("reconcile workspace proxy: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %+v", result)
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxyResourceName(workspaceProxy.Name), Namespace: workspaceProxy.Namespace}, deployment); err != nil {
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
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxyResourceName(workspaceProxy.Name), Namespace: workspaceProxy.Namespace}, service); err != nil {
		t.Fatalf("expected service to be reconciled: %v", err)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 80 {
		t.Fatalf("expected default service port 80, got %+v", service.Spec.Ports)
	}
}

func TestCoderWorkspaceProxyReconcile_DoesNotCollideWithControlPlaneChildren(t *testing.T) {
	ctx := context.Background()
	resourceName := "shared-name"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-shared-token", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultTokenSecretKey: []byte("proxy-token"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create shared secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, secret)
	})

	controlPlane := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "control-plane-image:latest",
		},
	}
	if err := k8sClient.Create(ctx, controlPlane); err != nil {
		t.Fatalf("create control plane resource: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, controlPlane)
	})

	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
		Spec: coderv1alpha1.WorkspaceProxySpec{
			Image:            "proxy-image:latest",
			PrimaryAccessURL: "https://coder.example.com",
			ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{
				Name: secret.Name,
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

	controlPlaneReconciler := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := controlPlaneReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: controlPlane.Name, Namespace: controlPlane.Namespace}}); err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}

	workspaceProxyReconciler := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := workspaceProxyReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}}); err != nil {
		t.Fatalf("reconcile workspace proxy: %v", err)
	}

	controlPlaneDeployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: controlPlane.Name, Namespace: controlPlane.Namespace}, controlPlaneDeployment); err != nil {
		t.Fatalf("expected control plane deployment: %v", err)
	}

	workspaceProxyDeploymentName := workspaceProxyResourceName(workspaceProxy.Name)
	if workspaceProxyDeploymentName == controlPlane.Name {
		t.Fatalf("expected workspace proxy deployment name to differ from control plane name")
	}
	workspaceProxyDeployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxyDeploymentName, Namespace: workspaceProxy.Namespace}, workspaceProxyDeployment); err != nil {
		t.Fatalf("expected workspace proxy deployment: %v", err)
	}
}

func TestCoderWorkspaceProxyReconcile_TruncatesLongInstanceLabelValue(t *testing.T) {
	ctx := context.Background()
	resourceName := strings.Repeat("a", 70)
	secretName := "proxy-long-name-token"

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

	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
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

	reconciler := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}}); err != nil {
		t.Fatalf("reconcile workspace proxy: %v", err)
	}

	expectedInstanceLabel := workspaceProxyInstanceLabelValue(resourceName)
	if len(expectedInstanceLabel) > 63 {
		t.Fatalf("expected test helper to produce <=63 label length, got %d", len(expectedInstanceLabel))
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxyResourceName(resourceName), Namespace: workspaceProxy.Namespace}, deployment); err != nil {
		t.Fatalf("get workspace proxy deployment: %v", err)
	}
	if got := deployment.Labels["app.kubernetes.io/instance"]; got != expectedInstanceLabel {
		t.Fatalf("expected deployment instance label %q, got %q", expectedInstanceLabel, got)
	}
	if got := deployment.Spec.Template.Labels["app.kubernetes.io/instance"]; got != expectedInstanceLabel {
		t.Fatalf("expected pod template instance label %q, got %q", expectedInstanceLabel, got)
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxyResourceName(resourceName), Namespace: workspaceProxy.Namespace}, service); err != nil {
		t.Fatalf("get workspace proxy service: %v", err)
	}
	if got := service.Labels["app.kubernetes.io/instance"]; got != expectedInstanceLabel {
		t.Fatalf("expected service instance label %q, got %q", expectedInstanceLabel, got)
	}
	if got := service.Spec.Selector["app.kubernetes.io/instance"]; got != expectedInstanceLabel {
		t.Fatalf("expected service selector instance label %q, got %q", expectedInstanceLabel, got)
	}
}

func TestCoderWorkspaceProxyReconcile_WithBootstrap_UsesExistingTokenWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	tokenSecretName := "proxy-bootstrap-existing-token"
	proxyTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName, Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultTokenSecretKey: []byte("existing-proxy-token"),
		},
	}
	if err := k8sClient.Create(ctx, proxyTokenSecret); err != nil {
		t.Fatalf("create existing proxy token secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, proxyTokenSecret)
	})

	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-bootstrap-existing", Namespace: "default"},
		Spec: coderv1alpha1.WorkspaceProxySpec{
			Image: "proxy-image:latest",
			Bootstrap: &coderv1alpha1.ProxyBootstrapSpec{
				CoderURL: "https://coder.example.com",
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{
					Name: "missing-bootstrap-credentials",
					Key:  coderv1alpha1.DefaultTokenSecretKey,
				},
				ProxyName:                "proxy-bootstrap-existing",
				GeneratedTokenSecretName: tokenSecretName,
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
	reconciler := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}})
	if err != nil {
		t.Fatalf("reconcile workspace proxy with existing token: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %+v", result)
	}
	if bootstrapClient.calls != 0 {
		t.Fatalf("expected bootstrap client to be skipped when token already exists, got %d calls", bootstrapClient.calls)
	}

	reconciled := &coderv1alpha1.CoderWorkspaceProxy{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled workspace proxy: %v", err)
	}
	if reconciled.Status.ProxyTokenSecretRef == nil {
		t.Fatalf("expected proxy token secret reference in status")
	}
	if reconciled.Status.ProxyTokenSecretRef.Name != tokenSecretName {
		t.Fatalf("expected status token secret name %q, got %q", tokenSecretName, reconciled.Status.ProxyTokenSecretRef.Name)
	}
}

func TestCoderWorkspaceProxyReconcile_WithBootstrap(t *testing.T) {
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

	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{
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
	reconciler := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

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
