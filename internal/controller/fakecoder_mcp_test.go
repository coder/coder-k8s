package controller_test

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

// MCP server config routes of the fake Coder. Citations name files at Coder
// tag v2.37.3. The routes are AGPL and need only an API key
// (coderd/chat_routes.go:210-232, coderd/coderd.go:1508-1517). The fake's one
// token is a site owner, so every answer is the full, unredacted view
// (coderd/mcp.go:526-546). The fake does not model other callers: the
// redacted member view (coderd/mcp.go:1635-1649) and the 403 for a user_oidc
// write without deployment-config permission (coderd/mcp.go:574-585) need a
// second token. The ACL routes are in fakecoder_mcp_acl_test.go.
const (
	routeMCPServers      = "mcpServers"
	routeCreateMCPServer = "createMCPServer"
	routeMCPServer       = "mcpServer"
	routeUpdateMCPServer = "updateMCPServer"
	routeDeleteMCPServer = "deleteMCPServer"
)

// fakeMCPState is the mcp_server_configs table and the recorded request
// bodies of the write routes. Callers hold f.mu.
type fakeMCPState struct {
	servers []*fakeMCPServer // Insert order. Deletes remove rows (hard delete).
	// bodies holds every create, update and ACL update body as sent, in
	// request order. They carry credentials, so tests may assert on them but
	// must never log them.
	bodies []fakeMCPBody
	groups []*fakeGroup // Groups other than Everyone, in insert order.
}

type fakeMCPBody struct {
	route string
	body  []byte
}

// fakeMCPServer is one mcp_server_configs row (coderd/database/dump.sql:2578-2618).
type fakeMCPServer struct {
	view               codersdk.MCPServerConfig // The non-secret columns.
	apiKeyValue        string
	oauth2ClientSecret string
	customHeaders      map[string]string      // Stored as JSON. Empty means "{}".
	userTokens         int                    // Rows in mcp_server_user_tokens for this config.
	groupACL, userACL  map[uuid.UUID]struct{} // Each entry grants read.
}

// answer mirrors convertMCPServerConfig (coderd/mcp.go:1592-1633): the secrets
// stay out, and has_* flags say whether each one is set.
func (s *fakeMCPServer) answer() codersdk.MCPServerConfig {
	v := s.view
	v.HasAPIKey = s.apiKeyValue != ""
	v.HasOAuth2Secret = s.oauth2ClientSecret != ""
	v.HasCustomHeaders = len(s.customHeaders) > 0
	v.ToolAllowList = append([]string{}, s.view.ToolAllowList...)
	v.ToolDenyList = append([]string{}, s.view.ToolDenyList...)
	v.AuthConnected = v.AuthType != "oauth2"
	return v
}

func (f *fakeCoder) registerMCPServerRoutes(mux *http.ServeMux) {
	const base = "/api/v2/organizations/{org}/mcp-servers"
	f.route(mux, "GET "+base, routeMCPServers, f.listMCPServers)
	f.route(mux, "POST "+base, routeCreateMCPServer, f.createMCPServer)
	f.route(mux, "GET "+base+"/{id}", routeMCPServer, f.withMCPServer(func(s *fakeMCPServer, _ *http.Request) (int, any) {
		return http.StatusOK, s.answer()
	}))
	f.route(mux, "PATCH "+base+"/{id}", routeUpdateMCPServer, f.withMCPServer(f.updateMCPServer))
	f.route(mux, "DELETE "+base+"/{id}", routeDeleteMCPServer, f.withMCPServer(f.deleteMCPServer))
	f.registerMCPServerACLRoutes(mux)
}

var fakeResourceNotFound = codersdk.Response{Message: "Resource not found or you do not have access to this resource"} // coderd/httpapi/httpapi.go:149

// withMCPServer mirrors ExtractOrganizationParam and then
// ExtractMCPServerConfigParam (coderd/httpmw/mcpserverconfigparam.go:27-74):
// the config ID must be a UUID, and a config of another organization is not
// found.
func (f *fakeCoder) withMCPServer(h func(*fakeMCPServer, *http.Request) (int, any)) func(*http.Request) (int, any) {
	return func(r *http.Request) (int, any) {
		org, ok := f.orgByIdent(r.PathValue("org"))
		if !ok {
			return http.StatusNotFound, fakeResourceNotFound
		}
		raw := r.PathValue("id")
		id, err := uuid.Parse(raw)
		if err != nil { // coderd/httpmw/httpmw.go:15-35
			return http.StatusBadRequest, codersdk.Response{Message: "Invalid UUID \"" + raw + "\".", Detail: err.Error()}
		}
		i := slices.IndexFunc(f.mcp.servers, func(s *fakeMCPServer) bool { return s.view.ID == id && s.view.OrganizationID == org.ID })
		if i < 0 {
			return http.StatusNotFound, fakeResourceNotFound
		}
		return h(f.mcp.servers[i], r)
	}
}

