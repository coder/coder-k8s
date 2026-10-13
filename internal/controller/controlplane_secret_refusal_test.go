package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/credentialsecret"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

// A Secret that a CoderControlPlane spec references is never usable as a
// credential of another resource in the namespace, even when it carries the
// opt-in label and lists the destination (#249, #260).
var (
	controlPlaneSecretRoles = credentialsecrettest.ControlPlaneSecretRoles
	setControlPlaneSecret   = credentialsecrettest.SetControlPlaneSecret
)

// createControlPlaneUsing creates a second control plane in namespace that
// uses secretName in role. It returns the control plane name.
func createControlPlaneUsing(ctx context.Context, t *testing.T, namespace, role, secretName string) string {
	t.Helper()
	controlPlane := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-user", Namespace: namespace},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "coder-control-plane:test"},
	}
	setControlPlaneSecret(&controlPlane.Spec, role, secretName)
	require.NoError(t, k8sClient.Create(ctx, controlPlane))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), controlPlane) })
	return controlPlane.Name
}

func refusalMessage(secretName, role, controlPlaneName string) string {
	return fmt.Sprintf("Secret %q is the %s Secret of CoderControlPlane %q and cannot be used here.", secretName, role, controlPlaneName)
}

func TestCoderWorkspaceProxyCredentials_RefusesControlPlaneSecrets(t *testing.T) {
	t.Parallel()

	allInputs := []string{"bootstrap.credentialsSecretRef", "proxySessionTokenSecretRef", "extraEnv"}
	for i, role := range controlPlaneSecretRoles {
		// The license and database roles run every input. The other roles share
		// one Read path, so each runs one input, in turn.
		inputs := []string{allInputs[i%len(allInputs)]}
		if role == "license" || role == "database" {
			inputs = allInputs
		}
		for _, input := range inputs {
			t.Run(role+"/"+input, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				namespace := createTestNamespace(ctx, t, "cwp-cpsecret")
				createSecret(ctx, t, tokenSecret(consentedObjectMeta("cp-secret", namespace, testCoderOrigin), "token", testProxyTokenValue))
				controlPlaneName := createControlPlaneUsing(ctx, t, namespace, role, "cp-secret")

				ref := coderv1alpha1.SecretKeySelector{Name: "cp-secret", Key: "token"}
				spec := coderv1alpha1.WorkspaceProxySpec{PrimaryAccessURL: testCoderOrigin}
				switch input {
				case "bootstrap.credentialsSecretRef":
					spec.Bootstrap = &coderv1alpha1.ProxyBootstrapSpec{CoderURL: testCoderOrigin, CredentialsSecretRef: ref}
				case "proxySessionTokenSecretRef":
					spec.ProxySessionTokenSecretRef = &ref
				case "extraEnv":
					createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))
					spec.ProxySessionTokenSecretRef = &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"}
					spec.ExtraEnv = []corev1.EnvVar{secretEnv("SOME_TOKEN", "cp-secret", "token")}
				}
				workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", spec)

				bootstrapClient := &fakeBootstrapClient{response: coderbootstrap.RegisterWorkspaceProxyResponse{ProxyToken: testGeneratedToken}}
				r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
				condition := requireProxyFailedStatic(ctx, t, r, bootstrapClient, workspaceProxy, credentialsecret.ReasonRefused)
				require.Contains(t, condition.Message, refusalMessage("cp-secret", role, controlPlaneName))
			})
		}
	}
}

