package controller_test

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
)

func createTestNamespace(ctx context.Context, t *testing.T, prefix string) string {
	t.Helper()

	namespaceName := fmt.Sprintf("%s-%s", prefix, strings.ToLower(uuid.NewString()[:8]))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
	require.NoError(t, k8sClient.Create(ctx, ns))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})

	return namespaceName
}

// createTestControlPlane creates a test CoderControlPlane and optionally sets status.url.
func createTestControlPlane(ctx context.Context, t *testing.T, namespace, name, url string) *coderv1alpha1.CoderControlPlane {
	t.Helper()

	controlPlane := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image: "coder-control-plane:test",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, controlPlane))
	if url != "" {
		controlPlane.Status.URL = url
		require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), controlPlane)
	})

	return controlPlane
}

// createBootstrapSecret creates the bootstrap credentials secret used by provisioner reconciliation.
func createBootstrapSecret(ctx context.Context, t *testing.T, namespace, name, key, value string) *corev1.Secret {
	t.Helper()

	if key == "" {
		key = coderv1alpha1.DefaultTokenSecretKey
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			key: []byte(value),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secret)
	})

	return secret
}

func expectedProvisionerResourceName(name string) string {
	const prefix = "provisioner-"
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

func expectedProvisionerServiceAccountName(name string) string {
	const suffix = "-provisioner"
	candidate := fmt.Sprintf("%s%s", name, suffix)
	if len(candidate) <= 63 {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	hashSuffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(suffix) - len(hashSuffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s%s", name[:available], hashSuffix, suffix)
}

func expectedProvisionerKeyName(name string) string {
	const maxKeyNameLength = 128
	if len(name) <= maxKeyNameLength {
		return name
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := maxKeyNameLength - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s", name[:available], suffix)
}

func reconcileProvisioner(ctx context.Context, t *testing.T, reconciler *controller.CoderProvisionerReconciler, namespacedName types.NamespacedName) {
	t.Helper()

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
}

func requireOwnerReference(t *testing.T, owner, child metav1.Object) {
	t.Helper()

	ownerReferences := child.GetOwnerReferences()
	require.NotEmpty(t, ownerReferences)

	for _, ownerReference := range ownerReferences {
		if ownerReference.Name == owner.GetName() && ownerReference.UID == owner.GetUID() {
			return
		}
	}

	require.Failf(t, "missing owner reference", "expected %s/%s to own %s/%s", owner.GetNamespace(), owner.GetName(), child.GetNamespace(), child.GetName())
}

func requireCondition(t *testing.T, conditions []metav1.Condition, condType string, status metav1.ConditionStatus) {
	t.Helper()
	condition := findCondition(t, conditions, condType)
	require.Equal(t, status, condition.Status, "condition %s: expected status %v, got %v", condType, status, condition.Status)
}

func findCondition(t *testing.T, conditions []metav1.Condition, condType string) metav1.Condition {
	t.Helper()
	for idx := range conditions {
		if conditions[idx].Type == condType {
			return conditions[idx]
		}
	}

	require.Failf(t, "condition not found", "expected condition %s to be present", condType)
	return metav1.Condition{}
}

func TestCoderProvisionerReconciler_BasicCreate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-basic")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-basic", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	organizationID := uuid.New()
	provisionerKeyID := uuid.New()
	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{{
			OrganizationID: organizationID,
			KeyID:          provisionerKeyID,
			KeyName:        "provisioner-key-name",
			Key:            "provisioner-key-material",
		}},
	}
	reconciler := &controller.CoderProvisionerReconciler{
		Client:          k8sClient,
		Scheme:          scheme,
		BootstrapClient: bootstrapClient,
	}

	replicas := int32(2)
	terminationGracePeriodSeconds := int64(120)
	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provisioner-basic",
			Namespace: namespace,
		},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef:  corev1.LocalObjectReference{Name: controlPlane.Name},
			OrganizationName: "acme",
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{
					Name: bootstrapSecret.Name,
					Key:  coderv1alpha1.DefaultTokenSecretKey,
				},
			},
			Key: coderv1alpha1.CoderProvisionerKeySpec{
				Name:       "provisioner-key-name",
				SecretName: "provisioner-basic-key",
				SecretKey:  "daemon-key",
			},
			Replicas:                      &replicas,
			Tags:                          map[string]string{"region": "test"},
			Image:                         "provisioner-image:test",
			ExtraArgs:                     []string{"--test-mode=true"},
			ExtraEnv:                      []corev1.EnvVar{{Name: "EXTRA_ENV", Value: "extra-value"}},
			ImagePullSecrets:              []corev1.LocalObjectReference{{Name: "regcred"}},
			TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	namespacedName := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
	reconcileProvisioner(ctx, t, reconciler, namespacedName)
	reconcileProvisioner(ctx, t, reconciler, namespacedName)

	reconciledProvisioner := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, namespacedName, reconciledProvisioner))
	require.Contains(t, reconciledProvisioner.Finalizers, coderv1alpha1.ProvisionerKeyCleanupFinalizer)

	require.Equal(t, 1, bootstrapClient.provisionerKeyCalls)
	require.Equal(t, 0, bootstrapClient.deleteKeyCalls)

	keySecret := &corev1.Secret{}
	keySecretName := types.NamespacedName{Name: provisioner.Spec.Key.SecretName, Namespace: provisioner.Namespace}
	require.NoError(t, k8sClient.Get(ctx, keySecretName, keySecret))
	require.Equal(t, "provisioner-key-material", string(keySecret.Data[provisioner.Spec.Key.SecretKey]))
	requireOwnerReference(t, reconciledProvisioner, keySecret)

	serviceAccount := &corev1.ServiceAccount{}
	saNamespacedName := types.NamespacedName{Name: expectedProvisionerServiceAccountName(provisioner.Name), Namespace: provisioner.Namespace}
	require.NoError(t, k8sClient.Get(ctx, saNamespacedName, serviceAccount))
	requireOwnerReference(t, reconciledProvisioner, serviceAccount)

	roleName := expectedProvisionerResourceName(provisioner.Name)
	role := &rbacv1.Role{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: provisioner.Namespace}, role))
	requireOwnerReference(t, reconciledProvisioner, role)
	require.Len(t, role.Rules, 1)
	require.ElementsMatch(t, []string{""}, role.Rules[0].APIGroups)
	require.ElementsMatch(t, []string{"pods", "persistentvolumeclaims"}, role.Rules[0].Resources)
	require.ElementsMatch(t, []string{"get", "list", "watch", "create", "update", "patch", "delete"}, role.Rules[0].Verbs)

	roleBinding := &rbacv1.RoleBinding{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: provisioner.Namespace}, roleBinding))
	requireOwnerReference(t, reconciledProvisioner, roleBinding)
	require.Equal(t, rbacv1.GroupName, roleBinding.RoleRef.APIGroup)
	require.Equal(t, "Role", roleBinding.RoleRef.Kind)
	require.Equal(t, role.Name, roleBinding.RoleRef.Name)
	require.Len(t, roleBinding.Subjects, 1)
	require.Equal(t, rbacv1.ServiceAccountKind, roleBinding.Subjects[0].Kind)
	require.Equal(t, serviceAccount.Name, roleBinding.Subjects[0].Name)
	require.Equal(t, provisioner.Namespace, roleBinding.Subjects[0].Namespace)

	deployment := &appsv1.Deployment{}
	deploymentName := types.NamespacedName{Name: roleName, Namespace: provisioner.Namespace}
	require.NoError(t, k8sClient.Get(ctx, deploymentName, deployment))
	requireOwnerReference(t, reconciledProvisioner, deployment)

	require.NotNil(t, deployment.Spec.Replicas)
	require.Equal(t, replicas, *deployment.Spec.Replicas)
	require.Equal(t, expectedProvisionerServiceAccountName(provisioner.Name), deployment.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, []corev1.LocalObjectReference{{Name: "regcred"}}, deployment.Spec.Template.Spec.ImagePullSecrets)
	require.NotNil(t, deployment.Spec.Template.Spec.TerminationGracePeriodSeconds)
	require.Equal(t, terminationGracePeriodSeconds, *deployment.Spec.Template.Spec.TerminationGracePeriodSeconds)
	require.NotEmpty(t, deployment.Spec.Template.Annotations["checksum/provisioner-key"])

	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	container := deployment.Spec.Template.Spec.Containers[0]
	require.Equal(t, "provisioner", container.Name)
	require.Equal(t, "provisioner-image:test", container.Image)
	require.Equal(t, []string{"provisionerd", "start", "--test-mode=true"}, container.Args)

	envByName := make(map[string]corev1.EnvVar, len(container.Env))
	for _, envVar := range container.Env {
		envByName[envVar.Name] = envVar
	}
	require.Equal(t, "https://coder.example.com", envByName["CODER_URL"].Value)
	require.Equal(t, "acme", envByName["CODER_ORGANIZATION"].Value)
	require.Equal(t, "extra-value", envByName["EXTRA_ENV"].Value)
	keyEnv, ok := envByName["CODER_PROVISIONER_DAEMON_KEY"]
	require.True(t, ok)
	require.NotNil(t, keyEnv.ValueFrom)
	require.NotNil(t, keyEnv.ValueFrom.SecretKeyRef)
	require.Equal(t, provisioner.Spec.Key.SecretName, keyEnv.ValueFrom.SecretKeyRef.Name)
	require.Equal(t, provisioner.Spec.Key.SecretKey, keyEnv.ValueFrom.SecretKeyRef.Key)

	require.Equal(t, reconciledProvisioner.Generation, reconciledProvisioner.Status.ObservedGeneration)
	require.Equal(t, int32(0), reconciledProvisioner.Status.ReadyReplicas)
	require.Equal(t, coderv1alpha1.CoderProvisionerPhasePending, reconciledProvisioner.Status.Phase)
	require.Equal(t, organizationID.String(), reconciledProvisioner.Status.OrganizationID)
	require.Equal(t, provisionerKeyID.String(), reconciledProvisioner.Status.ProvisionerKeyID)
	require.Equal(t, "provisioner-key-name", reconciledProvisioner.Status.ProvisionerKeyName)
	require.NotNil(t, reconciledProvisioner.Status.SecretRef)
	require.Equal(t, provisioner.Spec.Key.SecretName, reconciledProvisioner.Status.SecretRef.Name)
	require.Equal(t, provisioner.Spec.Key.SecretKey, reconciledProvisioner.Status.SecretRef.Key)
}

