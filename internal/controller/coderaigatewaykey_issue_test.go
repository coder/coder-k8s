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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// secretWrites wraps the reconciler's client. Every Secret update that is not
// a dry run goes to write, which can fail it, run it, or both.
func (env *aiGatewayKeyEnv) secretWrites(t *testing.T, write func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error) {
	t.Helper()
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && !isDryRunUpdate(opts) {
				return write(ctx, c, obj, opts...)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
}

func requireOutputSecretEmpty(ctx context.Context, t *testing.T, namespace string) {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testAIGatewaySecret}, secret))
	require.Empty(t, secret.Data)
}

// Plan 4.3 outcome classes: every failed create clears the marker and keeps
// the serial used. No-effect answers count nothing. Uncertain answers count
// one failure, and a committed key stays an orphan for the sweep. The Secret
// never receives a key, and no key leaks (plan S3, checked by reconcile).
func TestCoderAIGatewayKeyCreateOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		setup    func(*fakeCoder)
		stalled  bool
		reason   string
		failures int32
		keys     int
	}{
		"rate limited":   {setup: func(f *fakeCoder) { f.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusTooManyRequests}) }, reason: "CoderUnavailable"},
		"token rejected": {setup: func(f *fakeCoder) { f.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusUnauthorized}) }, reason: "CoderUnavailable"},
		"not site owner": {setup: func(f *fakeCoder) { f.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusForbidden}) }, stalled: true, reason: "CoderRejected"},
		// The LIST passes, then the license or the route goes away.
		"no license": {setup: func(f *fakeCoder) {
			f.failNext(routeAIGatewayKeys, fakeFault{Rewrite: func(a any) any { f.aiGateway.featureOff = true; return a }})
		}, stalled: true, reason: "LicenseRequired"},
		"route missing": {setup: func(f *fakeCoder) {
			f.failNext(routeAIGatewayKeys, fakeFault{Rewrite: func(a any) any { f.aiGateway.routesMissing = true; return a }})
		}, stalled: true, reason: "NotSupported"},
		"server error after commit": {
			setup: func(f *fakeCoder) {
				f.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
			},
			reason: "IssuingKey", failures: 1, keys: 1,
		},
		"reset after commit": {
			setup:  func(f *fakeCoder) { f.failNext(routeCreateAIGatewayKey, fakeFault{Reset: true, AfterCommit: true}) },
			reason: "IssuingKey", failures: 1, keys: 1,
		},
		"HTML 201 after commit": {
			setup: func(f *fakeCoder) {
				f.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusCreated, HTML: "<html>proxy</html>", AfterCommit: true})
			},
			reason: "IssuingKey", failures: 1, keys: 1,
		},
		// The body holds the key, and the SDK error's detail holds the body.
		"200 with the key": {setup: func(f *fakeCoder) { f.answerNextAIGatewayKeyCreateWith(http.StatusOK) }, reason: "IssuingKey", failures: 1, keys: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			tc.setup(env.fake)
			got, result := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
			requireAIGatewayKeyStep(t, got, tc.stalled, tc.reason)
			require.Positive(t, result.RequeueAfter)
			require.Nil(t, got.Status.PendingKey)
			require.Nil(t, got.Status.CurrentKey)
			require.Equal(t, int64(1), got.Status.NextSerial, "a used serial is never reused")
			require.Equal(t, tc.failures, got.Status.ConsecutiveIssueFailures)
			require.Len(t, env.fake.aiGatewayKeys(), tc.keys)
			require.LessOrEqual(t, env.fake.requestCount(routeCreateAIGatewayKey), 1)
			requireOutputSecretEmpty(ctx, t, env.namespace)
		})
	}
}

// A key with the next name already exists: the create gets Coder's duplicate
// answer, and the next reconcile issues the next serial.
func TestCoderAIGatewayKeyDuplicateName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	prefix := "ck8s-" + strings.ReplaceAll(string(key.UID), "-", "") + "-"
	_, err := env.fake.client(t, 5*time.Second).CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: prefix + "0"})
	require.NoError(t, err)

	got, result := env.reconcile(ctx, t, key)
	requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	require.Equal(t, time.Second, result.RequeueAfter)
	require.Zero(t, got.Status.ConsecutiveIssueFailures)
	require.Equal(t, int64(1), got.Status.NextSerial)

	// The key with the used name is an orphan now: the create gate waits
	// for the settle window, the sweep deletes it, then serial 1 is issued.
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey), "the test's create and one from the controller")
	env.clock.Step(5 * time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, prefix+"1", got.Status.CurrentKey.Name)
	require.Len(t, env.fake.aiGatewayKeys(), 1)
}

