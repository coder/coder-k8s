package controller_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
)

// shareEnv is a poolMemberEnv whose pool is "sharers" in Share mode. The
// owner lists the template organization's ID in shareGroupIDs, which names
// its Everyone group. Coder user bob is an active member of that
// organization. Every test that uses it checks that the controller never
// changed a sharing setting (plan T6).
type shareEnv struct {
	*poolMemberEnv
	bob uuid.UUID
}

func newShareEnv(t *testing.T) *shareEnv {
	t.Helper()
	e := &shareEnv{poolMemberEnv: newPoolMemberEnv(t)}
	spec := e.pool.Spec.DeepCopy()
	spec.Claims.Mode = coderv1alpha1.CoderWorkspacePoolClaimModeShare
	e.pool = &coderv1alpha1.CoderWorkspacePool{ObjectMeta: metav1.ObjectMeta{Name: "sharers", Namespace: e.ns}, Spec: *spec}
	require.NoError(t, k8sClient.Create(e.ctx, e.pool))
	e.resolve(t, nil)
	e.setShareGroups(t, e.orgID.String())
	e.bob = e.fake.addUser("bob", codersdk.LoginTypePassword)
	e.fake.addMember(e.orgID, e.bob)
	t.Cleanup(func() {
		if n := e.fake.requestCount(routeUpdateSharingSettings); n != 0 {
			t.Errorf("the controller sent %d sharing settings writes", n)
		}
	})
	return e
}

// setShareGroups sets the shareGroupIDs of the control plane's one owner.
func (e *poolMemberEnv) setShareGroups(t *testing.T, groupIDs ...string) {
	t.Helper()
	cp := &coderv1alpha1.CoderControlPlane{}
	require.NoError(t, k8sClient.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "coder"}, cp))
	cp.Spec.WorkspacePools.Owners[0].ShareGroupIDs = groupIDs
	require.NoError(t, k8sClient.Update(e.ctx, cp))
}

func (e *poolMemberEnv) createShareClaim(t *testing.T, name, user string) types.NamespacedName {
	t.Helper()
	claim := &coderv1alpha1.CoderWorkspaceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec:       coderv1alpha1.CoderWorkspaceClaimSpec{PoolName: e.pool.Name, Share: &coderv1alpha1.CoderWorkspaceClaimShare{User: user}},
	}
	require.NoError(t, k8sClient.Create(e.ctx, claim))
	return client.ObjectKeyFromObject(claim)
}

// bindShareClaim binds a ready member to a new Share claim for user.
func (e *shareEnv) bindShareClaim(t *testing.T, user string) (member, claim types.NamespacedName, workspaceID uuid.UUID) {
	t.Helper()
	member, workspaceID = e.readyMember(t)
	claim = e.createShareClaim(t, "build", user)
	e.reconcilePool(t, 1)
	e.requireBound(t, member, claim)
	return member, claim, workspaceID
}

// acl reads the workspace ACL from Coder. The read counts as a request.
func (e *poolMemberEnv) acl(t *testing.T, workspaceID uuid.UUID) []codersdk.WorkspaceUser {
	t.Helper()
	acl, err := e.fake.client(t, 5*time.Second).WorkspaceACL(e.ctx, workspaceID)
	require.NoError(t, err)
	require.Empty(t, acl.Groups)
	return acl.Users
}

func requireShareClaim(t *testing.T, claim *coderv1alpha1.CoderWorkspaceClaim, phase, reason string, ready, stalled bool) {
	t.Helper()
	require.Equal(t, phase, claim.Status.Phase, claim.Status.Message)
	require.Equal(t, reason, claim.Status.Reason, claim.Status.Message)
	conditions := claim.Status.Conditions
	require.Equal(t, ready, meta.IsStatusConditionTrue(conditions, coderv1alpha1.CoderWorkspaceClaimConditionReady))
	require.Equal(t, stalled, meta.IsStatusConditionTrue(conditions, coderv1alpha1.CoderWorkspaceClaimConditionStalled))
	require.Equal(t, !ready && !stalled, meta.IsStatusConditionTrue(conditions, coderv1alpha1.CoderWorkspaceClaimConditionReconciling))
}

