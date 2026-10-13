package controller

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

// This file holds the workspace steps that every controller which builds
// workspaces shares: provenance after an uncertain create, the readiness rules
// for one build, the delete step, and the HTTP 429 backoff. CoderTemplateTest
// uses them today, and CoderWorkspacePool (#153) will too.

const (
	// coderBuildPoll is how often a controller reads a workspace while a
	// build or a delete runs.
	coderBuildPoll = 5 * time.Second

	// The HTTP 429 backoff doubles from 2 s to its 2 m cap, with 20 % jitter,
	// like the CoderProvisioner controller.
	coderBackoffBase = 2 * time.Second
	coderBackoffCap  = 2 * time.Minute

	// The delete retry backoff doubles from 1 m to its 30 m cap. Both values
	// are guesses, not measurements (#152 plan A3).
	coderDeleteBackoffBase = time.Minute
	coderDeleteBackoffCap  = 30 * time.Minute
)

// coderBackoff counts consecutive HTTP 429 answers per key, in memory, so a
// restart starts over. The zero value is ready to use.
type coderBackoff struct {
	attempts sync.Map // Key -> consecutive HTTP 429 answers.
}

// next counts one more 429 for key and returns the wait before the next try.
func (b *coderBackoff) next(key any) time.Duration {
	attempts := 0
	if v, ok := b.attempts.Load(key); ok {
		attempts = v.(int)
	}
	b.attempts.Store(key, attempts+1)
	backoff := coderBackoffCap
	if attempts < 6 { // 2 s * 2^6 passes the cap.
		backoff = min(coderBackoffBase<<attempts, coderBackoffCap)
	}
	jittered := time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // Jitter needs no secure randomness.
	return min(jittered, coderBackoffCap)
}

// forget drops the count for key.
func (b *coderBackoff) forget(key any) {
	b.attempts.Delete(key)
}

// workspaceProvenance returns the start build that proves the operator
// created the workspace: organization and template match the pinned values,
// and the operator user started the workspace's first build with the pinned
// version. It returns nil without proof. It reads the newest 100 builds: a
// workspace with more builds stays unproven, which keeps its finalizer.
//
// Coder v2.37.2 can answer the create request by claiming a prebuilt
// workspace when the parameters match a preset (coderd/workspaces.go:670-685).
// Build 1 is then the prebuild: a start build by the prebuilds system user.
// Only Coder's prebuilds reconciler starts builds as that user
// (enterprise/coderd/prebuilds/reconcile.go:1119-1122), and nobody can log in
// or get an API key as it (coderd/apikey.go:62-66). The claim build is the
// first build by anyone else: a start build by the operator with the
// requested version (coderd/workspaces.go:769-785). Any other build between
// the prebuild and the claim leaves the workspace unproven (fail-safe).
func workspaceProvenance(
	ctx context.Context, sdk *codersdk.Client, workspace codersdk.Workspace, operatorID uuid.UUID, organizationID, templateID, templateVersionID string,
) (*codersdk.WorkspaceBuild, error) {
	if workspace.OrganizationID.String() != organizationID || workspace.TemplateID.String() != templateID {
		return nil, nil
	}
	builds, err := sdk.WorkspaceBuilds(ctx, codersdk.WorkspaceBuildsRequest{WorkspaceID: workspace.ID, Pagination: codersdk.Pagination{Limit: 100}})
	if err != nil {
		return nil, err
	}
	prebuildsUser := uuid.MustParse(codersdk.PrebuildsSystemUserID)
	var first, claim *codersdk.WorkspaceBuild
	for i := range builds {
		b := &builds[i]
		if err := coderAnswerFor("build workspace", workspace.ID.String(), b.WorkspaceID.String()); err != nil {
			return nil, err
		}
		if b.BuildNumber == 1 {
			first = b
		}
		if b.InitiatorID != prebuildsUser && (claim == nil || b.BuildNumber < claim.BuildNumber) {
			claim = b
		}
	}
	// Without build 1 the answer proves nothing. Without a prebuild, claim is
	// build 1: a later start build by the operator does not prove who created
	// the workspace.
	if first == nil || claim == nil || claim.Transition != codersdk.WorkspaceTransitionStart || claim.InitiatorID != operatorID ||
		claim.TemplateVersionID.String() != templateVersionID {
		return nil, nil
	}
	if claim.ID == uuid.Nil {
		return nil, &coderAnswerError{msg: "assertion failed: Coder answered a start build without an ID"}
	}
	return claim, nil
}

