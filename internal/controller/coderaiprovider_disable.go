package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/coder/v2/codersdk"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// disableWithoutConsent handles a provider that this object created when its
// key Secret fails the credential contract (#267, #290). Coder keeps the key
// and sends it to the base URL that Coder holds. Unless the recorded Secret
// still passes the contract for that base URL, the controller disables the
// provider in Coder with one PATCH that holds no key and no URL.
//
// It fails closed on every credentialsecret.Error: a removed origin or
// opt-in, a deleted or refused Secret, a malformed annotation, and also a
// missing or malformed key value. The Secret then no longer holds proof of
// consent for the key that Coder uses, and an owner who deletes or empties
// the key expects Coder to stop using it.
//
// The disable is a kill switch like a dashboard disable: appliedGeneration
// stays, so a later sync reports DisabledExternally and only a spec edit
// enables the provider again. A re-arm would also undo a key change in the
// dashboard.
func (r *CoderAIProviderReconciler) disableWithoutConsent(
	ctx context.Context, provider *coderv1alpha1.CoderAIProvider, credErr *credentialsecret.Error,
) (*aiProviderStep, error) {
	if provider.Status.ProviderID == "" || credErr == nil {
		return nil, fmt.Errorf("assertion failed: AI provider %s/%s needs a provider ID and a credential failure to be disabled", provider.Namespace, provider.Name)
	}
	if err := checkAIProviderID(provider); err != nil {
		return nil, err
	}
	// Status messages are cut at 256 characters, so the new fact goes first.
	// No Kubernetes event follows a dashboard enable, so the stall resyncs.
	stalled := func(format string, args ...any) *aiProviderStep {
		return aiProviderStalled(aiProviderResync, credErr.Reason, "%s %s", fmt.Sprintf(format, args...), credErr.Message)
	}
	unchecked := func(format string, args ...any) *aiProviderStep {
		return aiProviderWait(aiProviderRetry, credErr.Reason, "Coder can still send the key of provider %s (%s). %s", provider.Name, fmt.Sprintf(format, args...), credErr.Message)
	}
	sdk, _, wait, err := controlPlaneCoderClient(ctx, r.Client, provider.Namespace, provider.Spec.ControlPlaneRef.Name)
	if err != nil {
		return nil, err
	}
	if wait != nil {
		return unchecked("the controller cannot reach Coder: %s", wait.message), nil
	}
	found, err := sdk.AIProvider(ctx, provider.Status.ProviderID)
	switch {
	case isCoderNotFound(err):
		return stalled("Provider %s does not exist in Coder.", provider.Name), nil
	case err != nil:
		return unchecked("get provider: %s", coderErrorSummary(err)), nil
	}
	if step := checkAIProviderAnswer(ctx, provider, found); step != nil {
		return step, nil
	}
	if !found.Enabled {
		return stalled("Provider %s is disabled in Coder.", provider.Name), nil
	}
	// The spec can name a new origin while Coder still uses an allowed one.
	// Coder holds the key of the recorded Secret, so only that Secret (same
	// UID) passing the contract for Coder's base URL keeps the provider. Any
	// other result disables it: a credential failure (it never clears by
	// waiting), another Secret, or an unparsable Coder origin.
	cred := provider.Status.Credential
	if coderURL, parseErr := aiProviderBaseURL(found.BaseURL); parseErr == nil && cred != nil {
		value, readErr := r.readKey(ctx, provider, coderURL)
		if readErr == nil && string(value.Token.SecretUID) == cred.SecretUID {
			return stalled("Coder still sends the key of provider %s to an origin that the Secret allows.", provider.Name), nil
		}
		if coderCredErr := (*credentialsecret.Error)(nil); readErr != nil && !errors.As(readErr, &coderCredErr) {
			return nil, readErr
		}
	}

	disabled := false
	updated, err := sdk.UpdateAIProvider(ctx, provider.Status.ProviderID, codersdk.UpdateAIProviderRequest{Enabled: &disabled})
	if err != nil {
		return unchecked("the disable failed: %s", coderErrorSummary(err)), nil
	}
	if step := checkAIProviderAnswer(ctx, provider, updated); step != nil {
		return step, nil
	}
	if updated.Enabled {
		return aiProviderWait(aiProviderRetry, "CoderAnswerMismatch",
			"Coder answered the disable of provider %s with an enabled provider. The controller disables it again.", provider.Name), nil
	}
	// The reason stays the credential reason, so it does not flap between
	// this reconcile and the next one, which finds the provider disabled.
	return stalled("The controller disabled provider %s in Coder: the key Secret no longer proves consent for the key that Coder uses. After the Secret is fixed, a spec edit enables the provider.", provider.Name), nil
}
