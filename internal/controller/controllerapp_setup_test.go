package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/coder/coder-k8s/internal/app/controllerapp"
)

// A manager with every controller of the controller app registered starts
// against the envtest API server: no field index is registered twice, every
// watched kind is served, and the caches sync. The test runs serially, and
// its cache sees only an empty namespace, so its reconcilers never touch
// the objects of other tests.
func TestSetupControllersStartsEveryController(t *testing.T) {
	ctx := context.Background()
	namespace := createTestNamespace(ctx, t, "setup")
	scheme := controllerapp.NewScheme()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Other tests build managers with the same controller names, and
		// controller-runtime checks names across the process.
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	require.NoError(t, controllerapp.SetupControllers(mgr))

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	require.True(t, mgr.GetCache().WaitForCacheSync(syncCtx), "the caches sync")
	cancel()
	require.NoError(t, <-done)
}
