package apiserverapp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// ---- PKI ----

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemPath string
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	ca, err := generateTestCA(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func generateTestCA(dir, name string) (*testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name+"-ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return &testCA{cert: cert, key: key, pemPath: path}, nil
}

func (ca *testCA) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// clientCert issues a client certificate. Kubernetes maps CN to the user name and O to groups.
func (ca *testCA) clientCert(t *testing.T, commonName string, groups ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName, Organization: groups},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// ---- Fake Kubernetes API for TokenReview, SubjectAccessReview and the auth ConfigMap ----

type fakeKubeAPI struct {
	server         *httptest.Server
	kubeconfigPath string

	mu                 sync.Mutex
	tokens             map[string]authenticationv1.UserInfo
	decide             func(authorizationv1.SubjectAccessReviewSpec) bool
	sars               []authorizationv1.SubjectAccessReviewSpec
	tokenReviews       int
	failTokenReview    bool
	failSAR            bool
	configMap          *corev1.ConfigMap
	configMapForbidden bool
}

func newFakeKubeAPI(t *testing.T) *fakeKubeAPI {
	t.Helper()
	f := &fakeKubeAPI{
		tokens: map[string]authenticationv1.UserInfo{},
		decide: func(authorizationv1.SubjectAccessReviewSpec) bool { return false },
	}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["fake"] = &clientcmdapi.Cluster{
		Server:                   f.server.URL,
		CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}),
	}
	cfg.AuthInfos["delegator"] = &clientcmdapi.AuthInfo{Token: "delegator-credential"}
	cfg.Contexts["fake"] = &clientcmdapi.Context{Cluster: "fake", AuthInfo: "delegator"}
	cfg.CurrentContext = "fake"
	f.kubeconfigPath = filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, f.kubeconfigPath); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeKubeAPI) setToken(token string, info authenticationv1.UserInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = info
}

func (f *fakeKubeAPI) setDecide(decide func(authorizationv1.SubjectAccessReviewSpec) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decide = decide
}

func (f *fakeKubeAPI) setFailures(tokenReview, sar bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failTokenReview, f.failSAR = tokenReview, sar
}

func (f *fakeKubeAPI) recordedSARs() []authorizationv1.SubjectAccessReviewSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]authorizationv1.SubjectAccessReviewSpec(nil), f.sars...)
}

func (f *fakeKubeAPI) resetSARs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sars = nil
}

func (f *fakeKubeAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const configMapCollection = "/api/v1/namespaces/kube-system/configmaps"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews":
		var review authenticationv1.TokenReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest")
			return
		}
		f.mu.Lock()
		f.tokenReviews++
		fail := f.failTokenReview
		info, ok := f.tokens[review.Spec.Token]
		f.mu.Unlock()
		if fail {
			writeStatus(w, http.StatusInternalServerError, "InternalError")
			return
		}
		review.Status = authenticationv1.TokenReviewStatus{Authenticated: ok}
		if ok {
			review.Status.User = info
		}
		review.APIVersion, review.Kind = "authentication.k8s.io/v1", "TokenReview"
		writeKubeJSON(w, http.StatusCreated, review)
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authorization.k8s.io/v1/subjectaccessreviews":
		var review authorizationv1.SubjectAccessReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest")
			return
		}
		f.mu.Lock()
		f.sars = append(f.sars, review.Spec)
		fail := f.failSAR
		decide := f.decide
		f.mu.Unlock()
		if fail {
			writeStatus(w, http.StatusInternalServerError, "InternalError")
			return
		}
		allowed := decide(review.Spec)
		review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, Denied: !allowed}
		review.APIVersion, review.Kind = "authorization.k8s.io/v1", "SubjectAccessReview"
		writeKubeJSON(w, http.StatusCreated, review)
	case r.Method == http.MethodGet && r.URL.Path == configMapCollection+"/extension-apiserver-authentication":
		f.mu.Lock()
		cm, forbidden := f.configMap, f.configMapForbidden
		f.mu.Unlock()
		switch {
		case forbidden:
			writeStatus(w, http.StatusForbidden, "Forbidden")
		case cm == nil:
			writeStatus(w, http.StatusNotFound, "NotFound")
		default:
			writeKubeJSON(w, http.StatusOK, cm)
		}
	case r.Method == http.MethodGet && r.URL.Path == configMapCollection:
		if r.URL.Query().Get("watch") == "true" {
			// client-go reflectors use streaming watch-list: send the current object (if any) and the
			// initial-events-end bookmark, then keep the watch open until the client goes away.
			f.mu.Lock()
			cm := f.configMap
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			encoder := json.NewEncoder(w)
			if cm != nil {
				_ = encoder.Encode(map[string]any{"type": "ADDED", "object": cm})
			}
			if r.URL.Query().Get("sendInitialEvents") == "true" {
				_ = encoder.Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"resourceVersion": "1", "annotations": map[string]string{"k8s.io/initial-events-end": "true"}},
				}})
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
			return
		}
		f.mu.Lock()
		cm, forbidden := f.configMap, f.configMapForbidden
		f.mu.Unlock()
		if forbidden {
			writeStatus(w, http.StatusForbidden, "Forbidden")
			return
		}
		list := corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
		if cm != nil {
			list.Items = append(list.Items, *cm)
		}
		writeKubeJSON(w, http.StatusOK, list)
	default:
		writeStatus(w, http.StatusNotFound, "NotFound")
	}
}

func writeKubeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason) {
	writeKubeJSON(w, code, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Code:     int32(code), //nolint:gosec // G115: HTTP status codes fit in int32.
		Reason:   reason,
	})
}

// fastBackoff keeps outage tests fast; production uses the vendored default backoff.
var fastBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 1}

