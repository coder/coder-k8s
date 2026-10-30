package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// poolClaims lists the claims of the pool in its namespace, oldest first.
// The list can come from the cache: the member list, which decides the
// binds, is uncached.
func (r *CoderWorkspacePoolReconciler) poolClaims(ctx context.Context, namespace, poolName string) ([]coderv1alpha1.CoderWorkspaceClaim, error) {
	list := &coderv1alpha1.CoderWorkspaceClaimList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list workspace claims in namespace %s: %w", namespace, err)
	}
	claims := slices.DeleteFunc(list.Items, func(c coderv1alpha1.CoderWorkspaceClaim) bool { return c.Spec.PoolName != poolName })
	for i := range claims {
		if claims[i].UID == "" {
			return nil, fmt.Errorf("assertion failed: workspace claim %s/%s has no UID", claims[i].Namespace, claims[i].Name)
		}
	}
	slices.SortStableFunc(claims, func(a, b coderv1alpha1.CoderWorkspaceClaim) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Name, b.Name))
	})
	return claims, nil
}

// poolClaimWaits reports a claim that no member holds yet and that can
// still bind.
func poolClaimWaits(claim *coderv1alpha1.CoderWorkspaceClaim, all []coderv1alpha1.CoderWorkspacePoolMember) bool {
	return claim.DeletionTimestamp.IsZero() && !poolClaimEverBound(claim) && poolClaimHolder(claim, all) == nil
}

// poolWant is the number of current unclaimed members the pool wants:
// spec.replicas plus the waiting claims (decision D4), so a pool at
// replicas 0 still creates a member for each claim.
func poolWant(pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim, all []coderv1alpha1.CoderWorkspacePoolMember) int32 {
	want := *pool.Spec.Replicas
	for i := range claims {
		if poolClaimWaits(&claims[i], all) {
			want++
		}
	}
	return want
}

// reconcileOrphanClaims writes the status of the claims of a pool that is
// gone or being deleted: a claim that a member holds stays as its member
// says, so a bound claim stays Bound, and the others wait with
// PoolNotFound.
func (r *CoderWorkspacePoolReconciler) reconcileOrphanClaims(ctx context.Context, namespace, poolName string) error {
	claims, err := r.poolClaims(ctx, namespace, poolName)
	if err != nil || len(claims) == 0 {
		return err
	}
	all := &coderv1alpha1.CoderWorkspacePoolMemberList{}
	if err := r.APIReader.List(ctx, all, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list pool members in namespace %s: %w", namespace, err)
	}
	return r.writeClaimStatuses(ctx, poolName, true, claims, all.Items, "")
}

// poolClaimHolder returns the member of the claim's pool that holds the
// claim, or nil. The member is the source of truth: after a crash between
// the bind and the claim status, the next reconcile finds the claim's
// member here. spec.poolName is immutable, so a member of another pool
// never counts.
func poolClaimHolder(claim *coderv1alpha1.CoderWorkspaceClaim, all []coderv1alpha1.CoderWorkspacePoolMember) *coderv1alpha1.CoderWorkspacePoolMember {
	i := slices.IndexFunc(all, func(m coderv1alpha1.CoderWorkspacePoolMember) bool {
		return m.Spec.ClaimRef != nil && m.Spec.ClaimRef.UID == string(claim.UID) && m.Spec.PoolName == claim.Spec.PoolName
	})
	if i < 0 {
		return nil
	}
	return &all[i]
}

// poolClaimEverBound reports a claim that a member held once. Such a claim
// never binds again: when its member is gone, it becomes Lost.
func poolClaimEverBound(claim *coderv1alpha1.CoderWorkspaceClaim) bool {
	return claim.Status.BoundTime != nil || claim.Status.Phase == coderv1alpha1.CoderWorkspaceClaimPhaseBound ||
		claim.Status.Phase == coderv1alpha1.CoderWorkspaceClaimPhaseLost
}

// refreshWaitingClaims reads each claim that waits by the (cached) list
// again without the cache, so a claim whose Bound or Lost status the cache
// has not shown yet neither binds a second member nor counts as demand. A
// claim that is gone, or whose name a new claim took, leaves the list.
func (r *CoderWorkspacePoolReconciler) refreshWaitingClaims(
	ctx context.Context, claims []coderv1alpha1.CoderWorkspaceClaim, all []coderv1alpha1.CoderWorkspacePoolMember,
) ([]coderv1alpha1.CoderWorkspaceClaim, error) {
	kept := claims[:0]
	for _, claim := range claims {
		if !claim.DeletionTimestamp.IsZero() || poolClaimEverBound(&claim) || poolClaimHolder(&claim, all) != nil {
			kept = append(kept, claim)
			continue
		}
		fresh := &coderv1alpha1.CoderWorkspaceClaim{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(&claim), fresh); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("get workspace claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		if fresh.UID != claim.UID {
			continue // A new claim with the same name, maybe of another pool.
		}
		kept = append(kept, *fresh)
	}
	return kept, nil
}

