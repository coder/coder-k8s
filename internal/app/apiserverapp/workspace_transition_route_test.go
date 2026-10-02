package apiserverapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

const (
	testStartPath = workspacesTestNS + "/" + testWorkspaceName + "/start"
	testStopPath  = workspacesTestNS + "/" + testWorkspaceName + "/stop"
)

// countBuildPosts counts the build POSTs the integration mock received.
func countBuildPosts(mock *integrationMockCoderServer) int {
	n := 0
	for _, r := range mock.recordedRequests() {
		if strings.HasPrefix(r, http.MethodPost+" ") && strings.HasSuffix(r, "/builds") {
			n++
		}
	}
	return n
}

func decodeTransition(t *testing.T, body string) aggregationv1alpha1.CoderWorkspaceTransitionStatus {
	t.Helper()
	var obj aggregationv1alpha1.CoderWorkspaceTransition
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if obj.Kind != "CoderWorkspaceTransition" || obj.Name != testWorkspaceName || obj.Namespace != "test-ns" {
		t.Fatalf("unexpected transition object: %s", body)
	}
	return obj.Status
}

// TestWorkspaceTransitionRoutes runs coderworkspaces/start and /stop through the production server.
func TestWorkspaceTransitionRoutes(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) })
	cert := f.frontProxyCert(t)
	alice := remoteUser("alice")

	status, body := f.server.do(t, cert, http.MethodGet, "/apis/aggregation.coder.com/v1alpha1", alice, "")
	if status != http.StatusOK {
		t.Fatalf("discovery: status=%d", status)
	}
	var resources metav1.APIResourceList
	if err := json.Unmarshal([]byte(body), &resources); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range resources.APIResources {
		if r.Name == "coderworkspaces/start" || r.Name == "coderworkspaces/stop" {
			found[r.Name] = true
			if len(r.Verbs) != 1 || r.Verbs[0] != "create" || !r.Namespaced || r.Kind != "CoderWorkspaceTransition" {
				t.Fatalf("%s discovery entry: %+v", r.Name, r)
			}
		}
	}
	if len(found) != 2 {
		t.Fatalf("discovery lacks start or stop: %v", found)
	}

	// The mock workspace runs: start is Unchanged without a POST, stop is queued.
	status, body = f.server.do(t, cert, http.MethodPost, testStartPath, alice, `{}`)
	if status != http.StatusCreated || decodeTransition(t, body).Outcome != "Unchanged" || countBuildPosts(f.server.mock) != 0 {
		t.Fatalf("start: status=%d body=%s posts=%d", status, body, countBuildPosts(f.server.mock))
	}
	status, body = f.server.do(t, cert, http.MethodPost, testStopPath, alice, `{"apiVersion":"aggregation.coder.com/v1alpha1","kind":"CoderWorkspaceTransition"}`)
	if got := decodeTransition(t, body); status != http.StatusCreated || got.Outcome != "Queued" || got.Transition != "stop" || got.BuildNumber != 2 || countBuildPosts(f.server.mock) != 1 {
		t.Fatalf("stop: status=%d body=%s posts=%d", status, body, countBuildPosts(f.server.mock))
	}

	// A dry-run through the real handler chain reports what would happen and posts nothing.
	status, body = f.server.do(t, cert, http.MethodPost, testStopPath+"?dryRun=All", alice, `{}`)
	if got := decodeTransition(t, body); status != http.StatusCreated || got.Outcome != "WouldQueue" || !got.DryRun || got.BuildID != "" || countBuildPosts(f.server.mock) != 1 {
		t.Fatalf("dry-run stop: status=%d body=%s posts=%d", status, body, countBuildPosts(f.server.mock))
	}

	if status, body := f.server.do(t, cert, http.MethodPost, testStopPath, alice, `{"metadata":{"name":"default.testuser.other"}}`); status != http.StatusBadRequest {
		t.Fatalf("body name mismatch: status=%d body=%.200s", status, body)
	}
	for _, path := range []string{testStartPath, testStopPath} {
		if status, _ := f.server.do(t, nil, http.MethodPost, path, nil, `{}`); status != http.StatusUnauthorized {
			t.Errorf("anonymous POST %s: status=%d, want 401", path, status)
		}
	}
	if got := countBuildPosts(f.server.mock); got != 1 {
		t.Fatalf("build POSTs: %d, want 1", got)
	}
}
