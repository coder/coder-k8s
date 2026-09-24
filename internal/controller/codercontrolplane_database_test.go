package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

const (
	// #nosec G101 -- fake test fixture used to detect leaks, not a real credential.
	testDatabasePassword = "s3cr3t-db-password"
	testDatabaseURL      = "postgresql://coder:" + testDatabasePassword + "@coder-db-rw.default.svc:5432/coder?sslmode=disable"
)

func newDatabaseControlPlane(name, namespace, secretName, secretKey string) *coderv1alpha1.CoderControlPlane {
	return &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "test-database-secret-ref:latest",
			Database: &coderv1alpha1.DatabaseSpec{
				ConnectionSecretRef: coderv1alpha1.SecretKeySelector{Name: secretName, Key: secretKey},
			},
		},
	}
}

func createTestObject(t *testing.T, obj ctrlclient.Object) {
	t.Helper()

	ctx := context.Background()
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %s/%s: %v", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, obj)
	})
}

func newDatabaseSecret(name, namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func reconcileControlPlane(t *testing.T, r *controller.CoderControlPlaneReconciler, cp *coderv1alpha1.CoderControlPlane) ctrl.Result {
	t.Helper()

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	if err != nil {
		assertNoDatabaseSecretLeak(t, "reconcile error", err.Error())
		t.Fatalf("reconcile control plane %s/%s: %v", cp.Namespace, cp.Name, err)
	}
	return result
}

func getControlPlane(t *testing.T, cp *coderv1alpha1.CoderControlPlane) *coderv1alpha1.CoderControlPlane {
	t.Helper()

	latest := &coderv1alpha1.CoderControlPlane{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, latest); err != nil {
		t.Fatalf("get control plane %s/%s: %v", cp.Namespace, cp.Name, err)
	}
	return latest
}

func getControlPlaneDeployment(t *testing.T, cp *coderv1alpha1.CoderControlPlane) *appsv1.Deployment {
	t.Helper()

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, deployment); err != nil {
		t.Fatalf("get deployment %s/%s: %v", cp.Namespace, cp.Name, err)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("assertion failed: expected one container, got %d", len(deployment.Spec.Template.Spec.Containers))
	}
	return deployment
}

// assertDeploymentReferencesDatabaseSecret checks that CODER_PG_CONNECTION_URL
// is injected exactly once as a Secret reference, never as a literal value.
func assertDeploymentReferencesDatabaseSecret(t *testing.T, deployment *appsv1.Deployment, secretName, secretKey string) {
	t.Helper()

	var matches []corev1.EnvVar
	for _, envVar := range deployment.Spec.Template.Spec.Containers[0].Env {
		if envVar.Name == "CODER_PG_CONNECTION_URL" {
			matches = append(matches, envVar)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one CODER_PG_CONNECTION_URL env var, got %d", len(matches))
	}
	envVar := matches[0]
	if envVar.Value != "" {
		t.Fatalf("expected CODER_PG_CONNECTION_URL to have no literal value")
	}
	if envVar.ValueFrom == nil || envVar.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("expected CODER_PG_CONNECTION_URL to use valueFrom.secretKeyRef, got %+v", envVar.ValueFrom)
	}
	if got := envVar.ValueFrom.SecretKeyRef.Name; got != secretName {
		t.Fatalf("expected secretKeyRef name %q, got %q", secretName, got)
	}
	if got := envVar.ValueFrom.SecretKeyRef.Key; got != secretKey {
		t.Fatalf("expected secretKeyRef key %q, got %q", secretKey, got)
	}
	if envVar.ValueFrom.SecretKeyRef.Optional != nil {
		t.Fatalf("expected secretKeyRef to be required, got optional=%v", *envVar.ValueFrom.SecretKeyRef.Optional)
	}

	deploymentJSON, err := json.Marshal(deployment.Spec)
	if err != nil {
		t.Fatalf("marshal deployment spec: %v", err)
	}
	assertNoDatabaseSecretLeak(t, "deployment spec", string(deploymentJSON))
}

