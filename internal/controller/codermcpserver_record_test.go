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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// mcpServerRecord returns the data of the cleanup record of uid, or nil when
// there is none. The env namespace is also the operator namespace.
func (env mcpServerEnv) mcpServerRecord(ctx context.Context, t *testing.T, uid types.UID) map[string]string {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(uid)}, configMap)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	require.Equal(t, "CoderMCPServer", configMap.Labels["coder.com/cleanup-record"])
	return configMap.Data
}

// #353 (#384 follow-up 3): before its create, the controller records which
// Coder deployment holds the config, and the config ID before the sync
// replaces the nonce. The record holds no credential. A confirmed delete in
// Coder removes it.
func TestCoderMCPServerCleanupRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)

	// Stop after the create, before the sync replaces the nonce.
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, false, "CoderUnavailable")
	record := env.mcpServerRecord(ctx, t, got.UID)
	require.NotEmpty(t, record["createAttemptTime"])
	delete(record, "createAttemptTime")
	require.Equal(t, map[string]string{
		"kind": "CoderMCPServer", "uid": string(got.UID), "namespace": env.namespace, "name": got.Name,
		"deploymentID": env.fake.deploymentID, "nonce": "coder-k8s:" + string(got.UID), "coderID": env.requireCreated(t, got).view.ID.String(),
		"type": "", "organizationID": env.orgID.String(), "slug": got.Spec.Slug,
	}, record, "the config ID is recorded before the sync replaces the nonce")

	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	for _, v := range env.mcpServerRecord(ctx, t, got.UID) {
		require.NotContains(t, v, testMCPKey, "a record holds no credential")
	}

	gone, _ := env.deleteMCPServer(ctx, t, got)
	require.Nil(t, gone)
	require.Empty(t, env.fake.mcpServers())
	require.Nil(t, env.mcpServerRecord(ctx, t, got.UID), "Coder confirmed the delete, so the record goes")
}

// #353 invariant 1: without a record, the controller sends no create.
func TestCoderMCPServerNoCreateWithoutRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("record write fails", func(t *testing.T) {
		t.Parallel()
		env := newMCPServerEnv(ctx, t, true)
		env.keySecret(ctx, t, testMCPKey)
		server := env.create(ctx, t, nil)
		env.r.Client = interceptor.NewClient(watchClient(t), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return errors.New("injected record write failure")
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)})
		require.ErrorContains(t, err, "injected record write failure")
		require.Zero(t, env.posts(), "no create without a record")
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), server))
		require.Nil(t, server.Status.CreateAttemptTime, "no create marker either")
	})
	t.Run("deployment ID unknown", func(t *testing.T) {
		t.Parallel()
		env := newMCPServerEnv(ctx, t, true)
		env.keySecret(ctx, t, testMCPKey)
		env.fake.failNext(routeBuildInfo, fakeFault{Status: http.StatusServiceUnavailable})
		got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
		requireMCPServerStep(t, got, false, "CoderUnavailable")
		require.Zero(t, env.posts())
		require.Nil(t, env.mcpServerRecord(ctx, t, got.UID))
	})
}

// #353: a release without a delete in Coder keeps the record, so a later
// sweep can delete the config. When the control plane reaches another Coder
// deployment, a delete there proves nothing, so the record stays too.
func TestCoderMCPServerRecordKeptWithoutDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, leave := range map[string]func(env mcpServerEnv){
		"control plane gone": func(env mcpServerEnv) { require.NoError(t, k8sClient.Delete(ctx, env.controlPlane)) },
		"deployment switch": func(env mcpServerEnv) {
			env.fake.mu.Lock()
			env.fake.deploymentID = uuid.NewString() // A new database behind the same control plane.
			env.fake.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, id := env.applied(ctx, t, nil)
			leave(env)
			gone, _ := env.deleteMCPServer(ctx, t, got)
			require.Nil(t, gone, "the finalizer goes without a delete")
			require.Zero(t, env.fake.requestCount(routeDeleteMCPServer))
			require.Len(t, env.fake.mcpServers(), 1, "the config stays in Coder")
			require.Equal(t, id.String(), env.mcpServerRecord(ctx, t, got.UID)["coderID"], "the record stays for a sweep")
		})
	}
}