func TestCoderProvisionerReconciler_ExistingSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-existing")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-existing", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisionerName := "provisioner-existing"
	secretName := fmt.Sprintf("%s-provisioner-key", provisionerName)
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			coderv1alpha1.DefaultProvisionerKeySecretKey: []byte("existing-key-material"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, existingSecret))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), existingSecret)
	})

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: provisionerName, Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
			Image: "provisioner-image:test",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{
			{
				OrganizationID: uuid.New(),
				KeyID:          uuid.New(),
				KeyName:        provisionerName,
				Key:            "", // Empty: coderd returns no plaintext for existing keys.
			},
			{
				OrganizationID: uuid.New(),
				KeyID:          uuid.New(),
				KeyName:        provisionerName,
				Key:            "rotated-key-material",
			},
		},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

	namespacedName := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
	reconcileProvisioner(ctx, t, reconciler, namespacedName)
	reconcileProvisioner(ctx, t, reconciler, namespacedName)

	// The first real reconcile triggers a metadata-only EnsureProvisionerKey
	// call because status.OrganizationName and status.TagsHash are empty.
	// The empty key response rotates by deleting and recreating the key.
	// The second reconcile skips since metadata is now populated.
	require.Equal(t, 2, bootstrapClient.provisionerKeyCalls)
	require.Equal(t, 1, bootstrapClient.deleteKeyCalls)

	reconciledSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, reconciledSecret))
	require.Equal(t, "rotated-key-material", string(reconciledSecret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))

	deployment := &appsv1.Deployment{}
	resourceName := expectedProvisionerResourceName(provisioner.Name)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: namespace}, deployment))
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)

	envByName := make(map[string]corev1.EnvVar, len(deployment.Spec.Template.Spec.Containers[0].Env))
	for _, envVar := range deployment.Spec.Template.Spec.Containers[0].Env {
		envByName[envVar.Name] = envVar
	}
	keyEnv, ok := envByName["CODER_PROVISIONER_DAEMON_KEY"]
	require.True(t, ok)
	require.NotNil(t, keyEnv.ValueFrom)
	require.NotNil(t, keyEnv.ValueFrom.SecretKeyRef)
	require.Equal(t, secretName, keyEnv.ValueFrom.SecretKeyRef.Name)
	require.Equal(t, coderv1alpha1.DefaultProvisionerKeySecretKey, keyEnv.ValueFrom.SecretKeyRef.Key)

	reconciledProvisioner := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, namespacedName, reconciledProvisioner))
	require.Equal(t, provisioner.Name, reconciledProvisioner.Status.ProvisionerKeyName)
	require.NotNil(t, reconciledProvisioner.Status.SecretRef)
	require.Equal(t, secretName, reconciledProvisioner.Status.SecretRef.Name)
	require.Equal(t, coderv1alpha1.DefaultProvisionerKeySecretKey, reconciledProvisioner.Status.SecretRef.Key)
}

