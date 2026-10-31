package controller

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// The share policy of Share mode (#153 plan 6.8). The operator user is a site
// owner, and Coder lets a site owner share where the organization's sharing
// setting forbids the workspace owner to share. So the controller enforces
// that setting itself, and never changes it.

// checkWorkspaceSharePolicy applies plan 6.8 items 1 and 2. A nil step means
// the organization lets owner share.
func checkWorkspaceSharePolicy(ctx context.Context, sdk *codersdk.Client, orgID uuid.UUID, owner codersdk.User, shareGroupIDs []string) *coderStep {
	if len(shareGroupIDs) == 0 {
		return coderWait("ShareNotAllowed", "The control plane lists no shareGroupIDs for Coder user %s, so Share mode is off for this owner.", owner.ID)
	}
	settings, err := sdk.WorkspaceSharingSettings(ctx, orgID.String())
	if err != nil {
		return coderStepUnavailable("get workspace sharing settings", err)
	}
	// Coder v2.37 also answers none then, but the flag alone suffices.
	if settings.SharingGloballyDisabled {
		return coderWait("ShareNotAllowed", "The Coder deployment disables workspace sharing.")
	}
	switch settings.ShareableWorkspaceOwners {
	case codersdk.ShareableWorkspaceOwnersEveryone:
		return nil
	case codersdk.ShareableWorkspaceOwnersServiceAccounts:
		if owner.IsServiceAccount {
			return nil
		}
		return coderWait("ShareNotAllowed", "Organization %s lets only service accounts share workspaces, and Coder user %s is not one.", orgID, owner.ID)
	default:
		return coderWait("ShareNotAllowed", "Organization %s does not allow workspace sharing (shareable_workspace_owners %q).", orgID, settings.ShareableWorkspaceOwners)
	}
}

// checkWorkspaceShareUser applies plan 6.8 item 3 to the user that a Share
// claim names (ident, a username or a UUID) and returns that user. A nil step
// means the user can get the share.
func checkWorkspaceShareUser(ctx context.Context, sdk *codersdk.Client, orgID uuid.UUID, shareGroupIDs []string, ident string) (codersdk.User, *coderStep, error) {
	user, err := sdk.User(ctx, ident)
	if isCoderNotFound(err) {
		return user, coderWait("ShareUserNotFound", "Coder user %q does not exist.", ident), nil
	} else if err != nil {
		return user, coderStepUnavailable("get share user", err), nil
	}
	answered := strings.ToLower(user.Username) // Coder matches usernames without case.
	if id, err := uuid.Parse(ident); err == nil && id.String() == ident {
		answered = user.ID.String()
	}
	if err := coderAnswerFor("user", strings.ToLower(ident), answered); err != nil {
		return user, nil, err
	}
	if user.ID == uuid.Nil {
		return user, nil, &coderAnswerError{msg: "assertion failed: Coder answered a user without an ID"}
	}
	if user.ID == uuid.MustParse(codersdk.PrebuildsSystemUserID) {
		return user, coderFail("ShareUserNotEligible", "Coder user %q is the Coder prebuilds system user, which never gets a share.", ident), nil
	}
	if user.Status != codersdk.UserStatusActive {
		return user, coderWait("ShareUserNotEligible", "Coder user %s has status %q. Only active users can get a share.", user.Username, user.Status), nil
	}
	if !slices.Contains(user.OrganizationIDs, orgID) {
		return user, coderWait("ShareUserNotEligible", "Coder user %s is not a member of organization %s.", user.Username, orgID), nil
	}
	var unread strings.Builder
	for _, entry := range shareGroupIDs {
		groupID, err := uuid.Parse(entry)
		if err != nil {
			return user, nil, fmt.Errorf("assertion failed: shareGroupIDs entry %q passed validation but is not a UUID: %w", entry, err)
		}
		// The template organization's Everyone group has the organization's
		// ID, and its members are the organization's members. Reading it
		// would need a license. Another organization's ID is read like any
		// group.
		if groupID == orgID {
			return user, nil, nil
		}
		group, err := sdk.Group(ctx, groupID, codersdk.GroupRequest{})
		switch status := coderStatus(err); {
		case err == nil:
		case status == http.StatusForbidden || status == http.StatusNotFound:
			// 403: groups need a Premium license. 404: no such group.
			_, _ = fmt.Fprintf(&unread, " Group %s could not be read: %s", groupID, coderErrorSummary(err))
			continue
		default:
			return user, coderStepUnavailable("get group", err), nil
		}
		if err := coderAnswerFor("group", groupID.String(), group.ID.String()); err != nil {
			return user, nil, err
		}
		if slices.ContainsFunc(group.Members, func(m codersdk.ReducedUser) bool { return m.ID == user.ID }) {
			return user, nil, nil
		}
	}
	return user, coderWait("ShareUserNotEligible", "Coder user %s is in none of the groups in shareGroupIDs.%s", user.Username, unread.String()), nil
}