// TestPoolSharesWorkspace: a Share claim binds a ready member, and the
// member's next reconcile gives the named user the use role with exactly one
// ACL write. That reconcile is also the first one after a crash right after
// the bind. The claim is Ready only after the share, and later reconciles
// read the ACL but never write it again.
func TestPoolSharesWorkspace(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"username", "UUID"} {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			e := newShareEnv(t)
			user := "bob"
			if form == "UUID" {
				user = e.bob.String()
			}
			member, claimKey, workspaceID := e.bindShareClaim(t, user)
			requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "SharingWorkspace", false, false)
			require.Zero(t, e.fake.requestCount(routeUpdateWorkspaceACL), "the pool never writes the ACL")

			var body []byte
			e.fake.failNext(routeUpdateWorkspaceACL, fakeFault{Inspect: func(b []byte) { body = b }})
			m := e.reconcileMember(t, member, 1)
			requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "shared with Coder user "+e.bob.String())
			require.Equal(t, e.bob.String(), m.Status.SharedUserID)
			require.Equal(t, 1, e.fake.requestCount(routeUpdateWorkspaceACL))
			require.JSONEq(t, `{"user_roles":{"`+e.bob.String()+`":"use"}}`, string(body), "only use, never admin or a group role")
			require.Equal(t, []codersdk.WorkspaceUser{{MinimalUser: codersdk.MinimalUser{ID: e.bob, Username: "bob"}, Role: codersdk.WorkspaceRoleUse}},
				e.acl(t, workspaceID))

			e.reconcilePool(t, 1)
			claim := e.claim(t, claimKey)
			requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true, false)
			require.Equal(t, e.bob.String(), claim.Status.SharedUserID)

			m = e.reconcileMember(t, member, 3)
			requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "")
			require.True(t, meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionShared))
			require.Equal(t, 1, e.fake.requestCount(routeUpdateWorkspaceACL), "a shared member never writes the ACL again")
		})
	}
}

// TestPoolShareNotAllowed: a Share pool whose organization does not let the
// owner share, or whose owner lists no shareGroupIDs, reports ShareNotAllowed
// at resolve time and binds no claim.
func TestPoolShareNotAllowed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, wantMessage string
		edit              func(t *testing.T, e *shareEnv)
	}{
		{name: "sharing none", wantMessage: "does not allow workspace sharing", edit: func(_ *testing.T, e *shareEnv) {
			e.fake.setSharing(e.orgID, codersdk.ShareableWorkspaceOwnersNone)
		}},
		{name: "service accounts, password owner", wantMessage: "lets only service accounts share", edit: func(_ *testing.T, e *shareEnv) {
			e.fake.setSharing(e.orgID, codersdk.ShareableWorkspaceOwnersServiceAccounts)
		}},
		{name: "no shareGroupIDs", wantMessage: "lists no shareGroupIDs", edit: func(t *testing.T, e *shareEnv) { e.setShareGroups(t) }},
		{name: "deployment disables sharing", wantMessage: "deployment disables workspace sharing", edit: func(_ *testing.T, e *shareEnv) {
			e.fake.failNext(routeSharingSettings, fakeFault{Rewrite: func(answer any) any {
				settings := answer.(codersdk.WorkspaceSharingSettings)
				settings.SharingGloballyDisabled = true
				return settings
			}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newShareEnv(t)
			member, _ := e.readyMember(t)
			tc.edit(t, e)
			claimKey := e.createShareClaim(t, "build", "bob")
			pool := e.reconcilePool(t, 1)
			require.Equal(t, "ShareNotAllowed", pool.Status.Reason)
			require.Contains(t, pool.Status.Message, tc.wantMessage)
			require.False(t, meta.IsStatusConditionTrue(pool.Status.Conditions, coderv1alpha1.CoderWorkspacePoolConditionReady))
			require.Equal(t, time.Minute, e.lastStep.RequeueAfter, "a fix in Coder sends no event")
			require.Nil(t, e.reconcileMember(t, member, 0).Spec.ClaimRef)
			claim := e.claim(t, claimKey)
			requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhasePending, "ShareNotAllowed", false, false)
			require.Contains(t, claim.Status.Message, tc.wantMessage)
		})
	}
}

// TestPoolDropsShareDemandWhileShareNotAllowed: while the owner may not
// share, no Share claim of the pool can bind, so none counts as demand. A
// pool at replicas 0 deletes the member it built for a waiting claim.
func TestPoolDropsShareDemandWhileShareNotAllowed(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	claimKey := e.createShareClaim(t, "build", "bob")
	member, _ := e.readyMember(t)
	e.setShareGroups(t)

	require.Equal(t, "ShareNotAllowed", e.reconcilePool(t, 1).Status.Reason)
	requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhasePending, "ShareNotAllowed", false, false)
	m := e.reconcileMember(t, member, 0)
	require.True(t, m == nil || !m.DeletionTimestamp.IsZero(), "the member built for the claim is deleted")
}

