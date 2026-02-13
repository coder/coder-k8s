package controller_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
)

type fakeOperatorAccessProvisioner struct {
	token          string
	err            error
	calls          int
	requests       []coderbootstrap.EnsureOperatorTokenRequest
	revokeErr      error
	revokeCalls    int
	revokeRequests []coderbootstrap.RevokeOperatorTokenRequest
}

func (f *fakeOperatorAccessProvisioner) EnsureOperatorToken(_ context.Context, req coderbootstrap.EnsureOperatorTokenRequest) (string, error) {
	f.calls++
	f.requests = append(f.requests, req)
	return f.token, f.err
}

func (f *fakeOperatorAccessProvisioner) RevokeOperatorToken(_ context.Context, req coderbootstrap.RevokeOperatorTokenRequest) error {
	f.revokeCalls++
	f.revokeRequests = append(f.revokeRequests, req)
	return f.revokeErr
}

type licenseUploadCall struct {
	coderURL     string
	sessionToken string
	licenseJWT   string
}

type fakeLicenseUploader struct {
	err               error
	addLicenseErrs    []error
	hasAnyLicenseErr  error
	hasAnyLicense     *bool
	hasAnyLicenseCall int
	calls             []licenseUploadCall
}

func (f *fakeLicenseUploader) AddLicense(_ context.Context, coderURL, sessionToken, licenseJWT string) error {
	f.calls = append(f.calls, licenseUploadCall{
		coderURL:     coderURL,
		sessionToken: sessionToken,
		licenseJWT:   licenseJWT,
	})
	if len(f.addLicenseErrs) > 0 {
		err := f.addLicenseErrs[0]
		f.addLicenseErrs = f.addLicenseErrs[1:]
		return err
	}
	return f.err
}

func (f *fakeLicenseUploader) HasAnyLicense(_ context.Context, _, _ string) (bool, error) {
	f.hasAnyLicenseCall++
	if f.hasAnyLicenseErr != nil {
		return false, f.hasAnyLicenseErr
	}
	if f.hasAnyLicense != nil {
		return *f.hasAnyLicense, nil
	}

	return len(f.calls) > 0, nil
}

type fakeEntitlementsInspector struct {
	response codersdk.Entitlements
	err      error
	calls    int
	requests []entitlementsInspectCall
}

type entitlementsInspectCall struct {
	coderURL     string
	sessionToken string
}

func (f *fakeEntitlementsInspector) Entitlements(_ context.Context, coderURL, sessionToken string) (codersdk.Entitlements, error) {
	f.calls++
	f.requests = append(f.requests, entitlementsInspectCall{coderURL: coderURL, sessionToken: sessionToken})
	if f.err != nil {
		return codersdk.Entitlements{}, f.err
	}
	if f.response.Features == nil {
		f.response.Features = map[codersdk.FeatureName]codersdk.Feature{}
	}
	return f.response, nil
}

func TestReconcile_NotFound(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
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
	ensureGatewaySchemeRegistered(t)
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
	expectedArgs := []string{"--http-address=0.0.0.0:8080", "--prometheus-enable=false"}
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
	ensureGatewaySchemeRegistered(t)
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
	ensureGatewaySchemeRegistered(t)
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
	ensureGatewaySchemeRegistered(t)
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
	ensureGatewaySchemeRegistered(t)
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

func TestReconcile_LicenseSecretRefNil_DoesNotUpload(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-license-no-ref",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example/coder",
			}},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-no-license-ref"}
	uploader := &fakeLicenseUploader{}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
		LicenseUploader:           uploader,
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
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

	if len(uploader.calls) != 0 {
		t.Fatalf("expected no license upload calls when licenseSecretRef is not configured, got %d", len(uploader.calls))
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	licenseCondition := findCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionLicenseApplied)
	if licenseCondition.Status != metav1.ConditionUnknown {
		t.Fatalf("expected license condition status %q, got %q", metav1.ConditionUnknown, licenseCondition.Status)
	}
	if licenseCondition.Reason != "Pending" {
		t.Fatalf("expected license condition reason %q, got %q", "Pending", licenseCondition.Reason)
	}
}

func TestReconcile_LicensePendingUntilControlPlaneReady(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	licenseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-pending-secret", Namespace: "default"},
		Data: map[string][]byte{
			coderv1alpha1.DefaultLicenseSecretKey: []byte("license-pending"),
		},
	}
	if err := k8sClient.Create(ctx, licenseSecret); err != nil {
		t.Fatalf("create license secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, licenseSecret)
	})

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-pending", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example/pending",
			}},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: licenseSecret.Name},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-pending"}
	uploader := &fakeLicenseUploader{}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
		LicenseUploader:           uploader,
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("expected no license upload calls before deployment readiness, got %d", len(uploader.calls))
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	licenseCondition := findCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionLicenseApplied)
	if licenseCondition.Status != metav1.ConditionFalse {
		t.Fatalf("expected license condition status %q, got %q", metav1.ConditionFalse, licenseCondition.Status)
	}
	if licenseCondition.Reason != "Pending" {
		t.Fatalf("expected license condition reason %q, got %q", "Pending", licenseCondition.Reason)
	}
}

func TestReconcile_LicenseAppliesOnceAndTracksHash(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	licenseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-apply-secret", Namespace: "default"},
		Data: map[string][]byte{
			coderv1alpha1.DefaultLicenseSecretKey: []byte("  license-jwt-initial  \n"),
		},
	}
	if err := k8sClient.Create(ctx, licenseSecret); err != nil {
		t.Fatalf("create license secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, licenseSecret)
	})

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-apply", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example/license-apply",
			}},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: licenseSecret.Name},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-license-apply"}
	uploader := &fakeLicenseUploader{}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
		LicenseUploader:           uploader,
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
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
	if len(uploader.calls) != 1 {
		t.Fatalf("expected one license upload call, got %d", len(uploader.calls))
	}
	if uploader.calls[0].sessionToken != "operator-token-license-apply" {
		t.Fatalf("expected license upload session token %q, got %q", "operator-token-license-apply", uploader.calls[0].sessionToken)
	}
	if uploader.calls[0].licenseJWT != "license-jwt-initial" {
		t.Fatalf("expected trimmed license JWT %q, got %q", "license-jwt-initial", uploader.calls[0].licenseJWT)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.LicenseLastApplied == nil {
		t.Fatalf("expected licenseLastApplied to be set after successful upload")
	}
	if reconciled.Status.LicenseLastAppliedHash == "" {
		t.Fatalf("expected licenseLastAppliedHash to be set after successful upload")
	}
	licenseCondition := findCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionLicenseApplied)
	if licenseCondition.Status != metav1.ConditionTrue {
		t.Fatalf("expected license condition status %q, got %q", metav1.ConditionTrue, licenseCondition.Status)
	}
	if licenseCondition.Reason != "Applied" {
		t.Fatalf("expected license condition reason %q, got %q", "Applied", licenseCondition.Reason)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("third reconcile control plane: %v", err)
	}
	if len(uploader.calls) != 1 {
		t.Fatalf("expected license upload call count to remain 1 for idempotent reconcile, got %d", len(uploader.calls))
	}
}

func TestReconcile_LicenseReuploadsWhenBackendHasNoLicenses(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	licenseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-backend-reset-secret", Namespace: "default"},
		Data: map[string][]byte{
			coderv1alpha1.DefaultLicenseSecretKey: []byte("license-jwt-backend-reset"),
		},
	}
	if err := k8sClient.Create(ctx, licenseSecret); err != nil {
		t.Fatalf("create license secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, licenseSecret)
	})

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-backend-reset", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example/license-backend-reset",
			}},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: licenseSecret.Name},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-backend-reset"}
	uploader := &fakeLicenseUploader{}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
		LicenseUploader:           uploader,
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
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
	if len(uploader.calls) != 1 {
		t.Fatalf("expected initial upload call count 1, got %d", len(uploader.calls))
	}

	backendHasNoLicenses := false
	uploader.hasAnyLicense = &backendHasNoLicenses
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("third reconcile control plane: %v", err)
	}
	if uploader.hasAnyLicenseCall == 0 {
		t.Fatalf("expected reconcile to query existing licenses when hash matches")
	}
	if len(uploader.calls) != 2 {
		t.Fatalf("expected license to be re-uploaded when backend has no licenses, got %d upload calls", len(uploader.calls))
	}
}

func TestReconcile_LicenseRotationUploadsNewSecretValue(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	licenseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-rotation-secret", Namespace: "default"},
		Data: map[string][]byte{
			coderv1alpha1.DefaultLicenseSecretKey: []byte("license-jwt-v1"),
		},
	}
	if err := k8sClient.Create(ctx, licenseSecret); err != nil {
		t.Fatalf("create license secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, licenseSecret)
	})

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-rotation", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example/license-rotation",
			}},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: licenseSecret.Name},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-license-rotation"}
	uploader := &fakeLicenseUploader{}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
		LicenseUploader:           uploader,
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
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
	if len(uploader.calls) != 1 {
		t.Fatalf("expected first license upload call, got %d", len(uploader.calls))
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	initialHash := reconciled.Status.LicenseLastAppliedHash
	if initialHash == "" {
		t.Fatalf("expected initial license hash to be set")
	}

	secretToRotate := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: licenseSecret.Name, Namespace: licenseSecret.Namespace}, secretToRotate); err != nil {
		t.Fatalf("get license secret for update: %v", err)
	}
	secretToRotate.Data[coderv1alpha1.DefaultLicenseSecretKey] = []byte("license-jwt-v2")
	if err := k8sClient.Update(ctx, secretToRotate); err != nil {
		t.Fatalf("update license secret: %v", err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("third reconcile control plane: %v", err)
	}
	if len(uploader.calls) != 2 {
		t.Fatalf("expected rotated license to trigger second upload call, got %d", len(uploader.calls))
	}
	if uploader.calls[1].licenseJWT != "license-jwt-v2" {
		t.Fatalf("expected rotated license JWT %q, got %q", "license-jwt-v2", uploader.calls[1].licenseJWT)
	}

	reconciledAfterRotation := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciledAfterRotation); err != nil {
		t.Fatalf("get reconciled control plane after rotation: %v", err)
	}
	if reconciledAfterRotation.Status.LicenseLastAppliedHash == initialHash {
		t.Fatalf("expected license hash to change after rotation")
	}
}

