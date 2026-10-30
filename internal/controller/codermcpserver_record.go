package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// cleanupRecordKindMCPServer is the record kind of a CoderMCPServer. Its
// record holds the nonce, the pinned organization ID and the slug, which
// find a config whose ID is not known yet, and the config ID once it is.
const cleanupRecordKindMCPServer = "CoderMCPServer"

// writeMCPServerRecord writes the cleanup record of server with the
// deployment ID that Coder answers now. createAttempt is the time of a create
// that the caller sends next, or nil. A failed read or write returns a wait
// or an error, and the caller then sends no create (#353 invariant 1).
func (r *CoderMCPServerReconciler) writeMCPServerRecord(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, createAttempt *time.Time,
) (*mcpServerStep, error) {
	if orgID, err := uuid.Parse(server.Status.OrganizationID); err != nil || orgID == uuid.Nil || orgID.String() != server.Status.OrganizationID {
		return nil, fmt.Errorf("assertion failed: MCP server %s/%s needs a pinned organization ID for its cleanup record, but has %q", server.Namespace, server.Name, server.Status.OrganizationID)
	}
	if id := server.Status.ConfigID; id != "" {
		// A sweep deletes the recorded ID, so only a canonical UUID goes in.
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
			return nil, fmt.Errorf("assertion failed: MCP server %s/%s has status.configID %q, which is not a canonical UUID", server.Namespace, server.Name, id)
		}
	}
	previous, existing, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, server.UID)
	if err != nil {
		return nil, err
	}
	deploymentID, err := coderDeploymentID(ctx, sdk)
	if err != nil {
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "read the Coder deployment ID for the cleanup record: %s", coderErrorSummary(err)), nil
	}
	if previous != nil && previous.DeploymentID != deploymentID {
		// The record is the only locator of what this object made in the old
		// Coder database, so the controller neither overwrites it nor creates
		// in the new one.
		return r.mcpServerDeploymentChanged(server, previous, deploymentID), nil
	}
	record := cleanupRecord{
		Kind: cleanupRecordKindMCPServer, UID: server.UID, Namespace: server.Namespace, Name: server.Name,
		DeploymentID: deploymentID, Nonce: mcpServerNonce(server), CoderID: server.Status.ConfigID,
		Organization: server.Status.OrganizationID, Slug: server.Spec.Slug,
	}
	if record.CoderID == "" && createAttempt == nil && previous != nil {
		// A status without the ID does not prove that the config is gone, and
		// after the sync only the recorded ID finds it. A new create drops the
		// ID: the controller clears it only after Coder's 404 for that ID.
		record.CoderID = previous.CoderID
	}
	switch {
	case createAttempt != nil:
		record.CreateAttemptTime = *createAttempt
	case server.Status.CreateAttemptTime != nil:
		record.CreateAttemptTime = server.Status.CreateAttemptTime.Time
	case previous != nil:
		// The marker went after a confirmed create. The sweep still waits
		// for the settle window after that create.
		record.CreateAttemptTime = previous.CreateAttemptTime
	}
	return nil, writeCleanupRecord(ctx, r.Client, r.OperatorNamespace, existing, record)
}

// mcpServerDeploymentChanged is the stall of an object whose record names
// another Coder deployment than the one that the control plane reaches now.
func (r *CoderMCPServerReconciler) mcpServerDeploymentChanged(server *coderv1alpha1.CoderMCPServer, record *cleanupRecord, deploymentID string) *mcpServerStep {
	return mcpServerStalled(mcpServerResync, "CoderDeploymentChanged",
		"MCP server config %s (ID %q) is in Coder deployment %s, but CoderControlPlane %s now reaches deployment %s. Delete the config there, then delete ConfigMap %s/%s. Until then the controller creates nothing.",
		server.Spec.Slug, record.CoderID, record.DeploymentID, server.Spec.ControlPlaneRef.Name, deploymentID, r.OperatorNamespace, cleanupRecordObjectName(server.UID))
}

// recordedDeploymentChanged returns the CoderDeploymentChanged stall when the
// record names another Coder deployment than the one that the control plane
// reaches now, or a wait when the deployment ID read fails. Without a record
// it returns nil. A config that looks deleted can then still exist in the
// old deployment.
func (r *CoderMCPServerReconciler) recordedDeploymentChanged(ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer) (*mcpServerStep, error) {
	record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, server.UID)
	if err != nil || record == nil {
		return nil, err
	}
	deploymentID, err := coderDeploymentID(ctx, sdk)
	if err != nil {
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "read the Coder deployment ID: %s", coderErrorSummary(err)), nil
	}
	if deploymentID != record.DeploymentID {
		return r.mcpServerDeploymentChanged(server, record, deploymentID), nil
	}
	return nil, nil
}

// backfillMCPServerRecord writes the missing record of a config that exists
// in Coder from before records existed. It runs before every other check, so
// an object that a check stalls or disables gets its record too. It never
// stops the reconcile: a failure is logged and retried.
func (r *CoderMCPServerReconciler) backfillMCPServerRecord(ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer) {
	if server.Status.ConfigID == "" {
		return // The create path writes the record.
	}
	if step, err := r.ensureMCPServerRecord(ctx, sdk, server); err != nil || step != nil {
		log.FromContext(ctx).Info("Cannot write the cleanup record of an MCP server config that exists in Coder", "error", err, "step", step)
	}
}

// ensureMCPServerRecord makes sure that a server which can exist in Coder has
// a record with its current config ID before the sync replaces the nonce.
// Objects from before records existed get one here too. With only a create
// marker, the write checks the deployment ID before a slug conflict in another
// deployment can clear the marker (#353).
func (r *CoderMCPServerReconciler) ensureMCPServerRecord(ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer) (*mcpServerStep, error) {
	if server.Status.ConfigID == "" && server.Status.CreateAttemptTime == nil {
		return nil, nil // Nothing of this object can exist in Coder yet. The create writes the record.
	}
	record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, server.UID)
	if err != nil {
		return nil, err
	}
	if record != nil && record.CoderID != "" && record.CoderID == server.Status.ConfigID {
		return nil, nil
	}
	return r.writeMCPServerRecord(ctx, sdk, server, nil)
}