func requireDatabaseCondition(
	t *testing.T,
	cp *coderv1alpha1.CoderControlPlane,
	status metav1.ConditionStatus,
	reason string,
) metav1.Condition {
	t.Helper()

	condition := apimeta.FindStatusCondition(cp.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved)
	if condition == nil {
		t.Fatalf("expected %s condition, got conditions %+v", coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved, cp.Status.Conditions)
	}
	if condition.Status != status || condition.Reason != reason {
		t.Fatalf("expected %s condition status=%s reason=%s, got status=%s reason=%s message=%q",
			coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved, status, reason, condition.Status, condition.Reason, condition.Message)
	}
	if condition.ObservedGeneration != cp.Generation {
		t.Fatalf("expected condition observedGeneration %d, got %d", cp.Generation, condition.ObservedGeneration)
	}
	if strings.Contains(strings.ToLower(condition.Message), "healthy") {
		t.Fatalf("condition message must not claim database health: %q", condition.Message)
	}
	return *condition
}

// assertNoDatabaseSecretLeak fails when text contains the test password, the
// database host, or the scheme of the invalid test values.
func assertNoDatabaseSecretLeak(t *testing.T, what, text string) {
	t.Helper()

	for _, secretValue := range []string{testDatabasePassword, "coder-db-rw.default.svc", "mysql://"} {
		if strings.Contains(text, secretValue) {
			t.Fatalf("%s leaks secret material %q: %q", what, secretValue, text)
		}
	}
}

func assertStatusAndEventsRedacted(t *testing.T, cp *coderv1alpha1.CoderControlPlane) {
	t.Helper()

	statusJSON, err := json.Marshal(cp.Status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	assertNoDatabaseSecretLeak(t, "status", string(statusJSON))

	var events corev1.EventList
	if err := k8sClient.List(context.Background(), &events, ctrlclient.InNamespace(cp.Namespace)); err != nil {
		t.Fatalf("list events: %v", err)
	}
	for i := range events.Items {
		assertNoDatabaseSecretLeak(t, "event", events.Items[i].Message)
	}
}

func TestReconcile_DatabaseSecretRef_WiresDeploymentAndBootstrap(t *testing.T) {
	ensureGatewaySchemeRegistered(t)

	secret := newDatabaseSecret("test-db-ref-wiring-app", "default", map[string][]byte{"uri": []byte(testDatabaseURL)})
	createTestObject(t, secret)
	cp := newDatabaseControlPlane("test-db-ref-wiring", "default", secret.Name, "uri")
	createTestObject(t, cp)

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-db-ref"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	result := reconcileControlPlane(t, r, cp)
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty reconcile result, got %+v", result)
	}

	assertDeploymentReferencesDatabaseSecret(t, getControlPlaneDeployment(t, cp), secret.Name, "uri")

	if provisioner.calls != 1 {
		t.Fatalf("expected bootstrap to run once, got %d calls", provisioner.calls)
	}
	if provisioner.requests[0].PostgresURL != testDatabaseURL {
		t.Fatalf("expected bootstrap to use the URL from the referenced Secret")
	}

	reconciled := getControlPlane(t, cp)
	condition := requireDatabaseCondition(t, reconciled, metav1.ConditionTrue, "Resolved")
	if !strings.Contains(condition.Message, "does not check that the database is reachable") {
		t.Fatalf("expected resolved message to disclaim reachability, got %q", condition.Message)
	}
	if !reconciled.Status.OperatorAccessReady {
		t.Fatalf("expected operator access ready=true")
	}
	assertStatusAndEventsRedacted(t, reconciled)
}

