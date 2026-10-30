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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// deleteMCPServer deletes the object and runs one reconcile. It returns the
// object, or nil when the finalizer went and the object is gone.
func (env mcpServerEnv) deleteMCPServer(ctx context.Context, t *testing.T, server *coderv1alpha1.CoderMCPServer) (*coderv1alpha1.CoderMCPServer, ctrl.Result) {
	t.Helper()
	require.NoError(t, client.IgnoreNotFound(k8sClient.Delete(ctx, server)))
	return env.reconcileDeleting(ctx, t, server)
}

func (env mcpServerEnv) reconcileDeleting(ctx context.Context, t *testing.T, server *coderv1alpha1.CoderMCPServer) (*coderv1alpha1.CoderMCPServer, ctrl.Result) {
	t.Helper()
	result, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)})
	require.NoError(t, err)
	got := &coderv1alpha1.CoderMCPServer{}
	err = k8sClient.Get(ctx, client.ObjectKeyFromObject(server), got)
	if apierrors.IsNotFound(err) {
		return nil, result
	}
	require.NoError(t, err)
	return got, result
}

// Plan 4.10 items 3 and 6: deleting the object deletes the config that it
// created, also when the control plane no longer allows the organization,
// and then releases the finalizer.
func TestCoderMCPServerDeleteDeletesTheConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		withdraw bool
	}{{name: "opted in"}, {name: "opt-in withdrawn", withdraw: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, id := env.applied(ctx, t, nil)
			if tc.withdraw {
				env.setOptIn(ctx, t, nil)
			}
			mark := len(env.writeRoutes())
			gone, _ := env.deleteMCPServer(ctx, t, got)
			require.Nil(t, gone, "the finalizer went")
			require.Empty(t, env.fake.mcpServers())
			require.Equal(t, 1, env.fake.requestCount(routeDeleteMCPServer))
			require.Empty(t, env.writesSince(mark), "no write other than the DELETE")
			require.NotContains(t, configIDs(env), id)
		})
	}
}

// Plan 4.10 item 3: a config that is already gone (Coder's own 404) releases
// the finalizer. Any other 404 keeps it and retries (#337), as do other
// errors (plan 4.10 item 4).
func TestCoderMCPServerDeleteAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer)
		fault   *fakeFault
		release bool
	}{
		{name: "gone in Coder", release: true, prepare: func(ctx context.Context, t *testing.T, env mcpServerEnv, got *coderv1alpha1.CoderMCPServer) {
			require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, env.requireCreated(t, got).view.ID))
		}},
		{name: "proxy 404", fault: ptrTo(notCoder404s["HTML page"])},
		{name: "route not found", fault: ptrTo(notCoder404s["route not found"])},
		{name: "Coder unavailable", fault: &fakeFault{Status: http.StatusBadGateway}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, _ := env.applied(ctx, t, nil)
			if tc.prepare != nil {
				tc.prepare(ctx, t, env, got)
			}
			if tc.fault != nil {
				env.fake.failNext(routeDeleteMCPServer, *tc.fault)
			}
			kept, result := env.deleteMCPServer(ctx, t, got)
			if tc.release {
				require.Nil(t, kept)
				return
			}
			require.NotNil(t, kept)
			require.Equal(t, []string{coderv1alpha1.CoderMCPServerCleanupFinalizer}, kept.Finalizers)
			requireMCPServerStep(t, kept, false, "CoderUnavailable")
			require.Equal(t, 60*time.Second, result.RequeueAfter)
			require.Len(t, env.fake.mcpServers(), 1, "the config stays")

			gone, _ := env.reconcileDeleting(ctx, t, kept)
			require.Nil(t, gone, "the retry deletes it")
			require.Empty(t, env.fake.mcpServers())
		})
	}
}

