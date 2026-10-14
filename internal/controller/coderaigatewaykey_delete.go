package controller

import (
	"context"
	"slices"
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
	if deleting {
		// While the control plane holds for this object, Coder keeps running:
		// use the normal path, which retries, instead of the bounded pass
		// (#353, item q).
		holds, err := controlPlaneHolds(ctx, r.Client, controlPlane, r.Clock.Now())
		if err != nil {
			return false, nil, err
		}
		deleting = !holds
	}
	sdk, _, wait, err := controlPlaneCoderClientFor(ctx, r.Client, ref.Namespace, ref.Name, !controlPlane.DeletionTimestamp.IsZero())
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
		if _, left, failure, err := r.deleteKeys(ctx, sdk, key); err != nil {
			return false, nil, err
		} else if failure != nil {
			return r.releaseWithoutDelete(ctx, key, "the one pass while CoderControlPlane is being deleted failed: "+failure.message, left...), nil, nil
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
		deleted, _, failure, err := r.deleteKeys(ctx, sdk, key)
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
// how many it deleted. On a failed DELETE it also returns the failure and
// the IDs of the listed keys that it did not delete.
func (r *CoderAIGatewayKeyReconciler) deleteKeys(ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey) (int, []string, *aiGatewayKeyStep, error) {
	prefix, err := aiGatewayKeyNamePrefix(key.UID)
	if err != nil {
		return 0, nil, nil, err
	}
	listed, err := sdk.ListAIGatewayKeys(ctx)
	if err != nil {
		return 0, nil, aiGatewayKeyDeleteFailure("list AI Gateway keys", err), nil
	}
	own := slices.DeleteFunc(ownAIGatewayKeys(prefix, listed), func(k codersdk.AIGatewayKey) bool {
		serial, _ := aiGatewayKeySerial(prefix, k.Name)
		return serial >= key.Status.NextSerial // No create of this object used this serial (plan I3).
	})
	for i, k := range own {
		// Only Coder's own 404 proves that the key is gone (as for providers, #325).
		if err := sdk.DeleteAIGatewayKey(ctx, k.ID); err != nil && !isCoderResourceNotFound(err) {
			var left []string
			for _, rest := range own[i:] {
				left = append(left, rest.ID.String())
			}
			return i, left, aiGatewayKeyDeleteFailure("delete key "+k.Name, err), nil
		}
	}
	return len(own), nil, nil, nil
}

// aiGatewayKeyDeleteFailure classifies a failed call of the deletion like a
// failed LIST, with the deletion cadence: 60 s while Coder does not answer
// (a 429 too, instead of the shorter shared backoff), 10 minutes while it
// refuses.
func aiGatewayKeyDeleteFailure(action string, err error) *aiGatewayKeyStep {
	step := aiGatewayKeyCoderFailure(action, err)
	step.requeue, step.rateLimited = aiGatewayKeyDeleteRetry, false
	if step.stalled {
		step.requeue = aiGatewayKeyDeleteRefusedRetry
		step.message = "The object is being deleted, but the operator cannot delete its keys in Coder. " + step.message
	}
	return step
}

// releaseWithoutDelete logs the key IDs that can stay in Coder, because
// nothing can delete them any more, and returns true (ControlPlaneGone). left
// holds the IDs that a failed pass listed but did not delete.
func (r *CoderAIGatewayKeyReconciler) releaseWithoutDelete(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, why string, left ...string) bool {
	var ids []string
	if current := key.Status.CurrentKey; current != nil {
		ids = append(ids, current.ID)
	}
	for _, retiring := range key.Status.RetiringKeys {
		ids = append(ids, retiring.ID)
	}
	releasedWithoutDelete.WithLabelValues("CoderAIGatewayKey").Inc()
	log.FromContext(ctx).Info("Releasing the finalizer without deleting the keys in Coder: the keys can stay in Coder",
		"reason", "ControlPlaneGone", "why", why, "keyIDs", ids, "listedKeyIDs", left, "nextSerial", key.Status.NextSerial)
	return true
}
