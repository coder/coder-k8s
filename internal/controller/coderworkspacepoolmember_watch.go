package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// poolMemberAgentLostAfter is how long an agent of a ready unclaimed
	// workspace can stay not ready before the member fails.
	poolMemberAgentLostAfter = 5 * time.Minute
	// poolMemberTemplateWriteWindow: a template schedule update sets the
	// template's updated_at, then last_used_at, in one transaction
	// (EnterpriseTemplateScheduleStore.Set). So the write comes at most this
	// long after updated_at.
	poolMemberTemplateWriteWindow = time.Second
)

// watchReady runs the rules of a ready member on one read of its workspace:
// the watch rules of an unclaimed member in plan order (6.2 step 10), and
// the deadline steps (step 10 rule 5, step 11 rule 1).
func (r *CoderWorkspacePoolMemberReconciler) watchReady(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, before *coderv1alpha1.CoderWorkspacePoolMemberStatus, now time.Time,
) (*coderStep, error) {
	bound := member.Spec.ClaimRef != nil
	if meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed) {
		// Someone used the workspace without a claim: Coder's own autostop
		// applies, and the controller never fails a used member.
		return coderWait(member.Status.Reason, "%s", member.Status.Message), nil
	}
	// A bound member only watches for the end (step 11 rule 3): the read
	// answers 410 once someone deleted the workspace, which sets
	// WorkspaceDeleted=True, and the claim becomes Lost.
	workspace, operatorID, step, err := readPoolMemberWorkspace(ctx, sdk, member)
	if step != nil || err != nil {
		return step, err
	}
	if bound && member.Status.BindExtended {
		// The one extension at bind is done. Builds by the claimant, a stop
		// included, are normal and change nothing.
		return coderWait(member.Status.Reason, "%s", member.Status.Message), nil
	}
	name := member.Status.WorkspaceName
	if !bound {
		// Rule 1 runs first, so the later rules never fail a used member.
		if step, err := r.checkPoolMemberUse(ctx, sdk, member, workspace, now); step != nil || err != nil {
			return step, err
		}
	}
	if step := poolMemberStartBuildStep(member, workspace, operatorID); step != nil {
		return step, nil // Rule 2: a stop, an update, or an autostop.
	}
	if !bound {
		if step := poolMemberAgentStep(member, workspace.LatestBuild, now); step != nil {
			return step, nil // Rule 3.
		}
		if workspace.DormantAt != nil || workspace.DeletingAt != nil {
			return coderFail("Dormant", "Workspace %s is dormant.", name), nil // Rule 4.
		}
	}
	return r.keepWarm(ctx, sdk, member, before, workspace, now)
}

// checkPoolMemberUse applies watch rule 1: a last_used_at later than the
// baseline from the ready read means that someone used the workspace without
// a claim. A template-wide write is not use: it moves the baseline instead.
// A nil step means the member is still unused.
func (r *CoderWorkspacePoolMemberReconciler) checkPoolMemberUse(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, workspace codersdk.Workspace, now time.Time,
) (*coderStep, error) {
	baseline := member.Status.ReadyLastUsedAt
	if baseline == nil {
		return nil, fmt.Errorf("assertion failed: ready pool member %s/%s has no readyLastUsedAt", member.Namespace, member.Name)
	}
	// Status stores whole seconds. Both times come from Coder's clock.
	lastUsed := workspace.LastUsedAt.Truncate(time.Second)
	if !lastUsed.After(baseline.Time) {
		return nil, nil
	}
	template, err := sdk.Template(ctx, uuid.MustParse(member.Spec.TemplateID))
	if err != nil {
		return coderStepUnavailable("get template", err), nil
	}
	if err := coderAnswerFor("template", member.Spec.TemplateID, template.ID.String()); err != nil {
		return nil, err
	}
	list, err := sdk.Workspaces(ctx, codersdk.WorkspaceFilter{Template: template.Name, Organization: template.OrganizationID.String()})
	if err != nil {
		return coderStepUnavailable("list workspaces of the template", err), nil
	}
	templateWide, err := r.templateWideWrite(ctx, member, workspace, template, list)
	if err != nil {
		return nil, err
	}
	if templateWide {
		member.Status.ReadyLastUsedAt = &metav1.Time{Time: lastUsed}
		return nil, nil
	}
	step := coderWait("Used", "Someone used workspace %s at %s without a claim. The pool releases it.", member.Status.WorkspaceName, lastUsed.UTC().Format(time.RFC3339))
	setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed, metav1.ConditionTrue, step.reason, step.message)
	return step, nil
}

