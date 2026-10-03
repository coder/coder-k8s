package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeCoderFixture struct {
	fake       *fakeCoder
	client     *codersdk.Client
	testerID   uuid.UUID
	orgID      uuid.UUID
	templateID uuid.UUID
	versionID  uuid.UUID
}

func newFakeCoderFixture(t *testing.T) fakeCoderFixture {
	t.Helper()
	f := newFakeCoder(t)
	fx := fakeCoderFixture{fake: f, client: f.client(t, 2*time.Second)}
	fx.orgID = f.addOrganization("default")
	fx.testerID = f.addUser("tester", codersdk.LoginTypePassword)
	f.addMember(fx.orgID, fx.testerID, codersdk.RoleOrganizationMember)
	fx.templateID = f.addTemplate(fx.orgID, "docker")
	fx.versionID = f.addVersion(fx.templateID, "v1", codersdk.ProvisionerJobSucceeded)
	return fx
}

func (fx fakeCoderFixture) create(ctx context.Context, name string) (codersdk.Workspace, error) {
	return fx.client.CreateUserWorkspace(ctx, fx.testerID.String(), codersdk.CreateWorkspaceRequest{
		TemplateVersionID: fx.versionID,
		Name:              name,
		AutomaticUpdates:  codersdk.AutomaticUpdatesNever,
	})
}

func requireCoderStatus(t *testing.T, err error, status int) {
	t.Helper()
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, status, sdkErr.StatusCode())
}

func TestFakeCoderLookups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	c := fx.client

	me, err := c.User(ctx, codersdk.Me)
	require.NoError(t, err)
	require.Equal(t, fx.fake.operatorID, me.ID)
	tester, err := c.User(ctx, fx.testerID.String())
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{fx.orgID}, tester.OrganizationIDs)

	org, err := c.OrganizationByName(ctx, "default")
	require.NoError(t, err)
	member, err := c.OrganizationMember(ctx, org.ID.String(), fx.testerID.String())
	require.NoError(t, err)
	require.Equal(t, codersdk.RoleOrganizationMember, member.Roles[0].Name)

	tpl, err := c.TemplateByName(ctx, org.ID, "Docker") // Coder matches template names case-insensitively.
	require.NoError(t, err)
	require.Equal(t, fx.versionID, tpl.ActiveVersionID)
	_, err = c.Template(ctx, tpl.ID)
	require.NoError(t, err)
	byName, err := c.TemplateVersionByName(ctx, tpl.ID, "v1")
	require.NoError(t, err)
	byID, err := c.TemplateVersion(ctx, fx.versionID)
	require.NoError(t, err)
	require.Equal(t, byName.ID, byID.ID)

	_, err = c.OrganizationByName(ctx, "missing")
	requireCoderStatus(t, err, 404)
	_, err = c.TemplateVersionByName(ctx, tpl.ID, "missing")
	requireCoderStatus(t, err, 404)
	_, err = c.OrganizationMember(ctx, org.ID.String(), me.ID.String())
	requireCoderStatus(t, err, 404)
	_, err = c.WorkspaceByOwnerAndName(ctx, uuid.NewString(), "ktt-a", codersdk.WorkspaceOptions{IncludeDeleted: true})
	requireCoderStatus(t, err, 404)
}

func TestFakeCoderCreateRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	for _, tc := range []struct {
		name     string
		job      codersdk.ProvisionerJobStatus
		archived bool
		status   int
	}{
		{name: "archived", job: codersdk.ProvisionerJobSucceeded, archived: true, status: 500},
		{name: "importing", job: codersdk.ProvisionerJobRunning, status: 406},
		{name: "import-failed", job: codersdk.ProvisionerJobFailed, status: 400},
	} {
		v := fx.fake.addVersion(fx.templateID, tc.name, tc.job)
		if tc.archived {
			fx.fake.archiveVersion(v)
		}
		_, err := fx.client.CreateUserWorkspace(ctx, fx.testerID.String(), codersdk.CreateWorkspaceRequest{TemplateVersionID: v, Name: "ktt-" + tc.name})
		requireCoderStatus(t, err, tc.status)
	}
	require.Zero(t, fx.fake.workspaceCount())
}

func TestFakeCoderCancelAndDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	f, c := fx.fake, fx.client
	ws, err := fx.create(ctx, "ktt-a")
	require.NoError(t, err)
	start := ws.LatestBuild
	running := codersdk.CancelWorkspaceBuildParams{ExpectStatus: codersdk.CancelWorkspaceBuildStatusRunning}

	// Cancel: 412 on a status mismatch, then canceling for a running job.
	requireCoderStatus(t, c.CancelWorkspaceBuild(ctx, start.ID, running), 412)
	f.setBuildJob(start.ID, codersdk.ProvisionerJobRunning)
	require.NoError(t, c.CancelWorkspaceBuild(ctx, start.ID, running))
	canceling, err := c.Workspace(ctx, ws.ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.ProvisionerJobCanceling, canceling.LatestBuild.Job.Status)
	require.NotNil(t, canceling.LatestBuild.Job.CanceledAt)                    // Coder sets it on every cancel.
	require.Nil(t, canceling.LatestBuild.Job.CompletedAt)                      // The worker still holds the job.
	requireCoderStatus(t, c.CancelWorkspaceBuild(ctx, start.ID, running), 400) // Canceling: 400 before the expect_status check.

	// A delete build waits until no build is active, and orphan deletes fail.
	deleteReq := codersdk.CreateWorkspaceBuildRequest{Transition: codersdk.WorkspaceTransitionDelete}
	_, err = c.CreateWorkspaceBuild(ctx, ws.ID, deleteReq)
	requireCoderStatus(t, err, 409)
	f.setBuildJob(start.ID, codersdk.ProvisionerJobCanceled)
	_, err = c.CreateWorkspaceBuild(ctx, ws.ID, codersdk.CreateWorkspaceBuildRequest{Transition: codersdk.WorkspaceTransitionDelete, Orphan: true})
	requireCoderStatus(t, err, 400)
	del, err := c.CreateWorkspaceBuild(ctx, ws.ID, deleteReq)
	require.NoError(t, err)
	require.Equal(t, f.operatorID, del.InitiatorID)

	builds, err := c.WorkspaceBuilds(ctx, codersdk.WorkspaceBuildsRequest{WorkspaceID: ws.ID})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{del.ID, start.ID}, []uuid.UUID{builds[0].ID, builds[1].ID})
	require.NotNil(t, builds[1].Job.CanceledAt)

	// A succeeded delete build deletes the workspace and frees its name.
	f.setBuildJob(del.ID, codersdk.ProvisionerJobSucceeded)
	requireCoderStatus(t, c.CancelWorkspaceBuild(ctx, del.ID, running), 400) // Completed: 400, not 412.
	_, err = c.Workspace(ctx, ws.ID)
	requireCoderStatus(t, err, 410)
	// Coder v2.37.2 accepts a repeated delete build on a deleted workspace.
	again, err := c.CreateWorkspaceBuild(ctx, ws.ID, deleteReq)
	require.NoError(t, err)
	require.NotEqual(t, del.ID, again.ID)
	require.Equal(t, codersdk.WorkspaceTransitionDelete, again.Transition)
	gone, err := c.DeletedWorkspace(ctx, ws.ID)
	require.NoError(t, err)
	require.Equal(t, again.ID, gone.LatestBuild.ID)
	_, err = fx.create(ctx, "ktt-a")
	require.NoError(t, err)

	// A pending job goes straight to canceled, with both timestamps set.
	pending, err := fx.create(ctx, "ktt-b")
	require.NoError(t, err)
	require.NoError(t, c.CancelWorkspaceBuild(ctx, pending.LatestBuild.ID,
		codersdk.CancelWorkspaceBuildParams{ExpectStatus: codersdk.CancelWorkspaceBuildStatusPending}))
	got, err := c.Workspace(ctx, pending.ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.ProvisionerJobCanceled, got.LatestBuild.Job.Status)
	require.NotNil(t, got.LatestBuild.Job.CompletedAt)
	require.NotNil(t, got.LatestBuild.Job.CanceledAt)
}

func TestFakeCoderWorkspaceLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newFakeCoderFixture(t)
	f, c := fx.fake, fx.client

	ws, err := fx.create(ctx, "ktt-a")
	require.NoError(t, err)
	start := ws.LatestBuild
	require.Equal(t, codersdk.WorkspaceTransitionStart, start.Transition)
	require.Equal(t, codersdk.ProvisionerJobPending, start.Job.Status)
	require.Equal(t, f.operatorID, start.InitiatorID)

	_, err = fx.create(ctx, "KTT-A")
	requireCoderStatus(t, err, 409)

	f.setBuildJob(start.ID, codersdk.ProvisionerJobRunning)
	agents := []codersdk.WorkspaceAgent{{ID: uuid.New(), Name: "main", Status: codersdk.WorkspaceAgentConnected}}
	f.setAgents(start.ID, agents...)
	agents[0].Status = codersdk.WorkspaceAgentTimeout // The fake keeps its own copy.
	f.setBuildJob(start.ID, codersdk.ProvisionerJobSucceeded)

	builds, err := c.WorkspaceBuilds(ctx, codersdk.WorkspaceBuildsRequest{WorkspaceID: ws.ID})
	require.NoError(t, err)
	require.Len(t, builds, 1)
	require.Equal(t, codersdk.ProvisionerJobSucceeded, builds[0].Job.Status)
	require.NotNil(t, builds[0].Job.CompletedAt)
	require.Len(t, builds[0].Resources[0].Agents, 1)
	require.Equal(t, codersdk.WorkspaceAgentConnected, builds[0].Resources[0].Agents[0].Status)

	// Deletion: the plain reads miss it, the include-deleted reads find it.
	f.markDeleted(ws.ID)
	_, err = c.Workspace(ctx, ws.ID)
	requireCoderStatus(t, err, 410)
	gone, err := c.DeletedWorkspace(ctx, ws.ID)
	require.NoError(t, err)
	require.Equal(t, start.ID, gone.LatestBuild.ID)
	_, err = c.WorkspaceByOwnerAndName(ctx, fx.testerID.String(), "ktt-a", codersdk.WorkspaceOptions{})
	requireCoderStatus(t, err, 404)
	gone, err = c.WorkspaceByOwnerAndName(ctx, fx.testerID.String(), "ktt-a", codersdk.WorkspaceOptions{IncludeDeleted: true})
	require.NoError(t, err)
	require.Equal(t, ws.ID, gone.ID)

	// The name is free again, and the live workspace wins the lookup.
	again, err := fx.create(ctx, "ktt-a")
	require.NoError(t, err)
	found, err := c.WorkspaceByOwnerAndName(ctx, fx.testerID.String(), "ktt-a", codersdk.WorkspaceOptions{IncludeDeleted: true})
	require.NoError(t, err)
	require.Equal(t, again.ID, found.ID)
}

func TestFakeCoderRejectsBadToken(t *testing.T) {
	t.Parallel()
	f := newFakeCoder(t)
	bad := f.client(t, time.Second)
	bad.SetSessionToken("wrong")
	_, err := bad.Workspace(context.Background(), uuid.New())
	requireCoderStatus(t, err, 401)
}

func TestFakeCoderFaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		fault     fakeFault
		status    int // zero: a transport error without a Coder status
		committed bool
	}{
		{name: "status before commit", fault: fakeFault{Status: 504}, status: 504},
		{name: "status after commit", fault: fakeFault{Status: 504, AfterCommit: true}, status: 504, committed: true},
		{name: "rate limited", fault: fakeFault{Status: 429}, status: 429},
		{name: "hang before commit", fault: fakeFault{Hang: true}},
		{name: "hang after commit", fault: fakeFault{Hang: true, AfterCommit: true}, committed: true},
		{name: "reset before commit", fault: fakeFault{Reset: true}},
		{name: "reset after commit", fault: fakeFault{Reset: true, AfterCommit: true}, committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newFakeCoderFixture(t)
			fx.client = fx.fake.client(t, 500*time.Millisecond)
			fx.fake.failNext(routeCreateWorkspace, tc.fault)

			_, err := fx.create(context.Background(), "ktt-fault")
			require.Error(t, err)
			var sdkErr *codersdk.Error
			if tc.status == 0 {
				require.False(t, errors.As(err, &sdkErr), "want a transport error, got %v", err)
			} else {
				requireCoderStatus(t, err, tc.status)
			}
			require.Equal(t, 1, fx.fake.requestCount(routeCreateWorkspace))
			want := 0
			if tc.committed {
				want = 1
			}
			require.Equal(t, want, fx.fake.workspaceCount())

			// The fault is used up: the next request reaches the handler.
			_, err = fx.create(context.Background(), "ktt-next")
			require.NoError(t, err)
		})
	}
}