// shareWorkspace gives the Coder user of the member's Share claim the use
// role on workspace (plan 6.2 step 11 rule 2), which the caller read with the
// identity guard in this reconcile, and reports in the Shared condition
// whether Coder's ACL gives it now. It checks the share policy at every poll
// and removes a share that the policy no longer allows. step is the member's
// step when no share is due. A refusal is a wait: the member stays ready and
// checks again at its next poll. A share that is gone, or that the controller
// removed, stays gone.
func (r *CoderWorkspacePoolMemberReconciler) shareWorkspace(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, before *coderv1alpha1.CoderWorkspacePoolMemberStatus,
	workspace codersdk.Workspace, step *coderStep,
) (*coderStep, error) {
	ref := member.Spec.ClaimRef
	claim := &coderv1alpha1.CoderWorkspaceClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: ref.Name}, claim); apierrors.IsNotFound(err) {
		return step, nil // Garbage collection deletes the member.
	} else if err != nil {
		return nil, fmt.Errorf("get workspace claim %s/%s: %w", member.Namespace, ref.Name, err)
	}
	if string(claim.UID) != ref.UID || claim.Spec.Share == nil {
		return step, nil
	}
	name, target := member.Status.WorkspaceName, member.Status.SharedUserID
	shared := meta.FindStatusCondition(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionShared)
	granted := shared != nil && (shared.Status == metav1.ConditionTrue || slices.Contains([]string{"ShareRemoved", "ShareRevoked", "ShareRevoking"}, shared.Reason))
	report := func(status metav1.ConditionStatus, reason, format string, args ...any) *coderStep {
		s := coderWait(reason, format, args...)
		setPoolMemberCondition(member, r.Clock.Now(), coderv1alpha1.CoderWorkspacePoolMemberConditionShared, status, reason, s.message)
		return s
	}

	acl, err := sdk.WorkspaceACL(ctx, workspace.ID)
	if err != nil {
		return coderStepUnavailable("get workspace ACL", err), nil
	}
	has := target != "" && slices.ContainsFunc(acl.Users, func(u codersdk.WorkspaceUser) bool {
		return u.ID.String() == target && u.Role == codersdk.WorkspaceRoleUse
	})
	switch {
	case !has && granted && shared.Reason == "ShareRevoked":
		return coderWait(shared.Reason, "%s", shared.Message), nil
	case !has && granted && shared.Reason == "ShareRevoking":
		return report(metav1.ConditionFalse, "ShareRevoked", "The controller removed the share of workspace %s with Coder user %s. It does not share it again.", name, target), nil
	case !has && granted:
		// For example, Coder drops shares when an admin tightens the
		// organization's sharing setting.
		return report(metav1.ConditionFalse, "ShareRemoved",
			"Coder user %s no longer has the use role on workspace %s. The controller does not share it again. Delete the claim to free the workspace.", target, name), nil
	}

	// After the first check, the stored user ID, never the claim's username
	// again: a username can move to another user.
	user, s, err := r.checkSharePolicy(ctx, sdk, member, cmp.Or(target, claim.Spec.Share.User))
	switch {
	case err != nil:
		return nil, err
	case s != nil && (s.reason == "CoderUnavailable" || s.reason == "ControlPlaneDeleting"):
		return s, nil // Nothing proves a refusal: the share stays as it is.
	case has && s == nil:
		// Also after a crash that lost the status write of the share.
		return report(metav1.ConditionTrue, "Shared", "Workspace %s is bound to claim %s and shared with Coder user %s.", name, claim.Name, target), nil
	case has:
		// The admin withdrew what allowed the share, for example removed the
		// user from every listed group.
		err := sdk.UpdateWorkspaceACL(ctx, workspace.ID, codersdk.UpdateWorkspaceACL{
			UserRoles: map[string]codersdk.WorkspaceRole{target: codersdk.WorkspaceRoleDeleted},
		})
		if err != nil {
			return report(metav1.ConditionFalse, "ShareRevoking", "The share of workspace %s with Coder user %s is no longer allowed: %s The controller could not remove it yet: %s",
				name, target, s.message, coderErrorSummary(err)), nil
		}
		return report(metav1.ConditionFalse, "ShareRevoked", "The controller removed the share of workspace %s with Coder user %s: %s It does not share it again.",
			name, target, s.message), nil
	case s != nil:
		return report(metav1.ConditionFalse, "ShareRefused", "Workspace %s is not shared: %s", name, s.message), nil
	}
	if target == "" {
		// Stored before the write, so a retry after a crash shares with the
		// same user.
		member.Status.SharedUserID = user.ID.String()
		if err := r.writeStatus(ctx, member, before); err != nil {
			return nil, err
		}
		*before = *member.Status.DeepCopy()
	}

	// Only the use role, never admin, and never a group role.
	err = sdk.UpdateWorkspaceACL(ctx, workspace.ID, codersdk.UpdateWorkspaceACL{
		UserRoles: map[string]codersdk.WorkspaceRole{member.Status.SharedUserID: codersdk.WorkspaceRoleUse},
	})
	switch {
	case err == nil:
		return report(metav1.ConditionTrue, "Shared", "Workspace %s is bound to claim %s and shared with Coder user %s.", name, claim.Name, member.Status.SharedUserID), nil
	case coderStatus(err) == http.StatusForbidden:
		return report(metav1.ConditionFalse, "ShareRefused", "Workspace %s is not shared: %s", name, coderErrorSummary(err)), nil
	default:
		// The ACL read at the next poll shows whether the write took effect.
		return coderStepUnavailable("update workspace ACL", err), nil
	}
}

