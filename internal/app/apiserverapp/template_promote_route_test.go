package apiserverapp

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
)

const (
	testTemplatesNS        = "/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/codertemplates"
	testPromotePath        = testTemplatesNS + "/default.my-template/promote"
	testActiveVersionID    = "33333333-3333-3333-3333-333333333333"
	testInactiveVersionID  = "77777777-7777-7777-7777-777777777770"
	testTemplateVersionsNS = "/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/codertemplateversions"
)

func promoteBody(versionID string) string {
	return `{"apiVersion":"aggregation.coder.com/v1alpha1","kind":"CoderTemplateVersionPromotion","spec":{"versionID":"` + versionID + `"}}`
}

// TestTemplatePromoteRoute runs codertemplates/promote through the production server: discovery,
// the authorization attributes, the preview results, and that no request writes to Coder.
func TestTemplatePromoteRoute(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) })
	cert := f.frontProxyCert(t)
	alice := remoteUser("alice")

	status, body := f.server.do(t, cert, http.MethodGet, "/apis/aggregation.coder.com/v1alpha1", alice, "")
	var resources metav1.APIResourceList
	if status != http.StatusOK || json.Unmarshal([]byte(body), &resources) != nil {
		t.Fatalf("discovery: status=%d body=%.300s", status, body)
	}
	verbs := map[string]string{}
	for _, r := range resources.APIResources {
		sorted := append([]string(nil), r.Verbs...)
		sort.Strings(sorted)
		verbs[r.Name] = strings.Join(sorted, ",")
		if r.Name == "codertemplates/promote" && (r.Kind != "CoderTemplateVersionPromotion" || !r.Namespaced) {
			t.Fatalf("codertemplates/promote discovery entry: %+v", r)
		}
	}
	if verbs["codertemplates/promote"] != "create" || verbs["codertemplates"] != "create,delete,get,list,patch,update,watch" ||
		verbs["codertemplateversions"] != "get,list" {
		t.Fatalf("unexpected discovery verbs: %v", verbs)
	}

	f.server.mock.resetRecordedRequests()
	for _, tc := range []struct {
		query, versionID string
		result           aggregationv1alpha1.CoderTemplateVersionPromotionResult
	}{
		{"?dryRun=All", testInactiveVersionID, aggregationv1alpha1.PromotionResultWouldPromote},
		{"?dryRun=All", testActiveVersionID, aggregationv1alpha1.PromotionResultAlreadyActive},
		{"", testActiveVersionID, aggregationv1alpha1.PromotionResultAlreadyActive},
	} {
		status, body := f.server.do(t, cert, http.MethodPost, testPromotePath+tc.query, alice, promoteBody(tc.versionID))
		var got aggregationv1alpha1.CoderTemplateVersionPromotion
		if status != http.StatusCreated || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("promote %s%s: status=%d body=%.300s", tc.versionID, tc.query, status, body)
		}
		want := aggregationv1alpha1.CoderTemplateVersionPromotionStatus{
			Result: tc.result, PreviousActiveVersionID: testActiveVersionID, ActiveVersionID: testActiveVersionID,
		}
		if got.Status != want || got.Name != "default.my-template" || got.Namespace != "test-ns" || got.Kind != "CoderTemplateVersionPromotion" {
			t.Fatalf("promote %s%s: got %+v", tc.versionID, tc.query, got)
		}
	}
	for _, tc := range []struct {
		query, body string
		status      int
		message     string
	}{
		{"", promoteBody(testInactiveVersionID), http.StatusBadRequest, "promotion is not enabled yet; use dryRun=All to preview it"},
		{"?dryRun=Bogus", promoteBody(testInactiveVersionID), http.StatusUnprocessableEntity, "dryRun"},
		{"?dryRun=All", promoteBody("not-a-uuid"), http.StatusUnprocessableEntity, "spec.versionID"},
	} {
		if status, body := f.server.do(t, cert, http.MethodPost, testPromotePath+tc.query, alice, tc.body); status != tc.status || !strings.Contains(body, tc.message) {
			t.Errorf("POST promote%s: status=%d want %d, body=%.300s", tc.query, status, tc.status, body)
		}
	}
	for _, request := range f.server.mock.recordedRequests() {
		if !strings.HasPrefix(request, "GET ") || strings.Contains(request, "/files") {
			t.Fatalf("promote preview sent a write or a file download to Coder: %v", f.server.mock.recordedRequests())
		}
	}

	var promoteSARs []string
	for _, sar := range f.kube.recordedSARs() {
		if sar.ResourceAttributes != nil && sar.ResourceAttributes.Subresource == "promote" {
			promoteSARs = append(promoteSARs, describeSAR(sar))
		}
	}
	want := "user=alice verb=create group=aggregation.coder.com resource=codertemplates subresource=promote ns=test-ns name=default.my-template"
	if len(promoteSARs) == 0 || promoteSARs[0] != want {
		t.Fatalf("promote SARs = %v, want %q", promoteSARs, want)
	}
}

