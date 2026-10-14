package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// retiringAfterDrift issues the first key, then deletes the Secret, so key 0
// retires from now and key 1 is current.
func retiringAfterDrift(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv) (*coderv1alpha1.CoderAIGatewayKey, fakeAIGatewayKey) {
	t.Helper()
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	old := env.fake.aiGatewayKeys()[0]
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: testAIGatewaySecret}}))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, []string{old.id.String()}, retiringIDs(got))
	return got, old
}

// requireReadyWhile checks Ready=True while a replaced key waits or a
// cleanup step runs.
func requireReadyWhile(t *testing.T, key *coderv1alpha1.CoderAIGatewayKey, reason string) {
	t.Helper()
	for conditionType, status := range map[string]metav1.ConditionStatus{"Ready": metav1.ConditionTrue, "Reconciling": metav1.ConditionTrue, "Stalled": metav1.ConditionFalse} {
		condition := meta.FindStatusCondition(key.Status.Conditions, conditionType)
		require.Equal(t, status, condition.Status, conditionType)
		require.Equal(t, reason, condition.Reason, conditionType)
	}
}

// Plan R5, R6, R7 and S9: with a fresh reconciler at every step, the
// replaced key lives through its grace period, is deleted when it ends, and
// a DELETE whose answer is lost is not sent again. A reconcile with no
// change sends one LIST and writes nothing.
func TestCoderAIGatewayKeyRetireStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, old := retiringAfterDrift(ctx, t, &env)

	var result ctrl.Result
	for _, step := range []time.Duration{30 * time.Minute, 29 * time.Minute} {
		env.clock.Step(step)
		got, result = env.reconcile(ctx, t, got)
		requireReadyWhile(t, got, "WaitingForGracePeriod")
	}
	require.Equal(t, time.Minute, result.RequeueAfter, "the requeue lands at the end of the grace period")
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))

	env.clock.Step(time.Minute)
	env.fake.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	got, _ = env.reconcile(ctx, t, got)
	requireReadyWhile(t, got, "CoderUnavailable")
	require.Equal(t, []string{old.id.String()}, retiringIDs(got), "the answer was lost, so status keeps the key")
	require.Len(t, env.fake.aiGatewayKeys(), 1)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Empty(t, got.Status.RetiringKeys)
	require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey), "no second DELETE")

	lists, total := env.fake.requestCount(routeAIGatewayKeys), env.fake.totalRequests()
	got, _ = env.reconcile(ctx, t, got)
	after, _ := env.reconcile(ctx, t, got)
	require.Equal(t, got.ResourceVersion, after.ResourceVersion, "no status write")
	require.Equal(t, lists+2, env.fake.requestCount(routeAIGatewayKeys))
	require.Equal(t, total+2, env.fake.totalRequests())
}

// Plan R10: a replaced key with live connections waits after its grace
// period. A heartbeat that stops, a newer heartbeat on the current key, or
// the end of consumerWaitSeconds each lead to one DELETE.
func TestCoderAIGatewayKeyRetireWaitsForConsumers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, release := range map[string]func(*aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey, fakeAIGatewayKey){
		"heartbeat stops": func(env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey, _ fakeAIGatewayKey) {
			env.clock.Step(11 * time.Minute)
		},
		"current key heartbeats": func(env *aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey, _ fakeAIGatewayKey) {
			env.clock.Step(2 * time.Minute)
			env.fake.setAIGatewayKeyHeartbeat(uuid.MustParse(key.Status.CurrentKey.ID), env.clock.Now())
		},
		"consumer wait ends": func(env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey, old fakeAIGatewayKey) {
			env.clock.Step(10 * time.Minute)
			env.fake.setAIGatewayKeyHeartbeat(old.id, env.clock.Now())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got, old := retiringAfterDrift(ctx, t, &env)
			got.Spec.Rotation.ConsumerWaitSeconds = ptrTo(int32(600))
			require.NoError(t, k8sClient.Update(ctx, got))
			env.clock.Step(time.Hour)
			env.fake.setAIGatewayKeyHeartbeat(old.id, env.clock.Now())
			got, _ = env.reconcile(ctx, t, got)
			requireReadyWhile(t, got, "WaitingForConsumers")
			require.Contains(t, meta.FindStatusCondition(got.Status.Conditions, "Ready").Message, "Restart the AI Gateway pods")
			require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))

			release(&env, got, old)
			got, _ = env.reconcile(ctx, t, got)
			requireReadyWhile(t, got, "RetiringKey")
			require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey))
			require.Empty(t, got.Status.RetiringKeys)
		})
	}
}

// Plan R9 in delete-only mode: the Secret holds the retiring key, so the
// retire step never deletes it, also long after its grace period.
func TestCoderAIGatewayKeyRetireKeepsHeldKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, old := retiringAfterDrift(ctx, t, &env)
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, cp))
	cp.Spec.AIGateway.AllowKeys = false
	require.NoError(t, k8sClient.Update(ctx, cp))
	setOutputSecretData(ctx, t, env.namespace, map[string][]byte{"key": []byte(old.plaintext)})
	env.clock.Step(48 * time.Hour)
	got, _ = env.reconcile(ctx, t, got)
	message := requireAIGatewayKeyStep(t, got, true, "NotAllowed")
	require.Contains(t, message, "now holds key "+old.name)
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))
}

// A lost final status write after a drift re-issue: recovery 4.4 counts the
// adopted key as a drift re-issue (fail closed), so another loss of the
// Secret right away gives SecretConflict, not a second re-issue.
func TestCoderAIGatewayKeyRecoveredReissueCountsAsDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	reconciler := env
	reconciler.statusWrites(t, func(ctx context.Context, c client.Client, key *coderv1alpha1.CoderAIGatewayKey, opts ...client.SubResourceUpdateOption) error {
		if key.Status.CurrentKey != nil && key.Status.CurrentKey.Serial == 1 {
			return apierrors.NewServerTimeout(schema.GroupResource{Group: "coder.com", Resource: "coderaigatewaykeys"}, "update", 1)
		}
		return c.Status().Update(ctx, key, opts...)
	})
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: testAIGatewaySecret}}))
	_, _, err := reconciler.reconcileErr(ctx, t, got)
	require.True(t, apierrors.IsServerTimeout(err), "want the lost status write, got %v", err)

	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, int64(1), got.Status.CurrentKey.Serial, "recovery 4.4 adopted the re-issued key")
	require.Equal(t, env.clock.Now().Unix(), got.Status.LastDriftReissueTime.Unix())
	setOutputSecretData(ctx, t, env.namespace, nil)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "SecretConflict")
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey))
}
