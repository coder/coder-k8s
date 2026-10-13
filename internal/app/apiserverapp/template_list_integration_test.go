package apiserverapp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// TestIntegrationTemplateListAppliesListOptions checks codertemplates LIST options over real HTTP
// routes: selectors filter, limit is ignored, and unsupported options fail before Coder is called.
func TestIntegrationTemplateListAppliesListOptions(t *testing.T) {
	t.Parallel()

	harness := startIntegrationAggregatedAPIServer(t)
	templates := harness.baseURL + "/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/codertemplates"
	var warmup aggregationv1alpha1.CoderTemplateList
	mustGetJSONWithRetry(t, harness.httpClient, harness.errCh, templates, &warmup)

	for _, tc := range []struct {
		query     url.Values
		wantItems int
	}{
		{url.Values{"fieldSelector": {"metadata.name=default.my-template"}}, 1},
		{url.Values{"fieldSelector": {"metadata.name=default.other"}}, 0},
		{url.Values{"fieldSelector": {"metadata.namespace=test-ns"}}, 1},
		// Templates carry no labels, so a selector that requires one matches nothing.
		{url.Values{"labelSelector": {"team=platform"}}, 0},
		{url.Values{"labelSelector": {"!team"}}, 1},
		{url.Values{"limit": {"1"}}, 1},
		{url.Values{"resourceVersion": {"123"}}, 1},
	} {
		status, body := doIntegrationRequest(t, harness, http.MethodGet, templates+"?"+tc.query.Encode(), "", "")
		var list aggregationv1alpha1.CoderTemplateList
		if status != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil {
			t.Fatalf("GET ?%s: expected 200 with a list, got %d: %.300s", tc.query.Encode(), status, body)
		}
		if len(list.Items) != tc.wantItems || list.Continue != "" {
			t.Fatalf("GET ?%s: expected %d items and no continue token, got %d items continue=%q", tc.query.Encode(), tc.wantItems, len(list.Items), list.Continue)
		}
	}

	harness.mockCoder.resetRecordedRequests()
	for _, query := range []url.Values{
		{"fieldSelector": {"spec.organization=default"}},
		{"continue": {"abc"}},
		{"resourceVersion": {"0"}, "resourceVersionMatch": {"NotOlderThan"}},
	} {
		if status, body := doIntegrationRequest(t, harness, http.MethodGet, templates+"?"+query.Encode(), "", ""); status != http.StatusBadRequest {
			t.Errorf("GET ?%s: expected 400, got %d: %.300s", query.Encode(), status, body)
		}
	}
	if recorded := harness.mockCoder.recordedRequests(); len(recorded) != 0 {
		t.Fatalf("rejected requests reached Coder: %v", recorded)
	}
}
