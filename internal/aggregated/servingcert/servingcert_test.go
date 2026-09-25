package servingcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
)

const testNS = "coder-system"

func TestDNSNames(t *testing.T) {
	want := []string{
		"coder-k8s-apiserver",
		"coder-k8s-apiserver.coder-system",
		"coder-k8s-apiserver.coder-system.svc",
		"coder-k8s-apiserver.coder-system.svc.cluster.local",
	}
	if got := DNSNames(testNS); !slices.Equal(got, want) {
		t.Fatalf("DNSNames = %v, want %v", got, want)
	}
}

func TestGenerateProducesVerifiableServingCert(t *testing.T) {
	now := time.Now()
	b, err := Generate(testNS, now)
	if err != nil {
		t.Fatal(err)
	}
	if !b.CACert.IsCA || b.CACert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("CA certificate must be a CA with KeyUsageCertSign")
	}
	if b.Cert.IsCA {
		t.Fatal("serving certificate must not be a CA")
	}
	if !slices.Equal(b.Cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Fatalf("serving ExtKeyUsage = %v, want [ServerAuth]", b.Cert.ExtKeyUsage)
	}
	if !slices.Equal(b.Cert.DNSNames, DNSNames(testNS)) {
		t.Fatalf("serving SANs = %v", b.Cert.DNSNames)
	}
	if got := b.Cert.NotAfter.Sub(now); got < ServingCertValidity-time.Minute || got > ServingCertValidity+time.Minute {
		t.Fatalf("serving certificate validity %s, want about %s", got, ServingCertValidity)
	}
	roots := x509.NewCertPool()
	roots.AddCert(b.CACert)
	for _, name := range DNSNames(testNS) {
		if _, err := b.Cert.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("verify for %s: %v", name, err)
		}
	}
	for _, name := range []string{"coder-k8s-apiserver.other.svc", "localhost", "kubernetes.default.svc"} {
		if _, err := b.Cert.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err == nil {
			t.Errorf("serving certificate must not verify for %s", name)
		}
	}
	if _, err := Parse(secretFor(b), testNS, now); err != nil {
		t.Fatalf("generated bundle does not parse: %v", err)
	}
}

func TestEnsureCreatesMarkedSecretAndServesIt(t *testing.T) {
	client := fake.NewClientset()
	m := newTestManager(t, client, time.Now())
	listener := &countingListener{}
	m.AddListener(listener)

	b, err := m.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secret, err := client.CoreV1().Secrets(testNS).Get(t.Context(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.Type != SecretType {
		t.Fatalf("secret type = %q, want %q", secret.Type, SecretType)
	}
	for k, v := range SecretLabels {
		if secret.Labels[k] != v {
			t.Fatalf("secret label %s = %q, want %q", k, secret.Labels[k], v)
		}
	}
	for _, key := range []string{CACertKey, CAKeyKey, CertKey, KeyKey} {
		if len(secret.Data[key]) == 0 {
			t.Fatalf("secret data[%q] missing", key)
		}
	}
	cert, key := m.CurrentCertKeyContent()
	if string(cert) != string(secret.Data[CertKey]) || string(key) != string(secret.Data[KeyKey]) {
		t.Fatal("served certificate must be the one stored in the Secret")
	}
	if string(m.CABundle()) != string(b.CACertPEM) {
		t.Fatal("CABundle must return the Secret's CA")
	}
	if listener.count.Load() != 1 {
		t.Fatalf("listener notified %d times, want 1", listener.count.Load())
	}
}

func TestEnsureAdoptsValidSecretUnchanged(t *testing.T) {
	now := time.Now()
	existing, err := Generate(testNS, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(secretFor(existing))
	m := newTestManager(t, client, now)
	listener := &countingListener{}
	m.AddListener(listener)
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	cert, _ := m.CurrentCertKeyContent()
	if string(cert) != string(existing.CertPEM) {
		t.Fatal("a valid Secret must be served as is")
	}
	assertNoWrites(t, client)
	// A second Ensure with nothing changed must not notify listeners again.
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if listener.count.Load() != 1 {
		t.Fatalf("listener notified %d times, want 1", listener.count.Load())
	}
}

func TestEnsureAdoptsSecretCreatedByAnotherReplica(t *testing.T) {
	now := time.Now()
	theirs, err := Generate(testNS, now)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset()
	var gets atomic.Int32
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if gets.Add(1) == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, SecretName)
		}
		return true, secretFor(theirs), nil
	})
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, SecretName)
	})
	m := newTestManager(t, client, now)
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if string(m.CABundle()) != string(theirs.CACertPEM) {
		t.Fatal("must adopt the other replica's CA after AlreadyExists")
	}
}

