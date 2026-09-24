package apiserverapp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
)

const (
	aggGroup          = "aggregation.coder.com"
	templatesTestNS   = "/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/codertemplates"
	workspacesTestNS  = "/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/coderworkspaces"
	templatesOtherNS  = "/apis/aggregation.coder.com/v1alpha1/namespaces/other-ns/codertemplates"
	templatesAllNS    = "/apis/aggregation.coder.com/v1alpha1/codertemplates"
	frontProxyName    = "front-proxy-client"
	testTemplateName  = "default.my-template"
	testWorkspaceName = "default.testuser.my-workspace"
)

// authFixture wires the production server to a fake Kubernetes API plus explicit CAs:
// frontProxyCA signs front-proxy (request-header) client certs, clusterCA signs ordinary
// Kubernetes client certs.
type authFixture struct {
	kube         *fakeKubeAPI
	frontProxyCA *testCA
	clusterCA    *testCA
	server       authTestServer
}

func newAuthFixture(t *testing.T, tune func(*fakeKubeAPI)) *authFixture {
	t.Helper()
	f := &authFixture{
		kube:         newFakeKubeAPI(t),
		frontProxyCA: newTestCA(t, "front-proxy"),
		clusterCA:    newTestCA(t, "cluster"),
	}
	if tune != nil {
		tune(f.kube)
	}
	authn, authz := f.kube.options(false)
	authn.RequestHeader.ClientCAFile = f.frontProxyCA.pemPath
	authn.RequestHeader.AllowedNames = []string{frontProxyName}
	authn.ClientCert.ClientCA = f.clusterCA.pemPath
	f.server = startAuthTestServer(t, authn, authz)
	return f
}

func (f *authFixture) frontProxyCert(t *testing.T) *tls.Certificate {
	cert := f.frontProxyCA.clientCert(t, frontProxyName)
	return &cert
}

func remoteUser(name string, groups ...string) map[string]string {
	h := map[string]string{"X-Remote-User": name}
	if len(groups) > 0 {
		h["X-Remote-Group"] = strings.Join(groups, ",")
	}
	return h
}

func allowAll(authorizationv1.SubjectAccessReviewSpec) bool { return true }

// TestDelegatedAuthRejectsUnauthenticatedRequests: SubjectAccessReview allows everything here, so
// every 401 below comes from authentication, not from RBAC.
func TestDelegatedAuthRejectsUnauthenticatedRequests(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setDecide(allowAll) })
	rogueCA := newTestCA(t, "rogue")
	rogueFrontProxy := rogueCA.clientCert(t, frontProxyName)
	wrongName := f.frontProxyCA.clientCert(t, "not-the-front-proxy")
	forged := remoteUser("kubernetes-admin", "system:masters")

	credentials := []struct {
		name    string
		cert    *tls.Certificate
		headers map[string]string
	}{
		{name: "no credentials"},
		{name: "forged remote user and masters group", headers: forged},
		{name: "forged masters group only", headers: map[string]string{"X-Remote-Group": "system:masters"}},
		{name: "unknown CA front-proxy cert with forged headers", cert: &rogueFrontProxy, headers: forged},
		{name: "front-proxy CA cert with disallowed CN", cert: &wrongName, headers: forged},
		{name: "unknown bearer token", headers: bearer("not-a-real-token")},
		{name: "unknown bearer token with forged headers", headers: mergeHeaders(bearer("not-a-real-token"), forged)},
	}
	paths := []string{
		templatesTestNS, workspacesTestNS, templatesAllNS, templatesTestNS + "/" + testTemplateName,
		templatesTestNS + "?watch=true", "/apis", "/apis/aggregation.coder.com/v1alpha1", "/version",
		"/openapi/v2", "/healthz/ping", "/readyz/informer-sync", "/livezX", "/metrics", "/",
	}
	for _, c := range credentials {
		for _, path := range paths {
			status, body := f.server.do(t, c.cert, http.MethodGet, path, c.headers, "")
			if status != http.StatusUnauthorized {
				t.Errorf("%s GET %s: status=%d, want 401; body=%.200s", c.name, path, status, body)
			}
		}
		status, _ := f.server.do(t, c.cert, http.MethodDelete, templatesTestNS+"/"+testTemplateName, c.headers, "")
		if status != http.StatusUnauthorized {
			t.Errorf("%s DELETE: status=%d, want 401", c.name, status)
		}
	}
	if calls := f.server.provider.calls.Load(); calls != 0 {
		t.Fatalf("unauthenticated requests reached the Coder backend %d times", calls)
	}
	if sars := f.kube.recordedSARs(); len(sars) != 0 {
		t.Fatalf("unauthenticated requests must be rejected before authorization, got SARs: %v", sars)
	}
}

