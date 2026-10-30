package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

// coderCallLog records the Coder URL of every call, and can fail the deletes
// of one key.
type coderCallLog struct {
	coderbootstrap.Client
	urls          []string
	failDelete    uuid.UUID
	failedDeletes int
}

func (c *coderCallLog) EnsureProvisionerKey(ctx context.Context, req coderbootstrap.EnsureProvisionerKeyRequest) (coderbootstrap.EnsureProvisionerKeyResponse, error) {
	c.urls = append(c.urls, req.CoderURL)
	return c.Client.EnsureProvisionerKey(ctx, req)
}

func (c *coderCallLog) DeleteProvisionerKeyByID(ctx context.Context, req coderbootstrap.DeleteProvisionerKeyByIDRequest) error {
	c.urls = append(c.urls, req.CoderURL)
	if req.KeyID == c.failDelete {
		c.failedDeletes++
		return errors.New("injected delete failure")
	}
	return c.Client.DeleteProvisionerKeyByID(ctx, req)
}

func (c *coderCallLog) ProvisionerKeyByID(ctx context.Context, req coderbootstrap.ProvisionerKeyByIDRequest) (*codersdk.ProvisionerKey, error) {
	c.urls = append(c.urls, req.CoderURL)
	return c.Client.ProvisionerKeyByID(ctx, req)
}

func (c *coderCallLog) ProvisionerKeyByName(ctx context.Context, req coderbootstrap.ProvisionerKeyByNameRequest) (*codersdk.ProvisionerKey, error) {
	c.urls = append(c.urls, req.CoderURL)
	return c.Client.ProvisionerKeyByName(ctx, req)
}

func (c *coderCallLog) Entitlements(ctx context.Context, coderURL, sessionToken string) (codersdk.Entitlements, error) {
	c.urls = append(c.urls, coderURL)
	return c.Client.Entitlements(ctx, coderURL, sessionToken)
}

// TestCoderProvisionerKey_EarlierTargetRevokedWhileTargetNotReady checks a
// key that a rotation from control plane A to B replaced and could not delete.
// When the provisioner moves on to a control plane C that is not ready, the
// key at A must still be deleted at A with the operator token of A, and C must
// get no request. Key B, which the Secret holds, must stay. A failed delete is
// retried at the pending key cadence, not on every reconcile.
func TestCoderProvisionerKey_EarlierTargetRevokedWhileTargetNotReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// notReady makes control plane C fail one of its checks.
		notReady func(ctx context.Context, t *testing.T, namespace string)
		// deleteFails fails every delete of key A at C.
		deleteFails bool
	}{
		{name: "target not entitled", notReady: markNotEntitled},
		{name: "target operator token missing", notReady: deleteOperatorToken},
		{name: "delete fails at the earlier target", notReady: markNotEntitled, deleteFails: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-earlier")
			createControlPlaneWithToken(ctx, t, f.namespace, "coder-b", "https://coder-b.example.com", "operator-token-b")
			createControlPlaneWithToken(ctx, t, f.namespace, "coder-c", "https://coder-c.example.com", "operator-token-c")
			apiURL := func(controlPlane string) string {
				return "http://" + controlPlane + "." + f.namespace + ".svc.cluster.local:80"
			}
			keyA := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-a"}
			keyB := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-b"}
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{keyA, keyB}
			calls := &coderCallLog{Client: f.bootstrapClient}
			f.reconciler.BootstrapClient = calls

			provisioner := f.createProvisioner(ctx, t, nil, nil)
			name := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
			moveTo := func(controlPlane string) {
				latest := f.get(ctx, t, provisioner)
				latest.Spec.ControlPlaneRef.Name = controlPlane
				require.NoError(t, k8sClient.Update(ctx, latest))
			}
			reconcile := func() {
				_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
				require.NoError(t, err)
			}

			f.reconcile(ctx, t, provisioner)
			f.reconcile(ctx, t, provisioner)
			require.Equal(t, keyA.KeyID.String(), f.get(ctx, t, provisioner).Status.ProvisionerKeyID)

			moveTo("coder-b")
			calls.failDelete = keyA.KeyID
			reconcile()
			atB := f.get(ctx, t, provisioner)
			require.Equal(t, keyB.KeyID.String(), atB.Status.ProvisionerKeyID)
			require.True(t, pendingKeyRecorded(atB, keyA.KeyID), "the failed delete must keep the record of key A")

			if !tt.deleteFails {
				calls.failDelete = uuid.Nil
			}
			calls.failedDeletes = 0
			calls.urls = nil
			f.bootstrapClient.deleteKeyByIDRequests = nil
			tt.notReady(ctx, t, f.namespace)
			moveTo("coder-c")
			reconcile()
			if tt.deleteFails {
				reconcile()
			}

			require.NotContains(t, calls.urls, apiURL("coder-c"), "control plane C must get no request while it is not ready")
			require.NotContains(t, keyDeletes(f.bootstrapClient), keyDelete{coderURL: apiURL("coder-b"), token: "operator-token-b", keyID: keyB.KeyID.String()},
				"nothing may delete key B, which the Secret holds")
			final := f.get(ctx, t, provisioner)
			require.Equal(t, keyB.KeyID.String(), final.Status.ProvisionerKeyID)
			if tt.deleteFails {
				require.Equal(t, 1, calls.failedDeletes, "a failed delete must wait for the pending key cadence")
				require.True(t, pendingKeyRecorded(final, keyA.KeyID), "a failed delete must keep the record of key A")
				return
			}
			require.Equal(t, []keyDelete{{coderURL: apiURL("coder"), token: testOperatorToken, keyID: keyA.KeyID.String()}}, keyDeletes(f.bootstrapClient),
				"key A must be deleted at A with the operator token of A")
			require.False(t, pendingKeyRecorded(final, keyA.KeyID), "the record of key A must go once Coder confirmed the delete")
		})
	}
}

func markNotEntitled(ctx context.Context, t *testing.T, namespace string) {
	t.Helper()
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "coder-c", Namespace: namespace}, controlPlane))
	now := metav1.NewTime(time.Now())
	controlPlane.Status.EntitlementsLastChecked = &now
	controlPlane.Status.ExternalProvisionerDaemonsEntitlement = string(codersdk.EntitlementNotEntitled)
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
}

func deleteOperatorToken(ctx context.Context, t *testing.T, namespace string) {
	t.Helper()
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "coder-c-operator-token", Namespace: namespace}}))
}