func TestCoderProvisionerCredentials_RefusesControlPlaneSecrets(t *testing.T) {
	t.Parallel()

	for _, role := range controlPlaneSecretRoles {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-cpsecret")
			createSecret(ctx, t, tokenSecret(consentedObjectMeta("cp-secret", f.namespace, testCoderOrigin), "value", testExtraEnvValue))
			// The provisioner's own control plane uses the Secret.
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(f.controlPlane), f.controlPlane))
			setControlPlaneSecret(&f.controlPlane.Spec, role, "cp-secret")
			require.NoError(t, k8sClient.Update(ctx, f.controlPlane))

			provisioner := f.createProvisioner(ctx, t, []corev1.EnvVar{secretEnv("FROM_SECRET", "cp-secret", "value")}, nil)
			condition := f.requireFailedStatic(ctx, t, provisioner, credentialsecret.ReasonRefused)
			require.Contains(t, condition.Message, "spec.extraEnv[0] (FROM_SECRET): "+refusalMessage("cp-secret", role, f.controlPlane.Name))
		})
	}
}

func TestCoderAIProviderRefusesControlPlaneSecrets(t *testing.T) {
	t.Parallel()

	for _, role := range controlPlaneSecretRoles {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newAIProviderEnv(ctx, t, true)
			secret := createSecret(ctx, t, env.keySecret(consentedObjectMeta("", "", testAIProviderOrigin), testAIProviderKey))
			controlPlaneName := createControlPlaneUsing(ctx, t, env.namespace, role, secret.Name)

			got, result := env.reconcile(ctx, t, env.createProvider(ctx, t, nil))
			message := requireAIProviderStep(t, got, true, credentialsecret.ReasonRefused)
			require.Contains(t, message, refusalMessage(secret.Name, role, controlPlaneName))
			require.Zero(t, env.fake.totalRequests(), "a refused key sends nothing to Coder")
			require.Zero(t, result.RequeueAfter, "the control plane watch requeues the provider")
		})
	}
}

// TestCoderWorkspaceProxyCredentials_ControlPlaneWatchRequeues runs the
// controller in a manager. A control plane that starts using the proxy's token
// Secret as its license or database Secret re-queues the proxy into a refusal.
// A control plane that stops using it re-queues the proxy back.
func TestCoderWorkspaceProxyCredentials_ControlPlaneWatchRequeues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "cwp-cpwatch")
	createSecret(ctx, t, tokenSecret(consentedObjectMeta("proxy-token", namespace, testCoderOrigin), "token", testProxyTokenValue))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Controller:             config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	r := &controller.CoderWorkspaceProxyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	require.NoError(t, r.SetupWithManager(mgr))
	mgrCtx, cancel := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-mgrDone)
	})

	workspaceProxy := createWorkspaceProxy(ctx, t, namespace, "proxy", coderv1alpha1.WorkspaceProxySpec{
		PrimaryAccessURL:           testCoderOrigin,
		ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "proxy-token", Key: "token"},
	})
	requireStatus := func(status metav1.ConditionStatus, reason string) {
		t.Helper()
		require.Eventually(t, func() bool {
			condition := meta.FindStatusCondition(getWorkspaceProxy(ctx, t, workspaceProxy).Status.Conditions, "CredentialsReady")
			return condition != nil && condition.Status == status && condition.Reason == reason
		}, 30*time.Second, 100*time.Millisecond, "%s %s", status, reason)
	}
	requireStatus(metav1.ConditionTrue, "CredentialsAccepted")

	controlPlane := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: namespace},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "coder-control-plane:test"},
	}
	require.NoError(t, k8sClient.Create(ctx, controlPlane))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), controlPlane) })
	for _, role := range controlPlaneSecretRoles {
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(controlPlane), controlPlane))
		setControlPlaneSecret(&controlPlane.Spec, role, "proxy-token")
		require.NoError(t, k8sClient.Update(ctx, controlPlane))
		requireStatus(metav1.ConditionFalse, credentialsecret.ReasonRefused)

		// Only the old object names the Secret now.
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(controlPlane), controlPlane))
		setControlPlaneSecret(&controlPlane.Spec, role, "other-secret")
		require.NoError(t, k8sClient.Update(ctx, controlPlane))
		requireStatus(metav1.ConditionTrue, "CredentialsAccepted")
	}
}
