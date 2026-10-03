package controller_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// uncertainCreate runs a test up to a create request that fails with fault,
// so the next reconcile runs confirming reads.
func (e *templateTestEnv) uncertainCreate(t *testing.T, fault fakeFault, timeoutSeconds ...int32) types.NamespacedName {
	t.Helper()
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"}, timeoutSeconds...)
	e.fake.failNext(routeCreateWorkspace, fault)
	requireTemplateTestRunning(t, e.reconcile(t, key, 3), "ConfirmingCreate", "")
	return key
}

func TestTemplateTestConfirmWithoutProvenance(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	v2 := e.fake.addVersion(e.tplID, "v2", codersdk.ProvisionerJobSucceeded)
	sdk := e.fake.client(t, 5*time.Second)
	foreign := func(versionID uuid.UUID) func(t *testing.T, key types.NamespacedName) {
		return func(t *testing.T, key types.NamespacedName) {
			_, err := sdk.CreateUserWorkspace(e.ctx, e.tester.String(), codersdk.CreateWorkspaceRequest{TemplateVersionID: versionID, Name: e.workspaceName(t, key)})
			require.NoError(t, err)
		}
	}
	cases := []struct {
		name  string
		fault fakeFault
		setup func(t *testing.T, key types.NamespacedName)
	}{
		{name: "another version", fault: fakeFault{Status: 502}, setup: foreign(v2)},
		{name: "a later start build by the operator", fault: fakeFault{Status: 502}, setup: func(t *testing.T, key types.NamespacedName) {
			foreign(v2)(t, key)
			e.fake.failNext(routeWorkspaceBuilds, fakeFault{Rewrite: func(a any) any {
				builds := a.([]codersdk.WorkspaceBuild)
				later := builds[0]
				later.ID, later.BuildNumber, later.TemplateVersionID = uuid.New(), builds[0].BuildNumber+1, e.v1
				return append([]codersdk.WorkspaceBuild{later}, builds...)
			}})
		}},
		{name: "a claimed prebuild", fault: fakeFault{Status: 504, AfterCommit: true}, setup: func(*testing.T, types.NamespacedName) {
			// Build 1 belongs to the prebuilds system user, the claim build to
			// the operator. Provenance cannot prove this workspace (fail-safe).
			e.fake.failNext(routeWorkspaceBuilds, fakeFault{Rewrite: func(a any) any {
				builds := a.([]codersdk.WorkspaceBuild)
				claim := builds[0]
				claim.ID, claim.BuildNumber = uuid.New(), builds[0].BuildNumber+1
				builds[len(builds)-1].InitiatorID = uuid.MustParse(codersdk.PrebuildsSystemUserID)
				return append([]codersdk.WorkspaceBuild{claim}, builds...)
			}})
		}},
		{name: "another initiator", fault: fakeFault{Status: 504, AfterCommit: true}, setup: func(*testing.T, types.NamespacedName) {
			e.fake.failNext(routeWorkspaceBuilds, fakeFault{Rewrite: func(a any) any {
				builds := a.([]codersdk.WorkspaceBuild)
				for i := range builds {
					builds[i].InitiatorID = uuid.New()
				}
				return builds
			}})
		}},
	}
	for _, tc := range cases {
		key := e.uncertainCreate(t, tc.fault)
		tc.setup(t, key)
		changes := e.fake.requestCount(routeCreateBuild) + e.fake.requestCount(routeCancelBuild)
		tt := e.settle(t, key)
		requireTemplateTestFailedWith(t, tt, "WorkspaceNameConflict", metav1.ConditionUnknown, "OwnershipUnknown")
		require.Empty(t, tt.Status.WorkspaceID, tc.name)
		require.Equal(t, changes, e.fake.requestCount(routeCreateBuild)+e.fake.requestCount(routeCancelBuild), "%s: the controller never touches the workspace", tc.name)
	}
}

func TestTemplateTestConfirmDeletedExternally(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.uncertainCreate(t, fakeFault{Status: 504, AfterCommit: true})
	ws, err := e.fake.client(t, 5*time.Second).WorkspaceByOwnerAndName(e.ctx, e.tester.String(), e.workspaceName(t, key), codersdk.WorkspaceOptions{})
	require.NoError(t, err)
	e.fake.markDeleted(ws.ID)

	tt := e.settle(t, key)
	requireTemplateTestFailedWith(t, tt, "WorkspaceDeletedExternally", metav1.ConditionTrue, "DeletedExternally")
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace))
}

