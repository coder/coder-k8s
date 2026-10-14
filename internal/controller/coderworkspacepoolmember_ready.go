package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// readWorkspace runs the step of a member whose create request may exist
// (plan 6.2 steps 7 to 11): it confirms an uncertain create result, applies
// the readiness rules to the start build, or runs the deadline steps of a
// ready member.
func (r *CoderWorkspacePoolMemberReconciler) readWorkspace(
	ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember, before *coderv1alpha1.CoderWorkspacePoolMemberStatus, workspaceName string, now time.Time,
) (*coderStep, error) {
	if member.Status.WorkspaceName != workspaceName {
		return nil, fmt.Errorf("assertion failed: pool member %s/%s may have a workspace, but status.workspaceName is %q", member.Namespace, member.Name, member.Status.WorkspaceName)
	}
	sdk, _, wait, err := controlPlaneCoderClient(ctx, r.Client, member.Namespace, member.Spec.ControlPlaneRef.Name)
	switch {
	case wait != nil:
		return coderWait(wait.reason, "%s", wait.message), nil
	case err != nil:
		return nil, err
	case member.Status.WorkspaceID == "":
		return confirmPoolMemberCreate(ctx, sdk, member, now)
	case member.Status.ReadyTime == nil:
		return checkPoolMemberReadiness(ctx, sdk, member, now)
	default:
		return r.watchReady(ctx, sdk, member, before, now)
	}
}

// confirmPoolMemberCreate reads Coder after an uncertain or malformed create
// answer, with the CoderTemplateTest rules. It never sends a request that
// changes anything, and it adopts only a workspace with provenance.
func confirmPoolMemberCreate(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, now time.Time,
) (*coderStep, error) {
	spec, name := member.Spec, member.Status.WorkspaceName
	operator, err := sdk.User(ctx, codersdk.Me)
	if err != nil {
		return coderStepUnavailable("get operator user", err), nil
	}
	if operator.ID == uuid.Nil {
		return nil, &coderAnswerError{msg: "assertion failed: Coder answered the operator user without an ID"}
	}
	// With include_deleted, Coder answers the live workspace first.
	workspace, err := sdk.WorkspaceByOwnerAndName(ctx, spec.OwnerID, name, codersdk.WorkspaceOptions{IncludeDeleted: true})
	found := err == nil
	// Only Coder's own 404 proves that no workspace exists (#351).
	if !found && !isCoderResourceNotFound(err) {
		return coderStepUnavailable("get workspace by name", err), nil
	}
	live := false
	var startBuild *codersdk.WorkspaceBuild
	if found {
		if err := errors.Join(coderAnswerFor("workspace", name, workspace.Name),
			coderAnswerFor("workspace owner", spec.OwnerID, workspace.OwnerID.String())); err != nil {
			return nil, err
		}
		if workspace.ID == uuid.Nil {
			return nil, &coderAnswerError{msg: "assertion failed: Coder answered a workspace without an ID"}
		}
		// Only a read by ID tells a live workspace from a deleted one (410).
		got, err := sdk.Workspace(ctx, workspace.ID)
		switch {
		case err == nil:
			if err := coderAnswerFor("workspace", workspace.ID.String(), got.ID.String()); err != nil {
				return nil, err
			}
			live = true
		case coderStatus(err) != http.StatusGone:
			return coderStepUnavailable("get workspace", err), nil
		}
		startBuild, err = workspaceProvenance(ctx, sdk, workspace, operator.ID, spec.OrganizationID, spec.TemplateID, spec.TemplateVersionID)
		var answerErr *coderAnswerError
		if errors.As(err, &answerErr) {
			return nil, err
		} else if err != nil {
			return coderStepUnavailable("get workspace builds", err), nil
		}
		// A start build with another preset is not the build this member asked for.
		if startBuild != nil && spec.PresetID != "" &&
			(startBuild.TemplateVersionPresetID == nil || startBuild.TemplateVersionPresetID.String() != spec.PresetID) {
			startBuild = nil
		}
	}

	switch {
	case live && startBuild != nil:
		member.Status.WorkspaceID, member.Status.StartBuildID = workspace.ID.String(), startBuild.ID.String()
		return coderWait("WaitingForBuild", "Workspace %s was created.", name), nil
	case live:
		// Nothing proves who created it: keep the finalizer, never touch it.
		step := coderFail("WorkspaceNameConflict", "Workspace %s exists, but nothing proves that this member created it.", name)
		step.deleted = &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "OwnershipUnknown", Message: step.message}
		return step, nil
	case startBuild != nil:
		step := coderFail("WorkspaceDeletedExternally", "Workspace %s was deleted by someone else.", name)
		step.deleted = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "DeletedExternally", Message: step.message}
		return step, nil
	case now.Before(member.Status.CreateAttemptTime.Add(templateTestSettleWindow)):
		return coderWait("ConfirmingCreate", "No workspace %s from this member found yet. Reading again.", name), nil
	case !slices.ContainsFunc(operator.Roles, func(role codersdk.SlimRole) bool { return role.Name == codersdk.RoleOwner && role.OrganizationID == "" }):
		// Coder also answers 404 when the caller may not read the workspace.
		// Only a site owner reads every workspace, so only then does a 404
		// prove that none exists. The member's deadline ends the wait.
		return coderWait("ConfirmingCreate", "No workspace %s found, but the operator user is not a site owner, so that proves nothing.", name), nil
	}
	// No workspace of this member exists, live or deleted. The name derives
	// from the member UID, so no other member uses it.
	member.Status.CreateAttemptTime = nil
	return coderFail("CreateOutcomeUnknown", "No workspace %s from this member appeared within %s of the create request.", name, templateTestSettleWindow), nil
}

