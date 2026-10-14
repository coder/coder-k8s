package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// Plan R13: one requested rotation with a fresh reconciler at every step
// sends exactly 2 creates and 1 DELETE. A create that fails during the
// rotation keeps Ready=True, because the Secret still holds the old key.
func TestCoderAIGatewayKeyRequestedRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createNamedKey(ctx, t, "gateway", "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	first := got.Status.CurrentKey

	got.Spec.Rotation.Trigger = "rotate-1"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusTooManyRequests})
	got, _ = env.reconcile(ctx, t, got)
	requireReadyWhile(t, got, "CoderUnavailable")
	require.Equal(t, first.ID, got.Status.CurrentKey.ID)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, "rotate-1", got.Status.LastHandledTrigger)
	require.Equal(t, []string{first.ID}, retiringIDs(got))

	for _, step := range []time.Duration{30 * time.Minute, 30 * time.Minute, 0} {
		env.clock.Step(step)
		got, _ = env.reconcile(ctx, t, got)
	}
	requireAIGatewayKeyReady(t, got)
	require.Empty(t, got.Status.RetiringKeys)
	require.Equal(t, 3, env.fake.requestCount(routeCreateAIGatewayKey), "the first key, one 429, the rotation")
	require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey))
	require.Len(t, env.fake.aiGatewayKeys(), 1)
}

// Plan 3.4: a requested rotation while one key retires works. A third one
// while two keys retire reports RotationBlocked with Ready=True and creates
// nothing, and it runs once the retire step leaves room.
func TestCoderAIGatewayKeyRotationBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	for _, trigger := range []string{"r1", "r2", "r3"} {
		got.Spec.Rotation.Trigger = trigger
		require.NoError(t, k8sClient.Update(ctx, got))
		got, _ = env.reconcile(ctx, t, got)
	}
	requireReadyWhile(t, got, "RotationBlocked")
	require.Contains(t, meta.FindStatusCondition(got.Status.Conditions, "Ready").Message, aiGatewayKeyPrefix(got)+"0")
	require.Len(t, got.Status.RetiringKeys, 2)
	require.Equal(t, 3, env.fake.requestCount(routeCreateAIGatewayKey))

	env.clock.Step(time.Hour)
	for range 3 { // Two retire steps, then the rotation.
		got, _ = env.reconcile(ctx, t, got)
	}
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, "r3", got.Status.LastHandledTrigger)
	require.Equal(t, int64(3), got.Status.CurrentKey.Serial)
	require.Equal(t, 4, env.fake.requestCount(routeCreateAIGatewayKey))
	require.Equal(t, 2, env.fake.requestCount(routeDeleteAIGatewayKey))
}

// Plan 3.4: the scheduled rotation counts from issuedAt, an interval change
// applies at once, and interval 0 turns it off.
func TestCoderAIGatewayKeyScheduledRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	key.Spec.Rotation.IntervalSeconds = ptrTo(int32(2 * 86400))
	require.NoError(t, k8sClient.Update(ctx, key))
	got, result := env.reconcile(ctx, t, key)
	requireAIGatewayKeyReady(t, got)
	issued := got.Status.CurrentKey.IssuedAt.Time
	require.Equal(t, issued.Add(48*time.Hour).Unix(), got.Status.NextRotationTime.Unix())
	require.LessOrEqual(t, result.RequeueAfter, 10*time.Minute)

	got.Spec.Rotation.IntervalSeconds = ptrTo(int32(86400))
	require.NoError(t, k8sClient.Update(ctx, got))
	env.clock.Step(24*time.Hour - time.Minute)
	got, result = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, issued.Add(24*time.Hour).Unix(), got.Status.NextRotationTime.Unix(), "the shorter interval applies at once")
	require.Equal(t, time.Minute, result.RequeueAfter)
	env.clock.Step(time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, int64(1), got.Status.CurrentKey.Serial)
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey))

	got.Spec.Rotation.IntervalSeconds = ptrTo(int32(0))
	require.NoError(t, k8sClient.Update(ctx, got))
	for range 3 {
		env.clock.Step(30 * 24 * time.Hour)
		got, _ = env.reconcile(ctx, t, got)
	}
	require.Nil(t, got.Status.NextRotationTime)
	require.Equal(t, int64(1), got.Status.CurrentKey.Serial)
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey))
}

// Plan R8: a malformed Secret value is a lost key, as in R7: a new key, and
// the old one retires with the full grace period.
func TestCoderAIGatewayKeyMalformedSecretValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	first := got.Status.CurrentKey
	setOutputSecretData(ctx, t, env.namespace, map[string][]byte{coderv1alpha1.CoderAIGatewayKeySecretDataKey: []byte(first.Name)})
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, int64(1), got.Status.CurrentKey.Serial)
	require.Equal(t, []string{first.ID}, retiringIDs(got))
	require.Equal(t, env.clock.Now().Unix(), got.Status.RetiringKeys[0].ReplacedAt.Unix())
}