func TestDelegatedAuthAnonymousHealthPathsOnly(t *testing.T) {
	f := newAuthFixture(t, nil)
	for _, path := range []string{"/healthz", "/livez", "/readyz"} {
		if status, body := f.server.do(t, nil, http.MethodGet, path, nil, ""); status != http.StatusOK {
			t.Errorf("anonymous GET %s: status=%d body=%s", path, status, body)
		}
	}
}

// TestDelegatedAuthFrontProxyIdentityAndSARAttributes checks that a request relayed by the
// front proxy is authorized for the relayed user with the exact verb/resource/namespace, and that
// every denial happens before the Coder backend is called.
func TestDelegatedAuthFrontProxyIdentityAndSARAttributes(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) {
		k.setDecide(func(spec authorizationv1.SubjectAccessReviewSpec) bool {
			ra := spec.ResourceAttributes
			return spec.User == "alice" && ra != nil && ra.Verb == "list" && ra.Group == aggGroup &&
				ra.Resource == "codertemplates" && ra.Namespace == "test-ns"
		})
	})
	cert := f.frontProxyCert(t)
	alice := mergeHeaders(remoteUser("alice", "team-a"), map[string]string{"X-Remote-Extra-Scopes": "read"})

	status, body := f.server.do(t, cert, http.MethodGet, templatesTestNS, alice, "")
	if status != http.StatusOK || !strings.Contains(body, testTemplateName) {
		t.Fatalf("allowed list: status=%d body=%.300s", status, body)
	}
	sars := f.kube.recordedSARs()
	if len(sars) != 1 {
		t.Fatalf("expected one SAR, got %d: %v", len(sars), sars)
	}
	spec := sars[0]
	if spec.User != "alice" || spec.ResourceAttributes == nil {
		t.Fatalf("unexpected SAR: %s", describeSAR(spec))
	}
	mustContain(t, spec.Groups, "team-a")
	mustContain(t, spec.Groups, user.AllAuthenticated)
	if got := spec.Extra["scopes"]; len(got) != 1 || got[0] != "read" {
		t.Fatalf("expected extra scopes=[read], got %v", spec.Extra)
	}
	if ra := spec.ResourceAttributes; ra.Version != "v1alpha1" || ra.Name != "" {
		t.Fatalf("unexpected resource attributes: %+v", ra)
	}
	callsAfterAllowed := f.server.provider.calls.Load()
	if callsAfterAllowed == 0 {
		t.Fatal("allowed list must reach the backend")
	}

	denied := []struct {
		method, path, body, contentType string
		verb, resource, namespace, name string
	}{
		{method: http.MethodGet, path: templatesOtherNS, verb: "list", resource: "codertemplates", namespace: "other-ns"},
		{method: http.MethodGet, path: templatesAllNS, verb: "list", resource: "codertemplates"},
		{method: http.MethodGet, path: templatesTestNS + "/" + testTemplateName, verb: "get", resource: "codertemplates", namespace: "test-ns", name: testTemplateName},
		{method: http.MethodGet, path: templatesTestNS + "?watch=true", verb: "watch", resource: "codertemplates", namespace: "test-ns"},
		{method: http.MethodPost, path: templatesTestNS, body: `{"apiVersion":"aggregation.coder.com/v1alpha1","kind":"CoderTemplate","metadata":{"name":"default.x"}}`, verb: "create", resource: "codertemplates", namespace: "test-ns"},
		{method: http.MethodPut, path: templatesTestNS + "/" + testTemplateName, body: `{"apiVersion":"aggregation.coder.com/v1alpha1","kind":"CoderTemplate","metadata":{"name":"default.my-template"}}`, verb: "update", resource: "codertemplates", namespace: "test-ns", name: testTemplateName},
		{method: http.MethodPatch, path: templatesTestNS + "/" + testTemplateName, body: `{"spec":{"running":false}}`, contentType: "application/merge-patch+json", verb: "patch", resource: "codertemplates", namespace: "test-ns", name: testTemplateName},
		{method: http.MethodDelete, path: templatesTestNS + "/" + testTemplateName, verb: "delete", resource: "codertemplates", namespace: "test-ns", name: testTemplateName},
		{method: http.MethodGet, path: workspacesTestNS, verb: "list", resource: "coderworkspaces", namespace: "test-ns"},
		{method: http.MethodGet, path: workspacesTestNS + "?watch=true", verb: "watch", resource: "coderworkspaces", namespace: "test-ns"},
		{method: http.MethodPost, path: workspacesTestNS, body: `{"apiVersion":"aggregation.coder.com/v1alpha1","kind":"CoderWorkspace","metadata":{"name":"default.testuser.x"}}`, verb: "create", resource: "coderworkspaces", namespace: "test-ns"},
		{method: http.MethodPatch, path: workspacesTestNS + "/" + testWorkspaceName, body: `{"spec":{"running":true}}`, contentType: "application/merge-patch+json", verb: "patch", resource: "coderworkspaces", namespace: "test-ns", name: testWorkspaceName},
		{method: http.MethodDelete, path: workspacesTestNS + "/" + testWorkspaceName, verb: "delete", resource: "coderworkspaces", namespace: "test-ns", name: testWorkspaceName},
	}
	for _, d := range denied {
		f.kube.resetSARs()
		headers := alice
		if d.contentType != "" {
			headers = mergeHeaders(alice, map[string]string{"Content-Type": d.contentType})
		}
		status, body := f.server.do(t, cert, d.method, d.path, headers, d.body)
		if status != http.StatusForbidden {
			t.Errorf("%s %s: status=%d, want 403; body=%.200s", d.method, d.path, status, body)
			continue
		}
		sars := f.kube.recordedSARs()
		if len(sars) != 1 || sars[0].ResourceAttributes == nil {
			t.Errorf("%s %s: expected one resource SAR, got %v", d.method, d.path, sars)
			continue
		}
		ra := sars[0].ResourceAttributes
		if sars[0].User != "alice" || ra.Verb != d.verb || ra.Group != aggGroup || ra.Resource != d.resource || ra.Namespace != d.namespace || ra.Name != d.name {
			t.Errorf("%s %s: SAR %s, want verb=%s resource=%s ns=%q name=%q", d.method, d.path, describeSAR(sars[0]), d.verb, d.resource, d.namespace, d.name)
		}
	}
	if calls := f.server.provider.calls.Load(); calls != callsAfterAllowed {
		t.Fatalf("denied requests reached the Coder backend: calls %d -> %d", callsAfterAllowed, calls)
	}
}

