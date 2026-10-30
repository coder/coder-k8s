package controller_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
		// staleRecord records a replaced key of P at another key name, whose
		// delete Coder has not confirmed yet.
		staleRecord bool
		// resolveName resolves the conflict with this key name for P.
		// Otherwise the key of Q goes.
		resolveName string
	}{
		{name: "first key of P"},
		{name: "key name change of P", holdsKey: true, resolveName: "p-own"},
		{name: "key name change of P, reverted", holdsKey: true, resolveName: "p-key"},
		{name: "key name change of P with a replaced key left", holdsKey: true, staleRecord: true, resolveName: "p-own"},
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

			if tt.staleRecord {
				stale := slotOf("p-stale")
				store.keys[stale] = uuid.New()
				latest := f.get(ctx, t, p)
				latest.Status.PendingKeys = append(latest.Status.PendingKeys, coderv1alpha1.CoderProvisionerKeyRecord{
					ControlPlaneRefName: f.controlPlane.Name,
					ControlPlaneUID:     string(f.controlPlane.UID),
					CoderURL:            internalCoderURL(f),
					OrganizationName:    "default",
					KeyName:             "p-stale",
					OrganizationID:      stale.organizationID().String(),
					KeyID:               store.keys[stale].String(),
				})
				require.NoError(t, k8sClient.Status().Update(ctx, latest))
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
			require.NotContains(t, store.keys, slotOf("p-stale"), "a key recorded at an earlier target must not wait for the conflict")
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
// of its control plane only at that control plane's current URL, and its
// applied key stays only when the current backend holds it.
func TestCoderProvisionerKey_ControlPlaneRecreated(t *testing.T) {
	t.Parallel()

	const (
		oldURL   = "http://coder-old.example.com"
		newURL   = "http://coder-new.example.com"
		newToken = "operator-token-new"
	)
	// What a rotation after the change leaves of the old key.
	const (
		oldKeyKept    = "kept"    // its record stays, and the provisioner reports it
		oldKeyDeleted = "deleted" // deleted where Coder holds it, no record left
		oldKeyDropped = "dropped" // the current backend has no such key, no record left
	)
	tests := []struct {
		name string
		// legacy removes the control plane UID from status, as an operator
		// before UID records wrote it.
		legacy bool
		// verifyFirst reconciles once before the recreate, while the backend
		// holds the old key.
		verifyFirst bool
		// keep keeps the control plane and moves P to another one instead.
		keep bool
		// sameURL creates the control plane again at its old URL.
		sameURL bool
		// sameBackend: the new control plane serves the Coder backend of the
		// old one, which still holds the old key.
		sameBackend bool
		deleteP     bool
		// oldKey is what a rotation leaves of the old key.
		oldKey string
	}{
		{name: "rotation after the recreate", oldKey: oldKeyKept},
		{name: "deletion after the recreate", deleteP: true},
		{name: "rotation after the recreate at the same URL", sameURL: true, oldKey: oldKeyKept},
		{name: "deletion after the recreate at the same URL", sameURL: true, deleteP: true},
		{name: "rotation after the recreate at the same URL and backend", sameURL: true, sameBackend: true, oldKey: oldKeyDeleted},
		{name: "status without control plane UID, rotation after the recreate", legacy: true, oldKey: oldKeyKept},
		{name: "status without control plane UID, deletion after the recreate", legacy: true, deleteP: true},
		{name: "status without control plane UID, recreate at the same URL", legacy: true, sameURL: true, oldKey: oldKeyDropped},
		{name: "status without control plane UID, verified before the recreate at the same URL", legacy: true, verifyFirst: true, sameURL: true, oldKey: oldKeyKept},
		{name: "status without control plane UID, control plane kept", legacy: true, keep: true, oldKey: oldKeyDeleted},
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
			// oldKeyInCoder: the backend at oldURL holds the old key.
			oldKeyInCoder := false
			oldKeyAt := func(coderURL, token string) *codersdk.ProvisionerKey {
				lookups = append(lookups, urlToken{coderURL, token})
				if !oldKeyInCoder || coderURL != oldURL || len(f.bootstrapClient.deleteKeyByIDRequests) > 0 {
					return nil
				}
				return &codersdk.ProvisionerKey{ID: keyOld.KeyID, OrganizationID: keyOld.OrganizationID, Name: keyOld.KeyName}
			}
			f.bootstrapClient.provisionerKeyByName = func(req coderbootstrap.ProvisionerKeyByNameRequest) *codersdk.ProvisionerKey {
				return oldKeyAt(req.CoderURL, req.SessionToken)
			}
			f.bootstrapClient.provisionerKeyByID = func(req coderbootstrap.ProvisionerKeyByIDRequest) *codersdk.ProvisionerKey {
				if req.OrganizationID != keyOld.OrganizationID || req.KeyID != keyOld.KeyID {
					return nil
				}
				return oldKeyAt(req.CoderURL, req.SessionToken)
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
			if tt.verifyFirst {
				oldKeyInCoder = true
				f.reconcile(ctx, t, p)
				latest := f.get(ctx, t, p)
				require.Equal(t, keyOld.KeyID.String(), latest.Status.ProvisionerKeyID, "a key that the backend holds must stay")
				require.NotEmpty(t, latest.Status.ControlPlaneUID, "the operator must bind the verified key to its control plane")
			}
			oldKeyInCoder = tt.sameBackend

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

			requireNoLeak := func() {
				t.Helper()
				// A record without a UID is bound at the current URL, so only
				// a recorded UID keeps the new token from the old key.
				bound := !tt.legacy || tt.verifyFirst
				for _, d := range keyDeletes(f.bootstrapClient) {
					require.False(t, bound && !tt.sameBackend && d.keyID == keyOld.KeyID.String() && d.token == newToken,
						"the new control plane's token must not be used for the old key, sent %+v", keyDeletes(f.bootstrapClient))
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
				return
			}

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
			oldKeyDeletes := slices.DeleteFunc(keyDeletes(f.bootstrapClient), func(d keyDelete) bool { return d.keyID != keyOld.KeyID.String() })
			switch tt.oldKey {
			case oldKeyKept:
				require.True(t, kept, "the record of the old key must stay")
				require.Positive(t, result.RequeueAfter, "P must check the kept record again")
				require.NotNil(t, report, "P must report the record that it keeps")
				require.Equal(t, metav1.ConditionTrue, report.Status)
				require.Contains(t, report.Message, oldURL)
				require.Empty(t, oldKeyDeletes)
			case oldKeyDeleted:
				token := newToken
				if tt.keep {
					token = testOperatorToken
				}
				require.Equal(t, []keyDelete{{coderURL: oldURL, token: token, keyID: keyOld.KeyID.String()}}, oldKeyDeletes,
					"the old key must be deleted once, where Coder holds it, with the token of the control plane that serves it")
				require.False(t, kept, "a deleted key must not stay recorded")
				require.Nil(t, report)
			case oldKeyDropped:
				require.Empty(t, oldKeyDeletes)
				require.False(t, kept)
				require.Nil(t, report)
			default:
				t.Fatalf("assertion failed: unknown oldKey %q", tt.oldKey)
			}
		})
	}
}

// renamedOrgCoder is a Coder at an httptest server, reached through the real
// SDK client, with one organization named team. Key creates answer from the
// fake and add the key to the server.
type renamedOrgCoder struct {
	*coderbootstrap.SDKClient
	fake *fakeBootstrapClient
	mu   sync.Mutex
	keys map[uuid.UUID]codersdk.ProvisionerKey
	// team is the ID of the organization named team.
	team uuid.UUID
	// failList is the ID of an organization whose key list fails.
	failList uuid.UUID
}

func (c *renamedOrgCoder) EnsureProvisionerKey(ctx context.Context, req coderbootstrap.EnsureProvisionerKeyRequest) (coderbootstrap.EnsureProvisionerKeyResponse, error) {
	response, err := c.fake.EnsureProvisionerKey(ctx, req)
	if err == nil {
		c.mu.Lock()
		c.keys[response.KeyID] = codersdk.ProvisionerKey{ID: response.KeyID, OrganizationID: response.OrganizationID, Name: response.KeyName}
		c.mu.Unlock()
	}
	return response, err
}

func (c *renamedOrgCoder) Entitlements(ctx context.Context, coderURL, sessionToken string) (codersdk.Entitlements, error) {
	return c.fake.Entitlements(ctx, coderURL, sessionToken)
}

func (c *renamedOrgCoder) serveHTTP(t *testing.T, w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/team" {
		_ = json.NewEncoder(w).Encode(codersdk.Organization{MinimalOrganization: codersdk.MinimalOrganization{ID: c.team, Name: "team"}})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "v2" && parts[2] == "organizations" && parts[4] == "provisionerkeys" {
		if orgID, err := uuid.Parse(parts[3]); err == nil {
			if r.Method == http.MethodGet && len(parts) == 5 {
				if orgID == c.failList {
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(codersdk.Response{Message: "injected list failure"})
					return
				}
				keys := []codersdk.ProvisionerKey{}
				for _, key := range c.keys {
					if key.OrganizationID == orgID {
						keys = append(keys, key)
					}
				}
				_ = json.NewEncoder(w).Encode(keys)
				return
			}
			if r.Method == http.MethodDelete && len(parts) == 6 {
				maps.DeleteFunc(c.keys, func(_ uuid.UUID, key codersdk.ProvisionerKey) bool {
					return key.OrganizationID == orgID && key.Name == parts[5]
				})
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
}

// TestCoderProvisionerKey_OrganizationRenamed checks a provisioner whose
// applied key is in organization A, named team. A is renamed, and another
// organization takes the name team, at the same control plane. A lookup of
// the name team then does not find the key. Coder still holds the key in A,
// so a rotation must delete it there by its recorded IDs, and so must the
// deletion of the provisioner.
func TestCoderProvisionerKey_OrganizationRenamed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// legacy removes the control plane UID from status, as an operator
		// before UID records wrote it.
		legacy bool
		// loseSecret deletes the provisioner key Secret after the rename.
		loseSecret bool
		// changeTags changes spec.tags after the rename.
		changeTags bool
	}{
		{name: "status without control plane UID", legacy: true},
		{name: "Secret loss", loseSecret: true},
		{name: "status without control plane UID, Secret loss", legacy: true, loseSecret: true},
		{name: "tags change", changeTags: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-orgrename")
			orgA, orgB := uuid.New(), uuid.New()
			keyA, keyB := uuid.New(), uuid.New()
			coder := &renamedOrgCoder{
				SDKClient: coderbootstrap.NewSDKClient(),
				fake:      f.bootstrapClient,
				keys:      map[uuid.UUID]codersdk.ProvisionerKey{},
				team:      orgA,
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { coder.serveHTTP(t, w, r) }))
			t.Cleanup(server.Close)
			setControlPlaneURL(ctx, t, f, server.URL)
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{
				{OrganizationID: orgA, KeyID: keyA, KeyName: "provisioner", Key: "material-a"},
				{OrganizationID: orgB, KeyID: keyB, KeyName: "provisioner", Key: "material-b"},
			}
			f.reconciler.BootstrapClient = coder

			p := f.createProvisioner(ctx, t, nil, nil)
			p.Spec.OrganizationName = "team"
			require.NoError(t, k8sClient.Update(ctx, p))
			f.reconcile(ctx, t, p)
			f.reconcile(ctx, t, p)
			require.Equal(t, keyA.String(), f.get(ctx, t, p).Status.ProvisionerKeyID)
			if tt.legacy {
				require.NoError(t, k8sClient.Status().Patch(ctx, f.get(ctx, t, p),
					ctrlclient.RawPatch(types.MergePatchType, []byte(`{"status":{"controlPlaneUID":null}}`))))
			}

			coder.mu.Lock()
			coder.team = orgB
			coder.mu.Unlock()
			if tt.loseSecret {
				require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: p.Name + "-provisioner-key", Namespace: p.Namespace}}))
			}
			if tt.changeTags {
				latest := f.get(ctx, t, p)
				latest.Spec.Tags = map[string]string{"scope": "organization"}
				require.NoError(t, k8sClient.Update(ctx, latest))
			}
			f.reconcile(ctx, t, p)
			if tt.loseSecret || tt.changeTags {
				require.Equal(t, keyB.String(), f.get(ctx, t, p).Status.ProvisionerKeyID, "the rotation must create a new key")
				coder.mu.Lock()
				require.NotContains(t, coder.keys, keyA, "the rotation must delete the replaced key in the renamed organization by ID")
				coder.mu.Unlock()
			}
			require.NoError(t, k8sClient.Delete(ctx, f.get(ctx, t, p)))
			f.reconcile(ctx, t, p)
			requireProvisionerGone(ctx, t, types.NamespacedName{Name: p.Name, Namespace: p.Namespace})

			coder.mu.Lock()
			defer coder.mu.Unlock()
			require.NotContains(t, coder.keys, keyA, "deleting the provisioner must revoke its recorded key in the renamed organization by ID")
			require.Empty(t, coder.keys, "deleting the provisioner must leave none of its keys in Coder")
		})
	}
}

