package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// deleteResult is the outcome of one delete step.
type deleteResult struct {
	deleted bool // A delete build succeeded: the workspace is gone.
	// failedBuild is true when this step counted a failed delete build of
	// this test. After a pass, that failure ends the test.
	failedBuild bool
	wait        *templateTestStep // Why the workspace still exists.
	requeue     time.Duration
}

// deleteStep runs one delete step (plan 2.6) on the workspace in
// status.workspaceID. It acts on the latest build: it cancels an active start
// or stop build, waits for an active delete build, and retries a failed
// delete build after a backoff. It never cancels a delete build and never
// sends an orphan delete. afterPass marks the delete of a passed test.
func (r *CoderTemplateTestReconciler) deleteStep(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, now time.Time, afterPass bool,
) (deleteResult, error) {
	workspaceID, err := uuid.Parse(tt.Status.WorkspaceID)
	if err != nil || workspaceID == uuid.Nil {
		return deleteResult{}, fmt.Errorf("assertion failed: template test %s/%s has workspace ID %q", tt.Namespace, tt.Name, tt.Status.WorkspaceID)
	}
	state := workspaceDeleteState{
		workspaceID: workspaceID, workspaceName: tt.Status.WorkspaceName,
		deleteBuildID: tt.Status.DeleteBuildID, deleteAttempts: tt.Status.DeleteAttempts,
	}
	// After a pass, every failed delete build counts and fails the test, even
	// one whose ID a failed status write lost, or someone else's. The test has
	// counted none yet, and the same status write makes it final, so it
	// counts once.
	result, err := deleteCoderWorkspace(ctx, sdk, &state, now, afterPass)
	tt.Status.DeleteBuildID, tt.Status.DeleteAttempts = state.deleteBuildID, state.deleteAttempts
	return deleteResult{deleted: result.deleted, failedBuild: result.failedBuild, wait: templateTestStepFrom(result.wait), requeue: result.requeue}, err
}

// deleteAfterPass runs the delete steps of a passed test. The test succeeds
// only when the delete build succeeded, and a failed delete build fails it.
func (r *CoderTemplateTestReconciler) deleteAfterPass(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, now time.Time,
) (*templateTestStep, error) {
	result, err := r.deleteStep(ctx, sdk, tt, now, true)
	switch {
	case err != nil:
		return nil, err
	case result.deleted:
		return &templateTestStep{succeeded: true, reason: "Succeeded", message: fmt.Sprintf(
			"Every agent of workspace %s was ready, and the workspace is deleted.", tt.Status.WorkspaceName)}, nil
	case result.failedBuild:
		step := templateTestFail("DeleteBuildFailed", "%s", result.wait.message)
		step.deleted = &metav1.Condition{Status: metav1.ConditionFalse, Reason: "DeleteRetrying", Message: result.wait.message}
		return step, nil
	}
	step := result.wait
	if step.reason != "CoderUnavailable" {
		step.reason = "DeletingWorkspace"
	}
	step.requeue = result.requeue
	return step, nil
}
