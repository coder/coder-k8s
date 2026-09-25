package apiserverapp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
)

const servingTestNS = "test-ns"

func TestNewRecommendedConfigKeepsManagedServingCert(t *testing.T) {
	manager := ensuredManager(t, fake.NewClientset())
	secureServingOptions, _ := newTestSecureServing(t)
	secureServingOptions.ServerCert.GeneratedCert = manager
	authn, authz := newFakeKubeAPI(t).options(false)
	cfg, err := NewRecommendedConfig(NewScheme(), codecsFor(), secureServingOptions, authn, authz)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecureServing.Cert != manager {
		t.Fatalf("serving certificate provider was replaced: %T", cfg.SecureServing.Cert)
	}
}

// TestManagedServingCertVerifiesAndHotSwaps runs the real server with a managed certificate:
// clients that trust only the Secret's CA connect with the Service name, other CAs are rejected,
// and a changed Secret is served without a restart.
func TestManagedServingCertVerifiesAndHotSwaps(t *testing.T) {
	client := fake.NewClientset()
	manager := ensuredManager(t, client)
	_, listener := newTestSecureServing(t)
	authn, authz := newFakeKubeAPI(t).options(false)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(ctx, Options{Listener: listener, Authentication: authn, Authorization: authz, ServingCert: manager})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("server exited with error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("timed out waiting for server shutdown")
		}
	})
	addr := listener.Addr().String()
	serverName := "coder-k8s-apiserver." + servingTestNS + ".svc"

	firstCA := manager.CABundle()
	waitForHealthz(t, addr, firstCA, serverName)

	if _, err := getHealthz(addr, firstCA, "coder-k8s-apiserver.other-ns.svc"); err == nil || !strings.Contains(err.Error(), "certificate is valid for") {
		t.Fatalf("expected a hostname mismatch for another namespace, got %v", err)
	}
	otherCA, err := servingcert.Generate(servingTestNS, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := getHealthz(addr, otherCA.CACertPEM, serverName); err == nil || !strings.Contains(err.Error(), "certificate signed by unknown authority") {
		t.Fatalf("expected unknown authority with a wrong CA, got %v", err)
	}

	// Replace the Secret (for example after a CA rotation) and let the manager adopt it.
	secret, err := client.CoreV1().Secrets(servingTestNS).Get(ctx, servingcert.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret.Data = otherCA.Data()
	if _, err := client.CoreV1().Secrets(servingTestNS).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := getHealthz(addr, otherCA.CACertPEM, serverName)
		if err == nil && status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new certificate not served without restart: status=%d err=%v", status, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := getHealthz(addr, firstCA, serverName); err == nil {
		t.Fatal("the old CA must no longer verify after the swap")
	}
}

func TestRunWithOptionsFailsOnCorruptServingCertSecret(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: servingcert.SecretName, Namespace: servingTestNS},
		Type:       servingcert.SecretType,
		Data:       map[string][]byte{"ca.crt": []byte("garbage")},
	})
	manager, err := servingcert.NewManager(client, servingTestNS)
	if err != nil {
		t.Fatal(err)
	}
	_, listener := newTestSecureServing(t)
	authn, authz := newFakeKubeAPI(t).options(false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = RunWithOptions(ctx, Options{Listener: listener, Authentication: authn, Authorization: authz, ServingCert: manager})
	if err == nil || !strings.Contains(err.Error(), "serving certificate") || !strings.Contains(err.Error(), `data["ca.key"] is missing`) {
		t.Fatalf("expected a diagnosable startup failure, got %v", err)
	}
}

func TestNewServingCertManager(t *testing.T) {
	dir := t.TempDir()
	if m, err := newServingCertManager("", filepath.Join(dir, "absent")); err != nil || m != nil {
		t.Fatalf("outside a Pod: manager=%v err=%v, want nil, nil", m, err)
	}
	empty := filepath.Join(dir, "empty")
	mustWrite(t, empty, " \n")
	if _, err := newServingCertManager("", empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("expected empty namespace file error, got %v", err)
	}
	nsFile := filepath.Join(dir, "namespace")
	mustWrite(t, nsFile, servingTestNS+"\n")
	if _, err := newServingCertManager(filepath.Join(dir, "no-kubeconfig"), nsFile); err == nil || !strings.Contains(err.Error(), "client config") {
		t.Fatalf("expected client config error, got %v", err)
	}
	m, err := newServingCertManager(newFakeKubeAPI(t).kubeconfigPath, nsFile)
	if err != nil || m == nil {
		t.Fatalf("in a Pod: manager=%v err=%v", m, err)
	}
	if !strings.Contains(m.Name(), servingTestNS+"/"+servingcert.SecretName) {
		t.Fatalf("manager must target %s/%s, got %s", servingTestNS, servingcert.SecretName, m.Name())
	}
	if err := os.Chmod(nsFile, 0o000); err == nil && os.Geteuid() != 0 {
		if _, err := newServingCertManager("", nsFile); err == nil || !strings.Contains(err.Error(), "read pod namespace") {
			t.Fatalf("expected read error, got %v", err)
		}
	}
}

func ensuredManager(t *testing.T, client *fake.Clientset) *servingcert.Manager {
	t.Helper()
	m, err := servingcert.NewManager(client, servingTestNS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	return m
}

func getHealthz(addr string, caPEM []byte, serverName string) (int, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return 0, fmt.Errorf("assertion failed: CA PEM did not parse")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}}
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func waitForHealthz(t *testing.T, addr string, caPEM []byte, serverName string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, err := getHealthz(addr, caPEM, serverName)
		if err == nil && status == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become healthy with verified TLS: status=%d err=%v", status, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
