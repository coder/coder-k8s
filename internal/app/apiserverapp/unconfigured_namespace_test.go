package apiserverapp

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// TestUnconfiguredServerListsNamespacesAsEmpty checks that a standalone server without a Coder URL
// and token returns empty namespaced lists, so the namespace controller can delete a namespace
// (#215). Other requests keep the 503 that names the missing configuration.
func TestUnconfiguredServerListsNamespacesAsEmpty(t *testing.T) {
	t.Parallel()

	provider, err := buildClientProvider(Options{}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scheme := NewScheme()
	apiGroupInfo, err := NewAPIGroupInfo(scheme, serializer.NewCodecFactory(scheme), provider)
	if err != nil {
		t.Fatal(err)
	}
	storages := apiGroupInfo.VersionedResourcesStorageMap[aggregationv1alpha1.SchemeGroupVersion.Version]
	ctx := genericapirequest.WithNamespace(context.Background(), "team-a")

	for resource, name := range map[string]string{
		"coderworkspaces":       "default.alice.dev",
		"codertemplates":        "default.starter",
		"codertemplateversions": "default.starter.v1",
	} {
		list, err := storages[resource].(rest.Lister).List(ctx, nil)
		if err != nil {
			t.Fatalf("%s: expected an empty list, got %v", resource, err)
		}
		if items, err := meta.ExtractList(list); err != nil || len(items) != 0 {
			t.Fatalf("%s: expected an empty list, got %d items (%v)", resource, len(items), err)
		}

		if _, err := storages[resource].(rest.Getter).Get(ctx, name, nil); !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("%s: expected get to return 503, got %v", resource, err)
		}
	}

	allNamespaces := genericapirequest.WithNamespace(context.Background(), "")
	if _, err := storages["coderworkspaces"].(rest.Lister).List(allNamespaces, nil); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected an all-namespaces list to return 503, got %v", err)
	}
}
