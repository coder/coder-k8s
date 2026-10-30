package controller_test

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

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
)

// keyRecordingClient checks that status records every key create before the
// create reaches Coder, and can fail the revoke of one key.
type keyRecordingClient struct {
	*fakeBootstrapClient
	t           *testing.T
	provisioner types.NamespacedName
	// onCreated runs after Coder returned a new key.
	onCreated func(coderbootstrap.EnsureProvisionerKeyResponse)
	// failRevoke makes the delete of this key ID fail.
	failRevoke uuid.UUID
}

func (c *keyRecordingClient) EnsureProvisionerKey(ctx context.Context, req coderbootstrap.EnsureProvisionerKeyRequest) (coderbootstrap.EnsureProvisionerKeyResponse, error) {
	latest := &coderv1alpha1.CoderProvisioner{}
	require.NoError(c.t, k8sClient.Get(ctx, c.provisioner, latest))
	recorded := slices.ContainsFunc(latest.Status.PendingKeys, func(record coderv1alpha1.CoderProvisionerKeyRecord) bool {
		return record.CoderURL == req.CoderURL && record.OrganizationName == req.OrganizationName && record.KeyName == req.KeyName
	})
	if !recorded {
		c.t.Errorf("key %q at %s can be created without a record in status.pendingKeys %+v", req.KeyName, req.CoderURL, latest.Status.PendingKeys)
	}
	response, err := c.fakeBootstrapClient.EnsureProvisionerKey(ctx, req)
	if err == nil && response.Key != "" && c.onCreated != nil {
		c.onCreated(response)
	}
	return response, err
}

func (c *keyRecordingClient) DeleteProvisionerKeyByID(ctx context.Context, req coderbootstrap.DeleteProvisionerKeyByIDRequest) error {
	if req.KeyID == c.failRevoke {
		return errors.New("injected revoke failure")
	}
	return c.fakeBootstrapClient.DeleteProvisionerKeyByID(ctx, req)
}

// keyDelete is one key delete request that reached the fake Coder.
type keyDelete struct {
	coderURL, token, keyName, keyID string
}

func keyDeletes(client *fakeBootstrapClient) []keyDelete {
	var deletes []keyDelete
	for _, req := range client.deleteKeyRequests {
		deletes = append(deletes, keyDelete{coderURL: req.CoderURL, token: req.SessionToken, keyName: req.KeyName})
	}
	for _, req := range client.deleteKeyByIDRequests {
		deletes = append(deletes, keyDelete{coderURL: req.CoderURL, token: req.SessionToken, keyID: req.KeyID.String()})
	}
	return deletes
}

func createControlPlaneWithToken(ctx context.Context, t *testing.T, namespace, name, url, token string) {
	t.Helper()
	createTestControlPlane(ctx, t, namespace, name, url)
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: name + "-operator-token", Namespace: namespace}, secret))
	secret.Data[coderv1alpha1.DefaultTokenSecretKey] = []byte(token)
	require.NoError(t, k8sClient.Update(ctx, secret))
}