// #378: without operator access, a delete keeps the finalizer and waits with
// the delete poll. It sends nothing to Coder.
func TestCoderMCPServerDeleteWithoutOperatorAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(env.controlPlane), env.controlPlane))
	env.controlPlane.Status.OperatorAccessReady = false
	require.NoError(t, k8sClient.Status().Update(ctx, env.controlPlane))
	requests := env.fake.totalRequests()

	kept, result := env.deleteMCPServer(ctx, t, got)
	require.NotNil(t, kept)
	require.Equal(t, []string{coderv1alpha1.CoderMCPServerCleanupFinalizer}, kept.Finalizers)
	require.Contains(t, requireMCPServerStep(t, kept, false, "OperatorAccessNotReady"), "once it can call Coder")
	require.Equal(t, 60*time.Second, result.RequeueAfter)
	require.Equal(t, requests, env.fake.totalRequests(), "no Coder request")
	require.Len(t, env.fake.mcpServers(), 1)
}

// Plan 4.10 item 2: with only a create marker, the slug read decides. A
// config with this object's nonce is deleted. Someone else's config with the
// slug is never touched. With no config, the controller waits until a late
// create cannot commit any more, then releases. #353: the record goes only
// when no config can still be the object's, as in a sweep.
func TestCoderMCPServerDeletePendingCreate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		create  func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer) // nil: no config in Coder.
		age     time.Duration                                                                  // Age of the create marker.
		release bool
		deletes int
		record  bool // The record stays.
	}{
		{name: "nonce found", create: createWithNonce, release: true, deletes: 1},
		{name: "someone else's config", create: createOther, release: true, record: true},
		{name: "nonce under another slug", create: createWithNonceUnderOtherSlug, age: 3 * time.Minute, release: true, record: true},
		{name: "no config, recent create", age: time.Second, record: true},
		{name: "no config, old create", age: 3 * time.Minute, release: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			server := env.pendingCreate(ctx, t, max(tc.age, time.Second), tc.create)
			before, deletes := len(env.fake.mcpServers()), env.fake.requestCount(routeDeleteMCPServer)

			kept, result := env.deleteMCPServer(ctx, t, server)
			require.Equal(t, tc.deletes, env.fake.requestCount(routeDeleteMCPServer)-deletes)
			require.Equal(t, tc.record, env.mcpServerRecord(ctx, t, server.UID) != nil, "record stays")
			if tc.release {
				require.Nil(t, kept)
				require.Len(t, env.fake.mcpServers(), before-tc.deletes)
				return
			}
			require.NotNil(t, kept)
			requireMCPServerStep(t, kept, false, "ConfirmingCreate")
			require.Positive(t, result.RequeueAfter)
			require.LessOrEqual(t, result.RequeueAfter, 2*time.Minute)
		})
	}
}

// #353: when a person renames the config of a lost create answer, the
// controller reports SlugConflict and drops its create marker. A delete then
// keeps the record, so a sweep can report the config for manual cleanup.
func TestCoderMCPServerDeleteKeepsRecordAfterSlugConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	env.fake.failNext(routeCreateMCPServer, fakeFault{AfterCommit: true, Reset: true})
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, false, "ConfirmingCreate")
	configs := env.fake.mcpServers()
	require.Len(t, configs, 1)
	displayName := "Renamed by a person"
	_, err := env.dashboard(t).UpdateMCPServerConfig(ctx, env.orgID, configs[0].view.ID, codersdk.UpdateMCPServerConfigRequest{DisplayName: &displayName})
	require.NoError(t, err)
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "SlugConflict")
	require.Nil(t, got.Status.CreateAttemptTime)

	gone, _ := env.deleteMCPServer(ctx, t, got)
	require.Nil(t, gone)
	require.Len(t, env.fake.mcpServers(), 1, "the controller never deletes a config that it cannot prove its own")
	require.NotNil(t, env.mcpServerRecord(ctx, t, got.UID), "the record stays for a sweep")
}

