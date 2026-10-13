package storage

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
)

func listTemplateNames(t *testing.T, storage *TemplateStorage, namespace string, opts *metainternalversion.ListOptions) (string, *aggregationv1alpha1.CoderTemplateList, error) {
	t.Helper()

	obj, err := storage.List(namespacedContext(namespace), opts)
	if err != nil {
		return "", nil, err
	}
	list, ok := obj.(*aggregationv1alpha1.CoderTemplateList)
	if !ok {
		t.Fatalf("expected *CoderTemplateList, got %T", obj)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Namespace+"/"+item.Name)
	}
	return strings.Join(names, " "), list, nil
}

func mustParseLabelSelector(t *testing.T, selector string) labels.Selector {
	t.Helper()

	parsed, err := labels.Parse(selector)
	if err != nil {
		t.Fatalf("parse label selector %q: %v", selector, err)
	}
	return parsed
}

func TestTemplateStorageListAppliesSelectors(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	storage := NewTemplateStorage(newTestClientProvider(t, server.URL))
	seedTemplateWithVersions(t, state, "docker", "v1")

	both := "control-plane/acme.docker control-plane/acme.starter-template"
	for _, tc := range []struct {
		name string
		opts *metainternalversion.ListOptions
		want string
	}{
		{"no options", nil, both},
		{"name", &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", "acme.docker")}, "control-plane/acme.docker"},
		{"not name", &metainternalversion.ListOptions{FieldSelector: fields.OneTermNotEqualSelector("metadata.name", "acme.docker")}, "control-plane/acme.starter-template"},
		{"own namespace", &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.namespace", "control-plane")}, both},
		{"other namespace", &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.namespace", "other")}, ""},
		// Templates carry no labels, so a selector that requires one matches nothing.
		{"label required", &metainternalversion.ListOptions{LabelSelector: mustParseLabelSelector(t, "team=platform")}, ""},
		{"label absent", &metainternalversion.ListOptions{LabelSelector: mustParseLabelSelector(t, "!team")}, both},
		// limit is ignored, and any resourceVersion is accepted: the list is always complete and current.
		{"limit", &metainternalversion.ListOptions{Limit: 1}, both},
		{"resourceVersion", &metainternalversion.ListOptions{ResourceVersion: "1781234567890123456"}, both},
	} {
		got, list, err := listTemplateNames(t, storage, "control-plane", tc.opts)
		if err != nil {
			t.Fatalf("%s: list: %v", tc.name, err)
		}
		if got != tc.want || list.Continue != "" {
			t.Fatalf("%s: expected items %q and no continue token, got %q continue=%q", tc.name, tc.want, got, list.Continue)
		}
	}

	state.resetRequests()
	for _, opts := range []*metainternalversion.ListOptions{
		{FieldSelector: fields.OneTermEqualSelector("spec.organization", "acme")},
		{Continue: "token"},
		{ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan, ResourceVersion: "0"},
		{ResourceVersionMatch: metav1.ResourceVersionMatchExact, ResourceVersion: "12345"},
	} {
		if _, _, err := listTemplateNames(t, storage, "control-plane", opts); !apierrors.IsBadRequest(err) {
			t.Fatalf("expected BadRequest for %+v, got %v", opts, err)
		}
	}
	if got := state.requests(); len(got) != 0 {
		t.Fatalf("rejected lists must not reach Coder, got %v", got)
	}
}

func TestTemplateStorageListAppliesSelectorsAcrossNamespaces(t *testing.T) {
	t.Parallel()

	serverA, _ := newMockCoderServer(t)
	defer serverA.Close()
	serverB, _ := newMockCoderServer(t)
	defer serverB.Close()
	storage := NewTemplateStorage(&multiNamespaceTestProvider{
		clients:    map[string]*codersdk.Client{"ns-a": newTestSDKClient(t, serverA.URL), "ns-b": newTestSDKClient(t, serverB.URL)},
		namespaces: []string{"ns-a", "ns-b"},
	})

	got, _, err := listTemplateNames(t, storage, "", &metainternalversion.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.namespace", "ns-b"),
	})
	if err != nil || got != "ns-b/acme.starter-template" {
		t.Fatalf("expected only ns-b/acme.starter-template, got %q err=%v", got, err)
	}
}