// TestCoderProvisionerKey_IntermediateEndpointRevoked checks a control plane
// target that changes twice, A to B to C, while the attempt at B fails after
// Coder created the key at B. When status recorded the ID of key B, both the
// next rotation and deletion must delete key B at B, with the operator token
// of control plane B. When it did not, nothing shows that this provisioner
// created the key at B, so key B must stay and the provisioner must report
// it. No delete may send a token to the endpoint of another control plane.
func TestCoderProvisionerKey_IntermediateEndpointRevoked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// statusWriteFails makes the status write that records key B fail,
		// and the revoke of the unpublished key B fail too, as after a crash
		// right after the create, so status never holds the ID of key B.
		// Otherwise the Secret write fails.
		statusWriteFails bool
		// deleteAtC deletes the provisioner instead of reconciling at C.
		deleteAtC bool
		// revokeFailsOnce fails the first delete of key B at C. The record
		// must stay until a later reconcile deletes the key.
		revokeFailsOnce bool
	}{
		{name: "secret write fails at B, rotation at C"},
		{name: "secret write fails at B, deletion at C", deleteAtC: true},
		{name: "key B unrecorded by ID, rotation at C", statusWriteFails: true},
		{name: "key B unrecorded by ID, deletion at C", statusWriteFails: true, deleteAtC: true},
		{name: "revoke of key B fails once, rotation at C", revokeFailsOnce: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-intermediate")
			createControlPlaneWithToken(ctx, t, f.namespace, "coder-b", "https://coder-b.example.com", "operator-token-b")
			createControlPlaneWithToken(ctx, t, f.namespace, "coder-c", "https://coder-c.example.com", "operator-token-c")
			apiURL := func(controlPlane string) string {
				return "http://" + controlPlane + "." + f.namespace + ".svc.cluster.local:80"
			}
			tokenAt := map[string]string{
				apiURL("coder"):   testOperatorToken,
				apiURL("coder-b"): "operator-token-b",
				apiURL("coder-c"): "operator-token-c",
			}
			keyA := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-a"}
			keyB := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-b"}
			keyC := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-c"}
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{keyA, keyB, keyC}
			if tt.statusWriteFails {
				f.bootstrapClient.provisionerKeyByName = func(req coderbootstrap.ProvisionerKeyByNameRequest) *codersdk.ProvisionerKey {
					if req.CoderURL != apiURL("coder-b") {
						return nil
					}
					return &codersdk.ProvisionerKey{ID: keyB.KeyID, OrganizationID: keyB.OrganizationID, Name: keyB.KeyName}
				}
			}

			provisioner := f.createProvisioner(ctx, t, nil, nil)
			name := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
			keySecretName := provisioner.Name + "-provisioner-key"
			client := &keyRecordingClient{fakeBootstrapClient: f.bootstrapClient, t: t, provisioner: name}
			f.reconciler.BootstrapClient = client
			var failSecretWrite, failStatusWrite bool
			base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
			require.NoError(t, err)
			f.reconciler.Client = interceptor.NewClient(base, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
					if _, ok := obj.(*coderv1alpha1.CoderProvisioner); ok && failStatusWrite {
						failStatusWrite = false
						return apierrors.NewServiceUnavailable("injected status write failure")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
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

			f.reconcile(ctx, t, provisioner)
			f.reconcile(ctx, t, provisioner)
			require.Equal(t, keyA.KeyID.String(), f.get(ctx, t, provisioner).Status.ProvisionerKeyID)

			moveTo("coder-b")
			if tt.statusWriteFails {
				client.failRevoke = keyB.KeyID
				client.onCreated = func(created coderbootstrap.EnsureProvisionerKeyResponse) {
					failStatusWrite = created.KeyID == keyB.KeyID
				}
			} else {
				failSecretWrite = true
			}
			_, err = f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
			require.Error(t, err, "the injected failure must fail the attempt at B")
			require.Equal(t, 2, f.bootstrapClient.provisionerKeyCalls, "Coder must have created key B")
			client.failRevoke = uuid.Nil
			client.onCreated = nil

			moveTo("coder-c")
			if tt.deleteAtC {
				require.NoError(t, k8sClient.Delete(ctx, f.get(ctx, t, provisioner)))
			}
			if tt.revokeFailsOnce {
				client.failRevoke = keyB.KeyID
				result, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
				require.NoError(t, err)
				require.Positive(t, result.RequeueAfter, "a failed delete must schedule another attempt")
				require.True(t, pendingKeyRecorded(f.get(ctx, t, provisioner), keyB.KeyID), "a failed delete must keep the record of key B")
				client.failRevoke = uuid.Nil
			}
			_, err = f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
			require.NoError(t, err)

			deletes := keyDeletes(f.bootstrapClient)
			if tt.statusWriteFails {
				require.False(t, slices.ContainsFunc(deletes, func(d keyDelete) bool { return d.coderURL == apiURL("coder-b") }),
					"a key whose ID status never held must not be deleted, got %+v", deletes)
			} else {
				require.Contains(t, deletes, keyDelete{coderURL: apiURL("coder-b"), token: "operator-token-b", keyID: keyB.KeyID.String()},
					"key B must be deleted at B with the operator token of B")
			}
			require.Contains(t, deletes, keyDelete{coderURL: apiURL("coder"), token: testOperatorToken, keyID: keyA.KeyID.String()},
				"key A must be deleted at A with the operator token of A")
			for _, d := range deletes {
				want, known := tokenAt[d.coderURL]
				require.True(t, known, "delete sent to %s, which no record names", d.coderURL)
				require.Equal(t, want, d.token, "delete at %s must use the operator token of that control plane", d.coderURL)
			}

			if tt.deleteAtC {
				requireProvisionerGone(ctx, t, name)
				return
			}
			require.False(t, slices.ContainsFunc(deletes, func(d keyDelete) bool { return d.coderURL == apiURL("coder-c") }),
				"nothing may delete key C, which the Secret holds")
			final := f.get(ctx, t, provisioner)
			require.Equal(t, keyC.KeyID.String(), final.Status.ProvisionerKeyID)
			require.Equal(t, "https://coder-c.example.com", final.Status.ControlPlaneURL)
			unverified := meta.FindStatusCondition(final.Status.Conditions, coderv1alpha1.CoderProvisionerConditionUnverifiedKeyKept)
			if tt.statusWriteFails {
				require.Len(t, final.Status.PendingKeys, 1)
				require.Equal(t, apiURL("coder-b"), final.Status.PendingKeys[0].CoderURL, "the record of key B must stay")
				require.Empty(t, final.Status.PendingKeys[0].KeyID)
				require.NotNil(t, unverified, "the provisioner must report the key that it left at B")
				require.Equal(t, metav1.ConditionTrue, unverified.Status)
				return
			}
			require.Empty(t, final.Status.PendingKeys, "every replaced key must be confirmed deleted")
			require.Nil(t, unverified)
		})
	}
}