// TestDelegatedAuthHeadersCannotOverrideCredentialIdentity covers forged privileged-group headers
// sent alongside a real (unprivileged) credential.
func TestDelegatedAuthHeadersCannotOverrideCredentialIdentity(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) {
		k.setToken("sa-token", authenticationv1.UserInfo{Username: "system:serviceaccount:test-ns:reader", Groups: []string{"system:serviceaccounts"}})
		k.setDecide(func(spec authorizationv1.SubjectAccessReviewSpec) bool {
			ra := spec.ResourceAttributes
			return spec.User == "system:serviceaccount:test-ns:reader" && ra != nil && ra.Verb == "list" && ra.Namespace == "test-ns"
		})
	})
	forged := remoteUser("kubernetes-admin", "system:masters")

	// Valid, unprivileged bearer token alone: allowed for its own RBAC.
	if status, body := f.server.do(t, nil, http.MethodGet, templatesTestNS, bearer("sa-token"), ""); status != http.StatusOK {
		t.Fatalf("bearer list: status=%d body=%.200s", status, body)
	}
	callsAfterAllowed := f.server.provider.calls.Load()

	f.kube.resetSARs()
	status, _ := f.server.do(t, nil, http.MethodDelete, templatesTestNS+"/"+testTemplateName, mergeHeaders(bearer("sa-token"), forged), "")
	if status != http.StatusForbidden {
		t.Fatalf("bearer + forged masters delete: status=%d, want 403", status)
	}
	sars := f.kube.recordedSARs()
	if len(sars) != 1 || sars[0].User != "system:serviceaccount:test-ns:reader" {
		t.Fatalf("expected SAR for the token user, got %v", sars)
	}
	mustNotContain(t, sars[0].Groups, "system:masters")

	// Ordinary cluster client cert plus forged headers: identity is the certificate subject.
	bob := f.clusterCA.clientCert(t, "bob", "team-b")
	f.kube.resetSARs()
	status, _ = f.server.do(t, &bob, http.MethodGet, templatesTestNS, forged, "")
	if status != http.StatusForbidden {
		t.Fatalf("client cert + forged masters: status=%d, want 403", status)
	}
	sars = f.kube.recordedSARs()
	if len(sars) != 1 || sars[0].User != "bob" {
		t.Fatalf("expected SAR for cert user bob, got %v", sars)
	}
	mustContain(t, sars[0].Groups, "team-b")
	mustNotContain(t, sars[0].Groups, "system:masters")

	if calls := f.server.provider.calls.Load(); calls != callsAfterAllowed {
		t.Fatalf("denied requests reached the Coder backend: calls %d -> %d", callsAfterAllowed, calls)
	}
}

