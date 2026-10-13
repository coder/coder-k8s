package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

const (
	// aiProviderNoncePrefix starts the display name of a provider that the
	// controller creates. With the object UID it proves that this object
	// created the provider, because Coder stores no owner.
	aiProviderNoncePrefix = "coder-k8s:"
	// aiProviderConfirmPoll is the poll while a create result is uncertain.
	aiProviderConfirmPoll = 5 * time.Second
)

func aiProviderNonce(provider *coderv1alpha1.CoderAIProvider) string {
	return aiProviderNoncePrefix + string(provider.UID)
}

// createOrConfirm handles a provider without a Coder ID. Without a create
// marker it looks up the name and creates the provider when the name is free.
// With a marker, a create can be in flight or committed, so it reads Coder
// first and never sends the create again before that read.
func (r *CoderAIProviderReconciler) createOrConfirm(
	ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider, key credentialsecret.Value, before *coderv1alpha1.CoderAIProviderStatus,
) (*aiProviderStep, error) {
	if stalled := meta.FindStatusCondition(provider.Status.Conditions, coderv1alpha1.CoderAIProviderConditionStalled); provider.Status.CreateAttemptTime == nil &&
		stalled != nil && stalled.Reason == "CoderRejected" && stalled.ObservedGeneration == provider.Generation {
		// Coder rejected this spec. Only a spec change retries.
		return aiProviderStalled(0, stalled.Reason, "%s", stalled.Message), nil
	}
	ready := meta.FindStatusCondition(provider.Status.Conditions, coderv1alpha1.CoderAIProviderConditionReady)
	rejected := provider.Status.CreateAttemptTime != nil && ready != nil &&
		ready.Reason == aiProviderConfirmingRejectedCreate && ready.ObservedGeneration == provider.Generation
	found, err := sdk.AIProvider(ctx, provider.Name)
	switch {
	case isCoderNotFound(err):
		if provider.Status.CreateAttemptTime != nil {
			if step := requireSiteOwner(ctx, sdk); step != nil {
				return keepRejectedCreate(step, rejected), nil
			}
			if rejected {
				// The last create of this spec was rejected, and now a read
				// confirms that it created nothing. Do not send it again.
				provider.Status.CreateAttemptTime, provider.Status.Credential = nil, nil
				return aiProviderStalled(0, "CoderRejected", "Coder rejected the create request of provider %s, and a read confirmed that nothing was created.", provider.Name), nil
			}
		}
		return r.createAIProvider(ctx, sdk, provider, key, before)
	case err != nil:
		return keepRejectedCreate(aiProviderWait(aiProviderRetry, "CoderUnavailable", "get provider %s: %s", provider.Name, coderErrorSummary(err)), rejected), nil
	}
	return recordAIProvider(ctx, provider, found)
}

// keepRejectedCreate keeps the memory of a rejected create in the reason of a
// wait, so the create is not sent again once the wait ends (#268).
func keepRejectedCreate(step *aiProviderStep, rejected bool) *aiProviderStep {
	if rejected {
		step.reason = aiProviderConfirmingRejectedCreate
	}
	return step
}

// requireSiteOwner returns a wait unless the operator user is a site owner.
// Coder v2.37.3 answers 403 to anyone else on the provider routes
// (coderd/ai_providers.go:640-643), but a 404 proves absence only for a
// caller that can read every provider, so the controller checks it.
func requireSiteOwner(ctx context.Context, sdk *codersdk.Client) *aiProviderStep {
	operator, err := sdk.User(ctx, codersdk.Me)
	if err != nil {
		return aiProviderWait(aiProviderRetry, "CoderUnavailable", "get operator user: %s", coderErrorSummary(err))
	}
	if !slices.ContainsFunc(operator.Roles, func(role codersdk.SlimRole) bool { return role.Name == codersdk.RoleOwner && role.OrganizationID == "" }) {
		return aiProviderWait(aiProviderResync, "ConfirmingCreate", "The operator user is not a site owner, so a missing provider proves nothing.")
	}
	return nil
}

