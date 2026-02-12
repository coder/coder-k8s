package controller_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/coder/v2/codersdk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

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

func TestReconcile_LicenseSecretRefNil_DoesNotUpload(t *testing.T) {
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
