package apiserverapp

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// TestIntegrationTemplateVersionsAreReadOnly checks the served codertemplateversions resource over
// real HTTP routes: discovery, GET and LIST identity, Table output, and that every other verb
// fails before storage sends anything to Coder.
func TestIntegrationTemplateVersionsAreReadOnly(t *testing.T) {
	t.Parallel()

	harness := startIntegrationAggregatedAPIServer(t)
	groupVersion := harness.baseURL + "/apis/aggregation.coder.com/v1alpha1"
	versions := groupVersion + "/namespaces/test-ns/codertemplateversions"

	var resources metav1.APIResourceList
	mustGetJSONWithRetry(t, harness.httpClient, harness.errCh, groupVersion, &resources)
	verbs := map[string]string{}
	for _, resource := range resources.APIResources {
		sorted := append([]string(nil), resource.Verbs...)
		sort.Strings(sorted)
		verbs[resource.Name] = strings.Join(sorted, ",")
		if resource.Name == "codertemplateversions" &&
			(resource.Kind != "CoderTemplateVersion" || resource.SingularName != "codertemplateversion" || !resource.Namespaced) {
			t.Fatalf("unexpected discovery entry: %+v", resource)
		}
	}
	if verbs["codertemplateversions"] != "get,list" {
		t.Fatalf("expected codertemplateversions verbs get,list, got %q", verbs["codertemplateversions"])
	}
	for _, name := range []string{"codertemplates", "coderworkspaces"} {
		if verbs[name] != "create,delete,get,list,patch,update,watch" {
			t.Fatalf("expected %s to keep its verbs, got %q", name, verbs[name])
		}
	}

	var list aggregationv1alpha1.CoderTemplateVersionList
	mustGetJSONWithRetry(t, harness.httpClient, harness.errCh, versions, &list)
	var names []string
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	// A dotted version name, and "V1" and "v1" as two distinct objects.
	if strings.Join(names, " ") != "default.my-template.v1.0.0 default.my-template.V1 default.my-template.v1" {
		t.Fatalf("unexpected list items: %v", names)
	}
	for _, item := range list.Items {
		var got aggregationv1alpha1.CoderTemplateVersion
		mustGetJSONWithRetry(t, harness.httpClient, harness.errCh, versions+"/"+item.Name, &got)
		if got.Name != item.Name || got.UID != item.UID || got.ResourceVersion != item.ResourceVersion {
			t.Fatalf("GET %q disagrees with LIST: %+v", item.Name, got.ObjectMeta)
		}
	}

	status, body := doIntegrationRequestWithAccept(t, harness, versions, "application/json;as=Table;v=v1;g=meta.k8s.io")
	var table metav1.Table
	if status != http.StatusOK || json.Unmarshal([]byte(body), &table) != nil || len(table.Rows) != 3 || table.ColumnDefinitions[1].Name != "Active" {
		t.Fatalf("expected a 3-row Table, got status=%d body=%.300s", status, body)
	}

	harness.mockCoder.resetRecordedRequests()
	for _, tc := range []struct{ method, url string }{
		{http.MethodGet, versions + "?watch=true"},
		{http.MethodPost, versions},
		{http.MethodPut, versions + "/default.my-template.v1"},
		{http.MethodPatch, versions + "/default.my-template.v1"},
		{http.MethodDelete, versions + "/default.my-template.v1"},
		{http.MethodDelete, versions},
	} {
		status, body := doIntegrationRequest(t, harness, tc.method, tc.url, "application/merge-patch+json", "{}")
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %d: %.200s", tc.method, tc.url, status, body)
		}
	}
	// Unsupported list options are rejected; generic validation may answer before storage does.
	for _, query := range []string{"?continue=abc", "?resourceVersion=0&resourceVersionMatch=Exact", "?resourceVersion=123"} {
		if status, body := doIntegrationRequest(t, harness, http.MethodGet, versions+query, "", ""); status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
			t.Errorf("GET %s: expected 400 or 422, got %d: %.200s", query, status, body)
		}
	}
	if recorded := harness.mockCoder.recordedRequests(); len(recorded) != 0 {
		t.Fatalf("rejected requests reached Coder: %v", recorded)
	}
}

func doIntegrationRequestWithAccept(t *testing.T, harness integrationAggregatedAPIServer, requestURL, accept string) (int, string) {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatalf("create request %q: %v", requestURL, err)
	}
	request.Header.Set("Accept", accept)
	response, err := harness.httpClient.Do(request)
	if err != nil {
		t.Fatalf("send request %q: %v", requestURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response %q: %v", requestURL, err)
	}
	return response.StatusCode, string(body)
}