// #353 (#362 follow-up 7, for MCP servers): a create whose answer was lost
// leaves a record without a config ID. When the control plane then reaches
// another Coder deployment, where someone else's config has the same slug,
// that slug conflict proves nothing about the old deployment. The controller
// keeps the marker and the record, so a delete keeps the record for a sweep.
func TestCoderMCPServerPendingCreateAfterDeploymentSwitch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	env.fake.failNext(routeCreateMCPServer, fakeFault{AfterCommit: true, Reset: true})
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, false, "ConfirmingCreate")
	before := env.mcpServerRecord(ctx, t, got.UID)
	require.NotNil(t, before)
	require.Empty(t, before["coderID"], "the create answer was lost")

	env.fake.mu.Lock()
	env.fake.deploymentID = uuid.NewString() // A new database behind the same control plane.
	for _, s := range env.fake.mcp.servers {
		s.view.DisplayName = "Someone else's server"
	}
	env.fake.mu.Unlock()
	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, "CoderDeploymentChanged"), before["deploymentID"])
	require.NotNil(t, got.Status.CreateAttemptTime, "the marker stays")
	require.Equal(t, before, env.mcpServerRecord(ctx, t, got.UID))

	gone, _ := env.deleteMCPServer(ctx, t, got)
	require.Nil(t, gone, "the finalizer goes without a delete")
	require.Zero(t, env.fake.requestCount(routeDeleteMCPServer), "no delete in the new deployment")
	require.Equal(t, before, env.mcpServerRecord(ctx, t, got.UID), "the record stays for a sweep")
}

// #353, #422 item 5: a status without the config ID keeps the recorded ID.
// After the sync the display name is no longer the nonce, so only the
// recorded ID still proves that the config with the slug is this object's.
// The controller takes it back instead of reporting SlugConflict, and a
// delete uses it.
func TestCoderMCPServerKeepsRecordedIDWithoutStatusID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, lose := range map[string]func(*coderv1alpha1.CoderMCPServerStatus){
		"create marker kept": func(s *coderv1alpha1.CoderMCPServerStatus) {
			s.ConfigID, s.CreateAttemptTime = "", &metav1.Time{Time: time.Now().Add(-time.Hour)}
		},
		"status lost everything": func(s *coderv1alpha1.CoderMCPServerStatus) { *s = coderv1alpha1.CoderMCPServerStatus{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, id := env.applied(ctx, t, nil)
			lose(&got.Status)
			require.NoError(t, k8sClient.Status().Update(ctx, got))

			got, _ = env.reconcile(ctx, t, got)
			requireMCPServerApplied(t, got)
			require.Equal(t, id.String(), got.Status.ConfigID, "the recorded ID proves the config")
			require.Equal(t, 1, env.posts(), "no new create")
			require.Equal(t, id.String(), env.mcpServerRecord(ctx, t, got.UID)["coderID"], "the record keeps the config ID")

			gone, _ := env.deleteMCPServer(ctx, t, got)
			require.Nil(t, gone)
			require.Empty(t, env.fake.mcpServers(), "the delete used the recorded ID")
			require.Nil(t, env.mcpServerRecord(ctx, t, got.UID))
		})
	}
}

// #422 item 5: the recorded ID proves a config only in the record's Coder
// deployment. After a status loss and a switch to another deployment that
// holds a config with the same ID, the controller stalls with
// CoderDeploymentChanged and writes nothing.
func TestCoderMCPServerRecordedIDNeedsRecordedDeployment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, _ := env.applied(ctx, t, nil)
	before := env.mcpServerRecord(ctx, t, got.UID)
	got.Status = coderv1alpha1.CoderMCPServerStatus{}
	require.NoError(t, k8sClient.Status().Update(ctx, got))
	env.fake.mu.Lock()
	env.fake.deploymentID = uuid.NewString() // A copy of the database under a new deployment ID.
	env.fake.mu.Unlock()
	mark := len(env.writeRoutes())

	got, _ = env.reconcile(ctx, t, got)
	require.Contains(t, requireMCPServerStep(t, got, true, "CoderDeploymentChanged"), before["deploymentID"])
	require.Empty(t, got.Status.ConfigID)
	require.Empty(t, env.writesSince(mark), "no write in another deployment")
	require.Equal(t, before, env.mcpServerRecord(ctx, t, got.UID), "the record keeps the old locator")
}