// TestPoolChecksShareUserBeforeBind (T12): a claim author can name any Coder
// user. The pool binds a Share claim only for an active member of the
// template's organization who is in one of the owner's shareGroupIDs groups.
// A refused claim stays Pending with its reason, binds no member, and adds no
// demand, so the pool builds no member for it.
func TestPoolChecksShareUserBeforeBind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// setup returns the claim's user.
		setup                   func(t *testing.T, e *shareEnv) string
		wantReason, wantMessage string // No reason: the claim binds, and the member shares.
		wantStalled             bool
	}{
		{name: "Everyone group of the template organization", setup: func(*testing.T, *shareEnv) string { return "bob" }},
		{name: "second listed group has the user", setup: func(t *testing.T, e *shareEnv) string {
			e.fake.setLicensed(true)
			e.setShareGroups(t, uuid.NewString(), e.fake.addGroup(e.orgID, "sharers", "Sharers", e.bob).String())
			return "bob"
		}},
		{name: "listed group without the user", wantReason: "ShareUserNotEligible", wantMessage: "in none of the groups", setup: func(t *testing.T, e *shareEnv) string {
			e.fake.setLicensed(true)
			e.setShareGroups(t, e.fake.addGroup(e.orgID, "sharers", "Sharers", e.tester).String())
			return "bob"
		}},
		{name: "listed group without a license", wantReason: "ShareUserNotEligible", setup: func(t *testing.T, e *shareEnv) string {
			group := e.fake.addGroup(e.orgID, "sharers", "Sharers", e.bob)
			e.setShareGroups(t, group.String())
			return "bob"
		}, wantMessage: "could not be read: Coder answered 403: Template RBAC is a Premium feature."},
		{name: "another organization's ID", wantReason: "ShareUserNotEligible", wantMessage: "could not be read", setup: func(t *testing.T, e *shareEnv) string {
			other := e.fake.addOrganization("other")
			e.fake.addMember(other, e.bob)
			e.setShareGroups(t, other.String())
			return "bob"
		}},
		{name: "suspended user", wantReason: "ShareUserNotEligible", wantMessage: `status "suspended"`, setup: func(_ *testing.T, e *shareEnv) string {
			e.fake.updateUser(e.bob, func(u *codersdk.User) { u.Status = codersdk.UserStatusSuspended })
			return "bob"
		}},
		{name: "dormant user", wantReason: "ShareUserNotEligible", wantMessage: `status "dormant"`, setup: func(_ *testing.T, e *shareEnv) string {
			e.fake.updateUser(e.bob, func(u *codersdk.User) { u.Status = codersdk.UserStatusDormant })
			return "bob"
		}},
		{name: "user outside the template organization", wantReason: "ShareUserNotEligible", wantMessage: "is not a member of organization", setup: func(_ *testing.T, e *shareEnv) string {
			carol := e.fake.addUser("carol", codersdk.LoginTypePassword)
			e.fake.addMember(e.fake.addOrganization("other"), carol)
			return "carol"
		}},
		{name: "unknown user", wantReason: "ShareUserNotFound", wantMessage: `"nobody" does not exist`, setup: func(*testing.T, *shareEnv) string { return "nobody" }},
		{name: "prebuilds user", wantReason: "ShareUserNotEligible", wantMessage: "prebuilds system user", wantStalled: true, setup: func(_ *testing.T, e *shareEnv) string {
			prebuilds := uuid.MustParse(codersdk.PrebuildsSystemUserID)
			e.fake.addUserWithID(prebuilds, "prebuilds")
			e.fake.addMember(e.orgID, prebuilds)
			return "prebuilds"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newShareEnv(t)
			member, _ := e.readyMember(t)
			claimKey := e.createShareClaim(t, "build", tc.setup(t, e))
			e.reconcilePool(t, 1)
			if tc.wantReason == "" {
				e.requireBound(t, member, claimKey)
				require.Equal(t, e.bob.String(), e.reconcileMember(t, member, 1).Status.SharedUserID)
				return
			}
			claim := e.claim(t, claimKey)
			requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhasePending, tc.wantReason, false, tc.wantStalled)
			require.Contains(t, claim.Status.Message, tc.wantMessage)
			require.Nil(t, e.reconcileMember(t, member, 0).Spec.ClaimRef, "a free ready member stays unbound")
			require.Len(t, e.poolMembers(t, e.pool.Name), 1, "the refused claim is no demand")
			if !tc.wantStalled {
				require.Equal(t, time.Minute, e.lastStep.RequeueAfter, "a fix in Coder sends no event")
			}
			require.Zero(t, e.fake.requestCount(routeUpdateWorkspaceACL))
		})
	}
}

