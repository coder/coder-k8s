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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
)

// coderCallLog records the Coder URL of every call, can fail the deletes of
// one key, and can fail every key lookup by name.
type coderCallLog struct {
	coderbootstrap.Client
	urls          []string
	failDelete    uuid.UUID
	failedDeletes int
	failLookup    bool
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
	if c.failLookup {
		return nil, errors.New("injected lookup failure")
	}
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
// retried at the pending key cadence, not on every reconcile, also by the
// rotation to a ready C. When the Secret
// write at B failed instead, the unpublished key B, which the Secret can hold,
// must stay until C is ready.
func TestCoderProvisionerKey_EarlierTargetRevokedWhileTargetNotReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// notReady makes control plane C fail one of its checks. When it is
		// nil, C is ready.
		notReady func(ctx context.Context, t *testing.T, namespace string)
		// deleteFails fails every delete of key A at C.
		deleteFails bool
		// unpublishedAtB fails the Secret write at B instead of the delete
		// of key A.
		unpublishedAtB bool
	}{
		{name: "target not entitled", notReady: markNotEntitled},
		{name: "target operator token missing", notReady: deleteOperatorToken},
		{name: "delete fails at the earlier target", notReady: markNotEntitled, deleteFails: true},
		{name: "delete fails at the earlier target, target ready", deleteFails: true},
		{name: "unpublished key at the earlier target", notReady: markNotEntitled, unpublishedAtB: true},
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
			keyC := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-c"}
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{keyA, keyB, keyC}
			calls := &coderCallLog{Client: f.bootstrapClient}
			f.reconciler.BootstrapClient = calls

			provisioner := f.createProvisioner(ctx, t, nil, nil)
			name := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
			keySecretName := provisioner.Name + "-provisioner-key"
			failSecretWrite := false
			base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
			require.NoError(t, err)
			f.reconciler.Client = interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
					if s, ok := obj.(*corev1.Secret); ok && s.Name == keySecretName && failSecretWrite {
						failSecretWrite = false
						return apierrors.NewServiceUnavailable("injected Secret write failure")
					}
					return c.Update(ctx, obj, opts...)
				},
			})
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
			if tt.unpublishedAtB {
				failSecretWrite = true
				_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
				require.Error(t, err, "the injected Secret write failure must fail the attempt at B")
				require.True(t, pendingKeyRecorded(f.get(ctx, t, provisioner), keyB.KeyID), "status must record the unpublished key B")
			} else {
				calls.failDelete = keyA.KeyID
				reconcile()
				atB := f.get(ctx, t, provisioner)
				require.Equal(t, keyB.KeyID.String(), atB.Status.ProvisionerKeyID)
				require.True(t, pendingKeyRecorded(atB, keyA.KeyID), "the failed delete must keep the record of key A")
			}

			if !tt.deleteFails {
				calls.failDelete = uuid.Nil
			}
			calls.failedDeletes = 0
			calls.urls = nil
			f.bootstrapClient.deleteKeyByIDRequests = nil
			if tt.notReady != nil {
				tt.notReady(ctx, t, f.namespace)
			}
			moveTo("coder-c")
			reconcile()
			if tt.deleteFails {
				reconcile()
			}

			if tt.notReady != nil {
				require.NotContains(t, calls.urls, apiURL("coder-c"), "control plane C must get no request while it is not ready")
				require.NotContains(t, keyDeletes(f.bootstrapClient), keyDelete{coderURL: apiURL("coder-b"), token: "operator-token-b", keyID: keyB.KeyID.String()},
					"nothing may delete key B, which the Secret holds or can hold, while C is not ready")
			}
			final := f.get(ctx, t, provisioner)
			if tt.unpublishedAtB {
				require.Empty(t, keyDeletes(f.bootstrapClient))
				require.True(t, pendingKeyRecorded(final, keyB.KeyID), "the record of the unpublished key B must stay")
				return
			}
			applied := keyB
			if tt.notReady == nil {
				applied = keyC
			}
			require.Equal(t, applied.KeyID.String(), final.Status.ProvisionerKeyID)
			if tt.deleteFails {
				require.Equal(t, 1, calls.failedDeletes, "a failed delete must wait for the pending key cadence")
				require.True(t, pendingKeyRecorded(final, keyA.KeyID), "a failed delete must keep the record of key A")
				controller.ElapseEarlierKeyRetryInterval(f.reconciler, final.UID)
				reconcile()
				require.Equal(t, 2, calls.failedDeletes, "the delete must be tried again after the pending key cadence")
				return
			}
			require.Equal(t, []keyDelete{{coderURL: apiURL("coder"), token: testOperatorToken, keyID: keyA.KeyID.String()}}, keyDeletes(f.bootstrapClient),
				"key A must be deleted at A with the operator token of A")
			require.False(t, pendingKeyRecorded(final, keyA.KeyID), "the record of key A must go once Coder confirmed the delete")
		})
	}
}