// workspaceBuildReadiness applies the readiness rules (#152 plan 2.5) to the
// start build of workspace name, after the caller checked that it is the
// expected build. Nil means the build succeeded and every top-level agent is
// connected and ready. Workspace.Health.Healthy is never the success signal:
// it is true while agents still run their startup scripts.
func workspaceBuildReadiness(build codersdk.WorkspaceBuild, name string) *coderStep {
	switch build.Job.Status {
	case codersdk.ProvisionerJobSucceeded:
	case codersdk.ProvisionerJobFailed:
		// Only the error code: Job.Error can hold Terraform output and secrets.
		if build.Job.ErrorCode == "" {
			return coderFail("BuildFailed", "The start build of workspace %s failed.", name)
		}
		return coderFail("BuildFailed", "The start build of workspace %s failed (error code %s).", name, build.Job.ErrorCode)
	case codersdk.ProvisionerJobCanceling, codersdk.ProvisionerJobCanceled:
		// The controllers cancel only during cleanup.
		return coderFail("BuildCanceled", "Someone else canceled the start build of workspace %s.", name)
	default:
		return coderWait("WaitingForBuild", "The start build of workspace %s is %s.", name, build.Job.Status)
	}

	var agents []codersdk.WorkspaceAgent
	for _, resource := range build.Resources {
		for _, agent := range resource.Agents {
			if !agent.ParentID.Valid { // Devcontainer sub-agents have a parent.
				agents = append(agents, agent)
			}
		}
	}
	if len(agents) == 0 {
		// Coder reports a workspace without agents as healthy: a false pass.
		return coderFail("NoAgents", "Workspace %s has no agents, so nothing proves that it works.", name)
	}
	// Messages name agents by ID: a template can derive agent names from
	// parameter values, which never go into status.
	var waiting *codersdk.WorkspaceAgent
	for i := range agents {
		agent := &agents[i]
		switch agent.LifecycleState {
		case codersdk.WorkspaceAgentLifecycleStartError:
			return coderFail("AgentStartError", "The startup script of agent %s failed.", agent.ID)
		case codersdk.WorkspaceAgentLifecycleStartTimeout:
			return coderFail("AgentStartTimeout", "The startup script of agent %s timed out.", agent.ID)
		case codersdk.WorkspaceAgentLifecycleShuttingDown, codersdk.WorkspaceAgentLifecycleShutdownTimeout,
			codersdk.WorkspaceAgentLifecycleShutdownError, codersdk.WorkspaceAgentLifecycleOff:
			return coderFail("AgentStopped", "Agent %s is %s.", agent.ID, agent.LifecycleState)
		}
		switch {
		case agent.Status == codersdk.WorkspaceAgentTimeout:
			return coderFail("AgentConnectionTimeout", "Agent %s did not connect in time.", agent.ID)
		case waiting == nil && (agent.Status != codersdk.WorkspaceAgentConnected || agent.LifecycleState != codersdk.WorkspaceAgentLifecycleReady):
			waiting = agent
		}
	}
	// An agent that was ready and then disconnects makes the caller wait, not
	// fail: disconnects can be short (#152 plan A10).
	if waiting != nil {
		return coderWait("WaitingForAgents", "Agent %s is %s and %s.", waiting.ID, waiting.Status, waiting.LifecycleState)
	}
	return nil
}

// workspaceDeleteState is what a controller stores to delete one workspace
// safely across reconciles and restarts.
type workspaceDeleteState struct {
	workspaceID    uuid.UUID
	workspaceName  string
	deleteBuildID  string // The delete build this controller started or saw running.
	deleteAttempts int32  // Failed delete builds counted so far, each once.
}

// workspaceDeleteResult is the outcome of one delete step.
type workspaceDeleteResult struct {
	deleted bool // A delete build succeeded: the workspace is gone.
	// failedBuild is true when this step counted a failed delete build.
	failedBuild bool
	wait        *coderStep // Why the workspace still exists.
	requeue     time.Duration
}