// listMCPServers mirrors the owner's path of listMCPServerConfigs
// (coderd/mcp.go:154-262) and GetMCPServerConfigsByOrganization
// (coderd/database/queries/mcpserverconfigs.sql:27-37): every config of the
// organization, ordered by display name, with no paging and no filter. The
// fake compares bytes, while Coder sorts with the database collation, so
// tests must not depend on the list order.
func (f *fakeCoder) listMCPServers(r *http.Request) (int, any) {
	org, ok := f.orgByIdent(r.PathValue("org"))
	if !ok {
		return http.StatusNotFound, fakeResourceNotFound
	}
	out := []codersdk.MCPServerConfig{}
	for _, s := range f.mcp.servers {
		if s.view.OrganizationID == org.ID {
			out = append(out, s.answer())
		}
	}
	slices.SortStableFunc(out, func(a, b codersdk.MCPServerConfig) int { return strings.Compare(a.DisplayName, b.DisplayName) })
	return http.StatusOK, out
}

// createMCPServer mirrors createMCPServerConfig (coderd/mcp.go:295-506).
func (f *fakeCoder) createMCPServer(r *http.Request) (int, any) {
	org, ok := f.orgByIdent(r.PathValue("org"))
	if !ok {
		return http.StatusNotFound, fakeResourceNotFound
	}
	var req codersdk.CreateMCPServerConfigRequest
	if status, resp, ok := f.readMCPBody(routeCreateMCPServer, r, &req); !ok {
		return status, resp
	}
	// The struct tags of CreateMCPServerConfigRequest (codersdk/mcp.go:144-177), in field order.
	if v := fakeValidate([]fakeTagCheck{
		{"display_name", "required", &req.DisplayName, false},
		{"slug", "required", &req.Slug, false},
		{"transport", "required,oneof=streamable_http sse", &req.Transport, false},
		{"url", "required,url", &req.URL, false},
		{"auth_type", "required,oneof=none oauth2 api_key custom_headers user_oidc", &req.AuthType, false},
		{"oauth2_auth_url", "omitempty,url", &req.OAuth2AuthURL, false},
		{"oauth2_token_url", "omitempty,url", &req.OAuth2TokenURL, false},
		{"oauth2_revocation_url", "omitempty,url", &req.OAuth2RevocationURL, false},
		{"availability", "required,oneof=force_on default_on default_off", &req.Availability, false},
	}); v != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Validation failed.", Validations: v}
	}
	if rev := strings.TrimSpace(req.OAuth2RevocationURL); rev != "" && !fakeRevocationURLAllowed(rev) {
		return fakeBadRevocationURL("oauth2_revocation_url must be an https URL (loopback hosts may use http).")
	}
	slug := strings.TrimSpace(req.Slug)
	switch req.AuthType {
	case "oauth2":
		switch {
		case req.OAuth2ClientID == "" && req.OAuth2AuthURL == "" && req.OAuth2TokenURL == "":
			// Coder checks the slug, then runs discovery and dynamic client
			// registration against url. The fake serves no metadata, so
			// discovery always fails, as it does for a private address
			// (coderd/mcp_ssrf.go:13-188).
			if f.mcpSlugTaken(org.ID, slug, uuid.Nil) {
				return http.StatusConflict, codersdk.Response{Message: "MCP server config already exists."}
			}
			return http.StatusBadRequest, codersdk.Response{
				Message: "OAuth2 auto-discovery failed. Provide oauth2_client_id, oauth2_auth_url, and oauth2_token_url manually, or ensure the MCP server supports RFC 9728 (Protected Resource Metadata) and RFC 7591 (Dynamic Client Registration).",
				Detail:  "fake coder: no OAuth2 metadata",
			}
		case req.OAuth2ClientID == "" || req.OAuth2AuthURL == "" || req.OAuth2TokenURL == "":
			return http.StatusBadRequest, codersdk.Response{Message: "OAuth2 auth type requires either all of oauth2_client_id, oauth2_auth_url, and oauth2_token_url (manual configuration), or none of them (automatic discovery via RFC 7591)."}
		}
	case "api_key":
		if req.APIKeyHeader == "" || req.APIKeyValue == "" {
			return http.StatusBadRequest, codersdk.Response{Message: "API key auth type requires api_key_header and api_key_value."}
		}
	case "custom_headers":
		if len(req.CustomHeaders) == 0 {
			return http.StatusBadRequest, codersdk.Response{Message: "Custom headers auth type requires at least one custom header."}
		}
	}
	// Coder checks no other auth type's fields, so they are stored too.
	if f.mcpSlugTaken(org.ID, slug, uuid.Nil) { // UNIQUE (organization_id, slug), with case (dump.sql:4519-4520).
		return http.StatusConflict, codersdk.Response{Message: "MCP server config already exists.", Detail: fakeMCPSlugViolation}
	}
	now := f.now().UTC().Truncate(time.Microsecond) // created_at and updated_at use the database clock.
	s := &fakeMCPServer{
		view: codersdk.MCPServerConfig{
			ID: uuid.New(), OrganizationID: org.ID, DisplayName: strings.TrimSpace(req.DisplayName), Slug: slug,
			Description: strings.TrimSpace(req.Description), IconURL: strings.TrimSpace(req.IconURL),
			Transport: strings.TrimSpace(req.Transport), URL: strings.TrimSpace(req.URL), AuthType: strings.TrimSpace(req.AuthType),
			OAuth2ClientID: strings.TrimSpace(req.OAuth2ClientID), OAuth2AuthURL: strings.TrimSpace(req.OAuth2AuthURL),
			OAuth2TokenURL: strings.TrimSpace(req.OAuth2TokenURL), OAuth2RevocationURL: strings.TrimSpace(req.OAuth2RevocationURL),
			OAuth2Scopes: strings.TrimSpace(req.OAuth2Scopes), APIKeyHeader: strings.TrimSpace(req.APIKeyHeader),
			ToolAllowList: fakeTrimList(req.ToolAllowList), ToolDenyList: fakeTrimList(req.ToolDenyList),
			Availability: strings.TrimSpace(req.Availability), Enabled: req.Enabled, ModelIntent: req.ModelIntent,
			AllowInPlanMode: req.AllowInPlanMode, ForwardCoderHeaders: req.ForwardCoderHeaders, CreatedAt: now, UpdatedAt: now,
		},
		apiKeyValue: strings.TrimSpace(req.APIKeyValue), oauth2ClientSecret: strings.TrimSpace(req.OAuth2ClientSecret),
		customHeaders: req.CustomHeaders,
		// The Everyone group, whose ID is the organization ID, can read a new config (coderd/mcp.go:475-479).
		groupACL: map[uuid.UUID]struct{}{org.ID: {}}, userACL: map[uuid.UUID]struct{}{},
	}
	f.mcp.servers = append(f.mcp.servers, s)
	return http.StatusCreated, s.answer()
}

