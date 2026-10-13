package controller_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These self-tests pin the fake's MCP server config routes to the Coder facts
// that the CoderMCPServer controller depends on (plan facts F1, F3, F7 to F12
// and F19).

const fakeMCPTestKey = "mcp-test-api-key-value" // #nosec G101 -- synthetic test value.

func apiKeyMCPRequest(slug string) codersdk.CreateMCPServerConfigRequest {
	return codersdk.CreateMCPServerConfigRequest{
		DisplayName: "GitHub", Slug: slug, Transport: "streamable_http", URL: "https://mcp.example.com/mcp",
		AuthType: "api_key", APIKeyHeader: "X-Api-Key", APIKeyValue: fakeMCPTestKey, Availability: "default_off",
	}
}

// F1, F12: answers carry has_* flags and never a credential, enabled is false
// when omitted, lookup is by UUID only, and delete is a hard delete that
// frees the slug.
func TestFakeCoderMCPServerLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	f.now = steppingClock(time.Date(2026, 10, 13, 10, 0, 0, 123456789, time.UTC))
	orgID, otherOrg := f.addOrganization("coder"), f.addOrganization("other")
	c := f.client(t, 2*time.Second)

	created, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	require.Equal(t, orgID, created.OrganizationID)
	require.False(t, created.Enabled, "enabled is false when omitted")
	require.True(t, created.HasAPIKey)
	require.False(t, created.HasCustomHeaders)
	require.True(t, created.AuthConnected)
	require.Equal(t, "X-Api-Key", created.APIKeyHeader)
	require.Equal(t, created.CreatedAt, created.UpdatedAt)
	require.Equal(t, []string{}, created.ToolAllowList)
	status, body := fakeRawRequest(t, f, http.MethodGet, "/api/v2/organizations/coder/mcp-servers/"+created.ID.String(), "")
	require.Equal(t, http.StatusOK, status, "the organization can also be named")
	require.NotContains(t, body, fakeMCPTestKey)
	require.Contains(t, body, `"has_api_key":true`)

	alpha := apiKeyMCPRequest("alpha")
	alpha.DisplayName = "Alpha"
	_, err = c.CreateMCPServerConfig(ctx, orgID, alpha)
	require.NoError(t, err)
	_, err = c.CreateMCPServerConfig(ctx, otherOrg, apiKeyMCPRequest("github"))
	require.NoError(t, err, "slugs are unique per organization")
	list, err := c.MCPServerConfigs(ctx, orgID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, []string{"alpha", "github"}, []string{list[0].Slug, list[1].Slug}, "ordered by display name")

	status, body = fakeRawRequest(t, f, http.MethodGet, "/api/v2/organizations/coder/mcp-servers/github", "")
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, body, `Invalid UUID \"github\".`)
	_, err = c.MCPServerConfigByID(ctx, otherOrg, created.ID)
	requireCoderMessage(t, err, http.StatusNotFound, "Resource not found or you do not have access to this resource")
	_, err = c.MCPServerConfigs(ctx, uuid.New())
	requireCoderMessage(t, err, http.StatusNotFound, "Resource not found or you do not have access to this resource")

	require.NoError(t, c.DeleteMCPServerConfig(ctx, orgID, created.ID))
	_, err = c.MCPServerConfigByID(ctx, orgID, created.ID)
	requireCoderStatus(t, err, http.StatusNotFound)
	requireCoderStatus(t, c.DeleteMCPServerConfig(ctx, orgID, created.ID), http.StatusNotFound)
	_, err = c.UpdateMCPServerConfig(ctx, orgID, created.ID, codersdk.UpdateMCPServerConfigRequest{})
	requireCoderStatus(t, err, http.StatusNotFound)
	again, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	require.NotEqual(t, created.ID, again.ID)
}