func TestCoderProvisionerReconciler_Deletion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-delete")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-delete", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-delete", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
			Image: "provisioner-image:test",
			Key: coderv1alpha1.CoderProvisionerKeySpec{
				Name: "cleanup-key",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{{Key: "provisioner-key-material"}},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

	namespacedName := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
	reconcileProvisioner(ctx, t, reconciler, namespacedName)
	reconcileProvisioner(ctx, t, reconciler, namespacedName)
	require.Equal(t, 1, bootstrapClient.provisionerKeyCalls)

	latest := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, namespacedName, latest))
	require.Contains(t, latest.Finalizers, coderv1alpha1.ProvisionerKeyCleanupFinalizer)

	require.NoError(t, k8sClient.Delete(ctx, latest))
	markedForDeletion := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, namespacedName, markedForDeletion))
	require.False(t, markedForDeletion.DeletionTimestamp.IsZero())

	reconcileProvisioner(ctx, t, reconciler, namespacedName)
	require.Equal(t, 1, bootstrapClient.deleteKeyCalls)

	require.Eventually(t, func() bool {
		reconciled := &coderv1alpha1.CoderProvisioner{}
		err := k8sClient.Get(ctx, namespacedName, reconciled)
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			t.Logf("get reconciled provisioner: %v", err)
			return false
		}

		return !controllerutil.ContainsFinalizer(reconciled, coderv1alpha1.ProvisionerKeyCleanupFinalizer)
	}, 5*time.Second, 100*time.Millisecond)
}