func TestTemplateTestConfirmSettleWindow(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key := e.uncertainCreate(t, fakeFault{Status: 502}, 3600)

	e.clock.SetTime(e.clock.Now().Add(15*time.Minute - time.Second))
	requireTemplateTestRunning(t, e.settle(t, key), "ConfirmingCreate", "")
	require.Equal(t, 5*time.Second, e.lastStep.RequeueAfter, "confirming reads run every 5 s")

	e.clock.SetTime(e.clock.Now().Add(time.Second))
	requireTemplateTestFailed(t, e.settle(t, key), "CreateOutcomeUnknown")
	require.Equal(t, 1, e.fake.requestCount(routeCreateWorkspace), "never a second create request")
}

func TestTemplateTestConfirmWrongAnswers(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	cases := []struct {
		name    string
		route   string
		rewrite func(any) any
		message string
	}{
		{name: "workspace by name", route: routeWorkspaceByName, message: "Coder answered workspace ", rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.Name = "other"
			return ws
		}},
		{name: "build of another workspace", route: routeWorkspaceBuilds, message: "Coder answered build workspace ", rewrite: func(a any) any {
			builds := a.([]codersdk.WorkspaceBuild)
			builds[0].WorkspaceID = uuid.New()
			return builds
		}},
		{name: "workspace without an ID", route: routeWorkspaceByName, message: "workspace without an ID", rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.ID = uuid.Nil
			return ws
		}},
		{name: "workspace by ID", route: routeWorkspace, message: "Coder answered workspace ", rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			ws.ID = uuid.New()
			return ws
		}},
		{name: "start build without an ID", route: routeWorkspaceBuilds, message: "start build without an ID", rewrite: func(a any) any {
			builds := a.([]codersdk.WorkspaceBuild)
			builds[len(builds)-1].ID = uuid.Nil
			return builds
		}},
		{name: "operator without an ID", route: routeUser, message: "operator user without an ID", rewrite: func(a any) any {
			u := a.(codersdk.User)
			u.ID = uuid.Nil
			return u
		}},
	}
	for _, tc := range cases {
		key := e.uncertainCreate(t, fakeFault{Status: 504, AfterCommit: true})
		e.fake.failNext(tc.route, fakeFault{Rewrite: tc.rewrite})
		tt := e.reconcile(t, key, 1)
		requireTemplateTestRunning(t, tt, "CoderAnswerMismatch", tc.message)
		require.Empty(t, tt.Status.WorkspaceID, tc.name)
		requireTemplateTestRunning(t, e.reconcile(t, key, 1), "WaitingForBuild", "")
	}
}

func TestTemplateTestConfirmNeedsEveryPin(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	nilID := uuid.Nil.String()
	for _, clear := range []func(*coderv1alpha1.CoderTemplateTestStatus){
		func(s *coderv1alpha1.CoderTemplateTestStatus) { s.OrganizationID = "" },
		func(s *coderv1alpha1.CoderTemplateTestStatus) { s.TemplateID = "" },
		func(s *coderv1alpha1.CoderTemplateTestStatus) { s.OwnerID = nilID },
		func(s *coderv1alpha1.CoderTemplateTestStatus) { s.OrganizationID = nilID },
		func(s *coderv1alpha1.CoderTemplateTestStatus) { s.TemplateID = nilID },
		func(s *coderv1alpha1.CoderTemplateTestStatus) { s.TemplateVersionID = nilID },
	} {
		key := e.uncertainCreate(t, fakeFault{Status: 504, AfterCommit: true})
		tt := &coderv1alpha1.CoderTemplateTest{}
		require.NoError(t, k8sClient.Get(e.ctx, key, tt))
		clear(&tt.Status)
		require.NoError(t, k8sClient.Status().Update(e.ctx, tt))
		r := &controller.CoderTemplateTestReconciler{Client: k8sClient, Scheme: scheme, Clock: e.clock}
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
		require.Error(t, err)
		require.True(t, strings.HasPrefix(err.Error(), "assertion failed:"), err.Error())
	}
}
