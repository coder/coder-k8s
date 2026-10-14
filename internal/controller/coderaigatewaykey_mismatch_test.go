package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// answerMismatch is a create answer whose key prefix does not match its key.
func answerMismatch() fakeFault {
	return fakeFault{Rewrite: func(a any) any {
		answer := a.(codersdk.CreateAIGatewayKeyResponse)
		answer.KeyPrefix = "00000000000"
		return answer
	}}
}

// requireMismatchStalled checks that the CoderAnswerMismatch gate is the
// reported stall, with or without Ready=True.
func requireMismatchStalled(t *testing.T, key *coderv1alpha1.CoderAIGatewayKey) {
	t.Helper()
	stalled := meta.FindStatusCondition(key.Status.Conditions, "Stalled")
	require.NotNil(t, stalled)
	require.Equal(t, metav1.ConditionTrue, stalled.Status)
	require.Equal(t, "CoderAnswerMismatch", stalled.Reason)
}

func (env *aiGatewayKeyEnv) controlPlane(ctx context.Context, t *testing.T, edit func(*coderv1alpha1.CoderControlPlane) error) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, cp))
	require.NoError(t, edit(cp))
}

func editStatus(ctx context.Context, t *testing.T, key *coderv1alpha1.CoderAIGatewayKey, edit func(*coderv1alpha1.CoderAIGatewayKeyStatus)) {
	t.Helper()
	edit(&key.Status)
	require.NoError(t, k8sClient.Status().Update(ctx, key))
}

// Plan 4.3 step 5: status keeps the CoderAnswerMismatch gate, so an early
// return that reports another condition (a failed LIST, a control plane
// wait, a stall found before the LIST, SecretConflict) does not re-arm the
// create. The re-issue path runs here, because SecretConflict needs a
// current key.
func TestCoderAIGatewayKeyAnswerMismatchSurvivesEarlyReturns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	type change func(context.Context, *testing.T, *aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey)
	operatorAccess := func(ready bool) change {
		return func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey) {
			env.controlPlane(ctx, t, func(cp *coderv1alpha1.CoderControlPlane) error {
				cp.Status.OperatorAccessReady = ready
				return k8sClient.Status().Update(ctx, cp)
			})
		}
	}
	allowKeys := func(allow bool) change {
		return func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey) {
			env.controlPlane(ctx, t, func(cp *coderv1alpha1.CoderControlPlane) error {
				cp.Spec.AIGateway.AllowKeys = allow
				return k8sClient.Update(ctx, cp)
			})
		}
	}
	driftReissued := func(at *metav1.Time) change {
		return func(ctx context.Context, t *testing.T, _ *aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey) {
			editStatus(ctx, t, key, func(s *coderv1alpha1.CoderAIGatewayKeyStatus) { s.LastDriftReissueTime = at })
		}
	}
	outputSecret := func(env *aiGatewayKeyEnv) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: testAIGatewaySecret}}
	}
	for name, tc := range map[string]struct {
		interrupt, undo change
		stalled         bool
		reason          string
	}{
		"failed LIST": {reason: "CoderUnavailable", interrupt: func(_ context.Context, _ *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey) {
			env.fake.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusBadGateway})
		}, undo: func(context.Context, *testing.T, *aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey) {}},
		"control plane wait": {reason: "OperatorAccessNotReady", interrupt: operatorAccess(false), undo: operatorAccess(true)},
		"not allowed":        {stalled: true, reason: "NotAllowed", interrupt: allowKeys(false), undo: allowKeys(true)},
		"secret not owned": {stalled: true, reason: "SecretNotOwned", interrupt: func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey) {
			require.NoError(t, k8sClient.Delete(ctx, outputSecret(env)))
			foreign := outputSecret(env)
			foreign.Data = map[string][]byte{"key": []byte("someone-else")}
			createSecret(ctx, t, foreign)
		}, undo: func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey) {
			require.NoError(t, k8sClient.Delete(ctx, outputSecret(env)))
		}},
		"secret conflict": {stalled: true, reason: "SecretConflict", interrupt: driftReissued(&metav1.Time{Time: time.Now()}), undo: driftReissued(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
			requireAIGatewayKeyReady(t, got)
			setOutputSecretData(ctx, t, env.namespace, nil)
			env.fake.failNext(routeCreateAIGatewayKey, answerMismatch())
			got, _ = env.reconcile(ctx, t, got)
			requireMismatchStalled(t, got)
			// A create after the early return would get a wrong answer again.
			env.fake.failNext(routeCreateAIGatewayKey, answerMismatch())

			tc.interrupt(ctx, t, &env, got)
			got, _ = env.reconcile(ctx, t, got)
			requireAIGatewayKeyStep(t, got, tc.stalled, tc.reason)
			tc.undo(ctx, t, &env, got)
			for _, step := range []time.Duration{0, 5 * time.Minute, 0} { // The sweep runs at the end of the settle window.
				env.clock.Step(step)
				got, _ = env.reconcile(ctx, t, got)
				requireAIGatewayKeyStep(t, got, true, "CoderAnswerMismatch")
			}
			require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey), "no create after the mismatch")
			require.Len(t, env.fake.aiGatewayKeys(), 1, "the sweep deleted the mismatched key")
		})
	}
}