// TestDelegatedAuthPrivilegedGroupBypassIsVendoredBehavior documents the retained vendored
// default: an authenticated system:masters member is authorized locally without SAR.
func TestDelegatedAuthPrivilegedGroupBypassIsVendoredBehavior(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) { k.setFailures(false, true) })
	status, body := f.server.do(t, f.frontProxyCert(t), http.MethodGet, templatesTestNS, remoteUser("admin", "system:masters"), "")
	if status != http.StatusOK {
		t.Fatalf("front-proxied system:masters list during SAR outage: status=%d body=%.200s", status, body)
	}
	if sars := f.kube.recordedSARs(); len(sars) != 0 {
		t.Fatalf("system:masters must not need SAR, got %v", sars)
	}
}

func TestDelegatedAuthFailsClosedWhenDelegationUnavailable(t *testing.T) {
	f := newAuthFixture(t, func(k *fakeKubeAPI) {
		k.setToken("fresh-token", authenticationv1.UserInfo{Username: "fresh"})
		k.setDecide(allowAll)
		k.setFailures(true, true)
	})
	cert := f.frontProxyCert(t)

	status, body := f.server.do(t, cert, http.MethodGet, templatesTestNS, remoteUser("carol", "team-c"), "")
	if status < 400 || strings.Contains(body, testTemplateName) {
		t.Fatalf("SAR outage (cold cache): status=%d body=%.200s", status, body)
	}
	if status, _ := f.server.do(t, nil, http.MethodGet, templatesTestNS, bearer("fresh-token"), ""); status != http.StatusUnauthorized {
		t.Fatalf("TokenReview outage: status=%d, want 401", status)
	}

	// Kubernetes API completely unreachable.
	f.kube.server.Close()
	status, body = f.server.do(t, cert, http.MethodDelete, templatesTestNS+"/"+testTemplateName, remoteUser("dave"), "")
	if status < 400 {
		t.Fatalf("Kubernetes API unreachable: status=%d body=%.200s", status, body)
	}
	if calls := f.server.provider.calls.Load(); calls != 0 {
		t.Fatalf("requests during a delegation outage reached the Coder backend %d times", calls)
	}
}