// pendingCreate returns an object with only a create marker of the given age.
// Coder holds no config of it, except one that create adds.
func (env mcpServerEnv) pendingCreate(
	ctx context.Context, t *testing.T, age time.Duration, create func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer),
) *coderv1alpha1.CoderMCPServer {
	t.Helper()
	env.keySecret(ctx, t, testMCPKey)
	server, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	for _, s := range env.fake.mcpServers() {
		require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, s.view.ID))
	}
	if create != nil {
		create(ctx, t, env, server)
	}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), server))
	server.Status.ConfigID = ""
	server.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-age)}
	require.NoError(t, k8sClient.Status().Update(ctx, server))
	// The record of a pending create has no config ID yet.
	record := &corev1.ConfigMap{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(server.UID)}, record))
	delete(record.Data, "coderID")
	require.NoError(t, k8sClient.Update(ctx, record))
	return server
}

// #378: with only a create marker, the slug read decides, so its answer
// does too. Coder's own 404 for the organization proves that no config of it
// can be reached any more, and releases. Any other failed read, and a
// missing config that the operator cannot prove absent because it is not a
// site owner, keep the finalizer and retry with the delete poll.
func TestCoderMCPServerDeletePendingCreateRead(t *testing.T) {
	t.Parallel()
	notSiteOwner := func(env mcpServerEnv) {
		env.fake.updateUser(env.fake.operatorID, func(u *codersdk.User) {
			u.Roles = []codersdk.SlimRole{{Name: codersdk.RoleOwner, OrganizationID: env.orgID.String()}}
		})
	}
	listFails := func(fault fakeFault) func(mcpServerEnv) {
		return func(env mcpServerEnv) { env.fake.failNext(routeMCPServers, fault) }
	}
	for _, tc := range []struct {
		name    string
		prepare func(mcpServerEnv)
		noNonce bool // No config of this object in Coder.
		release bool
		reason  string
		message string
	}{
		{name: "organization gone", release: true, noNonce: true, prepare: listFails(fakeFault{Status: http.StatusNotFound, Message: coderResourceNotFound})},
		{name: "proxy 404", reason: "CoderUnavailable", prepare: listFails(notCoder404s["HTML page"])},
		{name: "route not found", reason: "CoderUnavailable", prepare: listFails(notCoder404s["route not found"])},
		{name: "list fails", reason: "CoderUnavailable", prepare: listFails(fakeFault{Status: http.StatusBadGateway})},
		{name: "not a site owner", reason: "ConfirmingCreate", message: "not a site owner", prepare: notSiteOwner, noNonce: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			create := createWithNonce
			if tc.noNonce {
				create = nil
			}
			server := env.pendingCreate(ctx, t, 3*time.Minute, create)
			configs, deletes := len(env.fake.mcpServers()), env.fake.requestCount(routeDeleteMCPServer)
			tc.prepare(env)

			kept, result := env.deleteMCPServer(ctx, t, server)
			require.Equal(t, deletes, env.fake.requestCount(routeDeleteMCPServer), "no DELETE")
			if tc.release {
				require.Nil(t, kept)
				return
			}
			require.NotNil(t, kept)
			require.Equal(t, []string{coderv1alpha1.CoderMCPServerCleanupFinalizer}, kept.Finalizers)
			require.Contains(t, requireMCPServerStep(t, kept, false, tc.reason), tc.message)
			require.Equal(t, 60*time.Second, result.RequeueAfter)
			require.Len(t, env.fake.mcpServers(), configs, "nothing changed in Coder")
		})
	}
}

func createWithNonce(ctx context.Context, t *testing.T, env mcpServerEnv, server *coderv1alpha1.CoderMCPServer) {
	t.Helper()
	req := apiKeyMCPRequest(server.Spec.Slug)
	req.DisplayName = "coder-k8s:" + string(server.UID)
	_, err := env.dashboard(t).CreateMCPServerConfig(ctx, env.orgID, req)
	require.NoError(t, err)
}

