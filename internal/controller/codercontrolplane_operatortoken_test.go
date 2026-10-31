package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

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
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
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
			require.Zero(t, provisioner.revokeCalls, "the operator never published a token here, so it has none to revoke")

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

	next := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = next.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	require.NoError(t, err)
	require.Zero(t, provisioner.revokeCalls, "the operator never held this Secret, so it deletes no token")
	require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)
}

// losesOwnerPostgresURL is the database URL with which
// publishThenReplaceOperatorTokenSecret publishes the operator token.
const losesOwnerPostgresURL = "postgres://coder:first-password@example.loses-owner/coder"

// publishThenReplaceOperatorTokenSecret creates a control plane in its own
// namespace and reconciles it once, so that it owns its operator token Secret
// and publishes it in status. It then replaces that Secret with one that has no
// owner, as a user with delete and create rights on Secrets can.
func publishThenReplaceOperatorTokenSecret(
	ctx context.Context,
	t *testing.T,
	prefix string,
) (*coderv1alpha1.CoderControlPlane, *fakeOperatorAccessProvisioner, *coder.ControlPlaneClientProvider) {
	t.Helper()
	published, provisioner, consumer := publishOperatorToken(ctx, t, prefix, losesOwnerPostgresURL)
	replaceOperatorTokenSecret(ctx, t, published)
	return published, provisioner, consumer
}

// publishOperatorToken creates a control plane with postgresURL in its own
// namespace and reconciles it once, so that it owns its operator token Secret
// and publishes it in status.
func publishOperatorToken(
	ctx context.Context,
	t *testing.T,
	prefix string,
	postgresURL string,
) (*coderv1alpha1.CoderControlPlane, *fakeOperatorAccessProvisioner, *coder.ControlPlaneClientProvider) {
	t.Helper()
	ensureGatewaySchemeRegistered(t)

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, prefix)},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "test-operator-token-loses-owner:latest",
			ExtraEnv: []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: postgresURL}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cp) })

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-published"}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	require.NoError(t, err)
	published := getControlPlane(t, cp)
	require.True(t, published.Status.OperatorAccessReady)
	require.NotNil(t, published.Status.OperatorTokenSecretRef)

	consumer, err := coder.NewControlPlaneClientProvider(k8sClient, k8sClient, time.Second)
	require.NoError(t, err)
	sdk, err := consumer.ClientForNamespace(ctx, cp.Namespace)
	require.NoError(t, err)
	require.Equal(t, "operator-token-published", sdk.SessionToken())

	return published, provisioner, consumer
}

// replaceOperatorTokenSecret replaces the operator token Secret of cp with one
// that has no owner, as a user with delete and create rights on Secrets can.
func replaceOperatorTokenSecret(ctx context.Context, t *testing.T, cp *coderv1alpha1.CoderControlPlane) {
	t.Helper()
	secretName := cp.Name + "-operator-token"
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cp.Namespace}}))
	replaced := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cp.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("foreign-token")},
	}
	require.NoError(t, k8sClient.Create(ctx, replaced))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), replaced) })
}

// updateControlPlaneSpec applies mutate to the stored spec of cp.
func updateControlPlaneSpec(ctx context.Context, t *testing.T, cp *coderv1alpha1.CoderControlPlane, mutate func(*coderv1alpha1.CoderControlPlaneSpec)) {
	t.Helper()
	latest := getControlPlane(t, cp)
	mutate(&latest.Spec)
	require.NoError(t, k8sClient.Update(ctx, latest))
}

// requireOneRevoke checks that the operator deleted, once and with postgresURL,
// the token that it provisioned for its first request.
func requireOneRevoke(t *testing.T, provisioner *fakeOperatorAccessProvisioner, postgresURL string) {
	t.Helper()
	require.NotEmpty(t, provisioner.requests)
	require.Equal(t, []coderbootstrap.RevokeOperatorTokenRequest{{
		PostgresURL:      postgresURL,
		OperatorUsername: provisioner.requests[0].OperatorUsername,
		TokenName:        provisioner.requests[0].TokenName,
	}}, provisioner.revokeRequests)
}

