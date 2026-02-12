package apiserverapp

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericoptions "k8s.io/apiserver/pkg/server/options"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	coderhelper "github.com/coder/coder-k8s/internal/aggregated/coder"
)

func TestNewSchemeRegistersAggregationKinds(t *testing.T) {
	t.Helper()

	scheme := NewScheme()
	if scheme == nil {
		t.Fatal("expected non-nil scheme")
	}

	for _, gvk := range []schema.GroupVersionKind{
		aggregationv1alpha1.SchemeGroupVersion.WithKind("CoderWorkspace"),
		aggregationv1alpha1.SchemeGroupVersion.WithKind("CoderWorkspaceList"),
		aggregationv1alpha1.SchemeGroupVersion.WithKind("CoderTemplate"),
		aggregationv1alpha1.SchemeGroupVersion.WithKind("CoderTemplateList"),
	} {
		if !scheme.Recognizes(gvk) {
			t.Fatalf("expected scheme to recognize %s", gvk.String())
		}
	}
}

func TestInstallAPIGroupRegistersDiscovery(t *testing.T) {
	t.Helper()

	scheme := NewScheme()
	if scheme == nil {
		t.Fatal("expected non-nil scheme")
	}
	codecs := serializer.NewCodecFactory(scheme)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("create test listener: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	secureServingOptions := genericoptions.NewSecureServingOptions()
	secureServingOptions.Listener = listener
	secureServingOptions.BindPort = 0
	secureServingOptions.ServerCert.CertDirectory = ""
	secureServingOptions.ServerCert.PairName = ""

	recommendedConfig, err := NewRecommendedConfig(scheme, codecs, secureServingOptions)
	if err != nil {
		t.Fatalf("build recommended config: %v", err)
	}

	server, err := NewGenericAPIServer(recommendedConfig)
	if err != nil {
		t.Fatalf("build generic API server: %v", err)
	}
	defer server.Destroy()

	coderURL, err := url.Parse("http://localhost:8080")
	if err != nil {
		t.Fatalf("parse test coder URL: %v", err)
	}
	provider, err := coderhelper.NewStaticClientProvider(
		coderhelper.Config{
			CoderURL:     coderURL,
			SessionToken: "test-session-token",
		},
		"",
	)
	if err != nil {
		t.Fatalf("build static client provider: %v", err)
	}

	apiGroupInfo, err := NewAPIGroupInfo(scheme, codecs, provider)
	if err != nil {
		t.Fatalf("build API group info: %v", err)
	}

	storageByVersion, ok := apiGroupInfo.VersionedResourcesStorageMap[aggregationv1alpha1.SchemeGroupVersion.Version]
	if !ok {
		t.Fatalf("expected storage map for version %s", aggregationv1alpha1.SchemeGroupVersion.Version)
	}
	if _, ok := storageByVersion["coderworkspaces"]; !ok {
		t.Fatal("expected coderworkspaces storage registration")
	}
	if _, ok := storageByVersion["codertemplates"]; !ok {
		t.Fatal("expected codertemplates storage registration")
	}

	if err := InstallAPIGroup(server, apiGroupInfo); err != nil {
		t.Fatalf("install API group: %v", err)
	}

	groups, err := server.DiscoveryGroupManager.Groups(context.Background(), httptest.NewRequest("GET", "https://example.com/apis", nil))
	if err != nil {
		t.Fatalf("list discovery groups: %v", err)
	}

	found := false
	for _, group := range groups {
		if group.Name != aggregationv1alpha1.SchemeGroupVersion.Group {
			continue
		}
		found = true
		if group.PreferredVersion.Version != aggregationv1alpha1.SchemeGroupVersion.Version {
			t.Fatalf("expected preferred version %s, got %s", aggregationv1alpha1.SchemeGroupVersion.Version, group.PreferredVersion.Version)
		}
	}
	if !found {
		t.Fatalf("expected discovery registration for group %s", aggregationv1alpha1.SchemeGroupVersion.Group)
	}
}

func TestBuildClientProviderDefersMissingCoderConfigAsServiceUnavailable(t *testing.T) {
	t.Parallel()

	provider, err := buildClientProvider(Options{}, 30*time.Second)
	if err != nil {
		t.Fatalf("expected missing coder config to return a deferred-error provider, got %v", err)
	}
	if provider == nil {
		t.Fatal("expected non-nil provider when coder config is missing")
	}

	sdkClient, err := provider.ClientForNamespace(context.Background(), "control-plane")
	if sdkClient != nil {
		t.Fatalf("expected nil sdk client when coder config is missing, got %T", sdkClient)
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable when provider is not configured, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "configure --coder-url and --coder-session-token") {
		t.Fatalf("expected missing-config error message, got %v", err)
	}
}

func TestBuildClientProviderRejectsPartialCoderConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
	}{
		{
			name: "missing coder URL",
			opts: Options{CoderSessionToken: "test-session-token"},
		},
		{
			name: "missing coder session token",
			opts: Options{CoderURL: "https://coder.example.com"},
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := buildClientProvider(testCase.opts, 30*time.Second)
			if err == nil {
				t.Fatal("expected partial coder config to fail")
			}
			if !strings.Contains(err.Error(), "partially configured") {
				t.Fatalf("expected partial-config error, got %v", err)
			}
		})
	}
}

