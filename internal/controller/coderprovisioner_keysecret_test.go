package controller_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

// #nosec G101 -- test fixtures, not credentials.
const (
	testUnownedKeyValue = "unowned-key-material-5e2b"
	testOwnedKeyValue   = "owned-key-material-a81c"
	testRotatedKeyValue = "rotated-key-material-0f37"
)

// createKeyProvisioner creates a CoderProvisioner whose spec.key.secretName is
// secretName ("" keeps the default name).
func (f provisionerCredentialFixture) createKeyProvisioner(ctx context.Context, t *testing.T, name, secretName string) *coderv1alpha1.CoderProvisioner {
	t.Helper()
	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: f.controlPlane.Name},
			Image:           "provisioner:v1",
			Key:             coderv1alpha1.CoderProvisionerKeySpec{SecretName: secretName},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		latest := &coderv1alpha1.CoderProvisioner{}
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(provisioner), latest); err == nil {
			latest.Finalizers = nil
			_ = k8sClient.Update(context.Background(), latest)
		}
		_ = k8sClient.Delete(context.Background(), provisioner)
	})
	return provisioner
}

func requireKeySecretMounted(ctx context.Context, t *testing.T, f provisionerCredentialFixture, provisioner *coderv1alpha1.CoderProvisioner, secretName string) {
	t.Helper()
	deployment, exists := f.deployment(ctx, t, provisioner)
	require.True(t, exists)
	var keyEnv *corev1.EnvVar
	for i := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env := &deployment.Spec.Template.Spec.Containers[0].Env[i]; env.Name == "CODER_PROVISIONER_DAEMON_KEY" {
			keyEnv = env
		}
	}
	require.NotNil(t, keyEnv)
	require.NotNil(t, keyEnv.ValueFrom)
	require.NotNil(t, keyEnv.ValueFrom.SecretKeyRef)
	require.Equal(t, secretName, keyEnv.ValueFrom.SecretKeyRef.Name)
}

func TestCoderProvisionerKeySecret_AbsentIsCreated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-key-absent")
	provisioner := f.createKeyProvisioner(ctx, t, "provisioner", "custom-key")

	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)

	latest := f.get(ctx, t, provisioner)
	requireCredentialsReady(t, latest.Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
	require.Equal(t, 1, f.bootstrapClient.provisionerKeyCalls)

	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "custom-key", Namespace: f.namespace}, secret))
	require.Equal(t, "provisioner-key-material", string(secret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))
	require.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	owner := metav1.GetControllerOf(secret)
	require.NotNil(t, owner)
	require.Equal(t, latest.UID, owner.UID)
	require.Equal(t, "CoderProvisioner", owner.Kind)
	requireKeySecretMounted(ctx, t, f, provisioner, "custom-key")

	// A further reconcile reuses the Secret it created.
	before := secret.ResourceVersion
	f.reconcile(ctx, t, provisioner)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	require.Equal(t, before, secret.ResourceVersion)
	require.Equal(t, 1, f.bootstrapClient.provisionerKeyCalls)
}

func TestCoderProvisionerKeySecret_OwnedIsReused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-key-owned")
	// Status records no key, so the controller creates one and writes it
	// into the Secret that this provisioner owns.
	f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{
		{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: testRotatedKeyValue},
	}
	provisioner := f.createKeyProvisioner(ctx, t, "provisioner", "")
	secret := tokenSecret(metav1.ObjectMeta{Name: "provisioner-provisioner-key", Namespace: f.namespace}, coderv1alpha1.DefaultProvisionerKeySecretKey, testOwnedKeyValue)
	require.NoError(t, controllerutil.SetControllerReference(provisioner, secret, scheme))
	createSecret(ctx, t, secret)
	before := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), before))

	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)

	latest := f.get(ctx, t, provisioner)
	requireCredentialsReady(t, latest.Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
	require.Equal(t, 1, f.bootstrapClient.provisionerKeyCalls)

	after := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), after))
	require.Equal(t, before.UID, after.UID, "the owned Secret is updated in place, not replaced")
	require.Equal(t, before.OwnerReferences, after.OwnerReferences)
	require.Equal(t, testRotatedKeyValue, string(after.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))
	requireKeySecretMounted(ctx, t, f, provisioner, secret.Name)
	require.Equal(t, secret.Name, latest.Status.SecretRef.Name)
}

// requireKeySecretRefused checks the outcome for a key Secret that this
// provisioner does not own: KeySecretNotOwned, no Coder request, no
// Deployment, and the Secret unchanged.
func requireKeySecretRefused(ctx context.Context, t *testing.T, f provisionerCredentialFixture, provisioner *coderv1alpha1.CoderProvisioner, secret *corev1.Secret, secretValues ...string) {
	t.Helper()
	before := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), before))

	condition := f.requireFailedStatic(ctx, t, provisioner, "KeySecretNotOwned")
	require.Contains(t, condition.Message, fmt.Sprintf("Secret %q", secret.Name))
	require.Contains(t, condition.Message, "is not owned by this provisioner")

	after := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), after))
	require.Equal(t, before.ResourceVersion, after.ResourceVersion, "the refused Secret must not change")
	require.Equal(t, before.Data, after.Data)
	require.Equal(t, before.Labels, after.Labels)
	require.Equal(t, before.OwnerReferences, after.OwnerReferences)

	latest := f.get(ctx, t, provisioner)
	require.Nil(t, latest.Status.SecretRef, "status must not point at the refused Secret")
	credentialsecrettest.RequireNoLeak(t, secretValues, map[string]any{"status": latest.Status})
}