func TestCoderProvisionerReconciler_DeletionControlPlaneGone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-delete-cpgone")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-cpgone", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-cpgone", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
			Image: "provisioner-image:test",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{{Key: "provisioner-key-material"}},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}

	namespacedName := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
	reconcileProvisioner(ctx, t, reconciler, namespacedName)
	reconcileProvisioner(ctx, t, reconciler, namespacedName)

	// Delete the control plane first (common in namespace teardown).
	require.NoError(t, k8sClient.Delete(ctx, controlPlane))

	// Now delete the provisioner — the finalizer should still be removed
	// even though the control plane is gone (best-effort cleanup).
	latest := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, namespacedName, latest))
	require.NoError(t, k8sClient.Delete(ctx, latest))

	reconcileProvisioner(ctx, t, reconciler, namespacedName)

	// DeleteProvisionerKey should NOT have been called since the control
	// plane was already gone.
	require.Equal(t, 0, bootstrapClient.deleteKeyCalls)

	// The finalizer should still be removed.
	require.Eventually(t, func() bool {
		reconciled := &coderv1alpha1.CoderProvisioner{}
		err := k8sClient.Get(ctx, namespacedName, reconciled)
		if apierrors.IsNotFound(err) {
			return true
		}
		if err != nil {
			t.Logf("get reconciled provisioner: %v", err)
			return false
		}

		return !controllerutil.ContainsFinalizer(reconciled, coderv1alpha1.ProvisionerKeyCleanupFinalizer)
	}, 5*time.Second, 100*time.Millisecond)
}

func TestCoderProvisionerReconciler_NotFound(t *testing.T) {
	t.Parallel()

	reconciler := &controller.CoderProvisionerReconciler{
		Client:          k8sClient,
		Scheme:          scheme,
		BootstrapClient: &fakeBootstrapClient{},
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"},
	})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
}

func TestCoderProvisionerReconciler_NilChecks(t *testing.T) {
	t.Parallel()

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"}}

	t.Run("nil client", func(t *testing.T) {
		t.Parallel()

		reconciler := &controller.CoderProvisionerReconciler{
			Client:          nil,
			Scheme:          scheme,
			BootstrapClient: &fakeBootstrapClient{},
		}

		_, err := reconciler.Reconcile(context.Background(), request)
		require.ErrorContains(t, err, "assertion failed: reconciler client must not be nil")
	})

	t.Run("nil scheme", func(t *testing.T) {
		t.Parallel()

		reconciler := &controller.CoderProvisionerReconciler{
			Client:          k8sClient,
			Scheme:          nil,
			BootstrapClient: &fakeBootstrapClient{},
		}

		_, err := reconciler.Reconcile(context.Background(), request)
		require.ErrorContains(t, err, "assertion failed: reconciler scheme must not be nil")
	})

	t.Run("nil bootstrap client", func(t *testing.T) {
		t.Parallel()

		reconciler := &controller.CoderProvisionerReconciler{
			Client:          k8sClient,
			Scheme:          scheme,
			BootstrapClient: nil,
		}

		_, err := reconciler.Reconcile(context.Background(), request)
		require.ErrorContains(t, err, "assertion failed: reconciler bootstrap client must not be nil")
	})
}