// TestPoolChecksShareGroupsWithoutCache: a bind cannot be undone, so the
// pool checks the claim's user against the control plane read without the
// cache. Groups that an admin just withdrew refuse the claim, although the
// cache still lists them.
func TestPoolChecksShareGroupsWithoutCache(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	member, _ := e.readyMember(t)
	e.setShareGroups(t)
	claimKey := e.createShareClaim(t, "build", "bob")
	stale := staleControlPlaneClient{Client: k8sClient, stale: func(o *coderv1alpha1.WorkspacePoolOwner) { o.ShareGroupIDs = []string{e.orgID.String()} }}
	r := &controller.CoderWorkspacePoolReconciler{Client: stale, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
	require.NoError(t, err)
	claim := e.claim(t, claimKey)
	requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhasePending, "ShareUserNotEligible", false, false)
	require.Contains(t, claim.Status.Message, "in none of the groups")
	require.Nil(t, e.reconcileMember(t, member, 0).Spec.ClaimRef, "no bind on withdrawn groups")
}

// TestPoolBoundsShareChecksOfRefusedClaims: any claim author can add claims
// that the policy refuses, so they must not multiply Coder reads. The pool
// reads each listed group once per reconcile, keeps a refusal for one poll,
// and stops checking at Coder's 429.
func TestPoolBoundsShareChecksOfRefusedClaims(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	e.fake.setLicensed(true)
	var groups []string
	for _, name := range []string{"a", "b", "c"} {
		groups = append(groups, e.fake.addGroup(e.orgID, name, name, e.tester).String())
	}
	e.setShareGroups(t, groups...)
	e.readyMember(t)
	var claims []types.NamespacedName
	for _, name := range []string{"c1", "c2", "c3", "c4"} {
		claims = append(claims, e.createShareClaim(t, name, "bob"))
	}
	r := &controller.CoderWorkspacePoolReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	reconcile := func() {
		t.Helper()
		_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: e.pool.Name}})
		require.NoError(t, err)
	}
	reconcile()
	require.Equal(t, 3, e.fake.requestCount(routeGroup), "each group once for 4 claims")
	for _, key := range claims {
		requireShareClaim(t, e.claim(t, key), coderv1alpha1.CoderWorkspaceClaimPhasePending, "ShareUserNotEligible", false, false)
	}
	reconcile()
	require.Equal(t, 3, e.fake.requestCount(routeGroup), "a refusal holds until the next poll")

	e.clock.SetTime(e.clock.Now().Add(time.Minute))
	userReads := e.fake.requestCount(routeUser)
	e.fake.failNext(routeUser, fakeFault{Inspect: func([]byte) {}}) // The owner read at resolve.
	e.fake.failNext(routeUser, fakeFault{Status: http.StatusTooManyRequests})
	reconcile()
	require.Equal(t, userReads+2, e.fake.requestCount(routeUser), "Coder's 429 for the first claim ends the checks")
	for _, key := range claims {
		requireShareClaim(t, e.claim(t, key), coderv1alpha1.CoderWorkspaceClaimPhasePending, "CoderUnavailable", false, false)
	}
}

// TestPoolKeepsDemandWhileShareCheckFails: a group read that fails for
// another reason than a license or a missing group proves nothing. The claim
// waits with CoderUnavailable and still counts as demand, so a pool at
// replicas 0 keeps the member built for it, and binds it once Coder answers.
func TestPoolKeepsDemandWhileShareCheckFails(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	e.fake.setLicensed(true)
	e.setShareGroups(t, e.fake.addGroup(e.orgID, "sharers", "Sharers", e.bob).String())
	e.editPool(t, func(s *coderv1alpha1.CoderWorkspacePoolSpec) { s.Replicas = new(int32(0)) })
	claimKey := e.createShareClaim(t, "build", "bob")
	member, _ := e.readyMember(t)

	e.fake.failNext(routeGroup, fakeFault{Status: http.StatusInternalServerError})
	e.reconcilePool(t, 1)
	requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhasePending, "CoderUnavailable", false, false)
	require.Nil(t, e.reconcileMember(t, member, 0).Spec.ClaimRef, "no bind without a check")
	require.Len(t, e.poolMembers(t, e.pool.Name), 1, "the claim still counts as demand")

	e.reconcilePool(t, 1)
	e.requireBound(t, member, claimKey)
}

