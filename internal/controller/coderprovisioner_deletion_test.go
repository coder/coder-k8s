package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// TestCoderProvisionerDeletion_NoRecordedKeySkipsCoder checks that a
// provisioner that never applied a key releases its finalizer without any
// Coder call, so it cannot delete a Coder key that has its spec key name.
func TestCoderProvisionerDeletion_NoRecordedKeySkipsCoder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, f provisionerCredentialFixture) []corev1.EnvVar
	}{
		{
			name: "refused credentials",
			setup: func(t *testing.T, f provisionerCredentialFixture) []corev1.EnvVar {
				createSecret(context.Background(), t, tokenSecret(metav1.ObjectMeta{Name: "extra", Namespace: f.namespace}, "value", testExtraEnvValue))
				return []corev1.EnvVar{secretEnv("FROM_SECRET", "extra", "value")}
			},
		},
		{
			name: "not entitled",
			setup: func(_ *testing.T, f provisionerCredentialFixture) []corev1.EnvVar {
				f.bootstrapClient.entitlementsResponse.Features = map[codersdk.FeatureName]codersdk.Feature{
					codersdk.FeatureExternalProvisionerDaemons: {Entitlement: codersdk.EntitlementNotEntitled},
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-delete-unrecorded")
			provisioner := f.createProvisioner(ctx, t, tt.setup(t, f), nil)
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}}

			for range 2 {
				_, err := f.reconciler.Reconcile(ctx, request)
				require.NoError(t, err)
			}
			latest := f.get(ctx, t, provisioner)
			require.Contains(t, latest.Finalizers, coderv1alpha1.ProvisionerKeyCleanupFinalizer)
			require.Empty(t, latest.Status.ProvisionerKeyID, "the provisioner must not have applied a key")
			require.Zero(t, f.bootstrapClient.provisionerKeyCalls)

			require.NoError(t, k8sClient.Delete(ctx, latest))
			_, err := f.reconciler.Reconcile(ctx, request)
			require.NoError(t, err)

			require.Zero(t, f.bootstrapClient.deleteKeyCalls, "no name-based key deletion")
			require.Empty(t, f.bootstrapClient.deleteKeyByIDRequests, "no key deletion without a recorded key")
			requireProvisionerGone(ctx, t, request.NamespacedName)
		})
	}
}

// TestCoderProvisionerDeletion_DeletesRecordedKey checks that deletion targets
// the organization and key IDs that status records, not the spec names.
func TestCoderProvisionerDeletion_DeletesRecordedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-delete-recorded")
	recorded := f.bootstrapClient.provisionerKeyResponses[0]
	provisioner := f.createProvisioner(ctx, t, nil, nil)
	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)

	latest := f.get(ctx, t, provisioner)
	require.Equal(t, recorded.OrganizationID.String(), latest.Status.OrganizationID)
	require.Equal(t, recorded.KeyID.String(), latest.Status.ProvisionerKeyID)

	// A later spec change that never applied must not redirect the deletion.
	latest.Spec.Key.Name = "other-key-name"
	latest.Spec.OrganizationName = "other-org"
	require.NoError(t, k8sClient.Update(ctx, latest))
	require.NoError(t, k8sClient.Delete(ctx, latest))
	f.reconcile(ctx, t, provisioner)

	require.Zero(t, f.bootstrapClient.deleteKeyCalls, "no name-based key deletion")
	require.Len(t, f.bootstrapClient.deleteKeyByIDRequests, 1)
	deleted := f.bootstrapClient.deleteKeyByIDRequests[0]
	require.Equal(t, recorded.OrganizationID, deleted.OrganizationID)
	require.Equal(t, recorded.KeyID, deleted.KeyID)
	require.Equal(t, testCoderOrigin, deleted.CoderURL)
	require.Equal(t, testOperatorToken, deleted.SessionToken)
	requireProvisionerGone(ctx, t, types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace})
}

func requireProvisionerGone(ctx context.Context, t *testing.T, name types.NamespacedName) {
	t.Helper()
	require.Eventually(t, func() bool {
		latest := &coderv1alpha1.CoderProvisioner{}
		err := k8sClient.Get(ctx, name, latest)
		if apierrors.IsNotFound(err) {
			return true
		}
		return err == nil && !controllerutil.ContainsFinalizer(latest, coderv1alpha1.ProvisionerKeyCleanupFinalizer)
	}, 5*time.Second, 100*time.Millisecond)
}