func TestReconcile_DatabaseSecretRef_UnresolvedReasons(t *testing.T) {
	ensureGatewaySchemeRegistered(t)

	tests := []struct {
		name       string
		secretData map[string][]byte // nil means the Secret does not exist.
		wantReason string
	}{
		{name: "missing-secret", wantReason: "SecretNotFound"},
		{name: "missing-key", secretData: map[string][]byte{"password": []byte(testDatabasePassword)}, wantReason: "KeyNotFound"},
		{name: "empty-value", secretData: map[string][]byte{"uri": {}}, wantReason: "EmptyValue"},
		{name: "whitespace-value", secretData: map[string][]byte{"uri": []byte("  \n")}, wantReason: "EmptyValue"},
		{name: "wrong-scheme", secretData: map[string][]byte{"uri": []byte("mysql://coder:" + testDatabasePassword + "@coder-db-rw.default.svc/coder")}, wantReason: "InvalidURL"},
		// url.Parse errors echo their input; the condition must not.
		{name: "parse-error", secretData: map[string][]byte{"uri": []byte("postgres://coder:" + testDatabasePassword + "@coder-db-rw.default.svc:port/coder")}, wantReason: "InvalidURL"},
		{name: "trailing-newline", secretData: map[string][]byte{"uri": []byte(testDatabaseURL + "\n")}, wantReason: "InvalidURL"},
		{name: "not-a-url", secretData: map[string][]byte{"uri": []byte("host=coder-db-rw.default.svc password=" + testDatabasePassword)}, wantReason: "InvalidURL"},
		// lib/pq only recognizes the exact lowercase prefixes as URLs.
		{name: "uppercase-scheme", secretData: map[string][]byte{"uri": []byte("POSTGRES://coder:" + testDatabasePassword + "@coder-db-rw.default.svc/coder")}, wantReason: "InvalidURL"},
		{name: "mixed-case-scheme", secretData: map[string][]byte{"uri": []byte("PostgreSQL://coder:" + testDatabasePassword + "@coder-db-rw.default.svc/coder")}, wantReason: "InvalidURL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretName := "test-db-ref-" + tt.name + "-app"
			if tt.secretData != nil {
				createTestObject(t, newDatabaseSecret(secretName, "default", tt.secretData))
			}
			cp := newDatabaseControlPlane("test-db-ref-"+tt.name, "default", secretName, "uri")
			createTestObject(t, cp)

			provisioner := &fakeOperatorAccessProvisioner{token: "should-not-be-used"}
			r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

			result := reconcileControlPlane(t, r, cp)
			if result.RequeueAfter <= 0 {
				t.Fatalf("expected bootstrap to wait with a delayed requeue, got %+v", result)
			}
			if provisioner.calls != 0 {
				t.Fatalf("expected bootstrap not to run while the database secret is unresolved, got %d calls", provisioner.calls)
			}

			// The Deployment keeps referencing the Secret so kubelet can start
			// pods as soon as it becomes valid.
			assertDeploymentReferencesDatabaseSecret(t, getControlPlaneDeployment(t, cp), secretName, "uri")

			reconciled := getControlPlane(t, cp)
			condition := requireDatabaseCondition(t, reconciled, metav1.ConditionFalse, tt.wantReason)
			if !strings.Contains(condition.Message, secretName) {
				t.Fatalf("expected condition message to name Secret %q, got %q", secretName, condition.Message)
			}
			if reconciled.Status.OperatorAccessReady {
				t.Fatalf("expected operator access ready=false")
			}
			assertStatusAndEventsRedacted(t, reconciled)
		})
	}
}

func TestReconcile_DatabaseSecretRef_ConditionRemovedWhenUnset(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := newDatabaseControlPlane("test-db-ref-unset", "default", "test-db-ref-unset-app", "uri")
	createTestObject(t, cp)

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	reconcileControlPlane(t, r, cp)
	requireDatabaseCondition(t, getControlPlane(t, cp), metav1.ConditionFalse, "SecretNotFound")

	latest := getControlPlane(t, cp)
	latest.Spec.Database = nil
	if err := k8sClient.Update(ctx, latest); err != nil {
		t.Fatalf("clear spec.database: %v", err)
	}
	reconcileControlPlane(t, r, cp)

	reconciled := getControlPlane(t, cp)
	if condition := apimeta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved); condition != nil {
		t.Fatalf("expected %s condition to be removed, got %+v", coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved, *condition)
	}
	for _, envVar := range getControlPlaneDeployment(t, cp).Spec.Template.Spec.Containers[0].Env {
		if envVar.Name == "CODER_PG_CONNECTION_URL" {
			t.Fatalf("expected CODER_PG_CONNECTION_URL to be removed from the Deployment, got %+v", envVar)
		}
	}
}

