package controlplaneurl

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

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

// parseCABundle accepts only a bundle of PEM certificates, so that a bundle
// with other data, or a broken or cut-off certificate, fails instead of
// trusting only some of its certificates.
func parseCABundle(caBundle []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	count := 0
	beginMarker := []byte("-----BEGIN ")
	for rest := bytes.TrimSpace(caBundle); len(rest) > 0; {
		// pem.Decode skips data before a block and blocks that do not decode,
		// so check that it decoded the block that starts here.
		var block *pem.Block
		if bytes.HasPrefix(rest, beginMarker) {
			var next []byte
			block, next = pem.Decode(rest)
			if bytes.Count(rest[:len(rest)-len(next)], beginMarker) != 1 {
				block = nil
			}
			rest = next
		}
		if block == nil {
			if count == 0 {
				return nil, errors.New("the bundle does not start with a PEM block")
			}
			return nil, fmt.Errorf("the data after certificate %d is not a PEM block", count)
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
		rest = bytes.TrimSpace(rest)
	}
	if count == 0 {
		return nil, errors.New("no PEM certificate")
	}
	return pool, nil
}

// ClientConfig returns a new TLS client configuration that verifies with t.
func (t *TLS) ClientConfig() *tls.Config {
	config := &tls.Config{ServerName: t.serverName, MinVersion: tls.VersionTLS12}
	if t.rootCAs != nil {
		// A copy, so that no change to the returned pool changes t under its key.
		config.RootCAs = t.rootCAs.Clone()
	}
	return config
}

// Key identifies the trust: two values with the same key verify the same
// certificates.
func (t *TLS) Key() string {
	return t.key
}