// TestTemplatePromoteRouteDeniedReachesNoBackend: a denied promote answers 403 before storage runs.
func TestTemplatePromoteRouteDeniedReachesNoBackend(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) {
		k.setDecide(func(spec authorizationv1.SubjectAccessReviewSpec) bool {
			return spec.ResourceAttributes == nil || spec.ResourceAttributes.Subresource != "promote"
		})
	})
	cert := f.frontProxyCert(t)
	calls := f.server.provider.calls.Load()
	if status, body := f.server.do(t, cert, http.MethodPost, testPromotePath+"?dryRun=All", remoteUser("alice"), promoteBody(testInactiveVersionID)); status != http.StatusForbidden {
		t.Fatalf("denied promote: status=%d body=%.300s", status, body)
	}
	if got := f.server.provider.calls.Load(); got != calls {
		t.Fatalf("denied promote reached Coder: calls %d -> %d", calls, got)
	}
}

// TestEnvtestTemplatePromoteRBAC: with real RBAC, only a codertemplates/promote grant allows promote,
// resourceNames limit it to the named template, and no codertemplates or codertemplateversions
// rule covers it.
func TestEnvtestTemplatePromoteRBAC(t *testing.T) {
	startSharedEnvtest(t)
	kubeconfigPath, _ := envtestUser(t, "coder-k8s-delegator", true, true)
	server := startEnvtestAuthServer(t, kubeconfigPath)
	user := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
	}
	rule := func(resource string, names []string, verbs ...string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{aggGroup}, Resources: []string{resource}, ResourceNames: names, Verbs: verbs}}
	}
	grantRole(t, "test-ns", user("alice-reader"), "promote-test-reader", rule("codertemplateversions", nil, "get", "list"))
	grantRole(t, "test-ns", user("bob-ci"), "promote-test-bob", rule("codertemplates/promote", []string{"default.my-template"}, "create"))
	grantRole(t, "test-ns", user("template-editor"), "promote-test-editor", rule("codertemplates", nil, "get", "list", "update", "patch"))
	grantRole(t, "test-ns", user("ns-promoter"), "promote-test-ns", rule("codertemplates/promote", nil, "create"))

	frontProxy := envtestFrontProxyCA.clientCert(t, frontProxyName)
	// The request-header CA is loaded asynchronously after startup; poll until a granted read is served.
	for deadline := time.Now().Add(15 * time.Second); ; {
		if status, _ := server.do(t, &frontProxy, http.MethodGet, testTemplateVersionsNS, remoteUser("alice-reader"), ""); status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alice-reader version LIST never returned 200")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// send returns the status and fails the test if a 403 reached Coder.
	send := func(who, method, path, body string) int {
		calls := server.provider.calls.Load()
		status, _ := server.do(t, &frontProxy, method, path, remoteUser(who), body)
		if after := server.provider.calls.Load(); status == http.StatusForbidden && after != calls {
			t.Errorf("%s %s %s: denied request reached Coder: calls %d -> %d", who, method, path, calls, after)
		}
		return status
	}
	promote := func(who, template string) int {
		return send(who, http.MethodPost, testTemplatesNS+"/"+template+"/promote?dryRun=All", promoteBody(testInactiveVersionID))
	}
	// default.other does not exist in Coder: 404 means that RBAC allowed the request.
	matrix := []struct {
		who                  string
		mine, other, reading int
	}{
		{"alice-reader", http.StatusForbidden, http.StatusForbidden, http.StatusOK},
		{"bob-ci", http.StatusCreated, http.StatusForbidden, http.StatusForbidden},
		{"template-editor", http.StatusForbidden, http.StatusForbidden, http.StatusForbidden},
		{"ns-promoter", http.StatusCreated, http.StatusNotFound, http.StatusForbidden},
	}
	for _, row := range matrix {
		got := []int{promote(row.who, "default.my-template"), promote(row.who, "default.other"), send(row.who, http.MethodGet, testTemplateVersionsNS, "")}
		if want := []int{row.mine, row.other, row.reading}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("%s: promote mine/other and read versions = %v, want %v", row.who, got, want)
		}
	}
}
