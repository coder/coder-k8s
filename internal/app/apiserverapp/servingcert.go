package apiserverapp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
)

// serviceAccountNamespaceFile is mounted into every Pod with a ServiceAccount token.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// newServingCertManager returns the serving certificate manager for the Pod's namespace, or nil
// when the process does not run in a Pod (no namespace file), for example `go run` on a laptop.
// It uses the same Kubernetes API authority as delegated authentication (kubeconfigPath, or the
// in-cluster ServiceAccount when empty).
func newServingCertManager(kubeconfigPath, namespaceFile string) (*servingcert.Manager, error) {
	data, err := os.ReadFile(namespaceFile) //nolint:gosec // G304: fixed ServiceAccount mount path (tests pass a temp file).
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pod namespace from %s: %w", namespaceFile, err)
	}
	namespace := strings.TrimSpace(string(data))
	if namespace == "" {
		return nil, fmt.Errorf("pod namespace file %s is empty", namespaceFile)
	}

	var cfg *rest.Config
	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client: %w", err)
	}
	return servingcert.NewManager(client, namespace)
}