// options returns delegated options pointed at the fake API. The ConfigMap lookup is skipped
// unless useConfigMap is true; CA files can then be supplied explicitly.
func (f *fakeKubeAPI) options(useConfigMap bool) (*genericoptions.DelegatingAuthenticationOptions, *genericoptions.DelegatingAuthorizationOptions) {
	authn, authz := newDelegatedAuthOptions(f.kubeconfigPath)
	authn.SkipInClusterLookup = !useConfigMap
	authn.WithCustomRetryBackoff(fastBackoff)
	authz.WithCustomRetryBackoff(fastBackoff)
	return authn, authz
}

// ---- Backend spy ----

// countingProvider counts every backend lookup so denial tests can assert that no Coder call
// (read or write) happened.
type countingProvider struct {
	inner coder.ClientProvider
	calls atomic.Int32
}

func (p *countingProvider) ClientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error) {
	p.calls.Add(1)
	return p.inner.ClientForNamespace(ctx, namespace)
}

// ---- Server harness ----

type authTestServer struct {
	baseURL  string
	provider *countingProvider
	mock     *integrationMockCoderServer
}

// startAuthTestServer boots the production server configuration (NewRecommendedConfig,
// NewGenericAPIServer, InstallAPIGroup) with the given delegated options against a mock Coder
// backend serving namespace test-ns.
func startAuthTestServer(
	t *testing.T,
	authn *genericoptions.DelegatingAuthenticationOptions,
	authz *genericoptions.DelegatingAuthorizationOptions,
) authTestServer {
	t.Helper()

	mockCoder := newIntegrationMockCoderServer("test-token")
	t.Cleanup(mockCoder.Close)
	mockCoderURL, err := url.Parse(mockCoder.URL())
	if err != nil {
		t.Fatal(err)
	}
	sdkClient := codersdk.New(mockCoderURL)
	sdkClient.SetSessionToken("test-token")
	provider := &countingProvider{inner: &coder.StaticClientProvider{Client: sdkClient, Namespace: "test-ns"}}

	scheme := NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	secureServingOptions := genericoptions.NewSecureServingOptions()
	secureServingOptions.Listener = listener
	secureServingOptions.BindPort = 0
	secureServingOptions.ServerCert.CertDirectory = ""
	secureServingOptions.ServerCert.PairName = ""

	recommendedConfig, err := NewRecommendedConfig(scheme, codecs, secureServingOptions, authn, authz)
	if err != nil {
		t.Fatalf("build recommended config: %v", err)
	}
	server, err := NewGenericAPIServer(recommendedConfig)
	if err != nil {
		t.Fatalf("build generic API server: %v", err)
	}
	t.Cleanup(server.Destroy)
	apiGroupInfo, err := NewAPIGroupInfo(scheme, codecs, provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallAPIGroup(server, apiGroupInfo); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- server.PrepareRun().RunWithContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-errCh:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("aggregated API server exited with error: %v", runErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("timed out waiting for aggregated API server to stop")
		}
	})

	s := authTestServer{
		baseURL:  strings.TrimSuffix(recommendedConfig.LoopbackClientConfig.Host, "/"),
		provider: provider,
		mock:     mockCoder,
	}
	// Anonymous /readyz is one of the allowed health paths, so it doubles as a startup probe.
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, _ := s.do(t, nil, http.MethodGet, "/readyz", nil, "")
		if status == http.StatusOK {
			break
		}
		select {
		case runErr := <-errCh:
			t.Fatalf("server exited during startup: %v", runErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready, last /readyz status %d", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

// do sends one request. cert is an optional TLS client certificate.
func (s authTestServer) do(t *testing.T, cert *tls.Certificate, method, path string, headers map[string]string, body string) (int, string) {
	t.Helper()
	tlsConfig := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Test server uses an ephemeral self-signed cert.
	if cert != nil {
		tlsConfig.Certificates = []tls.Certificate{*cert}
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	defer client.CloseIdleConnections()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func mergeHeaders(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func mustContain(t *testing.T, list []string, want string) {
	t.Helper()
	for _, v := range list {
		if v == want {
			return
		}
	}
	t.Fatalf("expected %q in %v", want, list)
}

func mustNotContain(t *testing.T, list []string, unwanted string) {
	t.Helper()
	for _, v := range list {
		if v == unwanted {
			t.Fatalf("did not expect %q in %v", unwanted, list)
		}
	}
}

func describeSAR(spec authorizationv1.SubjectAccessReviewSpec) string {
	if spec.ResourceAttributes != nil {
		ra := spec.ResourceAttributes
		return fmt.Sprintf("user=%s verb=%s group=%s resource=%s ns=%s name=%s", spec.User, ra.Verb, ra.Group, ra.Resource, ra.Namespace, ra.Name)
	}
	if spec.NonResourceAttributes != nil {
		return fmt.Sprintf("user=%s verb=%s path=%s", spec.User, spec.NonResourceAttributes.Verb, spec.NonResourceAttributes.Path)
	}
	return "user=" + spec.User
}

func newTestSecureServing(t *testing.T) (*genericoptions.SecureServingOptions, net.Listener) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	opts := genericoptions.NewSecureServingOptions()
	opts.Listener = listener
	opts.BindPort = 0
	opts.ServerCert.CertDirectory = ""
	opts.ServerCert.PairName = ""
	return opts, listener
}

func codecsFor() serializer.CodecFactory {
	return serializer.NewCodecFactory(NewScheme())
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func httptestRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "https://example.test/healthz", nil)
}

func newFakeKubeAPIOptionsOnly(t *testing.T) *genericoptions.DelegatingAuthenticationOptions {
	t.Helper()
	authn, _ := newFakeKubeAPI(t).options(false)
	return authn
}
