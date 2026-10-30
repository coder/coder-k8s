package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// syncMCPServer reads the config that this object created and makes it match
// the spec (plan 4.5 create steps 5 and 6, 4.7). It runs on every reconcile,
// so a dashboard edit is set back at the latest on the next resync.
//
// One sparse PATCH sends every field that differs. A PATCH that sets a URL or
// the auth type also carries every credential of the auth type (plan 4.6 item
// 5), and a credential also goes when its Secret changed or Coder lost it.
// Credentials of the other auth types are cleared. The ACL entries that
// spec.access does not list go before the PATCH and the missing groups after
// it, so the ACL never holds a group that neither generation allows (plan
// S20).
//
// After a full apply of this generation, a dashboard delete, disable or
// credential clear is a kill switch: the controller reports it and writes
// nothing until the spec changes (plan 4.7 item 2). An auth type user_oidc
// is always set back, with only the auth fields and url, so a disable stays.
//
// recreate is true when the config is gone before a full apply of this
// generation: the caller then creates it again, if the slug is free.
func (r *CoderMCPServerReconciler) syncMCPServer(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID, values map[string]credentialsecret.Value,
) (step *mcpServerStep, recreate bool, err error) {
	id, err := uuid.Parse(server.Status.ConfigID)
	if err != nil || id == uuid.Nil {
		return nil, false, fmt.Errorf("assertion failed: MCP server %s/%s has status.configID %q, which is not a UUID", server.Namespace, server.Name, server.Status.ConfigID)
	}
	applied := server.Status.AppliedGeneration == server.Generation
	found, err := sdk.MCPServerConfigByID(ctx, orgID, id)
	switch {
	// Only Coder's own 404 proves that the config is gone. A proxy page or
	// "Route not found." waits below and changes nothing (#337).
	case isCoderResourceNotFound(err):
		server.Status.Enabled = nil
		// The control plane can reach another Coder deployment now, which
		// answers 404 for a config of the old one (#353).
		if step, err := r.recordedDeploymentChanged(ctx, sdk, server); step != nil || err != nil {
			return step, false, err
		}
		if applied {
			return mcpServerStalled(mcpServerResync, "DeletedExternally",
				"MCP server config %s was deleted in Coder. The controller does not create it again until the spec changes.", id), false, nil
		}
		server.Status.ConfigID, server.Status.Credentials = "", nil
		r.sentCredentials.Delete(client.ObjectKeyFromObject(server))
		return nil, true, nil
	case err != nil:
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "get MCP server config %s: %s", id, coderErrorSummary(err)), false, nil
	}
	if found.ID != id || found.OrganizationID != orgID || found.Slug != server.Spec.Slug {
		return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch",
			"assertion failed: Coder answered MCP server config %s with slug %s in organization %s when asked for %s", found.ID, found.Slug, found.OrganizationID, id), false, nil
	}
	server.Status.Enabled = &found.Enabled

	// user_oidc sends each chat user's login token to url (plan F14), so it is
	// set back even when a kill switch holds every other write.
	oidc := found.AuthType == "user_oidc"
	enabled := server.Spec.Enabled == nil || *server.Spec.Enabled
	var kill *mcpServerStep
	switch active := mcpServerActiveCredentials(server.Spec.Auth); {
	case applied && enabled && !found.Enabled:
		kill = mcpServerStalled(mcpServerResync, "DisabledExternally",
			"MCP server config %s was disabled in Coder. The controller does not enable it again until the spec changes.", id)
	case applied && !oidc && (active.apiKey && !found.HasAPIKey || active.headers && !found.HasCustomHeaders || active.secret && !found.HasOAuth2Secret):
		// A dashboard change of the auth type lands here too: Coder clears
		// the credentials of the old type (plan F9).
		kill = mcpServerStalled(mcpServerResync, "CredentialClearedExternally",
			"A credential of MCP server config %s was cleared in Coder (auth type %s). The controller does not send it again until the spec changes.", id, found.AuthType)
	}
	if kill != nil && !oidc {
		return kill, false, nil
	}

	full, err := mcpServerUpdateRequest(server, values)
	if err != nil {
		return nil, false, err
	}
	// A hash is only for a field of the spec's auth type: a change of the
	// auth type cleared the others in Coder, also when its answer was lost.
	r.pruneSentCredentials(server)
	req, sent := r.mcpServerSparseRequest(server, full, found, values)
	if kill != nil {
		req = mcpServerAuthOnly(req)
	}
	if req != (codersdk.UpdateMCPServerConfigRequest{}) {
		if kill == nil {
			if step, err := narrowMCPServerACL(ctx, sdk, orgID, id, server.Spec.Access); step != nil || err != nil {
				if step != nil && step.reason == "GroupNotFound" && req.Enabled != nil && !*req.Enabled {
					// Fail closed (#313): a generation that disables the
					// server disables it now, while a group is missing.
					return r.disableForMissingGroup(ctx, sdk, server, orgID, id, step), false, nil
				}
				return step, false, err
			}
		}
		// Coder can commit the PATCH and the answer still get lost. Until a
		// good answer, the controller does not know which value Coder holds,
		// so a retry must send these credentials again.
		r.forgetSentCredentials(server, sent)
		updated, err := sdk.UpdateMCPServerConfig(ctx, orgID, id, req)
		switch status := coderStatus(err); {
		case status == http.StatusBadRequest:
			return mcpServerStalled(0, "CoderRejected", "Coder rejected the update: %s", coderErrorSummary(err)), false, nil
		case err != nil:
			// The PATCH sets absolute values, so sending it again is safe. This
			// includes a 404: a proxy can put one in place of a committed answer,
			// so the GET of the next reconcile decides whether the config is gone.
			return mcpServerWait(mcpServerRetry, "CoderUnavailable", "update MCP server config %s: %s", id, coderErrorSummary(err)), false, nil
		}
		check := full
		if kill != nil {
			check = mcpServerAuthOnly(full)
		}
		if field := mcpServerMismatch(server.Spec.Auth, check, updated); field != "" || updated.ID != id {
			return mcpServerStalled(mcpServerResync, "CoderAnswerMismatch", "assertion failed: Coder answered the update of MCP server config %s with another %s", id, cmp.Or(field, "ID")), false, nil
		}
		server.Status.Enabled = &updated.Enabled
		r.rememberSentCredentials(server, sent, values)
	} else if field := mcpServerMismatch(server.Spec.Auth, full, found); field != "" {
		return nil, false, fmt.Errorf("assertion failed: MCP server %s/%s differs in %s, but the sync request is empty", server.Namespace, server.Name, field)
	}
	if kill != nil {
		kill.message = "The controller set the auth type back from user_oidc. " + kill.message
		return kill, false, nil
	}
	if step, err := syncMCPServerACL(ctx, sdk, orgID, id, server.Spec.Access); step != nil || err != nil {
		return step, false, err
	}
	server.Status.Credentials = mcpServerCredentialStatus(server.Spec.Auth, values)
	server.Status.AppliedGeneration = server.Generation
	return mcpServerReady(server), false, nil
}

