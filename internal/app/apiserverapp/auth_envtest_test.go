package apiserverapp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// A real kube-apiserver (envtest) configured with front-proxy request-header flags, so it
// publishes the real kube-system/extension-apiserver-authentication ConfigMap and serves real
// TokenReview/SubjectAccessReview with RBAC. The aggregated server under test uses the
// production delegated options; only the kubeconfig path is test-specific.
var (
	envtestOnce         sync.Once
	envtestErr          error
	envtestEnv          *envtest.Environment
	envtestDir          string
	envtestAdmin        kubernetes.Interface
	envtestFrontProxyCA *testCA
)

func TestMain(m *testing.M) {
	code := m.Run()
	if envtestEnv != nil {
		if err := envtestEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}
	if envtestDir != "" {
		_ = os.RemoveAll(envtestDir)
	}
	os.Exit(code)
}

func startSharedEnvtest(t *testing.T) {
	t.Helper()
	envtestOnce.Do(func() {
		if os.Getenv("KUBEBUILDER_ASSETS") == "" {
			envtestErr = fmt.Errorf("KUBEBUILDER_ASSETS is not set; run via `make test`")
			return
		}
		envtestDir, envtestErr = os.MkdirTemp("", "apiserverapp-envtest-")
		if envtestErr != nil {
			return
		}
		envtestFrontProxyCA, envtestErr = generateTestCA(envtestDir, "front-proxy")
		if envtestErr != nil {
			return
		}
		env := &envtest.Environment{}
		args := env.ControlPlane.GetAPIServer().Configure()
		args.Set("requestheader-client-ca-file", envtestFrontProxyCA.pemPath)
		args.Set("requestheader-allowed-names", frontProxyName)
		args.Set("requestheader-username-headers", "X-Remote-User")
		args.Set("requestheader-group-headers", "X-Remote-Group")
		args.Set("requestheader-extra-headers-prefix", "X-Remote-Extra-")
		cfg, err := env.Start()
		if err != nil {
			envtestErr = fmt.Errorf("start envtest: %w", err)
			return
		}
		envtestEnv = env
		envtestAdmin, envtestErr = kubernetes.NewForConfig(cfg)
		if envtestErr != nil {
			return
		}
		ctx := context.Background()
		for _, ns := range []string{"test-ns", "other-ns"} {
			if _, err := envtestAdmin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
				envtestErr = err
				return
			}
		}
		// kube-apiserver publishes the ConfigMap from a post-start hook; wait for the request-header keys.
		deadline := time.Now().Add(30 * time.Second)
		for {
			cm, err := envtestAdmin.CoreV1().ConfigMaps("kube-system").Get(ctx, "extension-apiserver-authentication", metav1.GetOptions{})
			if err == nil && cm.Data["requestheader-client-ca-file"] != "" && cm.Data["client-ca-file"] != "" {
				return
			}
			if time.Now().After(deadline) {
				envtestErr = fmt.Errorf("extension-apiserver-authentication not published: %w", err)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	if envtestErr != nil {
		t.Fatalf("envtest unavailable: %v", envtestErr)
	}
}

// envtestUser creates a certificate-authenticated envtest user, binds the given roles, and
// returns a kubeconfig path plus the user's client certificate.
func envtestUser(t *testing.T, name string, authDelegator, authReader bool) (string, tls.Certificate) {
	t.Helper()
	ctx := t.Context()
	u, err := envtestEnv.AddUser(envtest.User{Name: name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := u.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name+".kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	subject := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}
	if authDelegator {
		mustCreate(t, func() error {
			_, err := envtestAdmin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-auth-delegator"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:auth-delegator"},
				Subjects:   subject,
			}, metav1.CreateOptions{})
			return err
		})
	}
	if authReader {
		mustCreate(t, func() error {
			_, err := envtestAdmin.RbacV1().RoleBindings("kube-system").Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-authentication-reader"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "extension-apiserver-authentication-reader"},
				Subjects:   subject,
			}, metav1.CreateOptions{})
			return err
		})
	}
	cert, err := tls.X509KeyPair(u.Config().CertData, u.Config().KeyData)
	if err != nil {
		t.Fatal(err)
	}
	return path, cert
}

func mustCreate(t *testing.T, create func() error) {
	t.Helper()
	if err := create(); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

func grantTemplateReader(t *testing.T, namespace string, subject rbacv1.Subject, name string) {
	t.Helper()
	ctx := t.Context()
	mustCreate(t, func() error {
		_, err := envtestAdmin.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{aggGroup}, Resources: []string{"codertemplates"}, Verbs: []string{"get", "list"}}},
		}, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := envtestAdmin.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{subject},
		}, metav1.CreateOptions{})
		return err
	})
}

func startEnvtestAuthServer(t *testing.T, kubeconfigPath string) authTestServer {
	t.Helper()
	// Production options: in-cluster ConfigMap lookup, fatal lookup errors, default caches.
	authn, authz := newDelegatedAuthOptions(kubeconfigPath)
	return startAuthTestServer(t, authn, authz)
}

