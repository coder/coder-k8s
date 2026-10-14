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
	// poolMemberTemplateWriteWindow: a last_used_at this close to the
	// template's updated_at comes from a template schedule update.
	poolMemberTemplateWriteWindow = 5 * time.Second
)

// watchReady runs the rules of a ready member on one read of its workspace:
// the watch rules of an unclaimed member in plan order (6.2 step 10), and
// the deadline steps (step 10 rule 5, step 11 rule 1).
func (r *CoderWorkspacePoolMemberReconciler) watchReady(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, before *coderv1alpha1.CoderWorkspacePoolMemberStatus, now time.Time,
) (*coderStep, error) {
	bound := member.Spec.ClaimRef != nil
	if (bound && member.Status.BindExtended) || meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed) {
		// The one extension at bind is done, or someone used the workspace
		// without a claim: Coder's own autostop applies, and the controller
		// never fails a used member.
		return coderWait(member.Status.Reason, "%s", member.Status.Message), nil
	}
	workspace, operatorID, step, err := readPoolMemberWorkspace(ctx, sdk, member)
	if step != nil || err != nil {
		return step, err
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
	// A template schedule update sets one last_used_at on every workspace of
	// the template, and moves the template's updated_at (plan 3.1 item 7).
	templateWide := lastUsed.Sub(template.UpdatedAt).Abs() <= poolMemberTemplateWriteWindow
	if !templateWide {
		// The template can change again after the write. Another unclaimed
		// member of the pool that moved its baseline to the same value
		// shows the same write. This member's own baseline is earlier, so
		// it never matches.
		members := &coderv1alpha1.CoderWorkspacePoolMemberList{}
		label := coderv1alpha1.CoderWorkspacePoolSelectorLabel
		if err := r.List(ctx, members, client.InNamespace(member.Namespace), client.MatchingLabels{label: member.Labels[label]}); err != nil {
			return nil, fmt.Errorf("list members of pool %s/%s: %w", member.Namespace, member.Spec.PoolName, err)
		}
		templateWide = slices.ContainsFunc(members.Items, func(m coderv1alpha1.CoderWorkspacePoolMember) bool {
			return m.Spec.ClaimRef == nil && m.Status.ReadyLastUsedAt != nil && m.Status.ReadyLastUsedAt.Time.Equal(lastUsed)
		})
	}
	if templateWide {
		member.Status.ReadyLastUsedAt = &metav1.Time{Time: lastUsed}
		return nil, nil
	}
	step := coderWait("Used", "Someone used workspace %s at %s without a claim. The pool releases it.", member.Status.WorkspaceName, lastUsed.UTC().Format(time.RFC3339))
	setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed, metav1.ConditionTrue, step.reason, step.message)
	return step, nil
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