func TestCoderProvisionerKeySecret_UnownedIsRefused(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		secretName string
	}{
		{name: "named in spec.key.secretName", secretName: "existing-key"},
		{name: "default name", secretName: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-key-unowned")
			name := tt.secretName
			if name == "" {
				name = "provisioner-provisioner-key"
			}
			secret := tokenSecret(metav1.ObjectMeta{
				Name:      name,
				Namespace: f.namespace,
				Labels:    map[string]string{"team": "platform"},
			}, coderv1alpha1.DefaultProvisionerKeySecretKey, testUnownedKeyValue)
			secret.Data["other"] = []byte("keep-me")
			createSecret(ctx, t, secret)
			provisioner := f.createKeyProvisioner(ctx, t, "provisioner", tt.secretName)

			requireKeySecretRefused(ctx, t, f, provisioner, secret, testUnownedKeyValue)
		})
	}
}

func TestCoderProvisionerKeySecret_OtherOwnersAreRefused(t *testing.T) {
	t.Parallel()

	t.Run("another provisioner's key Secret", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		f := newProvisionerCredentialFixture(ctx, t, "cprov-key-other")
		other := f.createKeyProvisioner(ctx, t, "other", "")
		secret := tokenSecret(metav1.ObjectMeta{Name: "other-provisioner-key", Namespace: f.namespace}, coderv1alpha1.DefaultProvisionerKeySecretKey, testOwnedKeyValue)
		require.NoError(t, controllerutil.SetControllerReference(other, secret, scheme))
		createSecret(ctx, t, secret)
		provisioner := f.createKeyProvisioner(ctx, t, "provisioner", secret.Name)

		requireKeySecretRefused(ctx, t, f, provisioner, secret, testOwnedKeyValue)
	})

	t.Run("operator token Secret of the control plane", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		f := newProvisionerCredentialFixture(ctx, t, "cprov-key-operator")
		operatorSecret := &corev1.Secret{}
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: f.controlPlane.Status.OperatorTokenSecretRef.Name, Namespace: f.namespace}, operatorSecret))
		provisioner := f.createKeyProvisioner(ctx, t, "provisioner", operatorSecret.Name)

		requireKeySecretRefused(ctx, t, f, provisioner, operatorSecret, testOperatorToken)
	})
}

func TestCoderProvisionerKeySecret_ReplacedByUnownedKeepsDeployment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-key-replaced")
	provisioner := f.createKeyProvisioner(ctx, t, "provisioner", "")
	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)
	before, exists := f.deployment(ctx, t, provisioner)
	require.True(t, exists)
	keyCalls := f.bootstrapClient.provisionerKeyCalls

	// Replace the owned Secret with an unowned one of the same name.
	key := types.NamespacedName{Name: "provisioner-provisioner-key", Namespace: f.namespace}
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}))
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.Secret{})))
	unowned := createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, coderv1alpha1.DefaultProvisionerKeySecretKey, testUnownedKeyValue))
	latest := f.get(ctx, t, provisioner)
	latest.Spec.Image = "provisioner:v2"
	require.NoError(t, k8sClient.Update(ctx, latest))

	f.reconcile(ctx, t, provisioner)
	requireCredentialsReady(t, f.get(ctx, t, provisioner).Status.Conditions, metav1.ConditionFalse, "KeySecretNotOwned")
	after, exists := f.deployment(ctx, t, provisioner)
	require.True(t, exists)
	require.Equal(t, before.ResourceVersion, after.ResourceVersion, "the Deployment must stay byte-identical")
	require.Equal(t, keyCalls, f.bootstrapClient.provisionerKeyCalls)
	stored := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, key, stored))
	require.Equal(t, unowned.UID, stored.UID)
	require.Empty(t, stored.OwnerReferences)
	require.Equal(t, testUnownedKeyValue, string(stored.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))
}

func TestCoderProvisionerKeySecret_DeletingUnownedRecovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-key-recover")
	secret := createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: "existing-key", Namespace: f.namespace}, coderv1alpha1.DefaultProvisionerKeySecretKey, testUnownedKeyValue))
	provisioner := f.createKeyProvisioner(ctx, t, "provisioner", secret.Name)
	requireKeySecretRefused(ctx, t, f, provisioner, secret, testUnownedKeyValue)

	// The documented remedy: delete the unowned Secret.
	require.NoError(t, k8sClient.Delete(ctx, secret))
	f.reconcile(ctx, t, provisioner)

	latest := f.get(ctx, t, provisioner)
	requireCredentialsReady(t, latest.Status.Conditions, metav1.ConditionTrue, "CredentialsAccepted")
	require.Equal(t, 1, f.bootstrapClient.provisionerKeyCalls)
	created := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), created))
	require.NotEqual(t, secret.UID, created.UID)
	owner := metav1.GetControllerOf(created)
	require.NotNil(t, owner)
	require.Equal(t, latest.UID, owner.UID)
	require.Equal(t, "provisioner-key-material", string(created.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))
	requireKeySecretMounted(ctx, t, f, provisioner, secret.Name)
}
