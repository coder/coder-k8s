package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/app/controllerapp"
)

// A manager with every controller of the controller app registered starts
// against the envtest API server: no field index is registered twice, every
// watched kind is served, and the caches sync. The test runs serially, and
// its cache sees only an empty namespace, so its reconcilers never touch
// the objects of other tests. A CoderMCPServer in that namespace gets its
// finalizer and a status from the registered controller.
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

	server := &coderv1alpha1.CoderMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: namespace}, Spec: validMCPServerSpec()}
	require.NoError(t, k8sClient.Create(ctx, server))
	t.Cleanup(func() {
		_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(server), server)
		server.Finalizers = nil
		_ = k8sClient.Update(context.Background(), server)
		_ = k8sClient.Delete(context.Background(), server)
	})
	require.Eventually(t, func() bool {
		got := &coderv1alpha1.CoderMCPServer{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(server), got); err != nil {
			return false
		}
		ready := meta.FindStatusCondition(got.Status.Conditions, coderv1alpha1.CoderMCPServerConditionReady)
		return controllerutil.ContainsFinalizer(got, coderv1alpha1.CoderMCPServerCleanupFinalizer) &&
			ready != nil && ready.Reason == "ControlPlaneNotReady"
	}, 30*time.Second, 100*time.Millisecond, "the CoderMCPServer controller reconciles the object")

	cancel()
	require.NoError(t, <-done)
}
