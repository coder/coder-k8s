package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// deleteRecorder records the options of every Delete call.
type deleteRecorder struct {
	client.Client
	opts []client.DeleteOption
}

func (d *deleteRecorder) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	d.opts = append(d.opts, opts...)
	return d.Client.Delete(ctx, obj, opts...)
}

// passTest drives a started test through a pass and the delete of its
// workspace, with several reconciles between Coder changes, like restarts.
func (e *templateTestEnv) passTest(t *testing.T, key types.NamespacedName) *coderv1alpha1.CoderTemplateTest {
	t.Helper()
	tt := e.reconcile(t, key, 6)
	buildID := uuid.MustParse(tt.Status.StartBuildID)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady))
	tt = e.reconcile(t, key, 6)
	requireTemplateTestRunning(t, tt, "DeletingWorkspace", "")
	e.fake.setBuildJob(uuid.MustParse(tt.Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	tt = e.reconcile(t, key, 6)
	require.Equal(t, coderv1alpha1.CoderTemplateTestPhaseSucceeded, tt.Status.Phase, tt.Status.Message)
	return tt
}

func TestTemplateTestRestartReplay(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	e.passTest(t, key)
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace), "exactly one create request")
	require.Equal(t, 1, e.fake.requestCount(routeCreateBuild), "exactly one delete build")
	require.Zero(t, e.fake.requestCount(routeCancelBuild))
}

// createTTLTest creates a test with a 60 s TTL after it finishes.
func (e *templateTestEnv) createTTLTest(t *testing.T) types.NamespacedName {
	t.Helper()
	ttl := int32(60)
	tt := &coderv1alpha1.CoderTemplateTest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "test-", Namespace: e.ns},
		Spec: coderv1alpha1.CoderTemplateTestSpec{
			ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"}, Template: "default.docker",
			Version: coderv1alpha1.CoderTemplateTestVersion{Name: "v1"}, TTLSecondsAfterFinished: &ttl,
		},
	}
	require.NoError(t, k8sClient.Create(e.ctx, tt))
	return types.NamespacedName{Namespace: tt.Namespace, Name: tt.Name}
}

// TestTemplateTestTTLCountsFromCleanupDelete checks that a slow Coder read in
// cleanup does not shorten the TTL: the deletion time is read after the call.
func TestTemplateTestTTLCountsFromCleanupDelete(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTTLTest(t)
	tt := e.reconcile(t, key, 3)
	requireTemplateTestRunning(t, tt, "WaitingForBuild", "was created")
	e.fake.setBuildJob(uuid.MustParse(tt.Status.StartBuildID), codersdk.ProvisionerJobFailed)
	tt = e.settle(t, key)
	requireTemplateTestFailedWith(t, tt, "BuildFailed", metav1.ConditionFalse, "Deleting")

	e.fake.setBuildJob(uuid.MustParse(tt.Status.DeleteBuildID), codersdk.ProvisionerJobSucceeded)
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		e.clock.SetTime(e.clock.Now().Add(30 * time.Second)) // The read takes 30 s.
		return a
	}})
	requireDeleted(t, e.reconcile(t, key, 1), metav1.ConditionTrue, "Deleted")
	require.Equal(t, 60*time.Second, e.lastStep.RequeueAfter, "the full TTL after the deletion")
}

func TestTemplateTestTTLAfterFinished(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.createTTLTest(t)
	finished := e.passTest(t, key)
	require.Equal(t, 60*time.Second, e.lastStep.RequeueAfter, "the controller comes back for the TTL")

	// The TTL counts from the later of completion and the workspace's
	// deletion. Both times come from the controller clock.
	e.clock.SetTime(finished.Status.CompletionTime.Add(59 * time.Second))
	require.NotNil(t, e.reconcile(t, key, 1), "not before the TTL")
	require.Equal(t, time.Second, e.lastStep.RequeueAfter)
	e.clock.SetTime(finished.Status.CompletionTime.Add(60 * time.Second))
	recorder := &deleteRecorder{Client: k8sClient}
	r := &controller.CoderTemplateTestReconciler{Client: recorder, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Nil(t, e.reconcile(t, key, 1), "deleted after the TTL")
	deleteOpts := &client.DeleteOptions{}
	deleteOpts.ApplyOptions(recorder.opts)
	require.NotNil(t, deleteOpts.Preconditions)
	require.Equal(t, finished.UID, *deleteOpts.Preconditions.UID, "only this object, never a newer one with the same name")
}
