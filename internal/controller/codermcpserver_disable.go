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
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID, failed *mcpServerStep,
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
	found, err := sdk.MCPServerConfigByID(ctx, orgID, id)
	switch {
	case isCoderResourceNotFound(err):
		// Only Coder's own 404 proves that the config is gone (#337).
		return stalled("MCP server config %s does not exist in Coder.", id), nil
	case err != nil:
		return unchecked("get MCP server config: %s", coderErrorSummary(err)), nil
	}
	if found.ID != id || found.OrganizationID != orgID || found.Slug != server.Spec.Slug {
		return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch",
			"assertion failed: Coder answered MCP server config %s with slug %s in organization %s when asked for %s", found.ID, found.Slug, found.OrganizationID, id), nil
	}
	if !found.Enabled {
		return stalled("MCP server config %s is disabled in Coder.", id), nil
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

	disabled := false
	updated, err := sdk.UpdateMCPServerConfig(ctx, orgID, id, codersdk.UpdateMCPServerConfigRequest{Enabled: &disabled})
	if err != nil {
		return unchecked("the disable failed: %s", coderErrorSummary(err)), nil
	}
	if updated.ID != id || updated.OrganizationID != orgID || updated.Slug != server.Spec.Slug || updated.Enabled {
		return mcpServerWait(mcpServerRetry, "CoderAnswerMismatch",
			"Coder answered the disable of MCP server config %s with another or an enabled config. The controller disables it again.", id), nil
	}
	// The disable counts as an external disable of this generation, also of
	// one that is not applied yet: the kill switch then holds until the next
	// spec edit.
	server.Status.AppliedGeneration = server.Generation
	// The reason stays the credential reason, so it does not flap between
	// this reconcile and the next one, which finds the config disabled.
	return stalled("The controller disabled MCP server config %s in Coder: a credential Secret no longer proves consent for a credential that Coder uses. After the Secret is fixed, a spec edit enables the config.", id), nil
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
