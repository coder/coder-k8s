package coder

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// Issue #209: a namespaced LIST in a namespace that no Coder backend serves answers with an empty
// list. The provider marks exactly those errors, and the marked errors keep their status for all
// other verbs.
func TestControlPlaneClientProviderMarksNamespaceNotServed(t *testing.T) {
	t.Parallel()

	notReady := eligibleControlPlane("team-a", "coder")
	notReady.Status.OperatorAccessReady = false

	tests := []struct {
		name          string
		controlPlanes []coderv1alpha1.CoderControlPlane
		secrets       []corev1.Secret
		namespace     string
		wantNotServed bool
		wantStatus    func(error) bool
	}{
		{
			name:          "no control plane anywhere",
			namespace:     "team-a",
			wantNotServed: true,
			wantStatus:    apierrors.IsServiceUnavailable,
		},
		{
			name:          "eligible control plane only in another namespace",
			controlPlanes: []coderv1alpha1.CoderControlPlane{eligibleControlPlane("team-b", "coder")},
			namespace:     "team-a",
			wantNotServed: true,
			wantStatus:    apierrors.IsServiceUnavailable,
		},
		{
			name:          "control plane in namespace is not eligible",
			controlPlanes: []coderv1alpha1.CoderControlPlane{notReady},
			namespace:     "team-a",
			wantNotServed: true,
			wantStatus:    apierrors.IsServiceUnavailable,
		},
		{
			name:       "all namespaces without eligible control plane",
			namespace:  "",
			wantStatus: apierrors.IsServiceUnavailable,
		},
		{
			name: "multiple eligible control planes in namespace",
			controlPlanes: []coderv1alpha1.CoderControlPlane{
				eligibleControlPlane("team-a", "coder-a"),
				eligibleControlPlane("team-a", "coder-b"),
			},
			namespace:  "team-a",
			wantStatus: apierrors.IsBadRequest,
		},
		{
			name:          "eligible control plane with unusable token secret",
			controlPlanes: []coderv1alpha1.CoderControlPlane{eligibleControlPlane("team-a", "coder")},
			secrets: []corev1.Secret{
				secretWithStringData("team-a", "operator-token", map[string]string{"other": "value"}),
			},
			namespace:  "team-a",
			wantStatus: apierrors.IsServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider, _ := newControlPlaneProviderForTest(t, tt.controlPlanes, tt.secrets)

			_, err := provider.ClientForNamespace(context.Background(), tt.namespace)
			if err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantStatus(err) {
				t.Fatalf("unexpected status for error %v", err)
			}
			if got := IsNamespaceNotServed(err); got != tt.wantNotServed {
				t.Fatalf("IsNamespaceNotServed() = %t, want %t (error %v)", got, tt.wantNotServed, err)
			}
			// Callers wrap provider errors; the mark and the status must survive wrapping.
			wrapped := fmt.Errorf("resolve client: %w", err)
			if got := IsNamespaceNotServed(wrapped); got != tt.wantNotServed {
				t.Fatalf("IsNamespaceNotServed(wrapped) = %t, want %t", got, tt.wantNotServed)
			}
			var statusErr *apierrors.StatusError
			if !errors.As(wrapped, &statusErr) || !tt.wantStatus(statusErr) {
				t.Fatalf("expected wrapped error to unwrap to its status error, got %v", wrapped)
			}
		})
	}
}

func TestStaticClientProviderMarksNamespaceNotServed(t *testing.T) {
	t.Parallel()

	client, err := NewSDKClient(Config{
		CoderURL:     mustParseURL(t, "https://coder.example.com"),
		SessionToken: "session-token",
	})
	if err != nil {
		t.Fatalf("create SDK client: %v", err)
	}

	pinned := &StaticClientProvider{Client: client, Namespace: "control-plane"}
	_, err = pinned.ClientForNamespace(context.Background(), "other-namespace")
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for a namespace mismatch, got %v", err)
	}
	if !IsNamespaceNotServed(err) {
		t.Fatalf("expected a namespace mismatch to be marked as not served, got %v", err)
	}

	unpinned := &StaticClientProvider{Client: client}
	_, err = unpinned.ClientForNamespace(context.Background(), "other-namespace")
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable for an unpinned provider, got %v", err)
	}
	if IsNamespaceNotServed(err) {
		t.Fatalf("expected missing configuration not to be marked as not served, got %v", err)
	}

	if IsNamespaceNotServed(nil) {
		t.Fatal("expected nil error not to be marked as not served")
	}
	if IsNamespaceNotServed(apierrors.NewServiceUnavailable("unrelated")) {
		t.Fatal("expected an unmarked status error not to be marked as not served")
	}
}
