package controller

import (
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
	// aiProviderDeletePoll is the retry of a delete that Coder did not
	// confirm. A delete has no deadline (plan 4.6).
	aiProviderDeletePoll = 60 * time.Second
	// aiProviderBoundedDeleteTimeout bounds the one delete attempt while the
	// control plane is being deleted (plan D12).
	aiProviderBoundedDeleteTimeout = 10 * time.Second
	// aiProviderCreateSettle is how long after a create request a 404 does
	// not yet prove absence: the request can still be in flight in Coder.
	aiProviderCreateSettle = 2 * time.Minute
)

// coderNotFoundMessage is the fixed message of Coder's resource 404, which
// the provider routes answer (coderd/httpapi/httpapi.go:149-155 and
// coderd/ai_providers.go:636-638 in v2.37.3). A 404 without it, for example
// an HTML page of a proxy or "Route not found." for a wrong base URL, does
// not prove that Coder has no such provider (#323).
const coderNotFoundMessage = "Resource not found or you do not have access to this resource"

// isCoderResourceNotFound reports a 404 that Coder itself answered.
func isCoderResourceNotFound(err error) bool {
	var sdkErr *codersdk.Error
	return isCoderNotFound(err) && errors.As(err, &sdkErr) && sdkErr.Message == coderNotFoundMessage
}

// checkAIProviderID fails unless status.providerID is a canonical UUID. The
// controller puts it into /api/v2/ai/providers/{id}, which also accepts a
// name, and the SDK resolves ".." segments, so any other value could send
// the operator token to another Coder route (#323). Only the controller
// writes it, so another value is impossible.
func checkAIProviderID(provider *coderv1alpha1.CoderAIProvider) error {
	id, err := uuid.Parse(provider.Status.ProviderID)
	if err != nil || id == uuid.Nil || id.String() != provider.Status.ProviderID {
		return fmt.Errorf("assertion failed: AI provider %s/%s has status.providerID %q, which is not a canonical UUID", provider.Namespace, provider.Name, provider.Status.ProviderID)
	}
	return nil
}

// deleteStep handles an object that is being deleted. It deletes in Coder
// only a provider that this object proved it created (plan S7), and it
// returns release when the finalizer can go. Otherwise the step says why
// the finalizer stays.
func (r *CoderAIProviderReconciler) deleteStep(ctx context.Context, provider *coderv1alpha1.CoderAIProvider) (release bool, step *aiProviderStep, err error) {
	if provider.Status.ProviderID == "" && provider.Status.CreateAttemptTime == nil {
		// Nothing of this object can exist in Coder.
		return true, nil, nil
	}
	ref := types.NamespacedName{Namespace: provider.Namespace, Name: provider.Spec.ControlPlaneRef.Name}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	if err := r.Get(ctx, ref, controlPlane); apierrors.IsNotFound(err) {
		return r.releaseWithoutDelete(ctx, provider, "CoderControlPlane is gone"), nil, nil
	} else if err != nil {
		return false, nil, err
	}
	deleting := !controlPlane.DeletionTimestamp.IsZero()
	sdk, _, wait, err := controlPlaneCoderClientFor(ctx, r.Client, ref.Namespace, ref.Name, deleting)
	switch {
	case err != nil && deleting:
		// One attempt only, also when the access setup fails (plan D12).
		return r.releaseWithoutDelete(ctx, provider, "the Coder access of CoderControlPlane, which is being deleted, failed: "+err.Error()), nil, nil
	case err != nil:
		return false, nil, err
	case wait != nil && deleting:
		return r.releaseWithoutDelete(ctx, provider, "CoderControlPlane is being deleted without operator access"), nil, nil
	case wait != nil:
		return false, aiProviderWait(aiProviderDeletePoll, wait.reason, "%s The controller deletes provider %s once it can call Coder.", wait.message, provider.Name), nil
	}
	if deleting {
		// One bounded attempt, then release, so the namespace deletion
		// finishes (plan D12).
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, aiProviderBoundedDeleteTimeout)
		defer cancel()
	}
	if step, err = r.deleteInCoder(ctx, sdk, provider); err != nil && deleting {
		// The attempt ended with an impossible answer. Log it, and still
		// release: the one attempt is over.
		log.FromContext(ctx).Error(err, "The one delete attempt while CoderControlPlane is being deleted got an impossible answer")
		return r.releaseWithoutDelete(ctx, provider, "the one delete attempt while CoderControlPlane is being deleted failed: "+err.Error()), nil, nil
	} else if err != nil {
		return false, nil, err
	}
	if step != nil && deleting {
		return r.releaseWithoutDelete(ctx, provider, "the one delete attempt while CoderControlPlane is being deleted failed: "+step.message), nil, nil
	}
	return step == nil, step, nil
}

