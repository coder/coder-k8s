package apiserverapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
)

// watchOptionCase is one frozen row of the watch option matrix. Each request
// goes through the generic handler (handlers/get.go: decode, then
// SetListOptionsDefaults, then ValidateListOptions) before storage Watch.
type watchOptionCase struct {
	name  string
	query func(resourceVersion string) url.Values
	// wantStatus is the only accepted HTTP status for this row.
	wantStatus int
	// wantReason and wantMessage are checked only for non-200 rows.
	wantReason  metav1.StatusReason
	wantMessage string
	// wantCauseField is set for 422 Invalid rows, which must carry exactly one
	// FieldValueForbidden (metav1.CauseTypeForbidden) cause on this field.
	wantCauseField string
}

const (
	msgSendInitialEventsTrueUnsupported      = "invalid watch options: sendInitialEvents=true is not supported for this watch endpoint"
	msgResourceVersionMatchUnsupported       = `invalid watch options: resourceVersionMatch "NotOlderThan" is not supported for this watch endpoint`
	msgSendInitialEventsRequiresNotOlderThan = "resourceVersionMatch: Forbidden: sendInitialEvents requires setting resourceVersionMatch to NotOlderThan"
	msgResourceVersionMatchRequiresSIE       = "resourceVersionMatch: Forbidden: resourceVersionMatch is forbidden for watch unless sendInitialEvents is provided"
)

