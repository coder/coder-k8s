package apiserverapp

import (
	"context"
	"net"
	"net/http/httptest"
	"net/url"
	"testing"

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
	provider, err := coderhelper.NewStaticClientProvider(coderhelper.Config{
		CoderURL:     coderURL,
		SessionToken: "test-session-token",
	})
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
