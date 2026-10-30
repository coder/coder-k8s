package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// startedWorkspace creates a workspace and succeeds its start build.
func (fx fakeCoderFixture) startedWorkspace(t *testing.T, name string) codersdk.Workspace {
	t.Helper()
	ctx := context.Background()
	ws, err := fx.create(ctx, name)
	require.NoError(t, err)
	fx.fake.setBuildJob(ws.LatestBuild.ID, codersdk.ProvisionerJobSucceeded)
	ws, err = fx.client.Workspace(ctx, ws.ID)
	require.NoError(t, err)
	return ws
}

func TestFakeCoderPoolDeadlines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	c := fx.client

	// Without a template TTL, a start build has no deadline, and an
	// extension answers 409 like Coder.
	manual := fx.startedWorkspace(t, "kwp-manual")
	require.Nil(t, manual.TTLMillis)
	require.False(t, manual.LatestBuild.Deadline.Valid)
	err := c.PutExtendWorkspace(ctx, manual.ID, codersdk.PutExtendWorkspaceRequest{Deadline: time.Now().Add(time.Hour)})
	requireCoderStatus(t, err, http.StatusConflict)

	// With a default TTL, the deadline is the build completion plus the TTL.
	fx.fake.setTemplateSchedule(fx.templateID, time.Hour, 0)
	building, err := fx.create(ctx, "kwp-ttl")
	require.NoError(t, err)
	require.Equal(t, time.Hour.Milliseconds(), *building.TTLMillis)
	err = c.PutExtendWorkspace(ctx, building.ID, codersdk.PutExtendWorkspaceRequest{Deadline: time.Now().Add(2 * time.Hour)})
	requireCoderStatus(t, err, http.StatusConflict) // Still building.
	fx.fake.setBuildJob(building.LatestBuild.ID, codersdk.ProvisionerJobSucceeded)
	ws, err := c.Workspace(ctx, building.ID)
	require.NoError(t, err)
	require.True(t, ws.LatestBuild.Deadline.Valid)
	require.True(t, ws.LatestBuild.Job.CompletedAt.Add(time.Hour).Equal(ws.LatestBuild.Deadline.Time))
	require.False(t, ws.LatestBuild.MaxDeadline.Valid)

	// Extensions follow Coder's rules: at least 29 minutes ahead, then set.
	err = c.PutExtendWorkspace(ctx, ws.ID, codersdk.PutExtendWorkspaceRequest{Deadline: time.Now().Add(10 * time.Minute)})
	requireCoderStatus(t, err, http.StatusBadRequest)
	want := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, c.PutExtendWorkspace(ctx, ws.ID, codersdk.PutExtendWorkspaceRequest{Deadline: want}))
	ws, err = c.Workspace(ctx, ws.ID)
	require.NoError(t, err)
	require.True(t, want.Equal(ws.LatestBuild.Deadline.Time))

	// An explicit ttl_ms wins over the template default, truncated to whole
	// minutes. Coder refuses less than a minute and more than 30 days, even
	// when the name is taken.
	createWithTTL := func(name string, ttl time.Duration) (codersdk.Workspace, error) {
		millis := ttl.Milliseconds()
		return c.CreateUserWorkspace(ctx, fx.testerID.String(), codersdk.CreateWorkspaceRequest{
			TemplateVersionID: fx.versionID, Name: name, TTLMillis: &millis,
		})
	}
	custom, err := createWithTTL("kwp-custom", 2*time.Hour+30*time.Second)
	require.NoError(t, err)
	require.Equal(t, (2 * time.Hour).Milliseconds(), *custom.TTLMillis)
	for _, ttl := range []time.Duration{59 * time.Second, -time.Hour, 30*24*time.Hour + time.Minute} {
		_, err = createWithTTL("kwp-custom", ttl)
		requireCoderStatus(t, err, http.StatusBadRequest)
	}

	// A stop build is latest: extensions answer 409.
	stop := fx.fake.addExternalBuild(ws.ID, fx.testerID, codersdk.WorkspaceTransitionStop)
	require.Equal(t, fx.testerID, stop.InitiatorID)
	err = c.PutExtendWorkspace(ctx, ws.ID, codersdk.PutExtendWorkspaceRequest{Deadline: time.Now().Add(time.Hour)})
	requireCoderStatus(t, err, http.StatusConflict)
	builds, err := c.WorkspaceBuilds(ctx, codersdk.WorkspaceBuildsRequest{WorkspaceID: ws.ID})
	require.NoError(t, err)
	require.Equal(t, stop.ID, builds[0].ID)
	require.Equal(t, fx.testerID, builds[0].InitiatorID)
}

func TestFakeCoderPoolMaxDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)

	// The autostop requirement caps a longer TTL.
	fx.fake.setTemplateSchedule(fx.templateID, 8*time.Hour, 2*time.Hour)
	ws := fx.startedWorkspace(t, "kwp-capped")
	completed := *ws.LatestBuild.Job.CompletedAt
	require.True(t, completed.Add(2*time.Hour).Equal(ws.LatestBuild.MaxDeadline.Time))
	require.True(t, completed.Add(2*time.Hour).Equal(ws.LatestBuild.Deadline.Time))
	err := fx.client.PutExtendWorkspace(ctx, ws.ID, codersdk.PutExtendWorkspaceRequest{Deadline: completed.Add(3 * time.Hour)})
	requireCoderStatus(t, err, http.StatusBadRequest)

	// Without a TTL, the deadline is the max deadline.
	fx.fake.setTemplateSchedule(fx.templateID, 0, 90*time.Minute)
	ws = fx.startedWorkspace(t, "kwp-required")
	require.True(t, ws.LatestBuild.Job.CompletedAt.Add(90*time.Minute).Equal(ws.LatestBuild.Deadline.Time))
}

func TestFakeCoderPoolACL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	c := fx.client
	bob := fx.fake.addUser("bob", codersdk.LoginTypePassword)
	ws := fx.startedWorkspace(t, "kwp-shared")

	settings, err := c.WorkspaceSharingSettings(ctx, fx.orgID.String())
	require.NoError(t, err)
	require.Equal(t, codersdk.ShareableWorkspaceOwnersEveryone, settings.ShareableWorkspaceOwners)
	acl, err := c.WorkspaceACL(ctx, ws.ID)
	require.NoError(t, err)
	require.Empty(t, acl.Users)

	require.NoError(t, c.UpdateWorkspaceACL(ctx, ws.ID, codersdk.UpdateWorkspaceACL{
		UserRoles: map[string]codersdk.WorkspaceRole{bob.String(): codersdk.WorkspaceRoleUse},
	}))
	acl, err = c.WorkspaceACL(ctx, ws.ID)
	require.NoError(t, err)
	require.Len(t, acl.Users, 1)
	require.Equal(t, bob, acl.Users[0].ID)
	require.Equal(t, codersdk.WorkspaceRoleUse, acl.Users[0].Role)

	for name, roles := range map[string]map[string]codersdk.WorkspaceRole{
		"unknown role": {bob.String(): "owner"},
		"unknown user": {uuid.NewString(): codersdk.WorkspaceRoleUse},
		"not a UUID":   {"bob": codersdk.WorkspaceRoleUse},
	} {
		patchErr := c.UpdateWorkspaceACL(ctx, ws.ID, codersdk.UpdateWorkspaceACL{UserRoles: roles})
		requireCoderStatus(t, patchErr, http.StatusBadRequest)
		var sdkErr *codersdk.Error
		require.ErrorAs(t, patchErr, &sdkErr)
		require.Len(t, sdkErr.Validations, 1, name)
	}

	// The empty role removes the entry.
	require.NoError(t, c.UpdateWorkspaceACL(ctx, ws.ID, codersdk.UpdateWorkspaceACL{
		UserRoles: map[string]codersdk.WorkspaceRole{bob.String(): codersdk.WorkspaceRoleDeleted},
	}))
	acl, err = c.WorkspaceACL(ctx, ws.ID)
	require.NoError(t, err)
	require.Empty(t, acl.Users)

	// The site-owner operator can share under service_accounts. Only none
	// refuses it.
	fx.fake.setSharing(fx.orgID, codersdk.ShareableWorkspaceOwnersServiceAccounts)
	use := codersdk.UpdateWorkspaceACL{UserRoles: map[string]codersdk.WorkspaceRole{bob.String(): codersdk.WorkspaceRoleUse}}
	require.NoError(t, c.UpdateWorkspaceACL(ctx, ws.ID, use))
	fx.fake.setSharing(fx.orgID, codersdk.ShareableWorkspaceOwnersNone)
	requireCoderStatus(t, c.UpdateWorkspaceACL(ctx, ws.ID, use), http.StatusForbidden)
	settings, err = c.WorkspaceSharingSettings(ctx, fx.orgID.String())
	require.NoError(t, err)
	require.Equal(t, codersdk.ShareableWorkspaceOwnersNone, settings.ShareableWorkspaceOwners)
	require.True(t, settings.SharingDisabled)

	// A workspace deleted out of band refuses writes loudly.
	fx.fake.setSharing(fx.orgID, codersdk.ShareableWorkspaceOwnersEveryone)
	fx.fake.markDeleted(ws.ID)
	requireCoderStatus(t, c.UpdateWorkspaceACL(ctx, ws.ID, use), http.StatusBadRequest)
	_, err = c.WorkspaceACL(ctx, uuid.New())
	requireCoderStatus(t, err, http.StatusNotFound)
}

