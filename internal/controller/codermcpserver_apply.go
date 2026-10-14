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
// 4.5 create steps 5 and 6). It reads the config, makes the ACL match
// spec.access, and then sends one PATCH with every spec field and every
// credential of the auth type. The ACL comes first, so the config is never
// enabled before its ACL matches (plan S20). Every PATCH sets url, so it
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
	if step, err := syncMCPServerACL(ctx, sdk, orgID, id, server.Spec.Access); step != nil || err != nil {
		return step, err
	}
	req, err := mcpServerUpdateRequest(server, values)
	if err != nil {
		return nil, err
	}
	updated, err := sdk.UpdateMCPServerConfig(ctx, orgID, id, req)
	switch status := coderStatus(err); {
	case isCoderNotFound(err):
		return mcpServerStalled(0, "DeletedExternally", "MCP server config %s no longer exists in Coder. This controller version does not create it again.", id), nil
	case status == http.StatusBadRequest:
		return mcpServerStalled(0, "CoderRejected", "Coder rejected the update: %s", coderErrorSummary(err)), nil
	case err != nil:
		// The PATCH sets absolute values, so sending it again is safe.
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "update MCP server config %s: %s", id, coderErrorSummary(err)), nil
	}
	if updated.ID != id || updated.DisplayName != *req.DisplayName || updated.Enabled != *req.Enabled || updated.URL != *req.URL {
		return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch", "assertion failed: Coder answered the update of MCP server config %s with other values", id), nil
	}
	server.Status.Credentials = mcpServerCredentialStatus(server.Spec.Auth, values)
	server.Status.AppliedGeneration = server.Generation
	return mcpServerReady(server), nil
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
