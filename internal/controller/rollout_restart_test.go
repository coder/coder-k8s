package controller_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
)

const (
	restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
	// An unusual but valid value proves the controller copies it verbatim
	// instead of generating or normalizing a timestamp.
	restartedAtValue      = "2026-09-24T10:11:12+02:00"
	foreignTemplateAnnote = "example.com/not-preserved"
)

// rolloutRestartCase drives one controller through the rollout-restart checks.
type rolloutRestartCase struct {
	// reconcile runs one reconcile of the owning object and fails on error.
	reconcile func(t *testing.T)
	// deploymentKey names the managed Deployment.
	deploymentKey types.NamespacedName
	// changeSpec updates the owning object so the desired container image changes.
	changeSpec func(t *testing.T)
	// wantImage is the container image expected after changeSpec.
	wantImage string
	// extraAnnotations are controller-owned template annotations that must survive.
	extraAnnotations []string
}

func getManagedDeployment(t *testing.T, key types.NamespacedName) *appsv1.Deployment {
	t.Helper()

	deployment := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(context.Background(), key, deployment))
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1, "assertion failed: expected one container")
	return deployment
}

// runRolloutRestartCase checks that reconciles never add restartedAt, keep an
// existing value verbatim across repeated reconciles, still apply spec
// changes, and drop other foreign template annotations.
func runRolloutRestartCase(t *testing.T, tc rolloutRestartCase) {
	t.Helper()
	ctx := context.Background()

	tc.reconcile(t)
	tc.reconcile(t)
	deployment := getManagedDeployment(t, tc.deploymentKey)
	require.NotContains(t, deployment.Spec.Template.Annotations, restartedAtAnnotation,
		"reconcile must not add %s when it was absent", restartedAtAnnotation)

	// Simulate `kubectl rollout restart` plus an unrelated foreign annotation.
	patch := ctrlclient.MergeFrom(deployment.DeepCopy())
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations[restartedAtAnnotation] = restartedAtValue
	deployment.Spec.Template.Annotations[foreignTemplateAnnote] = "drop-me"
	require.NoError(t, k8sClient.Patch(ctx, deployment, patch))

	tc.changeSpec(t)
	for range 3 {
		tc.reconcile(t)
	}

	deployment = getManagedDeployment(t, tc.deploymentKey)
	require.Equal(t, restartedAtValue, deployment.Spec.Template.Annotations[restartedAtAnnotation],
		"reconcile must keep %s verbatim", restartedAtAnnotation)
	require.NotContains(t, deployment.Spec.Template.Annotations, foreignTemplateAnnote,
		"only %s may be preserved", restartedAtAnnotation)
	for _, annotation := range tc.extraAnnotations {
		require.NotEmpty(t, deployment.Spec.Template.Annotations[annotation], "controller annotation %s must remain", annotation)
	}
	require.Equal(t, tc.wantImage, deployment.Spec.Template.Spec.Containers[0].Image,
		"desired pod spec changes must still apply")
}

func TestRolloutRestart_CoderControlPlaneKeepsRestartedAt(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-rollout-restart-cp", Namespace: "default"},
		Spec:       coderv1alpha1.CoderControlPlaneSpec{Image: "rollout-restart-cp:v1"},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cp) })

	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
	key := types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}
	runRolloutRestartCase(t, rolloutRestartCase{
		reconcile: func(t *testing.T) {
			t.Helper()
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
		},
		deploymentKey: key,
		changeSpec: func(t *testing.T) {
			t.Helper()
			latest := &coderv1alpha1.CoderControlPlane{}
			require.NoError(t, k8sClient.Get(ctx, key, latest))
			latest.Spec.Image = "rollout-restart-cp:v2"
			require.NoError(t, k8sClient.Update(ctx, latest))
		},
		wantImage: "rollout-restart-cp:v2",
	})
}

func TestRolloutRestart_CoderProvisionerKeepsRestartedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	namespace := createTestNamespace(ctx, t, "rollout-restart-prov")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-rollout-restart", "https://coder.example.com")
	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{{
			OrganizationID: uuid.New(),
			KeyID:          uuid.New(),
			KeyName:        "rollout-restart-key",
			Key:            "rollout-restart-key-material",
		}},
	}
	r := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-rollout-restart", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Image:           "rollout-restart-provisioner:v1",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), provisioner) })

	key := types.NamespacedName{Name: provisioner.Name, Namespace: namespace}
	runRolloutRestartCase(t, rolloutRestartCase{
		reconcile: func(t *testing.T) {
			t.Helper()
			reconcileProvisioner(ctx, t, r, key)
		},
		deploymentKey: types.NamespacedName{Name: expectedProvisionerResourceName(provisioner.Name), Namespace: namespace},
		changeSpec: func(t *testing.T) {
			t.Helper()
			latest := &coderv1alpha1.CoderProvisioner{}
			require.NoError(t, k8sClient.Get(ctx, key, latest))
			latest.Spec.Image = "rollout-restart-provisioner:v2"
			require.NoError(t, k8sClient.Update(ctx, latest))
		},
		wantImage:        "rollout-restart-provisioner:v2",
		extraAnnotations: []string{"checksum/provisioner-key"},
	})
}

func TestRolloutRestart_CoderWorkspaceProxyKeepsRestartedAt(t *testing.T) {
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-rollout-restart-token", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("token-value")},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), secret) })

	workspaceProxy := &coderv1alpha1.CoderWorkspaceProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-rollout-restart", Namespace: "default"},
		Spec: coderv1alpha1.WorkspaceProxySpec{
			Image:            "rollout-restart-proxy:v1",
			PrimaryAccessURL: "https://coder.example.com",
			ProxySessionTokenSecretRef: &coderv1alpha1.SecretKeySelector{
				Name: secret.Name,
				Key:  coderv1alpha1.DefaultTokenSecretKey,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, workspaceProxy))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspaceProxy) })

	r := &controller.CoderWorkspaceProxyReconciler{Client: k8sClient, Scheme: scheme}
	key := types.NamespacedName{Name: workspaceProxy.Name, Namespace: workspaceProxy.Namespace}
	runRolloutRestartCase(t, rolloutRestartCase{
		reconcile: func(t *testing.T) {
			t.Helper()
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
		},
		deploymentKey: types.NamespacedName{Name: workspaceProxyResourceName(workspaceProxy.Name), Namespace: workspaceProxy.Namespace},
		changeSpec: func(t *testing.T) {
			t.Helper()
			latest := &coderv1alpha1.CoderWorkspaceProxy{}
			require.NoError(t, k8sClient.Get(ctx, key, latest))
			latest.Spec.Image = "rollout-restart-proxy:v2"
			require.NoError(t, k8sClient.Update(ctx, latest))
		},
		wantImage: "rollout-restart-proxy:v2",
	})
}
