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

// startPoolManager runs the pool or member reconciler, or both, through
// their SetupWithManager in a manager for the env's namespace. The cache
// refuses reads of a kind without an informer, so each SetupWithManager must
// start the informers of everything it reads through the cache.
func (e *poolMemberEnv) startPoolManager(t *testing.T, pool, member bool) {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache:      cache.Options{ReaderFailOnMissingInformer: true, DefaultNamespaces: map[string]cache.Config{e.ns: {}}},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	if pool {
		r := &controller.CoderWorkspacePoolReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Clock: e.clock}
		require.NoError(t, r.SetupWithManager(mgr))
	}
	if member {
		m := &controller.CoderWorkspacePoolMemberReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Clock: e.clock}
		require.NoError(t, m.SetupWithManager(mgr))
	}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
}

// settleWatchStep waits until the reconciles that the last step started are
// done, the 1 s requeue after storing new resolved inputs included. The next
// step must then finish within watchStepTimeout, before any poll of the pool
// (15 s while not Ready, 60 s while it waits, none while Ready), so only the
// watch under test can satisfy it.
func settleWatchStep() { time.Sleep(2 * time.Second) }

const watchStepTimeout = 5 * time.Second

// TestPoolWatches runs the pool reconciler in a manager. A control plane
// change wakes a pool that waits for it, a pool spec change wakes the pool,
// a new claim wakes a Ready pool, and a change of a bound member wakes its
// pool, although the member's controller is the claim.
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
	e.startPoolManager(t, true, false)

	poolKey := types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}
	getPool := func() *coderv1alpha1.CoderWorkspacePool {
		pool := &coderv1alpha1.CoderWorkspacePool{}
		require.NoError(t, k8sClient.Get(e.ctx, poolKey, pool))
		return pool
	}
	require.Eventually(t, func() bool { return getPool().Status.Reason == "OperatorAccessNotReady" }, 10*time.Second, 50*time.Millisecond)
	settleWatchStep()
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = true
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	require.Eventually(t, func() bool {
		return meta.IsStatusConditionTrue(getPool().Status.Conditions, coderv1alpha1.CoderWorkspacePoolConditionReady)
	}, watchStepTimeout, 50*time.Millisecond, "the control plane watch wakes the pool")

	settleWatchStep()
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.MaxInFlight = new(int32(4)) })
	require.Eventually(t, func() bool {
		pool := getPool()
		return pool.Status.ObservedGeneration == pool.Generation
	}, watchStepTimeout, 50*time.Millisecond, "the pool watch wakes the pool")

	settleWatchStep()
	claimKey := e.createClaim(t, "build-1")
	claimIs := func(phase string) func() bool {
		return func() bool { return e.claim(t, claimKey).Status.Phase == phase }
	}
	require.Eventually(t, claimIs(coderv1alpha1.CoderWorkspaceClaimPhaseBound), watchStepTimeout, 50*time.Millisecond, "the claim watch wakes the pool")
	e.requireBound(t, ready, claimKey)

	settleWatchStep()
	e.setMemberStatus(t, ready, memberPhase(coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed,
		memberCondition(coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, metav1.ConditionTrue, "DeletedExternally")))
	require.Eventually(t, claimIs(coderv1alpha1.CoderWorkspaceClaimPhaseLost), watchStepTimeout, 50*time.Millisecond, "the member watch wakes the pool")
}

// TestPoolMemberSetupWithManager runs only the member reconciler in a
// manager. A new member gets its finalizer and waits for its control plane.
// A control plane change wakes it before its 15 s poll, and it sends its
// create request, which reads the control plane and the operator token
// through the cache.
func TestPoolMemberSetupWithManager(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	e.startPoolManager(t, false, true)
	key := e.createMember(t, nil)
	require.Eventually(t, func() bool {
		m := e.reconcileMember(t, key, 0)
		return controllerutil.ContainsFinalizer(m, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer) && m.Status.Reason == "OperatorAccessNotReady"
	}, 10*time.Second, 50*time.Millisecond)

	settleWatchStep()
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Status.OperatorAccessReady = true
	require.NoError(t, k8sClient.Status().Update(e.ctx, cp))
	require.Eventually(t, func() bool { return e.reconcileMember(t, key, 0).Status.WorkspaceID != "" },
		watchStepTimeout, 50*time.Millisecond, "the control plane watch wakes the member")
}

// TestPoolMemberReleasedWhenControlPlaneDeleted runs the member reconciler in
// a manager. A deleted member that stopped with OwnershipUnknown keeps its
// finalizer. Once its control plane is deleted, the control plane watch wakes
// it, and it releases its finalizer without a Coder call: nothing can reach
// Coder any more. OwnershipUnknown schedules no poll, so only the watch can
// release it.
func TestPoolMemberReleasedWhenControlPlaneDeleted(t *testing.T) {
	t.Parallel()
	e := newPoolMemberEnv(t)
	key, workspaceID := e.readyMember(t)
	e.fake.renameWorkspace(workspaceID, "renamed")
	e.deleteMember(t, key)
	requireDeletedCondition(t, e.reconcileMember(t, key, 1), metav1.ConditionUnknown, "OwnershipUnknown")
	e.startPoolManager(t, false, true)
	settleWatchStep()
	require.NotNil(t, e.reconcileMember(t, key, 0), "the finalizer stays")

	requests := e.fake.totalRequests()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	require.NoError(t, k8sClient.Delete(e.ctx, cp))
	require.Eventually(t, func() bool { return e.reconcileMember(t, key, 0) == nil }, watchStepTimeout, 50*time.Millisecond,
		"the control plane watch wakes the member, and it releases its finalizer")
	require.Equal(t, requests, e.fake.totalRequests(), "no Coder call")
}
