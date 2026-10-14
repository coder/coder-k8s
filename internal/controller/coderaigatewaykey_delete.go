package controller

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	reasonDeletingKeys = "DeletingKeys"
	// aiGatewayKeyDeleteRetry is the retry of a deletion that Coder did not
	// answer, with no deadline (plan 3.5).
	aiGatewayKeyDeleteRetry = 60 * time.Second
	// aiGatewayKeyDeleteRefusedRetry is the retry of a deletion that Coder
	// refused (403 or no key API): only a person can fix that.
	aiGatewayKeyDeleteRefusedRetry = 10 * time.Minute
	// aiGatewayKeyBoundedDeleteTimeout bounds the one pass while the control
	// plane is being deleted (plan D12 of #154).
	aiGatewayKeyBoundedDeleteTimeout = 10 * time.Second
)

// deleteStep handles an object that is being deleted (plan 3.5). It deletes
// every key of this object in Coder, the current key included, and returns
// release when the finalizer can go. Otherwise the step says why it stays.
func (r *CoderAIGatewayKeyReconciler) deleteStep(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey) (release bool, step *aiGatewayKeyStep, err error) {
	if key.Status.NextSerial == 0 {
		return true, nil, nil // No create was ever sent, so no key of this object can exist.
	}
	ref := types.NamespacedName{Namespace: key.Namespace, Name: key.Spec.ControlPlaneRef.Name}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	if err := r.Get(ctx, ref, controlPlane); apierrors.IsNotFound(err) {
		return r.releaseWithoutDelete(ctx, key, "CoderControlPlane is gone"), nil, nil
	} else if err != nil {
		return false, nil, err
	}
	deleting := !controlPlane.DeletionTimestamp.IsZero()
	sdk, _, wait, err := controlPlaneCoderClientFor(ctx, r.Client, ref.Namespace, ref.Name, deleting)
	switch {
	case err != nil && deleting:
		return r.releaseWithoutDelete(ctx, key, "the Coder access of CoderControlPlane, which is being deleted, failed: "+err.Error()), nil, nil
	case err != nil:
		return false, nil, err
	case wait != nil && deleting:
		return r.releaseWithoutDelete(ctx, key, "CoderControlPlane is being deleted without operator access"), nil, nil
	case wait != nil:
		return false, aiGatewayKeyWait(aiGatewayKeyDeleteRetry, wait.reason, "%s The operator deletes the keys of this object once it can call Coder.", wait.message), nil
	}
	if deleting {
		// One bounded pass, then release, so the namespace deletion finishes.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, aiGatewayKeyBoundedDeleteTimeout)
		defer cancel()
		if _, failure, err := r.deleteKeys(ctx, sdk, key); err != nil {
			return false, nil, err
		} else if failure != nil {
			return r.releaseWithoutDelete(ctx, key, "the one pass while CoderControlPlane is being deleted failed: "+failure.message), nil, nil
		}
		return true, nil, nil
	}

	now := r.Clock.Now()
	if key.Status.LastCreateAttemptTime == nil {
		// Only a status edit loses this time. Wait from now (fail closed).
		key.Status.LastCreateAttemptTime = &metav1.Time{Time: now}
	}
	if settleAt := key.Status.LastCreateAttemptTime.Add(aiGatewayKeySettleWindow); now.Before(settleAt) {
		// A create sent moments ago can still commit after a LIST, so the
		// LIST waits until no write of the last attempt can land (plan S8).
		return false, aiGatewayKeyWait(max(settleAt.Sub(now), time.Second), reasonDeletingKeys,
			"The object is being deleted. The operator deletes its keys in Coder at %s, when no create of the last attempt can still commit.",
			settleAt.UTC().Format(time.RFC3339)), nil
	}
	// The pass repeats until a LIST shows no key of this object, so the
	// release follows a LIST that proves it.
	for range 2 {
		deleted, failure, err := r.deleteKeys(ctx, sdk, key)
		if err != nil || failure != nil {
			return false, failure, err
		} else if deleted == 0 {
			return true, nil, nil
		}
	}
	return false, aiGatewayKeyWait(aiGatewayKeyDeleteRetry, reasonDeletingKeys,
		"The object is being deleted. Coder still listed keys of this object after the operator deleted them, so it lists them again."), nil
}

// deleteKeys lists the keys of this object, deletes each one, and returns
// how many it deleted, or the failure that stopped it.
func (r *CoderAIGatewayKeyReconciler) deleteKeys(ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey) (int, *aiGatewayKeyStep, error) {
	prefix, err := aiGatewayKeyNamePrefix(key.UID)
	if err != nil {
		return 0, nil, err
	}
	listed, err := sdk.ListAIGatewayKeys(ctx)
	if err != nil {
		return 0, aiGatewayKeyDeleteFailure("list AI Gateway keys", err), nil
	}
	deleted := 0
	for _, k := range ownAIGatewayKeys(prefix, listed) {
		if serial, _ := aiGatewayKeySerial(prefix, k.Name); serial >= key.Status.NextSerial {
			continue // No create of this object used this serial (plan I3).
		}
		if err := sdk.DeleteAIGatewayKey(ctx, k.ID); (err != nil && coderStatus(err) != http.StatusNotFound) || coderRouteMissing(err) {
			return deleted, aiGatewayKeyDeleteFailure("delete key "+k.Name, err), nil
		}
		deleted++
	}
	return deleted, nil, nil
}

// aiGatewayKeyDeleteFailure classifies a failed call of the deletion like a
// failed LIST, with the deletion cadence: 60 s while Coder does not answer,
// 10 minutes while it refuses.
func aiGatewayKeyDeleteFailure(action string, err error) *aiGatewayKeyStep {
	step := aiGatewayKeyCoderFailure(action, err)
	step.requeue = aiGatewayKeyDeleteRetry
	if step.stalled {
		step.requeue = aiGatewayKeyDeleteRefusedRetry
		step.message = "The object is being deleted, but the operator cannot delete its keys in Coder. " + step.message
	}
	return step
}

// releaseWithoutDelete logs the key IDs that can stay in Coder, because
// nothing can delete them any more, and returns true (ControlPlaneGone).
func (r *CoderAIGatewayKeyReconciler) releaseWithoutDelete(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, why string) bool {
	var ids []string
	if current := key.Status.CurrentKey; current != nil {
		ids = append(ids, current.ID)
	}
	for _, retiring := range key.Status.RetiringKeys {
		ids = append(ids, retiring.ID)
	}
	log.FromContext(ctx).Info("Releasing the finalizer without deleting the keys in Coder: the keys can stay in Coder",
		"reason", "ControlPlaneGone", "why", why, "keyIDs", ids, "nextSerial", key.Status.NextSerial)
	return true
}
