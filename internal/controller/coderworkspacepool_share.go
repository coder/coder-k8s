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
// identity guard in this reconcile, and then reports in the Shared condition
// whether Coder's ACL still gives it. step is the member's step when no share
// is due. A refusal is a wait: the member stays ready and checks again at its
// next poll. The controller never shares a second time once Coder showed the
// share: a share that is gone stays gone.
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
	granted := shared != nil && (shared.Status == metav1.ConditionTrue || shared.Reason == "ShareRemoved")
	report := func(status metav1.ConditionStatus, reason, format string, args ...any) *coderStep {
		s := coderWait(reason, format, args...)
		setPoolMemberCondition(member, r.Clock.Now(), coderv1alpha1.CoderWorkspacePoolMemberConditionShared, status, reason, s.message)
		return s
	}
	refused := func(s *coderStep) *coderStep {
		if s.reason == "CoderUnavailable" {
			return s
		}
		return report(metav1.ConditionFalse, "ShareRefused", "Workspace %s is not shared: %s", name, s.message)
	}

	acl, err := sdk.WorkspaceACL(ctx, workspace.ID)
	if err != nil {
		return coderStepUnavailable("get workspace ACL", err), nil
	}
	switch {
	case target != "" && slices.ContainsFunc(acl.Users, func(u codersdk.WorkspaceUser) bool {
		return u.ID.String() == target && u.Role == codersdk.WorkspaceRoleUse
	}):
		// Also after a crash that lost the status write of the share.
		return report(metav1.ConditionTrue, "Shared", "Workspace %s is bound to claim %s and shared with Coder user %s.", name, claim.Name, target), nil
	case granted:
		// For example, Coder drops shares when an admin tightens the
		// organization's sharing setting.
		return report(metav1.ConditionFalse, "ShareRemoved",
			"Coder user %s no longer has the use role on workspace %s. The controller does not share it again. Delete the claim to free the workspace.", target, name), nil
	}

	// Uncached: an admin who just withdrew the consent or the groups wins.
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: member.Spec.ControlPlaneRef.Name}, controlPlane); err != nil {
		return nil, fmt.Errorf("get CoderControlPlane %s/%s: %w", member.Namespace, member.Spec.ControlPlaneRef.Name, err)
	}
	if !controlPlane.DeletionTimestamp.IsZero() {
		return coderWait("ControlPlaneDeleting", "CoderControlPlane %s is being deleted.", controlPlane.Name), nil
	}
	entry := workspacePoolOwner(controlPlane, member.Spec.OwnerID)
	if entry == nil {
		return refused(coderWait("OwnerNotAllowed", "CoderControlPlane %s does not list Coder user %s in spec.workspacePools.owners.", controlPlane.Name, member.Spec.OwnerID)), nil
	}
	owner, err := sdk.User(ctx, member.Spec.OwnerID)
	if err != nil {
		return coderStepUnavailable("get owner", err), nil
	}
	if err := coderAnswerFor("user", member.Spec.OwnerID, owner.ID.String()); err != nil {
		return nil, err
	}
	orgID := uuid.MustParse(member.Spec.OrganizationID) // assertPoolMember checked it.
	if s := checkWorkspaceSharePolicy(ctx, sdk, orgID, owner, entry.ShareGroupIDs); s != nil {
		return refused(s), nil
	}
	// After the first check, the stored user ID, never the claim's username
	// again: a username can move to another user.
	user, s, err := checkWorkspaceShareUser(ctx, sdk, orgID, entry.ShareGroupIDs, cmp.Or(target, claim.Spec.Share.User))
	if err != nil {
		return nil, err
	}
	if s != nil {
		return refused(s), nil
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
		return refused(coderWait("ShareRefused", "%s", coderErrorSummary(err))), nil
	default:
		// The ACL read at the next poll shows whether the write took effect.
		return coderStepUnavailable("update workspace ACL", err), nil
	}
}
