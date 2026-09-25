// Package servingcert manages the aggregated API server's serving certificate.
//
// The server keeps a private CA and a CA-signed serving certificate in one Secret in its own
// namespace. The serving certificate is served through a dynamiccertificates.CertKeyContentProvider,
// so the generic API server's DynamicServingCertificateController picks up renewals without a
// restart. The CA is what the APIService caBundle will trust (#137).
package servingcert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
)

const (
	// ServiceName is the Service that fronts the aggregated API server (deploy/apiserver-service.yaml).
	ServiceName = "coder-k8s-apiserver"
	// SecretName is the Secret that holds the CA and the serving certificate.
	SecretName = "coder-k8s-apiserver-tls" //nolint:gosec // G101: a resource name, not a credential.
	// SecretType marks the Secret as coder-k8s's aggregated API server CA. It is deliberately not
	// kubernetes.io/tls: the Secret also holds the CA private key.
	SecretType corev1.SecretType = "coder.com/aggregated-apiserver-serving-ca" //nolint:gosec // G101: a type name, not a credential.

	// CACertKey is the Secret data key for the PEM CA certificate.
	CACertKey = "ca.crt"
	// CAKeyKey is the Secret data key for the PEM CA private key.
	CAKeyKey = "ca.key"
	// CertKey is the Secret data key for the PEM serving certificate.
	CertKey = corev1.TLSCertKey
	// KeyKey is the Secret data key for the PEM serving private key.
	KeyKey = corev1.TLSPrivateKeyKey

	// ServingCertValidity is the lifetime of a serving certificate. The CA lives 10 years
	// (client-go certutil.NewSelfSignedCACert).
	ServingCertValidity = 365 * 24 * time.Hour
	// clockSkewAllowance backdates serving certificates so small clock differences do not reject them.
	clockSkewAllowance = time.Hour
)

// SecretLabels mark the Secret as managed by coder-k8s.
var SecretLabels = map[string]string{
	"app.kubernetes.io/name":       "coder-k8s",
	"app.kubernetes.io/component":  "aggregated-apiserver-serving-ca",
	"app.kubernetes.io/managed-by": "coder-k8s",
}

// DNSNames returns the serving certificate's subject alternative names for the Service in namespace.
// kube-apiserver verifies the aggregated server with ServerName "<service>.<namespace>.svc".
func DNSNames(namespace string) []string {
	return []string{
		ServiceName,
		ServiceName + "." + namespace,
		ServiceName + "." + namespace + ".svc",
		ServiceName + "." + namespace + ".svc.cluster.local",
	}
}

// Bundle is parsed, validated trust material from the Secret.
type Bundle struct {
	CACertPEM []byte
	CAKeyPEM  []byte
	CertPEM   []byte
	KeyPEM    []byte

	CACert *x509.Certificate
	Cert   *x509.Certificate
	caKey  crypto.Signer
}

// Data returns the Secret data for the bundle.
func (b *Bundle) Data() map[string][]byte {
	return map[string][]byte{CACertKey: b.CACertPEM, CAKeyKey: b.CAKeyPEM, CertKey: b.CertPEM, KeyKey: b.KeyPEM}
}

// CorruptSecretError reports trust material that cannot be used. The server never replaces such a
// Secret on its own, because clients may already trust its CA.
type CorruptSecretError struct {
	Namespace string
	Problem   string
}

func (e *CorruptSecretError) Error() string {
	return fmt.Sprintf("secret %s/%s: %s; fix it, or delete it so coder-k8s generates a new CA (clients that trust the old CA must then be updated)",
		e.Namespace, SecretName, e.Problem)
}

func corrupt(namespace, format string, args ...any) error {
	return &CorruptSecretError{Namespace: namespace, Problem: fmt.Sprintf(format, args...)}
}

// Generate creates a new CA and a serving certificate for namespace.
func Generate(namespace string, now time.Time) (*Bundle, error) {
	if namespace == "" {
		return nil, fmt.Errorf("assertion failed: namespace must not be empty")
	}
	caKeyPEM, err := keyutil.MakeEllipticPrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	caKey, err := parseSigner(caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("assertion failed: parse generated CA key: %w", err)
	}
	caCert, err := certutil.NewSelfSignedCACert(certutil.Config{
		CommonName:   fmt.Sprintf("%s-ca@%d", ServiceName, now.Unix()),
		Organization: []string{"coder-k8s"},
	}, caKey)
	if err != nil {
		return nil, fmt.Errorf("generate CA certificate: %w", err)
	}
	caCertPEM, err := certutil.EncodeCertificates(caCert)
	if err != nil {
		return nil, fmt.Errorf("encode CA certificate: %w", err)
	}
	b := &Bundle{CACertPEM: caCertPEM, CAKeyPEM: caKeyPEM, CACert: caCert, caKey: caKey}
	if err := b.issueServingCert(namespace, now); err != nil {
		return nil, err
	}
	return b, nil
}