func TestReconcile_DatabaseSecretRef_LegacyExtraEnvUnchanged(t *testing.T) {
	ensureGatewaySchemeRegistered(t)

	extraEnv := []corev1.EnvVar{
		{
			Name: "CODER_PG_CONNECTION_URL",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "test-db-legacy-app"},
				Key:                  "uri",
			}},
		},
		{Name: "CODER_ACCESS_URL", Value: "http://localhost:3000"},
	}
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-db-legacy-extra-env", Namespace: "default"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "test-db-legacy:latest",
			ExtraEnv: extraEnv,
		},
	}
	createTestObject(t, cp)

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	reconcileControlPlane(t, r, cp)

	wantEnv := append([]corev1.EnvVar{
		{Name: "KUBE_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
		{Name: "CODER_DERP_SERVER_RELAY_URL", Value: "http://$(KUBE_POD_IP):8080"},
	}, extraEnv...)
	gotEnv := getControlPlaneDeployment(t, cp).Spec.Template.Spec.Containers[0].Env
	if !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Fatalf("expected legacy extraEnv to pass through unchanged\nwant: %+v\ngot:  %+v", wantEnv, gotEnv)
	}
	if condition := apimeta.FindStatusCondition(getControlPlane(t, cp).Status.Conditions, coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved); condition != nil {
		t.Fatalf("expected no %s condition without spec.database, got %+v", coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved, *condition)
	}
}

func TestDatabaseSecretRef_AdmissionRejectsConflictAndEmptyFields(t *testing.T) {
	ctx := context.Background()
	conflictEnv := []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: testDatabaseURL}}

	tests := []struct {
		name        string
		mutate      func(cp *coderv1alpha1.CoderControlPlane)
		wantMessage string
	}{
		{
			name:        "extra-env-conflict",
			mutate:      func(cp *coderv1alpha1.CoderControlPlane) { cp.Spec.ExtraEnv = conflictEnv },
			wantMessage: "spec.extraEnv must not set CODER_PG_CONNECTION_URL when spec.database.connectionSecretRef is set",
		},
		{
			name:        "empty-name",
			mutate:      func(cp *coderv1alpha1.CoderControlPlane) { cp.Spec.Database.ConnectionSecretRef.Name = "" },
			wantMessage: "connectionSecretRef.name must not be empty",
		},
		{
			name:        "missing-key",
			mutate:      func(cp *coderv1alpha1.CoderControlPlane) { cp.Spec.Database.ConnectionSecretRef.Key = "" },
			wantMessage: "connectionSecretRef.key must not be empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := newDatabaseControlPlane("test-db-admission-"+tt.name, "default", "test-db-admission-app", "uri")
			tt.mutate(cp)
			err := k8sClient.Create(ctx, cp)
			if err == nil {
				_ = k8sClient.Delete(ctx, cp)
				t.Fatalf("expected admission to reject %s", tt.name)
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("expected Invalid error containing %q, got %v", tt.wantMessage, err)
			}
			assertNoDatabaseSecretLeak(t, "admission error", err.Error())
		})
	}

	// Adding the conflicting env var to an existing object is rejected too.
	cp := newDatabaseControlPlane("test-db-admission-update", "default", "test-db-admission-app", "uri")
	createTestObject(t, cp)
	latest := getControlPlane(t, cp)
	latest.Spec.ExtraEnv = conflictEnv
	err := k8sClient.Update(ctx, latest)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tests[0].wantMessage) {
		t.Fatalf("expected update with conflicting extraEnv to be rejected, got %v", err)
	}
	assertNoDatabaseSecretLeak(t, "admission error", err.Error())
}

