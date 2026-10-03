package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// templateTestSettleWindow is how long confirming reads look for a workspace
// after an uncertain create result. It is a guess, not a measurement: Coder
// has no known upper bound for a create request.
const templateTestSettleWindow = 15 * time.Minute

// confirmCreate reads Coder after an uncertain create result. It never sends
// a request that changes anything.
func (r *CoderTemplateTestReconciler) confirmCreate(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, now time.Time,
) (*templateTestStep, error) {
	if tt.Status.OwnerID == "" || tt.Status.WorkspaceName == "" || tt.Status.TemplateVersionID == "" {
		return nil, fmt.Errorf("assertion failed: template test %s/%s has a create marker without pinned owner, name, and version", tt.Namespace, tt.Name)
	}
	operator, err := sdk.User(ctx, codersdk.Me)
	if err != nil {
		return coderUnavailable("get operator user", err), nil
	}
	if operator.ID == uuid.Nil {
		return nil, &coderAnswerError{msg: "assertion failed: Coder answered the operator user without an ID"}
	}
	workspace, err := sdk.WorkspaceByOwnerAndName(ctx, tt.Status.OwnerID, tt.Status.WorkspaceName, codersdk.WorkspaceOptions{})
	live := err == nil
	if isCoderNotFound(err) {
		workspace, err = sdk.WorkspaceByOwnerAndName(ctx, tt.Status.OwnerID, tt.Status.WorkspaceName, codersdk.WorkspaceOptions{IncludeDeleted: true})
	}
	var startBuild *codersdk.WorkspaceBuild
	switch {
	case err == nil:
		if err := errors.Join(coderAnswerFor("workspace", tt.Status.WorkspaceName, workspace.Name),
			coderAnswerFor("workspace owner", tt.Status.OwnerID, workspace.OwnerID.String())); err != nil {
			return nil, err
		}
		startBuild, err = provenance(ctx, sdk, tt, workspace, operator.ID)
		var answerErr *coderAnswerError
		if errors.As(err, &answerErr) {
			return nil, err
		} else if err != nil {
			return coderUnavailable("get workspace builds", err), nil
		}
	case !isCoderNotFound(err):
		return coderUnavailable("get workspace by name", err), nil
	}

	switch {
	case live && startBuild != nil:
		tt.Status.WorkspaceID, tt.Status.StartBuildID = workspace.ID.String(), startBuild.ID.String()
		return templateTestWait("WaitingForBuild", "Workspace %s was created.", workspace.Name), nil
	case live:
		// Ownership is not proven either way, so the controller keeps the
		// finalizer and never touches the workspace.
		step := templateTestFail("WorkspaceNameConflict", "Workspace %s exists, but nothing proves that this test created it.", workspace.Name)
		step.deleted = &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "OwnershipUnknown", Message: step.message}
		return step, nil
	case startBuild != nil:
		step := templateTestFail("WorkspaceDeletedExternally", "Workspace %s was deleted by someone else.", workspace.Name)
		step.deleted = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "DeletedExternally", Message: step.message}
		return step, nil
	case now.Before(tt.Status.CreateAttemptTime.Add(templateTestSettleWindow)):
		return templateTestWait("ConfirmingCreate", "No workspace %s from this test found yet. Reading again.", tt.Status.WorkspaceName), nil
	}
	// No workspace of this test exists, live or deleted. The name derives
	// from the object UID, so no other test uses it. A commit after the
	// window is left to an audit of the "ktt-" prefix.
	return templateTestFailNotCreated("CreateOutcomeUnknown",
		"No workspace %s from this test appeared within %s of the create request.", tt.Status.WorkspaceName, templateTestSettleWindow), nil
}

// provenance returns the start build that proves this test created the
// workspace: organization and template match the pinned values, and the
// operator user started a build of the pinned version. It returns nil without
// proof. It reads the newest 100 builds: a workspace with more builds
// stays unproven, which keeps its finalizer.
func provenance(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, workspace codersdk.Workspace, operatorID uuid.UUID,
) (*codersdk.WorkspaceBuild, error) {
	if workspace.OrganizationID.String() != tt.Status.OrganizationID || workspace.TemplateID.String() != tt.Status.TemplateID {
		return nil, nil
	}
	builds, err := sdk.WorkspaceBuilds(ctx, codersdk.WorkspaceBuildsRequest{WorkspaceID: workspace.ID, Pagination: codersdk.Pagination{Limit: 100}})
	if err != nil {
		return nil, err
	}
	for i := range builds {
		b := &builds[i]
		if err := coderAnswerFor("build workspace", workspace.ID.String(), b.WorkspaceID.String()); err != nil {
			return nil, err
		}
		if b.Transition == codersdk.WorkspaceTransitionStart && b.InitiatorID == operatorID &&
			b.TemplateVersionID.String() == tt.Status.TemplateVersionID {
			return b, nil
		}
	}
	return nil, nil
}
