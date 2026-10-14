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

// keepWarm runs the deadline steps of a ready member (plan 6.2
// step 10 rule 5 and step 11 rule 1). An unclaimed workspace gets a new
// deadline when its deadline is near. A bound workspace gets one extension at
// bind, so the claimant has a full TTL, and then never another. Each
// extension follows a fresh read that proves the workspace is this member's.
func (r *CoderWorkspacePoolMemberReconciler) keepWarm(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, before *coderv1alpha1.CoderWorkspacePoolMemberStatus, now time.Time,
) (*coderStep, error) {
	name, bound := member.Status.WorkspaceName, member.Spec.ClaimRef != nil
	if bound && member.Status.BindExtended {
		// The one extension at bind is done: Coder's own autostop applies.
		return coderWait(member.Status.Reason, "%s", member.Status.Message), nil
	}
	reason := "WorkspaceReady" // A successful read replaces an earlier wait.
	if bound {
		reason = "Bound"
	}
	workspace, step, err := readOwnPoolMemberWorkspace(ctx, sdk, member)
	if step != nil || err != nil {
		return step, err
	}
	build := workspace.LatestBuild
	deadline, target := build.Deadline, member.Status.BindExtendDeadline
	switch {
	case (!deadline.Valid || deadline.Time.IsZero()) && bound:
		member.Status.BindExtended = true
		return coderWait(reason, "Workspace %s has no autostop deadline, so the bind needs no extension.", name), nil
	case !deadline.Valid || deadline.Time.IsZero():
		return coderWait(reason, "Workspace %s is ready and has no autostop deadline.", name), nil
	case bound && target != nil && !deadline.Time.Before(target.Time):
		// An earlier extend request took effect, even if its status write
		// was lost.
		member.Status.BindExtended = true
		return poolMemberBound(member, deadline.Time), nil
	case !bound && deadline.Time.Sub(now) >= poolMemberExtendBefore:
		return coderWait(reason, "Every agent of workspace %s is ready.", name), nil
	}

	template, err := sdk.Template(ctx, uuid.MustParse(member.Spec.TemplateID))
	if err != nil {
		return coderStepUnavailable("get template", err), nil
	}
	if err := coderAnswerFor("template", member.Spec.TemplateID, template.ID.String()); err != nil {
		return nil, err
	}
	// The reads can take a while, so the new deadline counts from a fresh
	// clock reading.
	extendTo := func() (time.Time, bool) {
		return poolMemberExtendTo(r.Clock.Now(), time.Duration(template.DefaultTTLMillis)*time.Millisecond, build.MaxDeadline)
	}
	to, ok := extendTo()
	switch {
	case bound && !deadline.Time.Before(to):
		// The workspace already has a full TTL: an extension would shorten it.
		member.Status.BindExtended = true
		return coderWait(reason, "Workspace %s is bound to claim %s. Its deadline %s needs no extension.",
			name, member.Spec.ClaimRef.Name, deadline.Time.UTC().Format(time.RFC3339)), nil
	case !ok && bound:
		member.Status.BindExtended = true
		return coderWait(reason, "Workspace %s is bound to claim %s. Its max deadline %s allows no extension.",
			name, member.Spec.ClaimRef.Name, build.MaxDeadline.Time.UTC().Format(time.RFC3339)), nil
	case !ok:
		// Coder would refuse the request. Rule 2 replaces the member after
		// Coder stops the workspace.
		return coderWait("MaxDeadline", "Coder stops workspace %s at its max deadline %s, which allows no extension.",
			name, build.MaxDeadline.Time.UTC().Format(time.RFC3339)), nil
	}
	if bound && target == nil {
		// Store the target before the request: a later read then proves
		// whether the extension happened, whatever status write is lost.
		member.Status.BindExtendDeadline = &metav1.Time{Time: to}
		if err := r.writeStatus(ctx, member, before); err != nil {
			return nil, err
		}
		*before = *member.Status.DeepCopy()
	}
	if bound {
		// The write can take a while: count again from a fresh clock
		// reading. Never ask for less than the stored target, so that a
		// successful request always proves the extension.
		to, _ = extendTo()
		if stored := member.Status.BindExtendDeadline.Time; to.Before(stored) {
			to = stored
		}
	}
	err = sdk.PutExtendWorkspace(ctx, workspace.ID, codersdk.PutExtendWorkspaceRequest{Deadline: to})
	switch status := coderStatus(err); {
	case err == nil:
	case status == http.StatusConflict:
		// The workspace changed after the read, for example a new build.
		// Read again at the next poll.
		return coderWait(reason, "Coder could not extend workspace %s now: %s", name, coderErrorSummary(err)), nil
	case status == http.StatusBadRequest:
		// The new deadline was too soon for Coder's clock, or past a max
		// deadline that changed after the read. The deadline stays, and a
		// bound member does not try again.
		if bound {
			member.Status.BindExtended = true
		} else {
			reason = "ExtendRefused"
		}
		return coderWait(reason, "Coder refused to extend workspace %s: %s", name, coderErrorSummary(err)), nil
	default:
		// The next poll reads the deadline: a bound member then sees
		// whether the request took effect.
		return coderStepUnavailable("extend workspace", err), nil
	}
	if bound {
		member.Status.BindExtended = true
		return poolMemberBound(member, to), nil
	}
	member.Status.LastExtendTime = &metav1.Time{Time: now}
	return coderWait(reason, "The controller extended the deadline of workspace %s to %s.", name, to.UTC().Format(time.RFC3339)), nil
}

// poolMemberExtendTo returns the deadline of an extension at now: now plus
// the template's default TTL, at least 30 minutes, at most the build's max
// deadline, in whole seconds as status stores them. It reports false when
// the max deadline leaves no extension that Coder accepts.
func poolMemberExtendTo(now time.Time, ttl time.Duration, maxDeadline codersdk.NullTime) (time.Time, bool) {
	now = now.Truncate(time.Second)
	to := now.Add(max(ttl, poolMemberMinExtension)).Truncate(time.Second)
	if maxDeadline.Valid && !maxDeadline.Time.IsZero() && to.After(maxDeadline.Time) {
		to = maxDeadline.Time.Truncate(time.Second)
	}
	return to, !to.Before(now.Add(poolMemberMinExtension))
}

func poolMemberBound(member *coderv1alpha1.CoderWorkspacePoolMember, deadline time.Time) *coderStep {
	return coderWait("Bound", "Workspace %s is bound to claim %s. Its deadline is now %s.",
		member.Status.WorkspaceName, member.Spec.ClaimRef.Name, deadline.UTC().Format(time.RFC3339))
}