func TestFakeCoderPoolPresets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	c := fx.client

	presets, err := c.TemplateVersionPresets(ctx, fx.versionID)
	require.NoError(t, err)
	require.Empty(t, presets)
	small := fx.fake.addPreset(fx.versionID, "small", codersdk.PresetParameter{Name: "cpu", Value: "2"})
	presets, err = c.TemplateVersionPresets(ctx, fx.versionID)
	require.NoError(t, err)
	require.Len(t, presets, 1)
	require.Equal(t, small, presets[0].ID)
	require.Equal(t, "small", presets[0].Name)
	require.Equal(t, []codersdk.PresetParameter{{Name: "cpu", Value: "2"}}, presets[0].Parameters)
	_, err = c.TemplateVersionPresets(ctx, uuid.New())
	requireCoderStatus(t, err, http.StatusNotFound)

	ws, err := c.CreateUserWorkspace(ctx, fx.testerID.String(), codersdk.CreateWorkspaceRequest{
		TemplateVersionID: fx.versionID, Name: "kwp-preset", TemplateVersionPresetID: small,
	})
	require.NoError(t, err)
	require.Equal(t, small, *ws.LatestBuild.TemplateVersionPresetID)

	// A preset of another version is refused loudly.
	v2 := fx.fake.addVersion(fx.templateID, "v2", codersdk.ProvisionerJobSucceeded)
	other := fx.fake.addPreset(v2, "small")
	_, err = c.CreateUserWorkspace(ctx, fx.testerID.String(), codersdk.CreateWorkspaceRequest{
		TemplateVersionID: fx.versionID, Name: "kwp-other", TemplateVersionPresetID: other,
	})
	requireCoderStatus(t, err, http.StatusBadRequest)
}

func TestFakeCoderPoolLastUsed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	c := fx.client
	read := func(id uuid.UUID) codersdk.Workspace {
		t.Helper()
		ws, err := c.Workspace(ctx, id)
		require.NoError(t, err)
		return ws
	}

	a, b := fx.startedWorkspace(t, "kwp-a"), fx.startedWorkspace(t, "kwp-b")
	require.False(t, a.LastUsedAt.IsZero(), "Coder sets last_used_at at create")

	used := a.LastUsedAt.Add(-2 * time.Hour) // Past times, so "now" below is later.
	fx.fake.markUsed(a.ID, used)
	require.True(t, used.Equal(read(a.ID).LastUsedAt))
	require.True(t, b.LastUsedAt.Equal(read(b.ID).LastUsedAt), "markUsed changes one workspace only")

	touched := used.Add(time.Hour)
	fx.fake.touchTemplateWorkspaces(fx.templateID, touched)
	require.True(t, touched.Equal(read(a.ID).LastUsedAt))
	require.True(t, touched.Equal(read(b.ID).LastUsedAt))
	tpl, err := c.Template(ctx, fx.templateID)
	require.NoError(t, err)
	require.True(t, touched.Equal(tpl.UpdatedAt))
	edited := touched.Add(time.Minute)
	fx.fake.editTemplate(fx.templateID, edited)
	tpl, err = c.Template(ctx, fx.templateID)
	require.NoError(t, err)
	require.True(t, edited.Equal(tpl.UpdatedAt))
	require.True(t, touched.Equal(read(b.ID).LastUsedAt), "a template edit writes no last_used_at")

	dormant, deleting := time.Now(), time.Now().Add(time.Hour)
	fx.fake.setDormant(a.ID, &dormant, &deleting)
	ws := read(a.ID)
	require.NotNil(t, ws.DormantAt)
	require.NotNil(t, ws.DeletingAt)
	require.True(t, touched.Equal(ws.LastUsedAt), "entering dormancy keeps last_used_at")
	fx.fake.setDormant(a.ID, nil, nil)
	ws = read(a.ID)
	require.Nil(t, ws.DormantAt)
	require.True(t, ws.LastUsedAt.After(touched), "leaving dormancy sets last_used_at to now")
}