func TestReconcile_LicenseRollbackDuplicateUploadConverges(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	licenseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-rollback-secret", Namespace: "default"},
		Data: map[string][]byte{
			coderv1alpha1.DefaultLicenseSecretKey: []byte("license-jwt-a"),
		},
	}
	if err := k8sClient.Create(ctx, licenseSecret); err != nil {
		t.Fatalf("create license secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, licenseSecret)
	})

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-rollback", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example/license-rollback",
			}},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: licenseSecret.Name},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	duplicateErr := codersdk.NewTestError(http.StatusInternalServerError, http.MethodPost, "/api/v2/licenses")
	duplicateErr.Message = "duplicate key value violates unique constraint \"licenses_jwt_key\""
	uploader := &fakeLicenseUploader{addLicenseErrs: []error{nil, nil, duplicateErr}}
	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-license-rollback"}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
		LicenseUploader:           uploader,
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
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
	if len(uploader.calls) != 1 {
		t.Fatalf("expected initial upload call count 1, got %d", len(uploader.calls))
	}

	reconciledAfterInitial := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciledAfterInitial); err != nil {
		t.Fatalf("get reconciled control plane after initial apply: %v", err)
	}
	hashA := reconciledAfterInitial.Status.LicenseLastAppliedHash
	if hashA == "" {
		t.Fatalf("expected hash after initial apply")
	}

	secretToRotate := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: licenseSecret.Name, Namespace: licenseSecret.Namespace}, secretToRotate); err != nil {
		t.Fatalf("get license secret: %v", err)
	}
	secretToRotate.Data[coderv1alpha1.DefaultLicenseSecretKey] = []byte("license-jwt-b")
	if err := k8sClient.Update(ctx, secretToRotate); err != nil {
		t.Fatalf("rotate license to B: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("third reconcile control plane: %v", err)
	}

	reconciledAfterB := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciledAfterB); err != nil {
		t.Fatalf("get reconciled control plane after B apply: %v", err)
	}
	if reconciledAfterB.Status.LicenseLastAppliedHash == hashA {
		t.Fatalf("expected hash to change after applying B")
	}

	secretToRotateBack := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: licenseSecret.Name, Namespace: licenseSecret.Namespace}, secretToRotateBack); err != nil {
		t.Fatalf("get license secret for rollback: %v", err)
	}
	secretToRotateBack.Data[coderv1alpha1.DefaultLicenseSecretKey] = []byte("license-jwt-a")
	if err := k8sClient.Update(ctx, secretToRotateBack); err != nil {
		t.Fatalf("rollback license to A: %v", err)
	}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("fourth reconcile control plane: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Fatalf("expected duplicate rollback upload handling to converge without requeue, got %+v", result)
	}
	if len(uploader.calls) != 3 {
		t.Fatalf("expected three upload attempts across A->B->A rollback, got %d", len(uploader.calls))
	}

	reconciledAfterRollback := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciledAfterRollback); err != nil {
		t.Fatalf("get reconciled control plane after rollback: %v", err)
	}
	if reconciledAfterRollback.Status.LicenseLastAppliedHash != hashA {
		t.Fatalf("expected rollback to converge to original hash %q, got %q", hashA, reconciledAfterRollback.Status.LicenseLastAppliedHash)
	}
	licenseCondition := findCondition(t, reconciledAfterRollback.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionLicenseApplied)
	if licenseCondition.Status != metav1.ConditionTrue {
		t.Fatalf("expected license condition true after duplicate rollback handling, got %q", licenseCondition.Status)
	}
}

func TestReconcile_LicenseNotSupportedSetsConditionWithoutRequeue(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	licenseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-not-supported-secret", Namespace: "default"},
		Data: map[string][]byte{
			coderv1alpha1.DefaultLicenseSecretKey: []byte("license-oss"),
		},
	}
	if err := k8sClient.Create(ctx, licenseSecret); err != nil {
		t.Fatalf("create license secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, licenseSecret)
	})

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-license-not-supported", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example/license-not-supported",
			}},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: licenseSecret.Name},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-license-not-supported"}
	uploader := &fakeLicenseUploader{err: codersdk.NewTestError(http.StatusNotFound, http.MethodPost, "/api/v2/licenses")}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
		LicenseUploader:           uploader,
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("first reconcile control plane: %v", err)
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

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("second reconcile control plane: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Fatalf("expected no requeue for not-supported license API, got %+v", result)
	}
	if len(uploader.calls) != 1 {
		t.Fatalf("expected one attempted license upload call, got %d", len(uploader.calls))
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.LicenseLastApplied != nil {
		t.Fatalf("expected licenseLastApplied to remain nil when API is not supported")
	}
	if reconciled.Status.LicenseLastAppliedHash != "" {
		t.Fatalf("expected licenseLastAppliedHash to remain empty when API is not supported")
	}
	licenseCondition := findCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionLicenseApplied)
	if licenseCondition.Status != metav1.ConditionFalse {
		t.Fatalf("expected license condition status %q, got %q", metav1.ConditionFalse, licenseCondition.Status)
	}
	if licenseCondition.Reason != "NotSupported" {
		t.Fatalf("expected license condition reason %q, got %q", "NotSupported", licenseCondition.Reason)
	}
}

func TestReconcile_DefaultsApplied(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
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

func TestReconcile_DefaultOperatorAccess_MissingPostgresURL(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-missing-postgres-url",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-missing-postgres:latest",
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "should-not-be-used"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected operator access reconcile to request requeue, got %+v", result)
	}
	if provisioner.calls != 0 {
		t.Fatalf("expected provisioner not to be called when Postgres URL is missing, got %d calls", provisioner.calls)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=false when Postgres URL is missing")
	}
	if reconciled.Status.OperatorTokenSecretRef != nil {
		t.Fatalf("expected operator token secret ref to be nil when Postgres URL is missing")
	}

	secret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name + "-operator-token", Namespace: cp.Namespace}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no operator token secret when Postgres URL is missing, got error %v", err)
	}
}

func TestReconcile_OperatorAccess_Disabled(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-disabled",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-disabled:latest",
			OperatorAccess: coderv1alpha1.OperatorAccessSpec{
				Disabled: true,
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "should-not-be-used"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile control plane with operator access disabled: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result when operator access is disabled, got %+v", result)
	}
	if provisioner.calls != 0 {
		t.Fatalf("expected provisioner not to be called when operator access is disabled, got %d calls", provisioner.calls)
	}
	if provisioner.revokeCalls != 0 {
		t.Fatalf("expected revoke not to be called when no managed credentials exist, got %d calls", provisioner.revokeCalls)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=false when feature is disabled")
	}
	if reconciled.Status.OperatorTokenSecretRef != nil {
		t.Fatalf("expected operator token secret ref to be nil when feature is disabled")
	}

	secret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name + "-operator-token", Namespace: cp.Namespace}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no operator token secret when feature is disabled, got error %v", err)
	}
}

func TestReconcile_OperatorAccess_Disabled_DoesNotDeleteUnmanagedSecret(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-disabled-unmanaged-secret",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:          "test-operator-disabled-unmanaged-secret:latest",
			OperatorAccess: coderv1alpha1.OperatorAccessSpec{Disabled: true},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	unmanagedSecretName := cp.Name + "-operator-token"
	unmanagedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: unmanagedSecretName, Namespace: cp.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultTokenSecretKey: []byte("unmanaged-token"),
		},
	}
	if err := k8sClient.Create(ctx, unmanagedSecret); err != nil {
		t.Fatalf("failed to create unmanaged operator secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, unmanagedSecret)
	})

	provisioner := &fakeOperatorAccessProvisioner{}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile disabled control plane with unmanaged secret: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result for unmanaged secret, got %+v", result)
	}
	if provisioner.revokeCalls != 0 {
		t.Fatalf("expected revoke not to run for unmanaged secret, got %d calls", provisioner.revokeCalls)
	}

	reconciledSecret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: unmanagedSecretName, Namespace: cp.Namespace}, reconciledSecret); err != nil {
		t.Fatalf("expected unmanaged secret to remain, got error %v", err)
	}
	if got := string(reconciledSecret.Data[coderv1alpha1.DefaultTokenSecretKey]); got != "unmanaged-token" {
		t.Fatalf("expected unmanaged secret token %q, got %q", "unmanaged-token", got)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=false when disabled")
	}
	if reconciled.Status.OperatorTokenSecretRef != nil {
		t.Fatalf("expected operator token secret ref to stay nil for unmanaged secret")
	}
}

func TestReconcile_OperatorAccess_Disabled_RevokesWithoutStatusOrManagedSecret(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-disabled-revoke-without-status",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-disabled-revoke-without-status:latest",
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example.disabled.revoke/coder",
			}},
			OperatorAccess: coderv1alpha1.OperatorAccessSpec{Disabled: true},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile disabled control plane without status ref: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result, got %+v", result)
	}
	if provisioner.revokeCalls != 1 {
		t.Fatalf("expected revoke to run even without managed secret/status ref, got %d calls", provisioner.revokeCalls)
	}
	if got := provisioner.revokeRequests[0].PostgresURL; got != "postgres://example.disabled.revoke/coder" {
		t.Fatalf("expected revoke Postgres URL %q, got %q", "postgres://example.disabled.revoke/coder", got)
	}
	if got := provisioner.revokeRequests[0].TokenName; !strings.HasPrefix(got, "coder-k8s-operator-") {
		t.Fatalf("expected revoke token name to be scoped with prefix %q, got %q", "coder-k8s-operator-", got)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=false when disabled")
	}
	if reconciled.Status.OperatorTokenSecretRef != nil {
		t.Fatalf("expected operator token secret ref to be nil when disabled")
	}
}

func TestReconcile_OperatorAccess_Disabled_RevokesTokenAndDeletesSecret(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-disabled-cleanup",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-disabled-cleanup:latest",
			ExtraEnv: []corev1.EnvVar{
				{Name: "CODER_PG_CONNECTION_URL", Value: "postgres://example.disabled/coder"},
			},
			OperatorAccess: coderv1alpha1.OperatorAccessSpec{Disabled: true},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	managedSecretName := cp.Name + "-operator-token"
	managedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      managedSecretName,
			Namespace: cp.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: coderv1alpha1.GroupVersion.String(),
				Kind:       "CoderControlPlane",
				Name:       cp.Name,
				UID:        cp.UID,
				Controller: ptrTo(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultTokenSecretKey: []byte("stale-operator-token"),
		},
	}
	if err := k8sClient.Create(ctx, managedSecret); err != nil {
		t.Fatalf("failed to create managed operator secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, managedSecret)
	})

	provisioner := &fakeOperatorAccessProvisioner{}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile disabled control plane with existing credentials: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result when disabled cleanup succeeds, got %+v", result)
	}
	if provisioner.calls != 0 {
		t.Fatalf("expected ensure token not to be called when disabled, got %d calls", provisioner.calls)
	}
	if provisioner.revokeCalls != 1 {
		t.Fatalf("expected revoke to be called once when disabling existing credentials, got %d calls", provisioner.revokeCalls)
	}
	if got := provisioner.revokeRequests[0].PostgresURL; got != "postgres://example.disabled/coder" {
		t.Fatalf("expected revoke Postgres URL %q, got %q", "postgres://example.disabled/coder", got)
	}
	if got := provisioner.revokeRequests[0].OperatorUsername; got != "coder-k8s-operator" {
		t.Fatalf("expected revoke operator username %q, got %q", "coder-k8s-operator", got)
	}
	if got := provisioner.revokeRequests[0].TokenName; !strings.HasPrefix(got, "coder-k8s-operator-") {
		t.Fatalf("expected revoke token name to be scoped with prefix %q, got %q", "coder-k8s-operator-", got)
	}

	secret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: managedSecretName, Namespace: cp.Namespace}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected managed operator secret to be deleted, got error %v", err)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=false when disabled")
	}
	if reconciled.Status.OperatorTokenSecretRef != nil {
		t.Fatalf("expected operator token secret ref to be nil when disabled")
	}
}