func TestEnsureRenewsServingCertWithSameCA(t *testing.T) {
	issued := time.Now()
	existing, err := Generate(testNS, issued)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(secretFor(existing))
	// Two thirds of the lifetime (measured from the backdated NotBefore) have passed.
	m := newTestManager(t, client, issued.Add(250*24*time.Hour))
	listener := &countingListener{}
	m.AddListener(listener)
	b, err := m.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(b.CACertPEM) != string(existing.CACertPEM) || string(b.CAKeyPEM) != string(existing.CAKeyPEM) {
		t.Fatal("renewal must keep the CA")
	}
	if b.Cert.SerialNumber.Cmp(existing.Cert.SerialNumber) == 0 {
		t.Fatal("renewal must issue a new serving certificate")
	}
	stored, err := client.CoreV1().Secrets(testNS).Get(t.Context(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Data[CertKey]) != string(b.CertPEM) {
		t.Fatal("renewed certificate must be written to the Secret")
	}
	if listener.count.Load() != 1 {
		t.Fatalf("listener notified %d times, want 1", listener.count.Load())
	}
}

func TestEnsureDoesNotRenewEarly(t *testing.T) {
	issued := time.Now()
	existing, err := Generate(testNS, issued)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(secretFor(existing))
	m := newTestManager(t, client, issued.Add(200*24*time.Hour))
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertNoWrites(t, client)
}

func TestEnsureRenewsWhenSANsDoNotMatchNamespace(t *testing.T) {
	now := time.Now()
	copied, err := Generate("another-namespace", now)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(secretFor(copied))
	m := newTestManager(t, client, now)
	b, err := m.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.Cert.DNSNames, DNSNames(testNS)) {
		t.Fatalf("renewed SANs = %v, want %v", b.Cert.DNSNames, DNSNames(testNS))
	}
}

func TestEnsureAdoptsAfterRenewalConflict(t *testing.T) {
	issued := time.Now()
	existing, err := Generate(testNS, issued)
	if err != nil {
		t.Fatal(err)
	}
	now := issued.Add(250 * 24 * time.Hour)
	// The other replica already renewed: its certificate is fresh relative to now.
	theirs := &Bundle{CACertPEM: existing.CACertPEM, CAKeyPEM: existing.CAKeyPEM, CACert: existing.CACert, caKey: existing.caKey}
	if err := theirs.issueServingCert(testNS, now); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(secretFor(existing))
	var updates atomic.Int32
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates.Add(1)
		if err := client.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, secretFor(theirs), testNS); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, SecretName, errors.New("modified"))
	})
	m := newTestManager(t, client, now)
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	cert, _ := m.CurrentCertKeyContent()
	if string(cert) != string(theirs.CertPEM) {
		t.Fatal("after a conflict the other replica's renewal must be adopted")
	}
	if updates.Load() != 1 {
		t.Fatalf("updates = %d, want 1", updates.Load())
	}
}