func TestRunWithOptionsRejectsPartialCoderConfig(t *testing.T) {
	t.Parallel()

	err := RunWithOptions(context.Background(), Options{CoderURL: "https://coder.example.com"})
	if err == nil {
		t.Fatal("expected partial coder config to fail startup")
	}
	if !strings.Contains(err.Error(), "partially configured") {
		t.Fatalf("expected partial-config startup error, got %v", err)
	}
}

func TestRunWithOptionsUsesClientProviderOverride(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("create test listener: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	coderURL, err := url.Parse("https://coder.example.com")
	if err != nil {
		t.Fatalf("parse test coder URL: %v", err)
	}
	provider, err := coderhelper.NewStaticClientProvider(
		coderhelper.Config{
			CoderURL:     coderURL,
			SessionToken: "test-session-token",
		},
		"control-plane",
	)
	if err != nil {
		t.Fatalf("build static client provider: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(ctx, Options{
			Listener:       listener,
			CoderURL:       "https://coder.example.com",
			ClientProvider: provider,
		})
	}()

	select {
	case runErr := <-errCh:
		t.Fatalf("expected startup to continue with provider override, got %v", runErr)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()

	select {
	case runErr := <-errCh:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("expected graceful shutdown after cancellation, got %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for aggregated apiserver shutdown")
	}
}

func TestBuildClientProviderRejectsMissingCoderNamespaceWhenBackendConfigured(t *testing.T) {
	t.Parallel()

	_, err := buildClientProvider(Options{
		CoderURL:          "https://coder.example.com",
		CoderSessionToken: "test-session-token",
	}, 30*time.Second)
	if err == nil {
		t.Fatal("expected missing coder namespace to fail when backend is otherwise configured")
	}
	if !strings.Contains(err.Error(), "configure --coder-namespace") {
		t.Fatalf("expected missing namespace error to mention --coder-namespace, got %v", err)
	}
}

func TestRunWithOptionsRejectsMissingCoderNamespaceWhenBackendConfigured(t *testing.T) {
	t.Parallel()

	err := RunWithOptions(context.Background(), Options{
		CoderURL:          "https://coder.example.com",
		CoderSessionToken: "test-session-token",
	})
	if err == nil {
		t.Fatal("expected missing coder namespace to fail startup when backend is otherwise configured")
	}
	if !strings.Contains(err.Error(), "configure --coder-namespace") {
		t.Fatalf("expected missing namespace startup error to mention --coder-namespace, got %v", err)
	}
}

func TestRunWithOptionsStartsWithMissingCoderConfig(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("create test listener: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(ctx, Options{Listener: listener})
	}()

	select {
	case runErr := <-errCh:
		t.Fatalf("expected startup to continue with deferred coder config, got %v", runErr)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()

	select {
	case runErr := <-errCh:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("expected graceful shutdown after cancellation, got %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for aggregated apiserver shutdown")
	}
}

func TestBuildClientProviderReturnsStaticProviderWithCoderConfig(t *testing.T) {
	t.Parallel()

	provider, err := buildClientProvider(Options{
		CoderURL:          "https://coder.example.com",
		CoderSessionToken: "test-session-token",
		CoderNamespace:    "control-plane",
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("build client provider: %v", err)
	}

	staticProvider, ok := provider.(*coderhelper.StaticClientProvider)
	if !ok {
		t.Fatalf("expected *coder.StaticClientProvider, got %T", provider)
	}
	if got, want := staticProvider.Namespace, "control-plane"; got != want {
		t.Fatalf("expected provider namespace %q, got %q", want, got)
	}

	sdkClient, err := staticProvider.ClientForNamespace(context.Background(), "control-plane")
	if err != nil {
		t.Fatalf("resolve static client for namespace: %v", err)
	}
	if sdkClient == nil {
		t.Fatal("expected non-nil sdk client")
	}
	if got, want := sdkClient.URL.String(), "https://coder.example.com"; got != want {
		t.Fatalf("expected client URL %q, got %q", want, got)
	}

	_, err = staticProvider.ClientForNamespace(context.Background(), "default")
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for namespace outside provider scope, got %v", err)
	}
}