// conflictInjectingClient simulates an object that bypassed admission by
// adding CODER_PG_CONNECTION_URL to extraEnv on every CoderControlPlane read.
type conflictInjectingClient struct {
	ctrlclient.Client
}

func (c *conflictInjectingClient) Get(ctx context.Context, key types.NamespacedName, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if cp, ok := obj.(*coderv1alpha1.CoderControlPlane); ok {
		cp.Spec.ExtraEnv = append(cp.Spec.ExtraEnv, corev1.EnvVar{Name: "CODER_PG_CONNECTION_URL", Value: testDatabaseURL})
	}
	return nil
}

func TestReconcile_DatabaseSecretRef_ControllerRejectsConflict(t *testing.T) {
	ensureGatewaySchemeRegistered(t)

	secret := newDatabaseSecret("test-db-ref-conflict-app", "default", map[string][]byte{"uri": []byte(testDatabaseURL)})
	createTestObject(t, secret)
	cp := newDatabaseControlPlane("test-db-ref-conflict", "default", secret.Name, "uri")
	// Pre-set the finalizer: adding it would patch the object and replace the
	// injected extraEnv with the stored spec.
	cp.Finalizers = []string{"coder.com/workspace-rbac-cleanup"}
	createTestObject(t, cp)

	provisioner := &fakeOperatorAccessProvisioner{token: "should-not-be-used"}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    &conflictInjectingClient{Client: k8sClient},
		Scheme:                    scheme,
		OperatorAccessProvisioner: provisioner,
	}
	result := reconcileControlPlane(t, r, cp)
	if result != (ctrl.Result{}) {
		t.Fatalf("expected no requeue for conflicting configuration, got %+v", result)
	}
	if provisioner.calls != 0 {
		t.Fatalf("expected bootstrap not to run for conflicting configuration, got %d calls", provisioner.calls)
	}

	err := k8sClient.Get(context.Background(), types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no Deployment for conflicting configuration, got err=%v", err)
	}

	reconciled := getControlPlane(t, cp)
	condition := requireDatabaseCondition(t, reconciled, metav1.ConditionFalse, "ConflictingConfiguration")
	if !strings.Contains(condition.Message, "spec.extraEnv") || !strings.Contains(condition.Message, "spec.database.connectionSecretRef") {
		t.Fatalf("expected conflict message to name both fields, got %q", condition.Message)
	}
	assertStatusAndEventsRedacted(t, reconciled)
}

