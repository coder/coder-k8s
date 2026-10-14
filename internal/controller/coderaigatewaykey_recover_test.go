package controller_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func aiGatewayKeyPrefix(key *coderv1alpha1.CoderAIGatewayKey) string {
	return "ck8s-" + strings.ReplaceAll(string(key.UID), "-", "") + "-"
}

// statusWrites wraps the reconciler's client. Every status update of the
// object goes to write.
func (env *aiGatewayKeyEnv) statusWrites(t *testing.T, write func(ctx context.Context, c client.Client, key *coderv1alpha1.CoderAIGatewayKey, opts ...client.SubResourceUpdateOption) error) {
	t.Helper()
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if key, ok := obj.(*coderv1alpha1.CoderAIGatewayKey); ok {
				return write(ctx, c, key, opts...)
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	})
}

// Plan R1 and C3: a marker from an attempt that sent no create is cleared
// and counts one failure. The next create uses the next serial, so Coder
// never answers with a duplicate name.
func TestCoderAIGatewayKeyLeftoverMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	now := metav1.NewTime(env.clock.Now())
	key.Status.NextSerial, key.Status.LastCreateAttemptTime = 1, &now
	key.Status.PendingKey = &coderv1alpha1.CoderAIGatewayKeyPending{Serial: 0, Name: aiGatewayKeyPrefix(key) + "0", CreateAttemptTime: now}
	require.NoError(t, k8sClient.Status().Update(ctx, key))

	got, _ := env.reconcile(ctx, t, key)
	requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	require.Nil(t, got.Status.PendingKey)
	require.Equal(t, int32(1), got.Status.ConsecutiveIssueFailures, "a leftover marker counts toward the failure cap")
	require.Zero(t, env.fake.totalRequests()-env.fake.requestCount(routeAIGatewayKeys))

	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, aiGatewayKeyPrefix(key)+"1", got.Status.CurrentKey.Name)
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey))
	require.Zero(t, got.Status.ConsecutiveIssueFailures)
}

// Plan R4 and C6: the Secret write succeeds and the final status write
// fails, so the marker stays while the Secret holds the new key. The next
// reconcile takes that key as current (recovery 4.4) and creates nothing.
func TestCoderAIGatewayKeyLostStatusWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	reconciler := env
	reconciler.statusWrites(t, func(ctx context.Context, c client.Client, key *coderv1alpha1.CoderAIGatewayKey, opts ...client.SubResourceUpdateOption) error {
		if key.Status.CurrentKey != nil {
			return apierrors.NewServerTimeout(schema.GroupResource{Group: "coder.com", Resource: "coderaigatewaykeys"}, "update", 1)
		}
		return c.Status().Update(ctx, key, opts...)
	})
	got, _, err := reconciler.reconcileErr(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	require.True(t, apierrors.IsServerTimeout(err), "want the status write error, got %v", err)
	require.NotNil(t, got.Status.PendingKey, "the marker stays")
	require.Nil(t, got.Status.CurrentKey)
	keys := env.fake.aiGatewayKeys()
	require.Len(t, keys, 1)
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
	require.Equal(t, []byte(keys[0].plaintext), secret.Data["key"])

	// The spec changes before the recovery. The adopted key was issued for
	// the older spec, so the new trigger stays unhandled.
	got.Spec.Rotation.Trigger = "rotate-me"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Nil(t, got.Status.PendingKey)
	require.Equal(t, keys[0].id.String(), got.Status.CurrentKey.ID)
	require.Empty(t, got.Status.LastHandledTrigger)
	require.Less(t, got.Status.AppliedGeneration, got.Generation)
	require.Equal(t, int64(0), got.Status.CurrentKey.Serial)
	require.Zero(t, got.Status.ConsecutiveIssueFailures)
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey))
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))
}

// Plan 4.3 in-flight bound 1: the Secret writes and the final status write of
// an attempt run under the 90 s attempt deadline (the Secret writes also
// under their own 10 s bound).
func TestCoderAIGatewayKeyAttemptDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	var secretDeadline, statusDeadline time.Duration
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if deadline, ok := ctx.Deadline(); ok && !isDryRunUpdate(opts) {
				secretDeadline = time.Until(deadline)
			}
			return c.Update(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if key, ok := obj.(*coderv1alpha1.CoderAIGatewayKey); ok && key.Status.CurrentKey != nil {
				deadline, ok := ctx.Deadline()
				require.True(t, ok, "the final status write runs under the attempt deadline")
				statusDeadline = time.Until(deadline)
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	require.True(t, secretDeadline > 0 && secretDeadline <= 10*time.Second, "Secret write deadline %s", secretDeadline)
	require.True(t, statusDeadline > 30*time.Second && statusDeadline <= 90*time.Second, "status write deadline %s", statusDeadline)
}

// A Secret deleted while the key is being written: the write stops, counts a
// failure, and the new key is an orphan. After the settle window the sweep
// deletes it, and the next issue claims the Secret again.
func TestCoderAIGatewayKeySecretDeletedDuringWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	reconciler := env
	reconciler.secretWrites(t, func(ctx context.Context, c client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
		require.NoError(t, c.Delete(ctx, obj.DeepCopyObject().(client.Object)))
		return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), nil)
	})
	got, _ := reconciler.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	message := requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	require.Contains(t, message, "was deleted while the key was being written")
	require.Equal(t, int32(1), got.Status.ConsecutiveIssueFailures)
	require.Nil(t, got.Status.PendingKey)
	requireNoOutputSecret(ctx, t, env.namespace, testAIGatewaySecret)

	env.clock.Step(5 * time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, int64(1), got.Status.CurrentKey.Serial)
	require.Len(t, env.fake.aiGatewayKeys(), 1)
}