// templateWideWrite reports whether the workspace's new last_used_at comes
// from a template schedule update with update_workspace_last_used_at, which
// writes one value to every workspace of the template (plan 3.1 item 7). Use
// can look the same: a usage flush writes one value to every workspace used
// since the last flush (coderd/workspacestats/tracker.go). So it is a
// template-wide write only when no workspace of the template is below the
// value, as an idle one is after use, and a sign of the write remains that
// this member's use cannot leave:
//   - the template's updated_at, which the update moves just before;
//   - an unclaimed member of the pool that moved its baseline to this value.
//
// Another workspace with the same value is no sign, whatever its state: a
// client can post usage for any workspace every minute, and Coder tracks it
// without a check (codersdk UpdateWorkspaceUsageWithBodyContext).
//
// What remains: use with no workspace of the template below it still counts
// as a template-wide write in the second after a template edit, or in the
// same flush as a member that someone used before it was ready. No sign
// moves with later flushes, so continued use is Used at the next flush,
// about a minute later: only one short use can be missed. A template-wide
// write without a sign is Used, for example when the template changes again
// before any member of the pool reads it: the pool releases the member, and
// one warm workspace leaks.
func (r *CoderWorkspacePoolMemberReconciler) templateWideWrite(
	ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember, workspace codersdk.Workspace, template codersdk.Template, list codersdk.WorkspacesResponse,
) (bool, error) {
	at := workspace.LastUsedAt
	listed := false
	same := map[string]bool{} // IDs of the other workspaces with this value.
	for _, w := range list.Workspaces {
		switch {
		case w.TemplateID != template.ID:
		case w.ID == workspace.ID:
			listed = true
		case w.LastUsedAt.Before(at):
			return false, nil
		case w.LastUsedAt.Equal(at):
			same[w.ID.String()] = true
		}
	}
	if !listed || list.Count != len(list.Workspaces) {
		return false, nil // The list does not show the whole template.
	}
	if since := at.Sub(template.UpdatedAt); since >= 0 && since <= poolMemberTemplateWriteWindow {
		return true, nil
	}
	if len(same) == 0 {
		return false, nil
	}
	members := &coderv1alpha1.CoderWorkspacePoolMemberList{}
	label := coderv1alpha1.CoderWorkspacePoolSelectorLabel
	if err := r.List(ctx, members, client.InNamespace(member.Namespace), client.MatchingLabels{label: member.Labels[label]}); err != nil {
		return false, fmt.Errorf("list members of pool %s/%s: %w", member.Namespace, member.Spec.PoolName, err)
	}
	// Status keeps whole seconds, so Coder's value must match too.
	return slices.ContainsFunc(members.Items, func(m coderv1alpha1.CoderWorkspacePoolMember) bool {
		return m.Spec.ClaimRef == nil && same[m.Status.WorkspaceID] &&
			m.Status.ReadyLastUsedAt != nil && m.Status.ReadyLastUsedAt.Time.Equal(at.Truncate(time.Second))
	}), nil
}

// poolMemberAgentStep applies watch rule 3 to the start build: an agent that
// is no longer connected and ready makes the member not ready, and after
// poolMemberAgentLostAfter it fails. A nil step means every agent is ready.
func poolMemberAgentStep(member *coderv1alpha1.CoderWorkspacePoolMember, build codersdk.WorkspaceBuild, now time.Time) *coderStep {
	step := workspaceBuildReadiness(build, member.Status.WorkspaceName)
	if step == nil {
		return nil
	}
	since := now
	if c := meta.FindStatusCondition(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady); c != nil &&
		c.Status == metav1.ConditionFalse && c.Reason == "AgentNotReady" {
		since = c.LastTransitionTime.Time
	}
	if now.Sub(since) >= poolMemberAgentLostAfter {
		return coderFail("AgentLost", "%s It has not been ready since %s.", step.message, since.UTC().Format(time.RFC3339))
	}
	return coderWait("AgentNotReady", "%s", step.message)
}