// Plan 4.3 step 5: an answer that does not match the request stalls with
// CoderAnswerMismatch, writes no key into the Secret, and stays stalled
// after a restart until the spec changes.
func TestCoderAIGatewayKeyAnswerMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Rewrite: func(a any) any {
		answer := a.(codersdk.CreateAIGatewayKeyResponse)
		answer.KeyPrefix = "00000000000"
		return answer
	}})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	message := requireAIGatewayKeyStep(t, got, true, "CoderAnswerMismatch")
	require.Contains(t, message, env.fake.aiGatewayKeys()[0].id.String())
	requireOutputSecretEmpty(ctx, t, env.namespace)

	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "CoderAnswerMismatch")
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey))

	// A spec change re-arms the issue step. The mismatched key is an orphan,
	// so the sweep deletes it after the settle window first.
	got.Spec.Rotation.Trigger = "after-the-fix"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.clock.Step(5 * time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, int64(2), got.Status.AppliedGeneration, "the issue applied the changed spec")
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey))
}

// Plan R12: a marker write that gets a conflict sends no create.
func TestCoderAIGatewayKeyMarkerConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if key, ok := obj.(*coderv1alpha1.CoderAIGatewayKey); ok && key.Status.PendingKey != nil {
				return apierrors.NewConflict(schema.GroupResource{Group: "coder.com", Resource: "coderaigatewaykeys"}, key.Name, nil)
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	})
	got, _, err := env.reconcileErr(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	require.True(t, apierrors.IsConflict(err), "want a conflict, got %v", err)
	require.Zero(t, env.fake.requestCount(routeCreateAIGatewayKey))
	require.Zero(t, got.Status.NextSerial)
}

// Plan R14: ten failed Secret writes in a row stall with IssueFailing, and
// the controller sends no create until the spec changes. A rejected write
// reports the reason, never the API server's message.
func TestCoderAIGatewayKeyIssueFailingCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	env.secretWrites(t, func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, testAIGatewaySecret, errServerText)
	})
	got := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	for i := int32(1); i < 10; i++ {
		if i > 1 {
			// The failed attempt left an orphan: after the settle window
			// the sweep deletes it, and only then the next create runs.
			env.clock.Step(5 * time.Minute)
			got, _ = env.reconcile(ctx, t, got)
			requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
		}
		got, _ = env.reconcile(ctx, t, got)
		message := requireAIGatewayKeyStep(t, got, false, "IssuingKey")
		require.Contains(t, message, "Forbidden, HTTP 403")
		require.NotContains(t, message, errServerText.Error())
		require.Equal(t, i, got.Status.ConsecutiveIssueFailures)
	}
	env.clock.Step(5 * time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "IssueFailing")
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "IssueFailing")
	require.Equal(t, 10, env.fake.requestCount(routeCreateAIGatewayKey))
	require.Equal(t, 9, env.fake.requestCount(routeDeleteAIGatewayKey))
	requireOutputSecretEmpty(ctx, t, env.namespace)

	got.Spec.Rotation.Trigger = "retry"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.clock.Step(5 * time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	got, _ = env.reconcile(ctx, t, got)
	require.Equal(t, int32(1), got.Status.ConsecutiveIssueFailures, "a spec change re-arms the issue step")
	require.Equal(t, 11, env.fake.requestCount(routeCreateAIGatewayKey))
}

var errServerText = apierrors.NewBadRequest("server-text-must-not-leak")

// Plan R17: when the create answer arrives 2 minutes or more after the marker
// write, the controller writes nothing into the Secret.
func TestCoderAIGatewayKeyLateCreateAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	writes := 0
	env.secretWrites(t, func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		writes++
		return c.Update(ctx, obj, opts...)
	})
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Rewrite: func(a any) any { env.clock.Step(2 * time.Minute); return a }})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	message := requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	require.Contains(t, message, "took too long")
	require.Zero(t, writes)
	require.Len(t, env.fake.aiGatewayKeys(), 1)
	require.Equal(t, int32(1), got.Status.ConsecutiveIssueFailures)
	requireOutputSecretEmpty(ctx, t, env.namespace)
}

// A marker write that takes 2 minutes uses up the time for Secret writes:
// the write-age limit counts from the marker's timestamp, the same instant
// that starts the settle window.
func TestCoderAIGatewayKeySlowMarkerWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	writes := 0
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	env.client = interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && !isDryRunUpdate(opts) {
				writes++
			}
			return c.Update(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if key, ok := obj.(*coderv1alpha1.CoderAIGatewayKey); ok && key.Status.PendingKey != nil {
				env.clock.Step(2 * time.Minute)
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	require.Contains(t, requireAIGatewayKeyStep(t, got, false, "IssuingKey"), "took too long")
	require.Zero(t, writes)
	requireOutputSecretEmpty(ctx, t, env.namespace)
}

// Plan 4.3 step 6: a Secret write whose answer is lost is resolved by the
// next live read, so the key is not written twice and the issue succeeds.
func TestCoderAIGatewayKeyUncertainSecretWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	writes := 0
	env.secretWrites(t, func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		writes++
		if err := c.Update(ctx, obj, opts...); err != nil {
			return err
		}
		return apierrors.NewTimeoutError("answer lost", 1)
	})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, 1, writes)
	require.Zero(t, got.Status.ConsecutiveIssueFailures)
}
