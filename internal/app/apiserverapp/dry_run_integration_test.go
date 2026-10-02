package apiserverapp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

// TestIntegrationServerSideDryRunNeverReachesCoder sends every write verb with
// ?dryRun=All through the real HTTP routes. Each must fail with 400 before
// storage sends any request to Coder: kubectl diff, kubectl apply
// --dry-run=server and Argo CD server-side diff must never change Coder.
func TestIntegrationServerSideDryRunNeverReachesCoder(t *testing.T) {
	t.Parallel()

	harness := startIntegrationAggregatedAPIServer(t)
	resourceBase := harness.baseURL + "/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/"
	templatePath := resourceBase + "codertemplates/default.my-template"
	workspacePath := resourceBase + "coderworkspaces/default.testuser.my-workspace"

	// Read the current objects: this waits for the server and gives full
	// bodies for PUT.
	var template aggregationv1alpha1.CoderTemplate
	mustGetJSONWithRetry(t, harness.httpClient, harness.errCh, templatePath, &template)
	template.Spec.DisplayName = "Changed By Dry Run"
	var workspace aggregationv1alpha1.CoderWorkspace
	mustGetJSONWithRetry(t, harness.httpClient, harness.errCh, workspacePath, &workspace)
	if !workspace.Spec.Running {
		t.Fatal("assertion failed: fixture workspace must be running so a stop is a real change")
	}
	workspace.Spec.Running = false

	const (
		mergePatch = "application/merge-patch+json"
		jsonPatch  = "application/json-patch+json"
		applyPatch = "application/apply-patch+yaml"
		jsonBody   = "application/json"
	)

	cases := []struct {
		name        string
		method      string
		url         string
		contentType string
		body        string
	}{
		{"template create", http.MethodPost, resourceBase + "codertemplates", jsonBody, mustJSON(t, map[string]any{
			"apiVersion": "aggregation.coder.com/v1alpha1", "kind": "CoderTemplate",
			"metadata": map[string]any{"name": "default.new-template"},
			"spec":     map[string]any{"organization": "default", "files": map[string]string{"main.tf": "# new\n"}},
		})},
		{"template update", http.MethodPut, templatePath, jsonBody, mustJSON(t, template)},
		{"template merge patch", http.MethodPatch, templatePath, mergePatch, `{"spec":{"displayName":"Changed By Dry Run"}}`},
		{"template json patch", http.MethodPatch, templatePath, jsonPatch, `[{"op":"add","path":"/spec/displayName","value":"Changed By Dry Run"}]`},
		{"template apply patch", http.MethodPatch, templatePath + "?fieldManager=dry-run-test&force=true", applyPatch, mustJSON(t, map[string]any{
			"apiVersion": "aggregation.coder.com/v1alpha1", "kind": "CoderTemplate",
			"metadata": map[string]any{"name": "default.my-template"},
			"spec":     map[string]any{"organization": "default", "displayName": "Changed By Dry Run"},
		})},
		{"template create on apply", http.MethodPatch, resourceBase + "codertemplates/default.new-template?fieldManager=dry-run-test", applyPatch, mustJSON(t, map[string]any{
			"apiVersion": "aggregation.coder.com/v1alpha1", "kind": "CoderTemplate",
			"metadata": map[string]any{"name": "default.new-template"},
			"spec":     map[string]any{"organization": "default", "files": map[string]string{"main.tf": "# new\n"}},
		})},
		{"template delete", http.MethodDelete, templatePath, "", ""},
		{"workspace create", http.MethodPost, resourceBase + "coderworkspaces", jsonBody, mustJSON(t, map[string]any{
			"apiVersion": "aggregation.coder.com/v1alpha1", "kind": "CoderWorkspace",
			"metadata": map[string]any{"name": "default.testuser.new-workspace"},
			"spec":     map[string]any{"organization": "default", "templateName": "my-template", "running": true},
		})},
		{"workspace update", http.MethodPut, workspacePath, jsonBody, mustJSON(t, workspace)},
		{"workspace merge patch", http.MethodPatch, workspacePath, mergePatch, `{"spec":{"running":false}}`},
		{"workspace json patch", http.MethodPatch, workspacePath, jsonPatch, `[{"op":"replace","path":"/spec/running","value":false}]`},
		{"workspace apply patch", http.MethodPatch, workspacePath + "?fieldManager=dry-run-test&force=true", applyPatch, mustJSON(t, map[string]any{
			"apiVersion": "aggregation.coder.com/v1alpha1", "kind": "CoderWorkspace",
			"metadata": map[string]any{"name": "default.testuser.my-workspace"},
			"spec":     map[string]any{"running": false},
		})},
		{"workspace delete", http.MethodDelete, workspacePath, "", ""},
	}

	// Control: the same template merge patch without dryRun reaches Coder
	// with a write, so an empty recording below is meaningful.
	harness.mockCoder.resetRecordedRequests()
	doIntegrationRequest(t, harness, http.MethodPatch, templatePath, mergePatch, `{"spec":{"displayName":"Changed"}}`)
	if !hasWriteRequest(harness.mockCoder.recordedRequests()) {
		t.Fatalf("assertion failed: control patch without dryRun sent no write to Coder: %v", harness.mockCoder.recordedRequests())
	}

	// Subtests run in order: they share the mock's request recording.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness.mockCoder.resetRecordedRequests()

			requestURL := tc.url
			if strings.Contains(requestURL, "?") {
				requestURL += "&dryRun=All"
			} else {
				requestURL += "?dryRun=All"
			}
			status, body := doIntegrationRequest(t, harness, tc.method, requestURL, tc.contentType, tc.body)

			if recorded := harness.mockCoder.recordedRequests(); len(recorded) != 0 {
				t.Fatalf("dry-run %s sent %d request(s) to Coder: %v", tc.name, len(recorded), recorded)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("expected status 400 for dry-run %s, got %d: %s", tc.name, status, body)
			}
			if !strings.Contains(body, "server-side dry-run is not supported") {
				t.Fatalf("expected the dry-run rejection message for %s, got: %s", tc.name, body)
			}
		})
	}
}

func doIntegrationRequest(t *testing.T, harness integrationAggregatedAPIServer, method, requestURL, contentType, body string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	request, err := http.NewRequest(method, requestURL, reader)
	if err != nil {
		t.Fatalf("create %s request %q: %v", method, requestURL, err)
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	response, err := harness.httpClient.Do(request)
	if err != nil {
		t.Fatalf("send %s request %q: %v", method, requestURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s response %q: %v", method, requestURL, err)
	}

	return response.StatusCode, string(responseBody)
}

// hasWriteRequest reports whether any recorded "METHOD /path" is not a read.
func hasWriteRequest(recorded []string) bool {
	for _, entry := range recorded {
		if !strings.HasPrefix(entry, http.MethodGet+" ") {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
	return string(encoded)
}
