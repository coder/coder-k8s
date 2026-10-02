package apiserverapp

import (
	"net/http"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
)

// TestEnvtestWorkspaceTransitionRBAC: with real RBAC, start and stop need their own create
// grants. No verb on coderworkspaces and no log grant reaches them, and resourceNames limits a
// grant to one workspace.
func TestEnvtestWorkspaceTransitionRBAC(t *testing.T) {
	startSharedEnvtest(t)
	kubeconfigPath, _ := envtestUser(t, "coder-k8s-delegator", true, true)
	server := startEnvtestAuthServer(t, kubeconfigPath)
	userSubject := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
	}
	rule := func(resource string, verbs []string, names ...string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{aggGroup}, Resources: []string{resource}, Verbs: verbs, ResourceNames: names}}
	}
	grantRole(t, "test-ns", userSubject("tr-viewer"), "ws-transition-test-viewer", rule("coderworkspaces", []string{"get", "list", "watch"}))
	grantRole(t, "test-ns", userSubject("tr-editor"), "ws-transition-test-editor", rule("coderworkspaces", []string{"get", "list", "watch", "create", "update", "patch", "delete"}))
	grantRole(t, "test-ns", userSubject("tr-starter"), "ws-transition-test-starter", rule("coderworkspaces/start", []string{"create"}))
	grantRole(t, "test-ns", userSubject("tr-stopper"), "ws-transition-test-stopper", rule("coderworkspaces/stop", []string{"create"}, testWorkspaceName))
	grantRole(t, "test-ns", userSubject("tr-log-reader"), "ws-transition-test-log-reader", rule("coderworkspaces/log", []string{"get"}))

	frontProxy := envtestFrontProxyCA.clientCert(t, frontProxyName)
	// The request-header CA is loaded asynchronously after startup; poll until the starter's
	// start request is served.
	for deadline := time.Now().Add(15 * time.Second); ; {
		status, _ := server.do(t, &frontProxy, http.MethodPost, testStartPath, remoteUser("tr-starter"), `{}`)
		if status == http.StatusCreated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("starter start: status=%d, want 201", status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	calls := server.provider.calls.Load()
	otherStopPath := workspacesTestNS + "/default.testuser.other/stop"
	for _, d := range []struct{ user, path string }{
		{"tr-viewer", testStartPath},
		{"tr-viewer", testStopPath},
		{"tr-editor", testStartPath},
		{"tr-editor", testStopPath},
		{"tr-starter", testStopPath},
		{"tr-stopper", testStartPath},
		{"tr-stopper", otherStopPath},
		{"tr-log-reader", testStartPath},
	} {
		if status, body := server.do(t, &frontProxy, http.MethodPost, d.path, remoteUser(d.user), `{}`); status != http.StatusForbidden {
			t.Errorf("%s POST %s: status=%d, want 403; body=%.200s", d.user, d.path, status, body)
		}
	}
	if got := server.provider.calls.Load(); got != calls {
		t.Fatalf("denied start/stop requests reached Coder: calls %d -> %d", calls, got)
	}
	if status, body := server.do(t, &frontProxy, http.MethodPost, testStopPath, remoteUser("tr-stopper"), `{}`); status != http.StatusCreated {
		t.Fatalf("stopper stop: status=%d body=%.200s", status, body)
	}
}
