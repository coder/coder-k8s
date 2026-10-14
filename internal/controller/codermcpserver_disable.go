package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// disableMCPServerWithoutConsent handles a config that this object created
// when a credential read fails (#311, plan G8). Coder keeps the stored
// credentials and sends them to the URLs that Coder holds. Unless every
// credential that Coder holds comes from a recorded Secret that still passes
// the contract for those URLs, the controller disables the config in Coder
// with one PATCH that holds no credential and no destination field.
//
// An observed auth type user_oidc is disabled too: the controller cannot set
// it back without the credentials (plan 4.7 item 4).
//
// It fails closed on every stalled credential failure, as CoderAIProvider
// does (#290): a deleted Secret, a removed opt-in label or origin, and also
// a missing or malformed value. The Secret then no longer proves consent for
// the credential that Coder uses.
//
// The disable is a kill switch like a dashboard disable: it sets
// appliedGeneration, so a later sync reports DisabledExternally and only a
// spec edit after the disable enables the config again.
func (r *CoderMCPServerReconciler) disableMCPServerWithoutConsent(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, before *coderv1alpha1.CoderMCPServerStatus, orgID uuid.UUID, failed *mcpServerStep,
) (*mcpServerStep, error) {
	id, err := uuid.Parse(server.Status.ConfigID)
	if err != nil || id == uuid.Nil || failed == nil || !failed.stalled {
		return nil, fmt.Errorf("assertion failed: MCP server %s/%s needs a config ID and a stalled credential failure to be disabled", server.Namespace, server.Name)
	}
	// Status messages are cut at 256 characters, so the new fact goes first.
	stalled := func(format string, args ...any) *mcpServerStep {
		return mcpServerStalled(mcpServerResync, failed.reason, "%s %s", fmt.Sprintf(format, args...), failed.message)
	}
	unchecked := func(format string, args ...any) *mcpServerStep {
		return mcpServerWait(mcpServerRetry, failed.reason, "Coder can still send the credentials of MCP server config %s (%s). %s", id, fmt.Sprintf(format, args...), failed.message)
	}
	found, step := readMCPServerToDisable(ctx, sdk, server, orgID, id, stalled, unchecked)
	if step != nil {
		return step, nil
	}
	// user_oidc sends each chat user's login token to url (plan F14). Without
	// a readable credential the controller cannot set the auth type back, so
	// it disables the config.
	ok := false
	if found.AuthType != "user_oidc" {
		ok, err = r.mcpServerConsentHolds(ctx, server, found)
		if errors.Is(err, errMCPServerSecretChanged) {
			return unchecked("a Secret changed during the consent check, so the controller reads it again"), nil
		} else if err != nil {
			return nil, err
		}
	}
	if ok {
		return stalled("Coder sends each credential of MCP server config %s only to origins that its recorded Secret allows.", id), nil
	}

	if step, err := r.sendMCPServerDisable(ctx, sdk, server, before, orgID, id, failed.reason, unchecked); step != nil || err != nil {
		return step, err
	}
	// The reason stays the credential reason, so it does not flap between
	// this reconcile and the next one, which finds the config disabled.
	return stalled("The controller disabled MCP server config %s in Coder: a credential Secret no longer proves consent for a credential that Coder uses. After the Secret is fixed, a spec edit enables the config.", id), nil
}

// readMCPServerToDisable reads the config of a fail-closed path. It returns
// a step when there is nothing to disable or the read failed: a config that
// is gone (only Coder's own 404 proves it, #337) or already disabled, an
// answer for another config, or an error.
func readMCPServerToDisable(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID, id uuid.UUID,
	stalled, unchecked func(format string, args ...any) *mcpServerStep,
) (codersdk.MCPServerConfig, *mcpServerStep) {
	found, err := sdk.MCPServerConfigByID(ctx, orgID, id)
	switch {
	case isCoderResourceNotFound(err):
		// The kill switch holds a gone config too: a pending generation must
		// not create an enabled config again once consent or the opt-in comes
		// back, before a spec edit.
		server.Status.AppliedGeneration = server.Generation
		return found, stalled("MCP server config %s does not exist in Coder.", id)
	case err != nil:
		return found, unchecked("get MCP server config: %s", coderErrorSummary(err))
	}
	if found.ID != id || found.OrganizationID != orgID || found.Slug != server.Spec.Slug {
		return found, mcpServerStalled(mcpServerResync, "CoderAnswerMismatch",
			"assertion failed: Coder answered MCP server config %s with slug %s in organization %s when asked for %s", found.ID, found.Slug, found.OrganizationID, id)
	}
	if !found.Enabled {
		// The disable answer can be lost after Coder committed it. Whoever
		// disabled the config, the kill switch now holds this generation too,
		// so an opt-in or consent that comes back does not apply a pending
		// generation that enables the config.
		server.Status.AppliedGeneration = server.Generation
		return found, stalled("MCP server config %s is disabled in Coder.", id)
	}
	return found, nil
}

