package apiserverapp

import (
	"net/http"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
)

// TestEnvtestWorkspaceLogRBAC: with real RBAC, no verb on coderworkspaces grants the log
// subresource, impersonation re-authorizes as the impersonated user, and a log grant works.
func TestEnvtestWorkspaceLogRBAC(t *testing.T) {
	startSharedEnvtest(t)
	kubeconfigPath, _ := envtestUser(t, "coder-k8s-delegator", true, true)
	server := startEnvtestAuthServer(t, kubeconfigPath)
	userSubject := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
	}
	workspaceRule := func(verbs ...string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{aggGroup}, Resources: []string{"coderworkspaces"}, Verbs: verbs}}
	}
	logRule := []rbacv1.PolicyRule{{APIGroups: []string{aggGroup}, Resources: []string{"coderworkspaces/log"}, Verbs: []string{"get"}}}
	grantRole(t, "test-ns", userSubject("ws-viewer"), "ws-log-test-viewer", workspaceRule("get", "list", "watch"))
	grantRole(t, "test-ns", userSubject("ws-editor"), "ws-log-test-editor", workspaceRule("get", "list", "watch", "update", "patch", "delete"))
	grantRole(t, "test-ns", userSubject("ws-log-reader"), "ws-log-test-reader", logRule)

	frontProxy := envtestFrontProxyCA.clientCert(t, frontProxyName)
	// The request-header CA is loaded asynchronously after startup; poll until the viewer's
	// ordinary workspace read is served.
	for deadline := time.Now().Add(15 * time.Second); ; {
		status, _ := server.do(t, &frontProxy, http.MethodGet, workspacesTestNS+"/"+testWorkspaceName, remoteUser("ws-viewer"), "")
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("viewer workspace GET: status=%d, want 200", status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	calls := server.provider.calls.Load()
	impersonateViewer := mergeHeaders(remoteUser("admin", "system:masters"), map[string]string{"Impersonate-User": "ws-viewer"})
	for name, headers := range map[string]map[string]string{
		"viewer":                remoteUser("ws-viewer"),
		"editor":                remoteUser("ws-editor"),
		"admin as ws-viewer":    impersonateViewer,
		"unknown authenticated": remoteUser("mallory"),
	} {
		if status, body := server.do(t, &frontProxy, http.MethodGet, testLogPath, headers, ""); status != http.StatusForbidden {
			t.Errorf("%s log GET: status=%d, want 403; body=%.200s", name, status, body)
		}
	}
	if got := server.provider.calls.Load(); got != calls {
		t.Fatalf("denied log requests reached Coder: calls %d -> %d", calls, got)
	}
	if status, body := server.do(t, &frontProxy, http.MethodGet, testLogPath, remoteUser("ws-log-reader"), ""); status != http.StatusOK || body != integrationMockRenderedLog() {
		t.Fatalf("log-reader log GET: status=%d body=%q", status, body)
	}

	// Positive control: the same viewer is allowed once granted the log Role. The earlier deny is
	// cached for up to 10 s, so poll past it.
	grantRole(t, "test-ns", userSubject("ws-viewer"), "ws-log-test-viewer-logs", logRule)
	for deadline := time.Now().Add(30 * time.Second); ; {
		status, _ := server.do(t, &frontProxy, http.MethodGet, testLogPath, remoteUser("ws-viewer"), "")
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("viewer log GET after the grant: status=%d, want 200", status)
		}
		time.Sleep(time.Second)
	}
}
