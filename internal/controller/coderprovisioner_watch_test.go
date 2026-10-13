package controller_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// TestCoderProvisionerCredentials_SecretWatchRequeues runs the reconciler in
// a manager and checks that deleting an unowned key Secret re-queues the
// provisioner through the Secret watch. A refused provisioner does not requeue
// itself, and the unowned Secret has no owner reference, so only the watch on
// the credential Secret name index can wake it.
func TestCoderProvisionerCredentials_SecretWatchRequeues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProvisionerCredentialFixture(ctx, t, "cprov-watch")
	unowned := createSecret(ctx, t, tokenSecret(metav1.ObjectMeta{Name: "existing-key", Namespace: f.namespace}, coderv1alpha1.DefaultProvisionerKeySecretKey, testUnownedKeyValue))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{f.namespace: {}}},
		Controller:             config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	// The manager goroutine owns f.bootstrapClient from here on: the test reads
	// only Kubernetes objects, so it does not race with the fake.
	reads := &atomic.Int64{}
	r := &controller.CoderProvisionerReconciler{Client: readCountingClient{Client: mgr.GetClient(), reads: reads}, Scheme: mgr.GetScheme(), BootstrapClient: f.bootstrapClient}
	require.NoError(t, r.SetupWithManager(mgr))
	mgrCtx, cancel := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-mgrDone)
	})

	provisioner := f.createKeyProvisioner(ctx, t, "provisioner", unowned.Name)
	require.Eventually(t, func() bool {
		condition := meta.FindStatusCondition(f.get(ctx, t, provisioner).Status.Conditions, "CredentialsReady")
		return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "KeySecretNotOwned"
	}, 30*time.Second, 100*time.Millisecond)
	_, exists := f.deployment(ctx, t, provisioner)
	require.False(t, exists, "a refused provisioner must not get a Deployment")

	// Writing CredentialsReady=False queues one more reconcile through the
	// provisioner's own watch. Wait until the reconciler is idle, so that the
	// Secret deletion below can only be picked up through the Secret watch.
	requireReconcilerIdle(t, reads, 5*time.Second)

	// The documented remedy: delete the unowned Secret. Only the watch can
	// trigger the next reconcile.
	require.NoError(t, k8sClient.Delete(ctx, unowned))

	require.Eventually(t, func() bool {
		latest := f.get(ctx, t, provisioner)
		condition := meta.FindStatusCondition(latest.Status.Conditions, "CredentialsReady")
		if condition == nil || condition.Status != metav1.ConditionTrue {
			return false
		}
		created := &corev1.Secret{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(unowned), created); err != nil {
			return false
		}
		owner := metav1.GetControllerOf(created)
		_, exists := f.deployment(ctx, t, provisioner)
		return created.UID != unowned.UID && owner != nil && owner.UID == latest.UID && exists
	}, 30*time.Second, 100*time.Millisecond)
}

// readCountingClient counts the reads of a reconciler. Every reconcile reads
// objects, so an unchanged count over a window means no reconcile ran in it.
type readCountingClient struct {
	client.Client
	reads *atomic.Int64
}

func (c readCountingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.reads.Add(1)
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c readCountingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.reads.Add(1)
	return c.Client.List(ctx, list, opts...)
}

// requireReconcilerIdle waits until reads stays unchanged for window.
func requireReconcilerIdle(t *testing.T, reads *atomic.Int64, window time.Duration) {
	t.Helper()
	last, stableSince := reads.Load(), time.Now()
	require.Eventually(t, func() bool {
		if current := reads.Load(); current != last {
			last, stableSince = current, time.Now()
			return false
		}
		return time.Since(stableSince) >= window
	}, 60*time.Second, 100*time.Millisecond, "the reconciler did not become idle")
}
