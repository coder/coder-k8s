package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// mcpServerDeletePoll is the retry while a delete cannot reach Coder
	// (plan 4.10 item 4).
	mcpServerDeletePoll = 60 * time.Second
	// mcpServerBoundedDeleteTimeout bounds the one delete attempt while the
	// control plane is being deleted (plan 4.10 item 5).
	mcpServerBoundedDeleteTimeout = 10 * time.Second
	// mcpServerCreateSettle is how long after a create request a missing
	// config does not yet prove absence: the request can still be in flight
	// in Coder.
	mcpServerCreateSettle = 2 * time.Minute
)

// deleteStep handles an object that is being deleted (plan 4.10). It
// deletes in Coder only a config that this object proved it created, and it
// returns release when the finalizer can go. Otherwise the step says why the
// finalizer stays. The scope gate never blocks it.
func (r *CoderMCPServerReconciler) deleteStep(ctx context.Context, server *coderv1alpha1.CoderMCPServer) (release bool, step *mcpServerStep, err error) {
	record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, server.UID)
	if errors.Is(err, errNotACleanupRecord) {
		record, err = nil, nil // Not the operator's ConfigMap: it proves nothing either way.
	} else if err != nil {
		return false, nil, err
	}
	if server.Status.ConfigID == "" && server.Status.CreateAttemptTime == nil && (record == nil || record.CoderID == "") {
		// The status shows nothing of this object in Coder. A record with a
		// config ID still locates a config, for example after a lost status
		// write. A record without one stays too: a SlugConflict clears the
		// marker also when a person renamed the config of a lost create
		// answer. The sweep removes it once Coder holds no config with the
		// slug or the nonce.
		return true, nil, nil
	}
	orgText := server.Status.OrganizationID
	if orgText == "" && record != nil {
		orgText = record.Organization // A status edit cannot strand the recorded config.
	}
	orgID, err := uuid.Parse(orgText)
	if err != nil || orgID == uuid.Nil {
		// The organization is pinned before the first MCP write.
		return false, nil, fmt.Errorf("assertion failed: MCP server %s/%s can exist in Coder but has status.organizationID %q", server.Namespace, server.Name, server.Status.OrganizationID)
	}
	ref := types.NamespacedName{Namespace: server.Namespace, Name: server.Spec.ControlPlaneRef.Name}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	if err := r.Get(ctx, ref, controlPlane); apierrors.IsNotFound(err) {
		return r.releaseWithoutDelete(ctx, server, "CoderControlPlane is gone"), nil, nil
	} else if err != nil {
		return false, nil, err
	}
	deleting := !controlPlane.DeletionTimestamp.IsZero()
	if deleting {
		// While the control plane holds for this object, Coder keeps running:
		// use the normal path, which retries, instead of the bounded pass
		// (#382, #353).
		now := time.Now()
		holds, err := controlPlaneHolds(ctx, r.Client, controlPlane, now)
		if err != nil {
			return false, nil, err
		}
		deleting = !holds
		if holds {
			// A pass under the hold ends at the hold end, however slowly
			// Coder answers. The bounded pass after it then still fits in
			// the control plane's grace period.
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, controlPlaneHoldEnd(controlPlane))
			defer cancel()
			defer func() {
				if step != nil {
					// From the time the pass ended: a pass that ran until the
					// deadline retries right after the hold end.
					step.requeue = controlPlaneHoldRequeue(controlPlane, time.Now(), step.requeue)
				}
			}()
		}
	}
	sdk, _, wait, err := controlPlaneCoderClientFor(ctx, r.Client, ref.Namespace, ref.Name, !controlPlane.DeletionTimestamp.IsZero())
	switch {
	case err != nil && deleting:
		// One attempt only, also when the access setup fails.
		return r.releaseWithoutDelete(ctx, server, "the Coder access of CoderControlPlane, which is being deleted, failed: "+err.Error()), nil, nil
	case err != nil:
		return false, nil, err
	case wait != nil && deleting:
		return r.releaseWithoutDelete(ctx, server, "CoderControlPlane is being deleted without operator access"), nil, nil
	case wait != nil:
		return false, mcpServerWait(mcpServerDeletePoll, wait.reason, "%s The controller deletes MCP server config %s once it can call Coder.", wait.message, server.Spec.Slug), nil
	}
	if deleting {
		// One bounded attempt, then release, so the namespace deletion
		// finishes.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mcpServerBoundedDeleteTimeout)
		defer cancel()
	}
	if record == nil && server.Status.ConfigID != "" {
		// A config from before records existed gets its record before a
		// delete that can release without a confirmation.
		r.backfillMCPServerRecord(ctx, sdk, server)
		if record, _, err = readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, server.UID); errors.Is(err, errNotACleanupRecord) {
			record = nil
		} else if err != nil {
			return false, nil, err
		}
	}
	recordedID := ""
	if record != nil {
		// The record names the deployment that holds the config. When the
		// control plane now reaches another deployment, a delete there
		// proves nothing, so the record stays for a sweep (#353 invariant 7).
		id, err := coderDeploymentID(ctx, sdk)
		switch {
		case err != nil && deleting:
			return r.releaseWithoutDelete(ctx, server, "the deployment ID read while CoderControlPlane is being deleted failed: "+coderErrorSummary(err)), nil, nil
		case err != nil:
			return false, mcpServerWait(mcpServerDeletePoll, "CoderUnavailable", "read the Coder deployment ID: %s", coderErrorSummary(err)), nil
		case id != record.DeploymentID:
			return r.releaseWithoutDelete(ctx, server, fmt.Sprintf("CoderControlPlane now reaches Coder deployment %s, but the config is in deployment %s", id, record.DeploymentID)), nil, nil
		}
		recordedID = record.CoderID
	}
	unproven, step, err := deleteMCPServerInCoder(ctx, sdk, server, orgID, recordedID)
	if err == nil && step == nil && unproven {
		// A config that can be this object's stays in Coder, so the record
		// stays for a sweep, which reports it for manual cleanup (#353).
		return true, nil, nil
	}
	if err == nil && step == nil {
		// Coder confirmed that it holds no config of this object. Only then
		// does the record go (#353 invariant 7).
		return true, nil, deleteCleanupRecord(ctx, r.Client, r.APIReader, r.OperatorNamespace, server.UID)
	}
	switch {
	case err != nil && deleting:
		// The attempt ended with an impossible answer. Log it, and still
		// release: the one attempt is over.
		log.FromContext(ctx).Error(err, "The one delete attempt while CoderControlPlane is being deleted got an impossible answer")
		return r.releaseWithoutDelete(ctx, server, "the one delete attempt while CoderControlPlane is being deleted failed: "+err.Error()), nil, nil
	case err != nil:
		return false, nil, err
	case step != nil && deleting:
		return r.releaseWithoutDelete(ctx, server, "the one delete attempt while CoderControlPlane is being deleted failed: "+step.message), nil, nil
	}
	return false, step, nil
}

