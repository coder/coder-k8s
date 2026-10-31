package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
// spec.replicas plus the waiting claims that are not refused (decision D4),
// so a pool at replicas 0 still creates a member for each claim that can
// bind. A claim whose check could not read Coder still counts, so a short
// outage does not delete the member built for it.
func poolWant(
	pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim, all []coderv1alpha1.CoderWorkspacePoolMember,
	steps map[types.UID]*coderStep,
) int32 {
	want := *pool.Spec.Replicas
	for i := range claims {
		step := steps[claims[i].UID]
		if poolClaimWaits(&claims[i], all) && (step == nil || step.reason == "CoderUnavailable" || step.reason == "CoderAnswerMismatch") {
			want++
		}
	}
	return want
}

// poolClaimModeStep refuses a claim whose spec.share does not match the
// pool's claims.mode. Both are immutable, so the refusal is final.
func poolClaimModeStep(pool *coderv1alpha1.CoderWorkspacePool, claim *coderv1alpha1.CoderWorkspaceClaim) *coderStep {
	share := pool.Spec.Claims.Mode == coderv1alpha1.CoderWorkspacePoolClaimModeShare
	switch {
	case share && claim.Spec.Share == nil:
		return coderFail("ClaimModeMismatch", "CoderWorkspacePool %s is in Share mode, so the claim needs spec.share.user.", pool.Name)
	case !share && claim.Spec.Share != nil:
		return coderFail("ClaimModeMismatch", "CoderWorkspacePool %s is in %s mode, so the claim must not set spec.share.", pool.Name, pool.Spec.Claims.Mode)
	}
	return nil
}

// checkShareClaims checks the Coder user of each waiting claim of a Share
// pool before any bind (plan 6.8 item 3), and stores each refusal in steps.
// The member checks the whole policy again right before its ACL write.
func (r *CoderWorkspacePoolReconciler) checkShareClaims(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim,
	all []coderv1alpha1.CoderWorkspacePoolMember, steps map[types.UID]*coderStep,
) error {
	if pool.Spec.Claims.Mode != coderv1alpha1.CoderWorkspacePoolClaimModeShare {
		return nil
	}
	var waiting []*coderv1alpha1.CoderWorkspaceClaim
	for i := range claims {
		if poolClaimWaits(&claims[i], all) && steps[claims[i].UID] == nil {
			waiting = append(waiting, &claims[i])
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	sdk, controlPlane, wait, err := controlPlaneCoderClient(ctx, r.Client, pool.Namespace, pool.Spec.ControlPlaneRef.Name)
	if err != nil {
		return err
	}
	ownerID := pool.Status.Resolved.OwnerID
	var step *coderStep
	if wait != nil {
		step = coderWait(wait.reason, "%s", wait.message)
	} else {
		// Uncached, as in freshOwnerCap: a bind cannot be undone, so consent
		// or groups that an admin just withdrew win over the cache.
		controlPlane = &coderv1alpha1.CoderControlPlane{}
		key := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Spec.ControlPlaneRef.Name}
		if err := r.APIReader.Get(ctx, key, controlPlane); err != nil {
			return fmt.Errorf("get CoderControlPlane %s: %w", key, err)
		}
		step = workspacePoolConsent(controlPlane, ownerID, pool.Spec.Template)
	}
	if step != nil {
		for _, claim := range waiting {
			steps[claim.UID] = step
		}
		return nil
	}
	shareGroupIDs := workspacePoolOwner(controlPlane, ownerID).ShareGroupIDs
	orgID := uuid.MustParse(pool.Status.Resolved.OrganizationID) // It equals what resolve read from Coder in this reconcile.
	// Any claim author can add claims that the policy refuses. So each group
	// is read once per reconcile, a refusal holds for one poll, and a 429
	// ends the checks: such claims cost Coder at most one user read each per
	// poll.
	type groupAnswer struct {
		group codersdk.Group
		err   error
	}
	groups := map[uuid.UUID]groupAnswer{}
	readGroup := func(ctx context.Context, id uuid.UUID) (codersdk.Group, error) {
		if a, ok := groups[id]; ok {
			return a.group, a.err
		}
		group, err := sdk.Group(ctx, id, codersdk.GroupRequest{})
		groups[id] = groupAnswer{group, err}
		return group, err
	}
	now := r.Clock.Now()
	r.shareRefusals.Range(func(uid, refusal any) bool {
		if !now.Before(refusal.(poolShareRefusal).until) {
			r.shareRefusals.Delete(uid)
		}
		return true
	})
	for i, claim := range waiting {
		if refusal, ok := r.shareRefusals.Load(claim.UID); ok {
			steps[claim.UID] = refusal.(poolShareRefusal).step
			continue
		}
		_, step, err := checkWorkspaceShareUser(ctx, sdk, readGroup, orgID, shareGroupIDs, claim.Spec.Share.User)
		if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
			return err
		}
		if step == nil {
			continue
		}
		steps[claim.UID] = step
		switch {
		case step.rateLimited:
			for _, rest := range waiting[i+1:] {
				steps[rest.UID] = step
			}
			return nil
		case step.reason != "CoderUnavailable" && step.reason != "CoderAnswerMismatch":
			r.shareRefusals.Store(claim.UID, poolShareRefusal{step: step, until: now.Add(poolResolvePoll)})
		}
	}
	return nil
}

