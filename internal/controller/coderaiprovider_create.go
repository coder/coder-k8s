package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	found, err := sdk.AIProvider(ctx, provider.Name)
	switch {
	case isCoderNotFound(err):
		if provider.Status.CreateAttemptTime != nil {
			if step := requireSiteOwner(ctx, sdk); step != nil {
				return step, nil
			}
		}
		return r.createAIProvider(ctx, sdk, provider, key, before)
	case err != nil:
		return aiProviderWait(aiProviderRetry, "CoderUnavailable", "get provider %s: %s", provider.Name, coderErrorSummary(err)), nil
	}
	return recordAIProvider(provider, found)
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
func recordAIProvider(provider *coderv1alpha1.CoderAIProvider, found codersdk.AIProvider) (*aiProviderStep, error) {
	if err := coderAnswerFor("AI provider", provider.Name, found.Name); err != nil {
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
	if found.ID == uuid.Nil || provider.Status.Credential == nil {
		return nil, fmt.Errorf("assertion failed: AI provider %s/%s has a create marker without a credential, or Coder answered without an ID", provider.Namespace, provider.Name)
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
		if step, err := recordAIProvider(provider, created); step != nil || err != nil {
			// A wrong answer leaves the marker, so a read decides.
			if step != nil && step.reason != "KeyChangedExternally" {
				provider.Status.CreateAttemptTime, provider.Status.Credential = before.CreateAttemptTime, before.Credential
				step = aiProviderWait(aiProviderConfirmPoll, "ConfirmingCreate", "The create answer does not match the request (%s). The controller reads Coder instead of sending it again.", step.message)
			}
			return step, err
		}
		return nil, nil
	case createHadNoEffect(err):
		provider.Status.CreateAttemptTime, provider.Status.Credential = nil, nil
		return aiProviderWait(aiProviderRetry, "CreateRetrying", "The create request had no effect: %s", coderErrorSummary(err)), nil
	case status == http.StatusConflict:
		// Someone created the name after the lookup. Read it: the nonce
		// proves whether an earlier create of this object committed.
		found, err := sdk.AIProvider(ctx, provider.Name)
		if err != nil {
			return aiProviderWait(aiProviderConfirmPoll, "ConfirmingCreate", "get provider %s after a conflict: %s", provider.Name, coderErrorSummary(err)), nil
		}
		return recordAIProvider(provider, found)
	case status >= 400 && status < 500:
		provider.Status.CreateAttemptTime, provider.Status.Credential = nil, nil
		return aiProviderStalled(0, "CoderRejected", "Coder rejected the create request: %s", coderErrorSummary(err)), nil
	default:
		return aiProviderWait(aiProviderConfirmPoll, "ConfirmingCreate",
			"The create result is uncertain (%s). The controller reads Coder instead of sending it again.", coderErrorSummary(err)), nil
	}
}

// applyAIProvider sets the display name, icon and enabled flag of the
// provider that this object created, once per generation. A changed base URL
// or key needs a later controller version, so it does not count as applied.
func applyAIProvider(ctx context.Context, sdk *codersdk.Client, provider *coderv1alpha1.CoderAIProvider, key credentialsecret.Value) *aiProviderStep {
	if provider.Status.AppliedGeneration == provider.Generation {
		return aiProviderReady(provider)
	}
	if !sameAIProviderKey(provider.Status.Credential, key.Token) {
		return aiProviderWait(aiProviderResync, "Pending", "The key Secret changed after the create. This controller version does not send a changed key.")
	}
	enabled := provider.Spec.Enabled == nil || *provider.Spec.Enabled
	updated, err := sdk.UpdateAIProvider(ctx, provider.Status.ProviderID, codersdk.UpdateAIProviderRequest{
		DisplayName: &provider.Spec.DisplayName, Icon: &provider.Spec.Icon, Enabled: &enabled,
	})
	status := coderStatus(err)
	switch {
	case status == http.StatusNotFound:
		return aiProviderStalled(0, "DeletedExternally", "Provider %s (ID %s) is gone from Coder. The controller does not create it again until the spec changes.", provider.Name, provider.Status.ProviderID)
	case status >= 400 && status < 500 && status != http.StatusTooManyRequests:
		return aiProviderStalled(0, "CoderRejected", "Coder rejected the update: %s", coderErrorSummary(err))
	case err != nil:
		return aiProviderWait(aiProviderRetry, "CoderUnavailable", "update provider %s: %s", provider.Name, coderErrorSummary(err))
	}
	if err := errors.Join(coderAnswerFor("AI provider", provider.Status.ProviderID, updated.ID.String()), coderAnswerFor("AI provider", provider.Name, updated.Name)); err != nil {
		return aiProviderStalled(aiProviderResync, "CoderAnswerMismatch", "%s", err.Error())
	}
	if updated.BaseURL != provider.Spec.BaseURL || string(updated.Type) != provider.Spec.Type {
		return aiProviderWait(aiProviderResync, "Pending", "The base URL changed after the create. This controller version does not send a changed base URL.")
	}
	provider.Status.AppliedGeneration = provider.Generation
	return aiProviderReady(provider)
}

func sameAIProviderKey(recorded *coderv1alpha1.CoderAIProviderCredentialStatus, token credentialsecret.ChangeToken) bool {
	return recorded != nil && recorded.SecretName == token.SecretName && recorded.SecretKey == token.SecretKey &&
		recorded.SecretUID == string(token.SecretUID) && recorded.SecretResourceVersion == token.ResourceVersion
}

func aiProviderReady(provider *coderv1alpha1.CoderAIProvider) *aiProviderStep {
	return &aiProviderStep{ready: true, reason: "Applied", message: fmt.Sprintf("Coder provider %s (ID %s) matches the spec.", provider.Name, provider.Status.ProviderID), requeue: aiProviderResync}
}