// TestCoderProvisionerKey_UpgradedStatusKeepsTargetRecords checks status from
// an older operator with two unpublished records of the target key under
// different organization IDs: the organization named team was renamed and
// its name reused twice. Coder still holds both keys. The rotation must
// delete each key by its recorded IDs before it drops the record, and keep a
// record whose key Coder did not confirm gone.
func TestCoderProvisionerKey_UpgradedStatusKeepsTargetRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// failOlderList fails the key list in the organization of the older
		// record until the first reconcile after the upgrade returns.
		failOlderList bool
		// published records the keys as published, so that nothing but the
		// records themselves calls for a rotation.
		published bool
	}{
		{name: "both keys deleted"},
		{name: "failed delete keeps the record", failOlderList: true},
		{name: "published records", published: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-upgraded")
			orgA, orgB, orgC, orgD := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			keyA, olderKey, newerKey, keyD := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			coder := &renamedOrgCoder{
				SDKClient: coderbootstrap.NewSDKClient(),
				fake:      f.bootstrapClient,
				keys:      map[uuid.UUID]codersdk.ProvisionerKey{},
				team:      orgA,
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { coder.serveHTTP(t, w, r) }))
			t.Cleanup(server.Close)
			setControlPlaneURL(ctx, t, f, server.URL)
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{
				{OrganizationID: orgA, KeyID: keyA, KeyName: "provisioner", Key: "material-a"},
				{OrganizationID: orgD, KeyID: keyD, KeyName: "provisioner", Key: "material-d"},
			}
			f.reconciler.BootstrapClient = coder

			p := f.createProvisioner(ctx, t, nil, nil)
			p.Spec.OrganizationName = "team"
			require.NoError(t, k8sClient.Update(ctx, p))
			f.reconcile(ctx, t, p)
			f.reconcile(ctx, t, p)
			require.Equal(t, keyA.String(), f.get(ctx, t, p).Status.ProvisionerKeyID)

			older := codersdk.ProvisionerKey{ID: olderKey, OrganizationID: orgB, Name: "provisioner"}
			newer := codersdk.ProvisionerKey{ID: newerKey, OrganizationID: orgC, Name: "provisioner"}
			coder.mu.Lock()
			coder.keys[olderKey] = older
			coder.keys[newerKey] = newer
			coder.team = orgD
			if tt.failOlderList {
				coder.failList = orgB
			}
			coder.mu.Unlock()
			latest := f.get(ctx, t, p)
			for _, key := range []codersdk.ProvisionerKey{older, newer} {
				latest.Status.PendingKeys = append(latest.Status.PendingKeys, coderv1alpha1.CoderProvisionerKeyRecord{
					ControlPlaneRefName: f.controlPlane.Name,
					CoderURL:            server.URL,
					OrganizationName:    "team",
					KeyName:             "provisioner",
					OrganizationID:      key.OrganizationID.String(),
					KeyID:               key.ID.String(),
					Unpublished:         !tt.published,
				})
			}
			require.NoError(t, k8sClient.Status().Update(ctx, latest))

			if tt.failOlderList {
				_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}})
				require.Error(t, err, "the rotation must stop when Coder does not confirm a recorded key gone")
				recorded := f.get(ctx, t, p).Status.PendingKeys
				for _, key := range []uuid.UUID{olderKey, newerKey} {
					require.True(t, slices.ContainsFunc(recorded, func(record coderv1alpha1.CoderProvisionerKeyRecord) bool {
						return record.KeyID == key.String()
					}), "status must keep the record of live key %s", key)
				}
				coder.mu.Lock()
				require.Contains(t, coder.keys, olderKey)
				coder.failList = uuid.Nil
				coder.mu.Unlock()
			}

			f.reconcile(ctx, t, p)
			rotated := f.get(ctx, t, p)
			require.Equal(t, keyD.String(), rotated.Status.ProvisionerKeyID, "the rotation must create a new key")
			require.Empty(t, rotated.Status.PendingKeys)
			coder.mu.Lock()
			require.NotContains(t, coder.keys, olderKey, "the rotation must delete the older recorded key by ID")
			require.NotContains(t, coder.keys, newerKey, "the rotation must delete the newer recorded key by ID")
			require.NotContains(t, coder.keys, keyA, "the rotation must delete the replaced key by ID")
			coder.mu.Unlock()

			require.NoError(t, k8sClient.Delete(ctx, rotated))
			f.reconcile(ctx, t, p)
			requireProvisionerGone(ctx, t, types.NamespacedName{Name: p.Name, Namespace: p.Namespace})
			coder.mu.Lock()
			defer coder.mu.Unlock()
			require.Empty(t, coder.keys, "deleting the provisioner must leave none of its keys in Coder")
		})
	}
}

