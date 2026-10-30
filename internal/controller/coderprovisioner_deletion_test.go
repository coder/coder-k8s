package controller_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
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
	require.Equal(t, internalCoderURL(f), deleted.CoderURL, "status.url is HTTPS, so the call uses the internal HTTP URL")
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

// TestCoderProvisionerKey_RecordedBeforePublished checks that a new key is in
// status before its plaintext reaches the Secret, and that a failed status
// write revokes the new key and publishes nothing.
func TestCoderProvisionerKey_RecordedBeforePublished(t *testing.T) {
	t.Parallel()

	for _, failStatus := range []bool{false, true} {
		name := "status written first"
		if failStatus {
			name = "status write fails"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-key-order")
			created := f.bootstrapClient.provisionerKeyResponses[0]
			provisioner := f.createProvisioner(ctx, t, nil, nil)
			keySecretName := types.NamespacedName{Name: provisioner.Name + "-provisioner-key", Namespace: provisioner.Namespace}

			base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
			require.NoError(t, err)
			var recordedAtSecretWrite bool
			ordered := interceptor.NewClient(base, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
					if p, ok := obj.(*coderv1alpha1.CoderProvisioner); ok && failStatus && pendingKeyRecorded(p, created.KeyID) {
						return apierrors.NewConflict(coderv1alpha1.GroupVersion.WithResource("coderprovisioners").GroupResource(), p.Name, errors.New("injected"))
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
					if s, ok := obj.(*corev1.Secret); ok && s.Name == keySecretName.Name {
						latest := &coderv1alpha1.CoderProvisioner{}
						require.NoError(t, c.Get(ctx, types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}, latest))
						recordedAtSecretWrite = pendingKeyRecorded(latest, created.KeyID)
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			f.reconciler.Client = ordered
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}}

			_, err = f.reconciler.Reconcile(ctx, request) // adds the finalizer
			require.NoError(t, err)
			_, err = f.reconciler.Reconcile(ctx, request)

			if !failStatus {
				require.NoError(t, err)
				require.True(t, recordedAtSecretWrite, "status must record the key before the Secret holds it")
				require.Empty(t, f.bootstrapClient.deleteKeyByIDRequests)
				return
			}
			require.Error(t, err)
			require.Len(t, f.bootstrapClient.deleteKeyByIDRequests, 1, "the unpublished key must be revoked")
			require.Equal(t, created.KeyID, f.bootstrapClient.deleteKeyByIDRequests[0].KeyID)
			require.Equal(t, created.OrganizationID, f.bootstrapClient.deleteKeyByIDRequests[0].OrganizationID)
			err = k8sClient.Get(ctx, keySecretName, &corev1.Secret{})
			require.True(t, apierrors.IsNotFound(err), "no Secret may hold the revoked key, got %v", err)
		})
	}
}

// TestCoderProvisionerKey_FailedSecretUpdateRotatesAgain checks that a drift
// rotation whose Secret update fails is retried: the Secret must not keep the
// material of the key that the rotation deleted.
func TestCoderProvisionerKey_FailedSecretUpdateRotatesAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-key-retry")
	f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{
		{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-1"},
		{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-2"},
		{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-3"},
	}
	provisioner := f.createProvisioner(ctx, t, nil, nil)
	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)
	keySecretName := types.NamespacedName{Name: provisioner.Name + "-provisioner-key", Namespace: provisioner.Namespace}
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, keySecretName, secret))
	require.Equal(t, "material-1", string(secret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))

	latest := f.get(ctx, t, provisioner)
	latest.Spec.Tags = map[string]string{"region": "rotated"}
	require.NoError(t, k8sClient.Update(ctx, latest))

	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	failSecretUpdate := true
	f.reconciler.Client = interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
			if s, ok := obj.(*corev1.Secret); ok && s.Name == keySecretName.Name && failSecretUpdate {
				failSecretUpdate = false
				return apierrors.NewServiceUnavailable("injected Secret update failure")
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}}

	_, err = f.reconciler.Reconcile(ctx, request)
	require.Error(t, err, "the injected Secret update failure must fail the reconcile")
	afterFailure := f.get(ctx, t, provisioner)
	require.True(t, pendingKeyRecorded(afterFailure, f.bootstrapClient.provisionerKeyResponses[1].KeyID),
		"status must record the new key before the Secret write")

	_, err = f.reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, keySecretName, secret))
	require.Equal(t, "material-3", string(secret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]),
		"the retry must rotate again and publish a live key")
	final := f.get(ctx, t, provisioner)
	require.Equal(t, f.bootstrapClient.provisionerKeyResponses[2].KeyID.String(), final.Status.ProvisionerKeyID)
	require.Empty(t, final.Status.PendingKeys, "the published key moves to the applied status fields")
}

// pendingKeyRecorded reports whether status.pendingKeys records the key ID.
func pendingKeyRecorded(provisioner *coderv1alpha1.CoderProvisioner, keyID uuid.UUID) bool {
	return slices.ContainsFunc(provisioner.Status.PendingKeys, func(record coderv1alpha1.CoderProvisionerKeyRecord) bool {
		return record.KeyID == keyID.String()
	})
}