// TestCoderProvisionerKey_UnpublishedKeyIsRotated checks keys that an
// earlier reconcile created at the target and did not publish. The next
// reconcile must replace them with a key that status records as applied, also
// when nothing else asks for a rotation, and deletion must delete that key.
func TestCoderProvisionerKey_UnpublishedKeyIsRotated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// fail makes one reconcile leave an unpublished key at the target.
		fail func(ctx context.Context, t *testing.T, f provisionerCredentialFixture, provisioner *coderv1alpha1.CoderProvisioner, unpublished coderbootstrap.EnsureProvisionerKeyResponse)
	}{
		{
			// The create reached Coder, but the operator got no answer. The
			// spec change that caused the rotation is then reverted.
			name: "create answer lost, spec reverted",
			fail: func(ctx context.Context, t *testing.T, f provisionerCredentialFixture, provisioner *coderv1alpha1.CoderProvisioner, _ coderbootstrap.EnsureProvisionerKeyResponse) {
				latest := f.get(ctx, t, provisioner)
				latest.Spec.Tags = map[string]string{"region": "rotated"}
				require.NoError(t, k8sClient.Update(ctx, latest))
				f.bootstrapClient.provisionerKeyErr = errors.New("injected lost create answer")
				_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}})
				require.Error(t, err)
				f.bootstrapClient.provisionerKeyErr = nil
				latest = f.get(ctx, t, provisioner)
				latest.Spec.Tags = nil
				require.NoError(t, k8sClient.Update(ctx, latest))
			},
		},
		{
			// The Secret holds the new key, but the status write after it fails.
			name: "status write after the Secret write fails",
			fail: func(ctx context.Context, t *testing.T, f provisionerCredentialFixture, provisioner *coderv1alpha1.CoderProvisioner, unpublished coderbootstrap.EnsureProvisionerKeyResponse) {
				require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: provisioner.Name + "-provisioner-key", Namespace: provisioner.Namespace}}))
				base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
				require.NoError(t, err)
				failStatusWrite := true
				f.reconciler.Client = interceptor.NewClient(base, interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
						if p, ok := obj.(*coderv1alpha1.CoderProvisioner); ok && failStatusWrite && p.Status.ProvisionerKeyID == unpublished.KeyID.String() {
							failStatusWrite = false
							return apierrors.NewServiceUnavailable("injected status write failure")
						}
						return c.SubResource(sub).Update(ctx, obj, opts...)
					},
				})
				_, err = f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}})
				require.Error(t, err)
				f.reconciler.Client = k8sClient
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-unpublished")
			first := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-1"}
			unpublished := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: first.OrganizationID, KeyID: uuid.New(), KeyName: "provisioner", Key: "material-2"}
			// Coder lists the unpublished key, without plaintext, until the
			// rotation deletes it by name.
			listed := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: unpublished.OrganizationID, KeyID: unpublished.KeyID, KeyName: "provisioner"}
			final := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: first.OrganizationID, KeyID: uuid.New(), KeyName: "provisioner", Key: "material-3"}
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{first, unpublished, listed, final}
			provisioner := f.createProvisioner(ctx, t, nil, nil)
			f.reconcile(ctx, t, provisioner)
			f.reconcile(ctx, t, provisioner)

			tt.fail(ctx, t, f, provisioner, unpublished)
			deletesBefore := len(f.bootstrapClient.deleteKeyRequests)
			f.reconcile(ctx, t, provisioner)

			latest := f.get(ctx, t, provisioner)
			require.Equal(t, final.KeyID.String(), latest.Status.ProvisionerKeyID, "status must name the key in the Secret")
			require.Empty(t, latest.Status.PendingKeys)
			secret := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: provisioner.Name + "-provisioner-key", Namespace: provisioner.Namespace}, secret))
			require.Equal(t, "material-3", string(secret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]))
			require.Contains(t, f.bootstrapClient.deleteKeyRequests[deletesBefore:],
				deleteKeyRequest{CoderURL: internalCoderURL(f), SessionToken: testOperatorToken, OrganizationName: "default", KeyName: "provisioner"},
				"the rotation must delete the unpublished key")

			require.NoError(t, k8sClient.Delete(ctx, latest))
			f.reconcile(ctx, t, provisioner)
			require.True(t, slices.ContainsFunc(f.bootstrapClient.deleteKeyByIDRequests, func(req coderbootstrap.DeleteProvisionerKeyByIDRequest) bool {
				return req.KeyID == final.KeyID
			}), "deletion must delete the key in the Secret, got %+v", f.bootstrapClient.deleteKeyByIDRequests)
			requireProvisionerGone(ctx, t, types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace})
		})
	}
}