// deleteMCPServerInCoder deletes the config of this object in Coder and
// returns a nil step when it is done. Without status.configID it deletes
// recordedID, the config ID of the cleanup record: a failed status write
// after the create can leave the ID only there. Without either ID it deletes
// nothing: a person can edit the slug and the display name of a config, so
// the nonce does not prove that this object created it (#422). Only Coder's
// own 404 proves absence (#337). unproven reports a config that it leaves
// although it can be this object's: a config with the slug, or one with the
// nonce under another slug.
func deleteMCPServerInCoder(ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID, recordedID string) (unproven bool, step *mcpServerStep, err error) {
	var id uuid.UUID
	if configID := cmp.Or(server.Status.ConfigID, recordedID); configID != "" {
		parsed, err := uuid.Parse(configID)
		if err != nil || parsed == uuid.Nil || parsed.String() != configID {
			return false, nil, fmt.Errorf("assertion failed: MCP server %s/%s has config ID %q, which is not a canonical UUID", server.Namespace, server.Name, configID)
		}
		id = parsed
	} else {
		configs, err := sdk.MCPServerConfigs(ctx, orgID)
		if err != nil && !isCoderResourceNotFound(err) {
			step := mcpServerListFailed(err)
			step.requeue = mcpServerDeletePoll
			return false, step, nil
		}
		// Coder's own 404 means that the pinned organization is gone, and no
		// config of it can be reached any more (#378).
		found, step := mcpServerWithSlug(configs, server.Spec.Slug, orgID)
		switch {
		case step != nil:
			step.requeue = max(step.requeue, mcpServerDeletePoll)
			return false, step, nil
		case found == nil:
			// A missing config proves absence only for a site owner. A role
			// change in Coder has no watch, so the wait uses the delete poll.
			if step := mcpServerRequireSiteOwner(ctx, sdk); step != nil {
				step.requeue = mcpServerDeletePoll
				return false, step, nil
			}
			if wait := mcpServerCreateSettle - time.Since(server.Status.CreateAttemptTime.Time); wait > 0 {
				// A create sent moments ago can still commit after this read.
				return false, mcpServerWait(wait, "ConfirmingCreate",
					"MCP server config %s is not in Coder, but its create request was sent at %s and can still commit. The controller reads again before it releases the finalizer.",
					server.Spec.Slug, server.Status.CreateAttemptTime.UTC().Format(time.RFC3339)), nil
			}
			return mcpServerWithDisplayName(configs, mcpServerNonce(server)) != nil, nil, nil
		default:
			return true, nil, nil
		}
	}
	if err := sdk.DeleteMCPServerConfig(ctx, orgID, id); err != nil && !isCoderResourceNotFound(err) {
		return false, mcpServerWait(mcpServerDeletePoll, "CoderUnavailable", "delete MCP server config %s (ID %s): %s", server.Spec.Slug, id, coderErrorSummary(err)), nil
	}
	return false, nil, nil
}

// releaseWithoutDelete logs what can stay in Coder, because nothing can
// delete it any more, and returns true (ControlPlaneGone).
func (r *CoderMCPServerReconciler) releaseWithoutDelete(ctx context.Context, server *coderv1alpha1.CoderMCPServer, why string) bool {
	countReleasedWithoutDelete(server.UID, "CoderMCPServer")
	log.FromContext(ctx).Info("Releasing the finalizer without a delete in Coder: the MCP server config can stay in the Coder database",
		"reason", "ControlPlaneGone", "why", why, "slug", server.Spec.Slug, "configID", server.Status.ConfigID,
		"createPending", server.Status.CreateAttemptTime != nil)
	return true
}