// TestCoderProvisionerKey_StatusWithoutUIDReplacedKeyBound checks status
// without a control plane UID when P moves from the control plane coder to
// coder-b, and Coder fails the delete of the replaced key. The control plane
// coder is then deleted and created again at the same URL with another
// operator token. The new token must not be used for the replaced key: its
// entry stays, and P reports it.
func TestCoderProvisionerKey_StatusWithoutUIDReplacedKeyBound(t *testing.T) {
	t.Parallel()

	const (
		oldURL   = "http://coder-old.example.com"
		newToken = "operator-token-new"
	)
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-bound")
	setControlPlaneURL(ctx, t, f, oldURL)
	keyOld := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-old"}
	keyNew := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-new"}
	f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{keyOld, keyNew}
	p := f.createProvisioner(ctx, t, nil, nil)
	pName := types.NamespacedName{Name: p.Name, Namespace: p.Namespace}
	f.reconcile(ctx, t, p)
	f.reconcile(ctx, t, p)
	require.Equal(t, keyOld.KeyID.String(), f.get(ctx, t, p).Status.ProvisionerKeyID)
	require.NoError(t, k8sClient.Status().Patch(ctx, f.get(ctx, t, p),
		ctrlclient.RawPatch(types.MergePatchType, []byte(`{"status":{"controlPlaneUID":null}}`))))

	createControlPlaneWithToken(ctx, t, f.namespace, "coder-b", "http://coder-b.example.com", "operator-token-b")
	latest := f.get(ctx, t, p)
	latest.Spec.ControlPlaneRef.Name = "coder-b"
	require.NoError(t, k8sClient.Update(ctx, latest))
	f.bootstrapClient.deleteKeyByIDErr = errors.New("injected delete failure")
	_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: pName})
	require.NoError(t, err)
	require.Equal(t, keyNew.KeyID.String(), f.get(ctx, t, p).Status.ProvisionerKeyID, "P must move its key to coder-b")

	require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: f.namespace}}))
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "coder-operator-token", Namespace: f.namespace}}))
	createControlPlaneWithToken(ctx, t, f.namespace, "coder", oldURL, newToken)
	f.bootstrapClient.deleteKeyByIDErr = nil
	_, err = f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: pName})
	require.NoError(t, err)

	for _, d := range keyDeletes(f.bootstrapClient) {
		require.False(t, d.keyID == keyOld.KeyID.String() && d.token == newToken,
			"the new control plane's token must not be used for the old key, sent %+v", keyDeletes(f.bootstrapClient))
	}
	latest = f.get(ctx, t, p)
	require.True(t, slices.ContainsFunc(latest.Status.PendingKeys, func(record coderv1alpha1.CoderProvisionerKeyRecord) bool {
		return record.KeyID == keyOld.KeyID.String()
	}), "the record of the replaced key must stay")
	report := meta.FindStatusCondition(latest.Status.Conditions, coderv1alpha1.CoderProvisionerConditionControlPlaneChangedKeyKept)
	require.NotNil(t, report, "P must report the record that it keeps")
	require.Equal(t, metav1.ConditionTrue, report.Status)
}
