package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// syncAIProvider reads the provider that this object created and sends one
// sparse PATCH with every field that differs from the spec. The base URL and
// the key always travel in the same PATCH, so Coder never pairs a key with a
// URL that its Secret does not allow, and never enables a provider at a stale
// URL. After a full apply, a dashboard delete, disable or key change is a
// kill switch: the controller reports it and waits for a spec change (D11).
//
// recreate is true when the provider is gone before a full apply of this
// generation: the caller then creates it again, if the name is free.
func (r *CoderAIProviderReconciler) syncAIProvider(
	ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider, key credentialsecret.Value, before *coderv1alpha1.CoderAIProviderStatus,
) (step *aiProviderStep, recreate bool, err error) {
	cred := provider.Status.Credential
	if cred == nil {
		return nil, false, fmt.Errorf("assertion failed: AI provider %s/%s has a provider ID without a credential", provider.Namespace, provider.Name)
	}
	if err := checkAIProviderID(provider); err != nil {
		return nil, false, err
	}
	applied := provider.Status.AppliedGeneration == provider.Generation
	found, err := sdk.AIProvider(ctx, provider.Status.ProviderID)
	switch {
	case isCoderNotFound(err):
		return r.providerGone(provider, applied)
	case err != nil:
		return aiProviderWait(aiProviderRetry, "CoderUnavailable", "get provider %s: %s", provider.Name, coderErrorSummary(err)), false, nil
	}
	if step := checkAIProviderAnswer(ctx, provider, found); step != nil {
		return step, false, nil
	}

	enabled := provider.Spec.Enabled == nil || *provider.Spec.Enabled
	keys := found.APIKeys
	ownKey := len(keys) == 1 && keys[0].ID.String() == cred.KeyID
	resending := cred.ResendAttemptTime != nil
	// A delete, disable or key change in Coder is a kill switch also while a
	// key resend is pending: a Secret rotation must not undo a revocation.
	// Coder answers keys masked, so nothing proves that an unknown key is a
	// resend that committed without an answer. Such a key stalls too.
	switch {
	case applied && enabled && !found.Enabled:
		return aiProviderStalled(aiProviderResync, "DisabledExternally",
			"Provider %s was disabled in Coder. The controller does not enable it again until the spec changes.", provider.Name), false, nil
	case applied && !ownKey && resending:
		return aiProviderStalled(aiProviderResync, "KeyChangedExternally",
			"The key of provider %s in Coder is not the recorded key: it was changed in Coder, or a key resend committed without an answer. The controller does not send its key again until the spec changes.", provider.Name), false, nil
	case applied && !ownKey:
		return aiProviderStalled(aiProviderResync, "KeyChangedExternally",
			"The key of provider %s was changed in Coder. The controller does not send its key again until the spec changes.", provider.Name), false, nil
	}

	// A spec edit re-arms the object, so it also replaces a key that Coder
	// holds but this object did not send.
	sendKey := resending || !ownKey
	if !sendKey && !sameAIProviderKey(cred, key.Token) {
		if r.sentKeyMatches(provider, key) {
			// Only the Secret's metadata changed: Coder holds this value.
			setAIProviderToken(cred, key.Token)
		} else {
			sendKey = true
		}
	}
	req := codersdk.UpdateAIProviderRequest{}
	if found.DisplayName != cmp.Or(provider.Spec.DisplayName, provider.Name) {
		req.DisplayName = &provider.Spec.DisplayName
	}
	if found.Icon != provider.Spec.Icon {
		req.Icon = &provider.Spec.Icon
	}
	if found.Enabled != enabled {
		req.Enabled = &enabled
	}
	if found.BaseURL != provider.Spec.BaseURL {
		req.BaseURL = &provider.Spec.BaseURL
	}
	if sendKey {
		value := key.Reveal()
		req.APIKeys = &[]codersdk.AIProviderKeyMutation{{APIKey: &value}}
	}
	if req.IsEmpty() {
		provider.Status.AppliedGeneration = provider.Generation
		return aiProviderReady(provider), false, nil
	}
	if sendKey && !resending {
		// The marker goes first. A crash after the PATCH leaves it set, and
		// the next reconcile sends the key once more (plan 5.3).
		cred.ResendAttemptTime = &metav1.Time{Time: time.Now()}
		if err := r.writeStatus(ctx, provider, before); err != nil {
			return nil, false, err
		}
		*before = *provider.Status.DeepCopy()
		// The status update decodes the stored object into provider.
		cred = provider.Status.Credential
	}

	updated, err := sdk.UpdateAIProvider(ctx, provider.Status.ProviderID, req)
	status := coderStatus(err)
	switch {
	case status == http.StatusNotFound:
		// A proxy can put a 404 in place of a committed PATCH, which can
		// have replaced the nonce. Only a read proves that the provider is
		// gone, so ownership is kept until then.
		if _, readErr := sdk.AIProvider(ctx, provider.Status.ProviderID); readErr == nil {
			return aiProviderWait(aiProviderRetry, "CoderAnswerMismatch", "Coder answered 404 to the update of provider %s, but the provider exists.", provider.Name), false, nil
		} else if !isCoderNotFound(readErr) {
			return aiProviderWait(aiProviderRetry, "CoderUnavailable", "get provider %s after a 404 update: %s", provider.Name, coderErrorSummary(readErr)), false, nil
		}
		return r.providerGone(provider, applied)
	case isCoderAuthFailure(status) || status == http.StatusTooManyRequests:
		// No Kubernetes event follows a repaired token or role, so retry.
		return aiProviderWait(aiProviderRetry, "CoderUnavailable", "update provider %s: %s", provider.Name, coderErrorSummary(err)), false, nil
	case status >= 400 && status < 500:
		return aiProviderStalled(0, "CoderRejected", "Coder rejected the update: %s", coderErrorSummary(err)), false, nil
	case err != nil:
		return aiProviderWait(aiProviderRetry, "CoderUnavailable", "update provider %s: %s", provider.Name, coderErrorSummary(err)), false, nil
	}
	if step := checkAIProviderAnswer(ctx, provider, updated); step != nil {
		return step, false, nil
	}
	if updated.DisplayName != cmp.Or(provider.Spec.DisplayName, provider.Name) || updated.Icon != provider.Spec.Icon ||
		updated.Enabled != enabled || updated.BaseURL != provider.Spec.BaseURL {
		return aiProviderStalled(aiProviderResync, "CoderAnswerMismatch",
			"Coder answered the update of provider %s without the requested display name, icon, enabled flag or base URL.", provider.Name), false, nil
	}
	if sendKey {
		if len(updated.APIKeys) != 1 || updated.APIKeys[0].ID == uuid.Nil || updated.APIKeys[0].ID.String() == cred.KeyID {
			// The marker stays, so the next reconcile sends the key again.
			return aiProviderWait(aiProviderRetry, "CoderAnswerMismatch",
				"Coder answered the key update of provider %s without exactly one new key. The controller sends the key again.", provider.Name), false, nil
		}
		cred.KeyID, cred.ResendAttemptTime = updated.APIKeys[0].ID.String(), nil
		setAIProviderToken(cred, key.Token)
		r.rememberSentKey(provider, key)
	} else if len(updated.APIKeys) != 1 || updated.APIKeys[0].ID.String() != cred.KeyID {
		// The key changed in Coder between the read and this PATCH. The next
		// reconcile decides: a kill switch, or a resend after a spec edit.
		return aiProviderWait(aiProviderConfirmPoll, "KeyChangedDuringUpdate",
			"The key of provider %s changed in Coder during the update. The controller reads the provider again.", provider.Name), false, nil
	}
	provider.Status.AppliedGeneration = provider.Generation
	return aiProviderReady(provider), false, nil
}