// requireNoOperatorClient checks that consumers of the control plane status
// get no Coder client, and never see the replaced Secret's value.
func requireNoOperatorClient(ctx context.Context, t *testing.T, consumer *coder.ControlPlaneClientProvider, namespace, wantMessage string) {
	t.Helper()
	sdk, err := consumer.ClientForNamespace(ctx, namespace)
	require.Nil(t, sdk)
	require.True(t, apierrors.IsServiceUnavailable(err), "got %v", err)
	require.Contains(t, err.Error(), wantMessage)
	require.NotContains(t, err.Error(), "foreign-token")
}

// TestReconcile_OperatorAccess_TokenSecretLosesOwner checks the transition from
// an owned to an unowned operator token Secret. Consumers refuse the replaced
// Secret before the control plane reconciles again. That reconcile deletes the
// operator token in Coder once, clears status.operatorTokenSecretRef and keeps
// consumers off operator access, also after a restart of the operator.
func TestReconcile_OperatorAccess_TokenSecretLosesOwner(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, consumer := publishThenReplaceOperatorTokenSecret(ctx, t, "operator-token-loses-owner")
	requireNoOperatorClient(ctx, t, consumer, cp.Namespace, "not the controller owner")

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	result, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)

	require.Equal(t, []coderbootstrap.RevokeOperatorTokenRequest{{
		PostgresURL:      losesOwnerPostgresURL,
		OperatorUsername: provisioner.requests[0].OperatorUsername,
		TokenName:        provisioner.requests[0].TokenName,
	}}, provisioner.revokeRequests, "the operator deletes the token that it provisioned")
	reconciled := getControlPlane(t, cp)
	require.False(t, reconciled.Status.OperatorAccessReady)
	require.Nil(t, reconciled.Status.OperatorTokenSecretRef)
	condition := apimeta.FindStatusCondition(reconciled.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "TokenSecretNotOwned", condition.Reason)
	require.NotContains(t, condition.Message, "has not deleted")
	requireNoOperatorClient(ctx, t, consumer, cp.Namespace, "no eligible CoderControlPlane")

	restarted := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, provisioner.revokeCalls, "the operator deletes the token once")
	require.Equal(t, 1, provisioner.calls, "the operator provisions no token while the Secret is unowned")
}

// TestReconcile_OperatorAccess_TokenSecretLosesOwner_RetriesRevoke checks that
// a failed delete in Coder keeps status.operatorTokenSecretRef as the record
// of the pending delete, with operator access off, and that the operator
// retries until the delete succeeds.
func TestReconcile_OperatorAccess_TokenSecretLosesOwner_RetriesRevoke(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, consumer := publishThenReplaceOperatorTokenSecret(ctx, t, "operator-token-revoke-retry")
	provisioner.revokeErr = errors.New("coder database unavailable")

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	result, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.Equal(t, 1, provisioner.revokeCalls)

	pending := getControlPlane(t, cp)
	require.False(t, pending.Status.OperatorAccessReady)
	require.Equal(t, cp.Status.OperatorTokenSecretRef, pending.Status.OperatorTokenSecretRef)
	condition := apimeta.FindStatusCondition(pending.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "TokenSecretNotOwned", condition.Reason)
	require.Contains(t, condition.Message, "has not deleted its earlier operator token in Coder yet")
	require.NotContains(t, condition.Message, "coder database unavailable")
	requireNoOperatorClient(ctx, t, consumer, cp.Namespace, "no eligible CoderControlPlane")

	provisioner.revokeErr = nil
	restarted := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, provisioner.revokeCalls)
	require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)

	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, provisioner.revokeCalls, "the operator stops once the delete succeeded")
}

// TestReconcile_OperatorAccess_TokenSecretLosesOwner_DatabaseChanged checks
// that the operator deletes its earlier operator token only in the database
// where it created that token. While the database URL names another database,
// the operator keeps status.operatorTokenSecretRef as the record and says that
// the token can still work. A new password for the same database is the same
// database.
func TestReconcile_OperatorAccess_TokenSecretLosesOwner_DatabaseChanged(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, consumer := publishThenReplaceOperatorTokenSecret(ctx, t, "operator-token-database-changed")
	setPostgresURL := func(postgresURL string) {
		latest := getControlPlane(t, cp)
		latest.Spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: postgresURL}}
		require.NoError(t, k8sClient.Update(ctx, latest))
	}
	provisioner.revokeErr = errors.New("coder database unavailable")

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, provisioner.revokeCalls)

	setPostgresURL("postgres://coder:first-password@example.other-database/coder")
	provisioner.revokeErr = nil
	restarted := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, provisioner.revokeCalls, "the operator must not delete a token in a database where it did not create it")

	refused := getControlPlane(t, cp)
	require.False(t, refused.Status.OperatorAccessReady)
	require.Equal(t, cp.Status.OperatorTokenSecretRef, refused.Status.OperatorTokenSecretRef)
	condition := apimeta.FindStatusCondition(refused.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned)
	require.NotNil(t, condition)
	require.Equal(t, "TokenSecretNotOwned", condition.Reason)
	require.Contains(t, condition.Message, "does not delete its earlier operator token")
	require.Contains(t, condition.Message, provisioner.requests[0].TokenName)
	requireNoOperatorClient(ctx, t, consumer, cp.Namespace, "no eligible CoderControlPlane")

	rotated := "postgres://coder:second-password@example.loses-owner/coder"
	setPostgresURL(rotated)
	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Len(t, provisioner.revokeRequests, 2)
	require.Equal(t, rotated, provisioner.revokeRequests[1].PostgresURL)
	require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)
}

