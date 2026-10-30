package controller_test

import (
	"cmp"
	"context"
	"slices"
	"testing"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

// TestCoderProvisionerKey_OtherProvisionersKeyKept checks two provisioners P
// and Q with the same key name at one Coder endpoint and organization. Q holds
// the key. Whatever way P reaches that key name, P must not delete or replace
// the key of Q, and must not change its own key, Secret or Deployment. It must
// report the conflict, and get its own key once it has a distinct key name or
// the key of Q is gone, or keep its key after a revert to its key name.
func TestCoderProvisionerKey_OtherProvisionersKeyKept(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// holdsKey makes P hold the key p-key before it reaches the key name
		// of Q.
		holdsKey bool
		// dropKeyStatus removes the key fields from the status of P, as
		// status that lost its key records.
		dropKeyStatus bool
		// resolveName resolves the conflict with this key name for P.
		// Otherwise the key of Q goes.
		resolveName string
	}{
		{name: "first key of P"},
		{name: "key name change of P", holdsKey: true, resolveName: "p-own"},
		{name: "key name change of P, reverted", holdsKey: true, resolveName: "p-key"},
		{name: "status without key records", holdsKey: true, dropKeyStatus: true, resolveName: "p-own"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-samename")
			store := &keyStoreClient{fakeBootstrapClient: f.bootstrapClient, keys: map[keySlot]uuid.UUID{}}
			f.reconciler.BootstrapClient = store
			slotOf := func(keyName string) keySlot { return newKeySlot(internalCoderURL(f), "", keyName) }

			q := f.createKeyProvisioner(ctx, t, "q", "")
			q.Spec.Key.Name = "provisioner"
			require.NoError(t, k8sClient.Update(ctx, q))
			f.reconcile(ctx, t, q)
			f.reconcile(ctx, t, q)
			qKeyID := store.keys[slotOf("provisioner")]
			require.Equal(t, qKeyID.String(), f.get(ctx, t, q).Status.ProvisionerKeyID, "Q must hold the key named provisioner")

			p := f.createProvisioner(ctx, t, nil, nil)
			pName := types.NamespacedName{Name: p.Name, Namespace: p.Namespace}
			keySecret := types.NamespacedName{Name: p.Name + "-provisioner-key", Namespace: p.Namespace}
			var pKeyID uuid.UUID
			var pSecret corev1.Secret
			if tt.holdsKey {
				latest := f.get(ctx, t, p)
				latest.Spec.Key.Name = "p-key"
				require.NoError(t, k8sClient.Update(ctx, latest))
				f.reconcile(ctx, t, p)
				f.reconcile(ctx, t, p)
				pKeyID = store.keys[slotOf("p-key")]
				require.Equal(t, pKeyID.String(), f.get(ctx, t, p).Status.ProvisionerKeyID)
				require.NoError(t, k8sClient.Get(ctx, keySecret, &pSecret))

				latest = f.get(ctx, t, p)
				latest.Spec.Key.Name = ""
				require.NoError(t, k8sClient.Update(ctx, latest))
				if tt.dropKeyStatus {
					latest = f.get(ctx, t, p)
					latest.Status = coderv1alpha1.CoderProvisionerStatus{SecretRef: latest.Status.SecretRef}
					require.NoError(t, k8sClient.Status().Update(ctx, latest))
				}
			} else {
				f.reconcile(ctx, t, p)
			}

			for range 2 {
				result, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: pName})
				require.NoError(t, err)
				require.Equal(t, qKeyID, store.keys[slotOf("provisioner")], "the key of Q must still exist")
				require.Positive(t, result.RequeueAfter, "P must check the key name again")
			}
			latest := f.get(ctx, t, p)
			keyReady := meta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady)
			require.NotNil(t, keyReady, "P must report the conflict")
			require.Equal(t, metav1.ConditionFalse, keyReady.Status)
			require.Equal(t, "KeyNameInUse", keyReady.Reason)
			require.Contains(t, keyReady.Message, `"provisioner"`)
			require.Contains(t, keyReady.Message, qKeyID.String())
			require.Nil(t, meta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderProvisionerConditionUnverifiedKeyKept))
			require.Empty(t, latest.Status.PendingKeys, "P must record no create")
			if tt.holdsKey {
				require.Equal(t, pKeyID, store.keys[slotOf("p-key")], "the key of P must stay while its new key name is taken")
				secret := &corev1.Secret{}
				require.NoError(t, k8sClient.Get(ctx, keySecret, secret))
				require.Equal(t, pSecret.Data, secret.Data, "the Secret of P must keep its key")
			} else {
				_, exists := f.deployment(ctx, t, p)
				require.False(t, exists, "P must not start without a key")
			}
			resourceVersion := latest.ResourceVersion
			_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: pName})
			require.NoError(t, err)
			require.Equal(t, resourceVersion, f.get(ctx, t, p).ResourceVersion, "a repeated conflict must not write status")

			keyName := cmp.Or(tt.resolveName, "provisioner")
			if tt.resolveName != "" {
				latest.Spec.Key.Name = keyName
				require.NoError(t, k8sClient.Update(ctx, latest))
			} else {
				delete(store.keys, slotOf("provisioner"))
			}
			f.reconcile(ctx, t, p)
			latest = f.get(ctx, t, p)
			require.Equal(t, store.keys[slotOf(keyName)].String(), latest.Status.ProvisionerKeyID, "P must hold its own key")
			keyReady = meta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady)
			require.NotNil(t, keyReady)
			require.Equal(t, metav1.ConditionTrue, keyReady.Status)
			require.Nil(t, meta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderProvisionerConditionUnverifiedKeyKept))
			if tt.resolveName != "" {
				require.Equal(t, qKeyID, store.keys[slotOf("provisioner")], "the key of Q must still exist")
			}
			if tt.resolveName == "p-key" {
				require.Equal(t, pKeyID.String(), latest.Status.ProvisionerKeyID, "a revert must keep the key of P")
			}
		})
	}
}