// #353: a config from before records existed gets one, with the deployment
// ID that Coder answers now.
func TestCoderMCPServerRecordBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, id := env.applied(ctx, t, nil)
	require.NoError(t, k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(got.UID)}}))

	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	record := env.mcpServerRecord(ctx, t, got.UID)
	require.Equal(t, id.String(), record["coderID"])
	require.Equal(t, env.fake.deploymentID, record["deploymentID"])
}

// #353: a config from before records gets one also while the controller
// disables it because the control plane withdrew its opt-in.
func TestCoderMCPServerRecordBackfillOnDisable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, id := env.applied(ctx, t, nil)
	require.NoError(t, k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(got.UID)}}))
	env.setOptIn(ctx, t, nil)

	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "MCPServersNotAllowed")
	require.False(t, env.stored(t, got).view.Enabled, "the disable still runs")
	require.Equal(t, id.String(), env.mcpServerRecord(ctx, t, got.UID)["coderID"])
}

// #353: a config from before records gets one also when its object is
// already being deleted, so a release without a confirmed delete leaves a
// record for a sweep.
func TestCoderMCPServerRecordBackfillOnDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, id := env.applied(ctx, t, nil)
	require.NoError(t, k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(got.UID)}}))
	holdControlPlane(ctx, t, env.namespace)
	env.fake.failNext(routeDeleteMCPServer, fakeFault{Status: http.StatusBadGateway})

	gone, _ := env.deleteMCPServer(ctx, t, got)
	require.Nil(t, gone, "released after one attempt")
	require.Len(t, env.fake.mcpServers(), 1)
	record := env.mcpServerRecord(ctx, t, got.UID)
	require.Equal(t, id.String(), record["coderID"], "the record stays for a sweep")
	require.Equal(t, env.fake.deploymentID, record["deploymentID"])
}

// #353: a create answer that was lost is confirmed by the next reconcile,
// which records the config ID before the sync replaces the nonce.
func TestCoderMCPServerRecordAfterLostCreateAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	env.fake.failNext(routeCreateMCPServer, fakeFault{AfterCommit: true, Reset: true})
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, false, "ConfirmingCreate")

	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, false, "CoderUnavailable")
	require.Equal(t, env.requireCreated(t, got).view.ID.String(), env.mcpServerRecord(ctx, t, got.UID)["coderID"])
}

// #353: a config that is gone before a full apply is created again. The
// record write before that create drops the old ID, so a delete does not
// trust the 404 of the old ID. Without the new ID it deletes nothing (#422
// item 1), and the record stays for a sweep.
func TestCoderMCPServerRecreateDropsGoneID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, old := env.applied(ctx, t, nil)
	got.Spec.Description = "pending"
	require.NoError(t, k8sClient.Update(ctx, got))
	require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, old))

	env.fake.failNext(routeCreateMCPServer, fakeFault{AfterCommit: true, Reset: true})
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, false, "ConfirmingCreate")
	require.Len(t, env.fake.mcpServers(), 1, "the create committed")
	require.Empty(t, env.mcpServerRecord(ctx, t, got.UID)["coderID"], "the gone ID left the record")

	gone, _ := env.deleteMCPServer(ctx, t, got)
	require.Nil(t, gone)
	require.Len(t, env.fake.mcpServers(), 1, "a display name never proves ownership")
	require.NotNil(t, env.mcpServerRecord(ctx, t, got.UID), "the record stays for a sweep")
}

