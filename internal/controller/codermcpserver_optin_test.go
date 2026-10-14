package controller_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
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
	got, result := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, true, "MCPServersNotAllowed")
	require.Equal(t, 10*time.Minute, result.RequeueAfter, "a timed resync (#358)")
	require.Zero(t, env.mcpRequests())
}

// #358: only Coder's own 404 proves that an organization does not exist. A
// proxy page or "Route not found." on the lookup of spec.organization, or of
// a listed name, waits with CoderUnavailable instead of OrganizationNotFound
// or MCPServersNotAllowed. An unlisted organization resyncs on a timer,
// because a listed name can resolve later without a Kubernetes event.
func TestCoderMCPServerOrganizationNotCoder404(t *testing.T) {
	t.Parallel()
	for name, fault := range notCoder404s {
		for _, lookup := range []string{"own", "listed"} {
			t.Run(name+"/"+lookup, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				env := newMCPServerEnv(ctx, t, true)
				env.keySecret(ctx, t, testMCPKey)
				env.fake.addOrganization("alias")
				env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"alias"}})
				server := env.create(ctx, t, nil)
				if lookup == "listed" {
					// The object's own organization resolves first.
					env.fake.failNext(routeOrganization, fakeFault{Rewrite: func(answer any) any { return answer }})
				}
				env.fake.failNext(routeOrganization, fault)
				got, result := env.reconcile(ctx, t, server)
				requireMCPServerStep(t, got, false, "CoderUnavailable")
				require.Equal(t, 30*time.Second, result.RequeueAfter)
				require.Zero(t, env.mcpRequests())

				got, result = env.reconcile(ctx, t, got)
				requireMCPServerStep(t, got, true, "MCPServersNotAllowed")
				require.Equal(t, 10*time.Minute, result.RequeueAfter)
			})
		}
	}
}

// A disable answer can be lost after Coder committed it. The next reconcile
// then finds the config disabled and records the generation, so the
// restored opt-in does not apply a pending generation that enables it.
func TestCoderMCPServerOptInFoundDisabledHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, id := env.applied(ctx, t, nil)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.Description = "pending"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, false, "CoderUnavailable")

	env.setOptIn(ctx, t, nil)
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, false, "MCPServersNotAllowed"), "the disable failed")
	require.False(t, env.stored(t, got).view.Enabled, "the PATCH committed")

	mark := len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, "MCPServersNotAllowed"), "is disabled in Coder")
	require.Equal(t, got.Generation, got.Status.AppliedGeneration)

	env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}})
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "DisabledExternally")
	require.Empty(t, env.writesSince(mark))
	stored := env.stored(t, got)
	require.False(t, stored.view.Enabled)
	require.Empty(t, stored.view.Description, "the pending generation is not applied")
	require.Equal(t, id, stored.view.ID)
}

// Only a withdrawn opt-in disables the config. Another stall of the scope
// check, such as a mismatched Coder answer for a listed name, keeps the
// config as it is and sends no MCP request.
func TestCoderMCPServerScopeMismatchKeepsConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	env.fake.addOrganization("alias")
	got, _ := env.applied(ctx, t, nil)
	env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"alias"}})
	// The object's own organization resolves, then the listed name gets an
	// answer for another organization.
	env.fake.failNext(routeOrganization, fakeFault{Rewrite: func(answer any) any { return answer }})
	env.fake.failNext(routeOrganization, fakeFault{Rewrite: func(answer any) any {
		org := answer.(codersdk.Organization)
		org.Name = "other"
		return org
	}})
	mcp := env.mcpRequests()
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "CoderAnswerMismatch")
	require.Equal(t, mcp, env.mcpRequests())
	require.True(t, env.stored(t, got).view.Enabled)
}