// A LIST that fails between reconciles, while Coder keeps answering creates
// with a wrong key, creates no key per settle window: one create on each
// path (first issue, re-issue, rotation) until the spec changes.
func TestCoderAIGatewayKeyAnswerMismatchWithFlappingList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		arm   func(context.Context, *testing.T, *aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey)
		ready bool // The path starts from a Ready key.
		keys  int  // Keys in Coder at the end.
	}{
		"first issue": {arm: func(context.Context, *testing.T, *aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey) {}},
		"re-issue": {ready: true, keys: 1, arm: func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey) {
			setOutputSecretData(ctx, t, env.namespace, nil)
		}},
		"rotation": {ready: true, keys: 1, arm: func(ctx context.Context, t *testing.T, _ *aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey) {
			key.Spec.Rotation.Trigger = "rotate-1"
			require.NoError(t, k8sClient.Update(ctx, key))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got := env.createKey(ctx, t, "coder", testAIGatewaySecret)
			if tc.ready {
				got, _ = env.reconcile(ctx, t, got)
				requireAIGatewayKeyReady(t, got)
			}
			tc.arm(ctx, t, &env, got)
			env.fake.failNext(routeCreateAIGatewayKey, answerMismatch())
			got, _ = env.reconcile(ctx, t, got)
			requireMismatchStalled(t, got)
			creates := env.fake.requestCount(routeCreateAIGatewayKey)

			for range 3 {
				env.fake.failNext(routeCreateAIGatewayKey, answerMismatch())
				env.fake.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusBadGateway})
				got, _ = env.reconcile(ctx, t, got)
				requireAIGatewayKeyStep(t, got, false, "CoderUnavailable")
				for _, step := range []time.Duration{0, 5 * time.Minute} {
					env.clock.Step(step)
					got, _ = env.reconcile(ctx, t, got)
					requireMismatchStalled(t, got)
				}
			}
			require.Equal(t, creates, env.fake.requestCount(routeCreateAIGatewayKey), "no create per settle window")
			require.Len(t, env.fake.aiGatewayKeys(), tc.keys)
		})
	}
}

// A kill-switch re-arm issues a key behind the same gate: a mismatched
// answer to the re-armed create blocks every later create in that
// generation, also across a failed LIST and the settle window.
func TestCoderAIGatewayKeyRearmKeepsAnswerMismatchGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	require.NoError(t, env.fake.client(t, 5*time.Second).DeleteAIGatewayKey(ctx, uuid.MustParse(got.Status.CurrentKey.ID)))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "KeyDeletedExternally")

	got.Spec.Rotation.Trigger = "re-arm"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got) // The re-arm clears the current key.
	env.fake.failNext(routeCreateAIGatewayKey, answerMismatch())
	got, _ = env.reconcile(ctx, t, got)
	requireMismatchStalled(t, got)
	require.Equal(t, got.Generation, got.Status.AnswerMismatchGeneration)
	creates := env.fake.requestCount(routeCreateAIGatewayKey)

	env.fake.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "CoderUnavailable")
	for _, step := range []time.Duration{0, 5 * time.Minute, 0} {
		env.clock.Step(step)
		got, _ = env.reconcile(ctx, t, got)
		requireAIGatewayKeyStep(t, got, true, "CoderAnswerMismatch")
	}
	require.Equal(t, creates, env.fake.requestCount(routeCreateAIGatewayKey), "no create after the mismatch")
	require.Empty(t, env.fake.aiGatewayKeys(), "the sweep deleted the mismatched key")
}