// disableForMissingGroup sends a PATCH that holds only enabled false, for a
// generation that disables the server while one of its access.groups does
// not resolve (#313). The generation stays unapplied, so the rest of it
// follows once the group exists. It returns the GroupNotFound step, or a
// wait when the PATCH failed.
func (r *CoderMCPServerReconciler) disableForMissingGroup(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID, id uuid.UUID, missing *mcpServerStep,
) *mcpServerStep {
	disabled := false
	updated, err := sdk.UpdateMCPServerConfig(ctx, orgID, id, codersdk.UpdateMCPServerConfigRequest{Enabled: &disabled})
	if err != nil {
		return mcpServerWait(mcpServerRetry, missing.reason, "MCP server config %s is still enabled: the disable failed: %s. %s", id, coderErrorSummary(err), missing.message)
	}
	if updated.ID != id || updated.OrganizationID != orgID || updated.Slug != server.Spec.Slug || updated.Enabled {
		return mcpServerWait(mcpServerRetry, "CoderAnswerMismatch",
			"Coder answered the disable of MCP server config %s with another or an enabled config. The controller disables it again.", id)
	}
	server.Status.Enabled = &updated.Enabled
	return missing
}

// mcpServerCredentialSet says which credential fields an auth type uses.
type mcpServerCredentialSet struct{ apiKey, headers, secret bool }