func TestEnvtestDelegatedAuthWithRealRBAC(t *testing.T) {
	startSharedEnvtest(t)
	kubeconfigPath, _ := envtestUser(t, "coder-k8s-delegator", true, true)
	server := startEnvtestAuthServer(t, kubeconfigPath)
	grantTemplateReader(t, "test-ns", rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"}, "alice-template-reader")

	frontProxy := envtestFrontProxyCA.clientCert(t, frontProxyName)
	alice := remoteUser("alice", "team-a")

	// Front-proxy relayed identity, authorized by real RBAC. The request-header CA is loaded
	// asynchronously from the ConfigMap after startup, so poll briefly.
	var status int
	var body string
	for deadline := time.Now().Add(15 * time.Second); ; {
		status, body = server.do(t, &frontProxy, http.MethodGet, templatesTestNS, alice, "")
		if status == http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status != http.StatusOK || !strings.Contains(body, testTemplateName) {
		t.Fatalf("alice list test-ns: status=%d body=%.300s", status, body)
	}
	allowedCalls := server.provider.calls.Load()
	for _, tc := range []struct {
		method, path string
		cert         *tls.Certificate
		headers      map[string]string
		want         int
	}{
		{method: http.MethodGet, path: templatesOtherNS, cert: &frontProxy, headers: alice, want: http.StatusForbidden},
		{method: http.MethodGet, path: workspacesTestNS, cert: &frontProxy, headers: alice, want: http.StatusForbidden},
		{method: http.MethodDelete, path: templatesTestNS + "/" + testTemplateName, cert: &frontProxy, headers: alice, want: http.StatusForbidden},
		{method: http.MethodGet, path: templatesTestNS, cert: &frontProxy, headers: remoteUser("mallory"), want: http.StatusForbidden},
		{method: http.MethodGet, path: templatesTestNS, want: http.StatusUnauthorized},
		{method: http.MethodGet, path: templatesTestNS, headers: alice, want: http.StatusUnauthorized},
		{method: http.MethodGet, path: templatesTestNS, headers: remoteUser("kubernetes-admin", "system:masters"), want: http.StatusUnauthorized},
	} {
		if status, body := server.do(t, tc.cert, tc.method, tc.path, tc.headers, ""); status != tc.want {
			t.Errorf("%s %s headers=%v: status=%d, want %d; body=%.200s", tc.method, tc.path, tc.headers, status, tc.want, body)
		}
	}

	// An ordinary cluster client certificate: identity is the certificate subject, headers ignored.
	_, carolCert := envtestUser(t, "carol", false, false)
	if status, _ := server.do(t, &carolCert, http.MethodGet, templatesTestNS, alice, ""); status != http.StatusForbidden {
		t.Errorf("carol cert + forged alice headers: status=%d, want 403", status)
	}

	// ServiceAccount bearer token via real TokenReview; RBAC granted after the first denial.
	ctx := t.Context()
	mustCreate(t, func() error {
		_, err := envtestAdmin.CoreV1().ServiceAccounts("test-ns").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "reader"}}, metav1.CreateOptions{})
		return err
	})
	tokenRequest, err := envtestAdmin.CoreV1().ServiceAccounts("test-ns").CreateToken(ctx, "reader", &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptrInt64(600)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	saToken := tokenRequest.Status.Token
	if status, _ := server.do(t, nil, http.MethodGet, templatesTestNS, bearer(saToken), ""); status != http.StatusForbidden {
		t.Fatalf("SA token without RBAC: status=%d, want 403", status)
	}
	if status, _ := server.do(t, nil, http.MethodGet, templatesTestNS, mergeHeaders(bearer(saToken), remoteUser("kubernetes-admin", "system:masters")), ""); status != http.StatusForbidden {
		t.Fatalf("SA token + forged masters headers: status=%d, want 403", status)
	}
	if calls := server.provider.calls.Load(); calls != allowedCalls {
		t.Fatalf("denied requests reached the Coder backend: calls %d -> %d", allowedCalls, calls)
	}
	grantTemplateReader(t, "test-ns", rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "reader", Namespace: "test-ns"}, "sa-template-reader")
	// The deny decision is cached for up to DenyCacheTTL (10s); poll past it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, _ := server.do(t, nil, http.MethodGet, templatesTestNS, bearer(saToken), "")
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SA token after RoleBinding: status=%d, want 200", status)
		}
		time.Sleep(time.Second)
	}
}

func TestEnvtestDelegatedAuthFailsClosedWithoutBindings(t *testing.T) {
	startSharedEnvtest(t)

	// Without the authentication-reader binding the ConfigMap lookup is forbidden: no startup.
	noReader, _ := envtestUser(t, "delegator-no-reader", true, false)
	authn, authz := newDelegatedAuthOptions(noReader)
	secureServingOptions, _ := newTestSecureServing(t)
	if _, err := NewRecommendedConfig(NewScheme(), codecsFor(), secureServingOptions, authn, authz); err == nil || !strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		t.Fatalf("expected forbidden ConfigMap startup failure, got %v", err)
	}

	// Without auth-delegator the server starts, but SubjectAccessReview is forbidden: no access.
	noDelegator, _ := envtestUser(t, "delegator-no-auth-delegator", false, true)
	server := startEnvtestAuthServer(t, noDelegator)
	grantTemplateReader(t, "test-ns", rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"}, "alice-template-reader")
	frontProxy := envtestFrontProxyCA.clientCert(t, frontProxyName)
	// Wait until the front-proxy CA is loaded (401 turns into an authorization outcome).
	var status int
	var body string
	for deadline := time.Now().Add(15 * time.Second); ; {
		status, body = server.do(t, &frontProxy, http.MethodGet, templatesTestNS, remoteUser("alice"), "")
		if status != http.StatusUnauthorized || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status == http.StatusUnauthorized {
		t.Fatal("front-proxy CA never loaded; cannot observe the authorization outcome")
	}
	if status < 400 || strings.Contains(body, testTemplateName) {
		t.Fatalf("SAR forbidden for the delegator: status=%d body=%.200s", status, body)
	}
	if calls := server.provider.calls.Load(); calls != 0 {
		t.Fatalf("requests reached the Coder backend %d times without authorization", calls)
	}
}

func ptrInt64(v int64) *int64 { return &v }