// Plan R15: a create whose answer was lost, then a late Secret write of that
// key (for example from a deposed leader). Inside the settle window nothing
// is deleted. Once the Secret holds the key, recovery 4.4 makes it current,
// and the sweep never touches it.
func TestCoderAIGatewayKeyLateSecretWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey), "the create gate sends no create while an orphan exists")

	orphan := env.fake.aiGatewayKeys()[0]
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
	secret.Data = map[string][]byte{"key": []byte(orphan.plaintext)}
	require.NoError(t, k8sClient.Update(ctx, secret))
	env.clock.Step(5 * time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, orphan.id.String(), got.Status.CurrentKey.ID)
	require.Equal(t, got.Generation, got.Status.AppliedGeneration, "the spec did not change since the attempt")
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey))
}

// Plan R18 and S4: while an orphan of this object is listed, the create gate
// sends no create. After the settle window the sweep deletes it (one DELETE
// per reconcile), then one key is issued. Keys of other objects, keys with
// serials this object never sent, and manual keys are never deleted.
func TestCoderAIGatewayKeyCreateGateAndSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	sdk := env.fake.client(t, 5*time.Second)
	foreign := []string{"ck8s-00000000000040008000000000000001-0", aiGatewayKeyPrefix(key) + "7", aiGatewayKeyPrefix(key) + "00", "carol"}
	for _, name := range foreign {
		_, err := sdk.CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: name})
		require.NoError(t, err)
	}
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	got, _ := env.reconcile(ctx, t, key)
	requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	creates := env.fake.requestCount(routeCreateAIGatewayKey)

	for range 3 {
		env.clock.Step(time.Minute)
		var result ctrl.Result
		got, result = env.reconcile(ctx, t, got)
		requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
		require.Positive(t, result.RequeueAfter)
	}
	require.Equal(t, creates, env.fake.requestCount(routeCreateAIGatewayKey), "no create while an orphan exists")
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey), "no delete inside the settle window")

	env.clock.Step(2 * time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, aiGatewayKeyPrefix(key)+"1", got.Status.CurrentKey.Name)

	env.clock.Step(time.Hour)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey))
	var names []string
	for _, k := range env.fake.aiGatewayKeys() {
		names = append(names, k.name)
	}
	require.ElementsMatch(t, append(foreign, aiGatewayKeyPrefix(key)+"1"), names)
}

// A sweep keeps Ready=True while the Secret holds the current key, also when
// the orphan's DELETE fails and is retried.
func TestCoderAIGatewayKeySweepKeepsReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	// An orphan with serial 1, as after a lost answer of a later create.
	_, err := env.fake.client(t, 5*time.Second).CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: aiGatewayKeyPrefix(got) + "1"})
	require.NoError(t, err)
	got.Status.NextSerial = 2
	require.NoError(t, k8sClient.Status().Update(ctx, got))
	env.clock.Step(5 * time.Minute)

	requireReadyWhile := func(key *coderv1alpha1.CoderAIGatewayKey, reason string) {
		t.Helper()
		for conditionType, status := range map[string]metav1.ConditionStatus{"Ready": metav1.ConditionTrue, "Reconciling": metav1.ConditionTrue, "Stalled": metav1.ConditionFalse} {
			condition := meta.FindStatusCondition(key.Status.Conditions, conditionType)
			require.Equal(t, status, condition.Status, conditionType)
			require.Equal(t, reason, condition.Reason, conditionType)
		}
	}
	env.fake.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusServiceUnavailable})
	got, _ = env.reconcile(ctx, t, got)
	requireReadyWhile(got, "CoderUnavailable")
	got, _ = env.reconcile(ctx, t, got)
	requireReadyWhile(got, "DeletingOrphans")
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Len(t, env.fake.aiGatewayKeys(), 1)
}

// Plan I1 at the sweep: a Secret that holds the orphan at the moment of the
// live read right before the DELETE keeps it.
func TestCoderAIGatewayKeySweepReadsSecretFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	orphan := env.fake.aiGatewayKeys()[0]

	// The second read of the output Secret in the reconcile is the sweep's
	// live read. It sees the orphan, as after a late write between the two.
	reads := 0
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	reconciler := env
	reconciler.client = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if secret, ok := obj.(*corev1.Secret); ok && key.Name == testAIGatewaySecret {
				if reads++; reads == 2 {
					secret.Data = map[string][]byte{"key": []byte(orphan.plaintext)}
				}
			}
			return nil
		},
	})
	env.clock.Step(5 * time.Minute)
	got, _ = reconciler.reconcile(ctx, t, got)
	message := requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	require.Contains(t, message, "so the operator does not delete it")
	require.Equal(t, 2, reads)
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))
}