func mcpServerActiveCredentials(auth coderv1alpha1.CoderMCPServerAuth) mcpServerCredentialSet {
	return mcpServerCredentialSet{apiKey: auth.Type == "api_key", headers: auth.Type == "custom_headers", secret: auth.Type == "oauth2"}
}

// mcpServerSparseRequest drops every field of full that found already holds.
// It keeps a credential when a URL or the auth type changes, when its change
// token changed and Coder does not hold the value already, or when Coder lost
// it. It clears credentials of the other auth types that Coder holds (#314).
// It returns the credential fields that the request sends.
func (r *CoderMCPServerReconciler) mcpServerSparseRequest(
	server *coderv1alpha1.CoderMCPServer, full codersdk.UpdateMCPServerConfigRequest, found codersdk.MCPServerConfig, values map[string]credentialsecret.Value,
) (codersdk.UpdateMCPServerConfigRequest, []string) {
	req := full
	authChanged := found.AuthType != *full.AuthType
	for _, f := range mcpServerStringFields(&req, found) {
		if *f.want != nil && (!authChanged || !f.auth) && mcpServerSameString(f.name, **f.want, f.got) {
			*f.want = nil
		}
	}
	for _, f := range mcpServerBoolFields(&req, found) {
		if *f.want != nil && **f.want == f.got {
			*f.want = nil
		}
	}
	destinationChanged := req.URL != nil || req.AuthType != nil || req.OAuth2AuthURL != nil || req.OAuth2TokenURL != nil || req.OAuth2RevocationURL != nil
	recorded := map[string]coderv1alpha1.CoderMCPServerCredentialStatus{}
	for _, c := range server.Status.Credentials {
		recorded[c.Field] = c
	}
	needs := func(field string, held bool) bool {
		if destinationChanged || !held {
			return true
		}
		token := values[field].Token
		c, ok := recorded[field]
		if ok && c.SecretName == token.SecretName && c.SecretKey == token.SecretKey && c.SecretUID == string(token.SecretUID) && c.SecretResourceVersion == token.ResourceVersion {
			return false
		}
		// Only the Secret's metadata changed when Coder holds this value.
		return !r.sentCredentialMatches(server, field, values[field])
	}
	var sent []string
	for _, c := range mcpServerCredentialRefs(server.Spec.Auth) {
		held := map[string]bool{"apiKey": found.HasAPIKey, "oauth2ClientSecret": found.HasOAuth2Secret}[c.field] || strings.HasPrefix(c.field, "header:") && found.HasCustomHeaders
		if needs(c.field, held) {
			sent = append(sent, c.field)
		}
	}
	active := mcpServerActiveCredentials(server.Spec.Auth)
	sends := func(prefix string) bool {
		return slices.ContainsFunc(sent, func(f string) bool { return strings.HasPrefix(f, prefix) })
	}
	if active.headers && sends("header:") {
		// Custom headers are one Coder field, so they all go together.
		sent = slices.DeleteFunc(sent, func(f string) bool { return strings.HasPrefix(f, "header:") })
		for _, h := range server.Spec.Auth.CustomHeaders {
			sent = append(sent, "header:"+h.Name)
		}
	} else {
		req.CustomHeaders = nil
	}
	if !active.apiKey || !sends("apiKey") {
		req.APIKeyValue = nil
	}
	if !active.secret || !sends("oauth2ClientSecret") {
		req.OAuth2ClientSecret = nil
	}
	// Coder keeps other types' credentials while the auth type stays.
	empty, none := "", map[string]string{}
	if !active.apiKey && found.HasAPIKey {
		req.APIKeyValue = &empty
	}
	if !active.headers && found.HasCustomHeaders {
		req.CustomHeaders = &none
	}
	if !active.secret && found.HasOAuth2Secret {
		req.OAuth2ClientSecret = &empty
	}
	return req, sent
}