func TestCoderProvisionerReconciler_ControlPlaneNotReady(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-cpnotready")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-notready", "")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-notready", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	bootstrapClient := &fakeBootstrapClient{}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
	namespacedName := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

	reconcileProvisioner(ctx, t, reconciler, namespacedName)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: namespacedName})
	require.ErrorContains(t, err, fmt.Sprintf("codercontrolplane %s/%s status.url is empty", controlPlane.Namespace, controlPlane.Name))
	require.Equal(t, ctrl.Result{}, result)
	require.Equal(t, 0, bootstrapClient.provisionerKeyCalls)
}

func TestCoderProvisionerReconciler_RotationOnSecretLoss(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-rotation")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-rotation", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-rotation", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
			Key: coderv1alpha1.CoderProvisionerKeySpec{
				Name:       "rotation-key",
				SecretName: "provisioner-rotation-key",
				SecretKey:  "daemon-key",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{
			{KeyName: "rotation-key", Key: ""},
			{KeyName: "rotation-key", Key: "rotated-key-material"},
			{KeyName: "rotation-key", Key: "rotated-key-material"},
		},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
	request := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

	reconcileProvisioner(ctx, t, reconciler, request)
	reconcileProvisioner(ctx, t, reconciler, request)
	reconcileProvisioner(ctx, t, reconciler, request)

	require.Equal(t, 2, bootstrapClient.provisionerKeyCalls)
	require.Equal(t, 1, bootstrapClient.deleteKeyCalls)
	require.Len(t, bootstrapClient.deleteKeyRequests, 1)
	require.Equal(t, "rotation-key", bootstrapClient.deleteKeyRequests[0].KeyName)

	keySecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: provisioner.Spec.Key.SecretName, Namespace: provisioner.Namespace}, keySecret))
	require.Equal(t, "rotated-key-material", string(keySecret.Data[provisioner.Spec.Key.SecretKey]))
}

func TestCoderProvisionerReconciler_TagsDrift(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-tags-drift")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-tags", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-tags", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
			Key:  coderv1alpha1.CoderProvisionerKeySpec{Name: "tags-drift-key"},
			Tags: map[string]string{"region": "us-east"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{
			{KeyName: "tags-drift-key", Key: "initial-key-material"},
			{KeyName: "tags-drift-key", Key: ""},
			{KeyName: "tags-drift-key", Key: "rotated-key-material"},
		},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
	request := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

	reconcileProvisioner(ctx, t, reconciler, request)
	reconcileProvisioner(ctx, t, reconciler, request)

	before := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, request, before))
	oldTagsHash := before.Status.TagsHash
	require.NotEmpty(t, oldTagsHash)

	before.Spec.Tags = map[string]string{"region": "eu-west"}
	require.NoError(t, k8sClient.Update(ctx, before))

	reconcileProvisioner(ctx, t, reconciler, request)

	require.Equal(t, 3, bootstrapClient.provisionerKeyCalls)
	require.Equal(t, 2, bootstrapClient.deleteKeyCalls)
	require.Len(t, bootstrapClient.deleteKeyRequests, 2)
	require.Equal(t, "tags-drift-key", bootstrapClient.deleteKeyRequests[0].KeyName)
	require.Equal(t, "tags-drift-key", bootstrapClient.deleteKeyRequests[1].KeyName)

	keySecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-provisioner-key", provisioner.Name), Namespace: provisioner.Namespace}, keySecret))
	require.Equal(t, "rotated-key-material", string(keySecret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))

	after := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, request, after))
	require.NotEmpty(t, after.Status.TagsHash)
	require.NotEqual(t, oldTagsHash, after.Status.TagsHash)
}

