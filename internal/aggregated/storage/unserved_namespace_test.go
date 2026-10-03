package storage

import (
	"context"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// newControlPlaneProviderForStorageTest builds the dynamic provider that --app=all uses, backed by a
// fake client that holds controlPlanes.
func newControlPlaneProviderForStorageTest(t *testing.T, controlPlanes ...coderv1alpha1.CoderControlPlane) coder.ClientProvider {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(coderv1alpha1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithLists(&coderv1alpha1.CoderControlPlaneList{Items: controlPlanes}).
		Build()

	provider, err := coder.NewControlPlaneClientProvider(reader, reader, 10*time.Second)
	if err != nil {
		t.Fatalf("new control plane client provider: %v", err)
	}
	return provider
}

func storageTestControlPlane(namespace, name string, ready bool) coderv1alpha1.CoderControlPlane {
	return coderv1alpha1.CoderControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: coderv1alpha1.CoderControlPlaneStatus{
			URL:                    "https://coder.example.com",
			OperatorAccessReady:    ready,
			OperatorTokenSecretRef: &coderv1alpha1.SecretKeySelector{Name: "operator-token", Key: "token"},
		},
	}
}

type listStorage interface {
	List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error)
}

// listStoragesForTest returns the three LIST implementations of the aggregated API, keyed by resource.
func listStoragesForTest(provider coder.ClientProvider) map[string]listStorage {
	return map[string]listStorage{
		"coderworkspaces":       NewWorkspaceStorage(provider),
		"codertemplates":        NewTemplateStorage(provider),
		"codertemplateversions": NewTemplateVersionStorage(provider),
	}
}

func assertEmptyTypedList(t *testing.T, resource string, obj runtime.Object) {
	t.Helper()

	var items any
	switch list := obj.(type) {
	case *aggregationv1alpha1.CoderWorkspaceList:
		if resource != "coderworkspaces" || list.Kind != "CoderWorkspaceList" {
			t.Fatalf("%s: unexpected list %T kind %q", resource, obj, list.Kind)
		}
		items = list.Items
	case *aggregationv1alpha1.CoderTemplateList:
		if resource != "codertemplates" || list.Kind != "CoderTemplateList" {
			t.Fatalf("%s: unexpected list %T kind %q", resource, obj, list.Kind)
		}
		items = list.Items
	case *aggregationv1alpha1.CoderTemplateVersionList:
		if resource != "codertemplateversions" || list.Kind != "CoderTemplateVersionList" {
			t.Fatalf("%s: unexpected list %T kind %q", resource, obj, list.Kind)
		}
		items = list.Items
	default:
		t.Fatalf("%s: unexpected list type %T", resource, obj)
	}
	// items must be an empty, non-nil slice so that the JSON response holds "items": [].
	value := reflect.ValueOf(items)
	if value.IsNil() || value.Len() != 0 {
		t.Fatalf("%s: expected empty non-nil items, got %#v", resource, items)
	}
}

// Issue #209: the namespace controller lists every deletable resource in a namespace before it
// removes the namespace. A namespaced LIST in a namespace that no Coder backend serves must answer
// with an empty list, or the namespace never finishes deleting.
func TestNamespacedListInUnservedNamespaceReturnsEmptyList(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()

	providers := map[string]coder.ClientProvider{
		"standalone provider pinned to another namespace": &coder.StaticClientProvider{
			Client: newTestSDKClient(t, server.URL), Namespace: "control-plane",
		},
		"no control plane": newControlPlaneProviderForStorageTest(t),
		"control plane only in another namespace": newControlPlaneProviderForStorageTest(t,
			storageTestControlPlane("control-plane", "coder", true)),
		"control plane in namespace is not ready": newControlPlaneProviderForStorageTest(t,
			storageTestControlPlane("empty-namespace", "coder", false)),
	}

	for providerName, provider := range providers {
		for resource, storage := range listStoragesForTest(provider) {
			obj, err := storage.List(namespacedContext("empty-namespace"), nil)
			if err != nil {
				t.Fatalf("%s: %s LIST: expected empty list, got error %v", providerName, resource, err)
			}
			assertEmptyTypedList(t, resource, obj)
		}
	}
	if requests := state.requests(); len(requests) != 0 {
		t.Fatalf("expected no Coder requests for unserved namespaces, got %v", requests)
	}
}