// mcpServerAuthOnly keeps only the auth fields of req and url. The auth
// fields can carry credentials, so url goes with them: Coder must never
// store a credential next to a url that the Secret does not allow (plan 4.6
// item 5).
func mcpServerAuthOnly(req codersdk.UpdateMCPServerConfigRequest) codersdk.UpdateMCPServerConfigRequest {
	return codersdk.UpdateMCPServerConfigRequest{
		URL: req.URL, AuthType: req.AuthType, APIKeyHeader: req.APIKeyHeader, APIKeyValue: req.APIKeyValue, CustomHeaders: req.CustomHeaders,
		OAuth2ClientID: req.OAuth2ClientID, OAuth2ClientSecret: req.OAuth2ClientSecret, OAuth2AuthURL: req.OAuth2AuthURL,
		OAuth2TokenURL: req.OAuth2TokenURL, OAuth2RevocationURL: req.OAuth2RevocationURL, OAuth2Scopes: req.OAuth2Scopes,
	}
}

type mcpServerStringField struct {
	name string
	want **string
	got  string
	auth bool
}

func mcpServerStringFields(req *codersdk.UpdateMCPServerConfigRequest, got codersdk.MCPServerConfig) []mcpServerStringField {
	return []mcpServerStringField{
		{"display_name", &req.DisplayName, got.DisplayName, false},
		{"description", &req.Description, got.Description, false},
		{"icon_url", &req.IconURL, got.IconURL, false},
		{"transport", &req.Transport, got.Transport, false},
		{"url", &req.URL, got.URL, false},
		{"availability", &req.Availability, got.Availability, false},
		{"auth_type", &req.AuthType, got.AuthType, true},
		{"api_key_header", &req.APIKeyHeader, got.APIKeyHeader, true},
		{"oauth2_client_id", &req.OAuth2ClientID, got.OAuth2ClientID, true},
		{"oauth2_auth_url", &req.OAuth2AuthURL, got.OAuth2AuthURL, true},
		{"oauth2_token_url", &req.OAuth2TokenURL, got.OAuth2TokenURL, true},
		{"oauth2_revocation_url", &req.OAuth2RevocationURL, got.OAuth2RevocationURL, true},
		{"oauth2_scopes", &req.OAuth2Scopes, got.OAuth2Scopes, true},
	}
}

type mcpServerBoolField struct {
	name string
	want **bool
	got  bool
}

func mcpServerBoolFields(req *codersdk.UpdateMCPServerConfigRequest, got codersdk.MCPServerConfig) []mcpServerBoolField {
	return []mcpServerBoolField{
		{"enabled", &req.Enabled, got.Enabled},
		{"model_intent", &req.ModelIntent, got.ModelIntent},
		{"allow_in_plan_mode", &req.AllowInPlanMode, got.AllowInPlanMode},
		{"forward_coder_headers", &req.ForwardCoderHeaders, got.ForwardCoderHeaders},
	}
}