func TestReconcile_OperatorAccess_Disabled_RetriesRevocationAfterFailure(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-disabled-retry-revoke",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-disabled-retry-revoke:latest",
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example.disabled.retry/coder",
			}},
			OperatorAccess: coderv1alpha1.OperatorAccessSpec{Disabled: true},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	managedSecretName := cp.Name + "-operator-token"
	managedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      managedSecretName,
			Namespace: cp.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: coderv1alpha1.GroupVersion.String(),
				Kind:       "CoderControlPlane",
				Name:       cp.Name,
				UID:        cp.UID,
				Controller: ptrTo(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultTokenSecretKey: []byte("stale-operator-token"),
		},
	}
	if err := k8sClient.Create(ctx, managedSecret); err != nil {
		t.Fatalf("failed to create managed operator secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, managedSecret)
	})

	provisioner := &fakeOperatorAccessProvisioner{revokeErr: errors.New("revoke failed")}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile disabled control plane with revoke error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected requeue when revoke fails, got %+v", result)
	}
	if provisioner.revokeCalls != 1 {
		t.Fatalf("expected revoke to be called once, got %d calls", provisioner.revokeCalls)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if reconciled.Status.OperatorTokenSecretRef == nil {
		t.Fatalf("expected status to retain operator token secret ref while cleanup is pending")
	}
	if reconciled.Status.OperatorTokenSecretRef.Name != managedSecretName {
		t.Fatalf("expected pending secret ref name %q, got %q", managedSecretName, reconciled.Status.OperatorTokenSecretRef.Name)
	}
	if reconciled.Status.OperatorTokenSecretRef.Key != coderv1alpha1.DefaultTokenSecretKey {
		t.Fatalf("expected pending secret ref key %q, got %q", coderv1alpha1.DefaultTokenSecretKey, reconciled.Status.OperatorTokenSecretRef.Key)
	}
	if reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=false while cleanup is pending")
	}

	secret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: managedSecretName, Namespace: cp.Namespace}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected managed operator secret to be deleted before revoke retry, got error %v", err)
	}

	result, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("second reconcile disabled control plane with revoke error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected second reconcile to requeue while revoke keeps failing, got %+v", result)
	}
	if provisioner.revokeCalls != 2 {
		t.Fatalf("expected revoke to be retried on second reconcile, got %d calls", provisioner.revokeCalls)
	}
}

func TestReconcile_OperatorAccess_MalformedManagedSecret_ReprovisionsToken(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-malformed-secret",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-malformed-secret:latest",
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example.malformed/coder",
			}},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	managedSecretName := cp.Name + "-operator-token"
	managedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: managedSecretName, Namespace: cp.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"wrong-key": []byte("not-a-token"),
		},
	}
	if err := k8sClient.Create(ctx, managedSecret); err != nil {
		t.Fatalf("failed to create malformed managed secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, managedSecret)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "recovered-operator-token"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile control plane with malformed managed secret: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result, got %+v", result)
	}
	if provisioner.calls != 1 {
		t.Fatalf("expected provisioner to be called once, got %d calls", provisioner.calls)
	}
	if got := provisioner.requests[0].ExistingToken; got != "" {
		t.Fatalf("expected existing token to be empty when managed secret is malformed, got %q", got)
	}

	reconciledSecret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: managedSecretName, Namespace: cp.Namespace}, reconciledSecret); err != nil {
		t.Fatalf("get reconciled managed secret: %v", err)
	}
	if got := string(reconciledSecret.Data[coderv1alpha1.DefaultTokenSecretKey]); got != "recovered-operator-token" {
		t.Fatalf("expected reconciled managed token %q, got %q", "recovered-operator-token", got)
	}
}

func TestReconcile_OperatorAccess_UsesDistinctTokenNamesPerControlPlane(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp1 := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator-access-token-name-one", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-token-name-one:latest",
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example.shared/coder",
			}},
		},
	}
	if err := k8sClient.Create(ctx, cp1); err != nil {
		t.Fatalf("failed to create first test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp1)
	})

	cp2 := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator-access-token-name-two", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-token-name-two:latest",
			ExtraEnv: []corev1.EnvVar{{
				Name:  "CODER_PG_CONNECTION_URL",
				Value: "postgres://example.shared/coder",
			}},
		},
	}
	if err := k8sClient.Create(ctx, cp2); err != nil {
		t.Fatalf("failed to create second test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp2)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "shared-operator-token"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp1.Name, Namespace: cp1.Namespace}}); err != nil {
		t.Fatalf("reconcile first control plane: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp2.Name, Namespace: cp2.Namespace}}); err != nil {
		t.Fatalf("reconcile second control plane: %v", err)
	}

	if provisioner.calls != 2 {
		t.Fatalf("expected provisioner to be called twice, got %d calls", provisioner.calls)
	}
	firstTokenName := provisioner.requests[0].TokenName
	secondTokenName := provisioner.requests[1].TokenName
	if firstTokenName == "" || secondTokenName == "" {
		t.Fatalf("expected non-empty token names, got %q and %q", firstTokenName, secondTokenName)
	}
	if !strings.HasPrefix(firstTokenName, "coder-k8s-operator-") {
		t.Fatalf("expected first token name to be scoped with prefix %q, got %q", "coder-k8s-operator-", firstTokenName)
	}
	if !strings.HasPrefix(secondTokenName, "coder-k8s-operator-") {
		t.Fatalf("expected second token name to be scoped with prefix %q, got %q", "coder-k8s-operator-", secondTokenName)
	}
	if firstTokenName == secondTokenName {
		t.Fatalf("expected distinct token names for different control planes, got %q", firstTokenName)
	}
}

func TestReconcile_OperatorAccess_ResolvesLiteralPostgresURLAndCreatesTokenSecret(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-literal-postgres-url",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-literal:latest",
			ExtraEnv: []corev1.EnvVar{
				{Name: "CODER_PG_CONNECTION_URL", Value: "postgres://example.literal/coder"},
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-literal"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile control plane with literal postgres URL: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result, got %+v", result)
	}
	if provisioner.calls != 1 {
		t.Fatalf("expected provisioner to be called once, got %d calls", provisioner.calls)
	}
	if got := provisioner.requests[0].PostgresURL; got != "postgres://example.literal/coder" {
		t.Fatalf("expected provisioner Postgres URL %q, got %q", "postgres://example.literal/coder", got)
	}
	if got := provisioner.requests[0].ExistingToken; got != "" {
		t.Fatalf("expected first provisioner call existing token to be empty, got %q", got)
	}

	secret := &corev1.Secret{}
	secretName := cp.Name + "-operator-token"
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cp.Namespace}, secret); err != nil {
		t.Fatalf("expected operator token secret %q: %v", secretName, err)
	}
	if got := string(secret.Data[coderv1alpha1.DefaultTokenSecretKey]); got != "operator-token-literal" {
		t.Fatalf("expected operator token secret value %q, got %q", "operator-token-literal", got)
	}
	assertSingleControllerOwnerReference(t, secret.OwnerReferences, cp.Name)

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if !reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=true")
	}
	if reconciled.Status.OperatorTokenSecretRef == nil {
		t.Fatalf("expected operator token secret reference to be set")
	}
	if reconciled.Status.OperatorTokenSecretRef.Name != secretName {
		t.Fatalf("expected operator token secret ref name %q, got %q", secretName, reconciled.Status.OperatorTokenSecretRef.Name)
	}
	if reconciled.Status.OperatorTokenSecretRef.Key != coderv1alpha1.DefaultTokenSecretKey {
		t.Fatalf("expected operator token secret ref key %q, got %q", coderv1alpha1.DefaultTokenSecretKey, reconciled.Status.OperatorTokenSecretRef.Key)
	}

	result, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("second reconcile control plane: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result on second reconcile, got %+v", result)
	}
	if provisioner.calls != 2 {
		t.Fatalf("expected provisioner to be called again to validate existing token, got %d calls", provisioner.calls)
	}
	if got := provisioner.requests[1].PostgresURL; got != "postgres://example.literal/coder" {
		t.Fatalf("expected second provisioner Postgres URL %q, got %q", "postgres://example.literal/coder", got)
	}
	if got := provisioner.requests[1].ExistingToken; got != "operator-token-literal" {
		t.Fatalf("expected second provisioner call existing token %q, got %q", "operator-token-literal", got)
	}
}

func TestReconcile_OperatorAccess_ResolvesPostgresURLFromSecretRef(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	postgresURLSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-postgres-url",
			Namespace: "default",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"url": []byte("postgres://example.secret/coder"),
		},
	}
	if err := k8sClient.Create(ctx, postgresURLSecret); err != nil {
		t.Fatalf("failed to create postgres URL secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, postgresURLSecret)
	})

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-operator-access-secret-postgres-url",
			Namespace: "default",
		},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-operator-secret-ref:latest",
			ExtraEnv: []corev1.EnvVar{{
				Name: "CODER_PG_CONNECTION_URL",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: postgresURLSecret.Name},
						Key:                  "url",
					},
				},
			}},
			OperatorAccess: coderv1alpha1.OperatorAccessSpec{
				GeneratedTokenSecretName: "test-operator-custom-token",
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("failed to create test CoderControlPlane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-secret-ref"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		t.Fatalf("reconcile control plane with secret postgres URL: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result, got %+v", result)
	}
	if provisioner.calls != 1 {
		t.Fatalf("expected provisioner to be called once, got %d calls", provisioner.calls)
	}
	if got := provisioner.requests[0].PostgresURL; got != "postgres://example.secret/coder" {
		t.Fatalf("expected provisioner Postgres URL %q, got %q", "postgres://example.secret/coder", got)
	}

	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-operator-custom-token", Namespace: cp.Namespace}, secret); err != nil {
		t.Fatalf("expected custom operator token secret: %v", err)
	}
	if got := string(secret.Data[coderv1alpha1.DefaultTokenSecretKey]); got != "operator-token-secret-ref" {
		t.Fatalf("expected operator token secret value %q, got %q", "operator-token-secret-ref", got)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	if !reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=true")
	}
	if reconciled.Status.OperatorTokenSecretRef == nil {
		t.Fatalf("expected operator token secret reference to be set")
	}
	if reconciled.Status.OperatorTokenSecretRef.Name != "test-operator-custom-token" {
		t.Fatalf("expected operator token secret ref name %q, got %q", "test-operator-custom-token", reconciled.Status.OperatorTokenSecretRef.Name)
	}
}