// checkSharePolicy applies the share policy (plan 6.8) to the Coder user
// ident on fresh inputs: the control plane read without the cache, so an
// admin who just withdrew the consent or the groups wins, the owner, the
// organization's setting, and the user. A step is a refusal, or a wait with
// CoderUnavailable or ControlPlaneDeleting that proves nothing.
func (r *CoderWorkspacePoolMemberReconciler) checkSharePolicy(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, ident string,
) (codersdk.User, *coderStep, error) {
	var user codersdk.User
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: member.Spec.ControlPlaneRef.Name}, controlPlane); err != nil {
		return user, nil, fmt.Errorf("get CoderControlPlane %s/%s: %w", member.Namespace, member.Spec.ControlPlaneRef.Name, err)
	}
	if !controlPlane.DeletionTimestamp.IsZero() {
		return user, coderWait("ControlPlaneDeleting", "CoderControlPlane %s is being deleted.", controlPlane.Name), nil
	}
	entry := workspacePoolOwner(controlPlane, member.Spec.OwnerID)
	if entry == nil {
		return user, coderWait("OwnerNotAllowed", "CoderControlPlane %s does not list Coder user %s in spec.workspacePools.owners.", controlPlane.Name, member.Spec.OwnerID), nil
	}
	owner, err := sdk.User(ctx, member.Spec.OwnerID)
	if err != nil {
		return user, coderStepUnavailable("get owner", err), nil
	}
	if err := coderAnswerFor("user", member.Spec.OwnerID, owner.ID.String()); err != nil {
		return user, nil, err
	}
	orgID := uuid.MustParse(member.Spec.OrganizationID) // assertPoolMember checked it.
	if s := checkWorkspaceSharePolicy(ctx, sdk, orgID, owner, entry.ShareGroupIDs); s != nil {
		return user, s, nil
	}
	return checkWorkspaceShareUser(ctx, sdk, orgID, entry.ShareGroupIDs, ident)
}