// mcpServerSameString compares one string field. Scopes are a set of tokens
// separated by spaces (plan 4.7 item 5).
func mcpServerSameString(name, want, got string) bool {
	if name != "oauth2_scopes" {
		return want == got
	}
	a, b := strings.Fields(want), strings.Fields(got)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// mcpServerMismatch returns the first field in which got differs from want,
// or "". It checks every field that want sets and, on every apply, that Coder
// holds exactly the credentials of the auth type (#314).
func mcpServerMismatch(auth coderv1alpha1.CoderMCPServerAuth, want codersdk.UpdateMCPServerConfigRequest, got codersdk.MCPServerConfig) string {
	for _, f := range mcpServerStringFields(&want, got) {
		if *f.want != nil && !mcpServerSameString(f.name, **f.want, f.got) {
			return f.name
		}
	}
	for _, f := range mcpServerBoolFields(&want, got) {
		if *f.want != nil && **f.want != f.got {
			return f.name
		}
	}
	if want.AuthType == nil {
		// An auth-only check without the auth type cannot happen.
		return ""
	}
	active := mcpServerActiveCredentials(auth)
	switch {
	case active.apiKey != got.HasAPIKey:
		return "has_api_key"
	case active.headers != got.HasCustomHeaders:
		return "has_custom_headers"
	case active.secret != got.HasOAuth2Secret:
		return "has_oauth2_secret"
	}
	return ""
}

// rememberSentCredentials keeps a SHA-256 of each credential value that Coder
// now holds, in process memory only (plan 4.6 item 4), so a metadata-only
// Secret change sends nothing. After a restart the map is empty, so one
// resend can happen. It drops the hashes of fields that the spec's auth type
// does not use: the PATCH cleared those credentials, so a later round trip
// of the auth type must send them again.
func (r *CoderMCPServerReconciler) rememberSentCredentials(server *coderv1alpha1.CoderMCPServer, sent []string, values map[string]credentialsecret.Value) {
	sums := map[string][sha256.Size]byte{}
	if old, ok := r.loadSentCredentials(server); ok {
		for _, c := range mcpServerCredentialRefs(server.Spec.Auth) {
			if sum, ok := old[c.field]; ok {
				sums[c.field] = sum
			}
		}
	}
	for _, field := range sent {
		sums[field] = sha256.Sum256([]byte(values[field].Reveal()))
	}
	r.storeSentCredentials(server, sums)
}

// pruneSentCredentials drops the hashes of fields that the spec's auth type
// does not use.
func (r *CoderMCPServerReconciler) pruneSentCredentials(server *coderv1alpha1.CoderMCPServer) {
	old, ok := r.loadSentCredentials(server)
	if !ok {
		return
	}
	sums := map[string][sha256.Size]byte{}
	for _, c := range mcpServerCredentialRefs(server.Spec.Auth) {
		if sum, ok := old[c.field]; ok {
			sums[c.field] = sum
		}
	}
	r.storeSentCredentials(server, sums)
}

// forgetSentCredentials drops the hashes of fields before a write that sends
// them.
func (r *CoderMCPServerReconciler) forgetSentCredentials(server *coderv1alpha1.CoderMCPServer, fields []string) {
	old, ok := r.loadSentCredentials(server)
	if !ok || len(fields) == 0 {
		return
	}
	sums := maps.Clone(old)
	for _, field := range fields {
		delete(sums, field)
	}
	r.storeSentCredentials(server, sums)
}

func (r *CoderMCPServerReconciler) sentCredentialMatches(server *coderv1alpha1.CoderMCPServer, field string, value credentialsecret.Value) bool {
	sums, ok := r.loadSentCredentials(server)
	if !ok {
		return false
	}
	sum, ok := sums[field]
	return ok && sum == sha256.Sum256([]byte(value.Reveal()))
}

// sentCredentialSums is the sentCredentials entry of one object.
type sentCredentialSums struct {
	uid  types.UID
	sums map[string][sha256.Size]byte
}

// loadSentCredentials returns the hashes of server, never those of an
// earlier object with the same name.
func (r *CoderMCPServerReconciler) loadSentCredentials(server *coderv1alpha1.CoderMCPServer) (map[string][sha256.Size]byte, bool) {
	v, ok := r.sentCredentials.Load(client.ObjectKeyFromObject(server))
	if !ok {
		return nil, false
	}
	entry := v.(sentCredentialSums)
	if entry.uid != server.UID {
		return nil, false
	}
	return entry.sums, true
}

func (r *CoderMCPServerReconciler) storeSentCredentials(server *coderv1alpha1.CoderMCPServer, sums map[string][sha256.Size]byte) {
	r.sentCredentials.Store(client.ObjectKeyFromObject(server), sentCredentialSums{uid: server.UID, sums: sums})
}

// mcpServerReady reports a config that matches the spec.
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