func TestReconcile_EntitlementsStatusFields(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	testCases := []struct {
		name                       string
		entitlements               codersdk.Entitlements
		expectedTier               string
		expectedProvisionerFeature string
	}{
		{
			name: "none",
			entitlements: codersdk.Entitlements{
				Features: map[codersdk.FeatureName]codersdk.Feature{
					codersdk.FeatureExternalProvisionerDaemons: {Entitlement: codersdk.EntitlementNotEntitled},
				},
				HasLicense: false,
			},
			expectedTier:               coderv1alpha1.CoderControlPlaneLicenseTierNone,
			expectedProvisionerFeature: string(codersdk.EntitlementNotEntitled),
		},
		{
			name: "trial",
			entitlements: codersdk.Entitlements{
				Features: map[codersdk.FeatureName]codersdk.Feature{
					codersdk.FeatureExternalProvisionerDaemons: {Entitlement: codersdk.EntitlementGracePeriod},
				},
				HasLicense: true,
				Trial:      true,
			},
			expectedTier:               coderv1alpha1.CoderControlPlaneLicenseTierTrial,
			expectedProvisionerFeature: string(codersdk.EntitlementGracePeriod),
		},
		{
			name: "premium",
			entitlements: codersdk.Entitlements{
				Features: map[codersdk.FeatureName]codersdk.Feature{
					codersdk.FeatureExternalProvisionerDaemons: {Entitlement: codersdk.EntitlementEntitled},
					codersdk.FeatureCustomRoles:                {Entitlement: codersdk.EntitlementEntitled},
				},
				HasLicense: true,
			},
			expectedTier:               coderv1alpha1.CoderControlPlaneLicenseTierPremium,
			expectedProvisionerFeature: string(codersdk.EntitlementEntitled),
		},
		{
			name: "enterprise",
			entitlements: codersdk.Entitlements{
				Features: map[codersdk.FeatureName]codersdk.Feature{
					codersdk.FeatureExternalProvisionerDaemons: {Entitlement: codersdk.EntitlementEntitled},
					codersdk.FeatureCustomRoles:                {Entitlement: codersdk.EntitlementNotEntitled},
					codersdk.FeatureMultipleOrganizations:      {Entitlement: codersdk.EntitlementNotEntitled},
				},
				HasLicense: true,
			},
			expectedTier:               coderv1alpha1.CoderControlPlaneLicenseTierEnterprise,
			expectedProvisionerFeature: string(codersdk.EntitlementEntitled),
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			controlPlaneName := "test-entitlements-" + strings.ToLower(testCase.name)

			cp := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: "default",
				},
				Spec: coderv1alpha1.CoderControlPlaneSpec{
					Image: "test-entitlements:latest",
					ExtraEnv: []corev1.EnvVar{{
						Name:  "CODER_PG_CONNECTION_URL",
						Value: "postgres://example.test/coder",
					}},
				},
			}
			if err := k8sClient.Create(ctx, cp); err != nil {
				t.Fatalf("failed to create test CoderControlPlane: %v", err)
			}
			t.Cleanup(func() {
				_ = k8sClient.Delete(ctx, cp)
			})

			provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-entitlements"}
			inspector := &fakeEntitlementsInspector{response: testCase.entitlements}
			r := &controller.CoderControlPlaneReconciler{
				Client:                    k8sClient,
				Scheme:                    scheme,
				OperatorAccessProvisioner: provisioner,
				EntitlementsInspector:     inspector,
			}

			namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
				t.Fatalf("reconcile control plane: %v", err)
			}

			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, namespacedName, deployment); err != nil {
				t.Fatalf("get deployment: %v", err)
			}
			deployment.Status.Replicas = 1
			deployment.Status.ReadyReplicas = 1
			if err := k8sClient.Status().Update(ctx, deployment); err != nil {
				t.Fatalf("update deployment status: %v", err)
			}

			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
				t.Fatalf("reconcile control plane after deployment ready: %v", err)
			}

			reconciled := &coderv1alpha1.CoderControlPlane{}
			if err := k8sClient.Get(ctx, namespacedName, reconciled); err != nil {
				t.Fatalf("get reconciled control plane: %v", err)
			}
			if reconciled.Status.LicenseTier != testCase.expectedTier {
				t.Fatalf("expected license tier %q, got %q", testCase.expectedTier, reconciled.Status.LicenseTier)
			}
			if reconciled.Status.ExternalProvisionerDaemonsEntitlement != testCase.expectedProvisionerFeature {
				t.Fatalf("expected external provisioner entitlement %q, got %q", testCase.expectedProvisionerFeature, reconciled.Status.ExternalProvisionerDaemonsEntitlement)
			}
			if reconciled.Status.EntitlementsLastChecked == nil {
				t.Fatal("expected entitlementsLastChecked to be set")
			}
			firstCheckedAt := reconciled.Status.EntitlementsLastChecked.DeepCopy()

			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
				t.Fatalf("reconcile control plane with unchanged entitlements: %v", err)
			}

			reconciledAgain := &coderv1alpha1.CoderControlPlane{}
			if err := k8sClient.Get(ctx, namespacedName, reconciledAgain); err != nil {
				t.Fatalf("get reconciled control plane after stable reconcile: %v", err)
			}
			if reconciledAgain.Status.EntitlementsLastChecked == nil {
				t.Fatal("expected entitlementsLastChecked to remain set")
			}
			if !reconciledAgain.Status.EntitlementsLastChecked.Equal(firstCheckedAt) {
				t.Fatalf("expected entitlementsLastChecked to remain %s, got %s", firstCheckedAt.UTC().Format(time.RFC3339Nano), reconciledAgain.Status.EntitlementsLastChecked.UTC().Format(time.RFC3339Nano))
			}
			if inspector.calls == 0 {
				t.Fatal("expected entitlements inspector to be called")
			}
		})
	}
}

func TestReconcile_ServiceAccount(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	testCases := []struct {
		name                string
		controlPlaneName    string
		serviceAccount      coderv1alpha1.ServiceAccountSpec
		expectedName        string
		expectCreated       bool
		expectedLabels      map[string]string
		expectedAnnotations map[string]string
	}{
		{
			name:             "DefaultName",
			controlPlaneName: "test-serviceaccount-default",
			expectedName:     "test-serviceaccount-default",
			expectCreated:    true,
		},
		{
			name:             "CustomName",
			controlPlaneName: "test-serviceaccount-custom",
			serviceAccount: coderv1alpha1.ServiceAccountSpec{
				Name: "custom-service-account",
			},
			expectedName:  "custom-service-account",
			expectCreated: true,
		},
		{
			name:             "CustomLabelsAndAnnotations",
			controlPlaneName: "test-serviceaccount-metadata",
			serviceAccount: coderv1alpha1.ServiceAccountSpec{
				Name: "test-serviceaccount-metadata-sa",
				Labels: map[string]string{
					"custom-label": "label-value",
				},
				Annotations: map[string]string{
					"custom-annotation": "annotation-value",
				},
			},
			expectedName:  "test-serviceaccount-metadata-sa",
			expectCreated: true,
			expectedLabels: map[string]string{
				"custom-label": "label-value",
			},
			expectedAnnotations: map[string]string{
				"custom-annotation": "annotation-value",
			},
		},
		{
			name:             "CreationDisabled",
			controlPlaneName: "test-serviceaccount-disabled",
			serviceAccount: coderv1alpha1.ServiceAccountSpec{
				DisableCreate: true,
			},
			expectedName:  "test-serviceaccount-disabled",
			expectCreated: false,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			cp := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: testCase.controlPlaneName, Namespace: "default"},
				Spec: coderv1alpha1.CoderControlPlaneSpec{
					Image:          "test-serviceaccount:latest",
					ServiceAccount: testCase.serviceAccount,
				},
			}
			if err := k8sClient.Create(ctx, cp); err != nil {
				t.Fatalf("create control plane: %v", err)
			}
			t.Cleanup(func() {
				_ = k8sClient.Delete(ctx, cp)
			})

			r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
				t.Fatalf("reconcile control plane: %v", err)
			}

			serviceAccount := &corev1.ServiceAccount{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: testCase.expectedName, Namespace: cp.Namespace}, serviceAccount)
			if !testCase.expectCreated {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("expected service account %s/%s to be absent, got error: %v", cp.Namespace, testCase.expectedName, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("get service account: %v", err)
			}

			if serviceAccount.Name != testCase.expectedName {
				t.Fatalf("expected service account name %q, got %q", testCase.expectedName, serviceAccount.Name)
			}
			if serviceAccount.Labels["app.kubernetes.io/name"] != "coder-control-plane" {
				t.Fatalf("expected managed label app.kubernetes.io/name=coder-control-plane, got %q", serviceAccount.Labels["app.kubernetes.io/name"])
			}
			if serviceAccount.Labels["app.kubernetes.io/instance"] != cp.Name {
				t.Fatalf("expected managed label app.kubernetes.io/instance=%q, got %q", cp.Name, serviceAccount.Labels["app.kubernetes.io/instance"])
			}
			if serviceAccount.Labels["app.kubernetes.io/managed-by"] != "coder-k8s" {
				t.Fatalf("expected managed label app.kubernetes.io/managed-by=coder-k8s, got %q", serviceAccount.Labels["app.kubernetes.io/managed-by"])
			}
			for key, value := range testCase.expectedLabels {
				if serviceAccount.Labels[key] != value {
					t.Fatalf("expected service account label %q=%q, got %q", key, value, serviceAccount.Labels[key])
				}
			}
			for key, value := range testCase.expectedAnnotations {
				if serviceAccount.Annotations[key] != value {
					t.Fatalf("expected service account annotation %q=%q, got %q", key, value, serviceAccount.Annotations[key])
				}
			}
		})
	}

	t.Run("DisableCreateDetachesManagedServiceAccount", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-serviceaccount-disable-detach", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-serviceaccount:latest",
				ServiceAccount: coderv1alpha1.ServiceAccountSpec{
					Name: "test-serviceaccount-disable-detach-sa",
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, cp)
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("reconcile control plane before disabling service account creation: %v", err)
		}

		serviceAccountName := cp.Spec.ServiceAccount.Name
		serviceAccount := &corev1.ServiceAccount{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceAccountName, Namespace: cp.Namespace}, serviceAccount); err != nil {
			t.Fatalf("get managed service account: %v", err)
		}
		ownerReference := metav1.GetControllerOf(serviceAccount)
		if ownerReference == nil || ownerReference.UID != cp.UID {
			t.Fatalf("expected service account to be controller-owned before disableCreate=true, got %#v", ownerReference)
		}

		latest := &coderv1alpha1.CoderControlPlane{}
		if err := k8sClient.Get(ctx, namespacedName, latest); err != nil {
			t.Fatalf("get latest control plane for disableCreate update: %v", err)
		}
		latest.Spec.ServiceAccount.DisableCreate = true
		if err := k8sClient.Update(ctx, latest); err != nil {
			t.Fatalf("update control plane to disable service account creation: %v", err)
		}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("reconcile control plane after disabling service account creation: %v", err)
		}

		serviceAccount = &corev1.ServiceAccount{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceAccountName, Namespace: cp.Namespace}, serviceAccount); err != nil {
			t.Fatalf("get service account after disableCreate=true: %v", err)
		}
		if ownerReference := metav1.GetControllerOf(serviceAccount); ownerReference != nil {
			t.Fatalf("expected service account controller reference to be removed when disableCreate=true, got %#v", ownerReference)
		}
	})

}

