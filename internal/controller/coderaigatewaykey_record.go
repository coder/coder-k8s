package controller

import (
	"context"
	"errors"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// cleanupRecordKindAIGatewayKey is the record kind of a CoderAIGatewayKey.
// Its record holds the key name prefix ck8s-<uid>- as the nonce, which finds
// every key of the object, and the current key ID once it is known.
const cleanupRecordKindAIGatewayKey = "CoderAIGatewayKey"

// writeAIGatewayKeyRecord writes the cleanup record of key with the deployment
// ID that Coder answers now. createAttempt is the time of the create that the
// caller sends next, or nil. A failed read or write returns a wait or an
// error, and the caller then sends no create (#353 invariant 1).
func (r *CoderAIGatewayKeyReconciler) writeAIGatewayKeyRecord(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, prefix string, createAttempt *time.Time,
) (*aiGatewayKeyStep, error) {
	previous, existing, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, key.UID)
	if err != nil {
		return nil, err
	}
	deploymentID, err := coderDeploymentID(ctx, sdk)
	if err != nil {
		return aiGatewayKeyWait(aiGatewayKeyRetry, "CoderUnavailable", "read the Coder deployment ID for the cleanup record: %s", coderErrorSummary(err)), nil
	}
	if previous != nil && previous.DeploymentID != deploymentID {
		// The keys of this object are in the old Coder database. The record
		// is their only locator, so the controller neither overwrites it nor
		// creates a key in the new database.
		return aiGatewayKeyStalled("CoderDeploymentChanged",
			"The keys of this object (names starting with %s) are in Coder deployment %s, but CoderControlPlane %s now reaches deployment %s. Delete the keys there, then delete ConfigMap %s/%s. Until then the operator creates no key.",
			prefix, previous.DeploymentID, key.Spec.ControlPlaneRef.Name, deploymentID, r.OperatorNamespace, cleanupRecordObjectName(key.UID)), nil
	}
	record := cleanupRecord{
		Kind: cleanupRecordKindAIGatewayKey, UID: key.UID, Namespace: key.Namespace, Name: key.Name,
		DeploymentID: deploymentID, Nonce: prefix,
	}
	if current := key.Status.CurrentKey; current != nil {
		record.CoderID = current.ID
	}
	switch {
	case createAttempt != nil:
		record.CreateAttemptTime = *createAttempt
	case key.Status.LastCreateAttemptTime != nil:
		record.CreateAttemptTime = key.Status.LastCreateAttemptTime.Time
	case previous != nil:
		record.CreateAttemptTime = previous.CreateAttemptTime
	}
	return nil, writeCleanupRecord(ctx, r.Client, r.OperatorNamespace, existing, record)
}

// recordIssuedKeyID writes the ID of the key that this reconcile created into
// the record that this reconcile wrote before the create, so it needs no new
// deployment ID read. A failure is logged, and recordAIGatewayKeyID retries.
func (r *CoderAIGatewayKeyReconciler) recordIssuedKeyID(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, id string) {
	record, existing, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, key.UID)
	if err == nil && record == nil {
		err = errors.New("the cleanup record is gone")
	}
	if err == nil && record.CoderID != id {
		record.CoderID = id
		err = writeCleanupRecord(ctx, r.Client, r.OperatorNamespace, existing, *record)
	}
	if err != nil {
		log.FromContext(ctx).Info("Cannot record the new key ID in the cleanup record", "error", err.Error())
	}
}

// recordAIGatewayKeyID writes the ID of a current key that Coder lists, and a
// create time that the observe step stamped later (#353), into the record. It
// never stops the reconcile: the record already holds the prefix, which
// finds the key too, so a failure is logged and retried.
func (r *CoderAIGatewayKeyReconciler) recordAIGatewayKeyID(ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, prefix string) {
	record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, key.UID)
	if err == nil && record != nil && record.CoderID == key.Status.CurrentKey.ID &&
		(key.Status.LastCreateAttemptTime == nil || !record.CreateAttemptTime.Before(key.Status.LastCreateAttemptTime.Time)) {
		return
	}
	if err == nil {
		var step *aiGatewayKeyStep
		if step, err = r.writeAIGatewayKeyRecord(ctx, sdk, key, prefix, nil); step != nil {
			log.FromContext(ctx).Info("Cannot record the current key ID in the cleanup record", "reason", step.reason, "message", step.message)
			return
		}
	}
	if err != nil {
		log.FromContext(ctx).Info("Cannot record the current key ID in the cleanup record", "error", err.Error())
	}
}

// aiGatewayKeyRecordDeployment checks, before a delete pass, that the control
// plane still reaches the Coder deployment that the record names. It returns
// why the pass proves nothing ("" when it does), or a wait when the read
// failed. Without a record (an object from before records) the pass runs.
func (r *CoderAIGatewayKeyReconciler) aiGatewayKeyRecordDeployment(ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey) (string, *aiGatewayKeyStep, error) {
	record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, key.UID)
	if errors.Is(err, errNotACleanupRecord) {
		err = nil // Not the operator's ConfigMap: it proves nothing either way.
	}
	if err != nil || record == nil {
		return "", nil, err
	}
	id, err := coderDeploymentID(ctx, sdk)
	if err != nil {
		return "", aiGatewayKeyWait(aiGatewayKeyDeleteRetry, "CoderUnavailable", "read the Coder deployment ID: %s", coderErrorSummary(err)), nil
	}
	if id != record.DeploymentID {
		return "CoderControlPlane now reaches Coder deployment " + id + ", but the keys are in deployment " + record.DeploymentID, nil, nil
	}
	return "", nil, nil
}