// checkPoolMemberReadiness applies the readiness rules to the member's start
// build once.
func checkPoolMemberReadiness(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, now time.Time,
) (*coderStep, error) {
	workspace, step, err := readOwnPoolMemberWorkspace(ctx, sdk, member)
	if step != nil || err != nil {
		return step, err
	}
	name := member.Status.WorkspaceName
	if step := workspaceBuildReadiness(workspace.LatestBuild, name); step != nil {
		return step, nil
	}
	// last_used_at from this read is the baseline for the use check: a
	// create that Coder serves from one of its prebuilds sets it at the claim.
	member.Status.ReadyTime = &metav1.Time{Time: now}
	member.Status.ReadyLastUsedAt = &metav1.Time{Time: workspace.LastUsedAt}
	return coderWait("WorkspaceReady", "Every agent of workspace %s is ready.", name), nil
}

// readOwnPoolMemberWorkspace reads the member's workspace, proves it is the
// member's own, and requires that the latest build is the start build
// (readPoolMemberWorkspace and poolMemberStartBuildStep). A nil step means
// the workspace is this member's own and still runs its start build.
func readOwnPoolMemberWorkspace(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember,
) (codersdk.Workspace, *coderStep, error) {
	workspace, operatorID, step, err := readPoolMemberWorkspace(ctx, sdk, member)
	if step != nil || err != nil {
		return workspace, step, err
	}
	return workspace, poolMemberStartBuildStep(member, workspace, operatorID), nil
}

// readPoolMemberWorkspace reads the member's workspace and checks its
// identity (plan 6.2, identity guard) against values that no status writer
// can change: the owner from the immutable spec, the kwp- name derived from
// the UID, and the template. It also returns the operator user's ID. A nil
// step means the workspace is this member's own.
func readPoolMemberWorkspace(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember,
) (codersdk.Workspace, uuid.UUID, *coderStep, error) {
	spec, name := member.Spec, member.Status.WorkspaceName
	workspaceID, err := uuid.Parse(member.Status.WorkspaceID)
	if err != nil || workspaceID == uuid.Nil || member.Status.StartBuildID == "" {
		return codersdk.Workspace{}, uuid.Nil, nil, fmt.Errorf("assertion failed: pool member %s/%s has workspace ID %q and start build ID %q",
			member.Namespace, member.Name, member.Status.WorkspaceID, member.Status.StartBuildID)
	}
	operator, err := sdk.User(ctx, codersdk.Me)
	if err != nil {
		return codersdk.Workspace{}, uuid.Nil, coderStepUnavailable("get operator user", err), nil
	}
	if operator.ID == uuid.Nil {
		return codersdk.Workspace{}, uuid.Nil, nil, &coderAnswerError{msg: "assertion failed: Coder answered the operator user without an ID"}
	}
	workspace, err := sdk.Workspace(ctx, workspaceID)
	// Only 410 proves deletion: Coder also answers 404 when the caller may
	// not read the workspace.
	switch {
	case coderStatus(err) == http.StatusGone:
		step := coderFail("WorkspaceDeletedExternally", "Workspace %s was deleted by someone else.", name)
		step.deleted = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "DeletedExternally", Message: step.message}
		return workspace, operator.ID, step, nil
	case err != nil:
		return workspace, operator.ID, coderStepUnavailable("get workspace", err), nil
	}
	if err := errors.Join(coderAnswerFor("workspace", workspaceID.String(), workspace.ID.String()),
		coderAnswerFor("latest build workspace", workspaceID.String(), workspace.LatestBuild.WorkspaceID.String())); err != nil {
		return workspace, operator.ID, nil, err
	}
	if workspace.OwnerID.String() != spec.OwnerID || workspace.Name != name || workspace.TemplateID.String() != spec.TemplateID {
		step := coderFail("OwnershipUnknown", "Workspace %s is not the workspace of this member.", workspaceID)
		step.deleted = &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "OwnershipUnknown", Message: step.message}
		return workspace, operator.ID, step, nil
	}
	return workspace, operator.ID, nil, nil
}

// poolMemberStartBuildStep requires that the latest build of the member's
// workspace is its start build, and that the operator user started it with
// the pinned version: a status writer can name another build or another
// kwp- workspace, but not one that the operator built for this member.
func poolMemberStartBuildStep(member *coderv1alpha1.CoderWorkspacePoolMember, workspace codersdk.Workspace, operatorID uuid.UUID) *coderStep {
	build, name := workspace.LatestBuild, member.Status.WorkspaceName
	if build.ID.String() != member.Status.StartBuildID {
		return coderFail("ChangedExternally", "Someone else started build %d of workspace %s.", build.BuildNumber, name)
	}
	if build.InitiatorID != operatorID || build.Transition != codersdk.WorkspaceTransitionStart || build.TemplateVersionID.String() != member.Spec.TemplateVersionID {
		step := coderFail("OwnershipUnknown", "Nothing proves that the controller started build %d of workspace %s.", build.BuildNumber, name)
		step.deleted = &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "OwnershipUnknown", Message: step.message}
		return step
	}
	return nil
}