// TestPoolMemberChecksSharePolicyBeforeACLWrite (T13): the member checks the
// organization's sharing setting, the owner's consent and shareGroupIDs, and
// the user again right before the ACL write, because Coder lets the
// operator, a site owner, share where the owner could not. Each case changes
// Coder or the control plane after the bind. A policy refusal sends no ACL
// write. Every refusal keeps the member ready and polling, and stalls the
// claim with ShareRefused.
func TestPoolMemberChecksSharePolicyBeforeACLWrite(t *testing.T) {
	t.Parallel()
	setSharing := func(owners codersdk.ShareableWorkspaceOwners) func(*testing.T, *shareEnv) {
		return func(_ *testing.T, e *shareEnv) { e.fake.setSharing(e.orgID, owners) }
	}
	for _, tc := range []struct {
		name        string
		edit        func(t *testing.T, e *shareEnv)
		wantMessage string // Empty: the member shares.
		wantWrites  int
	}{
		{name: "everyone", edit: setSharing(codersdk.ShareableWorkspaceOwnersEveryone)},
		{name: "service accounts, password owner", edit: setSharing(codersdk.ShareableWorkspaceOwnersServiceAccounts), wantMessage: "lets only service accounts share"},
		{name: "service accounts, service account owner", edit: func(t *testing.T, e *shareEnv) {
			setSharing(codersdk.ShareableWorkspaceOwnersServiceAccounts)(t, e)
			e.fake.updateUser(e.tester, func(u *codersdk.User) { u.IsServiceAccount = true })
		}},
		{name: "none", edit: setSharing(codersdk.ShareableWorkspaceOwnersNone), wantMessage: `(shareable_workspace_owners "none")`},
		{name: "unknown setting", edit: setSharing("members"), wantMessage: `(shareable_workspace_owners "members")`},
		{name: "no shareGroupIDs", edit: func(t *testing.T, e *shareEnv) { e.setShareGroups(t) }, wantMessage: "lists no shareGroupIDs"},
		{name: "owner no longer listed", edit: func(t *testing.T, e *shareEnv) { e.allowOwner(t, "") }, wantMessage: "does not list Coder user"},
		{name: "user suspended", edit: func(_ *testing.T, e *shareEnv) {
			e.fake.updateUser(e.bob, func(u *codersdk.User) { u.Status = codersdk.UserStatusSuspended })
		}, wantMessage: `status "suspended"`},
		{name: "Coder refuses the write", edit: func(_ *testing.T, e *shareEnv) {
			e.fake.failNext(routeUpdateWorkspaceACL, fakeFault{Status: http.StatusForbidden, Message: "Workspace sharing is disabled for this organization."})
		}, wantMessage: "Coder answered 403", wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newShareEnv(t)
			member, claimKey, workspaceID := e.bindShareClaim(t, "bob")
			tc.edit(t, e)
			m := e.reconcileMember(t, member, 1)
			require.Equal(t, time.Minute, e.lastStep.RequeueAfter)
			if tc.wantMessage == "" {
				requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "")
				require.Len(t, e.acl(t, workspaceID), 1)
				return
			}
			requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRefused", tc.wantMessage)
			require.False(t, meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionShared))
			if tc.wantWrites == 0 {
				require.Empty(t, m.Status.SharedUserID, "a refusal before the write stores no user")
			}
			require.Equal(t, tc.wantWrites, e.fake.requestCount(routeUpdateWorkspaceACL))
			require.Empty(t, e.acl(t, workspaceID))
			e.reconcilePool(t, 1)
			claim := e.claim(t, claimKey)
			requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "ShareRefused", false, true)
			require.Contains(t, claim.Status.Message, tc.wantMessage)
		})
	}
}

// TestPoolMemberShareAfterCoderErrors: a failed Coder read leaves the member
// ready and the claim waiting. After an ACL write whose answer is lost, the
// ACL read at the next reconcile shows that the write took effect, so the
// member never sends it again.
func TestPoolMemberShareAfterCoderErrors(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	member, claimKey, _ := e.bindShareClaim(t, "bob")
	for _, route := range []string{routeWorkspaceACL, routeSharingSettings} {
		e.fake.failNext(route, fakeFault{Status: http.StatusServiceUnavailable})
		m := e.reconcileMember(t, member, 1)
		requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "CoderUnavailable", "")
		require.Empty(t, m.Status.SharedUserID)
	}
	e.reconcilePool(t, 1)
	requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "SharingWorkspace", false, false)
	require.Zero(t, e.fake.requestCount(routeUpdateWorkspaceACL))

	e.fake.failNext(routeUpdateWorkspaceACL, fakeFault{Status: http.StatusBadGateway, AfterCommit: true})
	requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "CoderUnavailable", "update workspace ACL")
	m := e.reconcileMember(t, member, 1)
	requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "")
	require.Equal(t, e.bob.String(), m.Status.SharedUserID)
	require.Equal(t, 1, e.fake.requestCount(routeUpdateWorkspaceACL), "the ACL read proves the earlier write")
}

// TestPoolMemberShareSurvivesLostStatus: a member that sent the ACL write
// and then lost its status write finds the share in the ACL at its next
// reconcile, and records it without a second write.
func TestPoolMemberShareSurvivesLostStatus(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	member, claimKey, workspaceID := e.bindShareClaim(t, "bob")

	e.loseSharedStatus(t, member)
	require.Equal(t, 1, e.fake.requestCount(routeUpdateWorkspaceACL))

	m := e.reconcileMember(t, member, 1)
	requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "")
	require.Equal(t, e.bob.String(), m.Status.SharedUserID)
	require.Equal(t, 1, e.fake.requestCount(routeUpdateWorkspaceACL), "the ACL read proves the earlier write")
	require.Len(t, e.acl(t, workspaceID), 1)
	e.reconcilePool(t, 1)
	requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true, false)
}

