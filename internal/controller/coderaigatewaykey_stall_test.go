package controller_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
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

// readyWithOrphan issues the first key, then adds an orphan with serial 1,
// as after a lost answer of a later create, and waits out the settle window.
func readyWithOrphan(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv) (*coderv1alpha1.CoderAIGatewayKey, fakeAIGatewayKey) {
	t.Helper()
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	created, err := env.fake.client(t, 5*time.Second).CreateAIGatewayKey(ctx, codersdk.CreateAIGatewayKeyRequest{Name: aiGatewayKeyPrefix(got) + "1"})
	require.NoError(t, err)
	got.Status.NextSerial = 2
	require.NoError(t, k8sClient.Status().Update(ctx, got))
	env.clock.Step(5 * time.Minute)
	for _, k := range env.fake.aiGatewayKeys() {
		if k.id == created.ID {
			return got, k
		}
	}
	t.Fatalf("assertion failed: fake coder lost orphan %s", created.ID)
	return nil, fakeAIGatewayKey{}
}

func setOutputSecretData(ctx context.Context, t *testing.T, namespace string, data map[string][]byte) {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testAIGatewaySecret}, secret))
	secret.Data = data
	require.NoError(t, k8sClient.Update(ctx, secret))
}

// Plan 4.8, S4 and R9: every stall that K7 can reach keeps sweeping
// orphans in delete-only mode. The stall stays the reported condition, and
// no create runs. A Secret that the object does not own still decides I1.
func TestCoderAIGatewayKeyStallsKeepSweeping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	emptySecret := func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey, _ fakeAIGatewayKey) {
		setOutputSecretData(ctx, t, env.namespace, nil)
	}
	withStatus := func(edit func(*coderv1alpha1.CoderAIGatewayKeyStatus, *coderv1alpha1.CoderAIGatewayKey)) func(context.Context, *testing.T, *aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey, fakeAIGatewayKey) {
		return func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey, orphan fakeAIGatewayKey) {
			emptySecret(ctx, t, env, key, orphan)
			edit(&key.Status, key)
			require.NoError(t, k8sClient.Status().Update(ctx, key))
		}
	}
	foreignSecret := func(value func(fakeAIGatewayKey) string) func(context.Context, *testing.T, *aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey, fakeAIGatewayKey) {
		return func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey, orphan fakeAIGatewayKey) {
			require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: testAIGatewaySecret}}))
			createSecret(ctx, t, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: testAIGatewaySecret},
				Data:       map[string][]byte{"key": []byte(value(orphan))},
			})
		}
	}
	for name, tc := range map[string]struct {
		mutate  func(context.Context, *testing.T, *aiGatewayKeyEnv, *coderv1alpha1.CoderAIGatewayKey, fakeAIGatewayKey)
		reason  string
		deletes int
		gated   bool // The first reconcile sweeps without the stall.
	}{
		"not allowed": {reason: "NotAllowed", deletes: 1, mutate: func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, _ *coderv1alpha1.CoderAIGatewayKey, _ fakeAIGatewayKey) {
			cp := &coderv1alpha1.CoderControlPlane{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, cp))
			cp.Spec.AIGateway.AllowKeys = false
			require.NoError(t, k8sClient.Update(ctx, cp))
		}},
		"secret replaced": {reason: "SecretNotOwned", deletes: 1, mutate: foreignSecret(func(fakeAIGatewayKey) string { return "someone-else" })},
		// R9: the replaced Secret holds the orphan, so it is never deleted.
		"secret replaced with the orphan": {reason: "SecretNotOwned", deletes: 0, mutate: foreignSecret(func(k fakeAIGatewayKey) string { return k.plaintext })},
		"current key deleted": {reason: "KeyDeletedExternally", deletes: 1, mutate: func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey, _ fakeAIGatewayKey) {
			require.NoError(t, env.fake.client(t, 5*time.Second).DeleteAIGatewayKey(ctx, uuid.MustParse(key.Status.CurrentKey.ID)))
		}},
		"drift inside the grace period": {reason: "SecretConflict", deletes: 1, mutate: withStatus(func(s *coderv1alpha1.CoderAIGatewayKeyStatus, _ *coderv1alpha1.CoderAIGatewayKey) {
			s.LastDriftReissueTime = &metav1.Time{Time: time.Now()}
		})},
		"failure cap": {reason: "IssueFailing", deletes: 1, mutate: withStatus(func(s *coderv1alpha1.CoderAIGatewayKeyStatus, _ *coderv1alpha1.CoderAIGatewayKey) {
			s.ConsecutiveIssueFailures = 10
		})},
		"answer mismatch": {reason: "CoderAnswerMismatch", deletes: 1, mutate: withStatus(func(s *coderv1alpha1.CoderAIGatewayKeyStatus, key *coderv1alpha1.CoderAIGatewayKey) {
			s.AnswerMismatchGeneration = key.Generation
		})},
		// The re-issue waits behind the create gate, so the sweep runs before
		// the dry run.
		"secret write rejected": {reason: "SecretWriteRejected", deletes: 1, gated: true, mutate: func(ctx context.Context, t *testing.T, env *aiGatewayKeyEnv, key *coderv1alpha1.CoderAIGatewayKey, orphan fakeAIGatewayKey) {
			emptySecret(ctx, t, env, key, orphan)
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
			require.NoError(t, err)
			env.client = interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*corev1.Secret); ok && isDryRunUpdate(opts) {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, testAIGatewaySecret, errors.New("policy"))
					}
					return c.Update(ctx, obj, opts...)
				},
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got, orphan := readyWithOrphan(ctx, t, &env)
			tc.mutate(ctx, t, &env, got, orphan)
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
			creates, deletes := env.fake.requestCount(routeCreateAIGatewayKey), env.fake.requestCount(routeDeleteAIGatewayKey)

			got, _ = env.reconcile(ctx, t, got)
			if tc.gated {
				requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
			} else {
				requireAIGatewayKeyStep(t, got, true, tc.reason)
			}
			require.Equal(t, deletes+tc.deletes, env.fake.requestCount(routeDeleteAIGatewayKey))
			got, _ = env.reconcile(ctx, t, got)
			requireAIGatewayKeyStep(t, got, true, tc.reason)
			require.Equal(t, deletes+tc.deletes, env.fake.requestCount(routeDeleteAIGatewayKey), "one orphan, one DELETE")
			require.Equal(t, creates, env.fake.requestCount(routeCreateAIGatewayKey), "a stall creates nothing")
			stillListed := false
			for _, k := range env.fake.aiGatewayKeys() {
				stillListed = stillListed || k.id == orphan.id
			}
			require.Equal(t, tc.deletes == 0, stillListed)
		})
	}
}

