package controller

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// poolMemberWatchPoll is how often a ready member reads its workspace.
	poolMemberWatchPoll = 60 * time.Second
	// poolMemberExtendBefore: an unclaimed workspace whose autostop deadline
	// is closer than this gets a new deadline (decision D5).
	poolMemberExtendBefore = 15 * time.Minute
	// poolMemberMinExtension is the shortest extension. Coder refuses a new
	// deadline less than about 30 minutes ahead.
	poolMemberMinExtension = 30 * time.Minute
)

// keepPoolMemberWarm runs the deadline steps of a ready member (plan 6.2
// step 10 rule 5 and step 11 rule 1). An unclaimed workspace gets a new
// deadline when its deadline is near. A bound workspace gets one extension at
// bind, so the claimant has a full TTL, and then never another. Each
// extension follows a fresh read that proves the workspace is this member's.
func keepPoolMemberWarm(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, now time.Time,
) (*coderStep, error) {
	name, bound := member.Status.WorkspaceName, member.Spec.ClaimRef != nil
	if bound && member.Status.BindExtended {
		// The one extension at bind is done: Coder's own autostop applies.
		return coderWait(member.Status.Reason, "%s", member.Status.Message), nil
	}
	workspace, step, err := readOwnPoolMemberWorkspace(ctx, sdk, member)
	if step != nil || err != nil {
		return step, err
	}
	deadline := workspace.LatestBuild.Deadline
	switch {
	case (!deadline.Valid || deadline.Time.IsZero()) && bound:
		member.Status.BindExtended = true
		return coderWait("Bound", "Workspace %s has no autostop deadline, so the bind needs no extension.", name), nil
	case !deadline.Valid || deadline.Time.IsZero():
		return coderWait("WorkspaceReady", "Workspace %s is ready and has no autostop deadline.", name), nil
	case !bound && deadline.Time.Sub(now) >= poolMemberExtendBefore:
		return coderWait(member.Status.Reason, "%s", member.Status.Message), nil
	}

	template, err := sdk.Template(ctx, uuid.MustParse(member.Spec.TemplateID))
	if err != nil {
		return coderStepUnavailable("get template", err), nil
	}
	if err := coderAnswerFor("template", member.Spec.TemplateID, template.ID.String()); err != nil {
		return nil, err
	}
	extendTo := now.Add(max(time.Duration(template.DefaultTTLMillis)*time.Millisecond, poolMemberMinExtension))
	err = sdk.PutExtendWorkspace(ctx, workspace.ID, codersdk.PutExtendWorkspaceRequest{Deadline: extendTo})
	switch status := coderStatus(err); {
	case err == nil:
	case status == http.StatusBadRequest || status == http.StatusConflict:
		// For example the template's max deadline. The deadline stays, and
		// a bound member does not try again.
		member.Status.BindExtended = bound
		return coderWait(member.Status.Reason, "Coder refused to extend workspace %s: %s", name, coderErrorSummary(err)), nil
	default:
		return coderStepUnavailable("extend workspace", err), nil
	}
	deadlineText := extendTo.UTC().Format(time.RFC3339)
	if bound {
		member.Status.BindExtended = true
		return coderWait("Bound", "Workspace %s is bound to claim %s. Its deadline is now %s.", name, member.Spec.ClaimRef.Name, deadlineText), nil
	}
	member.Status.LastExtendTime = &metav1.Time{Time: now}
	return coderWait("WorkspaceReady", "The controller extended the deadline of workspace %s to %s.", name, deadlineText), nil
}