func TestReconcile_WorkspaceRBAC(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	t.Run("RoleAndRoleBindingCreated", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-workspace-rbac-default", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-workspace-rbac:latest",
				ServiceAccount: coderv1alpha1.ServiceAccountSpec{
					Name: "test-workspace-rbac-default-sa",
				},
				RBAC: coderv1alpha1.RBACSpec{
					WorkspacePerms:    true,
					EnableDeployments: true,
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, cp)
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
			t.Fatalf("reconcile control plane: %v", err)
		}

		serviceAccountName := cp.Spec.ServiceAccount.Name
		roleName := serviceAccountName + "-workspace-perms"
		role := &rbacv1.Role{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: cp.Namespace}, role); err != nil {
			t.Fatalf("get workspace role: %v", err)
		}
		if !roleContainsRuleForResource(role.Rules, "", "pods") {
			t.Fatal("expected workspace role to include pods permissions")
		}
		if !roleContainsRuleForResource(role.Rules, "", "persistentvolumeclaims") {
			t.Fatal("expected workspace role to include persistentvolumeclaims permissions")
		}
		if !roleContainsRuleForResource(role.Rules, "apps", "deployments") {
			t.Fatal("expected workspace role to include deployments permissions")
		}

		roleBinding := &rbacv1.RoleBinding{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceAccountName, Namespace: cp.Namespace}, roleBinding); err != nil {
			t.Fatalf("get workspace role binding: %v", err)
		}
		if roleBinding.RoleRef.Kind != "Role" || roleBinding.RoleRef.Name != roleName {
			t.Fatalf("expected role binding roleRef to Role %q, got %#v", roleName, roleBinding.RoleRef)
		}
		if len(roleBinding.Subjects) != 1 {
			t.Fatalf("expected one role binding subject, got %d", len(roleBinding.Subjects))
		}
		subject := roleBinding.Subjects[0]
		if subject.Kind != rbacv1.ServiceAccountKind || subject.Name != serviceAccountName || subject.Namespace != cp.Namespace {
			t.Fatalf("expected role binding service account subject %s/%s, got %#v", cp.Namespace, serviceAccountName, subject)
		}
	})

	t.Run("DeploymentsRuleDisabled", func(t *testing.T) {
		cp := createCoderControlPlaneUnstructured(ctx, t, "test-workspace-rbac-no-deployments", "default", map[string]any{
			"image": "test-workspace-rbac:latest",
			"serviceAccount": map[string]any{
				"name": "test-workspace-rbac-no-deployments-sa",
			},
			"rbac": map[string]any{
				"workspacePerms":    true,
				"enableDeployments": false,
			},
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
			t.Fatalf("reconcile control plane: %v", err)
		}

		roleName := cp.Spec.ServiceAccount.Name + "-workspace-perms"
		role := &rbacv1.Role{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: cp.Namespace}, role); err != nil {
			t.Fatalf("get workspace role: %v", err)
		}
		if roleContainsRuleForResource(role.Rules, "apps", "deployments") {
			t.Fatal("expected workspace role deployments permissions to be omitted when enableDeployments=false")
		}
	})

	t.Run("RBACDisabled", func(t *testing.T) {
		cp := createCoderControlPlaneUnstructured(ctx, t, "test-workspace-rbac-disabled", "default", map[string]any{
			"image": "test-workspace-rbac:latest",
			"rbac": map[string]any{
				"workspacePerms": false,
			},
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
			t.Fatalf("reconcile control plane: %v", err)
		}

		role := &rbacv1.Role{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name + "-workspace-perms", Namespace: cp.Namespace}, role)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected workspace role to be absent when RBAC is disabled, got error: %v", err)
		}

		roleBinding := &rbacv1.RoleBinding{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, roleBinding)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected workspace role binding to be absent when RBAC is disabled, got error: %v", err)
		}
	})

	t.Run("RBACDisabledCleansPreviouslyManagedRoles", func(t *testing.T) {
		workspaceNamespace := "workspace-rbac-cleanup-disabled"
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceNamespace}}
		if err := k8sClient.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create workspace namespace: %v", err)
		}

		serviceAccountName := "test-workspace-rbac-cleanup-disabled-sa"
		cp := createCoderControlPlaneUnstructured(ctx, t, "test-workspace-rbac-cleanup-disabled", "default", map[string]any{
			"image": "test-workspace-rbac:latest",
			"serviceAccount": map[string]any{
				"name": serviceAccountName,
			},
			"rbac": map[string]any{
				"workspacePerms":      true,
				"enableDeployments":   true,
				"workspaceNamespaces": []any{workspaceNamespace},
			},
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("reconcile control plane before disable: %v", err)
		}

		roleName := serviceAccountName + "-workspace-perms"
		roleBindingName := serviceAccountName
		for _, namespaceName := range []string{cp.Namespace, workspaceNamespace} {
			role := &rbacv1.Role{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: namespaceName}, role); err != nil {
				t.Fatalf("expected workspace role %s/%s to exist before disabling RBAC: %v", namespaceName, roleName, err)
			}
			roleBinding := &rbacv1.RoleBinding{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespaceName}, roleBinding); err != nil {
				t.Fatalf("expected workspace role binding %s/%s to exist before disabling RBAC: %v", namespaceName, roleBindingName, err)
			}
		}

		unstructuredCP := &unstructured.Unstructured{}
		unstructuredCP.SetAPIVersion(coderv1alpha1.GroupVersion.String())
		unstructuredCP.SetKind("CoderControlPlane")
		if err := k8sClient.Get(ctx, namespacedName, unstructuredCP); err != nil {
			t.Fatalf("get unstructured control plane for RBAC disable update: %v", err)
		}
		if err := unstructured.SetNestedField(unstructuredCP.Object, false, "spec", "rbac", "workspacePerms"); err != nil {
			t.Fatalf("set spec.rbac.workspacePerms=false: %v", err)
		}
		if err := k8sClient.Update(ctx, unstructuredCP); err != nil {
			t.Fatalf("update control plane to disable workspace RBAC: %v", err)
		}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("reconcile control plane after disable: %v", err)
		}

		for _, namespaceName := range []string{cp.Namespace, workspaceNamespace} {
			role := &rbacv1.Role{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: namespaceName}, role)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("expected workspace role %s/%s to be removed after disabling RBAC, got: %v", namespaceName, roleName, err)
			}
			roleBinding := &rbacv1.RoleBinding{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespaceName}, roleBinding)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("expected workspace role binding %s/%s to be removed after disabling RBAC, got: %v", namespaceName, roleBindingName, err)
			}
		}
	})

	t.Run("DeleteControlPlaneCleansCrossNamespaceRBAC", func(t *testing.T) {
		workspaceNamespace := "workspace-rbac-cleanup-delete"
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceNamespace}}
		if err := k8sClient.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create workspace namespace: %v", err)
		}

		serviceAccountName := "test-workspace-rbac-cleanup-delete-sa"
		cp := createCoderControlPlaneUnstructured(ctx, t, "test-workspace-rbac-cleanup-delete", "default", map[string]any{
			"image": "test-workspace-rbac:latest",
			"serviceAccount": map[string]any{
				"name": serviceAccountName,
			},
			"rbac": map[string]any{
				"workspacePerms":      true,
				"enableDeployments":   true,
				"workspaceNamespaces": []any{workspaceNamespace},
			},
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName})
		if err != nil {
			t.Fatalf("reconcile control plane before delete: %v", err)
		}
		if result.RequeueAfter <= 0 {
			t.Fatalf("expected cross-namespace workspace RBAC to request periodic drift requeue, got %+v", result)
		}

		roleName := serviceAccountName + "-workspace-perms"
		roleBindingName := serviceAccountName
		role := &rbacv1.Role{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: workspaceNamespace}, role); err != nil {
			t.Fatalf("expected cross-namespace role %s/%s before delete: %v", workspaceNamespace, roleName, err)
		}
		roleBinding := &rbacv1.RoleBinding{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: workspaceNamespace}, roleBinding); err != nil {
			t.Fatalf("expected cross-namespace role binding %s/%s before delete: %v", workspaceNamespace, roleBindingName, err)
		}

		if err := k8sClient.Delete(ctx, cp); err != nil {
			t.Fatalf("delete control plane: %v", err)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("reconcile control plane deletion: %v", err)
		}

		role = &rbacv1.Role{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: workspaceNamespace}, role)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected cross-namespace role %s/%s to be removed after control plane deletion, got: %v", workspaceNamespace, roleName, err)
		}
		roleBinding = &rbacv1.RoleBinding{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: workspaceNamespace}, roleBinding)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected cross-namespace role binding %s/%s to be removed after control plane deletion, got: %v", workspaceNamespace, roleBindingName, err)
		}
	})

	t.Run("RBACCleanupPreservesUnmanagedLabeledResources", func(t *testing.T) {
		workspaceNamespace := "workspace-rbac-preserve-unmanaged"
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceNamespace}}
		if err := k8sClient.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create workspace namespace: %v", err)
		}

		serviceAccountName := "test-workspace-rbac-preserve-unmanaged-sa"
		cp := createCoderControlPlaneUnstructured(ctx, t, "test-workspace-rbac-preserve-unmanaged", "default", map[string]any{
			"image": "test-workspace-rbac:latest",
			"serviceAccount": map[string]any{
				"name": serviceAccountName,
			},
			"rbac": map[string]any{
				"workspacePerms":      true,
				"enableDeployments":   true,
				"workspaceNamespaces": []any{workspaceNamespace},
			},
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("reconcile control plane before creating unmanaged RBAC: %v", err)
		}

		workspaceLabels := map[string]string{
			"app.kubernetes.io/name":            "coder-control-plane",
			"app.kubernetes.io/instance":        cp.Name,
			"app.kubernetes.io/managed-by":      "coder-k8s",
			"coder.com/control-plane":           cp.Name,
			"coder.com/control-plane-namespace": cp.Namespace,
		}
		manualRoleName := "manual-external-workspace-role"
		manualRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      manualRoleName,
				Namespace: workspaceNamespace,
				Labels:    workspaceLabels,
			},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get"},
			}},
		}
		if err := k8sClient.Create(ctx, manualRole); err != nil {
			t.Fatalf("create unmanaged role with matching labels: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), manualRole)
		})

		manualRoleBindingName := "manual-external-workspace-rolebinding"
		manualRoleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      manualRoleBindingName,
				Namespace: workspaceNamespace,
				Labels:    workspaceLabels,
			},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: manualRoleName},
			Subjects: []rbacv1.Subject{{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccountName,
				Namespace: cp.Namespace,
			}},
		}
		if err := k8sClient.Create(ctx, manualRoleBinding); err != nil {
			t.Fatalf("create unmanaged role binding with matching labels: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), manualRoleBinding)
		})

		unstructuredCP := &unstructured.Unstructured{}
		unstructuredCP.SetAPIVersion(coderv1alpha1.GroupVersion.String())
		unstructuredCP.SetKind("CoderControlPlane")
		if err := k8sClient.Get(ctx, namespacedName, unstructuredCP); err != nil {
			t.Fatalf("get unstructured control plane for RBAC disable update: %v", err)
		}
		if err := unstructured.SetNestedField(unstructuredCP.Object, false, "spec", "rbac", "workspacePerms"); err != nil {
			t.Fatalf("set spec.rbac.workspacePerms=false: %v", err)
		}
		if err := k8sClient.Update(ctx, unstructuredCP); err != nil {
			t.Fatalf("update control plane to disable workspace RBAC: %v", err)
		}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("reconcile control plane after disabling workspace RBAC: %v", err)
		}

		manualRole = &rbacv1.Role{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: manualRoleName, Namespace: workspaceNamespace}, manualRole); err != nil {
			t.Fatalf("expected unmanaged role with matching labels to be preserved, got: %v", err)
		}
		manualRoleBinding = &rbacv1.RoleBinding{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: manualRoleBindingName, Namespace: workspaceNamespace}, manualRoleBinding); err != nil {
			t.Fatalf("expected unmanaged role binding with matching labels to be preserved, got: %v", err)
		}
	})

	t.Run("ExtraRulesAppended", func(t *testing.T) {
		extraRule := rbacv1.PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get", "list"},
		}
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-workspace-rbac-extra-rules", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-workspace-rbac:latest",
				ServiceAccount: coderv1alpha1.ServiceAccountSpec{
					Name: "test-workspace-rbac-extra-rules-sa",
				},
				RBAC: coderv1alpha1.RBACSpec{
					WorkspacePerms:    true,
					EnableDeployments: true,
					ExtraRules:        []rbacv1.PolicyRule{extraRule},
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, cp)
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
			t.Fatalf("reconcile control plane: %v", err)
		}

		roleName := cp.Spec.ServiceAccount.Name + "-workspace-perms"
		role := &rbacv1.Role{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: cp.Namespace}, role); err != nil {
			t.Fatalf("get workspace role: %v", err)
		}
		if !roleContainsRuleForResource(role.Rules, "", "configmaps") {
			t.Fatal("expected workspace role to include extra configmaps rule")
		}
	})
}

