package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// #415: the Enabled column shows what Coder holds, not spec.enabled. A
// refused object that Coder does not have shows no value.
func TestCoderMCPServerEnabledColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("refused before a create", func(t *testing.T) {
		t.Parallel()
		env := newMCPServerEnv(ctx, t, true)
		got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
		requireMCPServerStep(t, got, true, credentialsecret.ReasonSecretNotFound)
		require.True(t, *got.Spec.Enabled)
		require.Nil(t, enabledColumn(ctx, t, "codermcpservers", got))
	})
	t.Run("opt-in withdrawn, then deleted in Coder", func(t *testing.T) {
		t.Parallel()
		env := newMCPServerEnv(ctx, t, true)
		env.keySecret(ctx, t, testMCPKey)
		got, id := env.applied(ctx, t, nil)
		require.Equal(t, true, enabledColumn(ctx, t, "codermcpservers", got))

		optIn := env.controlPlane.Spec.MCPServers.DeepCopy()
		env.setOptIn(ctx, t, nil)
		got, _ = env.reconcile(ctx, t, got)
		requireMCPServerStep(t, got, true, "MCPServersNotAllowed")
		require.Equal(t, false, enabledColumn(ctx, t, "codermcpservers", got))

		env.setOptIn(ctx, t, optIn)
		got, _ = env.reconcile(ctx, t, got)
		requireMCPServerStep(t, got, true, "DisabledExternally")
		require.Equal(t, false, enabledColumn(ctx, t, "codermcpservers", got))

		require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, id))
		got, _ = env.reconcile(ctx, t, got)
		requireMCPServerStep(t, got, true, "DeletedExternally")
		require.Nil(t, enabledColumn(ctx, t, "codermcpservers", got))
	})
}

// #415: a status write between two steps must not advance
// status.observedGeneration past the conditions, or GitOps health checks read
// the conditions of an older generation as current. The write that records a
// committed create runs before the PATCH of the sync.
func TestCoderMCPServerInterruptedUpdateKeepsObservedGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	env.fake.failNext(routeCreateMCPServer, fakeFault{Reset: true, AfterCommit: true})
	got, _ := env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, false, "ConfirmingCreate")
	require.Empty(t, got.Status.ConfigID)
	confirmedGeneration := got.Generation

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.DisplayName = "GitHub tools"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Hang: true})
	short, cancel := context.WithTimeout(ctx, hangTimeout)
	defer cancel()
	_, err := env.r.Reconcile(short, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
	require.Error(t, err)

	stored := &coderv1alpha1.CoderMCPServer{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), stored))
	require.NotEmpty(t, stored.Status.ConfigID, "the config ID was written before the PATCH")
	require.Equal(t, confirmedGeneration, stored.Status.ObservedGeneration, "the config ID write does not observe the edit")
	require.Equal(t, confirmedGeneration, meta.FindStatusCondition(stored.Status.Conditions, "Ready").ObservedGeneration)
	require.Equal(t, env.mcpServerRecord(ctx, t, stored.UID)["nonce"], env.stored(t, stored).view.DisplayName)

	got, _ = env.reconcile(ctx, t, stored)
	requireMCPServerApplied(t, got)
	require.Equal(t, "GitHub tools", env.stored(t, got).view.DisplayName)
}
