package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// #337: in the sync, only Coder's own 404 proves that the config is gone. A
// proxy page or "Route not found." waits with CoderUnavailable and changes
// nothing, both for an applied config (no DeletedExternally kill switch) and
// after a spec edit (no cleared config ID, no create).
func TestCoderMCPServerSyncNotCoder404(t *testing.T) {
	t.Parallel()
	for name, fault := range notCoder404s {
		for _, edited := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/edited=%v", name, edited), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				env := newMCPServerEnv(ctx, t, true)
				env.keySecret(ctx, t, testMCPKey)
				got, _ := env.applied(ctx, t, nil)
				if edited {
					require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
					got.Spec.Description = "edited"
					require.NoError(t, k8sClient.Update(ctx, got))
				}
				before := got.Status.DeepCopy()
				mark := len(env.writeRoutes())
				env.fake.failNext(routeMCPServer, fault)
				got, result := env.reconcile(ctx, t, got)
				requireMCPServerStep(t, got, false, "CoderUnavailable")
				require.Equal(t, 30*time.Second, result.RequeueAfter)
				require.Equal(t, before.ConfigID, got.Status.ConfigID, "ownership is kept")
				require.Equal(t, before.Credentials, got.Status.Credentials)
				require.Empty(t, env.writesSince(mark), "no Coder write")

				got, _ = env.reconcile(ctx, t, got)
				requireMCPServerApplied(t, got)
				require.Equal(t, 1, env.posts(), "the only create is the first one")
			})
		}
	}
}

// #337: the read of the disable path also needs Coder's own 404 before the
// config counts as gone. Another 404 waits, keeps the config ID, and the next
// reconcile disables the config.
func TestCoderMCPServerDisableNotCoder404(t *testing.T) {
	t.Parallel()
	for name, fault := range notCoder404s {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			secret := env.keySecret(ctx, t, testMCPKey)
			got, _ := env.applied(ctx, t, nil)
			require.NoError(t, k8sClient.Delete(ctx, secret))
			before := got.Status.DeepCopy()
			mark := len(env.writeRoutes())
			env.fake.failNext(routeMCPServer, fault)
			got, result := env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, false, credentialsecret.ReasonSecretNotFound), "Coder can still send the credentials")
			require.Equal(t, 30*time.Second, result.RequeueAfter)
			require.Equal(t, before.ConfigID, got.Status.ConfigID, "ownership is kept")
			require.Empty(t, env.writesSince(mark), "no Coder write")
			require.True(t, env.stored(t, got).view.Enabled)

			got, _ = env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, true, credentialsecret.ReasonSecretNotFound), "The controller disabled MCP server config")
			require.False(t, env.stored(t, got).view.Enabled)
		})
	}
}

// Coder's own 404 still counts as gone in the disable read, so the test
// above cannot pass by never treating a 404 as gone. The sync read is
// covered by TestCoderMCPServerSyncDeletedConfig.
func TestCoderMCPServerDisableCoder404(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	secret := env.keySecret(ctx, t, testMCPKey)
	got, id := env.applied(ctx, t, nil)
	require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, id))
	require.NoError(t, k8sClient.Delete(ctx, secret))
	mark := len(env.writeRoutes())
	got, result := env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, credentialsecret.ReasonSecretNotFound), "does not exist in Coder")
	require.Equal(t, 10*time.Minute, result.RequeueAfter)
	require.Empty(t, env.writesSince(mark))
}
