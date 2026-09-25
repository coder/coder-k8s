package servingcert

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

var log = ctrl.Log.WithName("servingcert")

// DefaultCheckInterval is how often Run re-reads the Secret and renews the serving certificate.
const DefaultCheckInterval = 12 * time.Hour

const maxEnsureAttempts = 5

// Manager keeps the serving certificate in sync with the Secret.
//
// Ensure is the adopt path: any valid Secret is used as is (so a later "bring your own Secret"
// mode only has to skip generation and renewal). Listeners registered through
// CertKeyContentProvider().AddListener are notified whenever the served certificate or the CA
// changes, so an APIService caBundle controller can react without polling.
type Manager struct {
	client    kubernetes.Interface
	namespace string
	now       func() time.Time

	mu        sync.RWMutex
	current   *Bundle
	listeners []dynamiccertificates.Listener
}

var _ dynamiccertificates.CertKeyContentProvider = (*Manager)(nil)

// NewManager returns a Manager for the Secret in namespace.
func NewManager(client kubernetes.Interface, namespace string) (*Manager, error) {
	if client == nil {
		return nil, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if namespace == "" {
		return nil, fmt.Errorf("assertion failed: namespace must not be empty")
	}
	return &Manager{client: client, namespace: namespace, now: time.Now}, nil
}

// Ensure loads, creates, or renews the Secret and makes it the served certificate. Invalid
// trust material returns a *CorruptSecretError and is never overwritten.
//
// An existing placeholder (see IsPlaceholder) is filled with a new CA instead of being created,
// so an identity that may only get and update this one Secret can still bootstrap it.
func (m *Manager) Ensure(ctx context.Context) (*Bundle, error) {
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	secrets := m.client.CoreV1().Secrets(m.namespace)
	for attempt := 1; attempt <= maxEnsureAttempts; attempt++ {
		now := m.now()
		secret, err := secrets.Get(ctx, SecretName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			bundle, genErr := Generate(m.namespace, now)
			if genErr != nil {
				return nil, genErr
			}
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: m.namespace, Labels: copyLabels()},
				Type:       SecretType,
				Data:       bundle.Data(),
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue // Another replica created it first; adopt theirs.
			}
			if apierrors.IsForbidden(err) {
				return nil, fmt.Errorf("create secret %s/%s: %w; if this identity may not create Secrets, "+
					"create an empty placeholder Secret %q of type %q (no data) and the server fills it",
					m.namespace, SecretName, err, SecretName, SecretType)
			}
			if err != nil {
				return nil, fmt.Errorf("create secret %s/%s: %w", m.namespace, SecretName, err)
			}
			log.Info("Created aggregated API server CA and serving certificate", "namespace", m.namespace, "secret", SecretName)
			return bundle, m.serve(bundle)
		case err != nil:
			return nil, fmt.Errorf("get secret %s/%s: %w", m.namespace, SecretName, err)
		}

		if IsPlaceholder(secret) {
			bundle, genErr := Generate(m.namespace, now)
			if genErr != nil {
				return nil, genErr
			}
			filled := secret.DeepCopy()
			filled.Data = bundle.Data()
			// Mark it like a created Secret, keeping any labels the placeholder already has.
			if filled.Labels == nil {
				filled.Labels = map[string]string{}
			}
			for k, v := range SecretLabels {
				filled.Labels[k] = v
			}
			// Without a resourceVersion the update would be unconditional and could replace a CA
			// that another replica wrote in the meantime.
			if filled.ResourceVersion == "" {
				return nil, fmt.Errorf("assertion failed: placeholder secret %s/%s has no resourceVersion", m.namespace, SecretName)
			}
			_, err = secrets.Update(ctx, filled, metav1.UpdateOptions{})
			if apierrors.IsConflict(err) {
				continue // Another replica filled it first; re-read and adopt theirs.
			}
			if err != nil {
				return nil, fmt.Errorf("fill placeholder secret %s/%s: %w", m.namespace, SecretName, err)
			}
			log.Info("Created aggregated API server CA and serving certificate in the placeholder Secret", "namespace", m.namespace, "secret", SecretName)
			return bundle, m.serve(bundle)
		}

		bundle, err := Parse(secret, m.namespace, now)
		if err != nil {
			return nil, err
		}
		if !bundle.NeedsRenewal(m.namespace, now) {
			return bundle, m.serve(bundle)
		}
		if err := bundle.issueServingCert(m.namespace, now); err != nil {
			return nil, err
		}
		updated := secret.DeepCopy()
		updated.Data = bundle.Data()
		_, err = secrets.Update(ctx, updated, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue // Another replica renewed it; re-read and adopt.
		}
		if err != nil {
			return nil, fmt.Errorf("update secret %s/%s: %w", m.namespace, SecretName, err)
		}
		log.Info("Renewed aggregated API server serving certificate", "namespace", m.namespace, "secret", SecretName, "notAfter", bundle.Cert.NotAfter)
		return bundle, m.serve(bundle)
	}
	return nil, fmt.Errorf("secret %s/%s kept changing; gave up after %d attempts", m.namespace, SecretName, maxEnsureAttempts)
}

// Run calls Ensure every interval until ctx is done. Failures are logged and the current
// certificate keeps being served.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		panic("assertion failed: interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.Ensure(ctx); err != nil {
				log.Error(err, "Could not refresh the aggregated API server serving certificate; still serving the current one")
			}
		}
	}
}

// CABundle returns the PEM CA that signs the served certificate, or nil before the first Ensure.
func (m *Manager) CABundle() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return nil
	}
	return m.current.CACertPEM
}

// serve makes bundle the served certificate and notifies listeners if anything changed.
func (m *Manager) serve(bundle *Bundle) error {
	// Same check the vendored static provider performs.
	if _, err := tls.X509KeyPair(bundle.CertPEM, bundle.KeyPEM); err != nil {
		return fmt.Errorf("assertion failed: validated serving certificate is not a usable key pair: %w", err)
	}
	m.mu.Lock()
	changed := m.current == nil || !bundleEqual(m.current, bundle)
	m.current = bundle
	listeners := append([]dynamiccertificates.Listener(nil), m.listeners...)
	m.mu.Unlock()
	if changed {
		for _, l := range listeners {
			l.Enqueue()
		}
	}
	return nil
}

// Name implements dynamiccertificates.CertKeyContentProvider.
func (m *Manager) Name() string {
	return "coder-k8s-managed-serving-cert::" + m.namespace + "/" + SecretName
}

// CurrentCertKeyContent implements dynamiccertificates.CertKeyContentProvider.
func (m *Manager) CurrentCertKeyContent() ([]byte, []byte) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return nil, nil
	}
	return m.current.CertPEM, m.current.KeyPEM
}

// AddListener implements dynamiccertificates.Notifier.
func (m *Manager) AddListener(listener dynamiccertificates.Listener) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listeners = append(m.listeners, listener)
}

func bundleEqual(a, b *Bundle) bool {
	return string(a.CACertPEM) == string(b.CACertPEM) && string(a.CertPEM) == string(b.CertPEM) && string(a.KeyPEM) == string(b.KeyPEM)
}

func copyLabels() map[string]string {
	out := make(map[string]string, len(SecretLabels))
	for k, v := range SecretLabels {
		out[k] = v
	}
	return out
}