// createWithNonceUnderOtherSlug is the config of a lost create answer after a
// person changed its slug in Coder.
func createWithNonceUnderOtherSlug(ctx context.Context, t *testing.T, env mcpServerEnv, server *coderv1alpha1.CoderMCPServer) {
	t.Helper()
	req := apiKeyMCPRequest("renamed")
	req.DisplayName = "coder-k8s:" + string(server.UID)
	_, err := env.dashboard(t).CreateMCPServerConfig(ctx, env.orgID, req)
	require.NoError(t, err)
}

func createOther(ctx context.Context, t *testing.T, env mcpServerEnv, server *coderv1alpha1.CoderMCPServer) {
	t.Helper()
	_, err := env.dashboard(t).CreateMCPServerConfig(ctx, env.orgID, apiKeyMCPRequest(server.Spec.Slug))
	require.NoError(t, err)
}

// Plan 4.10 item 5: while the control plane is being deleted (for example
// with its namespace), the controller makes one bounded DELETE attempt and
// then releases the finalizer, also when that attempt fails.
func TestCoderMCPServerDeleteWhileControlPlaneDeletes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fault *fakeFault
	}{
		{name: "attempt succeeds"},
		{name: "attempt fails", fault: &fakeFault{Status: http.StatusBadGateway}},
		// The attempt is bounded at 10 s, below the 30 s request timeout.
		{name: "attempt hangs", fault: &fakeFault{Hang: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, _ := env.applied(ctx, t, nil)
			holdControlPlane(ctx, t, env.namespace)
			if tc.fault != nil {
				env.fake.failNext(routeDeleteMCPServer, *tc.fault)
			}
			start := time.Now()
			gone, _ := env.deleteMCPServer(ctx, t, got)
			require.Less(t, time.Since(start), 20*time.Second, "the attempt is bounded")
			require.Nil(t, gone, "released after one attempt")
			require.Equal(t, 1, env.fake.requestCount(routeDeleteMCPServer))
			if tc.fault != nil && tc.fault.Hang {
				return
			}
			require.Equal(t, tc.fault == nil, len(env.fake.mcpServers()) == 0)
		})
	}
}

// #384 follow-up 2 (#353): a release without a delete in Coder counts once in
// coder_k8s_cleanup_released_without_delete_total, after the finalizer patch
// succeeds. A release that a failed patch left recorded does not count when
// the retry deletes the config. Not parallel: other tests release MCP servers
// too.
func TestCoderMCPServerReleasedWithoutDeleteMetric(t *testing.T) {
	ctx := context.Background()
	failFirstPatch := func(t *testing.T, env *mcpServerEnv) {
		t.Helper()
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
		require.NoError(t, err)
		failed := false
		env.r = &controller.CoderMCPServerReconciler{Scheme: scheme, APIReader: k8sClient, OperatorNamespace: env.namespace, Client: interceptor.NewClient(base, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*coderv1alpha1.CoderMCPServer); ok && !failed {
					failed = true
					return apierrors.NewConflict(schema.GroupResource{Resource: "codermcpservers"}, obj.GetName(), nil)
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})}
	}
	t.Run("control plane gone", func(t *testing.T) {
		env := newMCPServerEnv(ctx, t, true)
		env.keySecret(ctx, t, testMCPKey)
		got, _ := env.applied(ctx, t, nil)
		before := releasedWithoutDelete(t, "CoderMCPServer")
		require.NoError(t, k8sClient.Delete(ctx, env.controlPlane))
		require.NoError(t, k8sClient.Delete(ctx, got))
		failFirstPatch(t, &env)
		_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
		require.Error(t, err, "the finalizer patch fails once")
		require.Equal(t, before, releasedWithoutDelete(t, "CoderMCPServer"), "no count before the finalizer is gone")
		gone, _ := env.reconcileDeleting(ctx, t, got)
		require.Nil(t, gone)
		require.Len(t, env.fake.mcpServers(), 1, "the config stays in Coder")
		require.Equal(t, before+1, releasedWithoutDelete(t, "CoderMCPServer"))
	})
	t.Run("recovered", func(t *testing.T) {
		env := newMCPServerEnv(ctx, t, true)
		env.keySecret(ctx, t, testMCPKey)
		got, _ := env.applied(ctx, t, nil)
		before := releasedWithoutDelete(t, "CoderMCPServer")
		holdControlPlane(ctx, t, env.namespace)
		require.NoError(t, k8sClient.Delete(ctx, got))
		env.fake.failNext(routeDeleteMCPServer, fakeFault{Status: http.StatusBadGateway})
		failFirstPatch(t, &env)
		_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
		require.Error(t, err, "the release after the failed bounded attempt fails its finalizer patch")
		gone, _ := env.reconcileDeleting(ctx, t, got)
		require.Nil(t, gone)
		require.Empty(t, env.fake.mcpServers(), "the retry deleted the config")
		require.Equal(t, before, releasedWithoutDelete(t, "CoderMCPServer"), "a release that deleted in Coder does not count")
	})
}

