package controller_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const secretText = "token=s3cr3t-value" //nolint:gosec // Test marker, not a credential.

func agent(name string, status codersdk.WorkspaceAgentStatus, lifecycle codersdk.WorkspaceAgentLifecycle) codersdk.WorkspaceAgent {
	return codersdk.WorkspaceAgent{ID: uuid.New(), Name: name, Status: status, LifecycleState: lifecycle}
}

func subAgent(name string, lifecycle codersdk.WorkspaceAgentLifecycle) codersdk.WorkspaceAgent {
	a := agent(name, codersdk.WorkspaceAgentConnecting, lifecycle)
	a.ParentID = uuid.NullUUID{UUID: uuid.New(), Valid: true}
	return a
}

// startedTest runs a test up to its created workspace.
func (e *templateTestEnv) startedTest(t *testing.T) (types.NamespacedName, uuid.UUID, uuid.UUID) {
	t.Helper()
	key := e.createTest(t, "default.docker", coderv1alpha1.CoderTemplateTestVersion{Name: "v1"})
	tt := e.reconcile(t, key, 3)
	requireTemplateTestRunning(t, tt, "WaitingForBuild", "was created")
	return key, uuid.MustParse(tt.Status.WorkspaceID), uuid.MustParse(tt.Status.StartBuildID)
}

func TestTemplateTestReadinessPass(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key, _, buildID := e.startedTest(t)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobRunning)
	requireTemplateTestRunning(t, e.settle(t, key), "WaitingForBuild", "is running")

	// Coder reports the workspace healthy while the startup script runs.
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleStarting))
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.Health.Healthy = true
		return ws
	}})
	requireTemplateTestRunning(t, e.reconcile(t, key, 1), "WaitingForAgents", "Agent main is connected and starting")

	// A devcontainer sub-agent that never starts does not block the pass.
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady),
		subAgent("dev", codersdk.WorkspaceAgentLifecycleCreated))
	tt := e.reconcile(t, key, 1)
	requireTemplateTestRunning(t, tt, "AgentsReady", "")
	require.Equal(t, e.clock.Now().Unix(), tt.Status.AgentsReadyTime.Unix())

	reads := e.fake.requestCount(routeWorkspace)
	requireTemplateTestRunning(t, e.settle(t, key), "AgentsReady", "")
	require.Equal(t, reads, e.fake.requestCount(routeWorkspace), "a passed test reads no readiness again")
}