// sendMCPServerDisable sends the one PATCH of a fail-closed path. It holds
// only enabled false: no credential and no destination field. It returns a
// wait step when the PATCH failed or Coder answered for another or an
// enabled config.
//
// The disable counts as an external disable of this generation, also of one
// that is not applied yet, so the DisabledExternally kill switch holds until
// the next spec edit. The controller writes appliedGeneration to status
// before the PATCH: Coder can commit the PATCH while its answer or the next
// status write is lost, and a pending generation must not enable the config
// once consent or the opt-in comes back. A failed status write sends no
// PATCH. Whether the config is disabled comes from Coder's answer, not from
// appliedGeneration, so a reconcile after a stop between the status write
// and the PATCH still sends the PATCH while consent or the opt-in stays
// withdrawn.
//
// Known limit (#365): if the process stops between the status write and the
// PATCH, and consent or the opt-in comes back before the next reconcile, the
// sync applies the current spec without a later spec edit. Coder never
// disabled the config in that window, and the sync checks every credential
// first, so no credential goes to an origin without consent. The same holds
// when the PATCH fails without a commit and consent or the opt-in comes back
// while the config is still enabled: the generation counts as applied, but
// the sync compares Coder with the spec on every reconcile, so it still
// sends a spec edit made before the disable.
func (r *CoderMCPServerReconciler) sendMCPServerDisable(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, before *coderv1alpha1.CoderMCPServerStatus,
	orgID, id uuid.UUID, reason string, unchecked func(format string, args ...any) *mcpServerStep,
) (*mcpServerStep, error) {
	if before == nil {
		return nil, fmt.Errorf("assertion failed: MCP server %s/%s needs the status before the reconcile to be disabled", server.Namespace, server.Name)
	}
	if server.Status.AppliedGeneration != server.Generation {
		server.Status.AppliedGeneration = server.Generation
		applyMCPServerStep(server, mcpServerWait(0, reason, "Disabling MCP server config %s in Coder.", id))
		if err := r.writeStatus(ctx, server, before); err != nil {
			return nil, err
		}
		*before = *server.Status.DeepCopy()
	}
	disabled := false
	updated, err := sdk.UpdateMCPServerConfig(ctx, orgID, id, codersdk.UpdateMCPServerConfigRequest{Enabled: &disabled})
	if err != nil {
		return unchecked("the disable failed: %s", coderErrorSummary(err)), nil
	}
	if updated.ID != id || updated.OrganizationID != orgID || updated.Slug != server.Spec.Slug || updated.Enabled {
		return mcpServerWait(mcpServerRetry, "CoderAnswerMismatch",
			"Coder answered the disable of MCP server config %s with another or an enabled config. The controller disables it again.", id), nil
	}
	return nil, nil
}

// disableMCPServerWithoutOptIn handles a config that this object created
// when the control plane withdraws its opt-in (#336): spec.mcpServers is
// removed, it no longer lists the organization, or it no longer allows
// force_on for a force_on server. The operator may no longer manage MCP
// servers there, so the controller disables the config with the one PATCH
// of sendMCPServerDisable. It reads no credential. The disable is a kill
// switch, as in disableMCPServerWithoutConsent: a restored opt-in needs a
// spec edit to enable the config again.
func (r *CoderMCPServerReconciler) disableMCPServerWithoutOptIn(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, before *coderv1alpha1.CoderMCPServerStatus, orgID uuid.UUID, failed *mcpServerStep,
) (*mcpServerStep, error) {
	id, err := uuid.Parse(server.Status.ConfigID)
	if err != nil || id == uuid.Nil || failed == nil || !failed.stalled {
		return nil, fmt.Errorf("assertion failed: MCP server %s/%s needs a config ID and a stalled opt-in failure to be disabled", server.Namespace, server.Name)
	}
	stalled := func(format string, args ...any) *mcpServerStep {
		return mcpServerStalled(mcpServerResync, failed.reason, "%s %s", fmt.Sprintf(format, args...), failed.message)
	}
	unchecked := func(format string, args ...any) *mcpServerStep {
		return mcpServerWait(mcpServerRetry, failed.reason, "Coder can still use MCP server config %s (%s). %s", id, fmt.Sprintf(format, args...), failed.message)
	}
	if _, step := readMCPServerToDisable(ctx, sdk, server, orgID, id, stalled, unchecked); step != nil {
		return step, nil
	}
	if step, err := r.sendMCPServerDisable(ctx, sdk, server, before, orgID, id, failed.reason, unchecked); step != nil || err != nil {
		return step, err
	}
	return stalled("The controller disabled MCP server config %s in Coder: the control plane no longer allows it. After the opt-in is back, a spec edit enables the config.", id), nil
}

