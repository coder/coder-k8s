package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// TestReconcile_OperatorAccess_UnownedTokenSecret checks that the operator
// does not read, write or adopt an operator token Secret that this control
// plane does not control, and that it creates the Secret once it is gone.
func TestReconcile_OperatorAccess_UnownedTokenSecret(t *testing.T) {
	ensureGatewaySchemeRegistered(t)

	tests := []struct {
		name            string
		configuredName  string
		ownerReferences func(cp *coderv1alpha1.CoderControlPlane) []metav1.OwnerReference
		wantMessage     string
	}{
		{
			name:        "no owner",
			wantMessage: "(the default operator token Secret) exists and is not owned by this control plane",
		},
		{
			name: "same name, other UID",
			ownerReferences: func(cp *coderv1alpha1.CoderControlPlane) []metav1.OwnerReference {
				return []metav1.OwnerReference{{
					APIVersion: coderv1alpha1.GroupVersion.String(),
					Kind:       "CoderControlPlane",
					Name:       cp.Name,
					UID:        types.UID("00000000-0000-0000-0000-000000000227"),
					Controller: ptrTo(true),
				}}
			},
			wantMessage: "(the default operator token Secret) exists and is not owned by this control plane",
		},
		{
			name:           "configured name",
			configuredName: "foreign-app-token",
			wantMessage:    "named in spec.operatorAccess.generatedTokenSecretName exists and is not owned by this control plane",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			testNamespace := createTestNamespace(ctx, t, "test-cp")
			cp := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "test-operator-token-unowned-" + string(rune('a'+i)), Namespace: testNamespace},
				Spec: coderv1alpha1.CoderControlPlaneSpec{
					Image:          "test-operator-token-unowned:latest",
					ExtraEnv:       []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: "postgres://example.unowned/coder"}},
					OperatorAccess: coderv1alpha1.OperatorAccessSpec{GeneratedTokenSecretName: tt.configuredName},
				},
			}
			require.NoError(t, k8sClient.Create(ctx, cp))
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, cp) })

			secretName := cp.Name + "-operator-token"
			if tt.configuredName != "" {
				secretName = tt.configuredName
			}
			foreign := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cp.Namespace},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("foreign-value")},
			}
			if tt.ownerReferences != nil {
				foreign.OwnerReferences = tt.ownerReferences(cp)
			}
			require.NoError(t, k8sClient.Create(ctx, foreign))
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, foreign) })
			foreignBefore := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cp.Namespace}, foreignBefore))

			provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-created"}
			r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}

			result, err := r.Reconcile(ctx, request)
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter, "an unowned Secret sends no event, so the reconciler must poll")
			deploymentBefore := getControlPlaneDeployment(t, cp)

			// A second pass must not change the Deployment either.
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			require.Zero(t, provisioner.calls, "the operator must not provision a token while the Secret is unowned")

			foreignAfter := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cp.Namespace}, foreignAfter))
			require.Equal(t, foreignBefore.ResourceVersion, foreignAfter.ResourceVersion, "the operator must not write the unowned Secret")
			require.Equal(t, foreignBefore.OwnerReferences, foreignAfter.OwnerReferences)
			require.Equal(t, deploymentBefore.ResourceVersion, getControlPlaneDeployment(t, cp).ResourceVersion)

			reconciled := getControlPlane(t, cp)
			require.False(t, reconciled.Status.OperatorAccessReady)
			require.Nil(t, reconciled.Status.OperatorTokenSecretRef)
			condition := apimeta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned)
			require.NotNil(t, condition)
			require.Equal(t, metav1.ConditionFalse, condition.Status)
			require.Equal(t, "TokenSecretNotOwned", condition.Reason)
			require.Contains(t, condition.Message, tt.wantMessage)
			require.Contains(t, condition.Message, secretName)
			require.NotContains(t, condition.Message, "foreign-value")

			// Recovery: once the Secret is gone, the operator creates its own.
			require.NoError(t, k8sClient.Delete(ctx, foreign))
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			require.Equal(t, 1, provisioner.calls)
			require.Empty(t, provisioner.requests[0].ExistingToken)

			created := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cp.Namespace}, created))
			assertSingleControllerOwnerReference(t, created.OwnerReferences, cp.Name)
			require.Equal(t, cp.UID, created.OwnerReferences[0].UID)
			require.Equal(t, "operator-token-created", string(created.Data[coderv1alpha1.DefaultTokenSecretKey]))

			recovered := getControlPlane(t, cp)
			require.True(t, recovered.Status.OperatorAccessReady)
			require.NotNil(t, recovered.Status.OperatorTokenSecretRef)
			require.Equal(t, secretName, recovered.Status.OperatorTokenSecretRef.Name)
			condition = apimeta.FindStatusCondition(recovered.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned)
			require.NotNil(t, condition)
			require.Equal(t, metav1.ConditionTrue, condition.Status)
		})
	}
}