// Plan R11 and D6: a key deleted in Coder stalls the object, and the stall
// creates nothing, also while an orphan's DELETE keeps failing. Only a spec
// change re-arms it, also when a failed LIST comes between the two.
func TestCoderAIGatewayKeyKillSwitch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, orphan := readyWithOrphan(ctx, t, &env)
	require.NoError(t, env.fake.client(t, 5*time.Second).DeleteAIGatewayKey(ctx, uuid.MustParse(got.Status.CurrentKey.ID)))
	creates := env.fake.requestCount(routeCreateAIGatewayKey)

	env.fake.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusForbidden})
	got, _ = env.reconcile(ctx, t, got)
	message := requireAIGatewayKeyStep(t, got, true, "KeyDeletedExternally")
	require.Contains(t, message, "cannot delete orphan key "+orphan.name)
	require.Equal(t, got.Generation, got.Status.KillSwitchGeneration)
	for range 2 {
		env.clock.Step(time.Hour)
		got, _ = env.reconcile(ctx, t, got)
		requireAIGatewayKeyStep(t, got, true, "KeyDeletedExternally")
	}
	require.Equal(t, creates, env.fake.requestCount(routeCreateAIGatewayKey))
	require.Len(t, env.fake.aiGatewayKeys(), 0, "the delete-only sweep removed the orphan")

	got.Spec.Rotation.Trigger = "re-arm"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.fake.failNext(routeAIGatewayKeys, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "CoderUnavailable")
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "IssuingKey")
	require.Nil(t, got.Status.CurrentKey)
	require.Zero(t, got.Status.KillSwitchGeneration)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, aiGatewayKeyPrefix(got)+"2", got.Status.CurrentKey.Name)
	require.Empty(t, got.Status.RetiringKeys, "Coder deleted the old key, so nothing retires")
	require.Equal(t, creates+1, env.fake.requestCount(routeCreateAIGatewayKey))

	// The re-armed key is deleted too, without a spec change: the kill
	// switch fires again instead of re-arming at once.
	require.NoError(t, env.fake.client(t, 5*time.Second).DeleteAIGatewayKey(ctx, uuid.MustParse(got.Status.CurrentKey.ID)))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "KeyDeletedExternally")
	require.Equal(t, creates+1, env.fake.requestCount(routeCreateAIGatewayKey))
}