func TestTemplateTestReadinessFailures(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	succeeded := func(agents ...codersdk.WorkspaceAgent) func(uuid.UUID, uuid.UUID) {
		return func(_, buildID uuid.UUID) {
			e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
			e.fake.setAgents(buildID, agents...)
		}
	}
	ready := agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady)
	cases := []struct {
		name    string
		setup   func(workspaceID, buildID uuid.UUID)
		reason  string
		deleted string // WorkspaceDeleted reason: True for DeletedExternally, else False.
	}{
		{name: "zero agents", setup: succeeded(), reason: "NoAgents"},
		{name: "only a sub-agent", setup: succeeded(subAgent("dev", codersdk.WorkspaceAgentLifecycleReady)), reason: "NoAgents"},
		{name: "connection timeout", setup: succeeded(ready, agent("b", codersdk.WorkspaceAgentTimeout, codersdk.WorkspaceAgentLifecycleCreated)), reason: "AgentConnectionTimeout"},
		{name: "start error", setup: succeeded(agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleStartError)), reason: "AgentStartError"},
		{name: "start timeout", setup: succeeded(agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleStartTimeout)), reason: "AgentStartTimeout"},
		{name: "stopped", setup: succeeded(agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleOff)), reason: "AgentStopped"},
		{name: "failure after a waiting agent", setup: succeeded(agent("a", codersdk.WorkspaceAgentConnecting, codersdk.WorkspaceAgentLifecycleCreated),
			agent("b", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleShuttingDown)), reason: "AgentStopped"},
		{name: "build failed", setup: func(_, buildID uuid.UUID) { e.fake.setBuildJob(buildID, codersdk.ProvisionerJobFailed) }, reason: "BuildFailed"},
		{name: "build canceled", setup: func(_, buildID uuid.UUID) { e.fake.setBuildJob(buildID, codersdk.ProvisionerJobCanceled) }, reason: "BuildCanceled"},
		{name: "latest build changed", setup: func(workspaceID, buildID uuid.UUID) {
			e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
			_, err := e.fake.client(t, 5*time.Second).CreateWorkspaceBuild(e.ctx, workspaceID, codersdk.CreateWorkspaceBuildRequest{Transition: codersdk.WorkspaceTransitionDelete})
			require.NoError(t, err)
		}, reason: "WorkspaceChangedExternally"},
		{name: "deleted (410)", setup: func(workspaceID, _ uuid.UUID) { e.fake.markDeleted(workspaceID) }, reason: "WorkspaceDeletedExternally", deleted: "DeletedExternally"},
		{name: "deleted (404)", setup: func(uuid.UUID, uuid.UUID) { e.fake.failNext(routeWorkspace, fakeFault{Status: 404}) }, reason: "WorkspaceDeletedExternally", deleted: "DeletedExternally"},
	}
	for _, tc := range cases {
		key, workspaceID, buildID := e.startedTest(t)
		tc.setup(workspaceID, buildID)
		tt := e.settle(t, key)
		if tc.deleted != "" {
			requireTemplateTestFailedWith(t, tt, tc.reason, metav1.ConditionTrue, tc.deleted)
			continue
		}
		requireTemplateTestFailedWith(t, tt, tc.reason, metav1.ConditionFalse, "CleanupPending")
		require.Nil(t, tt.Status.AgentsReadyTime, tc.name)
	}
}

func TestTemplateTestReadinessWrongAnswers(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	for _, rewrite := range []func(*codersdk.Workspace){
		func(ws *codersdk.Workspace) { ws.ID = uuid.New() },
		func(ws *codersdk.Workspace) { ws.LatestBuild.WorkspaceID = uuid.New() },
	} {
		key, _, _ := e.startedTest(t)
		e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
			ws := a.(codersdk.Workspace)
			rewrite(&ws)
			return ws
		}})
		requireTemplateTestRunning(t, e.reconcile(t, key, 1), "CoderAnswerMismatch", "Coder answered ")
		requireTemplateTestRunning(t, e.reconcile(t, key, 1), "WaitingForBuild", "")
	}
}

// TestTemplateTestMessageHygiene checks that error details, validation
// errors, and job errors never reach status.
func TestTemplateTestMessageHygiene(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	statusText := func(key types.NamespacedName) string {
		tt := &coderv1alpha1.CoderTemplateTest{}
		require.NoError(t, k8sClient.Get(e.ctx, key, tt))
		raw, err := json.Marshal(tt.Status)
		require.NoError(t, err)
		return string(raw)
	}

	key, _, buildID := e.startedTest(t)
	e.fake.failNext(routeWorkspace, fakeFault{Status: 500, Detail: secretText})
	requireTemplateTestRunning(t, e.reconcile(t, key, 1), "CoderUnavailable", "Coder answered 500: fake fault 500")
	require.NotContains(t, statusText(key), secretText)

	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobFailed)
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.LatestBuild.Job.Error, ws.LatestBuild.Job.ErrorCode = secretText, codersdk.RequiredTemplateVariables
		return ws
	}})
	tt := e.reconcile(t, key, 1)
	requireTemplateTestFailedWith(t, tt, "BuildFailed", metav1.ConditionFalse, "CleanupPending")
	require.Contains(t, tt.Status.Message, string(codersdk.RequiredTemplateVariables))
	require.NotContains(t, statusText(key), secretText)
}