// deleteCoderWorkspace runs one delete step on state's workspace and updates
// state. It acts on the latest build: it cancels an active start or stop
// build, waits for an active delete build, and retries a failed delete build
// after a backoff. It never cancels a delete build and never sends an orphan
// delete. countEveryFailure counts a failed delete build that state does not
// name, for example one whose ID a failed status write lost, or someone
// else's.
func deleteCoderWorkspace(
	ctx context.Context, sdk *codersdk.Client, state *workspaceDeleteState, now time.Time, countEveryFailure bool,
) (workspaceDeleteResult, error) {
	if state == nil || state.workspaceID == uuid.Nil {
		return workspaceDeleteResult{}, errors.New("assertion failed: the delete step needs a workspace ID")
	}
	name, workspaceID := state.workspaceName, state.workspaceID
	wait := func(requeue time.Duration, reason, format string, args ...any) (workspaceDeleteResult, error) {
		return workspaceDeleteResult{wait: coderWait(reason, format, args...), requeue: requeue}, nil
	}
	// include_deleted: a workspace whose delete build succeeded stays
	// readable. A 404 is not proof of deletion (#152 plan A9).
	workspace, err := sdk.DeletedWorkspace(ctx, workspaceID)
	if err != nil {
		return workspaceDeleteResult{wait: coderStepUnavailable("get workspace", err), requeue: coderBuildPoll}, nil
	}
	build := workspace.LatestBuild
	if err := errors.Join(coderAnswerFor("workspace", workspaceID.String(), workspace.ID.String()),
		coderAnswerFor("latest build workspace", workspaceID.String(), build.WorkspaceID.String())); err != nil {
		return workspaceDeleteResult{}, err
	}

	status := build.Job.Status
	switch {
	case build.Transition == codersdk.WorkspaceTransitionDelete && status == codersdk.ProvisionerJobSucceeded:
		return workspaceDeleteResult{deleted: true}, nil
	case build.Transition == codersdk.WorkspaceTransitionDelete && status.Active():
		// Any initiator: a delete build by someone else also deletes it.
		state.deleteBuildID = build.ID.String()
		return wait(coderBuildPoll, "Deleting", "The delete build of workspace %s is %s.", name, status)
	case build.Transition == codersdk.WorkspaceTransitionDelete && status != codersdk.ProvisionerJobFailed && status != codersdk.ProvisionerJobCanceled:
		// An unknown status proves no failure: wait.
		return wait(coderBuildPoll, "Deleting", "The delete build of workspace %s is %s.", name, status)
	case build.Transition == codersdk.WorkspaceTransitionDelete && (build.ID.String() == state.deleteBuildID || countEveryFailure):
		// Count each failed delete build once. The caller stores the count
		// before any new delete build.
		state.deleteAttempts++
		state.deleteBuildID = ""
		retryAt := coderDeleteRetryAt(build, state.deleteAttempts, now)
		// At least 1 s: the count must be stored before the next delete build.
		result, _ := wait(max(retryAt.Sub(now), time.Second), "DeleteRetrying", "Delete build %d of workspace %s ended %s. The controller retries at %s.",
			state.deleteAttempts, name, status, retryAt.UTC().Format(time.RFC3339))
		result.failedBuild = true
		return result, nil
	case build.Transition == codersdk.WorkspaceTransitionDelete:
		// Already counted, or someone else's: retry after the backoff.
		if retryAt := coderDeleteRetryAt(build, state.deleteAttempts, now); now.Before(retryAt) {
			return wait(retryAt.Sub(now), "DeleteRetrying", "The last delete build of workspace %s ended %s. The controller retries at %s.", name, status, retryAt.UTC().Format(time.RFC3339))
		}
	case status == codersdk.ProvisionerJobPending || status == codersdk.ProvisionerJobRunning:
		// expect_status makes Coder refuse the cancel when the job moved on.
		err := sdk.CancelWorkspaceBuild(ctx, build.ID, codersdk.CancelWorkspaceBuildParams{ExpectStatus: codersdk.CancelWorkspaceBuildStatus(status)})
		if err != nil && coderStatus(err) != http.StatusPreconditionFailed && coderStatus(err) != http.StatusBadRequest {
			return workspaceDeleteResult{wait: coderStepUnavailable("cancel build", err), requeue: coderBuildPoll}, nil
		}
		return wait(coderBuildPoll, "Deleting", "The controller canceled the %s build of workspace %s before deleting it.", build.Transition, name)
	case status == codersdk.ProvisionerJobCanceling:
		return wait(coderBuildPoll, "Deleting", "The %s build of workspace %s is canceling.", build.Transition, name)
	}

	deleteBuild, err := sdk.CreateWorkspaceBuild(ctx, workspaceID, codersdk.CreateWorkspaceBuildRequest{Transition: codersdk.WorkspaceTransitionDelete})
	switch {
	case coderStatus(err) == http.StatusConflict:
		// A build became active: read again.
		return wait(coderBuildPoll, "Deleting", "Another build of workspace %s started. Reading again.", name)
	case err != nil:
		return workspaceDeleteResult{wait: coderStepUnavailable("create delete build", err), requeue: coderBuildPoll}, nil
	}
	if err := errors.Join(coderAnswerFor("delete build workspace", workspaceID.String(), deleteBuild.WorkspaceID.String()),
		coderAnswerFor("delete build transition", string(codersdk.WorkspaceTransitionDelete), string(deleteBuild.Transition))); err != nil {
		return workspaceDeleteResult{}, err
	}
	if deleteBuild.ID == uuid.Nil {
		return workspaceDeleteResult{}, &coderAnswerError{msg: "assertion failed: Coder answered a delete build without an ID"}
	}
	state.deleteBuildID = deleteBuild.ID.String()
	return wait(coderBuildPoll, "Deleting", "The controller started a delete build of workspace %s.", name)
}

// coderDeleteRetryAt is when a controller may replace a failed delete build:
// its completion plus the backoff. Without a completion time it uses the
// build's own stable times, so repeated reads agree on the retry time.
func coderDeleteRetryAt(build codersdk.WorkspaceBuild, attempts int32, now time.Time) time.Time {
	anchor := now
	switch {
	case build.Job.CompletedAt != nil:
		anchor = *build.Job.CompletedAt
	case !build.UpdatedAt.IsZero():
		anchor = build.UpdatedAt
	case !build.CreatedAt.IsZero():
		anchor = build.CreatedAt
	}
	return anchor.Add(coderDeleteBackoff(attempts))
}

// coderDeleteBackoff is the wait after the given number of failed delete
// builds: 1 m, 2 m, 4 m, and so on, capped at 30 m.
func coderDeleteBackoff(attempts int32) time.Duration {
	backoff := coderDeleteBackoffBase
	for i := int32(1); i < attempts && backoff < coderDeleteBackoffCap; i++ {
		backoff *= 2
	}
	return min(backoff, coderDeleteBackoffCap)
}