func TestReconcile_DatabaseSecretRef_ConflictKeepsObservedGeneration(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	secret := newDatabaseSecret("test-db-ref-conflict-gen-app", "default", map[string][]byte{"uri": []byte(testDatabaseURL)})
	createTestObject(t, secret)
	cp := newDatabaseControlPlane("test-db-ref-conflict-gen", "default", secret.Name, "uri")
	createTestObject(t, cp)

	// Generation 1 reconciles successfully.
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	reconcileControlPlane(t, r, cp)
	healthy := getControlPlane(t, cp)
	if healthy.Generation != 1 || healthy.Status.ObservedGeneration != 1 {
		t.Fatalf("assertion failed: expected generation 1 observed, got generation=%d observedGeneration=%d",
			healthy.Generation, healthy.Status.ObservedGeneration)
	}
	requireDatabaseCondition(t, healthy, metav1.ConditionTrue, "Resolved")
	deploymentBefore := getControlPlaneDeployment(t, cp)

	// Generation 2 changes the spec, and every read also carries a conflicting
	// CODER_PG_CONNECTION_URL, as if the object bypassed admission.
	healthy.Spec.Image = "test-database-secret-ref:v2"
	if err := k8sClient.Update(ctx, healthy); err != nil {
		t.Fatalf("update control plane spec: %v", err)
	}
	conflicting := &controller.CoderControlPlaneReconciler{Client: &conflictInjectingClient{Client: k8sClient}, Scheme: scheme}
	result := reconcileControlPlane(t, conflicting, cp)
	if result != (ctrl.Result{}) {
		t.Fatalf("expected no requeue for conflicting configuration, got %+v", result)
	}

	reconciled := getControlPlane(t, cp)
	if reconciled.Generation != 2 {
		t.Fatalf("assertion failed: expected generation 2, got %d", reconciled.Generation)
	}
	// The top-level status still describes generation 1 ...
	if reconciled.Status.ObservedGeneration != 1 {
		t.Fatalf("expected top-level observedGeneration to stay 1 on the conflict path, got %d", reconciled.Status.ObservedGeneration)
	}
	if reconciled.Status.Phase != healthy.Status.Phase || reconciled.Status.URL != healthy.Status.URL ||
		reconciled.Status.ReadyReplicas != healthy.Status.ReadyReplicas {
		t.Fatalf("expected phase/url/readyReplicas unchanged, before=%+v after=%+v", healthy.Status, reconciled.Status)
	}
	// ... while the condition records that generation 2 was observed.
	requireDatabaseCondition(t, reconciled, metav1.ConditionFalse, "ConflictingConfiguration")
	assertStatusAndEventsRedacted(t, reconciled)

	deploymentAfter := getControlPlaneDeployment(t, cp)
	if deploymentAfter.ResourceVersion != deploymentBefore.ResourceVersion {
		t.Fatalf("expected the Deployment to be untouched on the conflict path")
	}
	if got := deploymentAfter.Spec.Template.Spec.Containers[0].Image; got != "test-database-secret-ref:latest" {
		t.Fatalf("expected the Deployment to keep the generation 1 image, got %q", got)
	}
}

// TestDatabaseSecretRef_SecretEventsTriggerReconcile runs the real manager
// wiring and checks that creating, updating, and deleting the referenced
// Secret each re-reconcile the control plane without any periodic requeue.
func TestDatabaseSecretRef_SecretEventsTriggerReconcile(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	const namespace = "database-secret-watch"
	createTestObject(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Scope the cache so this manager only reconciles objects owned by this test.
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	r := &controller.CoderControlPlaneReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme()}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler with manager: %v", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() {
		mgrDone <- mgr.Start(mgrCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-mgrDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("manager exited with error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("timed out waiting for manager to stop")
		}
	})

	secretName := "watched-db-app" // #nosec G101 -- Secret name, not a credential.
	cp := newDatabaseControlPlane("watched-db", namespace, secretName, "uri")
	createTestObject(t, cp)

	waitForDatabaseReason := func(step, wantReason string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		lastReason := "<none>"
		for time.Now().Before(deadline) {
			latest := getControlPlane(t, cp)
			if condition := apimeta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved); condition != nil {
				lastReason = condition.Reason
				if condition.Reason == wantReason {
					assertStatusAndEventsRedacted(t, latest)
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: expected %s reason %q, last reason %q", step, coderv1alpha1.CoderControlPlaneConditionDatabaseSecretResolved, wantReason, lastReason)
	}

	waitForDatabaseReason("before Secret exists", "SecretNotFound")

	secret := newDatabaseSecret(secretName, namespace, map[string][]byte{"uri": []byte(testDatabaseURL)})
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create database secret: %v", err)
	}
	waitForDatabaseReason("after Secret create", "Resolved")

	secret.Data["uri"] = []byte("mysql://coder:" + testDatabasePassword + "@coder-db-rw.default.svc/coder")
	if err := k8sClient.Update(ctx, secret); err != nil {
		t.Fatalf("update database secret: %v", err)
	}
	waitForDatabaseReason("after Secret update", "InvalidURL")

	if err := k8sClient.Delete(ctx, secret); err != nil {
		t.Fatalf("delete database secret: %v", err)
	}
	waitForDatabaseReason("after Secret delete", "SecretNotFound")
}