// bindClaims binds the pool's waiting claims, oldest first, to its ready
// unclaimed members (plan 6.3 step 5): current before outdated, then the
// oldest ready time. Ready means what the counts use: phase Ready,
// Ready=True, and not used. One Update per bind sets spec.claimRef and
// moves the controller reference from the pool to the claim, with the
// listed resourceVersion, so a member that changed after the list (for
// example, someone used it) answers 409 and stays. A conflict ends the
// reconcile. That status is the member's last read of Coder, up to a minute
// old, so the pool reads the workspace again right before each bind and
// skips a member that this read does not prove unused (#409). skipped
// reports such a member, so the pool reads again soon. The bound members
// change in all, so the later steps see them bound. The claims come from
// refreshWaitingClaims. Before each bind, the pool stores ownerName in the
// claim status, so the name survives a crash between the bind and the
// status. It also sets the pool label again, in case it drifted.
func (r *CoderWorkspacePoolReconciler) bindClaims(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim,
	all []coderv1alpha1.CoderWorkspacePoolMember, counts *poolMemberCounts, ownerName string,
) (skipped bool, err error) {
	if ownerName == "" {
		return false, fmt.Errorf("assertion failed: pool %s/%s binds claims without the owner's Coder username", pool.Namespace, pool.Name)
	}
	var free []*coderv1alpha1.CoderWorkspacePoolMember
	for i := range all {
		m := &all[i]
		controller := metav1.GetControllerOf(m)
		// spec.poolName is immutable and decides the holder, so a member that
		// names another pool is never bound here.
		if controller != nil && controller.UID == pool.UID && m.Spec.PoolName == pool.Name && m.Spec.ClaimRef == nil && m.DeletionTimestamp.IsZero() &&
			poolMemberReady(m) && !meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed) {
			free = append(free, m)
		}
	}
	readyAt := func(m *coderv1alpha1.CoderWorkspacePoolMember) time.Time {
		return cmp.Or(m.Status.ReadyTime, &m.CreationTimestamp).Time
	}
	slices.SortStableFunc(free, func(a, b *coderv1alpha1.CoderWorkspacePoolMember) int {
		aOld, bOld := poolMemberInputMismatch(a, pool) != "", poolMemberInputMismatch(b, pool) != ""
		if aOld != bOld {
			if aOld {
				return 1
			}
			return -1
		}
		return readyAt(a).Compare(readyAt(b))
	})
	if len(free) == 0 {
		return false, nil
	}
	sdk, _, wait, err := controlPlaneCoderClient(ctx, r.Client, pool.Namespace, pool.Spec.ControlPlaneRef.Name)
	if err != nil || wait != nil {
		return wait != nil, err // No bind without a read of Coder.
	}
	for i := range claims {
		claim := &claims[i]
		if len(free) == 0 {
			return skipped, nil
		}
		if !poolClaimWaits(claim, all) {
			continue
		}
		if claim.Status.OwnerName != ownerName {
			claim.Status.OwnerName = ownerName
			if err := r.Status().Update(ctx, claim); err != nil {
				return false, fmt.Errorf("update status of workspace claim %s/%s: %w", claim.Namespace, claim.Name, err)
			}
		}
		for len(free) > 0 {
			unused, err := poolMemberUnusedNow(ctx, sdk, free[0], r.Clock.Now())
			if err != nil {
				return false, err
			}
			if unused {
				break
			}
			free, skipped = free[1:], true
		}
		if len(free) == 0 {
			return skipped, nil
		}
		m := free[0]
		current := poolMemberInputMismatch(m, pool) == ""
		bound := m.DeepCopy()
		bound.Spec.ClaimRef = &coderv1alpha1.CoderWorkspaceClaimReference{Name: claim.Name, UID: string(claim.UID)}
		bound.OwnerReferences = slices.DeleteFunc(bound.OwnerReferences, func(o metav1.OwnerReference) bool { return o.UID == pool.UID })
		if bound.Labels == nil {
			bound.Labels = map[string]string{}
		}
		bound.Labels[coderv1alpha1.CoderWorkspacePoolSelectorLabel] = string(pool.UID)
		if err := controllerutil.SetControllerReference(claim, bound, r.Scheme); err != nil {
			return false, fmt.Errorf("assertion failed: set claim %s as controller of member %s: %w", claim.Name, m.Name, err)
		}
		if err := r.Update(ctx, bound); err != nil {
			return false, fmt.Errorf("bind pool member %s/%s to claim %s: %w", m.Namespace, m.Name, claim.Name, err)
		}
		*m, free = *bound, free[1:]
		counts.unclaimed--
		counts.ready--
		counts.bound++
		if current {
			counts.current--
			counts.readyCurrent--
		} else {
			counts.outdated--
		}
	}
	return skipped, nil
}

