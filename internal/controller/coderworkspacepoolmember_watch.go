package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// poolMemberAgentLostAfter is how long an agent of a ready unclaimed
	// workspace can stay not ready before the member fails.
	poolMemberAgentLostAfter = 5 * time.Minute
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
		if step, err := checkPoolMemberUse(member, workspace, now); step != nil || err != nil {
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
// baseline from the ready read means that someone may have used the
// workspace without a claim. A nil step means the member is still unused.
//
// A template schedule update with update_workspace_last_used_at counts as
// use too (#349). It writes one value to every workspace of the template, and
// so does a usage flush after someone used each of them
// (coderd/workspacestats/tracker.go). The template does not store the flag,
// and other edits, such as a version promote, move its updated_at too, so no
// read tells the two apart.
// After such an update, every unclaimed ready member is Used: the pool
// releases it, and its workspace stays in Coder.
func checkPoolMemberUse(member *coderv1alpha1.CoderWorkspacePoolMember, workspace codersdk.Workspace, now time.Time) (*coderStep, error) {
	baseline := member.Status.ReadyLastUsedAt
	if baseline == nil {
		return nil, fmt.Errorf("assertion failed: ready pool member %s/%s has no readyLastUsedAt", member.Namespace, member.Name)
	}
	// Status stores whole seconds. Both times come from Coder's clock.
	lastUsed := workspace.LastUsedAt.Truncate(time.Second)
	if !lastUsed.After(baseline.Time) {
		return nil, nil
	}
	step := coderWait("Used", "Someone may have used workspace %s without a claim: its last_used_at moved to %s. The pool releases it.", member.Status.WorkspaceName, lastUsed.UTC().Format(time.RFC3339))
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