// TestDelegatedAuthCacheTTLs documents that each delegated decision is cached for its own TTL and
// that an outage denies once the cached entry expires.
func TestDelegatedAuthCacheTTLs(t *testing.T) {
	authn, authz := newDelegatedAuthOptions("unused")
	if authn.CacheTTL != 10*time.Second || authz.AllowCacheTTL != 10*time.Second || authz.DenyCacheTTL != 10*time.Second {
		t.Fatalf("documented default TTLs changed: token=%s allow=%s deny=%s", authn.CacheTTL, authz.AllowCacheTTL, authz.DenyCacheTTL)
	}

	const ttl = 300 * time.Millisecond
	kube := newFakeKubeAPI(t)
	kube.setDecide(allowAll)
	kube.setToken("cached-token", authenticationv1.UserInfo{Username: "erin"})
	authn, authz = kube.options(false)
	authn.CacheTTL, authz.AllowCacheTTL, authz.DenyCacheTTL = ttl, ttl, ttl
	server := startAuthTestServer(t, authn, authz)

	if status, _ := server.do(t, nil, http.MethodGet, templatesTestNS, bearer("cached-token"), ""); status != http.StatusOK {
		t.Fatalf("warm-up: status=%d", status)
	}
	kube.setFailures(true, true)
	if status, _ := server.do(t, nil, http.MethodGet, templatesTestNS, bearer("cached-token"), ""); status != http.StatusOK {
		t.Fatalf("within TTL the cached decisions apply: status=%d", status)
	}
	time.Sleep(2 * ttl)
	if status, _ := server.do(t, nil, http.MethodGet, templatesTestNS, bearer("cached-token"), ""); status != http.StatusUnauthorized {
		t.Fatalf("after TTL expiry during an outage: status=%d, want 401", status)
	}
}

func TestDelegatedAuthConfigMapTrustMaterial(t *testing.T) {
	frontProxyCA := newTestCA(t, "front-proxy")
	validData := func() map[string]string {
		names, _ := json.Marshal([]string{frontProxyName})
		users, _ := json.Marshal([]string{"X-Remote-User"})
		groups, _ := json.Marshal([]string{"X-Remote-Group"})
		extra, _ := json.Marshal([]string{"X-Remote-Extra-"})
		return map[string]string{
			"requestheader-client-ca-file":       string(frontProxyCA.pem()),
			"requestheader-allowed-names":        string(names),
			"requestheader-username-headers":     string(users),
			"requestheader-group-headers":        string(groups),
			"requestheader-extra-headers-prefix": string(extra),
		}
	}
	tests := []struct {
		name   string
		data   func() map[string]string // nil: ConfigMap absent
		wantOK bool
	}{
		{name: "valid", data: validData, wantOK: true},
		{name: "absent"},
		{name: "CA key missing", data: func() map[string]string { d := validData(); delete(d, "requestheader-client-ca-file"); return d }},
		{name: "CA empty", data: func() map[string]string { d := validData(); d["requestheader-client-ca-file"] = ""; return d }},
		{name: "CA malformed", data: func() map[string]string {
			d := validData()
			d["requestheader-client-ca-file"] = "not a certificate"
			return d
		}},
		{name: "username headers missing", data: func() map[string]string { d := validData(); delete(d, "requestheader-username-headers"); return d }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kube := newFakeKubeAPI(t)
			kube.setDecide(allowAll)
			if tt.data != nil {
				kube.configMap = &corev1.ConfigMap{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
					ObjectMeta: metav1.ObjectMeta{Name: "extension-apiserver-authentication", Namespace: "kube-system", ResourceVersion: "1"},
					Data:       tt.data(),
				}
			}
			authn, authz := kube.options(true)
			server := startAuthTestServer(t, authn, authz)
			cert := frontProxyCA.clientCert(t, frontProxyName)

			// The CA controller loads asynchronously after startup; poll for the positive case.
			deadline := time.Now().Add(10 * time.Second)
			var status int
			for {
				status, _ = server.do(t, &cert, http.MethodGet, templatesTestNS, remoteUser("alice"), "")
				if status == http.StatusOK || !tt.wantOK || time.Now().After(deadline) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if tt.wantOK && status != http.StatusOK {
				t.Fatalf("valid ConfigMap: front-proxy status=%d, want 200", status)
			}
			if !tt.wantOK {
				// Give the async loader the same chance it had in the positive case.
				time.Sleep(time.Second)
				if status, _ = server.do(t, &cert, http.MethodGet, templatesTestNS, remoteUser("alice"), ""); status != http.StatusUnauthorized {
					t.Fatalf("front-proxy status=%d, want 401", status)
				}
				if calls := server.provider.calls.Load(); calls != 0 {
					t.Fatalf("rejected requests reached the Coder backend %d times", calls)
				}
			}
		})
	}
}

func TestDelegatedAuthConfigMapForbiddenFailsStartup(t *testing.T) {
	kube := newFakeKubeAPI(t)
	kube.configMapForbidden = true
	authn, authz := kube.options(true)
	secureServingOptions, _ := newTestSecureServing(t)
	_, err := NewRecommendedConfig(NewScheme(), codecsFor(), secureServingOptions, authn, authz)
	if err == nil || !strings.Contains(err.Error(), "configure delegated authentication") {
		t.Fatalf("expected startup failure on forbidden ConfigMap, got %v", err)
	}
}