// A disable PATCH can commit while its answer is lost, and the opt-in can
// come back before the next reconcile. The controller wrote
// appliedGeneration before the PATCH, so the sync then finds
// DisabledExternally and does not apply a pending generation. When that
// status write fails, the controller sends no PATCH.
func TestCoderMCPServerOptInDisableMarkerFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		lostStatus bool // The status write before the PATCH fails.
	}{{name: "answer lost, opt-in back"}, {name: "marker write fails", lostStatus: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, _ := env.applied(ctx, t, nil)
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
			got.Spec.Description = "pending"
			require.NoError(t, k8sClient.Update(ctx, got))
			env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerStep(t, got, false, "CoderUnavailable")
			env.setOptIn(ctx, t, nil)

			if tc.lostStatus {
				base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
				require.NoError(t, err)
				r := &controller.CoderMCPServerReconciler{Client: interceptor.NewClient(base, interceptor.Funcs{
					SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
						return errors.New("lost status write")
					},
				}), Scheme: scheme}
				mark := len(env.writeRoutes())
				_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
				require.ErrorContains(t, err, "lost status write")
				require.Empty(t, env.writesSince(mark), "no PATCH without the marker")
				require.True(t, env.stored(t, got).view.Enabled)
				return
			}

			env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
			got, _ = env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, false, "MCPServersNotAllowed"), "the disable failed")
			require.False(t, env.stored(t, got).view.Enabled, "the PATCH committed")
			require.Equal(t, got.Generation, got.Status.AppliedGeneration, "written before the PATCH")

			env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}})
			mark := len(env.writeRoutes())
			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerStep(t, got, true, "DisabledExternally")
			require.Empty(t, env.writesSince(mark))
			require.Empty(t, env.stored(t, got).view.Description, "the pending generation is not applied")
		})
	}
}

// A renamed, deleted or replaced organization keeps the config in the
// pinned organization. A withdrawn opt-in still disables it there, while a
// name that is still listed changes nothing.
func TestCoderMCPServerOptInUsesPinnedOrganization(t *testing.T) {
	t.Parallel()
	gone := fakeFault{Status: http.StatusNotFound, Message: coderResourceNotFound}
	replaced := fakeFault{Rewrite: func(answer any) any {
		org := answer.(codersdk.Organization)
		org.ID = uuid.New()
		return org
	}}
	for _, tc := range []struct {
		name     string
		fault    fakeFault
		reason   string
		withdraw bool
	}{
		{name: "not found, opt-in removed", fault: gone, reason: "OrganizationNotFound", withdraw: true},
		{name: "replaced, opt-in removed", fault: replaced, reason: "OrganizationChanged", withdraw: true},
		{name: "not found, still listed", fault: gone, reason: "OrganizationNotFound"},
		{name: "replaced, still listed", fault: replaced, reason: "OrganizationChanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, _ := env.applied(ctx, t, nil)
			if tc.withdraw {
				env.setOptIn(ctx, t, nil)
			}
			env.fake.failNext(routeOrganization, tc.fault)
			mark := len(env.writeRoutes())
			got, _ = env.reconcile(ctx, t, got)
			if !tc.withdraw {
				message := requireMCPServerStep(t, got, tc.reason == "OrganizationChanged", tc.reason)
				require.NotContains(t, message, "disabled")
				require.Empty(t, env.writesSince(mark))
				require.True(t, env.stored(t, got).view.Enabled)
				return
			}
			require.Contains(t, requireMCPServerStep(t, got, true, "MCPServersNotAllowed"), "The controller disabled MCP server config")
			writes := env.writesSince(mark)
			require.Len(t, writes, 1)
			require.JSONEq(t, `{"enabled":false}`, string(writes[0].body))
			require.False(t, env.stored(t, got).view.Enabled)
		})
	}
}

// A config that is gone while a generation is pending and the opt-in is
// withdrawn: the kill switch holds it too. The restored opt-in gives
// DeletedExternally and no create, and only a spec edit creates the config
// again.
func TestCoderMCPServerOptInGoneConfigHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, id := env.applied(ctx, t, nil)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.Description = "pending"
	require.NoError(t, k8sClient.Update(ctx, got))
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, false, "CoderUnavailable")

	require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, id))
	env.setOptIn(ctx, t, nil)
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, "MCPServersNotAllowed"), "does not exist in Coder")
	require.Equal(t, got.Generation, got.Status.AppliedGeneration)

	env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}})
	mark := len(env.writeRoutes())
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "DeletedExternally")
	require.Empty(t, env.writesSince(mark), "no create before a spec edit")
	require.Equal(t, 1, env.posts())

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got))
	got.Spec.Description = "after the disable"
	require.NoError(t, k8sClient.Update(ctx, got))
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.Equal(t, 2, env.posts())
}
