package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// applyMCPServer sets the spec on the config that this object created (plan
// 4.5 create steps 5 and 6). It reads the config, removes the ACL entries
// that spec.access does not list, sends one PATCH with every spec field and
// every credential of the auth type, and only then adds the missing groups.
// So while the PATCH changes or enables the config, the ACL holds only groups
// that both the old and the new access list allow (plan S20), and a failed
// PATCH leaves the ACL narrower, never broader. Every PATCH sets url, so it
// always carries the credentials (plan 4.6 item 5). It returns a ready step
// and sets status.appliedGeneration on success.
func applyMCPServer(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID, values map[string]credentialsecret.Value,
) (*mcpServerStep, error) {
	id, err := uuid.Parse(server.Status.ConfigID)
	if err != nil || id == uuid.Nil {
		return nil, fmt.Errorf("assertion failed: MCP server %s/%s has status.configID %q, which is not a UUID", server.Namespace, server.Name, server.Status.ConfigID)
	}
	current, err := sdk.MCPServerConfigByID(ctx, orgID, id)
	switch {
	case isCoderNotFound(err):
		// Re-creating it after a spec change comes with the update path.
		return mcpServerStalled(0, "DeletedExternally", "MCP server config %s no longer exists in Coder. This controller version does not create it again.", id), nil
	case err != nil:
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "get MCP server config %s: %s", id, coderErrorSummary(err)), nil
	}
	if current.ID != id || current.OrganizationID != orgID || current.Slug != server.Spec.Slug {
		return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch",
			"assertion failed: Coder answered MCP server config %s with slug %s in organization %s when asked for %s", current.ID, current.Slug, current.OrganizationID, id), nil
	}
	if step, err := narrowMCPServerACL(ctx, sdk, orgID, id, server.Spec.Access); step != nil || err != nil {
		return step, err
	}
	req, err := mcpServerUpdateRequest(server, values)
	if err != nil {
		return nil, err
	}
	updated, err := sdk.UpdateMCPServerConfig(ctx, orgID, id, req)
	switch status := coderStatus(err); {
	case status == http.StatusBadRequest:
		return mcpServerStalled(0, "CoderRejected", "Coder rejected the update: %s", coderErrorSummary(err)), nil
	case err != nil:
		// The PATCH sets absolute values, so sending it again is safe. This
		// includes a 404: a proxy can put one in place of a committed answer,
		// so the GET of the next reconcile decides whether the config is gone.
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "update MCP server config %s: %s", id, coderErrorSummary(err)), nil
	}
	if field := mcpServerUpdateMismatch(id, req, updated); field != "" {
		return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch", "assertion failed: Coder answered the update of MCP server config %s with another %s", id, field), nil
	}
	if step, err := syncMCPServerACL(ctx, sdk, orgID, id, server.Spec.Access); step != nil || err != nil {
		return step, err
	}
	server.Status.Credentials = mcpServerCredentialStatus(server.Spec.Auth, values)
	server.Status.AppliedGeneration = server.Generation
	return mcpServerReady(server), nil
}

