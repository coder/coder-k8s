package controller

import (
	"context"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// cleanupRecordKindAIProvider is the record kind of a CoderAIProvider.
const cleanupRecordKindAIProvider = "CoderAIProvider"

// writeAIProviderRecord writes the cleanup record of provider with the
// deployment ID that Coder answers now, so the record names the database that
// the next create or the learned provider ID belongs to. createAttempt is the
// time of a create that the caller sends next, or nil. It returns the
// recorded nonce, the display name of that create. A failed read or write
// returns a wait or an error, and the caller then sends no create (#353
// invariant 1).
func (r *CoderAIProviderReconciler) writeAIProviderRecord(
	ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider, createAttempt *time.Time,
) (string, *aiProviderStep, error) {
	previous, existing, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, provider.UID)
	if err != nil {
		return "", nil, err
	}
	deploymentID, err := coderDeploymentID(ctx, sdk)
	if err != nil {
		return "", aiProviderWait(aiProviderRetry, "CoderUnavailable", "read the Coder deployment ID for the cleanup record: %s", coderErrorSummary(err)), nil
	}
	if previous != nil && previous.DeploymentID != deploymentID {
		// The control plane now reaches another Coder database. The record
		// is the only locator of what this object made in the old one, so
		// the controller neither overwrites it nor creates in the new one.
		return "", r.aiProviderDeploymentChanged(provider, previous, deploymentID), nil
	}
	record := cleanupRecord{
		Kind: cleanupRecordKindAIProvider, UID: provider.UID, Namespace: provider.Namespace, Name: provider.Name,
		DeploymentID: deploymentID, Nonce: keptCreateNonce(aiProviderNoncePrefix, provider.UID, previous), CoderID: provider.Status.ProviderID, Type: provider.Spec.Type,
	}
	if createAttempt != nil {
		record.Nonce = nextCreateNonce(aiProviderNoncePrefix, previous)
	}
	if record.CoderID == "" && createAttempt == nil && previous != nil {
		// A status without the ID does not prove that the provider is gone,
		// and after the sync only the recorded ID finds it. A new create
		// drops the ID: it follows Coder's 404 for the name, and Coder
		// cannot rename a provider.
		record.CoderID = previous.CoderID
	}
	if createAttempt != nil {
		record.CreateAttemptTime = *createAttempt
	} else if provider.Status.CreateAttemptTime != nil {
		record.CreateAttemptTime = provider.Status.CreateAttemptTime.Time
	} else if previous != nil {
		// The marker went after a confirmed create. The sweep still waits
		// for the settle window after that create.
		record.CreateAttemptTime = previous.CreateAttemptTime
	}
	return record.Nonce, nil, writeCleanupRecord(ctx, r.Client, r.OperatorNamespace, existing, record)
}

// aiProviderDeploymentChanged is the stall of an object whose record names
// another Coder deployment than the one that the control plane reaches now.
func (r *CoderAIProviderReconciler) aiProviderDeploymentChanged(provider *coderv1alpha1.CoderAIProvider, record *cleanupRecord, deploymentID string) *aiProviderStep {
	return aiProviderStalled(aiProviderResync, "CoderDeploymentChanged",
		"Provider %s (ID %q) is in Coder deployment %s, but CoderControlPlane %s now reaches deployment %s. Delete the provider there, then delete ConfigMap %s/%s. Until then the controller creates nothing.",
		provider.Name, record.CoderID, record.DeploymentID, provider.Spec.ControlPlaneRef.Name, deploymentID, r.OperatorNamespace, cleanupRecordObjectName(provider.UID))
}

// recordedDeploymentChanged returns the CoderDeploymentChanged stall when the
// record names another Coder deployment than the one that the control plane
// reaches now, or a wait when the deployment ID read fails. Without a record
// it returns nil. A provider that looks deleted can then still exist in the
// old deployment.
func (r *CoderAIProviderReconciler) recordedDeploymentChanged(ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider) (*aiProviderStep, error) {
	record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, provider.UID)
	if err != nil || record == nil {
		return nil, err
	}
	deploymentID, err := coderDeploymentID(ctx, sdk)
	if err != nil {
		return aiProviderWait(aiProviderRetry, "CoderUnavailable", "read the Coder deployment ID: %s", coderErrorSummary(err)), nil
	}
	if deploymentID != record.DeploymentID {
		return r.aiProviderDeploymentChanged(provider, record, deploymentID), nil
	}
	return nil, nil
}

// backfillAIProviderRecord writes the missing record of a provider that
// exists in Coder from before records existed. It runs before every other
// check, so a stalled object (a bad base URL or key Secret) gets its record
// too. It never stops the reconcile: a failure is logged and retried.
func (r *CoderAIProviderReconciler) backfillAIProviderRecord(ctx context.Context, provider *coderv1alpha1.CoderAIProvider) {
	if provider.Status.ProviderID == "" || checkAIProviderID(provider) != nil {
		return // The create path writes the record. A forged ID fails later checks.
	}
	if record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, provider.UID); err == nil && record != nil && record.CoderID == provider.Status.ProviderID {
		return
	}
	sdk, _, wait, err := controlPlaneCoderClient(ctx, r.Client, provider.Namespace, provider.Spec.ControlPlaneRef.Name)
	if err != nil || wait != nil {
		return // The control plane cannot be reached now. A later reconcile retries.
	}
	if step, err := r.ensureAIProviderRecord(ctx, sdk, provider); err != nil || step != nil {
		log.FromContext(ctx).Info("Cannot write the cleanup record of a provider that exists in Coder", "error", err, "step", step)
	}
}

// ensureAIProviderRecord makes sure that a provider which can exist in Coder
// has a record with its current provider ID. Objects from before records
// existed get one here too.
func (r *CoderAIProviderReconciler) ensureAIProviderRecord(
	ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider,
) (*aiProviderStep, error) {
	if provider.Status.ProviderID == "" && provider.Status.CreateAttemptTime == nil {
		return nil, nil // Nothing of this object can exist in Coder yet. The create writes the record.
	}
	if provider.Status.ProviderID != "" {
		// A sweep deletes the recorded ID, so only a canonical UUID goes in.
		if err := checkAIProviderID(provider); err != nil {
			return nil, err
		}
	}
	record, _, err := readCleanupRecord(ctx, r.APIReader, r.OperatorNamespace, provider.UID)
	if err != nil {
		return nil, err
	}
	// A record without an ID names the deployment of a pending create, so
	// the write checks the deployment ID before the create is confirmed in
	// another deployment (#353).
	if record != nil && record.CoderID != "" && record.CoderID == provider.Status.ProviderID {
		return nil, nil
	}
	_, step, err := r.writeAIProviderRecord(ctx, sdk, provider, nil)
	return step, err
}
