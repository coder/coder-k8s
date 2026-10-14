package controller_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder/v2/codersdk"
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
	// The status that the marker write stored, read while Coder answers.
	marker := make(chan metav1.Condition, 1)
	name := client.ObjectKeyFromObject(got)
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Rewrite: func(a any) any {
		stored := &coderv1alpha1.CoderAIGatewayKey{}
		if err := k8sClient.Get(ctx, name, stored); err == nil && stored.Status.PendingKey != nil {
			marker <- *meta.FindStatusCondition(stored.Status.Conditions, "Ready")
		}
		return a
	}})
	got, _ = env.reconcile(ctx, t, got)
	require.Len(t, marker, 1, "the create ran after the marker write")
	ready := <-marker
	require.Equal(t, metav1.ConditionTrue, ready.Status, "the marker write keeps Ready=True")
	require.Equal(t, "IssuingKey", ready.Reason)
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

// requireReadyStalled checks that the Secret's key still works (Ready=True)
// while a person must act (Stalled=True), and returns the message.
func requireReadyStalled(t *testing.T, key *coderv1alpha1.CoderAIGatewayKey, reason string) string {
	t.Helper()
	for conditionType, status := range map[string]metav1.ConditionStatus{"Ready": metav1.ConditionTrue, "Reconciling": metav1.ConditionFalse, "Stalled": metav1.ConditionTrue} {
		condition := meta.FindStatusCondition(key.Status.Conditions, conditionType)
		require.NotNil(t, condition, conditionType)
		require.Equal(t, status, condition.Status, conditionType)
		require.Equal(t, reason, condition.Reason, conditionType)
	}
	message := meta.FindStatusCondition(key.Status.Conditions, "Ready").Message
	require.Equal(t, 1, strings.Count(message, "still works"), message)
	return message
}

// Plan 4.3 step 5 during a rotation: a create answer that does not match
// keeps the old key Ready, stalls with CoderAnswerMismatch, and stays
// stalled while the sweep deletes the mismatched key (delete-only mode). No
// further create runs until the spec changes, for a requested and for a
// scheduled rotation.
func TestCoderAIGatewayKeyAnswerMismatchDuringRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, arm := range map[string]func(*aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey){
		"requested": func(_ *aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey) {
			key.Spec.Rotation.Trigger = "rotate-1"
			require.NoError(t, k8sClient.Update(ctx, key))
		},
		"scheduled": func(env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey) { env.clock.Step(24 * time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
			key.Spec.Rotation.IntervalSeconds = ptrTo(int32(86400))
			require.NoError(t, k8sClient.Update(ctx, key))
			got, _ := env.reconcile(ctx, t, key)
			requireAIGatewayKeyReady(t, got)
			first := got.Status.CurrentKey

			arm(&env, got)
			env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Rewrite: func(a any) any {
				answer := a.(codersdk.CreateAIGatewayKeyResponse)
				answer.KeyPrefix = "00000000000"
				return answer
			}})
			for _, step := range []time.Duration{0, 0, 5 * time.Minute, time.Hour} {
				env.clock.Step(step)
				got, _ = env.reconcile(ctx, t, got)
				requireReadyStalled(t, got, "CoderAnswerMismatch")
				require.Equal(t, first.ID, got.Status.CurrentKey.ID)
			}
			require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey), "no create after the mismatch")
			require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey), "the sweep deleted the mismatched key")
			require.Len(t, env.fake.aiGatewayKeys(), 1)

			got.Spec.Rotation.Trigger = "after-the-fix"
			require.NoError(t, k8sClient.Update(ctx, got))
			got, _ = env.reconcile(ctx, t, got)
			require.Equal(t, int64(2), got.Status.CurrentKey.Serial, "the spec change re-arms the rotation")
			require.Equal(t, 3, env.fake.requestCount(routeCreateAIGatewayKey))
		})
	}
}

// A due rotation keeps an issue stall that blocks its create: the failure
// cap during a scheduled rotation, and CoderAnswerMismatch while retiring
// keys block a requested rotation. Ready=True and Stalled=True hold while the
// retire step frees the slots, and no create runs.
func TestCoderAIGatewayKeyRotationKeepsIssueStall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("failure cap", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
		key.Spec.Rotation.IntervalSeconds = ptrTo(int32(86400))
		require.NoError(t, k8sClient.Update(ctx, key))
		got, _ := env.reconcile(ctx, t, key)
		requireAIGatewayKeyReady(t, got)
		editStatus(ctx, t, got, func(s *coderv1alpha1.CoderAIGatewayKeyStatus) { s.ConsecutiveIssueFailures = 10 })
		for _, step := range []time.Duration{24 * time.Hour, 0, 5 * time.Minute} {
			env.clock.Step(step)
			got, _ = env.reconcile(ctx, t, got)
			requireReadyStalled(t, got, "IssueFailing")
		}
		require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey))
	})
	t.Run("answer mismatch while keys retire", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
		for _, trigger := range []string{"r1", "r2", "r3"} {
			got.Spec.Rotation.Trigger = trigger
			require.NoError(t, k8sClient.Update(ctx, got))
			got, _ = env.reconcile(ctx, t, got)
		}
		requireReadyWhile(t, got, "RotationBlocked")
		editStatus(ctx, t, got, func(s *coderv1alpha1.CoderAIGatewayKeyStatus) { s.AnswerMismatchGeneration = got.Generation })
		got, _ = env.reconcile(ctx, t, got)
		requireReadyStalled(t, got, "CoderAnswerMismatch")
		env.clock.Step(time.Hour)
		for range 3 { // Two retire steps, then the rotation would run.
			got, _ = env.reconcile(ctx, t, got)
			requireReadyStalled(t, got, "CoderAnswerMismatch")
		}
		require.Empty(t, got.Status.RetiringKeys)
		require.Equal(t, 3, env.fake.requestCount(routeCreateAIGatewayKey))
		require.Equal(t, 2, env.fake.requestCount(routeDeleteAIGatewayKey))
	})
}

// A rotation whose Secret write finds the Secret deleted or replaced reports
// Ready=False at once: the Secret no longer holds the current key.
func TestCoderAIGatewayKeyRotationSecretChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, replace := range map[string]bool{"deleted": false, "replaced": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
			requireAIGatewayKeyReady(t, got)
			got.Spec.Rotation.Trigger = "rotate-1"
			require.NoError(t, k8sClient.Update(ctx, got))
			env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Rewrite: func(a any) any {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: testAIGatewaySecret}}
				if err := k8sClient.Delete(ctx, secret); err != nil {
					t.Error(err)
				}
				if replace {
					foreign := &corev1.Secret{ObjectMeta: secret.ObjectMeta, Data: map[string][]byte{"key": []byte("someone-else")}}
					if err := k8sClient.Create(ctx, foreign); err != nil {
						t.Error(err)
					}
				}
				return a
			}})
			got, _ = env.reconcile(ctx, t, got)
			ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			require.Equal(t, metav1.ConditionFalse, ready.Status, ready.Message)
			require.NotContains(t, ready.Message, "still works")
			require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey))
		})
	}
}