// poolShareRefusal is a Share claim's refusal until its next check.
type poolShareRefusal struct {
	step  *coderStep
	until time.Time
}

// reconcileOrphanClaims writes the status of the claims of a pool that is
// gone or being deleted: a claim that a member holds stays as its member
// says, so a bound claim stays Bound, and the others wait with
// PoolNotFound. It also finishes the idle reaps that the pool committed.
func (r *CoderWorkspacePoolReconciler) reconcileOrphanClaims(ctx context.Context, namespace, poolName string) error {
	claims, err := r.poolClaims(ctx, namespace, poolName)
	if err != nil || len(claims) == 0 {
		return err
	}
	all := &coderv1alpha1.CoderWorkspacePoolMemberList{}
	if err := r.APIReader.List(ctx, all, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list pool members in namespace %s: %w", namespace, err)
	}
	if err := r.finishIdleReaps(ctx, claims, all.Items, r.Clock.Now()); err != nil {
		return err
	}
	return r.writeClaimStatuses(ctx, poolName, true, claims, all.Items, nil, "", "")
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
// refreshWaitingClaims. Before each bind, the pool reserves the member in the
// claim status: memberName and workspaceName, with ownerName, so the name
// survives a crash between the bind and the status. The write carries the
// claim's resourceVersion, so of two reconciles that pick different members
// for one claim (two operator replicas), only one reserves. A reconcile that
// finds a reservation binds only that member until poolReservationStale says
// otherwise. It also sets the pool label again, in case it drifted.
func (r *CoderWorkspacePoolReconciler) bindClaims(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, claims []coderv1alpha1.CoderWorkspaceClaim,
	all []coderv1alpha1.CoderWorkspacePoolMember, steps map[types.UID]*coderStep, counts *poolMemberCounts, ownerName string, now time.Time,
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
		aOld, bOld := !poolMemberCurrent(a, pool, now), !poolMemberCurrent(b, pool, now)
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
		if !poolClaimWaits(claim, all) || steps[claim.UID] != nil {
			continue
		}
		follow := claim.Status.MemberName != ""
		if follow {
			stale, err := r.poolReservationStale(ctx, claim)
			if err != nil {
				return false, err
			}
			if stale {
				follow = false
			} else if !slices.ContainsFunc(free, func(m *coderv1alpha1.CoderWorkspacePoolMember) bool { return poolReserves(claim, m) }) {
				skipped = true // It holds the claim already, or this list is older than the reservation.
				continue
			}
		}
		var m *coderv1alpha1.CoderWorkspacePoolMember
		for j := 0; m == nil && j < len(free); {
			if follow && !poolReserves(claim, free[j]) {
				j++
				continue
			}
			unused, err := poolMemberUnusedNow(ctx, sdk, free[j], now)
			if err != nil {
				return false, err
			}
			if !unused {
				free, skipped = slices.Delete(free, j, j+1), true
				continue
			}
			m = free[j]
		}
		if m == nil {
			continue
		}
		workspaceName, err := uidWorkspaceName(poolMemberWorkspacePrefix, m.UID)
		if err != nil {
			return false, err
		}
		if claim.Status.OwnerName != ownerName || claim.Status.MemberName != m.Name || claim.Status.WorkspaceName != workspaceName {
			claim.Status.OwnerName, claim.Status.MemberName, claim.Status.WorkspaceName = ownerName, m.Name, workspaceName
			if err := r.Status().Update(ctx, claim); err != nil {
				return false, fmt.Errorf("reserve pool member %s for workspace claim %s/%s: %w", m.Name, claim.Namespace, claim.Name, err)
			}
		}
		current := poolMemberCurrent(m, pool, now)
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
		*m = *bound
		free = slices.DeleteFunc(free, func(f *coderv1alpha1.CoderWorkspacePoolMember) bool { return f == m })
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

// poolReserves reports whether claim reserved m: its name and the workspace
// name derived from its UID. A member that a delete and a create gave the
// name of the reserved one is another member.
func poolReserves(claim *coderv1alpha1.CoderWorkspaceClaim, m *coderv1alpha1.CoderWorkspacePoolMember) bool {
	workspaceName, err := uidWorkspaceName(poolMemberWorkspacePrefix, m.UID)
	return err == nil && m.Name == claim.Status.MemberName && workspaceName == claim.Status.WorkspaceName
}

// poolReservationStale reports whether the member that claim reserved can
// no longer be bound to it, read without the cache. Only states that a member
// never leaves count: it is gone, being deleted, failed, used, or held by
// another claim. A reconcile that reserved the member, or that found the
// reservation, binds it with the resourceVersion of a list in which it was
// free. A member that went through any of these states since then has a newer
// resourceVersion, so that update fails, and a later list never shows it
// free again. A member that holds the claim, or that is only not ready for
// now, keeps the reservation. So does a ready member whose workspace read
// keeps failing: the claim waits for it, as for any member that a bind cannot
// read.
func (r *CoderWorkspacePoolReconciler) poolReservationStale(ctx context.Context, claim *coderv1alpha1.CoderWorkspaceClaim) (bool, error) {
	m := &coderv1alpha1.CoderWorkspacePoolMember{}
	key := client.ObjectKey{Namespace: claim.Namespace, Name: claim.Status.MemberName}
	if err := r.APIReader.Get(ctx, key, m); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("get reserved pool member %s: %w", key, err)
	}
	if !poolReserves(claim, m) {
		return true, nil
	}
	if m.Spec.ClaimRef != nil {
		return m.Spec.ClaimRef.UID != string(claim.UID), nil
	}
	return !m.DeletionTimestamp.IsZero() || m.Status.Phase == coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed ||
		meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed), nil
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
// is deleted becomes Lost, which is final. ownerName is the Coder username of
// ownerID, the pool's owner, from this reconcile, or "" when the owner read
// did not run. It updates only the claims whose member ownerID owns: a new
// pool with the name of a deleted one can have another owner. A pending
// claim keeps memberName, its reservation (see bindClaims), and reports its
// step from steps, if any. A bound Share claim is ready only once its member
// shared the workspace. poolMissing marks a pool that is gone or being
// deleted.
func (r *CoderWorkspacePoolReconciler) writeClaimStatuses(
	ctx context.Context, poolName string, poolMissing bool, claims []coderv1alpha1.CoderWorkspaceClaim,
	all []coderv1alpha1.CoderWorkspacePoolMember, steps map[types.UID]*coderStep, ownerID, ownerName string,
) error {
	now := metav1.NewTime(r.Clock.Now())
	for i := range claims {
		claim := &claims[i]
		if claim.Status.Phase == coderv1alpha1.CoderWorkspaceClaimPhaseLost {
			continue // Final.
		}
		before := claim.Status.DeepCopy()
		st := &claim.Status
		ready, lost, stalled := false, false, false
		reason, message := "WaitingForReadyWorkspace", fmt.Sprintf("CoderWorkspacePool %s has no ready workspace for this claim yet.", poolName)
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
			if ownerName != "" && m.Spec.OwnerID == ownerID {
				st.OwnerName = ownerName
			}
			if st.BoundTime == nil {
				st.BoundTime = &now
			}
			memberReady := poolMemberReady(m)
			shared := meta.FindStatusCondition(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionShared)
			isShared := shared != nil && shared.Status == metav1.ConditionTrue
			ready = memberReady && (claim.Spec.Share == nil || isShared)
			st.SharedUserID = ""
			if claim.Spec.Share != nil && isShared {
				st.SharedUserID = m.Status.SharedUserID
			}
			reason, message = "WorkspaceReady", fmt.Sprintf("Workspace %s is ready.", m.Status.WorkspaceName)
			switch {
			case !memberReady:
				reason, message = cmp.Or(m.Status.Reason, "WorkspaceNotReady"), truncateTemplateTestMessage(m.Status.Message)
			case !ready && shared != nil && slices.Contains([]string{"ShareRefused", "ShareRemoved", "ShareRevoked", "ShareRevoking"}, shared.Reason):
				reason, message, stalled = shared.Reason, truncateTemplateTestMessage(shared.Message), true
			case !ready:
				reason, message = "SharingWorkspace", fmt.Sprintf("Workspace %s is being shared with Coder user %s.", m.Status.WorkspaceName, claim.Spec.Share.User)
			}
			if meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted) {
				st.Phase, lost = coderv1alpha1.CoderWorkspaceClaimPhaseLost, true
				reason, message = "WorkspaceDeleted", fmt.Sprintf("Workspace %s was deleted. The claim never binds again.", m.Status.WorkspaceName)
			}
		default:
			st.Phase = coderv1alpha1.CoderWorkspaceClaimPhasePending
			if step := steps[claim.UID]; step != nil {
				reason, message, stalled = step.reason, truncateTemplateTestMessage(step.message), step.failed
			}
		}
		st.Reason, st.Message = reason, message
		for conditionType, ok := range map[string]bool{
			coderv1alpha1.CoderWorkspaceClaimConditionReady:       ready,
			coderv1alpha1.CoderWorkspaceClaimConditionReconciling: !ready && !lost && !stalled,
			coderv1alpha1.CoderWorkspaceClaimConditionStalled:     lost || stalled,
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
