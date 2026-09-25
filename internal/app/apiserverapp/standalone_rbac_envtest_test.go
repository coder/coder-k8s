package apiserverapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/coder/coder-k8s/internal/aggregated/apiservicetrust"
	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
)

const (
	standaloneManifests  = "../../../config/apiserver-standalone"
	cabundleRoleManifest = "../../../config/rbac/apiservice-cabundle-role.yaml"
	standaloneNS         = "coder-system"
	standaloneUser       = "system:serviceaccount:coder-system:coder-k8s-apiserver"
)

var manifestGVRs = map[string]schema.GroupVersionResource{
	"ServiceAccount":     {Version: "v1", Resource: "serviceaccounts"},
	"Secret":             {Version: "v1", Resource: "secrets"},
	"Role":               {Group: rbacv1.GroupName, Version: "v1", Resource: "roles"},
	"RoleBinding":        {Group: rbacv1.GroupName, Version: "v1", Resource: "rolebindings"},
	"ClusterRole":        {Group: rbacv1.GroupName, Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding": {Group: rbacv1.GroupName, Version: "v1", Resource: "clusterrolebindings"},
}

// applyShippedManifests creates every object in the files exactly as shipped, so the test checks
// the RBAC users apply rather than a copy.
func applyShippedManifests(t *testing.T, dyn dynamic.Interface, paths ...string) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path) //nolint:gosec // G304: repository manifests.
		if err != nil {
			t.Fatal(err)
		}
		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var obj map[string]any
			if err := decoder.Decode(&obj); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if len(obj) == 0 {
				continue
			}
			u := &unstructured.Unstructured{Object: obj}
			gvr, ok := manifestGVRs[u.GetKind()]
			if !ok {
				t.Fatalf("%s: unexpected kind %q", path, u.GetKind())
			}
			var resource dynamic.ResourceInterface = dyn.Resource(gvr)
			if u.GetNamespace() != "" {
				resource = dyn.Resource(gvr).Namespace(u.GetNamespace())
			}
			mustCreate(t, func() error { _, err := resource.Create(t.Context(), u, metav1.CreateOptions{}); return err })
		}
	}
}

