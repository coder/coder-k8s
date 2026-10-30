package operatortoken_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/operatortoken"
)

func TestRead(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, coderv1alpha1.AddToScheme(scheme))

	controlPlane := func() *coderv1alpha1.CoderControlPlane {
		cp := &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "ns", UID: "cp-uid"}}
		cp.Status.OperatorAccessReady = true
		cp.Status.OperatorTokenSecretRef = &coderv1alpha1.SecretKeySelector{Name: "coder-operator-token"}
		return cp
	}
	controllerRef := func(mutate func(*metav1.OwnerReference)) []metav1.OwnerReference {
		ref := metav1.OwnerReference{
			APIVersion: coderv1alpha1.GroupVersion.String(),
			Kind:       "CoderControlPlane",
			Name:       "coder",
			UID:        "cp-uid",
			Controller: ptr.To(true),
		}
		if mutate != nil {
			mutate(&ref)
		}
		return []metav1.OwnerReference{ref}
	}
	tokenSecret := func(owners []metav1.OwnerReference, data map[string]string) *corev1.Secret {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "coder-operator-token", Namespace: "ns", OwnerReferences: owners},
			Data:       map[string][]byte{},
		}
		for key, value := range data {
			secret.Data[key] = []byte(value)
		}
		return secret
	}
	owned := func(data map[string]string) *corev1.Secret { return tokenSecret(controllerRef(nil), data) }

	tests := []struct {
		name        string
		cp          func() *coderv1alpha1.CoderControlPlane
		secret      *corev1.Secret
		wantToken   string
		wantMessage string
	}{
		{
			name:      "owned Secret, default key",
			cp:        controlPlane,
			secret:    owned(map[string]string{"token": "secret-token"}),
			wantToken: "secret-token",
		},
		{
			name: "owned Secret, custom key",
			cp: func() *coderv1alpha1.CoderControlPlane {
				cp := controlPlane()
				cp.Status.OperatorTokenSecretRef.Key = " custom "
				return cp
			},
			secret:    owned(map[string]string{"custom": "secret-token"}),
			wantToken: "secret-token",
		},
		{
			name: "operator access not ready",
			cp: func() *coderv1alpha1.CoderControlPlane {
				cp := controlPlane()
				cp.Status.OperatorAccessReady = false
				return cp
			},
			secret:      owned(map[string]string{"token": "secret-token"}),
			wantMessage: "operator token unavailable: operator access is not ready",
		},
		{
			name: "no Secret reference",
			cp: func() *coderv1alpha1.CoderControlPlane {
				cp := controlPlane()
				cp.Status.OperatorTokenSecretRef = nil
				return cp
			},
			secret:      owned(map[string]string{"token": "secret-token"}),
			wantMessage: "operator token unavailable: status.operatorTokenSecretRef names no Secret",
		},
		{
			name: "blank Secret name",
			cp: func() *coderv1alpha1.CoderControlPlane {
				cp := controlPlane()
				cp.Status.OperatorTokenSecretRef.Name = " "
				return cp
			},
			wantMessage: "operator token unavailable: status.operatorTokenSecretRef names no Secret",
		},
		{
			name:        "Secret missing",
			cp:          controlPlane,
			wantMessage: `operator token unavailable: Secret "coder-operator-token" does not exist`,
		},
		{
			name:        "no owner",
			cp:          controlPlane,
			secret:      tokenSecret(nil, map[string]string{"token": "foreign-value"}),
			wantMessage: `operator token unavailable: the control plane is not the controller owner of Secret "coder-operator-token"`,
		},
		{
			name:        "controller with the same name and another UID",
			cp:          controlPlane,
			secret:      tokenSecret(controllerRef(func(ref *metav1.OwnerReference) { ref.UID = "other-uid" }), map[string]string{"token": "foreign-value"}),
			wantMessage: `operator token unavailable: the control plane is not the controller owner of Secret "coder-operator-token"`,
		},
		{
			name:        "owner that is not the controller",
			cp:          controlPlane,
			secret:      tokenSecret(controllerRef(func(ref *metav1.OwnerReference) { ref.Controller = nil }), map[string]string{"token": "foreign-value"}),
			wantMessage: `operator token unavailable: the control plane is not the controller owner of Secret "coder-operator-token"`,
		},
		{
			name:        "controller of another kind with the same UID",
			cp:          controlPlane,
			secret:      tokenSecret(controllerRef(func(ref *metav1.OwnerReference) { ref.Kind = "CoderProvisioner" }), map[string]string{"token": "foreign-value"}),
			wantMessage: `operator token unavailable: the control plane is not the controller owner of Secret "coder-operator-token"`,
		},
		{
			name:        "key missing",
			cp:          controlPlane,
			secret:      owned(map[string]string{"other": "secret-token"}),
			wantMessage: `operator token unavailable: Secret "coder-operator-token" has no value for key "token"`,
		},
		{
			name:        "empty value",
			cp:          controlPlane,
			secret:      owned(map[string]string{"token": ""}),
			wantMessage: `operator token unavailable: Secret "coder-operator-token" has no value for key "token"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.secret != nil {
				builder = builder.WithObjects(tt.secret)
			}
			token, err := operatortoken.Read(context.Background(), builder.Build(), tt.cp())
			if tt.wantMessage == "" {
				require.NoError(t, err)
				require.Equal(t, tt.wantToken, token)
				return
			}
			require.ErrorIs(t, err, operatortoken.ErrUnavailable)
			require.EqualError(t, err, tt.wantMessage)
			require.Empty(t, token)
		})
	}

	t.Run("read error is not ErrUnavailable", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("apiserver down")
			},
		}).Build()
		_, err := operatortoken.Read(context.Background(), reader, controlPlane())
		require.EqualError(t, err, "read operator token Secret ns/coder-operator-token: apiserver down")
		require.NotErrorIs(t, err, operatortoken.ErrUnavailable)
	})

	t.Run("assertions", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(scheme).Build()
		_, err := operatortoken.Read(context.Background(), nil, controlPlane())
		require.ErrorContains(t, err, "assertion failed:")
		_, err = operatortoken.Read(context.Background(), reader, nil)
		require.ErrorContains(t, err, "assertion failed:")
		withoutUID := controlPlane()
		withoutUID.UID = types.UID("")
		_, err = operatortoken.Read(context.Background(), reader, withoutUID)
		require.ErrorContains(t, err, "assertion failed:")
	})
}
