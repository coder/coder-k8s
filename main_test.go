package main

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

func TestSchemeRegistersCoderControlPlaneKinds(t *testing.T) {
	t.Helper()

	for _, gvk := range []schema.GroupVersionKind{
		coderv1alpha1.GroupVersion.WithKind("CoderControlPlane"),
		coderv1alpha1.GroupVersion.WithKind("CoderControlPlaneList"),
	} {
		if !scheme.Recognizes(gvk) {
			t.Fatalf("expected scheme to recognize %s", gvk.String())
		}
	}
}

func TestHealthProbeBindAddressIsEnabled(t *testing.T) {
	t.Helper()

	if healthProbeBindAddress == "" || healthProbeBindAddress == "0" {
		t.Fatalf("expected non-empty healthProbeBindAddress, got %q", healthProbeBindAddress)
	}
}

func TestReconcilerSetupWithManagerRequiresManager(t *testing.T) {
	t.Helper()

	r := &controller.CoderControlPlaneReconciler{}
	err := r.SetupWithManager(nil)
	if err == nil {
		t.Fatal("expected an error when manager is nil")
	}
	if !strings.Contains(err.Error(), "manager must not be nil") {
		t.Fatalf("unexpected error: %v", err)
	}
}
