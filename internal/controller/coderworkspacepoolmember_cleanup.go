package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// poolMemberUnknownOutcomePoll is how often a deleted member reads Coder
// again when its create outcome is still unknown after the settle window.
const poolMemberUnknownOutcomePoll = 10 * time.Minute

// cleanup runs the delete steps of a member being deleted (plan 6.5). It
// releases the finalizer once no workspace of the member can exist, the
// workspace is deleted, the workspace was used but the pool never released
// the member, or the control plane is gone. A workspace that nothing proves
// to be the member's keeps the finalizer, and the controller never touches
// it.
func (r *CoderWorkspacePoolMemberReconciler) cleanup(ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember) (ctrl.Result, error) {
	deleted := meta.FindStatusCondition(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted)
	if !poolMemberMayHaveWorkspace(member) || (deleted != nil && deleted.Status == metav1.ConditionTrue) {
		return ctrl.Result{}, r.releaseFinalizer(ctx, member)
	}
	gone, err := controlPlaneGone(ctx, r.Client, r.APIReader, member.Namespace, member.Spec.ControlPlaneRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if gone {
		// Nothing can reach Coder any more. The docs tell admins to delete
		// leftover kwp- workspaces.
		log.FromContext(ctx).Info("The control plane is gone, so the controller releases the pool member without deleting its workspace",
			"workspaceID", member.Status.WorkspaceID, "workspaceName", member.Status.WorkspaceName)
		return ctrl.Result{}, r.releaseFinalizer(ctx, member)
	}
	if deleted != nil && deleted.Reason == "OwnershipUnknown" {
		// Only a person who removes the finalizer releases the member.
		return ctrl.Result{}, nil
	}

	before := member.Status.DeepCopy()
	step, requeue, release, err := r.cleanupStep(ctx, member)
	if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
		return ctrl.Result{}, err
	}
	if release {
		return ctrl.Result{}, r.releaseFinalizer(ctx, member)
	}
	now := r.Clock.Now()
	condition := cmp.Or(step.deleted, &metav1.Condition{Status: metav1.ConditionFalse, Reason: step.reason, Message: step.message})
	member.Status.Phase, member.Status.Reason, member.Status.Message = coderv1alpha1.CoderWorkspacePoolMemberPhaseDeleting, step.reason, step.message
	setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionReady, metav1.ConditionFalse, step.reason, step.message)
	setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, condition.Status, condition.Reason, condition.Message)
	if err := r.writeStatus(ctx, member, before); err != nil {
		return ctrl.Result{}, err
	}
	if condition.Status == metav1.ConditionTrue {
		return ctrl.Result{}, r.releaseFinalizer(ctx, member)
	}
	if condition.Reason == "OwnershipUnknown" {
		return ctrl.Result{}, nil
	}
	// A wrong Coder answer is a wait without its own poll.
	return ctrl.Result{RequeueAfter: cmp.Or(requeue, templateTestUnavailablePoll)}, nil
}