func TestReconcile_DeploymentAlignment(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	t.Run("PortAndHAEnvAndDefaultAccessURL", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deployment-alignment-default", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-deployment-alignment:latest",
				ServiceAccount: coderv1alpha1.ServiceAccountSpec{
					Name: "test-deployment-alignment-sa",
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
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
			t.Fatalf("get deployment: %v", err)
		}
		if len(deployment.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("expected one deployment container, got %d", len(deployment.Spec.Template.Spec.Containers))
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		if len(container.Args) == 0 || container.Args[0] != "--http-address=0.0.0.0:8080" {
			t.Fatalf("expected deployment arg --http-address=0.0.0.0:8080, got %v", container.Args)
		}
		if !containerHasPort(container, "http", 8080) {
			t.Fatalf("expected deployment container to expose http port 8080, got %+v", container.Ports)
		}

		kubePodIPEnv := mustFindEnvVar(t, container.Env, "KUBE_POD_IP")
		if kubePodIPEnv.ValueFrom == nil || kubePodIPEnv.ValueFrom.FieldRef == nil || kubePodIPEnv.ValueFrom.FieldRef.FieldPath != "status.podIP" {
			t.Fatalf("expected KUBE_POD_IP fieldRef status.podIP, got %#v", kubePodIPEnv.ValueFrom)
		}
		if got := mustFindEnvVar(t, container.Env, "CODER_DERP_SERVER_RELAY_URL").Value; got != "http://$(KUBE_POD_IP):8080" {
			t.Fatalf("expected CODER_DERP_SERVER_RELAY_URL %q, got %q", "http://$(KUBE_POD_IP):8080", got)
		}
		expectedAccessURL := "http://" + cp.Name + "." + cp.Namespace + ".svc.cluster.local"
		if got := mustFindEnvVar(t, container.Env, "CODER_ACCESS_URL").Value; got != expectedAccessURL {
			t.Fatalf("expected default CODER_ACCESS_URL %q, got %q", expectedAccessURL, got)
		}

		if deployment.Spec.Template.Spec.ServiceAccountName != cp.Spec.ServiceAccount.Name {
			t.Fatalf("expected pod serviceAccountName %q, got %q", cp.Spec.ServiceAccount.Name, deployment.Spec.Template.Spec.ServiceAccountName)
		}
	})

	t.Run("DefaultAccessURLIncludesCustomServicePort", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deployment-alignment-custom-service-port", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-deployment-alignment:latest",
				Service: coderv1alpha1.ServiceSpec{
					Port: 8080,
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
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
			t.Fatalf("get deployment: %v", err)
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		expectedAccessURL := "http://" + cp.Name + "." + cp.Namespace + ".svc.cluster.local:8080"
		if got := mustFindEnvVar(t, container.Env, "CODER_ACCESS_URL").Value; got != expectedAccessURL {
			t.Fatalf("expected default CODER_ACCESS_URL %q, got %q", expectedAccessURL, got)
		}
	})

	t.Run("UserDefinedAccessURLTakesPrecedence", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deployment-alignment-custom-access-url", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-deployment-alignment:latest",
				ExtraEnv: []corev1.EnvVar{{
					Name:  "CODER_ACCESS_URL",
					Value: "https://coder.example.com",
				}},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
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
			t.Fatalf("get deployment: %v", err)
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		if countEnvVar(container.Env, "CODER_ACCESS_URL") != 1 {
			t.Fatalf("expected exactly one CODER_ACCESS_URL env var, got %d", countEnvVar(container.Env, "CODER_ACCESS_URL"))
		}
		if got := mustFindEnvVar(t, container.Env, "CODER_ACCESS_URL").Value; got != "https://coder.example.com" {
			t.Fatalf("expected user-defined CODER_ACCESS_URL to win, got %q", got)
		}
	})

	t.Run("ResourcesAndSecurityContextsApplied", func(t *testing.T) {
		runAsUser := int64(1001)
		allowPrivilegeEscalation := false
		fsGroup := int64(2001)
		resources := &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resourceMustParse(t, "250m"),
				corev1.ResourceMemory: resourceMustParse(t, "128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resourceMustParse(t, "500m"),
				corev1.ResourceMemory: resourceMustParse(t, "256Mi"),
			},
		}
		securityContext := &corev1.SecurityContext{
			RunAsUser:                &runAsUser,
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		}
		podSecurityContext := &corev1.PodSecurityContext{FSGroup: &fsGroup}

		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deployment-alignment-security", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image:              "test-deployment-alignment:latest",
				Resources:          resources,
				SecurityContext:    securityContext,
				PodSecurityContext: podSecurityContext,
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
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
			t.Fatalf("get deployment: %v", err)
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		if !reflect.DeepEqual(container.Resources, *resources) {
			t.Fatalf("expected container resources %#v, got %#v", *resources, container.Resources)
		}
		if !reflect.DeepEqual(container.SecurityContext, securityContext) {
			t.Fatalf("expected container security context %#v, got %#v", securityContext, container.SecurityContext)
		}
		if !reflect.DeepEqual(deployment.Spec.Template.Spec.SecurityContext, podSecurityContext) {
			t.Fatalf("expected pod security context %#v, got %#v", podSecurityContext, deployment.Spec.Template.Spec.SecurityContext)
		}
	})
}

func TestReconcile_ProbeConfiguration(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	t.Run("ReadinessEnabledByDefault", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-probe-defaults", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-probes:latest",
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
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
			t.Fatalf("get deployment: %v", err)
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		if container.ReadinessProbe == nil {
			t.Fatal("expected readiness probe to be configured by default")
		}
		if container.ReadinessProbe.HTTPGet == nil {
			t.Fatal("expected readiness probe to use HTTP GET")
		}
		if container.ReadinessProbe.HTTPGet.Path != "/healthz" {
			t.Fatalf("expected readiness probe path %q, got %q", "/healthz", container.ReadinessProbe.HTTPGet.Path)
		}
		if container.ReadinessProbe.HTTPGet.Port != intstr.FromString("http") {
			t.Fatalf("expected readiness probe port name %q, got %#v", "http", container.ReadinessProbe.HTTPGet.Port)
		}
	})

	t.Run("LivenessProbeDisabled", func(t *testing.T) {
		cp := createCoderControlPlaneUnstructured(ctx, t, "test-probe-liveness-disabled", "default", map[string]any{
			"image": "test-probes:latest",
			"livenessProbe": map[string]any{
				"enabled": false,
			},
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
			t.Fatalf("reconcile control plane: %v", err)
		}

		deployment := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment); err != nil {
			t.Fatalf("get deployment: %v", err)
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		if container.LivenessProbe != nil {
			t.Fatalf("expected liveness probe to be disabled, got %#v", container.LivenessProbe)
		}
	})

	t.Run("BothProbesEnabledWithCustomTiming", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-probe-custom", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-probes:latest",
				ReadinessProbe: coderv1alpha1.ProbeSpec{
					Enabled:             true,
					InitialDelaySeconds: 3,
					PeriodSeconds:       ptrTo(int32(7)),
					TimeoutSeconds:      ptrTo(int32(2)),
					SuccessThreshold:    ptrTo(int32(2)),
					FailureThreshold:    ptrTo(int32(5)),
				},
				LivenessProbe: coderv1alpha1.ProbeSpec{
					Enabled:             true,
					InitialDelaySeconds: 11,
					PeriodSeconds:       ptrTo(int32(13)),
					TimeoutSeconds:      ptrTo(int32(4)),
					SuccessThreshold:    ptrTo(int32(1)),
					FailureThreshold:    ptrTo(int32(6)),
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
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
			t.Fatalf("get deployment: %v", err)
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		if container.ReadinessProbe == nil || container.LivenessProbe == nil {
			t.Fatalf("expected both probes to be configured, got readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
		}
		if container.ReadinessProbe.InitialDelaySeconds != 3 || container.ReadinessProbe.PeriodSeconds != 7 || container.ReadinessProbe.TimeoutSeconds != 2 || container.ReadinessProbe.SuccessThreshold != 2 || container.ReadinessProbe.FailureThreshold != 5 {
			t.Fatalf("unexpected readiness probe settings: %#v", container.ReadinessProbe)
		}
		if container.LivenessProbe.InitialDelaySeconds != 11 || container.LivenessProbe.PeriodSeconds != 13 || container.LivenessProbe.TimeoutSeconds != 4 || container.LivenessProbe.SuccessThreshold != 1 || container.LivenessProbe.FailureThreshold != 6 {
			t.Fatalf("unexpected liveness probe settings: %#v", container.LivenessProbe)
		}
	})
}

func TestReconcile_TLSAlignment(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-tls-alignment", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-tls:latest",
			TLS: coderv1alpha1.TLSSpec{
				SecretNames: []string{"my-tls"},
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create control plane: %v", err)
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
		t.Fatalf("get deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if got := mustFindEnvVar(t, container.Env, "CODER_TLS_ENABLE").Value; got != "true" {
		t.Fatalf("expected CODER_TLS_ENABLE=true, got %q", got)
	}
	if got := mustFindEnvVar(t, container.Env, "CODER_TLS_ADDRESS").Value; got != "0.0.0.0:8443" {
		t.Fatalf("expected CODER_TLS_ADDRESS=0.0.0.0:8443, got %q", got)
	}
	if got := mustFindEnvVar(t, container.Env, "CODER_TLS_CERT_FILE").Value; got != "/etc/ssl/certs/coder/my-tls/tls.crt" {
		t.Fatalf("expected CODER_TLS_CERT_FILE for my-tls secret, got %q", got)
	}
	if got := mustFindEnvVar(t, container.Env, "CODER_TLS_KEY_FILE").Value; got != "/etc/ssl/certs/coder/my-tls/tls.key" {
		t.Fatalf("expected CODER_TLS_KEY_FILE for my-tls secret, got %q", got)
	}
	if !containerHasPort(container, "https", 8443) {
		t.Fatalf("expected deployment container to expose https port 8443, got %+v", container.Ports)
	}
	if !podHasSecretVolume(deployment.Spec.Template.Spec, "tls-my-tls", "my-tls") {
		t.Fatalf("expected pod volume tls-my-tls to mount secret my-tls, got %+v", deployment.Spec.Template.Spec.Volumes)
	}
	if !containerHasVolumeMount(container, "tls-my-tls", "/etc/ssl/certs/coder/my-tls") {
		t.Fatalf("expected container volume mount tls-my-tls at /etc/ssl/certs/coder/my-tls, got %+v", container.VolumeMounts)
	}
	if got := mustFindEnvVar(t, container.Env, "CODER_ACCESS_URL").Value; got != "https://test-tls-alignment.default.svc.cluster.local" {
		t.Fatalf("expected default CODER_ACCESS_URL to use https, got %q", got)
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, service); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if !serviceHasPort(service.Spec.Ports, "https", 443) {
		t.Fatalf("expected service https port 443, got %+v", service.Spec.Ports)
	}

	reconciled := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, reconciled); err != nil {
		t.Fatalf("get reconciled control plane: %v", err)
	}
	expectedStatusURL := "https://" + cp.Name + "." + cp.Namespace + ".svc.cluster.local:443"
	if reconciled.Status.URL != expectedStatusURL {
		t.Fatalf("expected status URL %q when TLS is enabled, got %q", expectedStatusURL, reconciled.Status.URL)
	}
}

func TestReconcile_TLSWithServicePort443(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-tls-service-port-443", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-tls-443:latest",
			Service: coderv1alpha1.ServiceSpec{
				Port: 443,
			},
			TLS: coderv1alpha1.TLSSpec{
				SecretNames: []string{"my-tls-443"},
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create control plane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, service); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(service.Spec.Ports) != 1 {
		t.Fatalf("expected exactly one service port when service.port=443 and TLS is enabled, got %+v", service.Spec.Ports)
	}
	port := service.Spec.Ports[0]
	if port.Name != "https" {
		t.Fatalf("expected single service port to be named https, got %q", port.Name)
	}
	if port.Port != 443 {
		t.Fatalf("expected single service port number 443, got %d", port.Port)
	}
	if port.TargetPort != intstr.FromInt(8443) {
		t.Fatalf("expected single service port target 8443, got %+v", port.TargetPort)
	}
}

func TestReconcile_TLSAndCertSecretVolumeNameSanitization(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-tls-cert-volume-sanitization", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-tls-sanitization:latest",
			TLS: coderv1alpha1.TLSSpec{
				SecretNames: []string{"my.tls.secret"},
			},
			Certs: coderv1alpha1.CertsSpec{
				Secrets: []coderv1alpha1.CertSecretSelector{{
					Name: "extra.ca.secret",
					Key:  "ca.crt",
				}},
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create control plane: %v", err)
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
		t.Fatalf("get deployment: %v", err)
	}
	podSpec := deployment.Spec.Template.Spec
	container := podSpec.Containers[0]

	tlsVolumeName := secretVolumeName(podSpec, "my.tls.secret")
	if tlsVolumeName == "" {
		t.Fatalf("expected TLS volume for dotted secret, got %+v", podSpec.Volumes)
	}
	if !strings.HasPrefix(tlsVolumeName, "tls-my-tls-secret") {
		t.Fatalf("expected TLS volume name to start with %q, got %q", "tls-my-tls-secret", tlsVolumeName)
	}
	if !containerHasVolumeMount(container, tlsVolumeName, "/etc/ssl/certs/coder/my.tls.secret") {
		t.Fatalf("expected TLS volume mount name %q for dotted secret, got %+v", tlsVolumeName, container.VolumeMounts)
	}

	certVolumeName := secretVolumeName(podSpec, "extra.ca.secret")
	if certVolumeName == "" {
		t.Fatalf("expected cert volume for dotted secret, got %+v", podSpec.Volumes)
	}
	if !strings.HasPrefix(certVolumeName, "ca-cert-extra-ca-secret") {
		t.Fatalf("expected cert volume name to start with %q, got %q", "ca-cert-extra-ca-secret", certVolumeName)
	}
	if !containerHasVolumeMount(container, certVolumeName, "/etc/ssl/certs/extra.ca.secret.crt") {
		t.Fatalf("expected cert volume mount name %q for dotted secret, got %+v", certVolumeName, container.VolumeMounts)
	}
}

func TestReconcile_PassThroughConfiguration(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	affinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "kubernetes.io/os",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"linux"},
						}},
					},
				},
			},
		},
	}

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pass-through", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-pass-through:latest",
			Volumes: []corev1.Volume{{
				Name: "extra-volume",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "extra-volume",
				MountPath: "/var/lib/coder-extra",
			}},
			EnvFrom: []corev1.EnvFromSource{{
				ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "coder-extra-env"},
				},
			}},
			NodeSelector: map[string]string{"topology.kubernetes.io/region": "us-west"},
			Tolerations: []corev1.Toleration{{
				Key:      "dedicated",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			}},
			Affinity: affinity,
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create control plane: %v", err)
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
		t.Fatalf("get deployment: %v", err)
	}
	podSpec := deployment.Spec.Template.Spec
	container := podSpec.Containers[0]

	if !reflect.DeepEqual(container.EnvFrom, cp.Spec.EnvFrom) {
		t.Fatalf("expected container EnvFrom %#v, got %#v", cp.Spec.EnvFrom, container.EnvFrom)
	}
	if !containerHasVolumeMount(container, "extra-volume", "/var/lib/coder-extra") {
		t.Fatalf("expected container to include extra volume mount, got %+v", container.VolumeMounts)
	}
	if !podHasVolume(podSpec, "extra-volume") {
		t.Fatalf("expected pod to include extra volume, got %+v", podSpec.Volumes)
	}
	if !reflect.DeepEqual(podSpec.NodeSelector, cp.Spec.NodeSelector) {
		t.Fatalf("expected pod node selector %#v, got %#v", cp.Spec.NodeSelector, podSpec.NodeSelector)
	}
	if !reflect.DeepEqual(podSpec.Tolerations, cp.Spec.Tolerations) {
		t.Fatalf("expected pod tolerations %#v, got %#v", cp.Spec.Tolerations, podSpec.Tolerations)
	}
	if !reflect.DeepEqual(podSpec.Affinity, cp.Spec.Affinity) {
		t.Fatalf("expected pod affinity %#v, got %#v", cp.Spec.Affinity, podSpec.Affinity)
	}
}