// TestReconcile_OperatorAccess_TokenSecretLosesOwner_DatabaseNotRecorded checks
// a status that an earlier version wrote: it names the operator token Secret
// but records no database. The operator cannot tell in which database it
// created that token, so it deletes none and keeps the record.
func TestReconcile_OperatorAccess_TokenSecretLosesOwner_DatabaseNotRecorded(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, _ := publishThenReplaceOperatorTokenSecret(ctx, t, "operator-token-database-not-recorded")
	earlierVersionStatus := ctrlclient.RawPatch(types.MergePatchType, []byte(`{"status":{"operatorTokenDatabaseHash":null}}`))
	require.NoError(t, k8sClient.Status().Patch(ctx, cp.DeepCopy(), earlierVersionStatus))

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Zero(t, provisioner.revokeCalls, "the operator must not delete a token in a database that it did not record")

	stored := getControlPlane(t, cp)
	require.False(t, stored.Status.OperatorAccessReady)
	require.Equal(t, cp.Status.OperatorTokenSecretRef, stored.Status.OperatorTokenSecretRef)
	condition := apimeta.FindStatusCondition(stored.Status.Conditions, coderv1alpha1.CoderControlPlaneConditionOperatorTokenSecretOwned)
	require.NotNil(t, condition)
	require.Contains(t, condition.Message, "does not delete its earlier operator token")
}

// TestReconcile_OperatorAccess_TokenSecretLosesOwner_StatusWriteFails checks
// that a failed status write after the delete keeps the record. The next
// reconcile deletes again in the same database, which removes nothing more,
// and then clears the record.
func TestReconcile_OperatorAccess_TokenSecretLosesOwner_StatusWriteFails(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, _ := publishThenReplaceOperatorTokenSecret(ctx, t, "operator-token-status-write-fails")
	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	statusOutage := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, ctrlclient.Client, string, ctrlclient.Object, ...ctrlclient.SubResourceUpdateOption) error {
			return errors.New("status update unavailable")
		},
	})

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: statusOutage, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = r.Reconcile(ctx, request)
	require.ErrorContains(t, err, "status update unavailable")
	require.Equal(t, 1, provisioner.revokeCalls)
	require.Equal(t, cp.Status.OperatorTokenSecretRef, getControlPlane(t, cp).Status.OperatorTokenSecretRef)

	restarted := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, provisioner.revokeCalls, "the record survived the failed status write, so the operator deletes again")
	require.Equal(t, provisioner.revokeRequests[0], provisioner.revokeRequests[1])
	require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)
}