// cleanupStep runs one delete step. release means that the finalizer can go
// without a status write: no workspace of the member exists, or someone
// used it.
func (r *CoderWorkspacePoolMemberReconciler) cleanupStep(
	ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember,
) (step *coderStep, requeue time.Duration, release bool, err error) {
	sdk, _, wait, err := controlPlaneCoderClient(ctx, r.Client, member.Namespace, member.Spec.ControlPlaneRef.Name)
	switch {
	case wait != nil:
		return coderWait(wait.reason, "%s", wait.message), templateTestUnavailablePoll, false, nil
	case err != nil:
		return nil, 0, false, err
	}
	now := r.Clock.Now()
	if member.Status.WorkspaceID == "" {
		// A create request may exist: the same confirming reads as before.
		step, err := confirmPoolMemberCreate(ctx, sdk, member, now)
		switch {
		case err != nil:
			return nil, 0, false, err
		case !poolMemberMayHaveWorkspace(member):
			return nil, 0, true, nil // The reads prove that no workspace exists.
		case member.Status.WorkspaceID != "":
			return coderWait("Deleting", "The controller adopted workspace %s to delete it.", member.Status.WorkspaceName), coderBuildPoll, false, nil
		case step.deleted != nil:
			return step, 0, false, nil
		case now.Before(member.Status.CreateAttemptTime.Add(templateTestSettleWindow)):
			return step, coderBuildPoll, false, nil
		}
		step.deleted = &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "CreateOutcomeUnknown", Message: step.message}
		return step, poolMemberUnknownOutcomePoll, false, nil
	}

	workspace, step, err := guardPoolMemberDelete(ctx, sdk, member)
	if step != nil || err != nil {
		return step, templateTestUnavailablePoll, false, err
	}
	// The pool releases a used member (decision D6). With no controller left,
	// garbage collection never deletes it, so its delete is deliberate and
	// deletes its workspace, as a claim's delete does.
	released := metav1.GetControllerOf(member) == nil &&
		meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed)
	var keep func(codersdk.Workspace) (bool, error)
	if member.Spec.ClaimRef == nil && !released {
		// The last use check runs on the delete step's own read, before any
		// write, on every step, so no delete build, a retry included, starts
		// after a use (#409): a leaked workspace is better than lost work. In
		// Coder v2.37.3 a failed or canceled delete build does not move
		// last_used_at, so a failed delete alone never releases the member.
		keep = func(latest codersdk.Workspace) (bool, error) {
			if meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed) {
				return true, nil
			}
			if member.Status.ReadyLastUsedAt == nil {
				return false, nil
			}
			step, err := checkPoolMemberUse(member, latest, now)
			return step != nil, err
		}
	}

	state := workspaceDeleteState{
		workspaceID: workspace.ID, workspaceName: member.Status.WorkspaceName,
		deleteBuildID: member.Status.DeleteBuildID, deleteAttempts: member.Status.DeleteAttempts,
	}
	result, err := deleteCoderWorkspace(ctx, sdk, &state, now, false, keep)
	member.Status.DeleteBuildID, member.Status.DeleteAttempts = state.deleteBuildID, state.deleteAttempts
	switch {
	case err != nil:
		return nil, 0, false, err
	case result.kept:
		log.FromContext(ctx).Info("Someone may have used the workspace without a claim, so the controller releases the pool member and starts no delete build",
			"workspaceID", member.Status.WorkspaceID, "workspaceName", member.Status.WorkspaceName)
		return nil, 0, true, nil
	case result.deleted:
		step := coderWait("Deleted", "The delete build of workspace %s succeeded.", member.Status.WorkspaceName)
		step.deleted = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "Deleted", Message: step.message}
		return step, 0, false, nil
	case result.wait.reason == "CoderUnavailable":
		return result.wait, templateTestUnavailablePoll, false, nil // Cleanup has no deadline: poll slowly.
	}
	return result.wait, cmp.Or(result.requeue, coderBuildPoll), false, nil
}

// guardPoolMemberDelete is the identity guard before every delete write
// (plan 6.2 and 6.5 step 1). It reads deleted workspaces too and accepts any
// latest build. The owner and the name must be the member's, and the build
// history must prove that the operator user created the workspace with the
// recorded start build. A status writer can change the IDs, but not that
// history. A nil step means the member can delete the workspace.
func guardPoolMemberDelete(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember,
) (codersdk.Workspace, *coderStep, error) {
	spec, name := member.Spec, member.Status.WorkspaceName
	workspaceID, err := uuid.Parse(member.Status.WorkspaceID)
	if err != nil || workspaceID == uuid.Nil {
		return codersdk.Workspace{}, nil, fmt.Errorf("assertion failed: pool member %s/%s has workspace ID %q", member.Namespace, member.Name, member.Status.WorkspaceID)
	}
	operator, err := sdk.User(ctx, codersdk.Me)
	if err != nil {
		return codersdk.Workspace{}, coderStepUnavailable("get operator user", err), nil
	}
	if operator.ID == uuid.Nil {
		return codersdk.Workspace{}, nil, &coderAnswerError{msg: "assertion failed: Coder answered the operator user without an ID"}
	}
	workspace, err := sdk.DeletedWorkspace(ctx, workspaceID)
	if err != nil {
		return workspace, coderStepUnavailable("get workspace", err), nil
	}
	if err := coderAnswerFor("workspace", workspaceID.String(), workspace.ID.String()); err != nil {
		return workspace, nil, err
	}
	var startBuild *codersdk.WorkspaceBuild
	if workspace.OwnerID.String() == spec.OwnerID && workspace.Name == name {
		startBuild, err = workspaceProvenance(ctx, sdk, workspace, operator.ID, spec.OrganizationID, spec.TemplateID, spec.TemplateVersionID)
		var answerErr *coderAnswerError
		if errors.As(err, &answerErr) {
			return workspace, nil, err
		} else if err != nil {
			return workspace, coderStepUnavailable("get workspace builds", err), nil
		}
	}
	if startBuild == nil || startBuild.ID.String() != member.Status.StartBuildID {
		step := coderWait("OwnershipUnknown", "Nothing proves that this member created workspace %s, so the controller never deletes it. Only removing the finalizer releases the member.", workspaceID)
		step.deleted = &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "OwnershipUnknown", Message: step.message}
		return workspace, step, nil
	}
	return workspace, nil, nil
}