func TestReconcile_IngressExposure(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	t.Run("IngressCreated", func(t *testing.T) {
		className := "nginx"
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-ingress-created", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-ingress:latest",
				Expose: &coderv1alpha1.ExposeSpec{
					Ingress: &coderv1alpha1.IngressExposeSpec{
						Host:      "coder.example.test",
						ClassName: &className,
						Annotations: map[string]string{
							"nginx.ingress.kubernetes.io/proxy-read-timeout": "300",
						},
					},
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, cp)
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
			t.Fatalf("reconcile control plane: %v", err)
		}

		ingress := &networkingv1.Ingress{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, ingress); err != nil {
			t.Fatalf("get ingress: %v", err)
		}
		if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != className {
			t.Fatalf("expected ingress className %q, got %#v", className, ingress.Spec.IngressClassName)
		}
		if ingress.Annotations["nginx.ingress.kubernetes.io/proxy-read-timeout"] != "300" {
			t.Fatalf("expected ingress annotation to be preserved, got %q", ingress.Annotations["nginx.ingress.kubernetes.io/proxy-read-timeout"])
		}
		if len(ingress.Spec.Rules) != 1 {
			t.Fatalf("expected one ingress rule, got %d", len(ingress.Spec.Rules))
		}
		rule := ingress.Spec.Rules[0]
		if rule.Host != "coder.example.test" {
			t.Fatalf("expected ingress host %q, got %q", "coder.example.test", rule.Host)
		}
		if rule.HTTP == nil || len(rule.HTTP.Paths) != 1 {
			t.Fatalf("expected one ingress HTTP path, got %#v", rule.HTTP)
		}
		path := rule.HTTP.Paths[0]
		if path.Path != "/" {
			t.Fatalf("expected ingress path %q, got %q", "/", path.Path)
		}
		if path.Backend.Service == nil {
			t.Fatal("expected ingress backend service to be configured")
		}
		if path.Backend.Service.Name != cp.Name {
			t.Fatalf("expected ingress backend service name %q, got %q", cp.Name, path.Backend.Service.Name)
		}
		if path.Backend.Service.Port.Number != 80 {
			t.Fatalf("expected ingress backend service port 80, got %d", path.Backend.Service.Port.Number)
		}
	})

	t.Run("IngressTLSAndWildcardHost", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-ingress-tls-wildcard", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-ingress:latest",
				Expose: &coderv1alpha1.ExposeSpec{
					Ingress: &coderv1alpha1.IngressExposeSpec{
						Host:         "coder.example.test",
						WildcardHost: "*.apps.example.test",
						TLS: &coderv1alpha1.IngressTLSExposeSpec{
							SecretName:         "coder-tls",
							WildcardSecretName: "coder-wildcard-tls",
						},
					},
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, cp)
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}); err != nil {
			t.Fatalf("reconcile control plane: %v", err)
		}

		ingress := &networkingv1.Ingress{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, ingress); err != nil {
			t.Fatalf("get ingress: %v", err)
		}
		if len(ingress.Spec.Rules) != 2 {
			t.Fatalf("expected two ingress rules for primary and wildcard hosts, got %d", len(ingress.Spec.Rules))
		}
		if !ingressHasHost(ingress.Spec.Rules, "coder.example.test") {
			t.Fatal("expected ingress rules to include primary host")
		}
		if !ingressHasHost(ingress.Spec.Rules, "*.apps.example.test") {
			t.Fatal("expected ingress rules to include wildcard host")
		}
		if len(ingress.Spec.TLS) != 2 {
			t.Fatalf("expected two ingress TLS entries, got %d", len(ingress.Spec.TLS))
		}
		if !ingressTLSContainsSecretAndHost(ingress.Spec.TLS, "coder-tls", "coder.example.test") {
			t.Fatal("expected ingress TLS to include primary host secret")
		}
		if !ingressTLSContainsSecretAndHost(ingress.Spec.TLS, "coder-wildcard-tls", "*.apps.example.test") {
			t.Fatal("expected ingress TLS to include wildcard host secret")
		}
	})

	t.Run("IngressCleanupOnRemoval", func(t *testing.T) {
		cp := &coderv1alpha1.CoderControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "test-ingress-cleanup", Namespace: "default"},
			Spec: coderv1alpha1.CoderControlPlaneSpec{
				Image: "test-ingress:latest",
				Expose: &coderv1alpha1.ExposeSpec{
					Ingress: &coderv1alpha1.IngressExposeSpec{
						Host: "cleanup.example.test",
					},
				},
			},
		}
		if err := k8sClient.Create(ctx, cp); err != nil {
			t.Fatalf("create control plane: %v", err)
		}
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, cp)
		})

		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("first reconcile control plane: %v", err)
		}
		ingress := &networkingv1.Ingress{}
		if err := k8sClient.Get(ctx, namespacedName, ingress); err != nil {
			t.Fatalf("expected ingress to exist before cleanup, got: %v", err)
		}

		latest := &coderv1alpha1.CoderControlPlane{}
		if err := k8sClient.Get(ctx, namespacedName, latest); err != nil {
			t.Fatalf("get latest control plane: %v", err)
		}
		latest.Spec.Expose = nil
		if err := k8sClient.Update(ctx, latest); err != nil {
			t.Fatalf("update control plane to remove exposure: %v", err)
		}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName}); err != nil {
			t.Fatalf("second reconcile control plane: %v", err)
		}
		err := k8sClient.Get(ctx, namespacedName, ingress)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected ingress to be deleted after exposure removal, got: %v", err)
		}
	})
}

