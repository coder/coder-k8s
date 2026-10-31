package controlplaneurl

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// ErrUnavailable marks a control plane with spec.tls.operatorAccess that has
// no verified HTTPS endpoint yet, for example because its CA bundle cannot be
// read or parsed. Operator components then make no Coder call, because they
// never send the operator token over plain HTTP instead.
var ErrUnavailable = errors.New("no verified HTTPS endpoint")

// TLS verifies the serving certificate of a Coder deployment. Build it with
// NewTLS.
type TLS struct {
	rootCAs    *x509.CertPool
	serverName string
	key        string
}

// NewTLS returns the trust that verifies a certificate for serverName, or for
// the URL host when serverName is empty, against the PEM CA certificates in
// caBundle. A nil caBundle uses the system roots.
func NewTLS(caBundle []byte, serverName string) (*TLS, error) {
	trust := &TLS{serverName: serverName}
	caKey := "system"
	if caBundle != nil {
		pool, err := parseCABundle(caBundle)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(caBundle)
		trust.rootCAs, caKey = pool, hex.EncodeToString(sum[:])
	}
	trust.key = "serverName=" + serverName + ",ca=" + caKey
	return trust, nil
}

func parseCABundle(caBundle []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	count := 0
	rest := caBundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		count++
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("PEM block %d is %q, not a CERTIFICATE", count, block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate %d: %w", count, err)
		}
		pool.AddCert(cert)
	}
	if count == 0 {
		return nil, errors.New("no PEM certificate")
	}
	return pool, nil
}

// ClientConfig returns a new TLS client configuration that verifies with t.
func (t *TLS) ClientConfig() *tls.Config {
	return &tls.Config{RootCAs: t.rootCAs, ServerName: t.serverName, MinVersion: tls.VersionTLS12}
}

// Key identifies the trust: two values with the same key verify the same
// certificates.
func (t *TLS) Key() string {
	return t.key
}

// OperatorTLS returns the trust for the operator's HTTPS calls to
// controlPlane, from spec.tls.operatorAccess and the CA bundle ConfigMap that
// reader reads. It is nil without spec.tls.operatorAccess. Errors that wrap
// ErrUnavailable name a missing or invalid CA bundle.
func OperatorTLS(ctx context.Context, reader client.Reader, controlPlane *coderv1alpha1.CoderControlPlane) (*TLS, error) {
	if controlPlane == nil {
		return nil, errors.New("assertion failed: coder control plane must not be nil")
	}
	access := controlPlane.Spec.TLS.OperatorAccess
	if access == nil {
		return nil, nil
	}
	ref := access.CABundleRef
	if ref == nil {
		return NewTLS(nil, access.ServerName)
	}
	if reader == nil {
		return nil, errors.New("assertion failed: CA bundle read needs a Kubernetes reader")
	}
	configMap := &corev1.ConfigMap{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: controlPlane.Namespace, Name: ref.Name}, configMap)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("%w: CA bundle ConfigMap %q does not exist", ErrUnavailable, ref.Name)
	case err != nil:
		return nil, fmt.Errorf("get CA bundle ConfigMap %s/%s: %w", controlPlane.Namespace, ref.Name, err)
	}
	caBundle, ok := configMap.Data[ref.Key]
	if !ok {
		return nil, fmt.Errorf("%w: CA bundle ConfigMap %q has no key %q", ErrUnavailable, ref.Name, ref.Key)
	}
	trust, err := NewTLS([]byte(caBundle), access.ServerName)
	if err != nil {
		return nil, fmt.Errorf("%w: CA bundle ConfigMap %q key %q: %w", ErrUnavailable, ref.Name, ref.Key, err)
	}
	return trust, nil
}
