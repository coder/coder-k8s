package controller_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// #336: when the control plane withdraws its opt-in, the controller disables
// a config that this object created with one PATCH that holds only enabled
// false, and it reads no credential Secret. No other write follows while the
// opt-in stays withdrawn. The disable is a kill switch: the restored opt-in
// gives DisabledExternally, and a spec edit enables the config again.
func TestCoderMCPServerDisablesWithoutOptIn(t *testing.T) {
	t.Parallel()
	forceOn := func(s *coderv1alpha1.CoderMCPServerSpec) { s.Availability = "force_on" }
	for _, tc := range []struct {
		name     string
		spec     func(*coderv1alpha1.CoderMCPServerSpec)
		optIn    *coderv1alpha1.MCPServersSpec // The opt-in before the withdrawal.
		withdraw *coderv1alpha1.MCPServersSpec
		reason   string
	}{
		{name: "opt-in removed", reason: "MCPServersNotAllowed"},
		{name: "organization unlisted", withdraw: &coderv1alpha1.MCPServersSpec{Organizations: []string{"other"}}, reason: "MCPServersNotAllowed"},
		{
			name: "force_on no longer allowed", spec: forceOn, reason: "ForceOnNotAllowed",
			optIn:    &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}, AllowForceOn: true},
			withdraw: &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			optIn := tc.optIn
			if optIn == nil {
				optIn = &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}}
			}
			env.setOptIn(ctx, t, optIn)
			got, _ := env.applied(ctx, t, tc.spec)
			before := env.stored(t, got)

			// Count the reads of the credential Secret from here on.
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
			require.NoError(t, err)
			var secretReads atomic.Int32
			env.r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Secret); ok && key.Name == "github-mcp" {
						secretReads.Add(1)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			env.setOptIn(ctx, t, tc.withdraw)

			// A failed disable is retried, and the message says that Coder
			// can still use the config.
			env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
			got, result := env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, false, tc.reason), "Coder can still use MCP server config")
			require.Equal(t, 30*time.Second, result.RequeueAfter)

			mark := len(env.writeRoutes())
			got, result = env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, true, tc.reason), "The controller disabled MCP server config")
			require.Equal(t, 10*time.Minute, result.RequeueAfter)
			writes := env.writesSince(mark)
			require.Len(t, writes, 1)
			require.Equal(t, routeUpdateMCPServer, writes[0].route)
			require.JSONEq(t, `{"enabled":false}`, string(writes[0].body))
			before.view.Enabled = false
			require.Equal(t, before, env.stored(t, got), "only enabled changed")

			mark = len(env.writeRoutes())
			for range 2 {
				got, _ = env.reconcile(ctx, t, got)
				require.Contains(t, requireMCPServerStep(t, got, true, tc.reason), "is disabled in Coder")
			}
			require.Empty(t, env.writesSince(mark), "no write while the opt-in stays withdrawn")
			require.Zero(t, secretReads.Load(), "no credential read")

			env.setOptIn(ctx, t, optIn)
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerStep(t, got, true, "DisabledExternally")
			require.Empty(t, env.writesSince(mark))

			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
			got.Spec.Description = "re-armed"
			require.NoError(t, k8sClient.Update(ctx, got))
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerApplied(t, got)
			require.True(t, env.stored(t, got).view.Enabled)
		})
	}
}

// An object without a config ID has nothing in Coder to disable, so a
// withdrawn opt-in still sends no MCP request (plan S17).
func TestCoderMCPServerOptInWithdrawnWithoutConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	env.setOptIn(ctx, t, nil)
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, true, "MCPServersNotAllowed")
	require.Zero(t, env.mcpRequests())
}
