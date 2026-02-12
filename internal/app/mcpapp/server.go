// Package mcpapp provides MCP server application modes for coder-k8s.
package mcpapp

import (
	"fmt"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	serverImplementationName    = "coder-k8s"
	serverImplementationVersion = "dev"
)

// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=aggregation.coder.com,resources=coderworkspaces;codertemplates,verbs=get;list;watch;update;patch

// NewServer creates an MCP server with all tools registered.
func NewServer(k8sClient client.Client, clientset kubernetes.Interface) *mcp.Server {
	if k8sClient == nil {
		panic("assertion failed: Kubernetes client must not be nil")
	}
	if clientset == nil {
		panic("assertion failed: Kubernetes clientset must not be nil")
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    serverImplementationName,
		Version: serverImplementationVersion,
	}, nil)
	registerTools(server, k8sClient, clientset)
	return server
}

func newClients() (client.Client, kubernetes.Interface, error) {
	cfg := ctrl.GetConfigOrDie()
	if cfg == nil {
		return nil, nil, fmt.Errorf("assertion failed: Kubernetes config is nil after successful construction")
	}

	scheme := newScheme()
	if scheme == nil {
		return nil, nil, fmt.Errorf("assertion failed: scheme is nil after successful construction")
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("build Kubernetes controller-runtime client: %w", err)
	}
	if k8sClient == nil {
		return nil, nil, fmt.Errorf("assertion failed: Kubernetes controller-runtime client is nil after successful construction")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build Kubernetes clientset: %w", err)
	}
	if clientset == nil {
		return nil, nil, fmt.Errorf("assertion failed: Kubernetes clientset is nil after successful construction")
	}

	return k8sClient, clientset, nil
}

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(coderv1alpha1.AddToScheme(scheme))
	utilruntime.Must(aggregationv1alpha1.AddToScheme(scheme))
	return scheme
}