// #382 (#353): a control plane that is being deleted waits for a
// CoderMCPServer that is being deleted, so Coder keeps running. Under that
// hold the object takes the normal delete path: a failed DELETE keeps the
// finalizer and retries, instead of one bounded attempt and a release. Then
// the control plane goes.
func TestCoderMCPServerDeleteUnderControlPlaneHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	deleteFinalizedControlPlane(ctx, t, env.namespace, "coder")
	require.NoError(t, k8sClient.Delete(ctx, got))

	controlPlaneGone := func() bool {
		t.Helper()
		r := &controller.CoderControlPlaneReconciler{Client: k8sClient, Scheme: scheme}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: env.namespace, Name: "coder"}})
		require.NoError(t, err)
		err = k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "coder"}, &coderv1alpha1.CoderControlPlane{})
		return apierrors.IsNotFound(err)
	}
	require.False(t, controlPlaneGone(), "the control plane waits for the MCP server")

	env.fake.failNext(routeDeleteMCPServer, fakeFault{Status: http.StatusBadGateway})
	kept, result := env.reconcileDeleting(ctx, t, got)
	require.NotNil(t, kept, "a failed delete keeps the finalizer while the control plane waits")
	requireMCPServerStep(t, kept, false, "CoderUnavailable")
	require.Equal(t, 60*time.Second, result.RequeueAfter)
	require.False(t, controlPlaneGone(), "the control plane still waits")

	gone, _ := env.reconcileDeleting(ctx, t, kept)
	require.Nil(t, gone)
	require.Empty(t, env.fake.mcpServers(), "the retry deleted the config")
	require.Equal(t, 2, env.fake.requestCount(routeDeleteMCPServer))
	require.True(t, controlPlaneGone(), "the control plane goes after the MCP server")
}

// From the #329 final assessment: a released object's entry in the
// reconciler's credential hash map goes, so the map does not grow without
// bound.
func TestCoderMCPServerDeleteForgetsSentCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	require.True(t, controller.HasSentCredentials(env.r, got.UID))
	gone, _ := env.deleteMCPServer(ctx, t, got)
	require.Nil(t, gone)
	require.False(t, controller.HasSentCredentials(env.r, got.UID))
}

// #378: a finalizer that someone removes by hand skips the release, so the
// reconcile that finds the object gone drops its credential hashes.
func TestCoderMCPServerManualFinalizerRemovalForgetsSentCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	require.True(t, controller.HasSentCredentials(env.r, got.UID))
	got.Finalizers = nil
	require.NoError(t, k8sClient.Update(ctx, got))
	require.NoError(t, k8sClient.Delete(ctx, got))

	gone, _ := env.reconcileDeleting(ctx, t, got)
	require.Nil(t, gone)
	require.False(t, controller.HasSentCredentials(env.r, got.UID))
}

func configIDs(env mcpServerEnv) []uuid.UUID {
	var out []uuid.UUID
	for _, s := range env.fake.mcpServers() {
		out = append(out, s.view.ID)
	}
	return out
}
