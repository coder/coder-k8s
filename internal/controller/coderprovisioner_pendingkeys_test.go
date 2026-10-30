package controller_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// Coder created the key at B. Both the next rotation and deletion must delete
// the key at B, with the operator token of control plane B, and must send no
// token to the endpoint of another control plane.
func TestCoderProvisionerKey_IntermediateEndpointRevoked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// statusWriteFails makes the status write that records key B fail,
		// and the revoke of the unpublished key B fail too, as after a crash
		// right after the create. Otherwise the Secret write fails.
		statusWriteFails bool
		// deleteAtC deletes the provisioner instead of reconciling at C.
		deleteAtC bool
	}{
		{name: "secret write fails at B, rotation at C"},
		{name: "secret write fails at B, deletion at C", deleteAtC: true},
		{name: "key B unrecorded by ID, rotation at C", statusWriteFails: true},
		{name: "key B unrecorded by ID, deletion at C", statusWriteFails: true, deleteAtC: true},
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
			f.reconcile(ctx, t, provisioner)

			deletes := keyDeletes(f.bootstrapClient)
			revokedAtB := slices.ContainsFunc(deletes, func(d keyDelete) bool {
				return d.coderURL == apiURL("coder-b") && d.token == "operator-token-b" &&
					(d.keyID == keyB.KeyID.String() || (d.keyID == "" && d.keyName == keyB.KeyName))
			})
			require.True(t, revokedAtB, "key B must be deleted at B with the operator token of B, got %+v", deletes)
			for _, d := range deletes {
				want, known := tokenAt[d.coderURL]
				require.True(t, known, "delete sent to %s, which no record names", d.coderURL)
				require.Equal(t, want, d.token, "delete at %s must use the operator token of that control plane", d.coderURL)
			}

			if tt.deleteAtC {
				requireProvisionerGone(ctx, t, name)
				return
			}
			final := f.get(ctx, t, provisioner)
			require.Equal(t, keyC.KeyID.String(), final.Status.ProvisionerKeyID)
			require.Equal(t, "https://coder-c.example.com", final.Status.ControlPlaneURL)
			require.Empty(t, final.Status.PendingKeys, "every replaced key must be confirmed deleted")
		})
	}
}