// F3, F19: create checks only its struct tags and the auth type's own
// fields. Slugs are unique per organization with case, and error answers
// echo only non-secret values.
func TestFakeCoderMCPServerCreateRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	orgID := f.addOrganization("coder")
	c := f.client(t, 2*time.Second)

	_, err := c.CreateMCPServerConfig(ctx, orgID, codersdk.CreateMCPServerConfigRequest{})
	validations := requireCoderMessage(t, err, http.StatusBadRequest, "Validation failed.")
	fields := []string{}
	for _, v := range validations {
		require.Equal(t, `Validation failed for tag "required" with value: ""`, v.Detail)
		fields = append(fields, v.Field)
	}
	require.Equal(t, []string{"display_name", "slug", "transport", "url", "auth_type", "availability"}, fields)

	bad := apiKeyMCPRequest("bad")
	bad.Transport, bad.URL, bad.Availability = "websocket", "not a url", "always"
	_, err = c.CreateMCPServerConfig(ctx, orgID, bad)
	require.Equal(t, []codersdk.ValidationError{
		{Field: "transport", Detail: `Validation failed for tag "oneof" with value: "websocket"`},
		{Field: "url", Detail: `Validation failed for tag "url" with value: "not a url"`},
		{Field: "availability", Detail: `Validation failed for tag "oneof" with value: "always"`},
	}, requireCoderMessage(t, err, http.StatusBadRequest, "Validation failed."))
	status, body := fakeRawRequest(t, f, http.MethodPost, "/api/v2/organizations/coder/mcp-servers", "{")
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, body, "Request body must be valid JSON.")

	oauth := func(slug, revocation string) codersdk.CreateMCPServerConfigRequest {
		r := apiKeyMCPRequest(slug)
		r.AuthType, r.APIKeyHeader, r.APIKeyValue = "oauth2", "", ""
		r.OAuth2ClientID, r.OAuth2ClientSecret = "client", "mcp-test-client-secret"
		r.OAuth2AuthURL, r.OAuth2TokenURL, r.OAuth2RevocationURL = "https://auth.example.com/a", "https://other.example.net/t", revocation
		return r
	}
	_, err = c.CreateMCPServerConfig(ctx, orgID, oauth("rev-http", "http://auth.example.com/r"))
	requireCoderMessage(t, err, http.StatusBadRequest, "Invalid OAuth2 revocation URL.")
	for _, rev := range []string{"", "https://auth.example.com/r", "http://localhost:9000/r", "http://127.0.0.1/r"} {
		got, err := c.CreateMCPServerConfig(ctx, orgID, oauth("rev-"+uuid.NewString()[:8], rev))
		require.NoError(t, err, rev)
		require.True(t, got.HasOAuth2Secret)
		require.False(t, got.AuthConnected, "oauth2 configs need each user to connect")
	}
	partial := oauth("partial", "")
	partial.OAuth2TokenURL = ""
	_, err = c.CreateMCPServerConfig(ctx, orgID, partial)
	requireCoderMessage(t, err, http.StatusBadRequest, "OAuth2 auth type requires either all of oauth2_client_id, oauth2_auth_url, and oauth2_token_url (manual configuration), or none of them (automatic discovery via RFC 7591).")
	discovery := oauth("github", "")
	discovery.OAuth2ClientID, discovery.OAuth2AuthURL, discovery.OAuth2TokenURL = "", "", ""
	_, err = c.CreateMCPServerConfig(ctx, orgID, discovery)
	requireCoderStatus(t, err, http.StatusBadRequest)

	missingKey := apiKeyMCPRequest("missing-key")
	missingKey.APIKeyValue = ""
	_, err = c.CreateMCPServerConfig(ctx, orgID, missingKey)
	requireCoderMessage(t, err, http.StatusBadRequest, "API key auth type requires api_key_header and api_key_value.")
	noHeaders := apiKeyMCPRequest("no-headers")
	noHeaders.AuthType = "custom_headers"
	_, err = c.CreateMCPServerConfig(ctx, orgID, noHeaders)
	requireCoderMessage(t, err, http.StatusBadRequest, "Custom headers auth type requires at least one custom header.")

	// Coder stores another auth type's fields too, and trims every string.
	mixed := apiKeyMCPRequest(" Mixed Slug ")
	mixed.AuthType, mixed.DisplayName, mixed.APIKeyValue = "none", "  GitHub  ", "  "+fakeMCPTestKey+"\n"
	mixed.CustomHeaders = map[string]string{"Host": "h"}
	mixed.ToolAllowList, mixed.ToolDenyList = []string{" a ", "", "a"}, nil
	got, err := c.CreateMCPServerConfig(ctx, orgID, mixed)
	require.NoError(t, err)
	require.Equal(t, "Mixed Slug", got.Slug, "Coder does not validate slugs")
	require.Equal(t, "GitHub", got.DisplayName)
	require.True(t, got.HasAPIKey)
	require.True(t, got.HasCustomHeaders)
	require.Equal(t, []string{"a", "a"}, got.ToolAllowList)
	require.Equal(t, []string{}, got.ToolDenyList)
	stored := f.mcpServers()
	require.Equal(t, fakeMCPTestKey, stored[len(stored)-1].apiKeyValue)

	// The oauth2 discovery path checks the slug first.
	discovery.Slug = "Mixed Slug"
	_, err = c.CreateMCPServerConfig(ctx, orgID, discovery)
	requireCoderMessage(t, err, http.StatusConflict, "MCP server config already exists.")
	_, err = c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("Mixed Slug"))
	requireCoderMessage(t, err, http.StatusConflict, "MCP server config already exists.")
	require.NotContains(t, err.Error(), fakeMCPTestKey)
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, fakeMCPSlugViolation, sdkErr.Detail)
	_, err = c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("mixed slug"))
	require.NoError(t, err, "slugs compare with case")
}