// #353: when the control plane reaches another Coder deployment while the
// object lives, the config looks gone, but the record still locates it in
// the old deployment. With or without a pending spec edit, the controller
// stalls with CoderDeploymentChanged, keeps the record and creates nothing in
// the new deployment, and a delete keeps the record too.
func TestCoderMCPServerDeploymentSwitchKeepsRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		edit    bool // A spec edit is pending.
		noOrgID bool // The new database has no organization with the pinned ID.
	}{
		"applied":                         {},
		"pending spec edit":               {edit: true},
		"organization not in the new one": {noOrgID: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			got, _ := env.applied(ctx, t, nil)
			before := env.mcpServerRecord(ctx, t, got.UID)
			env.fake.mu.Lock()
			env.fake.deploymentID = uuid.NewString() // A new database behind the same control plane.
			env.fake.mcp.servers = nil
			if tc.noOrgID {
				delete(env.fake.orgs, env.orgID)
			}
			env.fake.mu.Unlock()
			if tc.edit {
				got.Spec.Description = "pending"
				require.NoError(t, k8sClient.Update(ctx, got))
			}

			got, _ = env.reconcile(ctx, t, got)
			require.Contains(t, requireMCPServerStep(t, got, true, "CoderDeploymentChanged"), before["deploymentID"])
			require.Equal(t, 1, env.posts(), "no create in the new deployment")
			require.Equal(t, before, env.mcpServerRecord(ctx, t, got.UID), "the record keeps the old locator")

			gone, _ := env.deleteMCPServer(ctx, t, got)
			require.Nil(t, gone, "the finalizer goes without a delete")
			require.Zero(t, env.fake.requestCount(routeDeleteMCPServer), "no delete in the new deployment")
			require.Equal(t, before, env.mcpServerRecord(ctx, t, got.UID), "the record stays for a sweep")
		})
	}
}

// #353: a delete never removes a ConfigMap of the record's name that is not
// this object's record. With a status that lost everything, the record still
// names the organization, so the delete of the recorded config finishes.
func TestCoderMCPServerDeleteRecordEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("foreign ConfigMap", func(t *testing.T) {
		t.Parallel()
		env := newMCPServerEnv(ctx, t, true)
		server := env.create(ctx, t, nil)
		server.Finalizers = []string{coderv1alpha1.CoderMCPServerCleanupFinalizer}
		require.NoError(t, k8sClient.Update(ctx, server))
		foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.namespace, Name: "ck8s-cleanup-" + string(server.UID)}, Data: map[string]string{"owner": "someone else"}}
		require.NoError(t, k8sClient.Create(ctx, foreign))
		gone, _ := env.deleteMCPServer(ctx, t, server)
		require.Nil(t, gone, "nothing of the object exists in Coder, so the finalizer goes")
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign), "the foreign ConfigMap stays")
	})
	t.Run("status lost everything", func(t *testing.T) {
		t.Parallel()
		env := newMCPServerEnv(ctx, t, true)
		env.keySecret(ctx, t, testMCPKey)
		got, _ := env.applied(ctx, t, nil)
		got.Status = coderv1alpha1.CoderMCPServerStatus{}
		require.NoError(t, k8sClient.Status().Update(ctx, got))
		gone, _ := env.deleteMCPServer(ctx, t, got)
		require.Nil(t, gone)
		require.Empty(t, env.fake.mcpServers(), "the delete used the recorded ID and organization")
		require.Nil(t, env.mcpServerRecord(ctx, t, got.UID))
	})
}

// The reconciler refuses to start without its cleanup record dependencies.
func TestCoderMCPServerSetupNeedsRecordDependencies(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]*controller.CoderMCPServerReconciler{
		"no operator namespace": {Client: k8sClient, Scheme: scheme, APIReader: k8sClient},
		"no API reader":         {Client: k8sClient, Scheme: scheme, OperatorNamespace: "coder-system"},
	} {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		require.NoError(t, err)
		require.ErrorContains(t, r.SetupWithManager(mgr), "assertion failed: MCP server reconciler needs", name)
	}
}