// updateMCPServer mirrors updateMCPServerConfig (coderd/mcp.go:611-971): a
// nil field keeps the stored value, a set field replaces it after trimming,
// and an empty PATCH still moves updated_at
// (coderd/database/queries/mcpserverconfigs.sql:145-180). Coder does not check
// the auth type's required fields again after the merge.
func (f *fakeCoder) updateMCPServer(s *fakeMCPServer, r *http.Request) (int, any) {
	var req codersdk.UpdateMCPServerConfigRequest
	if status, resp, ok := f.readMCPBody(routeUpdateMCPServer, r, &req); !ok {
		return status, resp
	}
	// The struct tags of UpdateMCPServerConfigRequest (codersdk/mcp.go:180-213).
	if v := fakeValidate([]fakeTagCheck{
		{"transport", "omitempty,oneof=streamable_http sse", req.Transport, true},
		{"url", "omitempty,url", req.URL, true},
		{"auth_type", "omitempty,oneof=none oauth2 api_key custom_headers user_oidc", req.AuthType, true},
		{"oauth2_auth_url", "omitempty,url", req.OAuth2AuthURL, true},
		{"oauth2_token_url", "omitempty,url", req.OAuth2TokenURL, true},
		{"availability", "omitempty,oneof=force_on default_on default_off", req.Availability, true},
	}); v != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Validation failed.", Validations: v}
	}
	if req.OAuth2RevocationURL != nil { // coderd/mcp.go:638-659: "" clears the URL.
		if rev := strings.TrimSpace(*req.OAuth2RevocationURL); rev != "" {
			if !fakeIsURL(rev) {
				return fakeBadRevocationURL("oauth2_revocation_url must be a valid URL or an empty string.")
			}
			if !fakeRevocationURLAllowed(rev) {
				return fakeBadRevocationURL("oauth2_revocation_url must be an https URL (loopback hosts may use http).")
			}
		}
	}
	old, next := s.view, s.view
	set := func(dst, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	set(&next.DisplayName, req.DisplayName)
	set(&next.Slug, req.Slug)
	set(&next.Description, req.Description)
	set(&next.IconURL, req.IconURL)
	set(&next.Transport, req.Transport)
	set(&next.URL, req.URL)
	set(&next.AuthType, req.AuthType)
	set(&next.OAuth2ClientID, req.OAuth2ClientID)
	set(&next.OAuth2AuthURL, req.OAuth2AuthURL)
	set(&next.OAuth2TokenURL, req.OAuth2TokenURL)
	set(&next.OAuth2RevocationURL, req.OAuth2RevocationURL)
	set(&next.OAuth2Scopes, req.OAuth2Scopes)
	set(&next.APIKeyHeader, req.APIKeyHeader)
	set(&next.Availability, req.Availability)
	apiKey, clientSecret, headers := s.apiKeyValue, s.oauth2ClientSecret, s.customHeaders
	set(&apiKey, req.APIKeyValue)
	set(&clientSecret, req.OAuth2ClientSecret)
	if req.CustomHeaders != nil {
		headers = *req.CustomHeaders
	}
	if req.ToolAllowList != nil {
		next.ToolAllowList = fakeTrimList(*req.ToolAllowList)
	}
	if req.ToolDenyList != nil {
		next.ToolDenyList = fakeTrimList(*req.ToolDenyList)
	}
	for _, b := range []struct {
		dst *bool
		src *bool
	}{{&next.Enabled, req.Enabled}, {&next.ModelIntent, req.ModelIntent}, {&next.AllowInPlanMode, req.AllowInPlanMode}, {&next.ForwardCoderHeaders, req.ForwardCoderHeaders}} {
		if b.src != nil {
			*b.dst = *b.src
		}
	}
	// A changed auth type clears the fields of the other types (coderd/mcp.go:818-879).
	if next.AuthType != old.AuthType {
		clearOAuth2 := func() {
			next.OAuth2ClientID, next.OAuth2AuthURL, next.OAuth2TokenURL, next.OAuth2RevocationURL, next.OAuth2Scopes = "", "", "", "", ""
			clientSecret = ""
		}
		clearAPIKey := func() { next.APIKeyHeader, apiKey = "", "" }
		clearHeaders := func() { headers = nil }
		switch next.AuthType {
		case "none", "user_oidc":
			clearOAuth2()
			clearAPIKey()
			clearHeaders()
		case "oauth2":
			clearAPIKey()
			clearHeaders()
		case "api_key":
			clearOAuth2()
			clearHeaders()
		case "custom_headers":
			clearOAuth2()
			clearAPIKey()
		}
	}
	if next.Slug != old.Slug && f.mcpSlugTaken(old.OrganizationID, next.Slug, old.ID) {
		return http.StatusConflict, codersdk.Response{Message: "MCP server config slug already exists.", Detail: fakeMCPSlugViolation}
	}
	// Users' OAuth2 tokens are bound to these fields (coderd/mcp.go:881-890).
	if next.URL != old.URL || next.AuthType != old.AuthType || next.OAuth2TokenURL != old.OAuth2TokenURL ||
		next.OAuth2RevocationURL != old.OAuth2RevocationURL || next.OAuth2ClientID != old.OAuth2ClientID {
		s.userTokens = 0
	}
	next.UpdatedAt = f.now().UTC().Truncate(time.Microsecond)
	s.view, s.apiKeyValue, s.oauth2ClientSecret, s.customHeaders = next, apiKey, clientSecret, headers
	return http.StatusOK, s.answer()
}

