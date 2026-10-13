package controller_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/credentialsecret"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

type provisionerCredentialFixture struct {
	namespace       string
	controlPlane    *coderv1alpha1.CoderControlPlane
	bootstrapClient *fakeBootstrapClient
	reconciler      *controller.CoderProvisionerReconciler
}

func newProvisionerCredentialFixture(ctx context.Context, t *testing.T, prefix string) provisionerCredentialFixture {
	t.Helper()
	namespace := createTestNamespace(ctx, t, prefix)
	controlPlane := createTestControlPlane(ctx, t, namespace, "coder", testCoderOrigin)
	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{{
			OrganizationID: uuid.New(),
			KeyID:          uuid.New(),
			KeyName:        "provisioner",
			Key:            "provisioner-key-material",
		}},
	}
	return provisionerCredentialFixture{
		namespace:       namespace,
		controlPlane:    controlPlane,
		bootstrapClient: bootstrapClient,
		reconciler:      &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient},
	}
}

func (f provisionerCredentialFixture) createProvisioner(ctx context.Context, t *testing.T, env []corev1.EnvVar, args []string) *coderv1alpha1.CoderProvisioner {
	t.Helper()
	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner", Namespace: f.namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: f.controlPlane.Name},
			Image:           "provisioner:v1",
			ExtraEnv:        env,
			ExtraArgs:       args,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		latest := &coderv1alpha1.CoderProvisioner{}
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}, latest); err == nil {
			latest.Finalizers = nil
			_ = k8sClient.Update(context.Background(), latest)
		}
		_ = k8sClient.Delete(context.Background(), provisioner)
	})
	return provisioner
}

func (f provisionerCredentialFixture) reconcile(ctx context.Context, t *testing.T, provisioner *coderv1alpha1.CoderProvisioner) {
	t.Helper()
	result, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
}

func (f provisionerCredentialFixture) get(ctx context.Context, t *testing.T, provisioner *coderv1alpha1.CoderProvisioner) *coderv1alpha1.CoderProvisioner {
	t.Helper()
	latest := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}, latest))
	return latest
}

func (f provisionerCredentialFixture) deployment(ctx context.Context, t *testing.T, provisioner *coderv1alpha1.CoderProvisioner) (*appsv1.Deployment, bool) {
	t.Helper()
	deployment := &appsv1.Deployment{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: expectedProvisionerResourceName(provisioner.Name), Namespace: provisioner.Namespace}, deployment)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return deployment, true
}

// requireFailedStatic checks one reconcile (after the finalizer reconcile):
// CredentialsReady=False with reason, no Coder request, no Deployment, and no
// secret value in status or logs.
func (f provisionerCredentialFixture) requireFailedStatic(ctx context.Context, t *testing.T, provisioner *coderv1alpha1.CoderProvisioner, reason string) metav1.Condition {
	t.Helper()
	logCtx, logs := capturingContext(ctx)
	f.reconcile(logCtx, t, provisioner)
	f.reconcile(logCtx, t, provisioner)

	latest := f.get(ctx, t, provisioner)
	condition := requireCredentialsReady(t, latest.Status.Conditions, metav1.ConditionFalse, reason)
	require.Contains(t, condition.Message, "The provisioner keeps its last applied configuration.")
	require.Equal(t, 0, f.bootstrapClient.provisionerKeyCalls, "expected no provisioner key request")
	require.Equal(t, 0, f.bootstrapClient.entitlementsCalls, "expected no entitlements request")
	_, exists := f.deployment(ctx, t, provisioner)
	require.False(t, exists, "expected no Deployment after a failed check")
	credentialsecrettest.RequireNoLeak(t, []string{testExtraEnvValue, testOperatorToken, "provisioner-key-material"},
		map[string]any{"status": latest.Status, "annotations": latest.Annotations, "logs": logs.String()})
	return condition
}

func secretEnv(name, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: key,
	}}}
}

func TestCoderProvisionerCredentials_ExtraEnvSecretChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		secret     func(t *testing.T, f provisionerCredentialFixture) string
		wantReason string
	}{
		{
			name: "unlabeled secret",
			secret: func(t *testing.T, f provisionerCredentialFixture) string {
				createSecret(context.Background(), t, tokenSecret(metav1.ObjectMeta{Name: "extra", Namespace: f.namespace}, "value", testExtraEnvValue))
				return "extra"
			},
			wantReason: credentialsecret.ReasonNotOptedIn,
		},
		{
			name: "destination is not the control plane URL",
			secret: func(t *testing.T, f provisionerCredentialFixture) string {
				createSecret(context.Background(), t, tokenSecret(consentedObjectMeta("extra", f.namespace, "https://other.example.com"), "value", testExtraEnvValue))
				return "extra"
			},
			wantReason: credentialsecret.ReasonDestinationNotAllowed,
		},
		{
			name:       "missing secret",
			secret:     func(*testing.T, provisionerCredentialFixture) string { return "missing" },
			wantReason: credentialsecret.ReasonSecretNotFound,
		},
		{
			name: "operator token secret of the control plane",
			secret: func(t *testing.T, f provisionerCredentialFixture) string {
				ctx := context.Background()
				operatorSecret := &corev1.Secret{}
				require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: f.controlPlane.Status.OperatorTokenSecretRef.Name, Namespace: f.namespace}, operatorSecret))
				consented := consentedObjectMeta(operatorSecret.Name, f.namespace, testCoderOrigin)
				operatorSecret.Labels, operatorSecret.Annotations = consented.Labels, consented.Annotations
				require.NoError(t, k8sClient.Update(ctx, operatorSecret))
				return operatorSecret.Name
			},
			wantReason: credentialsecret.ReasonRefused,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-extraenv")
			secretName := tt.secret(t, f)
			provisioner := f.createProvisioner(ctx, t, []corev1.EnvVar{
				{Name: "PLAIN", Value: "plain"},
				secretEnv("FROM_SECRET", secretName, "value"),
			}, nil)

			condition := f.requireFailedStatic(ctx, t, provisioner, tt.wantReason)
			require.Contains(t, condition.Message, "spec.extraEnv[1] (FROM_SECRET): Secret \""+secretName+"\"")
		})
	}
}