// urlToken is the Coder URL and the operator token of one Coder request.
type urlToken struct{ coderURL, token string }

// sentTokens lists the Coder URL and token of every Coder request that the
// fake received, and records lookups by name into lookups.
func sentTokens(client *fakeBootstrapClient, lookups []urlToken) []urlToken {
	sent := slices.Clone(lookups)
	for _, req := range client.provisionerKeyRequests {
		sent = append(sent, urlToken{req.CoderURL, req.SessionToken})
	}
	for _, req := range client.entitlementsRequests {
		sent = append(sent, urlToken{req.CoderURL, req.SessionToken})
	}
	for _, req := range keyDeletes(client) {
		sent = append(sent, urlToken{req.coderURL, req.token})
	}
	return sent
}

// TestCoderProvisionerKey_ControlPlaneRecreated checks a control plane that is
// deleted and created again under the same name with another operator token,
// at another URL or at the same URL with another Coder backend. The new token
// must never reach the URL of the old control plane, and must never be used
// for the key of the old one: the record of the old key stays, and the
// provisioner reports it. The provisioner must create a new key. A record
// from an operator that did not record control plane UIDs may use the token
// of its control plane only at that control plane's current URL.
func TestCoderProvisionerKey_ControlPlaneRecreated(t *testing.T) {
	t.Parallel()

	const (
		oldURL   = "http://coder-old.example.com"
		newURL   = "http://coder-new.example.com"
		newToken = "operator-token-new"
	)
	tests := []struct {
		name string
		// legacy removes the control plane UID from status, as an operator
		// before UID records wrote it.
		legacy bool
		// keep keeps the control plane and moves P to another one instead.
		keep bool
		// sameURL creates the control plane again at its old URL.
		sameURL bool
		// sameBackend: the new control plane serves the Coder backend of the
		// old one, which still holds the old key.
		sameBackend bool
		deleteP     bool
	}{
		{name: "rotation after the recreate"},
		{name: "deletion after the recreate", deleteP: true},
		{name: "rotation after the recreate at the same URL", sameURL: true},
		{name: "deletion after the recreate at the same URL", sameURL: true, deleteP: true},
		{name: "rotation after the recreate at the same URL and backend", sameURL: true, sameBackend: true},
		{name: "status without control plane UID, rotation after the recreate", legacy: true},
		{name: "status without control plane UID, deletion after the recreate", legacy: true, deleteP: true},
		{name: "status without control plane UID, control plane kept", legacy: true, keep: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-recreated")
			setControlPlaneURL(ctx, t, f, oldURL)
			keyOld := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-old"}
			keyNew := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-new"}
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{keyOld, keyNew}
			var lookups []urlToken
			var recreated bool
			f.bootstrapClient.provisionerKeyByName = func(req coderbootstrap.ProvisionerKeyByNameRequest) *codersdk.ProvisionerKey {
				lookups = append(lookups, urlToken{req.CoderURL, req.SessionToken})
				if !tt.sameBackend || !recreated || req.CoderURL != oldURL || len(f.bootstrapClient.deleteKeyByIDRequests) > 0 {
					return nil
				}
				return &codersdk.ProvisionerKey{ID: keyOld.KeyID, OrganizationID: keyOld.OrganizationID, Name: keyOld.KeyName}
			}

			p := f.createProvisioner(ctx, t, nil, nil)
			pName := types.NamespacedName{Name: p.Name, Namespace: p.Namespace}
			f.reconcile(ctx, t, p)
			f.reconcile(ctx, t, p)
			require.Equal(t, keyOld.KeyID.String(), f.get(ctx, t, p).Status.ProvisionerKeyID)
			if tt.legacy {
				require.NoError(t, k8sClient.Status().Patch(ctx, f.get(ctx, t, p),
					ctrlclient.RawPatch(types.MergePatchType, []byte(`{"status":{"controlPlaneUID":null}}`))))
			}

			currentURL := newURL
			if tt.sameURL {
				currentURL = oldURL
			}
			if tt.keep {
				createControlPlaneWithToken(ctx, t, f.namespace, "coder-b", newURL, newToken)
				latest := f.get(ctx, t, p)
				latest.Spec.ControlPlaneRef.Name = "coder-b"
				require.NoError(t, k8sClient.Update(ctx, latest))
			} else {
				require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: f.namespace}}))
				require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "coder-operator-token", Namespace: f.namespace}}))
				createControlPlaneWithToken(ctx, t, f.namespace, "coder", currentURL, newToken)
			}
			recreated = true

			requireNoLeak := func() {
				t.Helper()
				// A record without a UID is bound at the current URL, so
				// only a recorded UID keeps the new token from the old key.
				for _, d := range keyDeletes(f.bootstrapClient) {
					require.False(t, !tt.legacy && !tt.sameBackend && d.keyID == keyOld.KeyID.String() && d.token == newToken, "the new control plane's token must not be used for the old key, sent %+v", keyDeletes(f.bootstrapClient))
				}
				if tt.sameURL {
					return
				}
				sent := sentTokens(f.bootstrapClient, lookups)
				for _, s := range sent {
					require.False(t, s.coderURL == oldURL && s.token == newToken, "the new control plane's token must not reach %s, sent %+v", oldURL, sent)
				}
			}
			if tt.deleteP {
				require.NoError(t, k8sClient.Delete(ctx, f.get(ctx, t, p)))
				f.reconcile(ctx, t, p)
				requireNoLeak()
				requireProvisionerGone(ctx, t, pName)
			} else {
				result, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: pName})
				require.NoError(t, err)
				requireNoLeak()
				latest := f.get(ctx, t, p)
				require.Equal(t, keyNew.KeyID.String(), latest.Status.ProvisionerKeyID, "P must create a new key")
				require.Equal(t, currentURL, latest.Status.ControlPlaneURL)
				kept := slices.ContainsFunc(latest.Status.PendingKeys, func(record coderv1alpha1.CoderProvisionerKeyRecord) bool {
					return record.KeyID == keyOld.KeyID.String() && record.CoderURL == oldURL
				})
				report := meta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneChangedKeyKept)
				switch {
				case tt.sameBackend:
					require.Contains(t, keyDeletes(f.bootstrapClient), keyDelete{coderURL: oldURL, token: newToken, keyID: keyOld.KeyID.String()},
						"the rotation must delete the old key that the current backend holds")
					require.False(t, kept, "a key that the current backend deleted must not stay recorded")
					require.Nil(t, report)
				case tt.keep:
					require.False(t, kept, "the old key must be deleted at its URL")
					require.Nil(t, report)
				default:
					require.True(t, kept, "the record of the old key must stay")
					require.Positive(t, result.RequeueAfter, "P must check the kept record again")
					require.NotNil(t, report, "P must report the record that it keeps")
					require.Equal(t, metav1.ConditionTrue, report.Status)
					require.Contains(t, report.Message, oldURL)
				}
			}

			if tt.keep {
				require.Contains(t, keyDeletes(f.bootstrapClient), keyDelete{coderURL: oldURL, token: testOperatorToken, keyID: keyOld.KeyID.String()},
					"the old key must be deleted at its URL with the token of its control plane")
			}
		})
	}
}