// issueServingCert signs a new serving certificate with the bundle's CA.
func (b *Bundle) issueServingCert(namespace string, now time.Time) error {
	if b.CACert == nil || b.caKey == nil {
		return fmt.Errorf("assertion failed: bundle has no CA")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate serving key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).SetInt64(math.MaxInt64-1))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}
	names := DNSNames(namespace)
	tmpl := &x509.Certificate{
		SerialNumber: new(big.Int).Add(serial, big.NewInt(1)),
		Subject:      pkix.Name{CommonName: names[2]},
		DNSNames:     names,
		NotBefore:    now.Add(-clockSkewAllowance).UTC(),
		NotAfter:     now.Add(ServingCertValidity).UTC(),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if tmpl.NotAfter.After(b.CACert.NotAfter) {
		tmpl.NotAfter = b.CACert.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, b.CACert, key.Public(), b.caKey)
	if err != nil {
		return fmt.Errorf("sign serving certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("assertion failed: parse signed serving certificate: %w", err)
	}
	certPEM, err := certutil.EncodeCertificates(cert)
	if err != nil {
		return fmt.Errorf("encode serving certificate: %w", err)
	}
	keyPEM, err := keyutil.MarshalPrivateKeyToPEM(key)
	if err != nil {
		return fmt.Errorf("encode serving key: %w", err)
	}
	b.Cert, b.CertPEM, b.KeyPEM = cert, certPEM, keyPEM
	return nil
}

// Parse validates the Secret's trust material. Each failure names the field that is wrong.
func Parse(secret *corev1.Secret, namespace string, now time.Time) (*Bundle, error) {
	if secret == nil {
		return nil, fmt.Errorf("assertion failed: secret must not be nil")
	}
	if secret.Type != SecretType {
		return nil, corrupt(namespace, "type is %q, want %q", secret.Type, SecretType)
	}
	for _, key := range []string{CACertKey, CAKeyKey, CertKey, KeyKey} {
		if len(secret.Data[key]) == 0 {
			return nil, corrupt(namespace, "data[%q] is missing or empty", key)
		}
	}
	b := &Bundle{
		CACertPEM: secret.Data[CACertKey], CAKeyPEM: secret.Data[CAKeyKey],
		CertPEM: secret.Data[CertKey], KeyPEM: secret.Data[KeyKey],
	}

	var err error
	if b.CACert, err = parseSingleCert(b.CACertPEM); err != nil {
		return nil, corrupt(namespace, "data[%q]: %v", CACertKey, err)
	}
	if !b.CACert.IsCA || b.CACert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, corrupt(namespace, "data[%q] is not a CA certificate", CACertKey)
	}
	if now.Before(b.CACert.NotBefore) {
		return nil, corrupt(namespace, "the CA in data[%q] is not valid until %s", CACertKey, b.CACert.NotBefore.UTC().Format(time.RFC3339))
	}
	if !now.Before(b.CACert.NotAfter) {
		return nil, corrupt(namespace, "the CA in data[%q] expired at %s", CACertKey, b.CACert.NotAfter.UTC().Format(time.RFC3339))
	}
	if b.caKey, err = parseSigner(b.CAKeyPEM); err != nil {
		return nil, corrupt(namespace, "data[%q]: %v", CAKeyKey, err)
	}
	if !publicKeysEqual(b.caKey.Public(), b.CACert.PublicKey) {
		return nil, corrupt(namespace, "data[%q] does not match the certificate in data[%q]", CAKeyKey, CACertKey)
	}

	if b.Cert, err = parseSingleCert(b.CertPEM); err != nil {
		return nil, corrupt(namespace, "data[%q]: %v", CertKey, err)
	}
	if err := b.Cert.CheckSignatureFrom(b.CACert); err != nil {
		return nil, corrupt(namespace, "data[%q] is not signed by the CA in data[%q]: %v", CertKey, CACertKey, err)
	}
	servingKey, err := parseSigner(b.KeyPEM)
	if err != nil {
		return nil, corrupt(namespace, "data[%q]: %v", KeyKey, err)
	}
	if !publicKeysEqual(servingKey.Public(), b.Cert.PublicKey) {
		return nil, corrupt(namespace, "data[%q] does not match the certificate in data[%q]", KeyKey, CertKey)
	}
	return b, nil
}

// NeedsRenewal reports whether the serving certificate must be re-issued: less than a third of
// its lifetime is left, it is not yet valid or expired, or it does not cover the Service names of
// namespace (for example after the Secret was copied from another namespace).
func (b *Bundle) NeedsRenewal(namespace string, now time.Time) bool {
	lifetime := b.Cert.NotAfter.Sub(b.Cert.NotBefore)
	renewAt := b.Cert.NotBefore.Add(lifetime * 2 / 3)
	if now.Before(b.Cert.NotBefore) || !now.Before(renewAt) {
		return true
	}
	roots := x509.NewCertPool()
	roots.AddCert(b.CACert)
	for _, name := range DNSNames(namespace) {
		if _, err := b.Cert.Verify(x509.VerifyOptions{
			DNSName: name, Roots: roots, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return true
		}
	}
	return false
}

func parseSingleCert(data []byte) (*x509.Certificate, error) {
	certs, err := certutil.ParseCertsPEM(data)
	if err != nil {
		return nil, fmt.Errorf("unparsable PEM certificate: %w", err)
	}
	if len(certs) != 1 {
		return nil, fmt.Errorf("holds %d certificates, want exactly 1", len(certs))
	}
	return certs[0], nil
}

func parseSigner(data []byte) (crypto.Signer, error) {
	key, err := keyutil.ParsePrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("unparsable PEM private key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key of type %T cannot sign", key)
	}
	return signer, nil
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	aDER, errA := x509.MarshalPKIXPublicKey(a)
	bDER, errB := x509.MarshalPKIXPublicKey(b)
	return errA == nil && errB == nil && bytes.Equal(aDER, bDER)
}
