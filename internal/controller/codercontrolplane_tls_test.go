package controller_test

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func TestCoderControlPlaneOperatorTLSAdmission(t *testing.T) {
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "operator-tls-admission")
	caBundleRef := &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca", Key: "ca.crt"}

	tests := []struct {
		name        string
		tls         coderv1alpha1.TLSSpec
		wantMessage string
	}{
		{name: "ca-bundle", tls: coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: caBundleRef}}},
		{name: "server-name", tls: coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com"}}},
		{
			name:        "no-tls-secrets",
			tls:         coderv1alpha1.TLSSpec{OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: caBundleRef}},
			wantMessage: "tls.operatorAccess needs tls.secretNames",
		},
		{
			name:        "no-trust",
			tls:         coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{}},
			wantMessage: "tls.operatorAccess needs caBundleRef or serverName",
		},
		{
			name:        "empty-key",
			tls:         coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{CABundleRef: &coderv1alpha1.ConfigMapKeySelector{Name: "coder-ca"}}},
			wantMessage: "spec.tls.operatorAccess.caBundleRef.key",
		},
		{
			name:        "server-name-with-port",
			tls:         coderv1alpha1.TLSSpec{SecretNames: []string{"coder-tls"}, OperatorAccess: &coderv1alpha1.OperatorTLSSpec{ServerName: "coder.example.com:443"}},
			wantMessage: "spec.tls.operatorAccess.serverName",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &coderv1alpha1.CoderControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: namespace},
				Spec:       coderv1alpha1.CoderControlPlaneSpec{TLS: tt.tls},
			}
			err := k8sClient.Create(ctx, cp)
			if tt.wantMessage == "" {
				if err != nil {
					t.Fatalf("expected admission to accept %s, got %v", tt.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected admission to reject %s", tt.name)
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("expected Invalid error containing %q, got %v", tt.wantMessage, err)
			}
		})
	}
}