func TestReconcile_HTTPRouteExposure(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	ensureHTTPRouteCRDInstalled(t)

	gatewayNamespace := "default"
	sectionName := "https"
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-httproute-created", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-httproute:latest",
			Expose: &coderv1alpha1.ExposeSpec{
				Gateway: &coderv1alpha1.GatewayExposeSpec{
					Host:         "coder.gateway.example.test",
					WildcardHost: "*.apps.gateway.example.test",
					ParentRefs: []coderv1alpha1.GatewayParentRef{
						{
							Name:        "coder-gateway",
							Namespace:   &gatewayNamespace,
							SectionName: &sectionName,
						},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create control plane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName})
	if err != nil {
		t.Fatalf("reconcile control plane: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected gateway exposure to request periodic drift requeue, got %+v", result)
	}

	httpRoute := &gatewayv1.HTTPRoute{}
	if err := k8sClient.Get(ctx, namespacedName, httpRoute); err != nil {
		t.Fatalf("get httproute: %v", err)
	}
	if len(httpRoute.Spec.Hostnames) != 2 {
		t.Fatalf("expected two hostnames on httproute, got %d", len(httpRoute.Spec.Hostnames))
	}
	if !httpRouteHasHostname(httpRoute.Spec.Hostnames, "coder.gateway.example.test") {
		t.Fatal("expected httproute to include primary host")
	}
	if !httpRouteHasHostname(httpRoute.Spec.Hostnames, "*.apps.gateway.example.test") {
		t.Fatal("expected httproute to include wildcard host")
	}
	if len(httpRoute.Spec.ParentRefs) != 1 {
		t.Fatalf("expected one parentRef, got %d", len(httpRoute.Spec.ParentRefs))
	}
	parentRef := httpRoute.Spec.ParentRefs[0]
	if string(parentRef.Name) != "coder-gateway" {
		t.Fatalf("expected parentRef name %q, got %q", "coder-gateway", parentRef.Name)
	}
	if parentRef.Namespace == nil || string(*parentRef.Namespace) != gatewayNamespace {
		t.Fatalf("expected parentRef namespace %q, got %#v", gatewayNamespace, parentRef.Namespace)
	}
	if parentRef.SectionName == nil || string(*parentRef.SectionName) != sectionName {
		t.Fatalf("expected parentRef sectionName %q, got %#v", sectionName, parentRef.SectionName)
	}
	if len(httpRoute.Spec.Rules) != 1 {
		t.Fatalf("expected one httproute rule, got %d", len(httpRoute.Spec.Rules))
	}
	rule := httpRoute.Spec.Rules[0]
	if len(rule.Matches) != 1 || rule.Matches[0].Path == nil || rule.Matches[0].Path.Value == nil || *rule.Matches[0].Path.Value != "/" {
		t.Fatalf("expected httproute prefix path match on '/', got %#v", rule.Matches)
	}
	if len(rule.BackendRefs) != 1 {
		t.Fatalf("expected one backendRef, got %d", len(rule.BackendRefs))
	}
	backendRef := rule.BackendRefs[0].BackendObjectReference
	if string(backendRef.Name) != cp.Name {
		t.Fatalf("expected backend service name %q, got %q", cp.Name, backendRef.Name)
	}
	if backendRef.Kind == nil || string(*backendRef.Kind) != "Service" {
		t.Fatalf("expected backend kind Service, got %#v", backendRef.Kind)
	}
	if backendRef.Group == nil || string(*backendRef.Group) != "" {
		t.Fatalf("expected backend group to be empty for core Service, got %#v", backendRef.Group)
	}
	if backendRef.Port == nil || int32(*backendRef.Port) != 80 {
		t.Fatalf("expected backend port 80, got %#v", backendRef.Port)
	}
}

func TestReconcile_HTTPRouteExposure_CRDMissingIsGraceful(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("register gateway API types in scheme: %v", err)
	}

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-httproute-crd-missing", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-httproute:latest",
			Expose: &coderv1alpha1.ExposeSpec{
				Gateway: &coderv1alpha1.GatewayExposeSpec{
					Host: "missing-crd.gateway.example.test",
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("create control plane: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cp)
	})

	clientWithNoMatch := &httpRouteNoMatchClient{Client: k8sClient}
	r := &controller.CoderControlPlaneReconciler{Client: clientWithNoMatch, Scheme: scheme}
	namespacedName := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName})
	if err != nil {
		t.Fatalf("expected reconcile to gracefully ignore missing Gateway CRDs, got error: %v", err)
	}
	if result.RequeueAfter > 0 {
		t.Fatalf("expected missing Gateway CRDs to avoid requeue, got %+v", result)
	}
	if clientWithNoMatch.HTTPRouteGetCalls() == 0 {
		t.Fatal("expected gateway exposure reconciliation to attempt HTTPRoute get")
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, namespacedName, deployment); err != nil {
		t.Fatalf("expected deployment reconciliation to continue when gateway CRDs are missing: %v", err)
	}
}

func createCoderControlPlaneUnstructured(ctx context.Context, t *testing.T, name, namespace string, spec map[string]any) *coderv1alpha1.CoderControlPlane {
	t.Helper()

	if strings.TrimSpace(name) == "" {
		t.Fatal("assertion failed: control plane name must not be empty")
	}
	if strings.TrimSpace(namespace) == "" {
		t.Fatal("assertion failed: control plane namespace must not be empty")
	}

	controlPlane := &unstructured.Unstructured{}
	controlPlane.SetAPIVersion(coderv1alpha1.GroupVersion.String())
	controlPlane.SetKind("CoderControlPlane")
	controlPlane.SetName(name)
	controlPlane.SetNamespace(namespace)
	controlPlane.Object["spec"] = spec

	if err := k8sClient.Create(ctx, controlPlane); err != nil {
		t.Fatalf("create unstructured control plane %s/%s: %v", namespace, name, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), controlPlane)
	})

	typed := &coderv1alpha1.CoderControlPlane{}
	namespacedName := types.NamespacedName{Name: name, Namespace: namespace}
	if err := k8sClient.Get(ctx, namespacedName, typed); err != nil {
		t.Fatalf("get typed control plane %s: %v", namespacedName, err)
	}
	if typed.Name != name || typed.Namespace != namespace {
		t.Fatalf("assertion failed: fetched control plane %s/%s does not match expected %s/%s", typed.Namespace, typed.Name, namespace, name)
	}

	return typed
}

func roleContainsRuleForResource(rules []rbacv1.PolicyRule, apiGroup, resource string) bool {
	for _, rule := range rules {
		if !sliceContainsString(rule.APIGroups, apiGroup) {
			continue
		}
		if !sliceContainsString(rule.Resources, resource) {
			continue
		}
		return true
	}
	return false
}

func mustFindEnvVar(t *testing.T, envVars []corev1.EnvVar, name string) corev1.EnvVar {
	t.Helper()

	for _, envVar := range envVars {
		if envVar.Name == name {
			return envVar
		}
	}
	t.Fatalf("expected environment variable %q to exist, got %v", name, envVars)
	return corev1.EnvVar{}
}

func countEnvVar(envVars []corev1.EnvVar, name string) int {
	count := 0
	for _, envVar := range envVars {
		if envVar.Name == name {
			count++
		}
	}
	return count
}

func containerHasPort(container corev1.Container, name string, port int32) bool {
	for _, containerPort := range container.Ports {
		if containerPort.Name == name && containerPort.ContainerPort == port {
			return true
		}
	}
	return false
}

func podHasSecretVolume(podSpec corev1.PodSpec, volumeName, secretName string) bool {
	for _, volume := range podSpec.Volumes {
		if volume.Name != volumeName {
			continue
		}
		if volume.Secret != nil && volume.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}

func secretVolumeName(podSpec corev1.PodSpec, secretName string) string {
	for _, volume := range podSpec.Volumes {
		if volume.Secret == nil {
			continue
		}
		if volume.Secret.SecretName == secretName {
			return volume.Name
		}
	}
	return ""
}

func podHasVolume(podSpec corev1.PodSpec, volumeName string) bool {
	for _, volume := range podSpec.Volumes {
		if volume.Name == volumeName {
			return true
		}
	}
	return false
}

func containerHasVolumeMount(container corev1.Container, mountName, mountPath string) bool {
	for _, volumeMount := range container.VolumeMounts {
		if volumeMount.Name == mountName && volumeMount.MountPath == mountPath {
			return true
		}
	}
	return false
}

func serviceHasPort(servicePorts []corev1.ServicePort, name string, port int32) bool {
	for _, servicePort := range servicePorts {
		if servicePort.Name == name && servicePort.Port == port {
			return true
		}
	}
	return false
}

func ingressHasHost(rules []networkingv1.IngressRule, host string) bool {
	for _, rule := range rules {
		if rule.Host == host {
			return true
		}
	}
	return false
}

func ingressTLSContainsSecretAndHost(entries []networkingv1.IngressTLS, secretName, host string) bool {
	for _, entry := range entries {
		if entry.SecretName != secretName {
			continue
		}
		if sliceContainsString(entry.Hosts, host) {
			return true
		}
	}
	return false
}

func httpRouteHasHostname(hostnames []gatewayv1.Hostname, hostname string) bool {
	for _, item := range hostnames {
		if string(item) == hostname {
			return true
		}
	}
	return false
}

func sliceContainsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func resourceMustParse(t *testing.T, quantity string) resource.Quantity {
	t.Helper()

	parsedQuantity, err := resource.ParseQuantity(quantity)
	if err != nil {
		t.Fatalf("parse resource quantity %q: %v", quantity, err)
	}
	return parsedQuantity
}

var (
	ensureGatewaySchemeOnce sync.Once
	ensureGatewaySchemeErr  error
)

func ensureGatewaySchemeRegistered(t *testing.T) {
	t.Helper()

	ensureGatewaySchemeOnce.Do(func() {
		if scheme == nil {
			ensureGatewaySchemeErr = errors.New("assertion failed: test scheme must not be nil")
			return
		}
		ensureGatewaySchemeErr = gatewayv1.Install(scheme)
	})
	if ensureGatewaySchemeErr != nil {
		t.Fatalf("register gateway API types in test scheme: %v", ensureGatewaySchemeErr)
	}
}

var ensureHTTPRouteCRDOnce sync.Once

func ensureHTTPRouteCRDInstalled(t *testing.T) {
	t.Helper()

	var installErr error
	ensureHTTPRouteCRDOnce.Do(func() {
		if err := gatewayv1.Install(scheme); err != nil {
			installErr = err
			return
		}

		apiextensionsClient, err := apiextensionsclientset.NewForConfig(cfg)
		if err != nil {
			installErr = err
			return
		}

		const httpRouteCRDName = "httproutes.gateway.networking.k8s.io"
		_, err = apiextensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), httpRouteCRDName, metav1.GetOptions{})
		if err == nil {
			return
		}
		if !apierrors.IsNotFound(err) {
			installErr = err
			return
		}

		httpRouteCRD := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: httpRouteCRDName,
				Annotations: map[string]string{
					"api-approved.kubernetes.io": "https://github.com/kubernetes-sigs/gateway-api",
				},
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: gatewayv1.GroupVersion.Group,
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural:   "httproutes",
					Singular: "httproute",
					Kind:     "HTTPRoute",
					ListKind: "HTTPRouteList",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name:    gatewayv1.GroupVersion.Version,
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type:                   "object",
									XPreserveUnknownFields: ptrTo(true),
								},
								"status": {
									Type:                   "object",
									XPreserveUnknownFields: ptrTo(true),
								},
							},
						},
					},
				}},
			},
		}
		if _, err := apiextensionsClient.ApiextensionsV1().CustomResourceDefinitions().Create(context.Background(), httpRouteCRD, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			installErr = err
			return
		}

		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			storedCRD, getErr := apiextensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), httpRouteCRDName, metav1.GetOptions{})
			if getErr != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if customResourceDefinitionEstablished(storedCRD) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}

		installErr = errors.New("timed out waiting for HTTPRoute CRD establishment")
	})
	if installErr != nil {
		t.Fatalf("install HTTPRoute CRD for test: %v", installErr)
	}
}

func customResourceDefinitionEstablished(customResourceDefinition *apiextensionsv1.CustomResourceDefinition) bool {
	if customResourceDefinition == nil {
		return false
	}
	for _, condition := range customResourceDefinition.Status.Conditions {
		if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}
	return false
}

type httpRouteNoMatchClient struct {
	ctrlclient.Client
	mu                sync.Mutex
	httpRouteGetCalls int
}

func (c *httpRouteNoMatchClient) Get(ctx context.Context, key types.NamespacedName, object ctrlclient.Object, opts ...ctrlclient.GetOption) error {
	if _, ok := object.(*gatewayv1.HTTPRoute); ok {
		c.mu.Lock()
		c.httpRouteGetCalls++
		c.mu.Unlock()
		return &apimeta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{
			Group:    gatewayv1.GroupVersion.Group,
			Version:  gatewayv1.GroupVersion.Version,
			Resource: "httproutes",
		}}
	}
	return c.Client.Get(ctx, key, object, opts...)
}

func (c *httpRouteNoMatchClient) HTTPRouteGetCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.httpRouteGetCalls
}

func ptrTo[T any](value T) *T {
	return &value
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
	ensureGatewaySchemeRegistered(t)
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
	ensureGatewaySchemeRegistered(t)
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