// Plan R16 and D7: a lost Secret value gives one new key per grace period,
// and the replaced key retires. A second drift inside the grace period, or a
// full retiringKeys list, gives SecretConflict and no create. A Secret
// restored with an older key does not make that key current again.
func TestCoderAIGatewayKeySecretDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	first := env.fake.aiGatewayKeys()[0]
	requireHolds := func(serial int64) {
		t.Helper()
		require.Equal(t, serial, got.Status.CurrentKey.Serial)
		var want string
		for _, k := range env.fake.aiGatewayKeys() {
			if k.id.String() == got.Status.CurrentKey.ID {
				want = k.plaintext
			}
		}
		secret := &corev1.Secret{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: testAIGatewaySecret}, secret))
		require.Equal(t, want, string(secret.Data["key"]))
	}

	// Someone deletes the Secret: a new key, the old one retires from now.
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: testAIGatewaySecret}}))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	requireHolds(1)
	require.Equal(t, []string{first.id.String()}, retiringIDs(got))
	require.Equal(t, env.clock.Now().Unix(), got.Status.RetiringKeys[0].ReplacedAt.Unix())
	require.Equal(t, env.clock.Now().Unix(), got.Status.LastDriftReissueTime.Unix())

	// A second drift inside the grace period: no create, the Secret stays.
	env.clock.Step(59 * time.Minute)
	setOutputSecretData(ctx, t, env.namespace, map[string][]byte{"key": []byte("edited-by-someone")})
	var result ctrl.Result
	got, result = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "SecretConflict")
	require.Equal(t, time.Minute, result.RequeueAfter, "the stall ends with the grace period")
	require.Equal(t, 2, env.fake.requestCount(routeCreateAIGatewayKey))

	// After the grace period the Secret is restored with the first key. It
	// does not become current again: a new key replaces it.
	env.clock.Step(time.Minute)
	setOutputSecretData(ctx, t, env.namespace, map[string][]byte{"key": []byte(first.plaintext)})
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	requireHolds(2)
	env.clock.Step(time.Hour)
	setOutputSecretData(ctx, t, env.namespace, nil)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	requireHolds(3)
	require.Len(t, got.Status.RetiringKeys, 3)

	// retiringKeys is full: no create. The stall retires key 0, whose grace
	// period ended (delete-only mode), and the freed slot allows the next
	// re-issue.
	env.clock.Step(2 * time.Hour)
	setOutputSecretData(ctx, t, env.namespace, nil)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "SecretConflict")
	require.NotContains(t, retiringIDs(got), first.id.String())
	require.Equal(t, 4, env.fake.requestCount(routeCreateAIGatewayKey))
	require.Equal(t, 1, env.fake.requestCount(routeDeleteAIGatewayKey))
	require.Len(t, got.Status.RetiringKeys, 2)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	requireHolds(4)
	require.Len(t, got.Status.RetiringKeys, 3)
}

func retiringIDs(key *coderv1alpha1.CoderAIGatewayKey) []string {
	var ids []string
	for _, r := range key.Status.RetiringKeys {
		ids = append(ids, r.ID)
	}
	return ids
}

// Carry-forwards from #292. An orphan whose DELETE Coder refuses with 403
// stalls the object instead of retrying in silence. A 5xx retries. Without
// lastCreateAttemptTime the settle window counts from the reconcile that
// sees the orphan.
func TestCoderAIGatewayKeyUndeletableOrphan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	env.fake.failNext(routeCreateAIGatewayKey, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	got.Status.LastCreateAttemptTime = nil
	require.NoError(t, k8sClient.Status().Update(ctx, got))
	env.clock.Step(time.Hour)

	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	require.Equal(t, env.clock.Now().Unix(), got.Status.LastCreateAttemptTime.Unix(), "the window starts now")
	require.Zero(t, env.fake.requestCount(routeDeleteAIGatewayKey))

	env.clock.Step(5 * time.Minute)
	env.fake.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusForbidden})
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, true, "CoderRejected")
	env.fake.failNext(routeDeleteAIGatewayKey, fakeFault{Status: http.StatusInternalServerError})
	var result ctrl.Result
	got, result = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "CoderUnavailable")
	require.Equal(t, 30*time.Second, result.RequeueAfter)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingOrphans")
	require.Equal(t, 3, env.fake.requestCount(routeDeleteAIGatewayKey))
	require.Empty(t, env.fake.aiGatewayKeys())
	require.Equal(t, 1, env.fake.requestCount(routeCreateAIGatewayKey))
}

// A DELETE that Coder refuses with 403, or with 404 "Route not found.",
// while the Secret holds the current key reports both facts: the key works
// (Ready=True, and the message says so), and a person must act on the
// orphan (Stalled=True).
func TestCoderAIGatewayKeyUndeletableOrphanWhileReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		fault  fakeFault
		reason string
	}{
		"forbidden":       {fault: fakeFault{Status: http.StatusForbidden}, reason: "CoderRejected"},
		"route not found": {fault: fakeFault{Status: http.StatusNotFound, Message: "Route not found."}, reason: "NotSupported"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got, orphan := readyWithOrphan(ctx, t, &env)
			env.fake.failNext(routeDeleteAIGatewayKey, tc.fault)
			got, _ = env.reconcile(ctx, t, got)
			for conditionType, status := range map[string]metav1.ConditionStatus{"Ready": metav1.ConditionTrue, "Reconciling": metav1.ConditionFalse, "Stalled": metav1.ConditionTrue} {
				condition := meta.FindStatusCondition(got.Status.Conditions, conditionType)
				require.Equal(t, status, condition.Status, conditionType)
				require.Equal(t, tc.reason, condition.Reason, conditionType)
				require.Contains(t, condition.Message, "still works")
				require.Contains(t, condition.Message, "cannot delete orphan key "+orphan.name)
			}
			require.Len(t, env.fake.aiGatewayKeys(), 2, "the orphan stays")
			got, _ = env.reconcile(ctx, t, got)
			stalled := meta.FindStatusCondition(got.Status.Conditions, "Stalled")
			require.Equal(t, metav1.ConditionFalse, stalled.Status, "the next DELETE succeeds")
			require.Len(t, env.fake.aiGatewayKeys(), 1)
		})
	}
}