// TestPoolMemberRevokesWithdrawnShare: the member checks the share policy at
// every poll. When the admin withdraws what allowed a share, the member
// removes it from the ACL with one write, reports ShareRevoked, and never
// shares again, even once the policy allows it again. This holds after a
// crash that lost the status write of the share, too. A Coder read that
// fails proves nothing and keeps the share.
func TestPoolMemberRevokesWithdrawnShare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		lostStatus      bool
		withdraw, allow func(t *testing.T, e *shareEnv, group uuid.UUID)
		wantMessage     string
	}{
		{
			name:        "user left every listed group",
			withdraw:    func(_ *testing.T, e *shareEnv, group uuid.UUID) { e.fake.setGroupMembers(group) },
			allow:       func(_ *testing.T, e *shareEnv, group uuid.UUID) { e.fake.setGroupMembers(group, e.bob) },
			wantMessage: "in none of the groups",
		},
		{
			name: "user left every listed group after a lost status write", lostStatus: true,
			withdraw:    func(_ *testing.T, e *shareEnv, group uuid.UUID) { e.fake.setGroupMembers(group) },
			allow:       func(_ *testing.T, e *shareEnv, group uuid.UUID) { e.fake.setGroupMembers(group, e.bob) },
			wantMessage: "in none of the groups",
		},
		{
			name:        "owner lists no shareGroupIDs",
			withdraw:    func(t *testing.T, e *shareEnv, _ uuid.UUID) { e.setShareGroups(t) },
			allow:       func(t *testing.T, e *shareEnv, group uuid.UUID) { e.setShareGroups(t, group.String()) },
			wantMessage: "lists no shareGroupIDs",
		},
		{
			name:     "owner no longer allowed",
			withdraw: func(t *testing.T, e *shareEnv, _ uuid.UUID) { e.allowOwner(t, "") },
			allow: func(t *testing.T, e *shareEnv, group uuid.UUID) {
				e.allowOwner(t, e.tester.String(), "default.docker")
				e.setShareGroups(t, group.String())
			},
			wantMessage: "does not list Coder user",
		},
		{
			name: "user suspended",
			withdraw: func(_ *testing.T, e *shareEnv, _ uuid.UUID) {
				e.fake.updateUser(e.bob, func(u *codersdk.User) { u.Status = codersdk.UserStatusSuspended })
			},
			allow: func(_ *testing.T, e *shareEnv, _ uuid.UUID) {
				e.fake.updateUser(e.bob, func(u *codersdk.User) { u.Status = codersdk.UserStatusActive })
			},
			wantMessage: `status "suspended"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newShareEnv(t)
			e.fake.setLicensed(true)
			group := e.fake.addGroup(e.orgID, "sharers", "Sharers", e.bob)
			e.setShareGroups(t, group.String())
			member, claimKey, workspaceID := e.bindShareClaim(t, "bob")
			if tc.lostStatus {
				e.loseSharedStatus(t, member)
			} else {
				requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "")
			}
			tc.withdraw(t, e, group)

			var body []byte
			e.fake.failNext(routeUpdateWorkspaceACL, fakeFault{Inspect: func(b []byte) { body = b }})
			m := e.reconcileMember(t, member, 1)
			requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRevoked", tc.wantMessage)
			require.JSONEq(t, `{"user_roles":{"`+e.bob.String()+`":""}}`, string(body), "removes only the operator's share")
			require.Empty(t, e.acl(t, workspaceID))
			e.reconcilePool(t, 1)
			claim := e.claim(t, claimKey)
			requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "ShareRevoked", false, true)
			require.Empty(t, claim.Status.SharedUserID)

			tc.allow(t, e, group)
			requireMember(t, e.reconcileMember(t, member, 2), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRevoked", "")
			require.Equal(t, 2, e.fake.requestCount(routeUpdateWorkspaceACL), "the share and its removal, nothing more")
			require.Empty(t, e.acl(t, workspaceID))
		})
	}
	t.Run("failed reads and a failed removal", func(t *testing.T) {
		t.Parallel()
		e := newShareEnv(t)
		member, claimKey, workspaceID := e.bindShareClaim(t, "bob")
		requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "")
		e.fake.failNext(routeSharingSettings, fakeFault{Status: http.StatusServiceUnavailable})
		requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "CoderUnavailable", "sharing settings")
		require.Len(t, e.acl(t, workspaceID), 1, "a failed policy read keeps the share")
		e.reconcilePool(t, 1)
		requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true, false)

		e.fake.updateUser(e.bob, func(u *codersdk.User) { u.Status = codersdk.UserStatusSuspended })
		e.fake.failNext(routeUpdateWorkspaceACL, fakeFault{Status: http.StatusBadGateway})
		requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRevoking", "could not remove it yet")
		e.reconcilePool(t, 1)
		requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "ShareRevoking", false, true)
		requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRevoked", "")
		require.Empty(t, e.acl(t, workspaceID))
	})
}

// TestPoolMemberEnforcesPolicyAfterRoleChange: a person can change the
// shared user's role from use to admin in Coder. The user still has the
// workspace, so the member keeps reporting the share and never writes the
// role back, but it still removes the share once the policy no longer allows
// it.
func TestPoolMemberEnforcesPolicyAfterRoleChange(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	member, claimKey, workspaceID := e.bindShareClaim(t, "bob")
	requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "(role use)")
	require.NoError(t, e.fake.client(t, 5*time.Second).UpdateWorkspaceACL(e.ctx, workspaceID, codersdk.UpdateWorkspaceACL{
		UserRoles: map[string]codersdk.WorkspaceRole{e.bob.String(): codersdk.WorkspaceRoleAdmin},
	}))

	requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "(role admin)")
	e.reconcilePool(t, 1)
	requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true, false)
	require.Equal(t, 2, e.fake.requestCount(routeUpdateWorkspaceACL), "the share and the person's change, nothing more")

	e.fake.updateUser(e.bob, func(u *codersdk.User) { u.Status = codersdk.UserStatusSuspended })
	requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRevoked", `status "suspended"`)
	require.Empty(t, e.acl(t, workspaceID))
	e.reconcilePool(t, 1)
	requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "ShareRevoked", false, true)
}

// loseSharedStatus runs one member reconcile that sends the ACL write and
// then loses the status write that reports the share, as a crash would. The
// user was stored before the write.
func (e *shareEnv) loseSharedStatus(t *testing.T, member types.NamespacedName) {
	t.Helper()
	lose := failStatusWriteClient{Client: k8sClient, match: func(m *coderv1alpha1.CoderWorkspacePoolMember) bool {
		return meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionShared)
	}}
	r := &controller.CoderWorkspacePoolMemberReconciler{Client: lose, APIReader: k8sClient, Scheme: scheme, Clock: e.clock}
	_, err := r.Reconcile(e.ctx, ctrl.Request{NamespacedName: member})
	require.True(t, apierrors.IsConflict(err), "the lost write ends the reconcile: %v", err)
	stored := e.reconcileMember(t, member, 0)
	require.Equal(t, e.bob.String(), stored.Status.SharedUserID, "the user is stored before the write")
	require.Nil(t, meta.FindStatusCondition(stored.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionShared))
}

// TestPoolMemberShareKeepsItsUserAfterRename: the member checks and shares
// with the user ID it stored before its first write, not with whoever has the
// claim's username later. After a lost status write or a write without
// effect, bob is renamed and a new eligible user takes the name bob. The
// member never shares with the new user, never reports a refusal while the
// first user has access, and refuses when the first user is no longer
// eligible.
func TestPoolMemberShareKeepsItsUserAfterRename(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                       string
		writeTookEffect, suspended bool
	}{
		{name: "write took effect", writeTookEffect: true},
		{name: "write without effect"},
		{name: "write without effect, first user suspended", suspended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newShareEnv(t)
			member, claimKey, workspaceID := e.bindShareClaim(t, "bob")
			if tc.writeTookEffect {
				e.loseSharedStatus(t, member)
			} else {
				e.fake.failNext(routeUpdateWorkspaceACL, fakeFault{Status: http.StatusBadGateway})
				m := e.reconcileMember(t, member, 1)
				requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "CoderUnavailable", "update workspace ACL")
				require.Equal(t, e.bob.String(), m.Status.SharedUserID)
			}
			e.fake.updateUser(e.bob, func(u *codersdk.User) {
				u.Username = "robert"
				if tc.suspended {
					u.Status = codersdk.UserStatusSuspended
				}
			})
			newBob := e.fake.addUser("bob", codersdk.LoginTypePassword)
			e.fake.addMember(e.orgID, newBob)

			m := e.reconcileMember(t, member, 1)
			e.reconcilePool(t, 1)
			claim := e.claim(t, claimKey)
			if tc.suspended {
				requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRefused", `status "suspended"`)
				require.Empty(t, e.acl(t, workspaceID), "neither the suspended user nor the new bob")
				requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "ShareRefused", false, true)
				return
			}
			requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", e.bob.String())
			require.Equal(t, []codersdk.WorkspaceUser{{MinimalUser: codersdk.MinimalUser{ID: e.bob, Username: "robert"}, Role: codersdk.WorkspaceRoleUse}},
				e.acl(t, workspaceID), "only the first user")
			requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "WorkspaceReady", true, false)
			require.Equal(t, e.bob.String(), claim.Status.SharedUserID)
		})
	}
}

// TestPoolMemberReportsRemovedShare: Coder drops the share when an admin
// tightens the organization's sharing setting. The member reads the ACL at
// each poll, reports ShareRemoved, never shares again, and the claim is no
// longer Ready. A status that claims a share Coder does not show is reported
// the same way.
func TestPoolMemberReportsRemovedShare(t *testing.T) {
	t.Parallel()
	t.Run("Coder drops the share", func(t *testing.T) {
		t.Parallel()
		e := newShareEnv(t)
		member, claimKey, workspaceID := e.bindShareClaim(t, "bob")
		requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "Shared", "")
		e.fake.setSharing(e.orgID, codersdk.ShareableWorkspaceOwnersNone)
		require.Empty(t, e.acl(t, workspaceID))
		e.fake.setSharing(e.orgID, codersdk.ShareableWorkspaceOwnersEveryone)

		m := e.reconcileMember(t, member, 2)
		requireMember(t, m, coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRemoved", "does not share it again")
		require.Equal(t, 1, e.fake.requestCount(routeUpdateWorkspaceACL), "no second share, although the policy allows it again")
		require.Empty(t, e.acl(t, workspaceID))
		e.reconcilePool(t, 1)
		claim := e.claim(t, claimKey)
		requireShareClaim(t, claim, coderv1alpha1.CoderWorkspaceClaimPhaseBound, "ShareRemoved", false, true)
		require.Empty(t, claim.Status.SharedUserID)
	})
	t.Run("forged status", func(t *testing.T) {
		t.Parallel()
		e := newShareEnv(t)
		member, claimKey, workspaceID := e.bindShareClaim(t, "bob")
		e.setMemberStatus(t, member, func(s *coderv1alpha1.CoderWorkspacePoolMemberStatus) {
			s.SharedUserID = uuid.NewString()
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: coderv1alpha1.CoderWorkspacePoolMemberConditionShared, Status: metav1.ConditionTrue, Reason: "Shared"})
		})
		requireMember(t, e.reconcileMember(t, member, 1), coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, "ShareRemoved", "")
		require.Zero(t, e.fake.requestCount(routeUpdateWorkspaceACL))
		require.Empty(t, e.acl(t, workspaceID))
		e.reconcilePool(t, 1)
		requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhaseBound, "ShareRemoved", false, true)
	})
}

// TestPoolRefusesClaimOfOtherMode: a claim whose spec.share does not match
// the pool's claims.mode never binds, is no demand, and is Stalled.
func TestPoolRefusesClaimOfOtherMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (*poolMemberEnv, func(t *testing.T) types.NamespacedName)
	}{
		{name: "Reserve claim on a Share pool", setup: func(t *testing.T) (*poolMemberEnv, func(t *testing.T) types.NamespacedName) {
			e := newShareEnv(t).poolMemberEnv
			return e, func(t *testing.T) types.NamespacedName { return e.createClaim(t, "build") }
		}},
		{name: "Share claim on a Reserve pool", setup: func(t *testing.T) (*poolMemberEnv, func(t *testing.T) types.NamespacedName) {
			e := newPoolMemberEnv(t)
			return e, func(t *testing.T) types.NamespacedName { return e.createShareClaim(t, "build", "tester") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, createClaim := tc.setup(t)
			member, _ := e.readyMember(t)
			claimKey := createClaim(t)
			e.reconcilePool(t, 1)
			requireShareClaim(t, e.claim(t, claimKey), coderv1alpha1.CoderWorkspaceClaimPhasePending, "ClaimModeMismatch", false, true)
			require.Nil(t, e.reconcileMember(t, member, 0).Spec.ClaimRef)
			require.Len(t, e.poolMembers(t, e.pool.Name), 1, "the claim is no demand")
		})
	}
}

// TestPoolMemberSharesThroughManager runs the member reconciler in a manager
// whose cache refuses reads of a kind without an informer. A bound member
// reads its claim through the cache, so it shares only because
// SetupWithManager starts the claim informer.
func TestPoolMemberSharesThroughManager(t *testing.T) {
	t.Parallel()
	e := newShareEnv(t)
	member, _, _ := e.bindShareClaim(t, "bob")
	e.startPoolManager(t, false, true)
	require.Eventually(t, func() bool { return e.reconcileMember(t, member, 0).Status.SharedUserID == e.bob.String() },
		10*time.Second, 50*time.Millisecond)
	require.Equal(t, 1, e.fake.requestCount(routeUpdateWorkspaceACL))
}
