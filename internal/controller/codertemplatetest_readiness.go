package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// checkReadiness reads the workspace once and applies the readiness rules
// (plan 2.5) in order. Workspace.Health.Healthy is never the success signal:
// it is true while agents still run their startup scripts.
func (r *CoderTemplateTestReconciler) checkReadiness(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, now time.Time,
) (*templateTestStep, error) {
	name := tt.Status.WorkspaceName
	if tt.Status.AgentsReadyTime != nil {
		// The delete steps come with plan PR 5.
		return templateTestWait("AgentsReady", "Every agent of workspace %s was ready. The delete steps are not enabled yet.", name), nil
	}
	workspaceID, err := uuid.Parse(tt.Status.WorkspaceID)
	if err != nil || workspaceID == uuid.Nil || tt.Status.StartBuildID == "" {
		return nil, fmt.Errorf("assertion failed: template test %s/%s has workspace ID %q and start build ID %q", tt.Namespace, tt.Name, tt.Status.WorkspaceID, tt.Status.StartBuildID)
	}
	workspace, err := sdk.Workspace(ctx, workspaceID)
	switch status := coderStatus(err); {
	case status == http.StatusNotFound || status == http.StatusGone:
		step := templateTestFail("WorkspaceDeletedExternally", "Workspace %s was deleted by someone else.", name)
		step.deleted = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "DeletedExternally", Message: step.message}
		return step, nil
	case err != nil:
		return coderUnavailable("get workspace", err), nil
	}
	build := workspace.LatestBuild
	if err := errors.Join(coderAnswerFor("workspace", workspaceID.String(), workspace.ID.String()),
		coderAnswerFor("latest build workspace", workspaceID.String(), build.WorkspaceID.String())); err != nil {
		return nil, err
	}
	if build.ID.String() != tt.Status.StartBuildID {
		return templateTestFail("WorkspaceChangedExternally", "Someone else started build %d of workspace %s.", build.BuildNumber, name), nil
	}

	switch build.Job.Status {
	case codersdk.ProvisionerJobSucceeded:
	case codersdk.ProvisionerJobFailed:
		// Only the error code: Job.Error can hold Terraform output and secrets.
		return templateTestFail("BuildFailed", "The start build of workspace %s failed (error code %q).", name, build.Job.ErrorCode), nil
	case codersdk.ProvisionerJobCanceling, codersdk.ProvisionerJobCanceled:
		// This controller cancels only during cleanup, which comes with plan PR 5.
		return templateTestFail("BuildCanceled", "Someone else canceled the start build of workspace %s.", name), nil
	default:
		return templateTestWait("WaitingForBuild", "The start build of workspace %s is %s.", name, build.Job.Status), nil
	}

	var agents []codersdk.WorkspaceAgent
	for _, resource := range build.Resources {
		for _, agent := range resource.Agents {
			if !agent.ParentID.Valid { // Devcontainer sub-agents have a parent.
				agents = append(agents, agent)
			}
		}
	}
	if len(agents) == 0 {
		// Coder reports a workspace without agents as healthy: a false pass.
		return templateTestFail("NoAgents", "Workspace %s has no agents, so nothing proves that it works.", name), nil
	}
	// Messages name agents by ID: a template can derive agent names from
	// parameter values, which never go into status.
	var waiting *codersdk.WorkspaceAgent
	for i := range agents {
		agent := &agents[i]
		switch agent.LifecycleState {
		case codersdk.WorkspaceAgentLifecycleStartError:
			return templateTestFail("AgentStartError", "The startup script of agent %s failed.", agent.ID), nil
		case codersdk.WorkspaceAgentLifecycleStartTimeout:
			return templateTestFail("AgentStartTimeout", "The startup script of agent %s timed out.", agent.ID), nil
		case codersdk.WorkspaceAgentLifecycleShuttingDown, codersdk.WorkspaceAgentLifecycleShutdownTimeout,
			codersdk.WorkspaceAgentLifecycleShutdownError, codersdk.WorkspaceAgentLifecycleOff:
			return templateTestFail("AgentStopped", "Agent %s is %s.", agent.ID, agent.LifecycleState), nil
		}
		switch {
		case agent.Status == codersdk.WorkspaceAgentTimeout:
			return templateTestFail("AgentConnectionTimeout", "Agent %s did not connect in time.", agent.ID), nil
		case waiting == nil && (agent.Status != codersdk.WorkspaceAgentConnected || agent.LifecycleState != codersdk.WorkspaceAgentLifecycleReady):
			waiting = agent
		}
	}
	if waiting != nil {
		return templateTestWait("WaitingForAgents", "Agent %s is %s and %s.", waiting.ID, waiting.Status, waiting.LifecycleState), nil
	}
	tt.Status.AgentsReadyTime = &metav1.Time{Time: now}
	return templateTestWait("AgentsReady", "Every agent of workspace %s is ready. The delete steps are not enabled yet.", name), nil
}