// TestCoderProvisionerKey_FailedRotationKeepsKeyIDs checks that a rotation
// that fails before its create keeps the IDs of the unpublished key at the
// target, so that a later delete still finds that key by ID.
func TestCoderProvisionerKey_FailedRotationKeepsKeyIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-keep-ids")
	first := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-1"}
	unpublished := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: first.OrganizationID, KeyID: uuid.New(), KeyName: "provisioner", Key: "material-2"}
	f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{first, unpublished}
	provisioner := f.createProvisioner(ctx, t, nil, nil)
	name := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)

	keySecretName := provisioner.Name + "-provisioner-key"
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: keySecretName, Namespace: provisioner.Namespace}}))
	base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)
	f.reconciler.Client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			if s, ok := obj.(*corev1.Secret); ok && s.Name == keySecretName {
				return apierrors.NewServiceUnavailable("injected Secret write failure")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	_, err = f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
	require.Error(t, err, "the injected Secret write failure must fail the rotation")
	require.True(t, pendingKeyRecorded(f.get(ctx, t, provisioner), unpublished.KeyID))

	f.bootstrapClient.provisionerKeyErr = errors.New("injected Coder failure")
	_, err = f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
	require.Error(t, err)
	require.True(t, pendingKeyRecorded(f.get(ctx, t, provisioner), unpublished.KeyID),
		"a failed rotation must keep the IDs of the unpublished key")
}

