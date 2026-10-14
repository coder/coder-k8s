package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// rotatedKey issues the first key and rotates once, so Coder holds the
// current key and one retiring key of the object.
func rotatedKey(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv) *coderv1alpha1.CoderAIGatewayKey {
	t.Helper()
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	got.Spec.Rotation.Trigger = "rotate-1"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	require.Len(t, got.Status.RetiringKeys, 1)
	require.Len(t, env.fake.aiGatewayKeys(), 2)
	return got
}

// Plan S7 and S8: the deletion waits for the settle window of the last
// create, with no Coder call, so a key that commits late is in the LIST.
// Then it deletes every key of this object, the current key included, and
// releases the finalizer after a LIST that shows none. A key with this
// object's prefix but an unused serial is not this object's (plan I3).
func TestCoderAIGatewayKeyDeleteDeletesEveryKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got := rotatedKey(ctx, t, &env)
	sdk := env.fake.client(t, 5*time.Second)
	_, err := sdk.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: aiGatewayKeyPrefix(got) + "9"})
	require.NoError(t, err)
	// A deposed leader sent the create of serial 2 moments ago.
	editStatus(ctx, t, got, func(s *coderv1alpha1.CoderAIGatewayKeyStatus) {
		s.NextSerial, s.LastCreateAttemptTime = 3, &metav1.Time{Time: env.clock.Now()}
	})
	require.NoError(t, k8sClient.Delete(ctx, got))

	requests := env.fake.totalRequests()
	got, result := env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingKeys")
	require.Equal(t, 5*time.Minute, result.RequeueAfter)
	require.Equal(t, requests, env.fake.totalRequests(), "no Coder call inside the settle window")
	require.Equal(t, int64(3), got.Status.NextSerial)
	// The create commits inside the window.
	late, err := sdk.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: aiGatewayKeyPrefix(got) + "2"})
	require.NoError(t, err)

	env.clock.Step(5 * time.Minute)
	gone, _ := env.reconcile(ctx, t, got)
	require.Nil(t, gone, "the finalizer went")
	keys := env.fake.aiGatewayKeys()
	require.Len(t, keys, 1, "only the key with the unused serial stays")
	require.Equal(t, aiGatewayKeyPrefix(got)+"9", keys[0].name)
	require.NotEqual(t, late.ID, keys[0].id)
	require.Equal(t, 2, env.fake.requestCount(routeAIGatewayKeys)-2, "one LIST to delete, one to prove none remain")
}

// Plan 3.5: while Coder does not answer, the finalizer stays and the
// deletion retries every 60 s. While Coder refuses (403 or no key API), it
// retries every 10 minutes. Both end in a release once Coder answers.
func TestCoderAIGatewayKeyDeleteRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		fail    func(*fakeCoder)
		heal    func(*fakeCoder)
		stalled bool
		reason  string
		requeue time.Duration
	}{
		"unavailable": {
			fail:   func(f *fakeCoder) { f.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusBadGateway}) },
			heal:   func(*fakeCoder) {},
			reason: "CoderUnavailable", requeue: time.Minute,
		},
		"premium feature": {
			fail: func(f *fakeCoder) { f.setAIGatewayFeature(false) }, heal: func(f *fakeCoder) { f.setAIGatewayFeature(true) },
			stalled: true, reason: "LicenseRequired", requeue: 10 * time.Minute,
		},
		"delete refused": {
			fail:    func(f *fakeCoder) { f.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusForbidden}) },
			heal:    func(*fakeCoder) {},
			stalled: true, reason: "CoderRejected", requeue: 10 * time.Minute,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
			require.NoError(t, k8sClient.Delete(ctx, got))
			env.clock.Step(5 * time.Minute)
			tc.fail(env.fake)
			got, result := env.reconcile(ctx, t, got)
			require.Equal(t, []string{coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer}, got.Finalizers)
			requireAIGatewayKeyStep(t, got, tc.stalled, tc.reason)
			require.Equal(t, tc.requeue, result.RequeueAfter)
			require.Len(t, env.fake.aiGatewayKeys(), 1)
			tc.heal(env.fake)
			gone, _ := env.reconcile(ctx, t, got)
			require.Nil(t, gone)
			require.Empty(t, env.fake.aiGatewayKeys())
		})
	}
}

// Plan 3.5 and D12 of #154: a gone control plane releases at once
// (ControlPlaneGone) with no Coder call, and the keys stay in Coder. While
// the control plane is being deleted, as in a namespace deletion, one
// bounded pass deletes the keys without the settle window, and the
// finalizer goes whatever the answer.
func TestCoderAIGatewayKeyDeleteWithControlPlaneGoing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("control plane gone", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		got := rotatedKey(ctx, t, &env)
		require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "coder"}}))
		require.NoError(t, k8sClient.Delete(ctx, got))
		requests := env.fake.totalRequests()
		gone, _ := env.reconcile(ctx, t, got)
		require.Nil(t, gone)
		require.Equal(t, requests, env.fake.totalRequests(), "no Coder call")
		require.Len(t, env.fake.aiGatewayKeys(), 2, "the keys stay in Coder")
	})
	for name, fail := range map[string]bool{"namespace deletion": false, "namespace deletion, DELETE fails": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got := rotatedKey(ctx, t, &env)
			require.NoError(t, k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.namespace}}))
			holdControlPlane(ctx, t, env.namespace)
			require.NoError(t, k8sClient.Delete(ctx, got))
			if fail {
				env.fake.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusBadGateway})
			}
			gone, _ := env.reconcile(ctx, t, got)
			require.Nil(t, gone, "the finalizer went inside the settle window")
			require.Equal(t, 1, env.fake.requestCount(routeAIGatewayKeys)-2, "one LIST")
			if fail {
				require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey), "the pass stops at the failed DELETE")
				require.Len(t, env.fake.aiGatewayKeys(), 2)
			} else {
				require.Empty(t, env.fake.aiGatewayKeys())
			}
		})
	}
}
