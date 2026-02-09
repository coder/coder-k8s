package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/app/controllerapp"
	"github.com/coder/coder-k8s/internal/controller"
)

func TestControllerSchemeRegistersCoderControlPlaneKinds(t *testing.T) {
	t.Helper()

	scheme := controllerapp.NewScheme()
	if scheme == nil {
		t.Fatal("expected non-nil scheme")
	}

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

	if controllerapp.HealthProbeBindAddress == "" || controllerapp.HealthProbeBindAddress == "0" {
		t.Fatalf("expected non-empty HealthProbeBindAddress, got %q", controllerapp.HealthProbeBindAddress)
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

func TestRunRejectsEmptyMode(t *testing.T) {
	t.Helper()

	err := run([]string{})
	if err == nil {
		t.Fatal("expected an error when --app is missing")
	}
	if !strings.Contains(err.Error(), "--app flag is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunRejectsUnknownMode(t *testing.T) {
	t.Helper()

	err := run([]string{"--app=unknown"})
	if err == nil {
		t.Fatal("expected an error when --app has unknown mode")
	}
	if !strings.Contains(err.Error(), "unsupported --app") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunDispatchesAggregatedAPIServerMode(t *testing.T) {
	t.Helper()

	previous := runAggregatedAPIServerApp
	t.Cleanup(func() {
		runAggregatedAPIServerApp = previous
	})

	expectedErr := errors.New("sentinel aggregated-apiserver error")
	called := false
	runAggregatedAPIServerApp = func(ctx context.Context) error {
		called = true
		if ctx == nil {
			t.Fatal("expected non-nil context passed to aggregated apiserver runner")
		}
		return expectedErr
	}

	err := run([]string{"--app=aggregated-apiserver"})
	if !called {
		t.Fatal("expected aggregated apiserver runner to be called")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected sentinel error %v, got %v", expectedErr, err)
	}
}