// deleteInCoder deletes the provider of this object in Coder and returns nil
// when Coder holds none. With only a create marker, a read decides first:
// only the nonce and the type prove that the provider is this object's.
func (r *CoderAIProviderReconciler) deleteInCoder(ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider) (*aiProviderStep, error) {
	id := provider.Status.ProviderID
	if id != "" {
		if err := checkAIProviderID(provider); err != nil {
			return nil, err
		}
	} else {
		found, err := sdk.AIProvider(ctx, provider.Name)
		switch {
		case isCoderResourceNotFound(err):
			// A 404 proves absence only for a site owner. A role change in
			// Coder has no watch, so the wait uses the delete poll.
			if step := requireSiteOwner(ctx, sdk); step != nil {
				step.requeue = aiProviderDeletePoll
				return step, nil
			}
			if wait := aiProviderCreateSettle - time.Since(provider.Status.CreateAttemptTime.Time); wait > 0 {
				// A create sent moments ago can still commit after this read.
				return aiProviderWait(wait, "ConfirmingCreate",
					"Provider %s is not in Coder, but its create request was sent at %s and can still commit. The controller reads again before it releases the finalizer.",
					provider.Name, provider.Status.CreateAttemptTime.UTC().Format(time.RFC3339)), nil
			}
			return nil, nil
		case err != nil:
			return aiProviderWait(aiProviderDeletePoll, "CoderUnavailable", "get provider %s: %s", provider.Name, coderErrorSummary(err)), nil
		}
		if err := coderAnswerFor("AI provider", provider.Name, found.Name); err != nil {
			log.FromContext(ctx).Error(err, "Coder answered something other than the controller asked for")
			return aiProviderWait(aiProviderDeletePoll, "CoderAnswerMismatch", "%s", err.Error()), nil
		}
		if found.DisplayName != aiProviderNonce(provider) || string(found.Type) != provider.Spec.Type {
			// Someone else's provider holds the name, so no create of this
			// object committed. The controller never deletes it.
			return nil, nil
		}
		if found.ID == uuid.Nil {
			return nil, fmt.Errorf("assertion failed: Coder answered AI provider %s without an ID", provider.Name)
		}
		id = found.ID.String()
	}
	if err := sdk.DeleteAIProvider(ctx, id); err != nil && !isCoderResourceNotFound(err) {
		return aiProviderWait(aiProviderDeletePoll, "CoderUnavailable", "delete provider %s (ID %s): %s", provider.Name, id, coderErrorSummary(err)), nil
	}
	return nil, nil
}

// releaseWithoutDelete logs what can stay in Coder, because nothing can
// delete it any more, and returns true (ControlPlaneGone).
func (r *CoderAIProviderReconciler) releaseWithoutDelete(ctx context.Context, provider *coderv1alpha1.CoderAIProvider, why string) bool {
	log.FromContext(ctx).Info("Releasing the finalizer without a delete in Coder: the provider can stay in the Coder database",
		"reason", "ControlPlaneGone", "why", why, "provider", provider.Name, "providerID", provider.Status.ProviderID,
		"createPending", provider.Status.CreateAttemptTime != nil)
	return true
}
