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

const (
	// The delete retry backoff doubles from 1 m to its 30 m cap. Both values
	// are guesses, not measurements (plan A3).
	templateTestDeleteBackoffBase = time.Minute
	templateTestDeleteBackoffCap  = 30 * time.Minute
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
// sends an orphan delete.
func (r *CoderTemplateTestReconciler) deleteStep(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, now time.Time,
) (deleteResult, error) {
	name := tt.Status.WorkspaceName
	wait := func(requeue time.Duration, reason, format string, args ...any) (deleteResult, error) {
		return deleteResult{wait: templateTestWait(reason, format, args...), requeue: requeue}, nil
	}
	workspaceID, err := uuid.Parse(tt.Status.WorkspaceID)
	if err != nil || workspaceID == uuid.Nil {
		return deleteResult{}, fmt.Errorf("assertion failed: template test %s/%s has workspace ID %q", tt.Namespace, tt.Name, tt.Status.WorkspaceID)
	}
	// include_deleted: a workspace whose delete build succeeded stays
	// readable. A 404 is not proof of deletion (plan A9).
	workspace, err := sdk.DeletedWorkspace(ctx, workspaceID)
	if err != nil {
		return deleteResult{wait: coderUnavailable("get workspace", err), requeue: templateTestRunningPoll}, nil
	}
	build := workspace.LatestBuild
	if err := errors.Join(coderAnswerFor("workspace", workspaceID.String(), workspace.ID.String()),
		coderAnswerFor("latest build workspace", workspaceID.String(), build.WorkspaceID.String())); err != nil {
		return deleteResult{}, err
	}

	status := build.Job.Status
	switch {
	case build.Transition == codersdk.WorkspaceTransitionDelete && status == codersdk.ProvisionerJobSucceeded:
		return deleteResult{deleted: true}, nil
	case build.Transition == codersdk.WorkspaceTransitionDelete && status.Active():
		// Any initiator: a delete build by someone else also deletes it.
		tt.Status.DeleteBuildID = build.ID.String()
		return wait(templateTestRunningPoll, "Deleting", "The delete build of workspace %s is %s.", name, status)
	case build.Transition == codersdk.WorkspaceTransitionDelete && status != codersdk.ProvisionerJobFailed && status != codersdk.ProvisionerJobCanceled:
		// An unknown status proves no failure: wait.
		return wait(templateTestRunningPoll, "Deleting", "The delete build of workspace %s is %s.", name, status)
	case build.Transition == codersdk.WorkspaceTransitionDelete && build.ID.String() == tt.Status.DeleteBuildID:
		// Count each failed delete build once. The status write stores the
		// count before any new delete build.
		tt.Status.DeleteAttempts++
		tt.Status.DeleteBuildID = ""
		retryAt := templateTestDeleteRetryAt(build, tt.Status.DeleteAttempts, now)
		// At least 1 s: the count must be stored before the next delete build.
		result, _ := wait(max(retryAt.Sub(now), time.Second), "DeleteRetrying", "Delete build %d of workspace %s ended %s. The controller retries at %s.",
			tt.Status.DeleteAttempts, name, status, retryAt.UTC().Format(time.RFC3339))
		result.failedBuild = true
		return result, nil
	case build.Transition == codersdk.WorkspaceTransitionDelete:
		// Already counted, or someone else's: retry after the backoff.
		if retryAt := templateTestDeleteRetryAt(build, tt.Status.DeleteAttempts, now); now.Before(retryAt) {
			return wait(retryAt.Sub(now), "DeleteRetrying", "The last delete build of workspace %s ended %s. The controller retries at %s.", name, status, retryAt.UTC().Format(time.RFC3339))
		}
	case status == codersdk.ProvisionerJobPending || status == codersdk.ProvisionerJobRunning:
		// expect_status makes Coder refuse the cancel when the job moved on.
		err := sdk.CancelWorkspaceBuild(ctx, build.ID, codersdk.CancelWorkspaceBuildParams{ExpectStatus: codersdk.CancelWorkspaceBuildStatus(status)})
		if err != nil && coderStatus(err) != http.StatusPreconditionFailed && coderStatus(err) != http.StatusBadRequest {
			return deleteResult{wait: coderUnavailable("cancel build", err), requeue: templateTestRunningPoll}, nil
		}
		return wait(templateTestRunningPoll, "Deleting", "The controller canceled the %s build of workspace %s before deleting it.", build.Transition, name)
	case status == codersdk.ProvisionerJobCanceling:
		return wait(templateTestRunningPoll, "Deleting", "The %s build of workspace %s is canceling.", build.Transition, name)
	}

	deleteBuild, err := sdk.CreateWorkspaceBuild(ctx, workspaceID, codersdk.CreateWorkspaceBuildRequest{Transition: codersdk.WorkspaceTransitionDelete})
	switch {
	case coderStatus(err) == http.StatusConflict:
		// A build became active: read again.
		return wait(templateTestRunningPoll, "Deleting", "Another build of workspace %s started. Reading again.", name)
	case err != nil:
		return deleteResult{wait: coderUnavailable("create delete build", err), requeue: templateTestRunningPoll}, nil
	}
	if err := errors.Join(coderAnswerFor("delete build workspace", workspaceID.String(), deleteBuild.WorkspaceID.String()),
		coderAnswerFor("delete build transition", string(codersdk.WorkspaceTransitionDelete), string(deleteBuild.Transition))); err != nil {
		return deleteResult{}, err
	}
	if deleteBuild.ID == uuid.Nil {
		return deleteResult{}, &coderAnswerError{msg: "assertion failed: Coder answered a delete build without an ID"}
	}
	tt.Status.DeleteBuildID = deleteBuild.ID.String()
	return wait(templateTestRunningPoll, "Deleting", "The controller started a delete build of workspace %s.", name)
}

// deleteAfterPass runs the delete steps of a passed test. The test succeeds
// only when the delete build succeeded, and a failed delete build fails it.
func (r *CoderTemplateTestReconciler) deleteAfterPass(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, now time.Time,
) (*templateTestStep, error) {
	result, err := r.deleteStep(ctx, sdk, tt, now)
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

// templateTestDeleteRetryAt is when the controller may replace a failed delete
// build: its completion plus the backoff.
func templateTestDeleteRetryAt(build codersdk.WorkspaceBuild, attempts int32, now time.Time) time.Time {
	completedAt := now
	if build.Job.CompletedAt != nil {
		completedAt = *build.Job.CompletedAt
	}
	return completedAt.Add(templateTestDeleteBackoff(attempts))
}

// templateTestDeleteBackoff is the wait after the given number of failed
// delete builds: 1 m, 2 m, 4 m, and so on, capped at 30 m.
func templateTestDeleteBackoff(attempts int32) time.Duration {
	backoff := templateTestDeleteBackoffBase
	for i := int32(1); i < attempts && backoff < templateTestDeleteBackoffCap; i++ {
		backoff *= 2
	}
	return min(backoff, templateTestDeleteBackoffCap)
}