func TestCoderProvisionerReconciler_KeyNameDrift(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-key-drift")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-key", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-key-drift", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
			Key: coderv1alpha1.CoderProvisionerKeySpec{
				Name:       "key-v1",
				SecretName: "provisioner-key-drift-secret",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{
			{KeyName: "key-v1", Key: "key-v1-material"},
			{KeyName: "key-v2", Key: "key-v2-material"},
		},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
	request := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

	reconcileProvisioner(ctx, t, reconciler, request)
	reconcileProvisioner(ctx, t, reconciler, request)

	updated := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, request, updated))
	updated.Spec.Key.Name = "key-v2"
	require.NoError(t, k8sClient.Update(ctx, updated))

	reconcileProvisioner(ctx, t, reconciler, request)

	require.GreaterOrEqual(t, bootstrapClient.deleteKeyCalls, 1)
	require.NotEmpty(t, bootstrapClient.deleteKeyRequests)
	lastDelete := bootstrapClient.deleteKeyRequests[len(bootstrapClient.deleteKeyRequests)-1]
	require.Equal(t, "key-v1", lastDelete.KeyName)

	require.GreaterOrEqual(t, len(bootstrapClient.provisionerKeyRequests), 2)
	lastEnsure := bootstrapClient.provisionerKeyRequests[len(bootstrapClient.provisionerKeyRequests)-1]
	require.Equal(t, "key-v2", lastEnsure.KeyName)

	keySecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: provisioner.Spec.Key.SecretName, Namespace: provisioner.Namespace}, keySecret))
	require.Equal(t, "key-v2-material", string(keySecret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))

	reconciled := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, request, reconciled))
	require.Equal(t, "key-v2", reconciled.Status.ProvisionerKeyName)
}

func TestCoderProvisionerReconciler_ReadyPhaseAndConditions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-ready")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-ready", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "provisioner-ready", Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{{
			KeyName: provisioner.Name,
			Key:     "provisioner-key-material",
		}},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
	request := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

	reconcileProvisioner(ctx, t, reconciler, request)
	reconcileProvisioner(ctx, t, reconciler, request)

	deployment := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: expectedProvisionerResourceName(provisioner.Name), Namespace: provisioner.Namespace}, deployment))
	deployment.Status.ReadyReplicas = 1
	deployment.Status.Replicas = 1
	require.NoError(t, k8sClient.Status().Update(ctx, deployment))

	reconcileProvisioner(ctx, t, reconciler, request)

	reconciled := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, request, reconciled))
	require.Equal(t, coderv1alpha1.CoderProvisionerPhaseReady, reconciled.Status.Phase)

	requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionDeploymentReady, metav1.ConditionTrue)
	deploymentReadyCondition := findCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionDeploymentReady)
	require.Equal(t, "MinimumReplicasReady", deploymentReadyCondition.Reason)

	requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneReady, metav1.ConditionTrue)
	requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionBootstrapSecretReady, metav1.ConditionTrue)
	requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady, metav1.ConditionTrue)
	requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionProvisionerKeySecretReady, metav1.ConditionTrue)
}

func TestCoderProvisionerReconciler_ConditionsOnFailure(t *testing.T) {
	t.Parallel()

	t.Run("control plane unavailable", func(t *testing.T) {
		ctx := context.Background()
		namespace := createTestNamespace(ctx, t, "coderprov-cond-cp")
		controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-cond-cp", "")
		bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

		provisioner := &coderv1alpha1.CoderProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "provisioner-cond-cp", Namespace: namespace},
			Spec: coderv1alpha1.CoderProvisionerSpec{
				ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
				Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
					CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, provisioner))
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), provisioner)
		})

		bootstrapClient := &fakeBootstrapClient{}
		reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
		request := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

		reconcileProvisioner(ctx, t, reconciler, request)
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: request})
		require.ErrorContains(t, err, "status.url is empty")
		require.Equal(t, ctrl.Result{}, result)

		reconciled := &coderv1alpha1.CoderProvisioner{}
		require.NoError(t, k8sClient.Get(ctx, request, reconciled))
		requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneReady, metav1.ConditionFalse)
		controlPlaneCondition := findCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneReady)
		require.Equal(t, "ControlPlaneUnavailable", controlPlaneCondition.Reason)
	})

	t.Run("bootstrap secret unavailable", func(t *testing.T) {
		ctx := context.Background()
		namespace := createTestNamespace(ctx, t, "coderprov-cond-bootstrap")
		controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-cond-bootstrap", "https://coder.example.com")

		provisioner := &coderv1alpha1.CoderProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "provisioner-cond-bootstrap", Namespace: namespace},
			Spec: coderv1alpha1.CoderProvisionerSpec{
				ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
				Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
					CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: "missing-bootstrap-secret", Key: coderv1alpha1.DefaultTokenSecretKey},
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, provisioner))
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), provisioner)
		})

		bootstrapClient := &fakeBootstrapClient{}
		reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
		request := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

		reconcileProvisioner(ctx, t, reconciler, request)
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: request})
		require.ErrorContains(t, err, "read bootstrap credentials secret")
		require.Equal(t, ctrl.Result{}, result)
		require.Equal(t, 0, bootstrapClient.provisionerKeyCalls)

		reconciled := &coderv1alpha1.CoderProvisioner{}
		require.NoError(t, k8sClient.Get(ctx, request, reconciled))
		requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneReady, metav1.ConditionTrue)
		requireCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionBootstrapSecretReady, metav1.ConditionFalse)
		bootstrapCondition := findCondition(t, reconciled.Status.Conditions, coderv1alpha1.CoderProvisionerConditionBootstrapSecretReady)
		require.Equal(t, "BootstrapSecretUnavailable", bootstrapCondition.Reason)
	})
}