// deleteMCPServer mirrors deleteMCPServerConfig (coderd/mcp.go:973-1031) and
// DeleteMCPServerConfigByID (coderd/database/queries/mcpserverconfigs.sql:192-196):
// a hard delete. Users' tokens go with the row (dump.sql:5443-5444), and the
// slug is free again.
func (f *fakeCoder) deleteMCPServer(s *fakeMCPServer, _ *http.Request) (int, any) {
	f.mcp.servers = slices.DeleteFunc(f.mcp.servers, func(x *fakeMCPServer) bool { return x == s })
	return http.StatusNoContent, nil
}

// fakeMCPSlugViolation is the Detail of Coder's unique violation answer. It
// names the constraint and never echoes a submitted value.
const fakeMCPSlugViolation = `pq: duplicate key value violates unique constraint "mcp_server_configs_organization_id_slug_key"`

func (f *fakeCoder) mcpSlugTaken(orgID uuid.UUID, slug string, except uuid.UUID) bool {
	return slices.ContainsFunc(f.mcp.servers, func(s *fakeMCPServer) bool {
		return s.view.OrganizationID == orgID && s.view.Slug == slug && s.view.ID != except
	})
}

// readMCPBody records the raw body, then decodes it like httpapi.Read
// (coderd/httpapi/httpapi.go:254-275).
func (f *fakeCoder) readMCPBody(route string, r *http.Request, into any) (int, any, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Request body must be valid JSON.", Detail: err.Error()}, false
	}
	f.mcp.bodies = append(f.mcp.bodies, fakeMCPBody{route: route, body: raw})
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(into); err != nil {
		return http.StatusBadRequest, codersdk.Response{Message: "Request body must be valid JSON.", Detail: err.Error()}, false
	}
	return 0, nil, true
}

