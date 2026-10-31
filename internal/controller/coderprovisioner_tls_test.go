package controller_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/tlstest"
)

// TestCoderProvisionerKey_OperatorTLS checks the opt-in to
// spec.tls.operatorAccess on a control plane with built-in TLS: the applied
// key stays, every later Coder call goes to status.url with the CA bundle, a
// pending key recorded at the plain HTTP Service URL is deleted over HTTPS,
// and a missing CA bundle stops the Coder calls instead of falling back to
// HTTP.
func TestCoderProvisionerKey_OperatorTLS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-tls")
	httpURL := "http://coder." + f.namespace + ".svc.cluster.local:80"
	httpsURL := "https://coder." + f.namespace + ".svc.cluster.local:443"

	controlPlane := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: f.controlPlane.Name, Namespace: f.namespace}, controlPlane))
	controlPlane.Spec.TLS.SecretNames = []string{"coder-tls"}
	require.NoError(t, k8sClient.Update(ctx, controlPlane))
	controlPlane.Status.URL = httpsURL
	require.NoError(t, k8sClient.Status().Update(ctx, controlPlane))

	provisioner := f.createProvisioner(ctx, t, nil, nil)
	f.reconcile(ctx, t, provisioner)
	f.reconcile(ctx, t, provisioner)
	require.Len(t, f.bootstrapClient.provisionerKeyRequests, 1)
	require.Equal(t, httpURL, f.bootstrapClient.provisionerKeyRequests[0].CoderURL, "without operatorAccess, the key calls use the plain HTTP Service URL")
	require.Nil(t, f.bootstrapClient.provisionerKeyRequests[0].CoderTLS)
	appliedKeyID := f.get(ctx, t, provisioner).Status.ProvisionerKeyID
	require.NotEmpty(t, appliedKeyID)

	caConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-ca", Namespace: f.namespace},
		Data:       map[string]string{"ca.crt": string(tlstest.NewCA(t).PEM)},
	}
	require.NoError(t, k8sClient.Create(ctx, caConfigMap))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: f.controlPlane.Name, Namespace: f.namespace}, controlPlane))
	controlPlane.Spec.TLS.OperatorAccess = &coderv1alpha1.OperatorTLSSpec{CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"}}
	require.NoError(t, k8sClient.Update(ctx, controlPlane))

	// A key that an earlier reconcile created at the plain HTTP URL and did
	// not apply.
	leftover := coderv1alpha1.CoderProvisionerKeyRecord{
		ControlPlaneRefName: controlPlane.Name,
		ControlPlaneUID:     string(controlPlane.UID),
		CoderURL:            httpURL,
		OrganizationName:    "default",
		KeyName:             "provisioner-old",
		OrganizationID:      uuid.NewString(),
		KeyID:               uuid.NewString(),
	}
	latest := f.get(ctx, t, provisioner)
	latest.Status.PendingKeys = append(latest.Status.PendingKeys, leftover)
	require.NoError(t, k8sClient.Status().Update(ctx, latest))
	f.bootstrapClient.entitlementsRequests = nil

	f.reconcile(ctx, t, provisioner)
	latest = f.get(ctx, t, provisioner)
	require.Equal(t, appliedKeyID, latest.Status.ProvisionerKeyID, "opting in must not rotate the applied key")
	require.Len(t, f.bootstrapClient.provisionerKeyRequests, 1, "opting in must not create a key")
	require.Empty(t, latest.Status.PendingKeys, "the leftover key must be deleted")
	require.Len(t, f.bootstrapClient.deleteKeyByIDRequests, 1)
	deleted := f.bootstrapClient.deleteKeyByIDRequests[0]
	require.Equal(t, leftover.KeyID, deleted.KeyID.String())
	require.Equal(t, httpsURL, deleted.CoderURL, "the key recorded at the HTTP URL is deleted over HTTPS")
	require.NotNil(t, deleted.CoderTLS)
	require.NotEmpty(t, f.bootstrapClient.entitlementsRequests)
	for _, req := range f.bootstrapClient.entitlementsRequests {
		require.Equal(t, httpsURL, req.CoderURL)
		require.NotNil(t, req.CoderTLS)
	}

	require.NoError(t, k8sClient.Delete(ctx, caConfigMap))
	f.bootstrapClient.entitlementsRequests = nil
	_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}})
	require.NoError(t, err)
	require.Empty(t, f.bootstrapClient.entitlementsRequests, "a missing CA bundle must stop the Coder calls")
	condition := meta.FindStatusCondition(f.get(ctx, t, provisioner).Status.Conditions, coderv1alpha1.CoderProvisionerConditionOperatorAccessReady)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "OperatorTLSUnavailable", condition.Reason)
}
