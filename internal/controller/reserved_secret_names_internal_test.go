package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

func reservedNameScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))
	return scheme
}

func requireReservedReason(t *testing.T, err error) {
	t.Helper()
	var credErr *credentialsecret.Error
	require.ErrorAs(t, err, &credErr)
	require.Equal(t, reasonSecretNameReserved, credErr.Reason)
}

// Plan S2 at the helper: every Secret name that another coder.com object in
// the namespace references or generates is reserved.
func TestReservedSecretName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := reservedNameScheme(t)
	key := func(name string) coderv1alpha1.SecretKeySelector {
		return coderv1alpha1.SecretKeySelector{Name: name, Key: "k"}
	}
	cp := &coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns"},
		Spec: coderv1alpha1.CoderControlPlaneSpec{
			LicenseSecretRef: ptr.To(key("license")),
			Database:         &coderv1alpha1.DatabaseSpec{ConnectionSecretRef: key("database")},
			EnvFrom:          []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "env-from"}}}},
			ExtraEnv: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "extra-env"}, Key: "k",
			}}}},
			// Builders trim these names, so the check trims them too.
			TLS:              coderv1alpha1.TLSSpec{SecretNames: []string{" tls "}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull"}},
			Certs:            coderv1alpha1.CertsSpec{Secrets: []coderv1alpha1.CertSecretSelector{{Name: "ca", Key: "ca.crt"}}},
			Expose: &coderv1alpha1.ExposeSpec{Ingress: &coderv1alpha1.IngressExposeSpec{TLS: &coderv1alpha1.IngressTLSExposeSpec{
				SecretName: "ingress-tls", WildcardSecretName: "wildcard-tls",
			}}},
			Volumes: []corev1.Volume{
				{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "volume"}}},
				{Name: "p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "projected"}}},
				}}}},
				{Name: "csi", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "d", NodePublishSecretRef: &corev1.LocalObjectReference{Name: "csi"}}}},
				{Name: "azure", VolumeSource: corev1.VolumeSource{AzureFile: &corev1.AzureFileVolumeSource{SecretName: "azure"}}},
				{Name: "iscsi", VolumeSource: corev1.VolumeSource{ISCSI: &corev1.ISCSIVolumeSource{SecretRef: &corev1.LocalObjectReference{Name: "iscsi"}}}},
				{Name: "flex", VolumeSource: corev1.VolumeSource{FlexVolume: &corev1.FlexVolumeSource{SecretRef: &corev1.LocalObjectReference{Name: "flex"}}}},
			},
			// Operator access is off, but turning it back on recreates the
			// token Secret, so its name stays reserved.
			OperatorAccess: coderv1alpha1.OperatorAccessSpec{Disabled: true},
		},
		Status: coderv1alpha1.CoderControlPlaneStatus{OperatorTokenSecretRef: ptr.To(key("recorded-token"))},
	}
	objects := []client.Object{
		cp,
		&coderv1alpha1.CoderWorkspaceProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "ns"},
			Spec: coderv1alpha1.WorkspaceProxySpec{
				Bootstrap:        &coderv1alpha1.ProxyBootstrapSpec{CredentialsSecretRef: key("proxy-bootstrap")},
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "proxy-pull"}},
			},
			Status: coderv1alpha1.WorkspaceProxyStatus{ProxyTokenSecretRef: ptr.To(key("proxy-recorded"))},
		},
		&coderv1alpha1.CoderProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "prov", Namespace: "ns"},
			Spec:       coderv1alpha1.CoderProvisionerSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "prov-pull"}}},
			Status:     coderv1alpha1.CoderProvisionerStatus{SecretRef: ptr.To(key("prov-recorded"))},
		},
		&coderv1alpha1.CoderAIProvider{ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "ns"}, Spec: coderv1alpha1.CoderAIProviderSpec{APIKeySecretRef: key("openai-key")}},
		&coderv1alpha1.CoderMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "mcp", Namespace: "ns"}, Spec: coderv1alpha1.CoderMCPServerSpec{Auth: coderv1alpha1.CoderMCPServerAuth{
			APIKey:        &coderv1alpha1.CoderMCPServerAPIKeyAuth{SecretRef: key("mcp-key")},
			CustomHeaders: []coderv1alpha1.CoderMCPServerHeader{{Name: "X", SecretRef: key("mcp-header")}},
			OAuth2:        &coderv1alpha1.CoderMCPServerOAuth2Auth{ClientSecretRef: key("mcp-oauth")},
		}}},
		// An object in another namespace reserves nothing here.
		&coderv1alpha1.CoderAIProvider{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "other"}, Spec: coderv1alpha1.CoderAIProviderSpec{APIKeySecretRef: key("free")}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	owner := &coderv1alpha1.CoderAIGatewayKey{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "ns", UID: "owner-uid"}}

	_, provisionerKeySecret, _ := provisionerKeyConfig(objects[2].(*coderv1alpha1.CoderProvisioner))
	for name, kind := range map[string]string{
		"coder-operator-token": "CoderControlPlane", "recorded-token": "CoderControlPlane", "license": "CoderControlPlane",
		"database": "CoderControlPlane", "env-from": "CoderControlPlane", "extra-env": "CoderControlPlane", "tls": "CoderControlPlane",
		"pull": "CoderControlPlane", "ca": "CoderControlPlane", "ingress-tls": "CoderControlPlane", "wildcard-tls": "CoderControlPlane",
		"volume": "CoderControlPlane", "projected": "CoderControlPlane", "csi": "CoderControlPlane", "azure": "CoderControlPlane",
		"iscsi": "CoderControlPlane", "flex": "CoderControlPlane", "proxy-bootstrap": "CoderWorkspaceProxy",
		"proxy-proxy-token": "CoderWorkspaceProxy", "proxy-pull": "CoderWorkspaceProxy", "proxy-recorded": "CoderWorkspaceProxy",
		"prov-pull": "CoderProvisioner", "prov-recorded": "CoderProvisioner",
		provisionerKeySecret: "CoderProvisioner", "openai-key": "CoderAIProvider", "mcp-key": "CoderMCPServer",
		"mcp-header": "CoderMCPServer", "mcp-oauth": "CoderMCPServer",
	} {
		err := reservedSecretName(ctx, c, owner, name)
		requireReservedReason(t, err)
		require.ErrorContains(t, err, kind, name)
	}
	require.NoError(t, reservedSecretName(ctx, c, owner, "free"))
	require.NoError(t, reservedSecretName(ctx, c, owner, "coder-ai-gateway-key"))

	// A kind whose CRD is not installed is skipped. Other list errors stop.
	for _, tc := range []struct {
		listErr error
		wantErr bool
	}{
		{listErr: &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "coder.com", Kind: "CoderMCPServer"}}},
		{listErr: apierrors.NewServiceUnavailable("down"), wantErr: true},
	} {
		failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*coderv1alpha1.CoderMCPServerList); ok {
					return tc.listErr
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
		err := reservedSecretName(ctx, failing, owner, "free")
		require.Equal(t, tc.wantErr, err != nil, "%v", err)
	}
}