func TestCoderProvisionerCredentials_ConsentedExtraEnvSecretPasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-consented")
	createSecret(ctx, t, tokenSecret(consentedObjectMeta("extra", f.namespace, testCoderOrigin), "value", testExtraEnvValue))
	provisioner := f.createProvisioner(ctx, t, []corev1.EnvVar{secretEnv("FROM_SECRET", "extra", "value")}, nil)

	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)

	requireCredentialsReady(t, f.get(ctx, t, provisioner).Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
	require.Equal(t, 1, f.bootstrapClient.provisionerKeyCalls)
	deployment, exists := f.deployment(ctx, t, provisioner)
	require.True(t, exists)
	// The provisioner runs as its own ServiceAccount and keeps its API token.
	require.Nil(t, deployment.Spec.Template.Spec.AutomountServiceAccountToken)
}

func TestCoderProvisionerCredentials_ReservedSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      []corev1.EnvVar
		args     []string
		reserved string
	}{
		{name: "env CODER_URL", env: []corev1.EnvVar{{Name: "CODER_URL", Value: "https://other.example.com"}}, reserved: "spec.extraEnv sets CODER_URL"},
		{name: "env CODER_PROVISIONER_DAEMON_KEY", env: []corev1.EnvVar{{Name: "CODER_PROVISIONER_DAEMON_KEY", Value: "x"}}, reserved: "spec.extraEnv sets CODER_PROVISIONER_DAEMON_KEY"},
		{name: "env CODER_URL from a secret", env: []corev1.EnvVar{secretEnv("CODER_URL", "any", "value")}, reserved: "spec.extraEnv sets CODER_URL"},
		{name: "flag --url value", args: []string{"--url", "https://other.example.com"}, reserved: "spec.extraArgs passes --url"},
		{name: "flag --url=value", args: []string{"--url=https://other.example.com"}, reserved: "spec.extraArgs passes --url"},
		{name: "flag -url value", args: []string{"-url", "https://other.example.com"}, reserved: "spec.extraArgs passes --url"},
		{name: "flag --key value", args: []string{"--key", "x"}, reserved: "spec.extraArgs passes --key"},
		{name: "flag --key=value", args: []string{"--key=x"}, reserved: "spec.extraArgs passes --key"},
		{name: "flag -key value", args: []string{"-key", "x"}, reserved: "spec.extraArgs passes --key"},
		{name: "env lowercase coder_url", env: []corev1.EnvVar{{Name: "coder_url", Value: "https://other.example.com"}}, reserved: "spec.extraEnv sets coder_url"},
		{
			name:     "flag hidden in an env reference",
			env:      []corev1.EnvVar{{Name: "X", Value: "--url=https://other.example.com"}},
			args:     []string{"$(X)"},
			reserved: "spec.extraArgs passes --url",
		},
		{name: "look-alike env and flags pass", env: []corev1.EnvVar{{Name: "CODER_URL_EXTRA", Value: "x"}}, args: []string{"--keyless", "--url-prefix=x", "--verbose", "key"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-reserved")
			provisioner := f.createProvisioner(ctx, t, tt.env, tt.args)

			if tt.reserved == "" {
				f.reconcile(ctx, t, provisioner)
				f.reconcile(ctx, t, provisioner)
				requireCredentialsReady(t, f.get(ctx, t, provisioner).Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
				_, exists := f.deployment(ctx, t, provisioner)
				require.True(t, exists)
				return
			}
			condition := f.requireFailedStatic(ctx, t, provisioner, "ReservedSettingOverride")
			require.Contains(t, condition.Message, tt.reserved)
			require.NotContains(t, condition.Message, "https://other.example.com", "the message must not echo the value")
		})
	}
}

func TestCoderProvisionerCredentials_FailedCheckLeavesDeploymentUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-static")
	provisioner := f.createProvisioner(ctx, t, nil, nil)
	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)
	before, exists := f.deployment(ctx, t, provisioner)
	require.True(t, exists)
	keyCalls := f.bootstrapClient.provisionerKeyCalls

	latest := f.get(ctx, t, provisioner)
	latest.Spec.Image = "provisioner:v2"
	latest.Spec.ExtraEnv = []corev1.EnvVar{secretEnv("FROM_SECRET", "unconsented", "value")}
	require.NoError(t, k8sClient.Update(ctx, latest))
	createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: "unconsented", Namespace: f.namespace}, "value", testExtraEnvValue))

	f.reconcile(ctx, t, provisioner)
	requireCredentialsReady(t, f.get(ctx, t, provisioner).Status.Conditions, metav1.ConditionFalse, credentialsecret.ReasonNotOptedIn)
	after, exists := f.deployment(ctx, t, provisioner)
	require.True(t, exists)
	require.Equal(t, before.ResourceVersion, after.ResourceVersion, "the Deployment must stay byte-identical")
	require.Equal(t, before.Spec, after.Spec)
	require.Equal(t, keyCalls, f.bootstrapClient.provisionerKeyCalls)
}