// TestReconcile_OperatorAccess_RecordsTokenBeforeCreate checks that status
// names the operator token Secret and the database before the operator can
// create a token in Coder. So when the operator stops after the create, before
// its last status write, and someone then replaces the Secret, the next
// reconcile still deletes that token.
func TestReconcile_OperatorAccess_RecordsTokenBeforeCreate(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, "operator-token-record-first")},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "test-operator-token-record-first:latest",
			ExtraEnv: []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: losesOwnerPostgresURL}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cp) })

	provisioner := &fakeOperatorAccessProvisioner{token: "operator-token-published"}
	failStatusWrite := func() bool { return true }
	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	statusOutage := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
			if failStatusWrite() {
				return errors.New("status update unavailable")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: statusOutage, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	_, err = r.Reconcile(ctx, request)
	require.ErrorContains(t, err, "status update unavailable")
	require.Zero(t, provisioner.calls, "the operator must not create a token that status does not record")

	failStatusWrite = func() bool { return provisioner.calls > 0 }
	_, err = r.Reconcile(ctx, request)
	require.ErrorContains(t, err, "status update unavailable")
	require.Equal(t, 1, provisioner.calls)
	recorded := getControlPlane(t, cp)
	require.False(t, recorded.Status.OperatorAccessReady)
	require.NotNil(t, recorded.Status.OperatorTokenSecretRef)
	require.Equal(t, cp.Name+"-operator-token", recorded.Status.OperatorTokenSecretRef.Name)
	require.NotEmpty(t, recorded.Status.OperatorTokenDatabaseHash)

	replaceOperatorTokenSecret(ctx, t, cp)
	restarted := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	requireOneRevoke(t, provisioner, losesOwnerPostgresURL)
	require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)
}

// TestReconcile_OperatorAccess_RecordsTokenBeforeCreate_StaleCache checks that
// the operator stores the record before a create also when its cache still
// shows a record that a delete already removed from the API server.
func TestReconcile_OperatorAccess_RecordsTokenBeforeCreate_StaleCache(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, _ := publishThenReplaceOperatorTokenSecret(ctx, t, "operator-token-record-stale-cache")
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	requireOneRevoke(t, provisioner, losesOwnerPostgresURL)
	require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cp.Name + "-operator-token", Namespace: cp.Namespace}}))

	stale := cp.Status.DeepCopy()
	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	staleCache := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if read, ok := obj.(*coderv1alpha1.CoderControlPlane); ok {
				stale.DeepCopyInto(&read.Status)
			}
			return nil
		},
		SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
			if provisioner.calls > 1 {
				return errors.New("status update unavailable")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	cached := &controller.CoderControlPlaneReconciler{Client: staleCache, APIReader: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	if _, err = cached.Reconcile(ctx, request); err != nil {
		require.ErrorContains(t, err, "status update unavailable", "only a status write after the create may fail")
	}
	require.Equal(t, 2, provisioner.calls)
	recorded := getControlPlane(t, cp)
	require.Equal(t, cp.Status.OperatorTokenSecretRef, recorded.Status.OperatorTokenSecretRef, "the API server must hold the record of the new token")
	require.Equal(t, cp.Status.OperatorTokenDatabaseHash, recorded.Status.OperatorTokenDatabaseHash)
}

// TestReconcile_OperatorAccess_SecretRenameKeepsPublishedRef checks that a new
// spec.operatorAccess.generatedTokenSecretName reaches status only together
// with its token. Until then consumers keep the published Secret, also when the
// operator stops after the create.
func TestReconcile_OperatorAccess_SecretRenameKeepsPublishedRef(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, consumer := publishOperatorToken(ctx, t, "operator-token-secret-rename", losesOwnerPostgresURL)
	updateControlPlaneSpec(ctx, t, cp, func(spec *coderv1alpha1.CoderControlPlaneSpec) {
		spec.OperatorAccess.GeneratedTokenSecretName = "renamed-operator-token"
	})
	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	stopAfterCreate := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
			if provisioner.calls > 1 {
				return errors.New("status update unavailable")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: stopAfterCreate, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = r.Reconcile(ctx, request)
	require.ErrorContains(t, err, "status update unavailable")
	require.Equal(t, 2, provisioner.calls)
	require.Equal(t, cp.Status.OperatorTokenSecretRef, getControlPlane(t, cp).Status.OperatorTokenSecretRef,
		"the published reference stays until the new Secret holds a token")
	sdk, err := consumer.ClientForNamespace(ctx, cp.Namespace)
	require.NoError(t, err)
	require.Equal(t, "operator-token-published", sdk.SessionToken())

	restarted := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err = restarted.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "renamed-operator-token", getControlPlane(t, cp).Status.OperatorTokenSecretRef.Name)
}

// TestReconcile_OperatorAccess_ErrorKeepsTokenRecord checks that a failed
// token check turns operator access off but keeps the record of the published
// token. So when someone replaces the Secret afterwards, the operator still
// deletes that token in Coder.
func TestReconcile_OperatorAccess_ErrorKeepsTokenRecord(t *testing.T) {
	tests := []struct {
		name    string
		fail    func(*testing.T, *coderv1alpha1.CoderControlPlane, *fakeOperatorAccessProvisioner)
		recover func(*testing.T, *coderv1alpha1.CoderControlPlane, *fakeOperatorAccessProvisioner)
	}{
		{
			name: "postgres error",
			fail: func(_ *testing.T, _ *coderv1alpha1.CoderControlPlane, provisioner *fakeOperatorAccessProvisioner) {
				provisioner.err = errors.New("dial tcp: connection refused")
			},
			recover: func(_ *testing.T, _ *coderv1alpha1.CoderControlPlane, provisioner *fakeOperatorAccessProvisioner) {
				provisioner.err = nil
			},
		},
		{
			name: "database URL not resolvable",
			fail: func(t *testing.T, cp *coderv1alpha1.CoderControlPlane, _ *fakeOperatorAccessProvisioner) {
				updateControlPlaneSpec(context.Background(), t, cp, func(spec *coderv1alpha1.CoderControlPlaneSpec) {
					spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "missing-database-url"}, Key: "url"},
					}}}
				})
			},
			recover: func(t *testing.T, cp *coderv1alpha1.CoderControlPlane, _ *fakeOperatorAccessProvisioner) {
				updateControlPlaneSpec(context.Background(), t, cp, func(spec *coderv1alpha1.CoderControlPlaneSpec) {
					spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: losesOwnerPostgresURL}}
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			cp, provisioner, consumer := publishOperatorToken(ctx, t, "operator-token-error-keeps-record", losesOwnerPostgresURL)
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
			r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

			tt.fail(t, cp, provisioner)
			result, err := r.Reconcile(ctx, request)
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter)
			failed := getControlPlane(t, cp)
			require.False(t, failed.Status.OperatorAccessReady)
			require.Equal(t, cp.Status.OperatorTokenSecretRef, failed.Status.OperatorTokenSecretRef, "the record of the published token stays")
			require.Equal(t, cp.Status.OperatorTokenDatabaseHash, failed.Status.OperatorTokenDatabaseHash)
			requireNoOperatorClient(ctx, t, consumer, cp.Namespace, "no eligible CoderControlPlane")

			replaceOperatorTokenSecret(ctx, t, cp)
			tt.recover(t, cp, provisioner)
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			requireOneRevoke(t, provisioner, losesOwnerPostgresURL)
			require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)
		})
	}
}