// Among CoderAIGatewayKey objects that name the same Secret, the oldest one
// (by creation time, then name) keeps it, so two objects never block each
// other.
func TestReservedSecretNameGatewayKeyOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t0 := metav1.Unix(1700000000, 0)
	key := func(name, uid string, created metav1.Time) *coderv1alpha1.CoderAIGatewayKey {
		return &coderv1alpha1.CoderAIGatewayKey{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid), CreationTimestamp: created},
			Spec:       coderv1alpha1.CoderAIGatewayKeySpec{SecretName: "shared"},
		}
	}
	oldest := key("b-oldest", "uid-1", t0)
	sameTimeLaterName := key("c-same-time", "uid-2", t0)
	newer := key("a-newer", "uid-3", metav1.NewTime(t0.Add(time.Minute)))
	c := fake.NewClientBuilder().WithScheme(reservedNameScheme(t)).WithObjects(oldest, sameTimeLaterName, newer).Build()

	require.NoError(t, reservedSecretName(ctx, c, oldest, "shared"), "the oldest object keeps the name")
	err := reservedSecretName(ctx, c, sameTimeLaterName, "shared")
	requireReservedReason(t, err)
	require.ErrorContains(t, err, `CoderAIGatewayKey "b-oldest"`)
	err = reservedSecretName(ctx, c, newer, "shared")
	requireReservedReason(t, err)
	require.ErrorContains(t, err, `"b-oldest", "c-same-time"`)
	require.NoError(t, reservedSecretName(ctx, c, newer, "other"))

	require.ErrorContains(t, reservedSecretName(ctx, c, &coderv1alpha1.CoderAIGatewayKey{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}}, "shared"),
		"assertion failed: a reserved Secret name check needs")
}