// F7 to F11: PATCH keeps nil fields, trims and replaces set ones, clears other
// auth types' fields on an auth type change, drops users' OAuth2 tokens when a
// bound field changes, checks nothing again after the merge, and always moves
// updated_at.
func TestFakeCoderMCPServerUpdateRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	f.now = steppingClock(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC))
	orgID := f.addOrganization("coder")
	c := f.client(t, 2*time.Second)
	created, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	patch := func(req codersdk.UpdateMCPServerConfigRequest) codersdk.MCPServerConfig {
		t.Helper()
		got, err := c.UpdateMCPServerConfig(ctx, orgID, created.ID, req)
		require.NoError(t, err)
		return got
	}
	stored := func() fakeMCPServer { t.Helper(); return f.mcpServers()[0] }

	got := patch(codersdk.UpdateMCPServerConfigRequest{})
	require.True(t, got.UpdatedAt.After(created.UpdatedAt), "an empty PATCH moves updated_at")
	before := got.UpdatedAt
	require.True(t, patch(codersdk.UpdateMCPServerConfigRequest{DisplayName: ptrTo("GitHub")}).UpdatedAt.After(before), "a same-value PATCH moves updated_at")

	f.setMCPUserTokens(created.ID, 3)
	got = patch(codersdk.UpdateMCPServerConfigRequest{Slug: ptrTo("gh"), Enabled: ptrTo(true)})
	require.Equal(t, "gh", got.Slug)
	require.True(t, got.Enabled)
	require.Equal(t, 3, stored().userTokens, "a slug change keeps users' tokens")
	got = patch(codersdk.UpdateMCPServerConfigRequest{URL: ptrTo("https://moved.example.org/mcp")})
	require.Equal(t, "https://moved.example.org/mcp", got.URL)
	require.True(t, got.HasAPIKey, "a URL change keeps the stored key")
	require.Equal(t, fakeMCPTestKey, stored().apiKeyValue)
	require.Zero(t, stored().userTokens, "a URL change drops users' tokens")

	got = patch(codersdk.UpdateMCPServerConfigRequest{APIKeyValue: ptrTo("")})
	require.False(t, got.HasAPIKey, `"" clears a credential`)
	got = patch(codersdk.UpdateMCPServerConfigRequest{AuthType: ptrTo("custom_headers"), CustomHeaders: &map[string]string{"X-A": "v"}})
	require.True(t, got.HasCustomHeaders)
	require.Empty(t, got.APIKeyHeader, "an auth type change clears the old type's fields")
	got = patch(codersdk.UpdateMCPServerConfigRequest{CustomHeaders: &map[string]string{}})
	require.False(t, got.HasCustomHeaders, "{} clears the headers")
	got = patch(codersdk.UpdateMCPServerConfigRequest{AuthType: ptrTo("api_key")})
	require.Equal(t, "api_key", got.AuthType)
	require.False(t, got.HasAPIKey, "Coder accepts api_key without a key after the merge")

	got = patch(codersdk.UpdateMCPServerConfigRequest{
		AuthType: ptrTo("oauth2"), OAuth2ClientID: ptrTo("client"), OAuth2ClientSecret: ptrTo("mcp-test-client-secret"),
		OAuth2AuthURL: ptrTo("https://auth.example.com/a"), OAuth2TokenURL: ptrTo("https://auth.example.com/t"),
		OAuth2RevocationURL: ptrTo("https://auth.example.com/r"),
	})
	require.True(t, got.HasOAuth2Secret)
	f.setMCPUserTokens(created.ID, 2)
	patch(codersdk.UpdateMCPServerConfigRequest{OAuth2ClientSecret: ptrTo("mcp-test-client-secret-2"), OAuth2AuthURL: ptrTo("https://auth.example.com/a2")})
	require.Equal(t, 2, stored().userTokens, "a client secret or auth URL change keeps users' tokens")
	got = patch(codersdk.UpdateMCPServerConfigRequest{OAuth2RevocationURL: ptrTo("")})
	require.Empty(t, got.OAuth2RevocationURL)
	require.Zero(t, stored().userTokens, "a revocation URL change drops users' tokens")
	got = patch(codersdk.UpdateMCPServerConfigRequest{AuthType: ptrTo("none")})
	require.False(t, got.HasOAuth2Secret)
	require.Empty(t, got.OAuth2ClientID)

	// The url tag parses the raw value, so a padded URL fails before Coder trims it.
	for field, req := range map[string]codersdk.UpdateMCPServerConfigRequest{
		"transport": {Transport: ptrTo("")}, "url": {URL: ptrTo(" https://moved.example.org/mcp")},
		"oauth2_token_url": {OAuth2TokenURL: ptrTo("")},
	} {
		_, err = c.UpdateMCPServerConfig(ctx, orgID, created.ID, req)
		validations := requireCoderMessage(t, err, http.StatusBadRequest, "Validation failed.")
		require.Equal(t, field, validations[0].Field)
	}
	_, err = c.UpdateMCPServerConfig(ctx, orgID, created.ID, codersdk.UpdateMCPServerConfigRequest{OAuth2RevocationURL: ptrTo("http://auth.example.com/r")})
	requireCoderMessage(t, err, http.StatusBadRequest, "Invalid OAuth2 revocation URL.")
	_, err = c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("taken"))
	require.NoError(t, err)
	_, err = c.UpdateMCPServerConfig(ctx, orgID, created.ID, codersdk.UpdateMCPServerConfigRequest{Slug: ptrTo("taken")})
	requireCoderMessage(t, err, http.StatusConflict, "MCP server config slug already exists.")
	require.Equal(t, "gh", stored().view.Slug, "a refused PATCH changes nothing")
}