// Every corruption mode fails with a message that names the field, and the Secret is left alone.
func TestEnsureRejectsCorruptSecret(t *testing.T) {
	now := time.Now()
	good, err := Generate(testNS, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	other, err := Generate(testNS, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	notCAPEM := other.CertPEM // a leaf certificate in the CA slot
	expiredCA, expiredCAKey := selfSignedCA(t, now.Add(-20*24*time.Hour), now.Add(-time.Hour))
	futureCA, futureCAKey := selfSignedCA(t, now.Add(time.Hour), now.Add(48*time.Hour))
	twoCerts := append(append([]byte{}, good.CACertPEM...), other.CACertPEM...)

	tests := []struct {
		name   string
		mutate func(s *corev1.Secret)
		want   string
	}{
		{"wrong type", func(s *corev1.Secret) { s.Type = corev1.SecretTypeTLS }, `type is "kubernetes.io/tls"`},
		{"missing ca.crt", func(s *corev1.Secret) { delete(s.Data, CACertKey) }, `data["ca.crt"] is missing or empty`},
		{"missing ca.key", func(s *corev1.Secret) { delete(s.Data, CAKeyKey) }, `data["ca.key"] is missing or empty`},
		{"missing tls.crt", func(s *corev1.Secret) { delete(s.Data, CertKey) }, `data["tls.crt"] is missing or empty`},
		{"empty tls.key", func(s *corev1.Secret) { s.Data[KeyKey] = nil }, `data["tls.key"] is missing or empty`},
		{"unparsable ca.crt", func(s *corev1.Secret) { s.Data[CACertKey] = []byte("not pem") }, `data["ca.crt"]: unparsable PEM certificate`},
		{"two certificates in ca.crt", func(s *corev1.Secret) { s.Data[CACertKey] = twoCerts }, `data["ca.crt"]: holds 2 certificates, want exactly 1`},
		{"ca.crt is not a CA", func(s *corev1.Secret) { s.Data[CACertKey] = notCAPEM }, `data["ca.crt"] is not a CA certificate`},
		{"expired CA", func(s *corev1.Secret) { s.Data[CACertKey], s.Data[CAKeyKey] = expiredCA, expiredCAKey }, `the CA in data["ca.crt"] expired at`},
		{"CA not yet valid", func(s *corev1.Secret) { s.Data[CACertKey], s.Data[CAKeyKey] = futureCA, futureCAKey }, `the CA in data["ca.crt"] is not valid until`},
		{"unparsable ca.key", func(s *corev1.Secret) { s.Data[CAKeyKey] = []byte("not pem") }, `data["ca.key"]: unparsable PEM private key`},
		{"ca.key does not match ca.crt", func(s *corev1.Secret) { s.Data[CAKeyKey] = other.CAKeyPEM }, `data["ca.key"] does not match the certificate in data["ca.crt"]`},
		{"unparsable tls.crt", func(s *corev1.Secret) { s.Data[CertKey] = []byte("not pem") }, `data["tls.crt"]: unparsable PEM certificate`},
		{"tls.crt signed by another CA", func(s *corev1.Secret) { s.Data[CertKey], s.Data[KeyKey] = other.CertPEM, other.KeyPEM }, `data["tls.crt"] is not signed by the CA in data["ca.crt"]`},
		{"unparsable tls.key", func(s *corev1.Secret) { s.Data[KeyKey] = []byte("not pem") }, `data["tls.key"]: unparsable PEM private key`},
		{"tls.key does not match tls.crt", func(s *corev1.Secret) { s.Data[KeyKey] = good.CAKeyPEM }, `data["tls.key"] does not match the certificate in data["tls.crt"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := secretFor(good)
			tt.mutate(secret)
			client := fake.NewClientset(secret)
			m := newTestManager(t, client, now)
			_, err := m.Ensure(t.Context())
			var corruptErr *CorruptSecretError
			if !errors.As(err, &corruptErr) {
				t.Fatalf("expected *CorruptSecretError, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name the problem %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "secret coder-system/coder-k8s-apiserver-tls") || !strings.Contains(err.Error(), "delete it") {
				t.Fatalf("error must name the Secret and the remedy: %q", err)
			}
			assertNoWrites(t, client)
			if cert, _ := m.CurrentCertKeyContent(); cert != nil {
				t.Fatal("nothing may be served from a corrupt Secret")
			}
		})
	}
}

func TestRunKeepsServingWhenRefreshFails(t *testing.T) {
	now := time.Now()
	existing, err := Generate(testNS, now)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(secretFor(existing))
	m := newTestManager(t, client, now)
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.Run(ctx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for gets.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if gets.Load() < 2 {
		t.Fatal("Run did not refresh periodically")
	}
	if cert, _ := m.CurrentCertKeyContent(); string(cert) != string(existing.CertPEM) {
		t.Fatal("a failed refresh must keep serving the current certificate")
	}
}

func TestRunRenewsAndNotifies(t *testing.T) {
	issued := time.Now()
	existing, err := Generate(testNS, issued)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(secretFor(existing))
	var clock atomic.Int64
	clock.Store(issued.UnixNano())
	m, err := NewManager(client, testNS)
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Unix(0, clock.Load()) }
	if _, err := m.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	listener := &countingListener{}
	m.AddListener(listener)
	clock.Store(issued.Add(300 * 24 * time.Hour).UnixNano())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.Run(ctx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for listener.count.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if listener.count.Load() == 0 {
		t.Fatal("renewal during Run must notify listeners")
	}
	if cert, _ := m.CurrentCertKeyContent(); string(cert) == string(existing.CertPEM) {
		t.Fatal("renewal during Run must swap the served certificate")
	}
}

func TestNewManagerAssertions(t *testing.T) {
	if _, err := NewManager(nil, testNS); err == nil {
		t.Fatal("expected assertion for nil client")
	}
	if _, err := NewManager(fake.NewClientset(), ""); err == nil {
		t.Fatal("expected assertion for empty namespace")
	}
}

func newTestManager(t *testing.T, client *fake.Clientset, now time.Time) *Manager {
	t.Helper()
	m, err := NewManager(client, testNS)
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return now }
	return m
}

func secretFor(b *Bundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: testNS, Labels: copyLabels(), ResourceVersion: "1"},
		Type:       SecretType,
		Data:       b.Data(),
	}
}

func assertNoWrites(t *testing.T, client *fake.Clientset) {
	t.Helper()
	for _, a := range client.Actions() {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
			t.Fatalf("unexpected %s on %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// selfSignedCA builds a CA with an explicit validity window (certutil fixes it at 10 years).
func selfSignedCA(t *testing.T, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := certutil.EncodeCertificates(cert)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := keyutil.MarshalPrivateKeyToPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM
}

type countingListener struct{ count atomic.Int32 }

func (l *countingListener) Enqueue() { l.count.Add(1) }
