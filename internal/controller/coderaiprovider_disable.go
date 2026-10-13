package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/coder/v2/codersdk"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// aiProviderConsentWithdrawn reports the credential failures that say the key
// Secret no longer allows an origin: the origin left the destinations
// annotation, or the Secret lost its opt-in label or annotation.
func aiProviderConsentWithdrawn(credErr *credentialsecret.Error) bool {
	return credErr.Reason == credentialsecret.ReasonDestinationNotAllowed || credErr.Reason == credentialsecret.ReasonNotOptedIn
}

// disableWithoutConsent handles a provider that this object created when its
// key Secret no longer allows the origin of spec.baseURL (#267). Coder keeps
// the key and sends it to the base URL that Coder holds. If the Secret does
// not allow that origin either, the controller disables the provider in
// Coder with one PATCH that holds no key and no URL.
//
// The disable is a kill switch like a dashboard disable: appliedGeneration
// stays, so a later sync reports DisabledExternally and only a spec edit
// enables the provider again. A re-arm would also undo a key change in the
// dashboard.
func (r *CoderAIProviderReconciler) disableWithoutConsent(
	ctx context.Context, provider *coderv1alpha1.CoderAIProvider, credErr *credentialsecret.Error,
) (*aiProviderStep, error) {
	if provider.Status.ProviderID == "" || !aiProviderConsentWithdrawn(credErr) {
		return nil, fmt.Errorf("assertion failed: AI provider %s/%s needs a provider ID and a withdrawn consent to be disabled", provider.Namespace, provider.Name)
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
	if step := checkAIProviderAnswer(provider, found); step != nil {
		return step, nil
	}
	if !found.Enabled {
		return stalled("Provider %s is disabled in Coder.", provider.Name), nil
	}
	// The spec can name a new origin while Coder still uses an allowed one.
	// Coder holds the key of the recorded Secret, so only that Secret (same
	// UID) can prove it. A refused or unparsable Coder origin, or another
	// Secret, disables the provider.
	cred := provider.Status.Credential
	if coderURL, parseErr := aiProviderBaseURL(found.BaseURL); parseErr == nil && cred != nil {
		value, readErr := credentialsecret.Read(ctx, r.Client, provider.Namespace, provider.Spec.APIKeySecretRef, coderURL)
		var coderCredErr *credentialsecret.Error
		switch {
		case readErr == nil && string(value.Token.SecretUID) == cred.SecretUID:
			return stalled("Coder still sends the key of provider %s to an origin that the Secret allows.", provider.Name), nil
		case readErr == nil:
			// Another Secret, or one recreated under the same name, did
			// not consent for the key that Coder holds.
		case !errors.As(readErr, &coderCredErr):
			return nil, readErr
		case !aiProviderConsentWithdrawn(coderCredErr):
			// The Secret changed between the two reads. The next reconcile
			// reads it again.
			return aiProviderWait(aiProviderRetry, coderCredErr.Reason, "%s", coderCredErr.Message), nil
		}
	}

	disabled := false
	updated, err := sdk.UpdateAIProvider(ctx, provider.Status.ProviderID, codersdk.UpdateAIProviderRequest{Enabled: &disabled})
	if err != nil {
		return unchecked("the disable failed: %s", coderErrorSummary(err)), nil
	}
	if step := checkAIProviderAnswer(provider, updated); step != nil {
		return step, nil
	}
	if updated.Enabled {
		return aiProviderWait(aiProviderRetry, "CoderAnswerMismatch",
			"Coder answered the disable of provider %s with an enabled provider. The controller disables it again.", provider.Name), nil
	}
	// The reason stays the credential reason, so it does not flap between
	// this reconcile and the next one, which finds the provider disabled.
	return stalled("The controller disabled provider %s in Coder: the Secret does not allow the origin that Coder uses. After the Secret allows it again, a spec edit enables the provider.", provider.Name), nil
}
