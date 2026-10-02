package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/aggregated/convert"
	"github.com/coder/coder/v2/codersdk"
)

// seedTemplateWithVersions adds a template to the mock's organization with one version per name,
// created one hour apart in the given order. The first version is the active one.
func seedTemplateWithVersions(t *testing.T, state *mockCoderServerState, name string, versionNames ...string) codersdk.Template {
	t.Helper()

	state.mu.Lock()
	defer state.mu.Unlock()
	template := codersdk.Template{
		ID: uuid.New(), OrganizationID: state.organization.ID, OrganizationName: state.organization.Name, Name: name,
	}
	for i, versionName := range versionNames {
		createdAt := time.Date(2026, time.February, 1, i, 0, 0, 0, time.UTC)
		version := codersdk.TemplateVersion{
			ID: uuid.New(), TemplateID: &template.ID, Name: versionName, CreatedAt: createdAt, UpdatedAt: createdAt,
			Job: codersdk.ProvisionerJob{Status: codersdk.ProvisionerJobSucceeded},
		}
		if i == 0 {
			template.ActiveVersionID = version.ID
		}
		state.templateVersionsByID[version.ID] = version
	}
	state.templatesByID[template.ID] = template
	state.templateIDsByOrg[state.organization.Name][name] = template.ID
	return template
}

func listTemplateVersions(t *testing.T, storage *TemplateVersionStorage, opts *metainternalversion.ListOptions) (*aggregationv1alpha1.CoderTemplateVersionList, error) {
	t.Helper()

	obj, err := storage.List(namespacedContext("control-plane"), opts)
	if err != nil {
		return nil, err
	}
	list, ok := obj.(*aggregationv1alpha1.CoderTemplateVersionList)
	if !ok {
		t.Fatalf("expected *CoderTemplateVersionList, got %T", obj)
	}
	return list, nil
}

func TestTemplateVersionStorageList(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))
	archived := seedTemplateVersion(t, state, "old")
	archived.Archived = true
	state.mu.Lock()
	state.templateVersionsByID[archived.ID] = archived
	state.mu.Unlock()
	// Older Coder releases allowed names that the current rules reject; LIST and GET must agree on them.
	seedTemplateWithVersions(t, state, "docker", "v1", "v1.2.3", "-legacy--v1_")

	state.resetRequests()
	list, err := listTemplateVersions(t, storage, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	want := []string{
		"acme.docker.v1", "acme.docker.v1.2.3", "acme.docker.-legacy--v1_",
		"acme.starter-template.starter-template-v1", "acme.starter-template.old",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("expected sorted items %v, got %v", want, names)
	}
	if !list.Items[0].Status.Active || list.Items[1].Status.Active || !list.Items[4].Status.Archived {
		t.Fatalf("unexpected active/archived flags: %+v", list.Items)
	}
	if list.Continue != "" || list.ResourceVersion != "" {
		t.Fatalf("expected no continue token or list resourceVersion, got %q %q", list.Continue, list.ResourceVersion)
	}
	// One template list plus one version list per template; no template source download.
	if got := state.requests(); len(got) != 3 || strings.Contains(strings.Join(got, " "), "/files") {
		t.Fatalf("expected exactly 3 Coder requests and no file download, got %v", got)
	}

	// Every listed item round-trips through GET under the same name, UID and resourceVersion.
	for _, item := range list.Items {
		got, err := getTemplateVersion(t, storage, item.Name)
		if err != nil {
			t.Fatalf("get listed item %q: %v", item.Name, err)
		}
		if got.Name != item.Name || got.UID != item.UID || got.ResourceVersion != item.ResourceVersion {
			t.Fatalf("GET %q disagrees with LIST: got name=%q uid=%q rv=%q", item.Name, got.Name, got.UID, got.ResourceVersion)
		}
	}

	// limit is ignored: the list is always complete and never has a continue token.
	for _, limit := range []int64{1, 500} {
		limited, err := listTemplateVersions(t, storage, &metainternalversion.ListOptions{Limit: limit, ResourceVersion: "0"})
		if err != nil || len(limited.Items) != len(want) || limited.Continue != "" {
			t.Fatalf("limit=%d: expected all %d items and no continue, got err=%v list=%+v", limit, len(want), err, limited)
		}
	}
	if len(state.mutations()) != 0 {
		t.Fatalf("reads must not mutate Coder: %v", state.mutations())
	}
}

func TestTemplateVersionStorageListFiltersAndSkips(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))
	docker := seedTemplateWithVersions(t, state, "docker", "v1", "v2")

	byLabel, err := listTemplateVersions(t, storage, &metainternalversion.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{convert.TemplateVersionTemplateLabel: "docker"}),
	})
	if err != nil || len(byLabel.Items) != 2 {
		t.Fatalf("label selector: expected 2 docker versions, got err=%v list=%+v", err, byLabel)
	}
	byName, err := listTemplateVersions(t, storage, &metainternalversion.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", "acme.docker.v2"),
	})
	if err != nil || len(byName.Items) != 1 || byName.Items[0].Name != "acme.docker.v2" {
		t.Fatalf("field selector: expected acme.docker.v2, got err=%v list=%+v", err, byName)
	}

	// A template deleted between the template list and its version list is skipped.
	state.mu.Lock()
	state.versionListNotFound = docker.ID
	state.mu.Unlock()
	skipped, err := listTemplateVersions(t, storage, nil)
	if err != nil || len(skipped.Items) != 1 || skipped.Items[0].Spec.TemplateName != "starter-template" {
		t.Fatalf("expected only the starter-template version, got err=%v list=%+v", err, skipped)
	}

	for _, opts := range []*metainternalversion.ListOptions{
		{Continue: "token"},
		{ResourceVersionMatch: metav1.ResourceVersionMatchExact, ResourceVersion: "0"},
		{ResourceVersion: "12345"},
	} {
		if _, err := listTemplateVersions(t, storage, opts); !apierrors.IsBadRequest(err) {
			t.Fatalf("expected BadRequest for %+v, got %v", opts, err)
		}
	}
}