// LIST keeps every error that does not mean "this namespace is not served".
func TestListKeepsErrorsOtherThanUnservedNamespace(t *testing.T) {
	t.Parallel()

	noControlPlane := newControlPlaneProviderForStorageTest(t)
	for resource, storage := range listStoragesForTest(noControlPlane) {
		_, err := storage.List(namespacedContext(""), nil)
		if !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("%s: all-namespaces LIST without a control plane: expected ServiceUnavailable, got %v", resource, err)
		}
	}

	duplicate := newControlPlaneProviderForStorageTest(t,
		storageTestControlPlane("team-a", "coder-a", true),
		storageTestControlPlane("team-a", "coder-b", true))
	for resource, storage := range listStoragesForTest(duplicate) {
		_, err := storage.List(namespacedContext("team-a"), nil)
		if !apierrors.IsBadRequest(err) {
			t.Fatalf("%s: LIST with two eligible control planes: expected BadRequest, got %v", resource, err)
		}
	}

	// The eligible control plane has no token Secret, so resolving its client fails.
	missingSecret := newControlPlaneProviderForStorageTest(t, storageTestControlPlane("team-a", "coder", true))
	for resource, storage := range listStoragesForTest(missingSecret) {
		obj, err := storage.List(namespacedContext("team-a"), nil)
		if err == nil {
			t.Fatalf("%s: LIST with an unreadable token Secret: expected error, got %#v", resource, obj)
		}
	}

	server, _ := newMockCoderServer(t)
	defer server.Close()
	unpinned := &coder.StaticClientProvider{Client: newTestSDKClient(t, server.URL)}
	for resource, storage := range listStoragesForTest(unpinned) {
		_, err := storage.List(namespacedContext("team-a"), nil)
		if !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("%s: LIST with an unpinned standalone provider: expected ServiceUnavailable, got %v", resource, err)
		}
	}
}

// GET, CREATE and DELETE in a namespace without an eligible control plane keep their 503.
func TestOtherVerbsInNamespaceWithoutControlPlaneKeepServiceUnavailable(t *testing.T) {
	t.Parallel()

	provider := newControlPlaneProviderForStorageTest(t)
	ctx := namespacedContext("empty-namespace")
	workspaceStorage := NewWorkspaceStorage(provider)
	templateStorage := NewTemplateStorage(provider)

	assertServiceUnavailable := func(verb string, err error) {
		t.Helper()
		if !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("%s: expected ServiceUnavailable, got %v", verb, err)
		}
		assertTopLevelStatusError(t, err)
	}

	_, err := workspaceStorage.Get(ctx, "acme.alice.dev", nil)
	assertServiceUnavailable("workspace GET", err)
	_, err = workspaceStorage.Create(ctx, &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "acme.alice.dev"},
		Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Organization: "acme", TemplateName: "starter-template"},
	}, rest.ValidateAllObjectFunc, nil)
	assertServiceUnavailable("workspace CREATE", err)
	_, _, err = workspaceStorage.Delete(ctx, "acme.alice.dev", rest.ValidateAllObjectFunc, nil)
	assertServiceUnavailable("workspace DELETE", err)

	_, err = templateStorage.Get(ctx, "acme.starter-template", nil)
	assertServiceUnavailable("template GET", err)
	_, err = templateStorage.Create(ctx, &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "acme.starter-template"},
		Spec:       aggregationv1alpha1.CoderTemplateSpec{Organization: "acme"},
	}, rest.ValidateAllObjectFunc, nil)
	assertServiceUnavailable("template CREATE", err)
	_, _, err = templateStorage.Delete(ctx, "acme.starter-template", rest.ValidateAllObjectFunc, nil)
	assertServiceUnavailable("template DELETE", err)
}

// WATCH never asks the provider: it serves local events, so it already works in a namespace
// without a control plane and across all namespaces.
func TestWatchInNamespaceWithoutControlPlaneSucceeds(t *testing.T) {
	t.Parallel()

	provider := newControlPlaneProviderForStorageTest(t)
	for _, namespace := range []string{"empty-namespace", ""} {
		workspaceWatch, err := NewWorkspaceStorage(provider).Watch(namespacedContext(namespace), nil)
		if err != nil {
			t.Fatalf("workspace WATCH in namespace %q: %v", namespace, err)
		}
		workspaceWatch.Stop()

		templateWatch, err := NewTemplateStorage(provider).Watch(namespacedContext(namespace), nil)
		if err != nil {
			t.Fatalf("template WATCH in namespace %q: %v", namespace, err)
		}
		templateWatch.Stop()
	}
}
