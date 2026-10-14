package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

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
func (r *CoderWorkspacePoolReconciler) poolClaims(ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool) ([]coderv1alpha1.CoderWorkspaceClaim, error) {
	list := &coderv1alpha1.CoderWorkspaceClaimList{}
	if err := r.List(ctx, list, client.InNamespace(pool.Namespace)); err != nil {
		return nil, fmt.Errorf("list workspace claims in namespace %s: %w", pool.Namespace, err)
	}
	claims := slices.DeleteFunc(list.Items, func(c coderv1alpha1.CoderWorkspaceClaim) bool { return c.Spec.PoolName != pool.Name })
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

// bindClaims binds the pool's waiting claims, oldest first, to its ready
// unclaimed members (plan 6.3 step 5): current before outdated, then the
// oldest ready time. Ready means what the counts use: phase Ready,
// Ready=True, and not used. One Update per bind sets spec.claimRef and
// moves the controller reference from the pool to the claim, with the
// listed resourceVersion, so a member that changed after the list (for
// example, someone used it) answers 409 and stays. A conflict ends the
// reconcile. The bound members change in all, so the later steps see them
// bound. Right before each bind, the pool reads the claim again without
// the cache, so a claim whose Bound or Lost status the cache has not shown
// yet never binds a second member, and it stores ownerName in the claim
// status, so the name survives a crash between the bind and the status.
// It also sets the pool label again, in case it drifted.
func (r *CoderWorkspacePoolReconciler) bindClaims(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim,
	all []coderv1alpha1.CoderWorkspacePoolMember, counts *poolMemberCounts, ownerName string,
) error {
	if ownerName == "" {
		return fmt.Errorf("assertion failed: pool %s/%s binds claims without the owner's Coder username", pool.Namespace, pool.Name)
	}
	var free []*coderv1alpha1.CoderWorkspacePoolMember
	for i := range all {
		m := &all[i]
		controller := metav1.GetControllerOf(m)
		if controller != nil && controller.UID == pool.UID && m.Spec.ClaimRef == nil && m.DeletionTimestamp.IsZero() &&
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
	for i := range claims {
		claim := &claims[i]
		if len(free) == 0 {
			return nil
		}
		if !claim.DeletionTimestamp.IsZero() || poolClaimEverBound(claim) || poolClaimHolder(claim, all) != nil {
			continue
		}
		fresh := &coderv1alpha1.CoderWorkspaceClaim{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(claim), fresh); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("get workspace claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		if fresh.UID != claim.UID || !fresh.DeletionTimestamp.IsZero() || poolClaimEverBound(fresh) {
			*claim = *fresh
			continue
		}
		if fresh.Status.OwnerName != ownerName {
			fresh.Status.OwnerName = ownerName
			if err := r.Status().Update(ctx, fresh); err != nil {
				return fmt.Errorf("update status of workspace claim %s/%s: %w", claim.Namespace, claim.Name, err)
			}
		}
		*claim = *fresh
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
			return fmt.Errorf("assertion failed: set claim %s as controller of member %s: %w", claim.Name, m.Name, err)
		}
		if err := r.Update(ctx, bound); err != nil {
			return fmt.Errorf("bind pool member %s/%s to claim %s: %w", m.Namespace, m.Name, claim.Name, err)
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
	return nil
}

// writeClaimStatuses writes the status of every claim of the pool from the
// member that holds it (plan 6.3 step 10). ownerName is the owner's Coder
// username from this reconcile, or "" when the owner read did not run.
func (r *CoderWorkspacePoolReconciler) writeClaimStatuses(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim,
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
		ready, lost, reason, message := false, false, "WaitingForReadyWorkspace", fmt.Sprintf("CoderWorkspacePool %s has no ready workspace for this claim yet.", pool.Name)
		m := poolClaimHolder(claim, all)
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