// TestReconcile_OperatorAccess_Disabled_DatabaseChanged checks that disabling
// operator access deletes the operator token by name only in the database
// where the operator created it, as the delete after a replaced Secret does.
// In another database it deletes only the exact token in the owned Secret.
func TestReconcile_OperatorAccess_Disabled_DatabaseChanged(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, _ := publishOperatorToken(ctx, t, "operator-token-disabled-database-changed", losesOwnerPostgresURL)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}

	otherDatabase := "postgres://coder:first-password@example.other-database/coder"
	updateControlPlaneSpec(ctx, t, cp, func(spec *coderv1alpha1.CoderControlPlaneSpec) {
		spec.OperatorAccess.Disabled = true
		spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: otherDatabase}}
	})
	result, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.Equal(t, []coderbootstrap.RevokeOperatorTokenRequest{{
		PostgresURL:      otherDatabase,
		OperatorUsername: provisioner.requests[0].OperatorUsername,
		TokenName:        provisioner.requests[0].TokenName,
		Token:            "operator-token-published",
	}}, provisioner.revokeRequests, "the operator must not delete a token by name in a database where it did not create it")
	provisioner.revokeRequests, provisioner.revokeCalls = nil, 0
	refused := getControlPlane(t, cp)
	require.False(t, refused.Status.OperatorAccessReady)
	require.Equal(t, cp.Status.OperatorTokenSecretRef, refused.Status.OperatorTokenSecretRef)
	require.Equal(t, cp.Status.OperatorTokenDatabaseHash, refused.Status.OperatorTokenDatabaseHash)
	err = k8sClient.Get(ctx, types.NamespacedName{Name: cp.Name + "-operator-token", Namespace: cp.Namespace}, &corev1.Secret{})
	require.True(t, apierrors.IsNotFound(err), "disabling deletes the owned Secret, got %v", err)

	rotated := "postgres://coder:second-password@example.loses-owner/coder"
	updateControlPlaneSpec(ctx, t, cp, func(spec *coderv1alpha1.CoderControlPlaneSpec) {
		spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: rotated}}
	})
	result, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	requireOneRevoke(t, provisioner, rotated)
	cleared := getControlPlane(t, cp)
	require.Nil(t, cleared.Status.OperatorTokenSecretRef)
	require.Empty(t, cleared.Status.OperatorTokenDatabaseHash)
}