func watchOptionMatrix() []watchOptionCase {
	return []watchOptionCase{
		{
			// WatchList defaulting turns a legacy watch into
			// sendInitialEvents=true&resourceVersionMatch=NotOlderThan,
			// which storage rejects.
			name:        "a_no_resource_version",
			query:       func(string) url.Values { return url.Values{"watch": {"true"}} },
			wantStatus:  http.StatusBadRequest,
			wantReason:  metav1.StatusReasonBadRequest,
			wantMessage: msgSendInitialEventsTrueUnsupported,
		},
		{
			name: "b_resource_version_0",
			query: func(string) url.Values {
				return url.Values{"watch": {"true"}, "resourceVersion": {"0"}}
			},
			wantStatus:  http.StatusBadRequest,
			wantReason:  metav1.StatusReasonBadRequest,
			wantMessage: msgSendInitialEventsTrueUnsupported,
		},
		{
			// A non-legacy resourceVersion skips defaulting, so the watch starts.
			name: "c_resource_version_from_get",
			query: func(resourceVersion string) url.Values {
				return url.Values{"watch": {"true"}, "resourceVersion": {resourceVersion}}
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "d_send_initial_events_false_only",
			query: func(string) url.Values {
				return url.Values{"watch": {"true"}, "sendInitialEvents": {"false"}}
			},
			wantStatus:     http.StatusUnprocessableEntity,
			wantReason:     metav1.StatusReasonInvalid,
			wantMessage:    msgSendInitialEventsRequiresNotOlderThan,
			wantCauseField: "resourceVersionMatch",
		},
		{
			name: "e_send_initial_events_false_with_not_older_than",
			query: func(string) url.Values {
				return url.Values{
					"watch":                {"true"},
					"sendInitialEvents":    {"false"},
					"resourceVersionMatch": {string(metav1.ResourceVersionMatchNotOlderThan)},
				}
			},
			wantStatus:  http.StatusBadRequest,
			wantReason:  metav1.StatusReasonBadRequest,
			wantMessage: msgResourceVersionMatchUnsupported,
		},
		{
			name: "f_send_initial_events_true_with_not_older_than",
			query: func(string) url.Values {
				return url.Values{
					"watch":                {"true"},
					"sendInitialEvents":    {"true"},
					"resourceVersionMatch": {string(metav1.ResourceVersionMatchNotOlderThan)},
				}
			},
			wantStatus:  http.StatusBadRequest,
			wantReason:  metav1.StatusReasonBadRequest,
			wantMessage: msgSendInitialEventsTrueUnsupported,
		},
		{
			name: "g_resource_version_match_only",
			query: func(string) url.Values {
				return url.Values{
					"watch":                {"true"},
					"resourceVersionMatch": {string(metav1.ResourceVersionMatchNotOlderThan)},
				}
			},
			wantStatus:     http.StatusUnprocessableEntity,
			wantReason:     metav1.StatusReasonInvalid,
			wantMessage:    msgResourceVersionMatchRequiresSIE,
			wantCauseField: "resourceVersionMatch",
		},
		{
			name: "g_resource_version_match_with_resource_version",
			query: func(resourceVersion string) url.Values {
				return url.Values{
					"watch":                {"true"},
					"resourceVersion":      {resourceVersion},
					"resourceVersionMatch": {string(metav1.ResourceVersionMatchNotOlderThan)},
				}
			},
			wantStatus:     http.StatusUnprocessableEntity,
			wantReason:     metav1.StatusReasonInvalid,
			wantMessage:    msgResourceVersionMatchRequiresSIE,
			wantCauseField: "resourceVersionMatch",
		},
	}
}

func TestIntegrationWatchOptionsThroughGenericAPIServer(t *testing.T) {
	t.Parallel()

	harness := startIntegrationAggregatedAPIServer(t)

	// The generic handler reads this global gate per request. Every row
	// below assumes WatchList is enabled; if a dependency bump changes the
	// default, fail here instead of silently shifting expectations.
	if !utilfeature.DefaultFeatureGate.Enabled(features.WatchList) {
		t.Fatalf(
			"assertion failed: expected WatchList feature gate enabled (emulation version %s); watch option matrix must be re-derived",
			utilfeature.DefaultMutableFeatureGate.EmulationVersion(),
		)
	}

	resources := []struct {
		resource string
		kind     string
		name     string
	}{
		{resource: "coderworkspaces", kind: "CoderWorkspace", name: "default.testuser.my-workspace"},
		{resource: "codertemplates", kind: "CoderTemplate", name: "default.my-template"},
	}

	for _, resource := range resources {
		t.Run(resource.resource, func(t *testing.T) {
			collectionURL := harness.baseURL + "/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/" + resource.resource

			var object metav1.PartialObjectMetadata
			mustGetJSONWithRetry(t, harness.httpClient, harness.errCh, collectionURL+"/"+resource.name, &object)
			if object.Kind != resource.kind || object.Name != resource.name {
				t.Fatalf("assertion failed: GET returned %s %q, want %s %q", object.Kind, object.Name, resource.kind, resource.name)
			}
			resourceVersion := object.ResourceVersion
			if resourceVersion == "" || resourceVersion == "0" {
				t.Fatalf("assertion failed: GET must return a non-legacy resourceVersion, got %q", resourceVersion)
			}

			for _, tc := range watchOptionMatrix() {
				t.Run(tc.name, func(t *testing.T) {
					assertWatchOptionCase(t, harness.httpClient, collectionURL+"?"+tc.query(resourceVersion).Encode(), tc)
				})
			}
		})
	}
}

func assertWatchOptionCase(t *testing.T, client *http.Client, requestURL string, tc watchOptionCase) {
	t.Helper()

	if client == nil {
		t.Fatal("assertion failed: HTTP client must not be nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatalf("create request %q: %v", requestURL, err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("watch request %q: %v", requestURL, err)
	}
	t.Cleanup(func() {
		_ = response.Body.Close()
	})

	if response.StatusCode != tc.wantStatus {
		// Never read a 200 body here: a started watch stream does not end.
		detail := "watch stream started"
		if response.StatusCode != http.StatusOK {
			unexpectedBody, _ := io.ReadAll(response.Body)
			detail = "body=" + string(unexpectedBody)
		}
		t.Fatalf("expected status %d for %q, got %d (%s)", tc.wantStatus, requestURL, response.StatusCode, detail)
	}

	if tc.wantStatus == http.StatusOK {
		// Headers are flushed when the watch stream starts
		// (handlers/watch.go HandleHTTP). Do not wait for events.
		if got := response.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected watch Content-Type application/json, got %q", got)
		}
		if !slices.Equal(response.TransferEncoding, []string{"chunked"}) {
			t.Errorf("expected chunked watch stream, got TransferEncoding %v", response.TransferEncoding)
		}
		cancel()
		if err := response.Body.Close(); err != nil {
			t.Errorf("close watch body: %v", err)
		}
		return
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body for %q: %v", requestURL, err)
	}

	var status metav1.Status
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode Status for %q: %v (body=%s)", requestURL, err, body)
	}
	if status.Kind != "Status" || status.Status != metav1.StatusFailure {
		t.Fatalf("expected Failure Status, got kind=%q status=%q body=%s", status.Kind, status.Status, body)
	}
	if int(status.Code) != tc.wantStatus {
		t.Errorf("expected Status.code %d, got %d", tc.wantStatus, status.Code)
	}
	if status.Reason != tc.wantReason {
		t.Errorf("expected Status.reason %q, got %q", tc.wantReason, status.Reason)
	}
	if !strings.Contains(status.Message, tc.wantMessage) {
		t.Errorf("expected Status.message to contain %q, got %q", tc.wantMessage, status.Message)
	}

	if tc.wantCauseField == "" {
		if status.Details != nil {
			t.Errorf("expected no Status.details, got %+v", status.Details)
		}
		return
	}
	if status.Details == nil {
		t.Fatalf("expected Status.details for Invalid response, body=%s", body)
	}
	if status.Details.Group != metav1.GroupName || status.Details.Kind != "ListOptions" {
		t.Errorf("expected details for %s ListOptions, got group=%q kind=%q", metav1.GroupName, status.Details.Group, status.Details.Kind)
	}
	if len(status.Details.Causes) != 1 {
		t.Fatalf("expected exactly one cause, got %+v", status.Details.Causes)
	}
	cause := status.Details.Causes[0]
	if cause.Type != metav1.CauseTypeForbidden || cause.Field != tc.wantCauseField {
		t.Errorf("expected %s cause on %q, got %s on %q", metav1.CauseTypeForbidden, tc.wantCauseField, cause.Type, cause.Field)
	}
}
