package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// poolReasonIdleTimeout is the Lost reason of a claim whose workspace the
// pool deleted for idleness.
const poolReasonIdleTimeout = "IdleTimeout"

// poolIdleLastUsedAnnotation holds the last_used_at, in RFC 3339 with
// microseconds, that the idle check of a reaped bound member saw.
const poolIdleLastUsedAnnotation = "coder.com/idle-check-last-used-at"

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
// never go here. Lost is final, so each reap runs in this order: store the
// last_used_at of the deciding Coder read on the member, make the claim Lost
// with IdleTimeout, then delete the member. The member's cleanup keeps the
// workspace if its own last read before the delete build shows a newer
// last_used_at. A deleted member is marked deleting in all. It returns when
// the next bound member can be idle.
func (r *CoderWorkspacePoolReconciler) reapIdleClaims(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim,
	all []coderv1alpha1.CoderWorkspacePoolMember, now time.Time,
) (time.Duration, error) {
	if err := r.finishIdleReaps(ctx, claims, all, now); err != nil {
		return 0, err
	}
	timeout := pool.Spec.Claims.IdleTimeoutSeconds
	if timeout == nil {
		return 0, nil
	}
	limit := time.Duration(*timeout) * time.Second
	var sdk *codersdk.Client
	var wake time.Duration
	for i := range claims {
		claim := &claims[i]
		m := poolClaimHolder(claim, all)
		// The label and the immutable owner and control plane tie the member
		// to this pool, not to an earlier pool of the same name.
		if m == nil || !m.DeletionTimestamp.IsZero() || !claim.DeletionTimestamp.IsZero() || claim.Status.BoundTime == nil ||
			claim.Status.Phase == coderv1alpha1.CoderWorkspaceClaimPhaseLost ||
			m.Labels[coderv1alpha1.CoderWorkspacePoolSelectorLabel] != string(pool.UID) ||
			m.Spec.OwnerID != pool.Spec.OwnerUserID || m.Spec.ControlPlaneRef.Name != pool.Spec.ControlPlaneRef.Name ||
			meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted) {
			continue
		}
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
		// A deleted or foreign workspace is the member's own read to report,
		// and a read without last_used_at proves no idleness.
		if step != nil || workspace.LastUsedAt.IsZero() {
			wake = earliestWake(wake, poolMemberPendingPoll)
			continue
		}
		lastUsed := workspace.LastUsedAt.Truncate(time.Microsecond)
		if due := lastUsed.Add(limit); now.Before(due) {
			wake = earliestWake(wake, due.Sub(now))
			continue
		}
		marked := m.DeepCopy()
		metav1.SetMetaDataAnnotation(&marked.ObjectMeta, poolIdleLastUsedAnnotation, lastUsed.UTC().Format(time.RFC3339Nano))
		if err := r.Update(ctx, marked); err != nil {
			return 0, fmt.Errorf("record the idle check of pool member %s/%s: %w", m.Namespace, m.Name, err)
		}
		*m = *marked
		claim.Status.Phase = coderv1alpha1.CoderWorkspaceClaimPhaseLost
		claim.Status.Reason = poolReasonIdleTimeout
		claim.Status.Message = fmt.Sprintf("Nobody used workspace %s for %ds, so the pool deletes it. The claim never binds again.",
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

// finishIdleReaps deletes the members that Lost IdleTimeout claims still
// hold, after a crash or a failed delete, also once the pool is gone. Only a
// member with the recorded idle check goes: the pool records it before the
// claim status, so a member without it never had one.
func (r *CoderWorkspacePoolReconciler) finishIdleReaps(
	ctx context.Context, claims []coderv1alpha1.CoderWorkspaceClaim, all []coderv1alpha1.CoderWorkspacePoolMember, now time.Time,
) error {
	for i := range claims {
		claim := &claims[i]
		if claim.Status.Phase != coderv1alpha1.CoderWorkspaceClaimPhaseLost || claim.Status.Reason != poolReasonIdleTimeout {
			continue
		}
		m := poolClaimHolder(claim, all)
		if m == nil || !m.DeletionTimestamp.IsZero() {
			continue
		}
		if _, ok := m.Annotations[poolIdleLastUsedAnnotation]; !ok {
			log.FromContext(ctx).Error(nil, "A Lost IdleTimeout claim holds a member without the recorded idle check, so the pool does not delete it",
				"claim", claim.Name, "member", m.Name)
			continue
		}
		if err := r.deleteIdleMember(ctx, m, now); err != nil {
			return err
		}
	}
	return nil
}

// deleteIdleMember deletes a reaped member with UID and resourceVersion
// preconditions. Its finalizer then deletes the workspace.
func (r *CoderWorkspacePoolReconciler) deleteIdleMember(ctx context.Context, m *coderv1alpha1.CoderWorkspacePoolMember, now time.Time) error {
	uid, version := m.UID, m.ResourceVersion
	if err := r.Delete(ctx, m, client.Preconditions{UID: &uid, ResourceVersion: &version}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete idle pool member %s/%s: %w", m.Namespace, m.Name, err)
	}
	m.DeletionTimestamp = &metav1.Time{Time: now}
	return nil
}

// poolIdleReapBaseline returns the last_used_at that the pool's idle check
// saw, when the member is being deleted for idleness: the member has the
// recorded check, and its claim, read without the cache, is Lost with
// IdleTimeout and not being deleted. A recorded check alone is no reap: the
// claim status write after it can have failed. A claim's delete is the
// claimant's own choice, so it gets no use check.
func poolIdleReapBaseline(ctx context.Context, reader client.Reader, member *coderv1alpha1.CoderWorkspacePoolMember) (*time.Time, error) {
	value, ok := member.Annotations[poolIdleLastUsedAnnotation]
	if !ok || member.Spec.ClaimRef == nil {
		return nil, nil
	}
	baseline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("assertion failed: pool member %s/%s has idle check %q: %w", member.Namespace, member.Name, value, err)
	}
	claim := &coderv1alpha1.CoderWorkspaceClaim{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: member.Namespace, Name: member.Spec.ClaimRef.Name}, claim); apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("get workspace claim %s/%s: %w", member.Namespace, member.Spec.ClaimRef.Name, err)
	}
	if string(claim.UID) != member.Spec.ClaimRef.UID || !claim.DeletionTimestamp.IsZero() ||
		claim.Status.Phase != coderv1alpha1.CoderWorkspaceClaimPhaseLost || claim.Status.Reason != poolReasonIdleTimeout {
		return nil, nil
	}
	return &baseline, nil
}
