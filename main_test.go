package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/app/apiserverapp"
	"github.com/coder/coder-k8s/internal/app/controllerapp"
	"github.com/coder/coder-k8s/internal/controller"
)

func installMockSignalHandler(t *testing.T) {
	t.Helper()

	previous := setupSignalHandler
	t.Cleanup(func() {
		setupSignalHandler = previous
	})

	setupSignalHandler = func() context.Context {
		return context.Background()
	}
}

func TestControllerSchemeRegistersCoderControlPlaneKinds(t *testing.T) {
	t.Helper()

	scheme := controllerapp.NewScheme()
	if scheme == nil {
		t.Fatal("expected non-nil scheme")
	}

	for _, gvk := range []schema.GroupVersionKind{
		coderv1alpha1.GroupVersion.WithKind("CoderControlPlane"),
		coderv1alpha1.GroupVersion.WithKind("CoderControlPlaneList"),
		coderv1alpha1.GroupVersion.WithKind("WorkspaceProxy"),
		coderv1alpha1.GroupVersion.WithKind("WorkspaceProxyList"),
		aggregationv1alpha1.SchemeGroupVersion.WithKind("CoderWorkspace"),
		aggregationv1alpha1.SchemeGroupVersion.WithKind("CoderWorkspaceList"),
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

func TestRunDefaultsToAllMode(t *testing.T) {
	t.Helper()
	installMockSignalHandler(t)

	previous := runAllApp
	t.Cleanup(func() {
		runAllApp = previous
	})

	expectedErr := errors.New("sentinel all error")
	called := false
	runAllApp = func(ctx context.Context, timeout time.Duration) error {
		called = true
		if ctx == nil {
			t.Fatal("expected non-nil context")
		}
		if got, want := timeout, 30*time.Second; got != want {
			t.Fatalf("expected coder request timeout %v, got %v", want, got)
		}
		return expectedErr
	}

	err := run([]string{})
	if !called {
		t.Fatal("expected all runner to be called")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected sentinel, got %v", err)
	}
}

func TestRunDispatchesAllMode(t *testing.T) {
	t.Helper()
	installMockSignalHandler(t)

	previous := runAllApp
	t.Cleanup(func() {
		runAllApp = previous
	})

	expectedErr := errors.New("sentinel all error")
	called := false
	runAllApp = func(ctx context.Context, timeout time.Duration) error {
		called = true
		if ctx == nil {
			t.Fatal("expected non-nil context")
		}
		if got, want := timeout, 45*time.Second; got != want {
			t.Fatalf("expected coder request timeout %v, got %v", want, got)
		}
		return expectedErr
	}

	err := run([]string{"--app=all", "--coder-request-timeout=45s"})
	if !called {
		t.Fatal("expected all runner to be called")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected sentinel, got %v", err)
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
	installMockSignalHandler(t)

	previous := runAggregatedAPIServerApp
	t.Cleanup(func() {
		runAggregatedAPIServerApp = previous
	})

	expectedErr := errors.New("sentinel aggregated-apiserver error")
	called := false
	runAggregatedAPIServerApp = func(ctx context.Context, opts apiserverapp.Options) error {
		called = true
		if ctx == nil {
			t.Fatal("expected non-nil context passed to aggregated apiserver runner")
		}
		if got, want := opts.CoderURL, "https://coder.example.com"; got != want {
			t.Fatalf("expected coder URL %q, got %q", want, got)
		}
		if got, want := opts.CoderSessionToken, "test-token"; got != want {
			t.Fatalf("expected coder session token %q, got %q", want, got)
		}
		if got, want := opts.CoderNamespace, "control-plane"; got != want {
			t.Fatalf("expected coder namespace %q, got %q", want, got)
		}
		if got, want := opts.CoderRequestTimeout, 45*time.Second; got != want {
			t.Fatalf("expected coder request timeout %v, got %v", want, got)
		}
		return expectedErr
	}

	err := run([]string{
		"--app=aggregated-apiserver",
		"--coder-url=https://coder.example.com",
		"--coder-session-token=test-token",
		"--coder-namespace=control-plane",
		"--coder-request-timeout=45s",
	})
	if !called {
		t.Fatal("expected aggregated apiserver runner to be called")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected sentinel error %v, got %v", expectedErr, err)
	}
}

func TestRunRejectsAggregatedAPIServerModeWithCoderURLMissingScheme(t *testing.T) {
	t.Helper()
	installMockSignalHandler(t)

	previous := runAggregatedAPIServerApp
	t.Cleanup(func() {
		runAggregatedAPIServerApp = previous
	})

	called := false
	runAggregatedAPIServerApp = func(context.Context, apiserverapp.Options) error {
		called = true
		return nil
	}

	err := run([]string{
		"--app=aggregated-apiserver",
		"--coder-url=coder.example.com",
	})
	if err == nil {
		t.Fatal("expected an error when --coder-url omits scheme")
	}
	if !strings.Contains(err.Error(), "must include scheme and host") {
		t.Fatalf("expected missing scheme/host validation error, got %v", err)
	}
	if called {
		t.Fatal("expected aggregated apiserver runner not to be called on invalid --coder-url")
	}
}

func TestRunRejectsAggregatedAPIServerModeWithUnsupportedCoderURLScheme(t *testing.T) {
	t.Helper()
	installMockSignalHandler(t)

	previous := runAggregatedAPIServerApp
	t.Cleanup(func() {
		runAggregatedAPIServerApp = previous
	})

	called := false
	runAggregatedAPIServerApp = func(context.Context, apiserverapp.Options) error {
		called = true
		return nil
	}

	err := run([]string{
		"--app=aggregated-apiserver",
		"--coder-url=ftp://coder.example.com",
	})
	if err == nil {
		t.Fatal("expected an error when --coder-url has unsupported scheme")
	}
	if !strings.Contains(err.Error(), "scheme must be http or https") {
		t.Fatalf("expected scheme validation error, got %v", err)
	}
	if called {
		t.Fatal("expected aggregated apiserver runner not to be called on invalid --coder-url")
	}
}

func TestRunDispatchesMCPHTTPMode(t *testing.T) {
	t.Helper()
	installMockSignalHandler(t)

	previous := runMCPHTTPApp
	t.Cleanup(func() {
		runMCPHTTPApp = previous
	})

	expectedErr := errors.New("sentinel mcp-http error")
	called := false
	runMCPHTTPApp = func(ctx context.Context) error {
		called = true
		if ctx == nil {
			t.Fatal("expected non-nil context passed to MCP HTTP runner")
		}
		return expectedErr
	}

	err := run([]string{"--app=mcp-http"})
	if !called {
		t.Fatal("expected MCP HTTP runner to be called")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected sentinel error %v, got %v", expectedErr, err)
	}
}
