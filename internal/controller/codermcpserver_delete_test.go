package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// Plan 4.10 item 2: with only a create marker, the slug read decides. A
// config with this object's nonce is deleted. Someone else's config with the
// slug is never touched. With no config, the controller waits until a late
// create cannot commit any more, then releases.
func TestCoderMCPServerDeletePendingCreate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		create  func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer) // nil: no config in Coder.
		age     time.Duration                                                                  // Age of the create marker.
		release bool
		deletes int
	}{
		{name: "nonce found", create: createWithNonce, release: true, deletes: 1},
		{name: "someone else's config", create: createOther, release: true},
		{name: "no config, recent create", age: time.Second},
		{name: "no config, old create", age: 3 * time.Minute, release: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			server := env.create(ctx, t, nil)
			server, _ = env.reconcile(ctx, t, server)
			for _, s := range env.fake.mcpServers() {
				require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, s.view.ID))
			}
			if tc.create != nil {
				tc.create(ctx, t, env, server)
			}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), server))
			server.Status.ConfigID = ""
			server.Status.CreateAttemptTime = &metav1.Time{Time: time.Now().Add(-max(tc.age, time.Second))}
			require.NoError(t, k8sClient.Status().Update(ctx, server))
			before, deletes := len(env.fake.mcpServers()), env.fake.requestCount(routeDeleteMCPServer)

			kept, result := env.deleteMCPServer(ctx, t, server)
			require.Equal(t, tc.deletes, env.fake.requestCount(routeDeleteMCPServer)-deletes)
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

func createWithNonce(ctx context.Context, t *testing.T, env mcpServerEnv, server *coderv1alpha1.CoderMCPServer) {
	t.Helper()
	req := apiKeyMCPRequest(server.Spec.Slug)
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

func configIDs(env mcpServerEnv) []uuid.UUID {
	var out []uuid.UUID
	for _, s := range env.fake.mcpServers() {
		out = append(out, s.view.ID)
	}
	return out
}