// TestReconcile_OperatorAccess_TokenSecretLosesOwner_KeywordValueDSN checks
// that a key=value PostgreSQL connection string records the database like a
// URL does, so that the operator deletes its token after the Secret was
// replaced, also with a new password.
func TestReconcile_OperatorAccess_TokenSecretLosesOwner_KeywordValueDSN(t *testing.T) {
	ctx := context.Background()
	cp, provisioner, _ := publishOperatorToken(ctx, t, "operator-token-keyword-value-dsn",
		"host=example.keyword-value port=5432 user=coder password='first password' dbname=coder sslmode=disable")
	require.NotEmpty(t, cp.Status.OperatorTokenDatabaseHash)
	replaceOperatorTokenSecret(ctx, t, cp)

	rotated := "host=example.keyword-value port=5432 user=coder password=second-password dbname=coder sslmode=disable"
	updateControlPlaneSpec(ctx, t, cp, func(spec *coderv1alpha1.CoderControlPlaneSpec) {
		spec.ExtraEnv = []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: rotated}}
	})
	r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme, OperatorAccessProvisioner: provisioner}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}})
	require.NoError(t, err)
	requireOneRevoke(t, provisioner, rotated)
	require.Nil(t, getControlPlane(t, cp).Status.OperatorTokenSecretRef)
}

// TestReconcile_OperatorAccess_TokenSecretReplacedDuringReconcile checks that
// the license upload and the entitlements check do not send the value of an
// operator token Secret that someone replaced after the ownership check of the
// same reconcile.
func TestReconcile_OperatorAccess_TokenSecretReplacedDuringReconcile(t *testing.T) {
	ensureGatewaySchemeRegistered(t)
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "operator-token-replaced-mid-reconcile")

	license := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-license", Namespace: namespace},
		Data:       map[string][]byte{coderv1alpha1.DefaultLicenseSecretKey: []byte("license-jwt")},
	}
	require.NoError(t, k8sClient.Create(ctx, license))
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: namespace},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			ExtraEnv:         []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: "postgres://example.replaced-mid-reconcile/coder"}},
			LicenseSecretRef: &coderv1alpha1.SecretKeySelector{Name: license.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cp))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cp) })

	replaceAfterFirstRead, tokenReads := false, 0
	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	racing := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			secret, ok := obj.(*corev1.Secret)
			if !ok || key.Name != cp.Name+"-operator-token" || !replaceAfterFirstRead {
				return nil
			}
			if tokenReads++; tokenReads > 1 {
				secret.OwnerReferences = nil
				secret.Data = map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("foreign-token")}
			}
			return nil
		},
	})
	uploader := &fakeLicenseUploader{}
	inspector := &fakeEntitlementsInspector{}
	r := &controller.CoderControlPlaneReconciler{
		Client:                    racing,
		Scheme:                    scheme,
		OperatorAccessProvisioner: &fakeOperatorAccessProvisioner{token: "operator-token-published"},
		LicenseUploader:           uploader,
		EntitlementsInspector:     inspector,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace}}
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	deployment := getControlPlaneDeployment(t, cp)
	deployment.Status.Replicas, deployment.Status.ReadyReplicas = 1, 1
	require.NoError(t, k8sClient.Status().Update(ctx, deployment))

	replaceAfterFirstRead = true
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Greater(t, tokenReads, 1, "the test must replace the Secret before a later read")
	require.Empty(t, uploader.calls, "the license upload must not use the replaced Secret")
	require.Empty(t, inspector.requests, "the entitlements check must not use the replaced Secret")
	condition := apimeta.FindStatusCondition(getControlPlane(t, cp).Status.Conditions, coderv1alpha1.CoderControlPlaneConditionLicenseApplied)
	require.NotNil(t, condition)
	require.Equal(t, "SecretMissing", condition.Reason)

	replaceAfterFirstRead = false
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Len(t, uploader.calls, 1)
	require.Equal(t, "operator-token-published", uploader.calls[0].sessionToken)
	require.NotEmpty(t, inspector.requests)
	for _, call := range inspector.requests {
		require.Equal(t, "operator-token-published", call.sessionToken)
	}
}
