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
	ensureGatewaySchemeRegistered(t)

	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: createTestNamespace(ctx, t, prefix)},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			Image:    "test-operator-token-loses-owner:latest",
			ExtraEnv: []corev1.EnvVar{{Name: "CODER_PG_CONNECTION_URL", Value: losesOwnerPostgresURL}},
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

	secretName := published.Status.OperatorTokenSecretRef.Name
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cp.Namespace}}))
	replaced := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cp.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{coderv1alpha1.DefaultTokenSecretKey: []byte("foreign-token")},
	}
	require.NoError(t, k8sClient.Create(ctx, replaced))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), replaced) })

	return published, provisioner, consumer
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
