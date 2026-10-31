package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// startPoolManager runs the pool and member reconcilers through their
// SetupWithManager in a manager for the env's namespace.
func (e *poolMemberEnv) startPoolManager(t *testing.T, member bool) {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{e.ns: {}}},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	pool := &controller.CoderWorkspacePoolReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Clock: e.clock}
	require.NoError(t, pool.SetupWithManager(mgr))
	if member {
		m := &controller.CoderWorkspacePoolMemberReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Clock: e.clock}
		require.NoError(t, m.SetupWithManager(mgr))
	}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
}

// TestPoolWatches runs the pool reconciler in a manager. A control plane
// change wakes a pool that waits for it, a new claim wakes a Ready pool, and
// a change of a bound member wakes its pool, although the member's controller
// is the claim. A Ready pool does not poll, and a waiting pool polls every
// 60 s, so only the watches are this fast.
func TestPoolWatches(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(1)) })
	ready := e.createMember(t, nil)
	e.setMemberStatus(t, ready, readyWithWorkspace("kwp-ready"))
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	e.startPoolManager(t, false)

	poolKey := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	poolIs := func(reason string) func() bool {
		return func() bool {
			pool := &coderv1alpha1.CoderWorkspacePool{}
			require.NoError(t, k8sClient.Get(e.ctx, poolKey, pool))
			return pool.Status.Reason == reason
		}
	}
	require.Eventually(t, poolIs("OperatorAccessNotReady"), 10*time.Second, 50*time.Millisecond)
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = true
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	require.Eventually(t, func() bool {
		pool := &coderv1alpha1.CoderWorkspacePool{}
		require.NoError(t, k8sClient.Get(e.ctx, poolKey, pool))
		return meta.IsStatusConditionTrue(pool.Status.Conditions, coderv1alpha1.CoderWorkspacePoolConditionReady)
	}, 5*time.Second, 50*time.Millisecond, "the control plane watch wakes the pool")
	// A pool that stores new resolved inputs reconciles once more after 1 s.
	time.Sleep(2 * time.Second)

	claimKey := e.createClaim(t, "build-1")
	claimIs := func(phase string) func() bool {
		return func() bool { return e.claim(t, claimKey).Status.Phase == phase }
	}
	require.Eventually(t, claimIs(coderv1alpha1.CoderWorkspaceClaimPhaseBound), 5*time.Second, 50*time.Millisecond, "the claim watch wakes the pool")
	e.requireBound(t, ready, claimKey)

	e.setMemberStatus(t, ready, memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed,
		memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, metav1.ConditionTrue, "DeletedExternally")))
	require.Eventually(t, claimIs(coderv1alpha1.CoderWorkspaceClaimPhaseLost), 5*time.Second, 50*time.Millisecond, "the member watch wakes the pool")
}

// TestPoolMemberSetupWithManager runs the member reconciler in a manager: a
// new member gets its finalizer.
func TestPoolMemberSetupWithManager(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(1)) })
	e.startPoolManager(t, true)
	key := e.createMember(t, nil)
	require.Eventually(t, func() bool {
		return controllerutil.ContainsFinalizer(e.reconcileMember(t, key, 0), coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer)
	}, 10*time.Second, 50*time.Millisecond)
}