// recordAIProvider decides whether found is the provider that this object
// created. Only the nonce and the type prove it. Anything else is a provider
// that the controller never touches (no adoption).
func recordAIProvider(ctx context.Context, provider *coderv1alpha1.CoderAIProvider, found codersdk.AIProvider) (*aiProviderStep, error) {
	if err := coderAnswerFor("AI provider", provider.Name, found.Name); err != nil {
		log.FromContext(ctx).Error(err, "Coder answered something other than the controller asked for")
		return aiProviderStalled(aiProviderResync, "CoderAnswerMismatch", "%s", err.Error()), nil
	}
	if found.DisplayName != aiProviderNonce(provider) || string(found.Type) != provider.Spec.Type {
		// No create of this object can commit while another provider holds
		// the name, so nothing of this object can exist in Coder.
		provider.Status.CreateAttemptTime, provider.Status.Credential = nil, nil
		return aiProviderStalled(aiProviderResync, "NameConflict",
			"Coder already has a provider named %s (ID %s) that this resource did not create. The controller never changes it. Choose another name, or delete that provider in Coder.",
			provider.Name, found.ID), nil
	}
	if found.ID == uuid.Nil {
		return nil, fmt.Errorf("assertion failed: Coder answered AI provider %s without an ID", provider.Name)
	}
	if provider.Status.Credential == nil {
		// A create of this object committed after its marker was cleared, so
		// the key it sent is not known. An empty change token makes the sync
		// step send the current key.
		provider.Status.Credential = &coderv1alpha1.CoderAIProviderCredentialStatus{}
	}
	if len(found.APIKeys) != 1 || found.APIKeys[0].ID == uuid.Nil {
		return aiProviderStalled(0, "KeyChangedExternally",
			"Provider %s has %d keys in Coder, but this resource created it with one. The controller does not change it until the spec changes.", provider.Name, len(found.APIKeys)), nil
	}
	provider.Status.ProviderID, provider.Status.Credential.KeyID = found.ID.String(), found.APIKeys[0].ID.String()
	provider.Status.CreateAttemptTime = nil
	return nil, nil
}

// createAIProvider writes the create marker and the change token of the key,
// then sends the only create request of this attempt. A conflict on the
// marker write ends the reconcile before any request. The provider starts
// disabled with the nonce as its display name. The apply step then sets the
// spec values.
func (r *CoderAIProviderReconciler) createAIProvider(
	ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider, key credentialsecret.Value, before *coderv1alpha1.CoderAIProviderStatus,
) (*aiProviderStep, error) {
	token := key.Token
	provider.Status.CreateAttemptTime = &metav1.Time{Time: time.Now()}
	provider.Status.Credential = &coderv1alpha1.CoderAIProviderCredentialStatus{
		SecretName: token.SecretName, SecretKey: token.SecretKey, SecretUID: string(token.SecretUID), SecretResourceVersion: token.ResourceVersion,
	}
	applyAIProviderStep(provider, aiProviderWait(0, "CreatingProvider", "Creating provider %s in Coder.", provider.Name))
	if err := r.writeStatus(ctx, provider, before); err != nil {
		return nil, err
	}
	*before = *provider.Status.DeepCopy()

	created, err := sdk.CreateAIProvider(ctx, codersdk.CreateAIProviderRequest{
		Type: codersdk.AIProviderType(provider.Spec.Type), Name: provider.Name, DisplayName: aiProviderNonce(provider),
		Enabled: false, BaseURL: provider.Spec.BaseURL, APIKeys: []string{key.Reveal()},
	})
	status := coderStatus(err)
	switch {
	case err == nil:
		if step, err := recordAIProvider(ctx, provider, created); step != nil || err != nil {
			// A wrong answer leaves the marker, so a read decides.
			if step != nil {
				provider.Status.CreateAttemptTime, provider.Status.Credential = before.CreateAttemptTime, before.Credential
				step = aiProviderWait(aiProviderConfirmPoll, "ConfirmingCreate", "The create answer does not match the request (%s). The controller reads Coder instead of sending it again.", step.message)
			}
			return step, err
		}
		r.rememberSentKey(provider, key)
		return nil, nil
	case status == 0 && createHadNoEffect(err):
		// A failed dial or TLS handshake never sent the request.
		provider.Status.CreateAttemptTime, provider.Status.Credential = nil, nil
		return aiProviderWait(aiProviderRetry, "CreateRetrying", "The create request had no effect: %s", coderErrorSummary(err)), nil
	case status >= 400 && status < 500:
		return r.confirmRejectedCreate(ctx, sdk, provider, status, err)
	default:
		return aiProviderWait(aiProviderConfirmPoll, "ConfirmingCreate",
			"The create result is uncertain (%s). The controller reads Coder instead of sending it again.", coderErrorSummary(err)), nil
	}
}

