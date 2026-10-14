package controller_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

func watchClient(t *testing.T) client.WithWatch {
	t.Helper()
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return c
}

// aiGatewayKeyRecord returns the data of the cleanup record of uid in the
// env's operator namespace, or nil when there is none.
func (env aiGatewayKeyEnv) aiGatewayKeyRecord(ctx context.Context, t *testing.T, uid types.UID) map[string]string {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: env.records, Name: "ck8s-cleanup-" + string(uid)}, configMap)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	require.Equal(t, "CoderAIGatewayKey", configMap.Labels["coder.com/cleanup-record"])
	return configMap.Data
}

// #353 part 2, key half: before each create, the controller records which
// Coder deployment holds the keys, the name prefix that finds them, and the
// time of the create. The current key ID follows once Coder answered it. The
// record holds no key. A delete that a LIST confirms removes it.
func TestCoderAIGatewayKeyCleanupRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	want := map[string]string{
		"kind": "CoderAIGatewayKey", "uid": string(got.UID), "namespace": env.namespace, "name": got.Name,
		"deploymentID": env.fake.deploymentID, "nonce": aiGatewayKeyPrefix(got), "coderID": got.Status.CurrentKey.ID, "type": "",
		"createAttemptTime": got.Status.LastCreateAttemptTime.UTC().Format(time.RFC3339),
	}
	// reconcileErr checks that the record holds no key.
	require.Equal(t, want, env.aiGatewayKeyRecord(ctx, t, got.UID))

	// A rotation records its own create time first, and the new ID after.
	env.clock.Step(time.Minute)
	got.Spec.Rotation.Trigger = "rotate-1"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	require.Len(t, got.Status.RetiringKeys, 1)
	want["coderID"] = got.Status.CurrentKey.ID
	want["createAttemptTime"] = env.clock.Now().UTC().Format(time.RFC3339)
	require.Equal(t, want, env.aiGatewayKeyRecord(ctx, t, got.UID))

	require.NoError(t, k8sClient.Delete(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyStep(t, got, false, "DeletingKeys")
	require.NotNil(t, env.aiGatewayKeyRecord(ctx, t, got.UID), "the record stays while the keys exist")
	env.clock.Step(5 * time.Minute)
	gone, _ := env.reconcile(ctx, t, got)
	require.Nil(t, gone)
	require.Empty(t, env.fake.aiGatewayKeys())
	require.Nil(t, env.aiGatewayKeyRecord(ctx, t, got.UID), "a LIST confirmed the delete, so the record goes")
}

// #353 invariant 1: without a record, the controller sends no create, and it
// writes no create marker.
func TestCoderAIGatewayKeyNoCreateWithoutRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("record write fails", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
		env.client = interceptor.NewClient(watchClient(t), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return errors.New("injected record write failure")
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		_, _, err := env.reconcileErr(ctx, t, key)
		require.ErrorContains(t, err, "injected record write failure")
		require.Zero(t, env.fake.requestCount(routeCreateAIGatewayKey), "no create without a record")
		got := &coderv1alpha1.CoderAIGatewayKey{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(key), got))
		require.Zero(t, got.Status.NextSerial, "no create marker either")
	})
	t.Run("deployment ID unknown", func(t *testing.T) {
		t.Parallel()
		env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
		env.fake.failNext(routeBuildInfo, fakeFault{Status: http.StatusServiceUnavailable})
		got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
		requireAIGatewayKeyStep(t, got, false, "CoderUnavailable")
		require.Zero(t, env.fake.requestCount(routeCreateAIGatewayKey))
		require.Zero(t, got.Status.NextSerial)
		require.Nil(t, env.aiGatewayKeyRecord(ctx, t, got.UID))
	})
}

