package controller_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #426: anyone who can read the object sees its UID, so a config that a
// person creates with the object's slug and the display name coder-k8s:<UID>
// is not this object's. The controller does not adopt it, does not change it,
// and the delete of the object leaves it.
func TestCoderMCPServerUIDDisplayNameNotAdopted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	req := apiKeyMCPRequest(server.Spec.Slug)
	req.DisplayName = "coder-k8s:" + string(server.UID)
	persons, err := env.dashboard(t).CreateMCPServerConfig(ctx, env.orgID, req)
	require.NoError(t, err)

	got, _ := env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, true, "SlugConflict")
	require.Empty(t, got.Status.ConfigID, "the person's config is not adopted")
	require.Len(t, env.fake.mcpServers(), 1)
	stored := env.fake.mcpServers()[0].view
	require.Equal(t, req.DisplayName, stored.DisplayName)
	require.Equal(t, persons.UpdatedAt, stored.UpdatedAt, "the controller did not change it")
	require.Zero(t, env.fake.requestCount(routeUpdateMCPServer))

	gone, _ := env.deleteMCPServer(ctx, t, got)
	require.Nil(t, gone)
	require.Zero(t, env.fake.requestCount(routeDeleteMCPServer))
	require.Len(t, env.fake.mcpServers(), 1, "the person's config stays")
}

// #426: the create sends a random nonce from the cleanup record as the
// display name, never the object UID. After a lost create answer, the next
// reconcile adopts the config that carries that nonce.
func TestCoderMCPServerLostCreateAnswerRecoversByNonce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	env.fake.failNext(routeCreateMCPServer, fakeFault{AfterCommit: true, Reset: true})
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, false, "ConfirmingCreate")
	require.Len(t, env.fake.mcpServers(), 1, "the create committed")
	nonce := env.mcpServerRecord(ctx, t, got.UID)["nonce"]
	require.Equal(t, nonce, env.fake.mcpServers()[0].view.DisplayName, "the create sent the recorded nonce")
	requireRandomNonce(t, nonce, got.UID)

	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.Equal(t, env.fake.mcpServers()[0].view.ID.String(), got.Status.ConfigID, "the config with the nonce is adopted")
	require.Equal(t, 1, env.posts())
}

// #426: once the operator knows the config ID, Coder has shown the nonce as
// the display name, so the next create gets a new one.
func TestCoderMCPServerNewNonceAfterKnownID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	got, old := env.applied(ctx, t, nil)
	first := env.mcpServerRecord(ctx, t, got.UID)["nonce"]
	requireRandomNonce(t, first, got.UID)

	got.Spec.Description = "pending"
	require.NoError(t, k8sClient.Update(ctx, got))
	require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, old))
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, false, "CoderUnavailable")
	require.Equal(t, 2, env.posts())
	second := env.mcpServerRecord(ctx, t, got.UID)["nonce"]
	requireRandomNonce(t, second, got.UID)
	require.NotEqual(t, first, second)
	require.Equal(t, second, env.requireCreated(t, got).view.DisplayName)
}

// #426 migration: a record of an operator version before #426 keeps its
// nonce coder-k8s:<UID> until the next create, so a create of that version
// whose answer was lost is still adopted. The next create gets a random
// nonce.
func TestCoderMCPServerOldNonceAdoptedUntilNextCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	req := apiKeyMCPRequest(server.Spec.Slug)
	req.DisplayName = "coder-k8s:" + string(server.UID)
	_, err := env.dashboard(t).CreateMCPServerConfig(ctx, env.orgID, req)
	require.NoError(t, err)
	attempt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	createOldCleanupRecord(ctx, t, env.namespace, "CoderMCPServer", server.UID, map[string]string{
		"namespace": env.namespace, "name": server.Name, "deploymentID": env.fake.deploymentID, "coderID": "", "type": "",
		"organizationID": env.orgID.String(), "slug": server.Spec.Slug, "createAttemptTime": attempt.Format(time.RFC3339),
	})
	server.Status.CreateAttemptTime = &metav1.Time{Time: attempt}
	require.NoError(t, k8sClient.Status().Update(ctx, server))

	got, _ := env.reconcile(ctx, t, server)
	requireMCPServerApplied(t, got)
	require.Equal(t, env.fake.mcpServers()[0].view.ID.String(), got.Status.ConfigID, "the create of the older version is adopted")
	require.Equal(t, "coder-k8s:"+string(server.UID), env.mcpServerRecord(ctx, t, got.UID)["nonce"])

	got.Spec.Description = "pending"
	require.NoError(t, k8sClient.Update(ctx, got))
	require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, env.fake.mcpServers()[0].view.ID))
	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerApplied(t, got)
	require.Equal(t, 2, env.posts(), "the setup create and one create of the controller")
	requireRandomNonce(t, env.mcpServerRecord(ctx, t, got.UID)["nonce"], got.UID)
}

// #426: after the operator learned the config ID, Coder has shown the nonce
// as the display name. A person who saw it can delete the config and create
// one with the slug and that display name. It is not this object's.
func TestCoderMCPServerShownNonceNotAdopted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	env.fake.failNext(routeUpdateMCPServer, fakeFault{Status: http.StatusBadGateway})
	got, _ := env.reconcile(ctx, t, env.create(ctx, t, nil))
	requireMCPServerStep(t, got, false, "CoderUnavailable")
	created := env.requireCreated(t, got).view
	require.Equal(t, env.mcpServerRecord(ctx, t, got.UID)["nonce"], created.DisplayName)

	require.NoError(t, env.dashboard(t).DeleteMCPServerConfig(ctx, env.orgID, created.ID))
	req := apiKeyMCPRequest(got.Spec.Slug)
	req.DisplayName = created.DisplayName
	_, err := env.dashboard(t).CreateMCPServerConfig(ctx, env.orgID, req)
	require.NoError(t, err)

	got, _ = env.reconcile(ctx, t, got)
	requireMCPServerStep(t, got, true, "SlugConflict")
	require.Empty(t, got.Status.ConfigID, "the person's config is not adopted")
	require.Equal(t, 1, env.fake.requestCount(routeUpdateMCPServer), "the controller did not change it")
}

// #426 migration: the nonce coder-k8s:<UID> of an older record proves a
// create only while that create is unconfirmed. An object that an older
// version left stalled with SlugConflict never creates again, so a person who
// later gives a config with its slug that display name does not get it
// adopted.
func TestCoderMCPServerOldNonceNotAdoptedWithoutPendingCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	createOldCleanupRecord(ctx, t, env.namespace, "CoderMCPServer", server.UID, map[string]string{
		"namespace": env.namespace, "name": server.Name, "deploymentID": env.fake.deploymentID, "coderID": "", "type": "",
		"organizationID": env.orgID.String(), "slug": server.Spec.Slug, "createAttemptTime": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	req := apiKeyMCPRequest(server.Spec.Slug)
	req.DisplayName = "coder-k8s:" + string(server.UID)
	_, err := env.dashboard(t).CreateMCPServerConfig(ctx, env.orgID, req)
	require.NoError(t, err)

	got, _ := env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, true, "SlugConflict")
	require.Empty(t, got.Status.ConfigID, "the person's config is not adopted")
}