// impersonatingKubeconfig writes a kubeconfig that acts as user through the envtest admin, the
// same file shape the server reads from KUBECONFIG.
func impersonatingKubeconfig(t *testing.T, user string) (string, *rest.Config) {
	t.Helper()
	admin := envtestEnv.Config
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["envtest"] = &clientcmdapi.Cluster{Server: admin.Host, CertificateAuthorityData: admin.CAData}
	cfg.AuthInfos["sa"] = &clientcmdapi.AuthInfo{ClientCertificateData: admin.CertData, ClientKeyData: admin.KeyData, Impersonate: user}
	cfg.Contexts["sa"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "sa"}
	cfg.CurrentContext = "sa"
	path := filepath.Join(t.TempDir(), "sa.kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	return path, restCfg
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestEnvtestStandaloneRunsWithOnlyItsOwnRBAC runs the standalone server's Kubernetes clients as
// the coder-k8s-apiserver ServiceAccount with only config/apiserver-standalone/ (and the APIService
// ClusterRole) bound: no manager-role.
func TestEnvtestStandaloneRunsWithOnlyItsOwnRBAC(t *testing.T) {
	startSharedEnvtest(t)
	ctx := t.Context()
	adminDyn := dynamic.NewForConfigOrDie(envtestEnv.Config)
	secrets := envtestAdmin.CoreV1().Secrets(standaloneNS)

	mustCreate(t, func() error {
		_, err := envtestAdmin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: standaloneNS}}, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}, StringData: map[string]string{"k": "v"}}, metav1.CreateOptions{})
		return err
	})
	apiService := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService",
		"metadata": map[string]any{"name": apiservicetrust.APIServiceName},
		"spec": map[string]any{
			"group": "aggregation.coder.com", "version": "v1alpha1", "insecureSkipTLSVerify": true,
			"groupPriorityMinimum": int64(1000), "versionPriority": int64(15),
			"service": map[string]any{"name": "coder-k8s-apiserver", "namespace": standaloneNS},
		},
	}}
	mustCreate(t, func() error {
		_, err := adminDyn.Resource(apiservicetrust.APIServiceGVR).Create(ctx, apiService, metav1.CreateOptions{})
		return err
	})
	t.Cleanup(func() {
		_ = adminDyn.Resource(apiservicetrust.APIServiceGVR).Delete(t.Context(), apiservicetrust.APIServiceName, metav1.DeleteOptions{})
	})
	files, err := filepath.Glob(filepath.Join(standaloneManifests, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests in %s: %v", standaloneManifests, err)
	}
	applyShippedManifests(t, adminDyn, append(files, cabundleRoleManifest)...)

	kubeconfig, saCfg := impersonatingKubeconfig(t, standaloneUser)
	sa := kubernetes.NewForConfigOrDie(saCfg)
	eventuallyNoError(t, "RBAC to propagate", func() error {
		_, err := sa.CoreV1().Secrets(standaloneNS).Get(ctx, servingcert.SecretName, metav1.GetOptions{})
		return err
	})

	// Nothing beyond the one Secret and what delegation needs; in particular nothing from manager-role.
	for name, call := range map[string]func() error{
		"get another Secret": func() error {
			_, err := sa.CoreV1().Secrets(standaloneNS).Get(ctx, "unrelated", metav1.GetOptions{})
			return err
		},
		"list Secrets":              func() error { _, err := sa.CoreV1().Secrets(standaloneNS).List(ctx, metav1.ListOptions{}); return err },
		"watch Secrets":             func() error { _, err := sa.CoreV1().Secrets(standaloneNS).Watch(ctx, metav1.ListOptions{}); return err },
		"list Secrets cluster-wide": func() error { _, err := sa.CoreV1().Secrets("").List(ctx, metav1.ListOptions{}); return err },
		"get a Secret in another ns": func() error {
			_, err := sa.CoreV1().Secrets("test-ns").Get(ctx, "any", metav1.GetOptions{})
			return err
		},
		"create a Secret": func() error {
			_, err := sa.CoreV1().Secrets(standaloneNS).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "minted"}}, metav1.CreateOptions{})
			return err
		},
		"delete the serving-CA Secret": func() error {
			return sa.CoreV1().Secrets(standaloneNS).Delete(ctx, servingcert.SecretName, metav1.DeleteOptions{})
		},
		"patch the serving-CA Secret": func() error {
			_, err := sa.CoreV1().Secrets(standaloneNS).Patch(ctx, servingcert.SecretName, types.MergePatchType, []byte(`{}`), metav1.PatchOptions{})
			return err
		},
		"list ConfigMaps (manager-role)": func() error {
			_, err := sa.CoreV1().ConfigMaps(standaloneNS).List(ctx, metav1.ListOptions{})
			return err
		},
		"create a Deployment (manager-role)": func() error {
			_, err := sa.AppsV1().Deployments(standaloneNS).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "x"}}, metav1.CreateOptions{})
			return err
		},
	} {
		if err := call(); !apierrors.IsForbidden(err) {
			t.Errorf("%s: want 403 Forbidden, got %v", name, err)
		}
	}

	// Production wiring: fill the shipped placeholder, then keep the APIService caBundle in sync.
	nsFile := filepath.Join(t.TempDir(), "namespace")
	mustWrite(t, nsFile, standaloneNS)
	tlsSetup, err := newManagedTLS(kubeconfig, nsFile)
	if err != nil || tlsSetup == nil {
		t.Fatalf("newManagedTLS: %v", err)
	}
	filled, err := tlsSetup.manager.Ensure(ctx)
	if err != nil {
		t.Fatalf("fill the placeholder as the ServiceAccount: %v", err)
	}
	stored, err := secrets.Get(ctx, servingcert.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Data[servingcert.CACertKey]) != string(filled.CACertPEM) {
		t.Fatal("the filled CA must be stored in the Secret")
	}
	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	tlsSetup.manager.AddListener(tlsSetup.sync)
	go func() { tlsSetup.sync.Run(syncCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	wantBundle := base64.StdEncoding.EncodeToString(filled.CACertPEM)
	eventuallyNoError(t, "caBundle patched by the ServiceAccount", func() error {
		u, err := adminDyn.Resource(apiservicetrust.APIServiceGVR).Get(ctx, apiservicetrust.APIServiceName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if got, _, _ := unstructured.NestedString(u.Object, "spec", "caBundle"); got != wantBundle {
			return errors.New("caBundle not set yet")
		}
		return nil
	})

	// Renewal: SANs for another namespace force a new serving certificate with the same CA.
	copied, err := servingcert.Generate("another-namespace", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stored.Data = copied.Data()
	if _, err := secrets.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	renewed, err := tlsSetup.manager.Ensure(ctx)
	if err != nil {
		t.Fatalf("renew as the ServiceAccount: %v", err)
	}
	if string(renewed.CACertPEM) != string(copied.CACertPEM) || renewed.Cert.DNSNames[0] != servingcert.DNSNames(standaloneNS)[0] {
		t.Fatal("renewal must keep the CA and issue a certificate for the server's namespace")
	}

	// Conflict: another replica fills the placeholder between this server's get and update.
	stored, err = secrets.Get(ctx, servingcert.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored.Data = nil
	if _, err := secrets.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	theirs, err := servingcert.Generate(standaloneNS, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var raced atomic.Bool
	racyCfg := rest.CopyConfig(saCfg)
	racyCfg.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/secrets/"+servingcert.SecretName) && raced.CompareAndSwap(false, true) {
				current, err := secrets.Get(r.Context(), servingcert.SecretName, metav1.GetOptions{})
				if err == nil {
					current.Data = theirs.Data()
					_, err = secrets.Update(r.Context(), current, metav1.UpdateOptions{})
				}
				if err != nil {
					return nil, err
				}
			}
			return next.RoundTrip(r)
		})
	})
	racer, err := servingcert.NewManager(kubernetes.NewForConfigOrDie(racyCfg), standaloneNS)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := racer.Ensure(ctx)
	if err != nil {
		t.Fatalf("fill after a conflict: %v", err)
	}
	if !raced.Load() || string(adopted.CACertPEM) != string(theirs.CACertPEM) {
		t.Fatal("after the real 409 Conflict the other replica's CA must be adopted")
	}

	// Delegated authentication and authorization: startup reads
	// kube-system/extension-apiserver-authentication, and requests create SubjectAccessReviews.
	if _, err := sa.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: "not-a-token"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create TokenReview as the ServiceAccount: %v", err)
	}
	server := startEnvtestAuthServer(t, kubeconfig)
	grantTemplateReader(t, "test-ns", rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "standalone-alice"}, "standalone-alice-reader")
	frontProxy := envtestFrontProxyCA.clientCert(t, frontProxyName)
	eventuallyNoError(t, "an authorized request through the standalone identity", func() error {
		if status, body := server.do(t, &frontProxy, http.MethodGet, templatesTestNS, remoteUser("standalone-alice"), ""); status != http.StatusOK {
			return errors.New(body)
		}
		return nil
	})

	// The Secret's type is immutable, and update cannot recreate a deleted Secret.
	current, err := sa.CoreV1().Secrets(standaloneNS).Get(ctx, servingcert.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Type = corev1.SecretTypeServiceAccountToken
	if _, err := sa.CoreV1().Secrets(standaloneNS).Update(ctx, current, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("changing the type must be rejected with 422 Invalid, got %v", err)
	}
	if err := secrets.Delete(ctx, servingcert.SecretName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	absent := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: servingcert.SecretName, Namespace: standaloneNS}, Type: servingcert.SecretType}
	if _, err := sa.CoreV1().Secrets(standaloneNS).Update(ctx, absent, metav1.UpdateOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("update of the absent Secret must return 404 NotFound, got %v", err)
	}
	if _, err := tlsSetup.manager.Ensure(ctx); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("without the placeholder, startup must fail with a Forbidden create that names the placeholder, got %v", err)
	}
}

func eventuallyNoError(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %v", what, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