// TestCoderProvisionerKey_EarlierTargetReportsBesideUnpublishedKey checks the
// reports of earlier target records while an unpublished record waits for a
// control plane C that is not ready. A record whose control plane was created
// again must be reported, and the report of a record without key IDs must go
// once Coder has no key with its name.
func TestCoderProvisionerKey_EarlierTargetReportsBesideUnpublishedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-earlier-reports")
	createControlPlaneWithToken(ctx, t, f.namespace, "coder-b", "https://coder-b.example.com", "operator-token-b")
	createControlPlaneWithToken(ctx, t, f.namespace, "coder-c", "https://coder-c.example.com", "operator-token-c")
	calls := &coderCallLog{Client: f.bootstrapClient}
	f.reconciler.BootstrapClient = calls
	provisioner := f.createProvisioner(ctx, t, nil, nil)
	name := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)
	calls.failLookup = true

	controlPlaneB := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "coder-b", Namespace: f.namespace}, controlPlaneB))
	record := func(keyName string, withIDs bool) coderv1alpha1.CoderProvisionerKeyRecord {
		record := coderv1alpha1.CoderProvisionerKeyRecord{
			ControlPlaneRefName: "coder-b",
			ControlPlaneUID:     string(controlPlaneB.UID),
			CoderURL:            "http://coder-b." + f.namespace + ".svc.cluster.local:80",
			OrganizationName:    "default",
			KeyName:             keyName,
		}
		if withIDs {
			record.OrganizationID = uuid.NewString()
			record.KeyID = uuid.NewString()
		}
		return record
	}
	recreated := record("recreated", true)
	recreated.ControlPlaneUID = uuid.NewString()
	unpublished := record("unpublished", true)
	unpublished.Unpublished = true
	idLess := record("id-less", false)
	latest := f.get(ctx, t, provisioner)
	latest.Status.PendingKeys = []coderv1alpha1.CoderProvisionerKeyRecord{recreated, unpublished, idLess}
	require.NoError(t, k8sClient.Status().Update(ctx, latest))
	markNotEntitled(ctx, t, f.namespace)
	latest = f.get(ctx, t, provisioner)
	latest.Spec.ControlPlaneRef.Name = "coder-c"
	require.NoError(t, k8sClient.Update(ctx, latest))
	reconcile := func() *coderv1alpha1.CoderProvisioner {
		_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
		require.NoError(t, err)
		return f.get(ctx, t, provisioner)
	}

	got := reconcile()
	require.Equal(t, []coderv1alpha1.CoderProvisionerKeyRecord{recreated, unpublished, idLess}, got.Status.PendingKeys)
	changed := meta.FindStatusCondition(got.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneChangedKeyKept)
	require.NotNil(t, changed, "the record whose control plane was created again must be reported")
	require.Contains(t, changed.Message, `"recreated"`)
	require.NotContains(t, changed.Message, `"unpublished"`)
	unverified := meta.FindStatusCondition(got.Status.Conditions, coderv1alpha1.CoderProvisionerConditionUnverifiedKeyKept)
	require.NotNil(t, unverified, "the record without key IDs must be reported")
	require.Contains(t, unverified.Message, `"id-less"`)

	calls.failLookup = false
	controller.ElapseEarlierKeyRetryInterval(f.reconciler, got.UID)
	got = reconcile()
	require.Equal(t, []coderv1alpha1.CoderProvisionerKeyRecord{recreated, unpublished}, got.Status.PendingKeys,
		"the record without key IDs must go once Coder has no key with its name")
	require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, coderv1alpha1.CoderProvisionerConditionUnverifiedKeyKept))
	require.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneChangedKeyKept))
	require.Empty(t, keyDeletes(f.bootstrapClient), "no recorded key may be deleted: one has another control plane, one is unpublished")
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