// setControlPlaneURL sets status.url of the fixture's control plane.
func setControlPlaneURL(ctx context.Context, t *testing.T, f provisionerCredentialFixture, url string) {
	t.Helper()
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: f.controlPlane.Name, Namespace: f.namespace}, controlPlane))
	controlPlane.Status.URL = url
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))
}

// TestCoderProvisionerKey_RevertAfterFailedRotation checks a rotation from
// endpoint A to B that fails after its create, followed by a revert to A.
// The Secret then holds key B, which is valid only at B, or key A, which the
// rotation deleted. The next reconcile must delete key B and write a new key
// for A.
func TestCoderProvisionerKey_RevertAfterFailedRotation(t *testing.T) {
	t.Parallel()

	for _, secretWritten := range []bool{true, false} {
		name := "status write after the Secret write fails"
		if !secretWritten {
			name = "Secret write fails"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-revert")
			first := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-1"}
			atB := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: uuid.New(), KeyID: uuid.New(), KeyName: "provisioner", Key: "material-b"}
			final := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: first.OrganizationID, KeyID: uuid.New(), KeyName: "provisioner", Key: "material-3"}
			f.bootstrapClient.provisionerKeyResponses = []coderbootstrap.EnsureProvisionerKeyResponse{first, atB, final}
			provisioner := f.createProvisioner(ctx, t, nil, nil)
			name := types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}
			keySecret := types.NamespacedName{Name: provisioner.Name + "-provisioner-key", Namespace: provisioner.Namespace}
			f.reconcile(ctx, t, provisioner)
			f.reconcile(ctx, t, provisioner)

			const urlB = "http://coder-b.example.com"
			setControlPlaneURL(ctx, t, f, urlB)
			base, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme})
			require.NoError(t, err)
			f.reconciler.Client = interceptor.NewClient(base, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
					if p, ok := obj.(*coderv1alpha1.CoderProvisioner); ok && secretWritten && p.Status.ProvisionerKeyID == atB.KeyID.String() {
						return apierrors.NewServiceUnavailable("injected status write failure")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
					if s, ok := obj.(*corev1.Secret); ok && !secretWritten && s.Name == keySecret.Name {
						return apierrors.NewServiceUnavailable("injected Secret write failure")
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			_, err = f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: name})
			require.Error(t, err, "the injected failure must fail the rotation to B")

			f.reconciler.Client = k8sClient
			setControlPlaneURL(ctx, t, f, testCoderOrigin)
			f.reconcile(ctx, t, provisioner)

			require.Contains(t, keyDeletes(f.bootstrapClient), keyDelete{coderURL: urlB, token: testOperatorToken, keyID: atB.KeyID.String()},
				"key B must be deleted at B")
			secret := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, keySecret, secret))
			require.Equal(t, "material-3", string(secret.Data[coderv1alpha1.DefaultProvisionerKeySecretKey]), "the Secret must hold a new key for A")
			latest := f.get(ctx, t, provisioner)
			require.Equal(t, final.KeyID.String(), latest.Status.ProvisionerKeyID)
			require.Equal(t, testCoderOrigin, latest.Status.ControlPlaneURL)
			require.Empty(t, latest.Status.PendingKeys)
		})
	}
}

// keySlot is the place of a provisioner key in Coder: Coder allows one key
// per name in an organization.
type keySlot struct{ coderURL, organizationName, keyName string }