// poolMemberUnusedNow reads the member's workspace with the member's identity
// guard and applies its use check. It reports false for a used workspace and
// for any read that proves nothing. The member's own next read then marks it
// Used or failed, and the pool releases or deletes it as usual.
func poolMemberUnusedNow(ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, now time.Time) (bool, error) {
	workspace, _, step, err := readPoolMemberWorkspace(ctx, sdk, member)
	if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil || step != nil {
		return false, err
	}
	used, err := checkPoolMemberUse(member.DeepCopy(), workspace, now)
	return used == nil, err
}

// writeClaimStatuses writes the status of every claim of the pool from the
// member that holds it (plan 6.3 step 10). A claim whose member's workspace
// is deleted becomes Lost, which is final. ownerName is the owner's Coder
// username from this reconcile, or "" when the owner read did not run.
// poolMissing marks a pool that is gone or being deleted.
func (r *CoderWorkspacePoolReconciler) writeClaimStatuses(
	ctx context.Context, poolName string, poolMissing bool, claims []coderv1alpha1.CoderWorkspaceClaim,
	all []coderv1alpha1.CoderWorkspacePoolMember, ownerName string,
) error {
	now := metav1.NewTime(r.Clock.Now())
	for i := range claims {
		claim := &claims[i]
		if claim.Status.Phase == coderv1alpha1.CoderWorkspaceClaimPhaseLost {
			continue // Final.
		}
		before := claim.Status.DeepCopy()
		st := &claim.Status
		ready, lost, reason, message := false, false, "WaitingForReadyWorkspace", fmt.Sprintf("CoderWorkspacePool %s has no ready workspace for this claim yet.", poolName)
		if poolMissing {
			reason, message = "PoolNotFound", fmt.Sprintf("CoderWorkspacePool %s does not exist or is being deleted.", poolName)
		}
		m := poolClaimHolder(claim, all)
		if m == nil && poolClaimEverBound(claim) {
			// Lost is final, so check the holder on an uncached list first:
			// another reconcile can have bound a member after this one's list.
			fresh := &coderv1alpha1.CoderWorkspacePoolMemberList{}
			if err := r.APIReader.List(ctx, fresh, client.InNamespace(claim.Namespace)); err != nil {
				return fmt.Errorf("list pool members in namespace %s: %w", claim.Namespace, err)
			}
			if h := poolClaimHolder(claim, fresh.Items); h != nil {
				m = h.DeepCopy()
			}
		}
		switch {
		case m == nil && poolClaimEverBound(claim):
			st.Phase, lost = coderv1alpha1.CoderWorkspaceClaimPhaseLost, true
			reason, message = "MemberDeleted", "The member that held this claim is gone. The claim never binds again."
		case m != nil:
			st.Phase, st.MemberName = coderv1alpha1.CoderWorkspaceClaimPhaseBound, m.Name
			st.WorkspaceID, st.WorkspaceName = m.Status.WorkspaceID, m.Status.WorkspaceName
			st.OwnerName = cmp.Or(ownerName, st.OwnerName)
			if st.BoundTime == nil {
				st.BoundTime = &now
			}
			ready = poolMemberReady(m)
			reason, message = "WorkspaceReady", fmt.Sprintf("Workspace %s is ready.", m.Status.WorkspaceName)
			if !ready {
				reason, message = cmp.Or(m.Status.Reason, "WorkspaceNotReady"), truncateTemplateTestMessage(m.Status.Message)
			}
			if meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted) {
				st.Phase, lost = coderv1alpha1.CoderWorkspaceClaimPhaseLost, true
				reason, message = "WorkspaceDeleted", fmt.Sprintf("Workspace %s was deleted. The claim never binds again.", m.Status.WorkspaceName)
			}
		default:
			st.Phase = coderv1alpha1.CoderWorkspaceClaimPhasePending
		}
		st.Reason, st.Message = reason, message
		for conditionType, ok := range map[string]bool{
			coderv1alpha1.CoderWorkspaceClaimConditionReady:       ready,
			coderv1alpha1.CoderWorkspaceClaimConditionReconciling: !ready && !lost,
			coderv1alpha1.CoderWorkspaceClaimConditionStalled:     lost,
		} {
			status := metav1.ConditionFalse
			if ok {
				status = metav1.ConditionTrue
			}
			meta.SetStatusCondition(&st.Conditions, metav1.Condition{
				Type: conditionType, Status: status, ObservedGeneration: claim.Generation, Reason: reason, Message: message, LastTransitionTime: now,
			})
		}
		if equality.Semantic.DeepEqual(before, st) {
			continue
		}
		if err := r.Status().Update(ctx, claim); err != nil {
			return fmt.Errorf("update status of workspace claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
	}
	return nil
}