// TestReconcile_OperatorAccess_OwnedTokenSecretIsReused checks the upgrade
// path: a Secret that an earlier version created with a controller reference
// keeps working, and its token goes to the provisioner for validation.
func TestReconcile_OperatorAccess_OwnedTokenSecretIsReused(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	testNamespace := createTestNamespace(ctx, t, "test-cp")

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator-token-owned-reuse", Namespace: testNamespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "test-operator-token-owned:latest",
			ExtraEnv: []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: "postgres://example.owned/coder"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cp) })

	secretName := cp.Name + "-operator-token"
	owned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cp.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         coderv1alpha1.GroupVersion.String(),
				Kind:               "CoderControlPlane",
				Name:               cp.Name,
				UID:                cp.UID,
				Controller:         ptrTo(true),
				BlockOwnerDeletion: ptrTo(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("existing-operator-token")},
	}
	require.NoError(t, k8sClient.Create(ctx, owned))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, owned) })

	provisioner := &fakeOperatorAccessProvisioner{token: "existing-operator-token"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	require.NoError(t, err)

	require.Equal(t, 1, provisioner.calls)
	require.Equal(t, "existing-operator-token", provisioner.requests[0].ExistingToken)
	reconciled := getControlPlane(t, cp)
	require.True(t, reconciled.Status.OperatorAccessReady)
	condition := apimeta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionTrue, condition.Status)

	// Disabling operator access removes the condition.
	latest := getControlPlane(t, cp)
	latest.Spec.OperatorAccess.Disabled = true
	require.NoError(t, k8sClient.Update(ctx, latest))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	require.NoError(t, err)
	require.Nil(t, apimeta.FindStatusCondition(getControlPlane(t, cp).Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned))
	err = k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cp.Namespace}, &corev1.Secret{})
	require.True(t, apierrors.IsNotFound(err), "disabling deletes the owned Secret, got %v", err)
}

// TestReconcile_OperatorAccess_TokenSecretCreateRace checks that a Secret
// that appears between the read and the create stops the reconcile before the
// operator changes the token in Coder.
func TestReconcile_OperatorAccess_TokenSecretCreateRace(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	testNamespace := createTestNamespace(ctx, t, "test-cp")

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator-token-create-race", Namespace: testNamespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "test-operator-token-race:latest",
			ExtraEnv: []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: "postgres://example.race/coder"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cp) })

	secretName := cp.Name + "-operator-token"
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cp.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("foreign-value")},
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	racing := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && obj.GetName() == secretName {
				// Another actor wins the race for the name.
				require.NoError(t, c.Create(ctx, foreign.DeepCopy()))
			}
			return c.Create(ctx, obj, opts...)
		},
	})

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-race"}
	r := &controller.CoderControlPlaneReconciler{Client: racing, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	require.Error(t, err)
	require.True(t, apierrors.IsAlreadyExists(err), "got %v", err)
	require.Zero(t, provisioner.calls, "the operator must not change the token in Coder before it holds the Secret")

	stored := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cp.Namespace}, stored))
	require.Empty(t, stored.OwnerReferences)
	require.Equal(t, "foreign-value", string(stored.Data[coderv1alpha1.DefaultTokenSecretKey]))
}
