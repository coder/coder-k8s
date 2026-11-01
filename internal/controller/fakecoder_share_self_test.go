package controller_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestFakeCoderShareGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	bob := fx.fake.addUser("bob", codersdk.LoginTypePassword)
	fx.fake.addMember(fx.orgID, bob)
	groupID := fx.fake.addGroup(fx.orgID, "sharers", "Sharers", bob)

	_, err := fx.client.Group(ctx, groupID, codersdk.GroupRequest{})
	requireCoderStatus(t, err, http.StatusForbidden)
	require.ErrorContains(t, err, "Template RBAC is a Premium feature. Contact sales!")

	fx.fake.setLicensed(true)
	group, err := fx.client.Group(ctx, groupID, codersdk.GroupRequest{})
	require.NoError(t, err)
	require.Equal(t, groupID, group.ID)
	require.Equal(t, fx.orgID, group.OrganizationID)
	require.Len(t, group.Members, 1)
	require.Equal(t, bob, group.Members[0].ID)
	require.Equal(t, "bob", group.Members[0].Username)

	everyone, err := fx.client.Group(ctx, fx.orgID, codersdk.GroupRequest{})
	require.NoError(t, err)
	var everyoneIDs []uuid.UUID
	for _, m := range everyone.Members {
		everyoneIDs = append(everyoneIDs, m.ID)
	}
	require.ElementsMatch(t, []uuid.UUID{fx.testerID, bob}, everyoneIDs, "the Everyone group has the organization's ID and members")

	fx.fake.setGroupMembers(groupID)
	group, err = fx.client.Group(ctx, groupID, codersdk.GroupRequest{})
	require.NoError(t, err)
	require.Empty(t, group.Members)
	_, err = fx.client.Group(ctx, uuid.New(), codersdk.GroupRequest{})
	requireCoderStatus(t, err, http.StatusNotFound)
}

// TestFakeCoderShareSettingsWrite: a sharing settings write reaches a counted
// route, so a zero count proves the controller sent none. The fault keeps the
// handler, which fails the test, from running.
func TestFakeCoderShareSettingsWrite(t *testing.T) {
	t.Parallel()
	fx := newFakeCoderFixture(t)
	fx.fake.failNext(routeUpdateSharingSettings, fakeFault{Status: http.StatusInternalServerError})
	_, err := fx.client.PatchWorkspaceSharingSettings(context.Background(), fx.orgID.String(),
		codersdk.UpdateWorkspaceSharingSettingsRequest{ShareableWorkspaceOwners: codersdk.ShareableWorkspaceOwnersEveryone})
	requireCoderStatus(t, err, http.StatusInternalServerError)
	require.Equal(t, 1, fx.fake.requestCount(routeUpdateSharingSettings))
}

func TestFakeCoderShareFixedUserID(t *testing.T) {
	t.Parallel()
	fx := newFakeCoderFixture(t)
	prebuilds := uuid.MustParse(codersdk.PrebuildsSystemUserID)
	fx.fake.addUserWithID(prebuilds, "prebuilds")
	for _, ident := range []string{prebuilds.String(), "prebuilds"} {
		u, err := fx.client.User(context.Background(), ident)
		require.NoError(t, err)
		require.Equal(t, prebuilds, u.ID)
		require.Equal(t, "prebuilds", u.Username)
	}
}
