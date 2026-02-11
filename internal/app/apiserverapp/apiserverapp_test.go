package apiserverapp

import (
	"context"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

func TestBuildClientProviderReturnsDeferredErrorWithoutCoderConfig(t *testing.T) {
	t.Parallel()

	provider, err := buildClientProvider(Options{}, 30*time.Second)
	if err != nil {
		t.Fatalf("build client provider: %v", err)
	}
	if provider == nil {
		t.Fatal("expected non-nil provider")
	}

	_, err = provider.ClientForNamespace(context.Background(), "control-plane")
	if err == nil {
		t.Fatal("expected deferred client error when coder config is missing")
	}
	if !strings.Contains(err.Error(), "missing coder URL and coder session token") {
		t.Fatalf("expected missing-config error, got %q", err)
	}
}

func TestBuildClientProviderReturnsStaticProviderWithCoderConfig(t *testing.T) {
	t.Parallel()

	provider, err := buildClientProvider(Options{
		CoderURL:          "https://coder.example.com",
		CoderSessionToken: "test-session-token",
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("build client provider: %v", err)
	}

	staticProvider, ok := provider.(*coderhelper.StaticClientProvider)
	if !ok {
		t.Fatalf("expected *coder.StaticClientProvider, got %T", provider)
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
}