// providerGone handles a provider that is missing in Coder. After a full
// apply it is a kill switch. Before one, the object creates it again.
func (r *CoderAIProviderReconciler) providerGone(provider *coderv1alpha1.CoderAIProvider, applied bool) (*aiProviderStep, bool, error) {
	if applied {
		return aiProviderStalled(aiProviderResync, "DeletedExternally",
			"Provider %s (ID %s) was deleted in Coder. The controller does not create it again until the spec changes.", provider.Name, provider.Status.ProviderID), false, nil
	}
	provider.Status.ProviderID, provider.Status.Credential = "", nil
	r.sentKeys.Delete(provider.UID)
	return nil, true, nil
}

// checkAIProviderAnswer checks that Coder answered for the provider that
// this object created, with the type it cannot change.
// A mismatch is an assertion failure. As in CoderTemplateTest
// (waitOnWrongAnswer), it is logged and reported, not returned: a returned
// error retries after a short backoff and sends more requests to a Coder that
// answers wrongly.
func checkAIProviderAnswer(ctx context.Context, provider *coderv1alpha1.CoderAIProvider, answer codersdk.AIProvider) *aiProviderStep {
	if err := errors.Join(coderAnswerFor("AI provider", provider.Status.ProviderID, answer.ID.String()),
		coderAnswerFor("AI provider", provider.Name, answer.Name),
		coderAnswerFor("AI provider type", provider.Spec.Type, string(answer.Type))); err != nil {
		log.FromContext(ctx).Error(err, "Coder answered something other than the controller asked for")
		return aiProviderStalled(aiProviderResync, "CoderAnswerMismatch", "%s", err.Error())
	}
	return nil
}

func setAIProviderToken(cred *coderv1alpha1.CoderAIProviderCredentialStatus, token credentialsecret.ChangeToken) {
	cred.SecretName, cred.SecretKey, cred.SecretUID, cred.SecretResourceVersion = token.SecretName, token.SecretKey, string(token.SecretUID), token.ResourceVersion
}

// rememberSentKey keeps a SHA-256 of the value that Coder now holds, in
// process memory only (plan 5.3, D1). It lets a metadata-only Secret change
// skip a resend. After a restart the map is empty, so one resend can happen.
func (r *CoderAIProviderReconciler) rememberSentKey(provider *coderv1alpha1.CoderAIProvider, key credentialsecret.Value) {
	r.sentKeys.Store(provider.UID, sha256.Sum256([]byte(key.Reveal())))
}

func (r *CoderAIProviderReconciler) sentKeyMatches(provider *coderv1alpha1.CoderAIProvider, key credentialsecret.Value) bool {
	sum, ok := r.sentKeys.Load(provider.UID)
	return ok && sum.([sha256.Size]byte) == sha256.Sum256([]byte(key.Reveal()))
}