// mcpServerUpdateMismatch returns the first field of the update answer that
// differs from the request, or "". Coder returns every field the request
// sets, except the credential values, for which it returns a has_* flag.
func mcpServerUpdateMismatch(id uuid.UUID, req codersdk.UpdateMCPServerConfigRequest, got codersdk.MCPServerConfig) string {
	if got.ID != id {
		return "ID"
	}
	strs := []struct {
		name string
		want *string
		got  string
	}{
		{"display_name", req.DisplayName, got.DisplayName},
		{"description", req.Description, got.Description},
		{"icon_url", req.IconURL, got.IconURL},
		{"transport", req.Transport, got.Transport},
		{"url", req.URL, got.URL},
		{"auth_type", req.AuthType, got.AuthType},
		{"availability", req.Availability, got.Availability},
		{"api_key_header", req.APIKeyHeader, got.APIKeyHeader},
		{"oauth2_client_id", req.OAuth2ClientID, got.OAuth2ClientID},
		{"oauth2_auth_url", req.OAuth2AuthURL, got.OAuth2AuthURL},
		{"oauth2_token_url", req.OAuth2TokenURL, got.OAuth2TokenURL},
		{"oauth2_revocation_url", req.OAuth2RevocationURL, got.OAuth2RevocationURL},
		{"oauth2_scopes", req.OAuth2Scopes, got.OAuth2Scopes},
	}
	for _, f := range strs {
		if f.want != nil && *f.want != f.got {
			return f.name
		}
	}
	bools := []struct {
		name string
		want *bool
		got  bool
	}{
		{"enabled", req.Enabled, got.Enabled},
		{"model_intent", req.ModelIntent, got.ModelIntent},
		{"allow_in_plan_mode", req.AllowInPlanMode, got.AllowInPlanMode},
		{"forward_coder_headers", req.ForwardCoderHeaders, got.ForwardCoderHeaders},
	}
	for _, f := range bools {
		if f.want != nil && *f.want != f.got {
			return f.name
		}
	}
	switch {
	case req.APIKeyValue != nil && !got.HasAPIKey:
		return "has_api_key"
	case req.CustomHeaders != nil && !got.HasCustomHeaders:
		return "has_custom_headers"
	case req.OAuth2ClientSecret != nil && !got.HasOAuth2Secret:
		return "has_oauth2_secret"
	}
	return ""
}

// mcpServerReady reports a config that matched the spec when this generation
// was applied. This version reads Coder only to apply a new generation.
func mcpServerReady(server *coderv1alpha1.CoderMCPServer) *mcpServerStep {
	return &mcpServerStep{
		ready: true, reason: "Applied", requeue: mcpServerResync,
		message: fmt.Sprintf("MCP server config %s matches generation %d of the spec.", server.Status.ConfigID, server.Status.AppliedGeneration),
	}
}

// mcpServerUpdateRequest sets every spec field and the credentials of the
// auth type. Fields of the other auth types stay unset: a changed auth type
// makes Coder clear them (plan F9). The tool lists stay unset until the spec
// has them. Credentials are revealed only here, into the request body.
func mcpServerUpdateRequest(server *coderv1alpha1.CoderMCPServer, values map[string]credentialsecret.Value) (codersdk.UpdateMCPServerConfigRequest, error) {
	create, err := mcpServerCreateRequest(server, values)
	if err != nil {
		return codersdk.UpdateMCPServerConfigRequest{}, err
	}
	spec := server.Spec
	enabled := spec.Enabled == nil || *spec.Enabled
	req := codersdk.UpdateMCPServerConfigRequest{
		DisplayName: &spec.DisplayName, Description: &create.Description, IconURL: &create.IconURL,
		Transport: &create.Transport, URL: &create.URL, AuthType: &create.AuthType, Availability: &create.Availability,
		Enabled: &enabled, ModelIntent: &create.ModelIntent, AllowInPlanMode: &create.AllowInPlanMode, ForwardCoderHeaders: &create.ForwardCoderHeaders,
	}
	switch spec.Auth.Type {
	case "api_key":
		req.APIKeyHeader, req.APIKeyValue = &create.APIKeyHeader, &create.APIKeyValue
	case "custom_headers":
		req.CustomHeaders = &create.CustomHeaders
	case "oauth2":
		req.OAuth2ClientID, req.OAuth2ClientSecret = &create.OAuth2ClientID, &create.OAuth2ClientSecret
		req.OAuth2AuthURL, req.OAuth2TokenURL = &create.OAuth2AuthURL, &create.OAuth2TokenURL
		// "" clears a revocation URL that an earlier generation set.
		req.OAuth2RevocationURL, req.OAuth2Scopes = &create.OAuth2RevocationURL, &create.OAuth2Scopes
	}
	if strings.TrimSpace(*req.DisplayName) == "" {
		return req, fmt.Errorf("assertion failed: MCP server %s/%s has an empty display name after admission", server.Namespace, server.Name)
	}
	return req, nil
}
