package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
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

// templateTestCreateAnswer finds the HTTP status of an uncertain create
// answer in the last message of a test: in the create step's own message, or
// in the note that keepCreateAnswer adds to each later wait.
var templateTestCreateAnswer = regexp.MustCompile(`(?:The create result is uncertain \(Coder answered|The create request got HTTP) (\d{3})\b`)

// keepCreateAnswer adds the HTTP status of an uncertain create answer, for
// example a proxy's 408, to a wait while the controller confirms the create.
// Each wait replaces the message, so without it the final DeadlineExceeded
// message no longer says why the test waited.
func keepCreateAnswer(step *templateTestStep, lastMessage string) {
	match := templateTestCreateAnswer.FindStringSubmatch(lastMessage)
	if step == nil || step.failed || match == nil {
		return
	}
	step.message += fmt.Sprintf(" The create request got HTTP %s from Coder or a proxy.", match[1])
}

// confirmCreate reads Coder after an uncertain create result. It never sends
// a request that changes anything.
func (r *CoderTemplateTestReconciler) confirmCreate(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, now time.Time,
) (*templateTestStep, error) {
	for _, pin := range []string{tt.Status.OwnerID, tt.Status.OrganizationID, tt.Status.TemplateID, tt.Status.TemplateVersionID} {
		if id, err := uuid.Parse(pin); err != nil || id == uuid.Nil || tt.Status.WorkspaceName == "" {
			return nil, fmt.Errorf("assertion failed: template test %s/%s has a create marker without a workspace name or with pinned ID %q", tt.Namespace, tt.Name, pin)
		}
	}
	operator, err := sdk.User(ctx, codersdk.Me)
	if err != nil {
		return coderUnavailable("get operator user", err), nil
	}
	if operator.ID == uuid.Nil {
		return nil, &coderAnswerError{msg: "assertion failed: Coder answered the operator user without an ID"}
	}
	// With include_deleted, Coder v2.37.2 answers the live workspace first and
	// a deleted one only without a live one (workspaceByOwnerAndName).
	workspace, err := sdk.WorkspaceByOwnerAndName(ctx, tt.Status.OwnerID, tt.Status.WorkspaceName, codersdk.WorkspaceOptions{IncludeDeleted: true})
	found := err == nil
	if !found && !isCoderNotFound(err) {
		return coderUnavailable("get workspace by name", err), nil
	}
	live := false
	var startBuild *codersdk.WorkspaceBuild
	if found {
		if err := errors.Join(coderAnswerFor("workspace", tt.Status.WorkspaceName, workspace.Name),
			coderAnswerFor("workspace owner", tt.Status.OwnerID, workspace.OwnerID.String())); err != nil {
			return nil, err
		}
		if workspace.ID == uuid.Nil {
			return nil, &coderAnswerError{msg: "assertion failed: Coder answered a workspace without an ID"}
		}
		// The answer does not say whether the workspace is deleted, so read
		// it by ID: Coder answers 410 for a deleted workspace.
		got, err := sdk.Workspace(ctx, workspace.ID)
		switch {
		case err == nil:
			if err := coderAnswerFor("workspace", workspace.ID.String(), got.ID.String()); err != nil {
				return nil, err
			}
			live = true
		case coderStatus(err) != http.StatusGone:
			return coderUnavailable("get workspace", err), nil
		}
		startBuild, err = workspaceProvenance(ctx, sdk, workspace, operator.ID, tt.Status.OrganizationID, tt.Status.TemplateID, tt.Status.TemplateVersionID)
		var answerErr *coderAnswerError
		if errors.As(err, &answerErr) {
			return nil, err
		} else if err != nil {
			return coderUnavailable("get workspace builds", err), nil
		}
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
	case !slices.ContainsFunc(operator.Roles, func(role codersdk.SlimRole) bool { return role.Name == codersdk.RoleOwner && role.OrganizationID == "" }):
		// Coder also answers 404 when the caller may not read the workspace
		// (coderd/workspaces.go:304, v2.37.2). Only a site owner reads every
		// workspace, so only then does a 404 prove that none exists.
		// Only a role change helps, so poll slowly: cleanup has no deadline.
		step := templateTestWait("ConfirmingCreate", "No workspace %s found, but the operator user is not a site owner, so that proves nothing.", tt.Status.WorkspaceName)
		step.requeue = templateTestUnavailablePoll
		return step, nil
	}
	// No workspace of this test exists, live or deleted. The name derives
	// from the object UID, so no other test uses it. A commit after the
	// window is left to an audit of the "ktt-" prefix.
	return templateTestFailNotCreated("CreateOutcomeUnknown",
		"No workspace %s from this test appeared within %s of the create request.", tt.Status.WorkspaceName, templateTestSettleWindow), nil
}