// confirmRejectedCreate reads Coder after a 4xx answer to a create. Coder
// sends its 4xx answers before the insert commits, but a proxy can put a 4xx
// in place of a committed answer, so only a read proves that nothing of this
// object exists. Until then the marker stays (#253 review round 3).
func (r *CoderAIProviderReconciler) confirmRejectedCreate(
	ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider, status int, createErr error,
) (*aiProviderStep, error) {
	found, err := sdk.AIProvider(ctx, provider.Name)
	switch {
	case err == nil:
		// Ours (the create committed) or someone else's (NameConflict).
		return recordAIProvider(ctx, provider, found)
	case !isCoderNotFound(err):
		reason := "ConfirmingCreate"
		if !aiProviderCreateRetryable(status) {
			// Remember the rejection, so a later read that confirms absence
			// stalls instead of sending the rejected create again.
			reason = aiProviderConfirmingRejectedCreate
		}
		return aiProviderWait(aiProviderRetry, reason, "get provider %s after a %d answer: %s", provider.Name, status, coderErrorSummary(err)), nil
	}
	if step := requireSiteOwner(ctx, sdk); step != nil {
		return keepRejectedCreate(step, !aiProviderCreateRetryable(status)), nil
	}
	provider.Status.CreateAttemptTime, provider.Status.Credential = nil, nil
	if aiProviderCreateRetryable(status) {
		return aiProviderWait(aiProviderRetry, "CreateRetrying", "The create request had no effect: %s", coderErrorSummary(createErr)), nil
	}
	return aiProviderStalled(0, "CoderRejected", "Coder rejected the create request: %s", coderErrorSummary(createErr)), nil
}

// aiProviderConfirmingRejectedCreate is the wait reason after a create that
// Coder rejected, while the read that confirms absence fails.
const aiProviderConfirmingRejectedCreate = "ConfirmingRejectedCreate"

// aiProviderCreateRetryable reports 4xx create answers that a retry can fix
// without a spec change.
func aiProviderCreateRetryable(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return false
}

// isCoderAuthFailure reports 401 and 403. An expired operator token or a
// removed owner role can be repaired without any Kubernetes event.
func isCoderAuthFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func sameAIProviderKey(recorded *coderv1alpha1.CoderAIProviderCredentialStatus, token credentialsecret.ChangeToken) bool {
	return recorded != nil && recorded.SecretName == token.SecretName && recorded.SecretKey == token.SecretKey &&
		recorded.SecretUID == string(token.SecretUID) && recorded.SecretResourceVersion == token.ResourceVersion
}

func aiProviderReady(provider *coderv1alpha1.CoderAIProvider) *aiProviderStep {
	return &aiProviderStep{ready: true, reason: "Applied", message: fmt.Sprintf("Coder provider %s (ID %s) matches the spec.", provider.Name, provider.Status.ProviderID), requeue: aiProviderResync}
}