// keyStoreClient is a Coder that keeps provisioner keys, one per slot. Each
// key create at a URL in failEnsureAt fails before it reaches Coder, and each
// key lookup by name at failLookupAt fails.
type keyStoreClient struct {
	*fakeBootstrapClient
	keys         map[keySlot]uuid.UUID
	failEnsureAt []string
	failLookupAt string
}

func newKeySlot(coderURL, organizationName, keyName string) keySlot {
	return keySlot{coderURL: coderURL, organizationName: cmp.Or(organizationName, "default"), keyName: keyName}
}

func (s keySlot) organizationID() uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(s.coderURL+"/"+s.organizationName))
}

func (c *keyStoreClient) EnsureProvisionerKey(_ context.Context, req coderbootstrap.EnsureProvisionerKeyRequest) (coderbootstrap.EnsureProvisionerKeyResponse, error) {
	if slices.Contains(c.failEnsureAt, req.CoderURL) {
		return coderbootstrap.EnsureProvisionerKeyResponse{}, errors.New("injected connection refused")
	}
	slot := newKeySlot(req.CoderURL, req.OrganizationName, req.KeyName)
	response := coderbootstrap.EnsureProvisionerKeyResponse{OrganizationID: slot.organizationID(), KeyName: req.KeyName}
	if keyID, ok := c.keys[slot]; ok {
		response.KeyID = keyID
		return response, nil
	}
	response.KeyID = uuid.New()
	response.Key = "material-" + response.KeyID.String()
	c.keys[slot] = response.KeyID
	return response, nil
}

func (c *keyStoreClient) DeleteProvisionerKey(_ context.Context, coderURL, _, organizationName, keyName string) error {
	delete(c.keys, newKeySlot(coderURL, organizationName, keyName))
	return nil
}

func (c *keyStoreClient) DeleteProvisionerKeyByID(_ context.Context, req coderbootstrap.DeleteProvisionerKeyByIDRequest) error {
	maps.DeleteFunc(c.keys, func(slot keySlot, keyID uuid.UUID) bool {
		return slot.coderURL == req.CoderURL && slot.organizationID() == req.OrganizationID && keyID == req.KeyID
	})
	return nil
}

func (c *keyStoreClient) ProvisionerKeyByName(_ context.Context, req coderbootstrap.ProvisionerKeyByNameRequest) (*codersdk.ProvisionerKey, error) {
	if req.CoderURL == c.failLookupAt {
		return nil, errors.New("injected lookup failure")
	}
	slot := newKeySlot(req.CoderURL, req.OrganizationName, req.KeyName)
	keyID, ok := c.keys[slot]
	if !ok {
		return nil, nil
	}
	return &codersdk.ProvisionerKey{ID: keyID, OrganizationID: slot.organizationID(), Name: slot.keyName}, nil
}