func fakeBadRevocationURL(detail string) (int, any) {
	return http.StatusBadRequest, codersdk.Response{Message: "Invalid OAuth2 revocation URL.", Detail: detail}
}

// fakeTagCheck is one validated field. For a pointer field (ptr), a nil value
// is an omitted field.
type fakeTagCheck struct {
	field, tags string
	value       *string
	ptr         bool
}

// fakeValidate mirrors the validator v10.30.0 tags that the MCP requests use,
// and the answer of httpapi.Read (coderd/httpapi/httpapi.go:277-290): one
// validation per failing field, stopping at the field's first failing tag.
// The detail echoes the value, so only non-secret fields carry tags.
func fakeValidate(checks []fakeTagCheck) []codersdk.ValidationError {
	var out []codersdk.ValidationError
fields:
	for _, c := range checks {
		if c.value == nil {
			continue
		}
		v := *c.value
		for _, tag := range strings.Split(c.tags, ",") {
			name, arg, _ := strings.Cut(tag, "=")
			ok := true
			switch name {
			case "omitempty":
				// An empty plain string skips its other tags. A set pointer
				// is checked even when it points to "": validator v10 skips
				// only a nil pointer.
				if !c.ptr && v == "" {
					continue fields
				}
			case "required":
				ok = v != ""
			case "oneof":
				ok = slices.Contains(strings.Fields(arg), v)
			case "url":
				ok = fakeIsURL(v)
			default:
				panic("assertion failed: unknown fake validation tag " + name)
			}
			if !ok {
				out = append(out, codersdk.ValidationError{Field: c.field, Detail: "Validation failed for tag \"" + name + "\" with value: \"" + v + "\""})
				break
			}
		}
	}
	return out
}

// fakeIsURL mirrors validator v10.30.0 isURL (baked_in.go:1477-1503).
func fakeIsURL(s string) bool {
	u, err := url.Parse(strings.ToLower(s))
	if s == "" || err != nil || u.Scheme == "" {
		return false
	}
	if u.Scheme == "file" {
		return u.Path != "" && u.Path != "/"
	}
	return u.Host != "" || u.Fragment != "" || u.Opaque != ""
}

// fakeRevocationURLAllowed mirrors ValidateRevocationEndpoint
// (coderd/x/chatd/mcpclient/mcpclient.go:1069-1106).
func fakeRevocationURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback()))
}

// fakeTrimList mirrors trimStringSlice and coalesceStringSlice
// (coderd/mcp.go:1666-1689): trimmed entries, empty ones dropped, duplicates
// kept, never nil.
func fakeTrimList(in []string) []string {
	out := []string{}
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Setup helpers and assertions. They lock the fake.

// mcpServers returns copies of the stored rows in insert order, credentials
// included, for assertions on what Coder holds.
func (f *fakeCoder) mcpServers() []fakeMCPServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeMCPServer, 0, len(f.mcp.servers))
	for _, s := range f.mcp.servers {
		c := *s
		c.customHeaders = make(map[string]string, len(s.customHeaders))
		for k, v := range s.customHeaders {
			c.customHeaders[k] = v
		}
		c.groupACL, c.userACL = maps.Clone(s.groupACL), maps.Clone(s.userACL)
		out = append(out, c)
	}
	return out
}

// mcpRequestBodies returns the raw bodies that route received, oldest first.
func (f *fakeCoder) mcpRequestBodies(route string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for _, b := range f.mcp.bodies {
		if b.route == route {
			out = append(out, slices.Clone(b.body))
		}
	}
	return out
}

// setMCPUserTokens records n users who connected their OAuth2 account to the
// config.
func (f *fakeCoder) setMCPUserTokens(id uuid.UUID, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.mcp.servers {
		if s.view.ID == id {
			s.userTokens = n
			return
		}
	}
	f.t.Fatalf("assertion failed: fake coder has no MCP server config %s", id)
}
