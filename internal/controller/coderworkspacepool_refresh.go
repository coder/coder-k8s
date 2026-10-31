package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// poolReasonIdleTimeout is the Lost reason of a claim whose workspace the
// pool deleted for idleness.
const poolReasonIdleTimeout = "IdleTimeout"

// poolMemberCurrent reports a member that pins the pool's resolved inputs
// and has not been ready for spec.maxUnclaimedAgeSeconds yet (plan 6.6 item
// 1). Any other unclaimed member is outdated: the pool replaces it, new
// before old.
func poolMemberCurrent(m *coderv1alpha1.CoderWorkspacePoolMember, pool *coderv1alpha1.CoderWorkspacePool, now time.Time) bool {
	refreshAt := poolMemberRefreshAt(m, pool)
	return poolMemberInputMismatch(m, pool) == "" && (refreshAt.IsZero() || now.Before(refreshAt))
}

// poolMemberRefreshAt is when a ready member gets too old to stay unclaimed,
// or zero without spec.maxUnclaimedAgeSeconds or a ready time.
func poolMemberRefreshAt(m *coderv1alpha1.CoderWorkspacePoolMember, pool *coderv1alpha1.CoderWorkspacePool) time.Time {
	if pool.Spec.MaxUnclaimedAgeSeconds == nil || m.Status.ReadyTime == nil {
		return time.Time{}
	}
	return m.Status.ReadyTime.Add(time.Duration(*pool.Spec.MaxUnclaimedAgeSeconds) * time.Second)
}

// poolRefreshWake is when the next current unclaimed member of the pool gets
// too old. No event reports that, so the pool polls for it.
func poolRefreshWake(pool *coderv1alpha1.CoderWorkspacePool, members []coderv1alpha1.CoderWorkspacePoolMember, now time.Time) time.Duration {
	var wake time.Duration
	for i := range members {
		m := &members[i]
		controller := metav1.GetControllerOf(m)
		if controller == nil || controller.UID != pool.UID || m.Spec.ClaimRef != nil || !m.DeletionTimestamp.IsZero() || !poolMemberCurrent(m, pool, now) {
			continue
		}
		if refreshAt := poolMemberRefreshAt(m, pool); !refreshAt.IsZero() {
			wake = earliestWake(wake, refreshAt.Sub(now))
		}
	}
	return wake
}

// earliestWake returns the earlier of two requeue delays, where zero means
// none.
func earliestWake(a, b time.Duration) time.Duration {
	if a <= 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// reapIdleClaims deletes the pool's bound members whose bind and whose
// workspace's last_used_at are both older than spec.claims.idleTimeoutSeconds
// (plan 6.6 item 2, decision D9). Unclaimed members are idle by design and
// never go here. The use check runs on a Coder read right before the claim
// becomes Lost with IdleTimeout. Lost is final, so the claim status goes
// first: after a crash before the member delete, the next reconcile deletes
// the member of a Lost IdleTimeout claim. Coder writes last_used_at once a
// minute, so a session that starts less than a minute before the read can
// still be lost. A deleted member is marked deleting in all. It returns when
// the next bound member can be idle.
func (r *CoderWorkspacePoolReconciler) reapIdleClaims(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim, all []coderv1alpha1.CoderWorkspacePoolMember,
) (time.Duration, error) {
	now := r.Clock.Now()
	var sdk *codersdk.Client
	var wake time.Duration
	for i := range claims {
		claim := &claims[i]
		m := poolClaimHolder(claim, all)
		if m == nil || !m.DeletionTimestamp.IsZero() || !claim.DeletionTimestamp.IsZero() ||
			m.Labels[coderv1alpha1.CoderWorkspacePoolSelectorLabel] != string(pool.UID) {
			continue
		}
		if claim.Status.Phase == coderv1alpha1.CoderWorkspaceClaimPhaseLost {
			if claim.Status.Reason == poolReasonIdleTimeout {
				if err := r.deleteIdleMember(ctx, m, now); err != nil {
					return 0, err
				}
			}
			continue
		}
		timeout := pool.Spec.Claims.IdleTimeoutSeconds
		if timeout == nil || claim.Status.BoundTime == nil ||
			meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted) {
			continue
		}
		limit := time.Duration(*timeout) * time.Second
		if due := claim.Status.BoundTime.Add(limit); now.Before(due) {
			wake = earliestWake(wake, due.Sub(now))
			continue
		}
		if sdk == nil {
			var wait *controlPlaneWait
			var err error
			if sdk, _, wait, err = controlPlaneCoderClient(ctx, r.Client, pool.Namespace, pool.Spec.ControlPlaneRef.Name); err != nil || wait != nil {
				return earliestWake(wake, poolMemberPendingPoll), err
			}
		}
		workspace, _, step, err := readPoolMemberWorkspace(ctx, sdk, m)
		if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
			return 0, err
		}
		if step != nil {
			// The member's own read reports a deleted or foreign workspace.
			wake = earliestWake(wake, poolMemberPendingPoll)
			continue
		}
		if due := workspace.LastUsedAt.Add(limit); now.Before(due) {
			wake = earliestWake(wake, due.Sub(now))
			continue
		}
		claim.Status.Phase = coderv1alpha1.CoderWorkspaceClaimPhaseLost
		claim.Status.Reason = poolReasonIdleTimeout
		claim.Status.Message = fmt.Sprintf("Nobody used workspace %s for %ds, so the pool deleted it. The claim never binds again.",
			m.Status.WorkspaceName, *timeout)
		for conditionType, status := range map[string]metav1.ConditionStatus{
			coderv1alpha1.CoderWorkspaceClaimConditionReady:       metav1.ConditionFalse,
			coderv1alpha1.CoderWorkspaceClaimConditionReconciling: metav1.ConditionFalse,
			coderv1alpha1.CoderWorkspaceClaimConditionStalled:     metav1.ConditionTrue,
		} {
			meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
				Type: conditionType, Status: status, ObservedGeneration: claim.Generation,
				Reason: poolReasonIdleTimeout, Message: claim.Status.Message, LastTransitionTime: metav1.NewTime(now),
			})
		}
		if err := r.Status().Update(ctx, claim); err != nil {
			return 0, fmt.Errorf("mark workspace claim %s/%s idle: %w", claim.Namespace, claim.Name, err)
		}
		if err := r.deleteIdleMember(ctx, m, now); err != nil {
			return 0, err
		}
	}
	return wake, nil
}

// deleteIdleMember deletes the member of a Lost IdleTimeout claim. Its
// finalizer then deletes the workspace.
func (r *CoderWorkspacePoolReconciler) deleteIdleMember(ctx context.Context, m *coderv1alpha1.CoderWorkspacePoolMember, now time.Time) error {
	uid, version := m.UID, m.ResourceVersion
	if err := r.Delete(ctx, m, client.Preconditions{UID: &uid, ResourceVersion: &version}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete idle pool member %s/%s: %w", m.Namespace, m.Name, err)
	}
	m.DeletionTimestamp = &metav1.Time{Time: now}
	return nil
}