func TestDelegatedAuthUnreachableKubernetesAPIFailsStartup(t *testing.T) {
	kube := newFakeKubeAPI(t)
	kube.server.Close()
	authn, authz := kube.options(true)
	secureServingOptions, _ := newTestSecureServing(t)
	_, err := NewRecommendedConfig(NewScheme(), codecsFor(), secureServingOptions, authn, authz)
	if err == nil {
		t.Fatal("expected startup failure when the Kubernetes API is unreachable")
	}
}

func TestNewRecommendedConfigRejectsMissingOrMismatchedAuthOptions(t *testing.T) {
	kube := newFakeKubeAPI(t)
	authn, authz := kube.options(false)
	secureServingOptions, _ := newTestSecureServing(t)
	if _, err := NewRecommendedConfig(NewScheme(), codecsFor(), secureServingOptions, nil, authz); err == nil || !strings.Contains(err.Error(), "must not be nil") {
		t.Fatalf("expected nil authentication assertion, got %v", err)
	}
	if _, err := NewRecommendedConfig(NewScheme(), codecsFor(), secureServingOptions, authn, nil); err == nil || !strings.Contains(err.Error(), "must not be nil") {
		t.Fatalf("expected nil authorization assertion, got %v", err)
	}
	authz.RemoteKubeConfigFile = filepath.Join(t.TempDir(), "other")
	if _, err := NewRecommendedConfig(NewScheme(), codecsFor(), secureServingOptions, authn, authz); err == nil || !strings.Contains(err.Error(), "same Kubernetes API authority") {
		t.Fatalf("expected authority mismatch assertion, got %v", err)
	}
}

type stubAuthenticator struct {
	resp *authenticator.Response
	ok   bool
	err  error
}

func (s stubAuthenticator) AuthenticateRequest(*http.Request) (*authenticator.Response, bool, error) {
	return s.resp, s.ok, s.err
}

func TestAnonymousHealthOnly(t *testing.T) {
	anonymous := &authenticator.Response{User: &user.DefaultInfo{Name: user.Anonymous, Groups: []string{user.AllUnauthenticated}}}
	alice := &authenticator.Response{User: &user.DefaultInfo{Name: "alice"}}
	boom := errors.New("verify failed")
	tests := []struct {
		name     string
		delegate stubAuthenticator
		path     string
		wantOK   bool
		wantErr  bool
	}{
		{name: "error passes through", delegate: stubAuthenticator{err: boom}, path: "/healthz", wantErr: true},
		{name: "not authenticated passes through", delegate: stubAuthenticator{}, path: "/healthz"},
		{name: "user on resource path", delegate: stubAuthenticator{resp: alice, ok: true}, path: templatesTestNS, wantOK: true},
		{name: "anonymous healthz", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: "/healthz", wantOK: true},
		{name: "anonymous livez", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: "/livez", wantOK: true},
		{name: "anonymous readyz", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: "/readyz", wantOK: true},
		{name: "anonymous healthz subpath", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: "/healthz/ping"},
		{name: "anonymous readyz suffix", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: "/readyzX"},
		{name: "anonymous traversal", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: "/healthz/../apis"},
		{name: "anonymous resource", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: templatesTestNS},
		{name: "anonymous version", delegate: stubAuthenticator{resp: anonymous, ok: true}, path: "/version"},
		{name: "authenticated without user", delegate: stubAuthenticator{resp: &authenticator.Response{}, ok: true}, path: "/healthz", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.URL.Path = tt.path
			resp, ok, err := anonymousHealthOnly{delegate: tt.delegate}.AuthenticateRequest(req)
			if (err != nil) != tt.wantErr || ok != tt.wantOK {
				t.Fatalf("ok=%v err=%v, want ok=%v wantErr=%v", ok, err, tt.wantOK, tt.wantErr)
			}
			if ok && resp == nil {
				t.Fatal("authenticated result must carry a response")
			}
		})
	}
	if _, _, err := (anonymousHealthOnly{}).AuthenticateRequest(httptestRequest(t)); err == nil {
		t.Fatal("expected assertion for nil delegate")
	}
}