func TestTemplateVersionStorageListAcrossNamespaces(t *testing.T) {
	t.Parallel()

	serverA, _ := newMockCoderServer(t)
	defer serverA.Close()
	serverB, _ := newMockCoderServer(t)
	defer serverB.Close()
	storage := NewTemplateVersionStorage(&multiNamespaceTestProvider{
		clients:    map[string]*codersdk.Client{"ns-a": newTestSDKClient(t, serverA.URL), "ns-b": newTestSDKClient(t, serverB.URL)},
		namespaces: []string{"ns-b", "ns-a"},
	})

	obj, err := storage.List(namespacedContext(""), nil)
	if err != nil {
		t.Fatalf("all-namespaces list: %v", err)
	}
	list := obj.(*aggregationv1alpha1.CoderTemplateVersionList)
	if len(list.Items) != 2 || list.Items[0].Namespace != "ns-a" || list.Items[1].Namespace != "ns-b" {
		t.Fatalf("expected one version per namespace sorted by namespace, got %+v", list.Items)
	}
}

func TestTemplateVersionStorageConvertToTable(t *testing.T) {
	t.Parallel()

	server, _ := newMockCoderServer(t)
	defer server.Close()
	storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))
	list, err := listTemplateVersions(t, storage, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	table, err := storage.ConvertToTable(namespacedContext("control-plane"), list, nil)
	if err != nil {
		t.Fatalf("convert list: %v", err)
	}
	var columns []string
	for _, column := range table.ColumnDefinitions {
		columns = append(columns, column.Name)
	}
	if strings.Join(columns, ",") != "Name,Active,Job,Archived,Created By,Age,ID,Message" ||
		table.ColumnDefinitions[6].Priority != 1 || table.ColumnDefinitions[5].Priority != 0 {
		t.Fatalf("unexpected columns: %+v", table.ColumnDefinitions)
	}
	row := table.Rows[0].Cells
	if len(table.Rows) != 1 || row[0] != "acme.starter-template.starter-template-v1" || row[1] != true || row[2] != "succeeded" {
		t.Fatalf("unexpected rows: %+v", table.Rows)
	}

	single, err := storage.ConvertToTable(namespacedContext("control-plane"), &list.Items[0], &metav1.TableOptions{NoHeaders: true})
	if err != nil || len(single.Rows) != 1 || single.ColumnDefinitions != nil || single.ResourceVersion != list.Items[0].ResourceVersion {
		t.Fatalf("unexpected single-object table: err=%v table=%+v", err, single)
	}
}

func TestTemplateVersionTableTruncatesMessageOnRunes(t *testing.T) {
	t.Parallel()

	storage := &TemplateVersionStorage{}
	version := &aggregationv1alpha1.CoderTemplateVersion{}
	version.Spec.Message = strings.Repeat("é", 70) + "\nsecond line"

	table, err := storage.ConvertToTable(namespacedContext("control-plane"), version, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	message, _ := table.Rows[0].Cells[7].(string)
	if message != strings.Repeat("é", 57)+"..." || !utf8.ValidString(message) {
		t.Fatalf("expected 57 runes plus an ellipsis, got %q", message)
	}
}

func TestTemplateVersionStorageListStopsAtTimeBudget(t *testing.T) {
	t.Parallel()

	server, state := newMockCoderServer(t)
	defer server.Close()
	seedTemplateWithVersions(t, state, "docker", "v1")
	seedTemplateWithVersions(t, state, "podman", "v1")
	state.mu.Lock()
	state.versionReadDelay = 300 * time.Millisecond // 3 templates take 900ms in total
	state.mu.Unlock()
	storage := NewTemplateVersionStorage(newTestClientProvider(t, server.URL))
	if storage.readBudget != TemplateVersionReadBudget || TemplateVersionReadBudget >= 60*time.Second {
		t.Fatalf("the default budget %s must apply and stay below kube-apiserver's 60s proxy timeout", storage.readBudget)
	}
	storage.readBudget = 400 * time.Millisecond

	started := time.Now()
	list, err := listTemplateVersions(t, storage, nil)
	if !apierrors.IsTimeout(err) || list != nil {
		t.Fatalf("expected a 504 Timeout and no partial list, got list=%v err=%v", list, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the list ran for %s; the budget did not stop it", elapsed)
	}
}

// blockingNamespaceProvider models namespace discovery that waits until the request context ends.
type blockingNamespaceProvider struct{ coder.ClientProvider }

func (blockingNamespaceProvider) EligibleNamespaces(ctx context.Context) ([]string, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("list control planes: %w", ctx.Err())
}

func TestTemplateVersionStorageListBudgetCoversNamespaceDiscovery(t *testing.T) {
	t.Parallel()

	storage := NewTemplateVersionStorage(blockingNamespaceProvider{})
	storage.readBudget = 100 * time.Millisecond
	if _, err := storage.List(namespacedContext(""), nil); !apierrors.IsTimeout(err) {
		t.Fatalf("expected a 504 Timeout when namespace discovery outlasts the budget, got %v", err)
	}
}