// errMCPServerSecretChanged reports that a Secret changed between two reads
// of one consent check.
var errMCPServerSecretChanged = errors.New("a Secret changed during the consent check")

// mcpServerConsentHolds reports whether every credential that found holds
// comes from a Secret in status.credentials that still passes the contract,
// with the same UID and a value that Coder can store, for each URL of found
// that receives it (plan 5.2). Coder holds the value of that Secret, so the
// spec's current URLs do not matter here.
//
// The spec must still name the recorded Secrets: a PATCH whose answer was
// lost after the commit can have sent a Secret of the spec that status does
// not record yet. CoderAIProvider reads the spec's ref for the same reason.
func (r *CoderMCPServerReconciler) mcpServerConsentHolds(ctx context.Context, server *coderv1alpha1.CoderMCPServer, found codersdk.MCPServerConfig) (bool, error) {
	recorded := map[string]coderv1alpha1.CoderMCPServerCredentialStatus{}
	for _, c := range server.Status.Credentials {
		recorded[c.Field] = c
	}
	for _, c := range mcpServerCredentialRefs(server.Spec.Auth) {
		if got, ok := recorded[c.field]; !ok || got.SecretName != c.ref.Name || got.SecretKey != c.ref.Key {
			return false, nil
		}
	}
	toURL := []string{found.URL}
	toOAuth2 := []string{found.URL, found.OAuth2AuthURL, found.OAuth2TokenURL}
	if found.OAuth2RevocationURL != "" {
		toOAuth2 = append(toOAuth2, found.OAuth2RevocationURL)
	}
	held := []struct {
		held         bool
		match        func(field string) bool
		destinations []string
		headerValue  bool
	}{
		{found.HasAPIKey, func(f string) bool { return f == "apiKey" }, toURL, true},
		{found.HasCustomHeaders, func(f string) bool { return strings.HasPrefix(f, "header:") }, toURL, true},
		{found.HasOAuth2Secret, func(f string) bool { return f == "oauth2ClientSecret" }, toOAuth2, false},
	}
	seen := map[string]credentialsecret.ChangeToken{}
	for _, h := range held {
		if !h.held {
			continue
		}
		matched := 0
		for _, c := range server.Status.Credentials {
			if !h.match(c.Field) {
				continue
			}
			matched++
			for _, raw := range h.destinations {
				destination, err := aiProviderBaseURL(raw)
				if err != nil {
					return false, nil //nolint:nilerr // An origin that does not parse has no proof of consent.
				}
				if raw == found.OAuth2RevocationURL && destination.Scheme != "https" {
					// The spec allows only an https revocation URL (#334).
					return false, nil
				}
				value, err := credentialsecret.Read(ctx, r.Client, server.Namespace, coderv1alpha1.SecretKeySelector{Name: c.SecretName, Key: c.SecretKey}, destination)
				if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
					return false, nil
				} else if err != nil {
					return false, err
				}
				// A malformed value also withdraws consent, as in the read of
				// readMCPServerCredentials.
				if string(value.Token.SecretUID) != c.SecretUID || mcpServerValueProblem(value, h.headerValue) != "" {
					return false, nil
				}
				// All reads of one Secret must see one Secret state, as in
				// readMCPServerCredentials.
				if first, ok := seen[c.SecretName]; ok && first.ResourceVersion != value.Token.ResourceVersion {
					return false, errMCPServerSecretChanged
				}
				seen[c.SecretName] = value.Token
			}
		}
		if matched == 0 {
			// A credential without a recorded Secret has no proof of consent.
			return false, nil
		}
	}
	return true, nil
}