func TestCoderProvisionerReconciler_LongNameTruncation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "coderprov-longname")
	controlPlane := createTestControlPlane(ctx, t, namespace, "controlplane-longname", "https://coder.example.com")
	bootstrapSecret := createBootstrapSecret(ctx, t, namespace, "bootstrap-creds", coderv1alpha1.DefaultTokenSecretKey, "session-token")

	provisionerName := strings.Repeat("a", 180)
	deploymentCandidateName := fmt.Sprintf("provisioner-%s", provisionerName)
	serviceAccountCandidateName := fmt.Sprintf("%s-provisioner", provisionerName)
	keyNameCandidate := provisionerName
	expectedKeyName := expectedProvisionerKeyName(provisionerName)
	require.Greater(t, len(deploymentCandidateName), 63)
	require.Greater(t, len(serviceAccountCandidateName), 63)
	require.Greater(t, len(keyNameCandidate), 128)
	require.Len(t, expectedKeyName, 128)

	provisioner := &coderv1alpha1.CoderProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: provisionerName, Namespace: namespace},
		Spec: coderv1alpha1.CoderProvisionerSpec{
			ControlPlaneRef: corev1.LocalObjectReference{Name: controlPlane.Name},
			Bootstrap: coderv1alpha1.CoderProvisionerBootstrapSpec{
				CredentialsSecretRef: coderv1alpha1.SecretKeySelector{Name: bootstrapSecret.Name, Key: coderv1alpha1.DefaultTokenSecretKey},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provisioner))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), provisioner)
	})

	bootstrapClient := &fakeBootstrapClient{
		provisionerKeyResponses: []coderbootstrap.EnsureProvisionerKeyResponse{{
			KeyName: expectedKeyName,
			Key:     "provisioner-key-material",
		}},
	}
	reconciler := &controller.CoderProvisionerReconciler{Client: k8sClient, Scheme: scheme, BootstrapClient: bootstrapClient}
	request := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}

	reconcileProvisioner(ctx, t, reconciler, request)
	reconcileProvisioner(ctx, t, reconciler, request)

	deploymentName := expectedProvisionerResourceName(provisionerName)
	serviceAccountName := expectedProvisionerServiceAccountName(provisionerName)
	require.LessOrEqual(t, len(deploymentName), 63)
	require.LessOrEqual(t, len(serviceAccountName), 63)

	require.Len(t, bootstrapClient.provisionerKeyRequests, 1)
	require.Equal(t, expectedKeyName, bootstrapClient.provisionerKeyRequests[0].KeyName)

	reconciledProvisioner := &coderv1alpha1.CoderProvisioner{}
	require.NoError(t, k8sClient.Get(ctx, request, reconciledProvisioner))
	require.Equal(t, expectedKeyName, reconciledProvisioner.Status.ProvisionerKeyName)
	require.LessOrEqual(t, len(reconciledProvisioner.Status.ProvisionerKeyName), 128)

	deployment := &appsv1.Deployment{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment))

	serviceAccount := &corev1.ServiceAccount{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: serviceAccountName, Namespace: namespace}, serviceAccount))
}
