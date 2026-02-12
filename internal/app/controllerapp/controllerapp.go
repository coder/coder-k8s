// Package controllerapp provides the controller-runtime application mode for coder-k8s.
package controllerapp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/coder/coder-k8s/internal/app/sharedscheme"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controller"
)

const (
	// HealthProbeBindAddress exposes /healthz and /readyz checks for kube probes.
	HealthProbeBindAddress = ":8081"

	// leaderElectionID is the stable identity used for leader-election lease objects.
	leaderElectionID = "coder-k8s-controller.coder.com"

	// defaultLeaderElectionNamespace is used when the pod namespace cannot be
	// detected (e.g. out-of-cluster development runs).
	defaultLeaderElectionNamespace = "kube-system"

	// inClusterNamespacePath is the standard path where Kubernetes injects the
	// pod namespace when running inside a cluster.
	inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

var setupLog = ctrl.Log.WithName("setup")

// NewScheme builds the runtime scheme used by the controller application.
func NewScheme() *runtime.Scheme {
	return sharedscheme.New()
}

// NewManager builds a controller-runtime manager for the controller application mode.
func NewManager(cfg *rest.Config, scheme *runtime.Scheme) (manager.Manager, error) {
	if cfg == nil {
		return nil, fmt.Errorf("assertion failed: config must not be nil")
	}
	if scheme == nil {
		return nil, fmt.Errorf("assertion failed: scheme must not be nil")
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme,
		HealthProbeBindAddress:        HealthProbeBindAddress,
		LeaderElection:                true,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       detectLeaderElectionNamespace(),
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to start manager: %w", err)
	}
	if mgr == nil {
		return nil, fmt.Errorf("assertion failed: manager is nil after successful construction")
	}

	return mgr, nil
}

// SetupControllers registers all controller reconcilers on the manager.
func SetupControllers(mgr manager.Manager) error {
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager must not be nil")
	}

	client := mgr.GetClient()
	if client == nil {
		return fmt.Errorf("assertion failed: manager client is nil")
	}

	managerScheme := mgr.GetScheme()
	if managerScheme == nil {
		return fmt.Errorf("assertion failed: manager scheme is nil")
	}

	reconciler := &controller.CoderControlPlaneReconciler{
		Client:                    client,
		Scheme:                    managerScheme,
		OperatorAccessProvisioner: coderbootstrap.NewPostgresOperatorAccessProvisioner(),
		LicenseUploader:           controller.NewSDKLicenseUploader(),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller: %w", err)
	}

	coderWorkspaceProxyReconciler := &controller.CoderWorkspaceProxyReconciler{
		Client:          client,
		Scheme:          managerScheme,
		BootstrapClient: coderbootstrap.NewSDKClient(),
	}
	if err := coderWorkspaceProxyReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create coder workspace proxy controller: %w", err)
	}

	provisionerReconciler := &controller.CoderProvisionerReconciler{
		Client:          client,
		Scheme:          managerScheme,
		BootstrapClient: coderbootstrap.NewSDKClient(),
	}
	if err := provisionerReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create provisioner controller: %w", err)
	}

	return nil
}

// SetupProbes configures health and readiness checks on the manager.
func SetupProbes(mgr manager.Manager) error {
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager must not be nil")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if synced := mgr.GetCache().WaitForCacheSync(ctx); !synced {
			return fmt.Errorf("informer caches not synced")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	return nil
}

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Run starts the controller-runtime manager for the controller application mode.
func Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}

	scheme := NewScheme()
	if scheme == nil {
		return fmt.Errorf("assertion failed: scheme is nil after successful construction")
	}

	mgr, err := NewManager(ctrl.GetConfigOrDie(), scheme)
	if err != nil {
		return err
	}

	if err := SetupControllers(mgr); err != nil {
		return err
	}
	if err := SetupProbes(mgr); err != nil {
		return err
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}

// detectLeaderElectionNamespace returns the namespace to use for leader-election
// lease objects. Resolution order:
//  1. POD_NAMESPACE env var (allows explicit override for any environment).
//  2. In-cluster namespace file (standard Kubernetes downward API path).
//  3. defaultLeaderElectionNamespace as a last-resort fallback.
func detectLeaderElectionNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	data, err := os.ReadFile(inClusterNamespacePath)
	if err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return defaultLeaderElectionNamespace
}
