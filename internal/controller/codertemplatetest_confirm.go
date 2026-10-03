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

// templateTestSettleWindow is how long confirming reads look for a workspace
// after an uncertain create result. It is a guess, not a measurement: Coder
// has no known upper bound for a create request.
const templateTestSettleWindow = 15 * time.Minute

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
		startBuild, err = provenance(ctx, sdk, tt, workspace, operator.ID)
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

// provenance returns the start build that proves this test created the
// workspace: organization and template match the pinned values, and the
// operator user started the workspace's first build with the pinned version.
// It returns nil without proof. It reads the newest 100 builds: a workspace
// with more builds stays unproven, which keeps its finalizer.
//
// Coder v2.37.2 can answer the create request by claiming a prebuilt
// workspace when the parameters match a preset (coderd/workspaces.go:670-685).
// Build 1 is then the prebuild: a start build by the prebuilds system user.
// Only Coder's prebuilds reconciler starts builds as that user
// (enterprise/coderd/prebuilds/reconcile.go:1119-1122), and nobody can log in
// or get an API key as it (coderd/apikey.go:62-66). The claim build is the
// first build by anyone else: a start build by the operator with the
// requested version (coderd/workspaces.go:769-785). Any other build between
// the prebuild and the claim leaves the workspace unproven (fail-safe).
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
	prebuildsUser := uuid.MustParse(codersdk.PrebuildsSystemUserID)
	var first, claim *codersdk.WorkspaceBuild
	for i := range builds {
		b := &builds[i]
		if err := coderAnswerFor("build workspace", workspace.ID.String(), b.WorkspaceID.String()); err != nil {
			return nil, err
		}
		if b.BuildNumber == 1 {
			first = b
		}
		if b.InitiatorID != prebuildsUser && (claim == nil || b.BuildNumber < claim.BuildNumber) {
			claim = b
		}
	}
	// Without build 1 the answer proves nothing. Without a prebuild, claim is
	// build 1: a later start build by the operator does not prove who created
	// the workspace.
	if first == nil || claim == nil || claim.Transition != codersdk.WorkspaceTransitionStart || claim.InitiatorID != operatorID ||
		claim.TemplateVersionID.String() != tt.Status.TemplateVersionID {
		return nil, nil
	}
	if claim.ID == uuid.Nil {
		return nil, &coderAnswerError{msg: "assertion failed: Coder answered a start build without an ID"}
	}
	return claim, nil
}