// #353 invariants 3 and 7: when the control plane reaches another Coder
// database, the record is the only locator of the keys in the old one. The
// controller creates no key in the new one and keeps the record, and a
// delete releases the finalizer without removing it.
func TestCoderAIGatewayKeyDeploymentChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	requireAIGatewayKeyReady(t, got)
	before := env.aiGatewayKeyRecord(ctx, t, got.UID)
	env.fake.mu.Lock()
	env.fake.deploymentID = uuid.NewString() // A new database behind the same URL.
	env.fake.mu.Unlock()

	got.Spec.Rotation.Trigger = "rotate-1"
	require.NoError(t, k8sClient.Update(ctx, got))
	creates := env.fake.requestCount(routeCreateAIGatewayKey)
	got, _ = env.reconcile(ctx, t, got)
	stalled := meta.FindStatusCondition(got.Status.Conditions, coderv1alpha1.CoderAIGatewayKeyConditionStalled)
	require.Equal(t, "CoderDeploymentChanged", stalled.Reason)
	require.Contains(t, stalled.Message, before["deploymentID"])
	require.Equal(t, creates, env.fake.requestCount(routeCreateAIGatewayKey), "no create in the new deployment")
	require.Equal(t, before, env.aiGatewayKeyRecord(ctx, t, got.UID))

	require.NoError(t, k8sClient.Delete(ctx, got))
	env.clock.Step(5 * time.Minute)
	deletes := env.fake.requestCount(routeDeleteAIGatewayKey)
	gone, _ := env.reconcile(ctx, t, got)
	require.Nil(t, gone, "the finalizer goes: a LIST in the new deployment proves nothing")
	require.Equal(t, deletes, env.fake.requestCount(routeDeleteAIGatewayKey))
	require.Equal(t, before, env.aiGatewayKeyRecord(ctx, t, got.UID), "the record stays for a sweep")
}

// #353 invariant 7 in the one bounded pass while the control plane goes: the
// pass deletes what its LIST shows, but inside the settle window a create can
// still commit after that LIST, so the record stays for a sweep. After the
// window, the pass proves that no key is left, and the record goes.
func TestCoderAIGatewayKeyRecordAfterBoundedPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, settled := range map[string]bool{"inside the settle window": false, "after the settle window": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
			got := rotatedKey(ctx, t, &env)
			require.NoError(t, k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.namespace}}))
			holdControlPlane(ctx, t, env.namespace)
			require.NoError(t, k8sClient.Delete(ctx, got))
			if settled {
				env.clock.Step(5 * time.Minute)
			}
			r := &controller.CoderAIGatewayKeyReconciler{Client: env.client, APIReader: env.client, Scheme: scheme, Clock: env.clock, OperatorNamespace: env.records}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
			require.NoError(t, err)
			require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got)), "the finalizer went")
			require.Empty(t, env.fake.aiGatewayKeys())
			if settled {
				require.Nil(t, env.aiGatewayKeyRecord(ctx, t, got.UID))
			} else {
				require.NotNil(t, env.aiGatewayKeyRecord(ctx, t, got.UID))
			}
		})
	}
}

// A record whose create marker write failed: no create was sent, so the
// delete removes the record with no Coder call.
func TestCoderAIGatewayKeyRecordWithoutCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	key := env.createKey(ctx, t, "coder", testAIGatewaySecret)
	failing := env
	failing.client = interceptor.NewClient(watchClient(t), interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("injected marker write failure")
		},
	})
	_, _, err := failing.reconcileErr(ctx, t, key)
	require.ErrorContains(t, err, "injected marker write failure")
	require.NotNil(t, env.aiGatewayKeyRecord(ctx, t, key.UID))
	require.Zero(t, env.fake.requestCount(routeCreateAIGatewayKey))

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(key), key))
	require.Zero(t, key.Status.NextSerial)
	require.NoError(t, k8sClient.Delete(ctx, key))
	requests := env.fake.totalRequests()
	gone, _ := env.reconcile(ctx, t, key)
	require.Nil(t, gone)
	require.Equal(t, requests, env.fake.totalRequests(), "no Coder call")
	require.Nil(t, env.aiGatewayKeyRecord(ctx, t, key.UID))
}

// An object from before records existed, or whose record lost its key ID,
// gets the record back once Coder lists its current key, with the time of
// its last create.
func TestCoderAIGatewayKeyRecordBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newAIGatewayKeyEnv(ctx, t, ptrTo(true))
	got, _ := env.reconcile(ctx, t, env.createKey(ctx, t, "coder", testAIGatewaySecret))
	want := env.aiGatewayKeyRecord(ctx, t, got.UID)
	require.NoError(t, k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.records, Name: "ck8s-cleanup-" + string(got.UID)}}))
	env.clock.Step(time.Minute)
	got, _ = env.reconcile(ctx, t, got)
	requireAIGatewayKeyReady(t, got)
	require.Equal(t, want, env.aiGatewayKeyRecord(ctx, t, got.UID))
}
