package controller_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	secretText = "token=s3cr3t-value" //nolint:gosec // Test marker, not a credential.
	// agentNamePrefix starts every fake agent name. Templates can derive agent
	// names from parameter values, so names never go into status.
	agentNamePrefix = "from-parameter-"
)

func agent(name string, status codersdk.WorkspaceAgentStatus, lifecycle codersdk.WorkspaceAgentLifecycle) codersdk.WorkspaceAgent {
	return codersdk.WorkspaceAgent{ID: uuid.New(), Name: agentNamePrefix + name, Status: status, LifecycleState: lifecycle}
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

// statusText is the stored status as JSON, for checks of what never goes in.
func (e *templateTestEnv) statusText(t *testing.T, key types.NamespacedName) string {
	t.Helper()
	tt := &coderv1alpha1.CoderTemplateTest{}
	require.NoError(t, k8sClient.Get(e.ctx, key, tt))
	raw, err := json.Marshal(tt.Status)
	require.NoError(t, err)
	return string(raw)
}

func TestTemplateTestReadinessPass(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key, _, buildID := e.startedTest(t)
	nameLookups := e.fake.requestCount(routeWorkspaceByName)
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobRunning)
	requireTemplateTestRunning(t, e.settle(t, key), "WaitingForBuild", "is running")

	// Coder reports the workspace healthy while the startup script runs.
	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobSucceeded)
	starting := agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleStarting)
	e.fake.setAgents(buildID, starting)
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.Health.Healthy = true
		return ws
	}})
	requireTemplateTestRunning(t, e.reconcile(t, key, 1), "WaitingForAgents", "Agent "+starting.ID.String()+" is connected and starting")
	require.NotContains(t, e.statusText(t, key), agentNamePrefix)

	// A ready agent that disconnects makes the test wait, not fail (plan A10).
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentDisconnected, codersdk.WorkspaceAgentLifecycleReady))
	requireTemplateTestRunning(t, e.reconcile(t, key, 1), "WaitingForAgents", "is disconnected and ready")

	// A devcontainer sub-agent that never starts does not block the pass.
	e.fake.setAgents(buildID, agent("main", codersdk.WorkspaceAgentConnected, codersdk.WorkspaceAgentLifecycleReady),
		subAgent("dev", codersdk.WorkspaceAgentLifecycleCreated))
	tt := e.reconcile(t, key, 1)
	requireTemplateTestRunning(t, tt, "AgentsReady", "")
	require.Equal(t, e.clock.Now().Unix(), tt.Status.AgentsReadyTime.Unix())

	reads := e.fake.requestCount(routeWorkspace)
	requireTemplateTestRunning(t, e.settle(t, key), "AgentsReady", "")
	require.Equal(t, reads, e.fake.requestCount(routeWorkspace), "a passed test reads no readiness again")
	require.Equal(t, nameLookups, e.fake.requestCount(routeWorkspaceByName), "the name check runs only before creation")
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
		require.NotContains(t, e.statusText(t, key), agentNamePrefix, tc.name)
		require.NotContains(t, tt.Status.Message, `""`, "%s: no empty values in the message", tc.name)
	}
}

// TestTemplateTestReadiness404 checks plan amendment A9: Coder also answers
// 404 when the caller may not read the workspace, so only 410 proves deletion.
func TestTemplateTestReadiness404(t *testing.T) {
	t.Parallel()
	e := newTemplateTestEnv(t)
	key, _, _ := e.startedTest(t)
	e.fake.failNext(routeWorkspace, fakeFault{Status: 404})
	tt := e.reconcile(t, key, 1)
	requireTemplateTestRunning(t, tt, "CoderUnavailable", "Coder answered 404")
	require.Nil(t, meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted))
	requireTemplateTestRunning(t, e.reconcile(t, key, 1), "WaitingForBuild", "is pending")
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
	key, _, buildID := e.startedTest(t)
	e.fake.failNext(routeWorkspace, fakeFault{Status: 500, Detail: secretText})
	requireTemplateTestRunning(t, e.reconcile(t, key, 1), "CoderUnavailable", "Coder answered 500: fake fault 500")
	require.NotContains(t, e.statusText(t, key), secretText)

	e.fake.setBuildJob(buildID, codersdk.ProvisionerJobFailed)
	e.fake.failNext(routeWorkspace, fakeFault{Rewrite: func(a any) any {
		ws := a.(codersdk.Workspace)
		ws.LatestBuild.Job.Error, ws.LatestBuild.Job.ErrorCode = secretText, codersdk.RequiredTemplateVariables
		return ws
	}})
	tt := e.reconcile(t, key, 1)
	requireTemplateTestFailedWith(t, tt, "BuildFailed", metav1.ConditionFalse, "CleanupPending")
	require.Contains(t, tt.Status.Message, string(codersdk.RequiredTemplateVariables))
	require.NotContains(t, e.statusText(t, key), secretText)
}
