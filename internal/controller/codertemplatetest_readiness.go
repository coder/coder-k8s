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
		// The pass is recorded: delete the workspace, then succeed.
		return r.deleteAfterPass(ctx, sdk, tt, now)
	}
	workspaceID, err := uuid.Parse(tt.Status.WorkspaceID)
	if err != nil || workspaceID == uuid.Nil || tt.Status.StartBuildID == "" {
		return nil, fmt.Errorf("assertion failed: template test %s/%s has workspace ID %q and start build ID %q", tt.Namespace, tt.Name, tt.Status.WorkspaceID, tt.Status.StartBuildID)
	}
	workspace, err := sdk.Workspace(ctx, workspaceID)
	// Only 410 proves deletion: Coder soft-deletes workspaces, and it also
	// answers 404 when the caller may not read the workspace (plan A9).
	switch {
	case coderStatus(err) == http.StatusGone:
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

	if step := workspaceBuildReadiness(build, name); step != nil {
		return templateTestStepFrom(step), nil
	}
	tt.Status.AgentsReadyTime = &metav1.Time{Time: now}
	return templateTestWait("AgentsReady", "Every agent of workspace %s is ready. The controller deletes the workspace next.", name), nil
}