func TestResolveDelegationKubeconfig(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid")
	kube := newFakeKubeAPI(t)
	data, err := os.ReadFile(kube.kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, valid, string(data))
	empty := filepath.Join(dir, "empty")
	mustWrite(t, empty, "apiVersion: v1\nkind: Config\n")
	garbage := filepath.Join(dir, "garbage")
	mustWrite(t, garbage, "{not yaml")

	homeValid := t.TempDir()
	mustWrite(t, filepath.Join(homeValid, ".kube", "config"), string(data))
	homeInvalid := t.TempDir()
	mustWrite(t, filepath.Join(homeInvalid, ".kube", "config"), "apiVersion: v1\nkind: Config\n")

	tests := []struct {
		name      string
		env       map[string]string
		home      string
		want      string
		wantError string
	}{
		{name: "explicit valid", env: map[string]string{"KUBECONFIG": valid}, want: valid},
		{name: "explicit wins over in-cluster", env: map[string]string{"KUBECONFIG": valid, "KUBERNETES_SERVICE_HOST": "10.0.0.1"}, want: valid},
		{name: "explicit missing never falls back", env: map[string]string{"KUBECONFIG": filepath.Join(dir, "absent"), "KUBERNETES_SERVICE_HOST": "10.0.0.1"}, home: homeValid, wantError: "load kubeconfig"},
		{name: "explicit empty never falls back", env: map[string]string{"KUBECONFIG": empty, "KUBERNETES_SERVICE_HOST": "10.0.0.1"}, home: homeValid, wantError: "invalid kubeconfig"},
		{name: "explicit garbage", env: map[string]string{"KUBECONFIG": garbage}, wantError: "load kubeconfig"},
		{name: "explicit list", env: map[string]string{"KUBECONFIG": valid + string(os.PathListSeparator) + valid}, wantError: "exactly one file"},
		{name: "in cluster", env: map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}, home: homeValid, want: ""},
		{name: "home config", home: homeValid, want: filepath.Join(homeValid, ".kube", "config")},
		{name: "invalid home config", home: homeInvalid, wantError: "invalid kubeconfig"},
		{name: "nothing configured", home: t.TempDir(), wantError: "no Kubernetes configuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDelegationKubeconfig(func(key string) string { return tt.env[key] }, tt.home)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got path=%q err=%v", tt.wantError, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got path=%q err=%v, want %q", got, err, tt.want)
			}
		})
	}
}

// TestRunWithOptionsFailsClosedWithoutKubernetesConfig exercises the production default path.
func TestRunWithOptionsFailsClosedWithoutKubernetesConfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("HOME", t.TempDir())
	_, listener := newTestSecureServing(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := RunWithOptions(ctx, Options{Listener: listener})
	if err == nil || !strings.Contains(err.Error(), "no Kubernetes configuration") {
		t.Fatalf("expected startup failure without Kubernetes config, got %v", err)
	}

	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	if err := RunWithOptions(ctx, Options{Listener: listener}); err == nil || !strings.Contains(err.Error(), "load kubeconfig") {
		t.Fatalf("expected startup failure with an invalid explicit KUBECONFIG, got %v", err)
	}
}

// TestRunWithOptionsFailsClosedInClusterWithoutServiceAccount: the in-cluster path must not start
// without delegation credentials (guards RemoteKubeConfigFileOptional=false).
func TestRunWithOptionsFailsClosedInClusterWithoutServiceAccount(t *testing.T) {
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		t.Skip("running inside a Pod with a ServiceAccount token")
	}
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	_, listener := newTestSecureServing(t)
	// Bounded: a regression that starts the server anyway must fail the test, not hang it.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := RunWithOptions(ctx, Options{Listener: listener})
	if err == nil || !strings.Contains(err.Error(), "delegated") {
		t.Fatalf("expected startup failure without in-cluster credentials, got %v", err)
	}
	if err := RunWithOptions(ctx, Options{Listener: listener, Authentication: newFakeKubeAPIOptionsOnly(t)}); err == nil || !strings.Contains(err.Error(), "set together") {
		t.Fatalf("expected assertion when only one option set is provided, got %v", err)
	}
}
