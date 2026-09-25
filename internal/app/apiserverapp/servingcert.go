package apiserverapp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/coder/coder-k8s/internal/aggregated/apiservicetrust"
	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
)

// serviceAccountNamespaceFile is mounted into every Pod with a ServiceAccount token.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// managedTLS is the in-Pod TLS setup: the serving certificate manager and the controller that
// keeps the APIService caBundle trusting its CA.
type managedTLS struct {
	manager *servingcert.Manager
	sync    *apiservicetrust.Controller
}

// newManagedTLS returns the managed TLS setup for the Pod's namespace, or nil when the process
// does not run in a Pod (no namespace file), for example `go run` on a laptop. It uses the same
// Kubernetes API authority as delegated authentication (kubeconfigPath, or the in-cluster
// ServiceAccount when empty).
func newManagedTLS(kubeconfigPath, namespaceFile string) (*managedTLS, error) {
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
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes dynamic client: %w", err)
	}
	manager, err := servingcert.NewManager(client, namespace)
	if err != nil {
		return nil, err
	}
	sync, err := apiservicetrust.New(dyn, client, namespace)
	if err != nil {
		return nil, err
	}
	return &managedTLS{manager: manager, sync: sync}, nil
}
