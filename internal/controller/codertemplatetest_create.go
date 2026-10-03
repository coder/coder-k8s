package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	templateTestBackoffBase = 2 * time.Second
	templateTestBackoffCap  = 2 * time.Minute
)

// createWorkspace writes the create marker, then sends the only create
// request of this attempt. A marker write conflict ends the reconcile before
// any request, so a decision made on a stale object never reaches Coder.
func (r *CoderTemplateTestReconciler) createWorkspace(
	ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, before *coderv1alpha1.CoderTemplateTestStatus, now, deadline time.Time,
) (*templateTestStep, error) {
	tt.Status.CreateAttemptTime = &metav1.Time{Time: now}
	applyTemplateTestStep(tt, now, templateTestWait("CreatingWorkspace", "Creating workspace %s.", tt.Status.WorkspaceName))
	if err := r.writeStatus(ctx, tt, before); err != nil {
		return nil, err
	}
	// The marker write can take a while. No request was sent yet, so a
	// test past its deadline clears the marker and fails without a workspace.
	if !r.Clock.Now().Before(deadline) {
		tt.Status.CreateAttemptTime = nil
		return templateTestWait("CreatingWorkspace", "No create request was sent."), nil
	}

	versionID, err := uuid.Parse(tt.Status.TemplateVersionID)
	if err != nil {
		return nil, fmt.Errorf("assertion failed: pinned version ID %q is not a UUID: %w", tt.Status.TemplateVersionID, err)
	}
	params := make([]codersdk.WorkspaceBuildParameter, 0, len(tt.Spec.Parameters))
	for _, p := range tt.Spec.Parameters {
		params = append(params, codersdk.WorkspaceBuildParameter{Name: p.Name, Value: p.Value})
	}
	// Only TemplateVersionID, never TemplateID, so Coder checks the archived flag.
	workspace, err := sdk.CreateUserWorkspace(ctx, tt.Status.OwnerID, codersdk.CreateWorkspaceRequest{
		TemplateVersionID:   versionID,
		Name:                tt.Status.WorkspaceName,
		RichParameterValues: params,
		AutomaticUpdates:    codersdk.AutomaticUpdatesNever,
	})
	status := coderStatus(err)
	switch {
	case err == nil:
		if workspace.ID == uuid.Nil || workspace.LatestBuild.ID == uuid.Nil {
			return nil, &coderAnswerError{msg: "assertion failed: Coder answered start build without a workspace or build ID"}
		}
		if err := errors.Join(
			coderAnswerFor("workspace", tt.Status.WorkspaceName, workspace.Name),
			coderAnswerFor("start build workspace", workspace.ID.String(), workspace.LatestBuild.WorkspaceID.String()),
			coderAnswerFor("start build transition", string(codersdk.WorkspaceTransitionStart), string(workspace.LatestBuild.Transition)),
			coderAnswerFor("workspace owner", tt.Status.OwnerID, workspace.OwnerID.String()),
			coderAnswerFor("start build version", tt.Status.TemplateVersionID, workspace.LatestBuild.TemplateVersionID.String()),
		); err != nil {
			return nil, err // The marker stays, so the confirm path decides.
		}
		tt.Status.WorkspaceID, tt.Status.StartBuildID = workspace.ID.String(), workspace.LatestBuild.ID.String()
		return templateTestWait("WaitingForBuild", "Workspace %s was created.", workspace.Name), nil
	case createHadNoEffect(err):
		tt.Status.CreateAttemptTime = nil
		if status == http.StatusNotAcceptable {
			return templateTestWait("TemplateVersionImporting", "Version %s is still importing.", tt.Status.TemplateVersionName), nil
		}
		step := templateTestWait("CreateRetrying", "The create request had no effect: %s", coderErrorSummary(err))
		step.rateLimited = status == http.StatusTooManyRequests
		return step, nil
	case status == http.StatusConflict:
		// This test sends a request only right after a marker, and clears a
		// marker only after a result without effect, so someone else holds
		// the name. The controller never adopts that workspace.
		return templateTestFailNotCreated("WorkspaceNameConflict", "Workspace %s already exists: %s", tt.Status.WorkspaceName, coderErrorSummary(err)), nil
	case status >= 400 && status < 500:
		return templateTestFailNotCreated("CreateRejected", "Coder rejected the create request: %s", coderErrorSummary(err)), nil
	default:
		return templateTestWait("ConfirmingCreate", "The create result is uncertain (%s). The controller reads Coder instead of sending it again.", coderErrorSummary(err)), nil
	}
}

// createHadNoEffect reports results that prove Coder changed nothing: the
// rate limiter and authentication answer before the handler, Coder v2.37.2
// answers 406 from inside a rolled-back transaction (coderd/workspaces.go,
// createWorkspace), and a failed dial or TLS handshake never sent the request.
func createHadNoEffect(err error) bool {
	switch coderStatus(err) {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusNotAcceptable:
		return true
	}
	var opErr *net.OpError
	var certErr *tls.CertificateVerificationError
	var headerErr tls.RecordHeaderError
	return (errors.As(err, &opErr) && opErr.Op == "dial") || errors.As(err, &certErr) || errors.As(err, &headerErr)
}

func templateTestFailNotCreated(reason, format string, args ...any) *templateTestStep {
	step := templateTestFail(reason, format, args...)
	step.deleted = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "NotCreated", Message: "Coder has no workspace from this test."}
	return step
}

// retryAfter returns the requeue delay for a step that waits. After HTTP 429
// it backs off from 2 s to 2 m with 20 % jitter, like the CoderProvisioner
// controller. The attempt count lives in memory, so a restart starts over.
// Reconcile forgets it after any outcome other than a wait after 429.
func (r *CoderTemplateTestReconciler) retryAfter(key types.NamespacedName, tt *coderv1alpha1.CoderTemplateTest, step *templateTestStep) time.Duration {
	if !step.rateLimited {
		if templateTestMayHaveWorkspace(tt) {
			return templateTestRunningPoll
		}
		return templateTestPendingPoll
	}
	attempts := 0
	if v, ok := r.rateLimited.Load(key); ok {
		attempts = v.(int)
	}
	r.rateLimited.Store(key, attempts+1)
	backoff := templateTestBackoffCap
	if attempts < 6 { // 2 s * 2^6 passes the cap.
		backoff = min(templateTestBackoffBase<<attempts, templateTestBackoffCap)
	}
	jittered := time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // Jitter needs no secure randomness.
	return min(jittered, templateTestBackoffCap)
}