// The fake records every write body as sent, so controller tests can assert
// which fields and credentials a request carried. Answers never hold them.
func TestFakeCoderMCPServerRecordsBodies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCoder(t)
	orgID := f.addOrganization("coder")
	c := f.client(t, 2*time.Second)
	created, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
	require.NoError(t, err)
	_, err = c.UpdateMCPServerConfig(ctx, orgID, created.ID, codersdk.UpdateMCPServerConfigRequest{Enabled: ptrTo(true)})
	require.NoError(t, err)

	creates, updates := f.mcpRequestBodies(routeCreateMCPServer), f.mcpRequestBodies(routeUpdateMCPServer)
	require.Len(t, creates, 1)
	require.Contains(t, string(creates[0]), fakeMCPTestKey)
	require.Equal(t, []string{`{"enabled":true}`}, []string{string(updates[0][:len(updates[0])-1])}, "the SDK sends only set fields")
	status, body := fakeRawRequest(t, f, http.MethodGet, "/api/v2/organizations/coder/mcp-servers", "")
	require.Equal(t, http.StatusOK, status)
	require.NotContains(t, body, fakeMCPTestKey)
}

// The shared fault injection reaches the create route, so controller tests can
// lose an answer before or after Coder commits the config.
func TestFakeCoderMCPServerFaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		fault     fakeFault
		status    int // zero: a transport error without a Coder status
		committed bool
	}{
		{name: "gateway timeout after commit", fault: fakeFault{Status: http.StatusGatewayTimeout, AfterCommit: true}, status: http.StatusGatewayTimeout, committed: true},
		{name: "rate limited", fault: fakeFault{Status: http.StatusTooManyRequests}, status: http.StatusTooManyRequests},
		{name: "reset before commit", fault: fakeFault{Reset: true}},
		{name: "reset after commit", fault: fakeFault{Reset: true, AfterCommit: true}, committed: true},
		{name: "hang after commit", fault: fakeFault{Hang: true, AfterCommit: true}, committed: true},
		{name: "proxy page after commit", fault: fakeFault{Status: http.StatusCreated, HTML: "<html>login</html>", AfterCommit: true}, status: http.StatusCreated, committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newFakeCoder(t)
			orgID := f.addOrganization("coder")
			c := f.client(t, 500*time.Millisecond)
			f.failNext(routeCreateMCPServer, tc.fault)

			_, err := c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
			require.Error(t, err)
			var sdkErr *codersdk.Error
			if tc.status == 0 {
				require.False(t, errors.As(err, &sdkErr), "want a transport error, got %v", err)
			} else {
				requireCoderStatus(t, err, tc.status)
			}
			require.Equal(t, 1, f.requestCount(routeCreateMCPServer))
			want := 0
			if tc.committed {
				want = 1
			}
			require.Len(t, f.mcpServers(), want)

			// The fault is used up. A committed create now blocks the slug.
			_, err = c.CreateMCPServerConfig(ctx, orgID, apiKeyMCPRequest("github"))
			if tc.committed {
				requireCoderStatus(t, err, http.StatusConflict)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