// TestCoderProvisionerKey_IDLessRecordKeepsOtherProvisionersKey checks a
// provisioner P whose target moves from A to endpoint B, where provisioner Q
// holds its key under the same key name, and then to endpoint C. P records
// each create without key IDs, and each create fails before it reaches
// Coder. Neither the cleanup after P moves back to A nor the deletion of P
// may delete the key of Q. After the move back, P must report each entry it
// keeps, and drop an entry and its report once Coder has no key with its
// name.
func TestCoderProvisionerKey_IDLessRecordKeepsOtherProvisionersKey(t *testing.T) {
	t.Parallel()

	for _, deleteP := range []bool{false, true} {
		name := "P moves back to A"
		if deleteP {
			name = "P is deleted"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProvisionerCredentialFixture(ctx, t, "cprov-idless")
			createControlPlaneWithToken(ctx, t, f.namespace, "coder-b", "https://coder-b.example.com", "operator-token-b")
			createControlPlaneWithToken(ctx, t, f.namespace, "coder-c", "https://coder-c.example.com", "operator-token-c")
			apiURL := func(controlPlane string) string {
				return "http://" + controlPlane + "." + f.namespace + ".svc.cluster.local:80"
			}
			urlB, urlC := apiURL("coder-b"), apiURL("coder-c")
			store := &keyStoreClient{fakeBootstrapClient: f.bootstrapClient, keys: map[keySlot]uuid.UUID{}}
			f.reconciler.BootstrapClient = store

			q := f.createKeyProvisioner(ctx, t, "q", "")
			q.Spec.ControlPlaneRef.Name = "coder-b"
			q.Spec.Key.Name = "provisioner"
			require.NoError(t, k8sClient.Update(ctx, q))
			f.reconcile(ctx, t, q)
			f.reconcile(ctx, t, q)
			slotB := newKeySlot(urlB, "", "provisioner")
			qKeyID := f.get(ctx, t, q).Status.ProvisionerKeyID
			require.Equal(t, store.keys[slotB].String(), qKeyID, "Q must hold the key named provisioner at B")

			p := f.createProvisioner(ctx, t, nil, nil)
			pName := types.NamespacedName{Name: p.Name, Namespace: p.Namespace}
			f.reconcile(ctx, t, p)
			f.reconcile(ctx, t, p)
			reconcileP := func() ctrl.Result {
				result, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: pName})
				require.NoError(t, err)
				return result
			}
			moveTo := func(controlPlane string) {
				latest := f.get(ctx, t, p)
				latest.Spec.ControlPlaneRef.Name = controlPlane
				require.NoError(t, k8sClient.Update(ctx, latest))
			}
			unverifiedReport := func() *metav1.Condition {
				return meta.FindStatusCondition(f.get(ctx, t, p).Status.Conditions, coderv1alpha1.CoderProvisionerConditionUnverifiedKeyKept)
			}

			store.failEnsureAt = []string{urlB, urlC}
			for _, controlPlane := range []string{"coder-b", "coder-c"} {
				moveTo(controlPlane)
				_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: pName})
				require.Error(t, err, "the create at %s must fail", controlPlane)
				require.True(t, slices.ContainsFunc(f.get(ctx, t, p).Status.PendingKeys, func(record coderv1alpha1.CoderProvisionerKeyRecord) bool {
					return record.CoderURL == apiURL(controlPlane) && record.KeyName == "provisioner" && record.KeyID == ""
				}), "P must record its create at %s without key IDs", controlPlane)
			}

			if deleteP {
				require.NoError(t, k8sClient.Delete(ctx, f.get(ctx, t, p)))
				f.reconcile(ctx, t, p)
				requireProvisionerGone(ctx, t, pName)
				require.Equal(t, qKeyID, store.keys[slotB].String(), "the key of Q at B must still exist")
				return
			}

			store.failLookupAt = urlC
			moveTo("coder")
			result := reconcileP()
			require.Equal(t, qKeyID, store.keys[slotB].String(), "the key of Q at B must still exist")
			require.Len(t, f.get(ctx, t, p).Status.PendingKeys, 2, "the entries at B and C must stay")
			report := unverifiedReport()
			require.NotNil(t, report, "P must report the entries that it keeps")
			require.Equal(t, metav1.ConditionTrue, report.Status)
			require.Contains(t, report.Message, urlB)
			require.Contains(t, report.Message, urlC)
			require.Positive(t, result.RequeueAfter, "P must check the entries again")

			delete(store.keys, slotB)
			reconcileP()
			require.Len(t, f.get(ctx, t, p).Status.PendingKeys, 1, "the entry at B must go once Coder has no key with its name")
			report = unverifiedReport()
			require.NotNil(t, report, "a failed lookup must keep the report of the entry at C")
			require.NotContains(t, report.Message, urlB, "the report must not name the entry at B any more")
			require.Contains(t, report.Message, urlC)

			store.failLookupAt = ""
			f.reconcile(ctx, t, p)
			require.Empty(t, f.get(ctx, t, p).Status.PendingKeys)
			require.Nil(t, unverifiedReport())
		})
	}
}
