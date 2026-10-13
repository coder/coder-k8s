package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

func validMCPServerSpec() coderv1alpha1.CoderMCPServerSpec {
	return coderv1alpha1.CoderMCPServerSpec{
		ControlPlaneRef: coderv1alpha1.CoderControlPlaneReference{Name: "coder"},
		Organization:    "coder",
		Slug:            "github",
		DisplayName:     "GitHub",
		URL:             "https://mcp.example.com/github/mcp",
		Auth: coderv1alpha1.CoderMCPServerAuth{
			Type: "api_key",
			APIKey: &coderv1alpha1.CoderMCPServerAPIKeyAuth{
				Header:    "Authorization",
				SecretRef: coderv1alpha1.SecretKeySelector{Name: "github-mcp", Key: "token"},
			},
		},
	}
}

func mcpHeaders(names ...string) []coderv1alpha1.CoderMCPServerHeader {
	headers := make([]coderv1alpha1.CoderMCPServerHeader, 0, len(names))
	for _, name := range names {
		headers = append(headers, coderv1alpha1.CoderMCPServerHeader{
			Name:      name,
			SecretRef: coderv1alpha1.SecretKeySelector{Name: "tracker-mcp", Key: strings.ToLower(name)},
		})
	}
	return headers
}

func validMCPOAuth2() *coderv1alpha1.CoderMCPServerOAuth2Auth {
	return &coderv1alpha1.CoderMCPServerOAuth2Auth{
		ClientID:        "coder-agents",
		ClientSecretRef: coderv1alpha1.SecretKeySelector{Name: "tracker-oauth", Key: "client-secret"},
		AuthURL:         "https://auth.example.com/oauth/authorize",
		TokenURL:        "https://auth.example.com/oauth/token",
	}
}

func useCustomHeaders(s *coderv1alpha1.CoderMCPServerSpec, names ...string) {
	s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "custom_headers", CustomHeaders: mcpHeaders(names...)}
}

func useOAuth2(s *coderv1alpha1.CoderMCPServerSpec, mutate func(*coderv1alpha1.CoderMCPServerOAuth2Auth)) {
	oauth2 := validMCPOAuth2()
	if mutate != nil {
		mutate(oauth2)
	}
	s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "oauth2", OAuth2: oauth2}
}

func TestCoderMCPServerAPIValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	manyHeaders := make([]string, 17)
	for i := range manyHeaders {
		manyHeaders[i] = fmt.Sprintf("X-Header-%d", i)
	}
	manyGroups := make([]string, 65)
	for i := range manyGroups {
		manyGroups[i] = fmt.Sprintf("group-%d", i)
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*coderv1alpha1.CoderMCPServerSpec)
		wantErr string // empty: the API server accepts the object
	}{
		{name: "api key auth"},
		{name: "no auth", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "none"}
		}},
		{name: "full spec", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Slug, s.Organization = strings.Repeat("a", 32), "Data-Science-2"
			s.DisplayName, s.Description = strings.Repeat("d", 128), "Issues and\npull requests"
			s.IconURL, s.URL, s.Transport, s.Availability = "/icon/github.svg", "http://[fd00::1]:8080/mcp?x=1", "sse", "force_on"
			s.Enabled, s.AllowInPlanMode, s.ModelIntent, s.ForwardCoderHeaders = ptrTo(false), true, true, true
			s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{"Everyone", "data-science"}}
		}},
		{name: "custom headers, a backtick in a name", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useCustomHeaders(s, "X-Api-Key", "X-Tenant", "x`odd")
		}},
		{name: "oauth2 with scopes and a revocation URL", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) {
				o.RevocationURL, o.Scopes = "https://auth.example.com/oauth/revoke", []string{"read", "repo:status"}
			})
		}},
		{name: "empty control plane name", wantErr: "spec.controlPlaneRef.name", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.ControlPlaneRef.Name = ""
		}},
		{name: "organization with an underscore", wantErr: "spec.organization", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Organization = "data_science"
		}},
		{name: "organization with 33 characters", wantErr: "spec.organization", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Organization = strings.Repeat("o", 33)
		}},
		{name: "slug with 33 characters", wantErr: "spec.slug", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Slug = strings.Repeat("a", 33)
		}},
		{name: "slug in upper case", wantErr: "spec.slug", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Slug = "GitHub"
		}},
		{name: "slug with an underscore", wantErr: "spec.slug", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Slug = "git_hub"
		}},
		{name: "empty display name", wantErr: "spec.displayName", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.DisplayName = ""
		}},
		{name: "display name with a trailing space", wantErr: "spec.displayName", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.DisplayName = "GitHub "
		}},
		{name: "display name with inner Unicode spaces", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.DisplayName = "Git\u00a0Hub\u2003Enterprise"
		}},
		{name: "display name with a trailing no-break space", wantErr: "spec.displayName", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.DisplayName = "GitHub\u00a0"
		}},
		{name: "display name with a leading vertical tab", wantErr: "spec.displayName", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.DisplayName = "\vGitHub"
		}},
		{name: "description with a leading em space", wantErr: "spec.description", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Description = "\u2003Issues"
		}},
		{name: "description with a trailing next-line character", wantErr: "spec.description", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Description = "Issues\u0085"
		}},
		{name: "icon URL with a trailing ideographic space", wantErr: "spec.iconURL", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.IconURL = "/icon/github.svg\u3000"
		}},
		{name: "URL with a trailing no-break space", wantErr: "spec.url", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.URL = "https://mcp.example.com/mcp\u00a0"
		}},
		{name: "oauth2 client ID with a trailing line separator", wantErr: "spec.auth.oauth2.clientID", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.ClientID = "coder-agents\u2028" })
		}},
		{name: "description with a leading newline", wantErr: "spec.description", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Description = "\nIssues"
		}},
		{name: "icon URL with a space", wantErr: "spec.iconURL", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.IconURL = "/icon/git hub.svg"
		}},
		{name: "URL with another scheme", wantErr: "url must be an http:// or https:// URL", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.URL = "ftp://mcp.example.com/mcp"
		}},
		{name: "URL with user info", wantErr: "url must be an http:// or https:// URL", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.URL = "https://user:pass@mcp.example.com/mcp"
		}},
		{name: "URL with a malformed escape", wantErr: "url must be a valid URL with a host", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.URL = "https://mcp.example.com/%zz"
		}},
		{name: "URL with a trailing space", wantErr: "spec.url", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.URL = "https://mcp.example.com/mcp "
		}},
		{name: "URL too long", wantErr: "spec.url", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.URL = "https://mcp.example.com/" + strings.Repeat("m", 2025)
		}},
		{name: "unknown transport", wantErr: "spec.transport", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Transport = "websocket"
		}},
		{name: "unknown availability", wantErr: "spec.availability", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Availability = "always"
		}},
		{name: "user_oidc auth", wantErr: "spec.auth.type", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "user_oidc"}
		}},
		{name: "api key type without its block", wantErr: "auth.apiKey must be set exactly when auth.type is api_key", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth.APIKey = nil
		}},
		{name: "no auth with an api key block", wantErr: "auth.apiKey must be set exactly when auth.type is api_key", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth.Type = "none"
		}},
		{name: "api key type with an extra headers block", wantErr: "auth.customHeaders must be set exactly when auth.type is custom_headers", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth.CustomHeaders = mcpHeaders("X-Api-Key")
		}},
		{name: "oauth2 type without its block", wantErr: "auth.oauth2 must be set exactly when auth.type is oauth2", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "oauth2"}
		}},
		{name: "api key header Host", wantErr: "header is reserved", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth.APIKey.Header = "Host"
		}},
		{name: "api key header with a space", wantErr: "spec.auth.apiKey.header", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth.APIKey.Header = "X Api Key"
		}},
		{name: "api key Secret without a key", wantErr: "secretRef.key must not be empty", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth.APIKey.SecretRef.Key = ""
		}},
		{name: "api key Secret without a name", wantErr: "secretRef.name must not be empty", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth.APIKey.SecretRef.Name = ""
		}},
		{name: "custom header with an MCP name in another case", wantErr: "header is reserved", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useCustomHeaders(s, "MCP-Session-ID")
		}},
		{name: "custom header with a Coder identity name", wantErr: "header is reserved", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useCustomHeaders(s, "X-Coder-Owner-Id")
		}},
		{name: "custom header names that differ only in case", wantErr: "names must be unique, ignoring case", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useCustomHeaders(s, "X-Api-Key", "x-api-key")
		}},
		{name: "17 custom headers", wantErr: "spec.auth.customHeaders", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useCustomHeaders(s, manyHeaders...)
		}},
		{name: "empty custom headers list", wantErr: "auth.customHeaders must be set exactly when auth.type is custom_headers", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "custom_headers", CustomHeaders: []coderv1alpha1.CoderMCPServerHeader{}}
		}},
		{name: "oauth2 client ID with a space", wantErr: "spec.auth.oauth2.clientID", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.ClientID = "coder agents" })
		}},
		{name: "oauth2 client secret without a key", wantErr: "clientSecretRef.key must not be empty", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.ClientSecretRef.Key = "" })
		}},
		{name: "oauth2 token URL with user info", wantErr: "tokenURL must be an http:// or https:// URL", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.TokenURL = "https://u:p@auth.example.com/token" })
		}},
		{name: "oauth2 auth URL without a host", wantErr: "authURL must be a valid URL with a host", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.AuthURL = "https://:8443/authorize" })
		}},
		{name: "oauth2 http revocation URL", wantErr: "revocationURL must be an https:// URL", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.RevocationURL = "http://auth.example.com/revoke" })
		}},
		{name: "oauth2 scope with a space", wantErr: "spec.auth.oauth2.scopes", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.Scopes = []string{"read write"} })
		}},
		{name: "access group with an underscore", wantErr: "spec.access.groups", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{"data_science"}}
		}},
		{name: "65 access groups", wantErr: "spec.access.groups", mutate: func(s *coderv1alpha1.CoderMCPServerSpec) {
			s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: manyGroups}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := validMCPServerSpec()
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			// Each case gets its own namespace, so cases can share a name.
			obj := &coderv1alpha1.CoderMCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: "mcp", Namespace: createTestNamespace(ctx, t, "mcp-api")},
				Spec:       spec,
			}
			err := k8sClient.Create(ctx, obj)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestCoderMCPServerDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	obj := &coderv1alpha1.CoderMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: createTestNamespace(ctx, t, "mcp-defaults")},
		Spec:       validMCPServerSpec(),
	}
	require.NoError(t, k8sClient.Create(ctx, obj))
	require.Equal(t, ptrTo(true), obj.Spec.Enabled)
	require.Equal(t, "streamable_http", obj.Spec.Transport)
	require.Equal(t, "default_off", obj.Spec.Availability)
	require.Nil(t, obj.Spec.Access, "an omitted access block must stay omitted")
}

func TestCoderMCPServerImmutableFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	obj := &coderv1alpha1.CoderMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: createTestNamespace(ctx, t, "mcp-immutable")},
		Spec:       validMCPServerSpec(),
	}
	require.NoError(t, k8sClient.Create(ctx, obj))

	for _, tc := range []struct {
		mutate  func(*coderv1alpha1.CoderMCPServerSpec)
		wantErr string
	}{
		{func(s *coderv1alpha1.CoderMCPServerSpec) { s.ControlPlaneRef.Name = "other" }, "controlPlaneRef is immutable"},
		{func(s *coderv1alpha1.CoderMCPServerSpec) { s.Organization = "other" }, "organization is immutable"},
		{func(s *coderv1alpha1.CoderMCPServerSpec) { s.Slug = "gitlab" }, "slug is immutable"},
	} {
		changed := obj.DeepCopy()
		tc.mutate(&changed.Spec)
		require.ErrorContains(t, k8sClient.Update(ctx, changed), tc.wantErr)
	}

	// The other spec fields, metadata, and status stay writable. enabled: false
	// and an empty access list must survive the round trip: both differ from
	// the omitted defaults.
	obj.Spec.URL, obj.Spec.DisplayName, obj.Spec.Enabled = "https://mcp2.example.com/mcp", "GitHub Enterprise", ptrTo(false)
	obj.Spec.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{}}
	useCustomHeaders(&obj.Spec, "X-Api-Key")
	obj.Finalizers = []string{coderv1alpha1.CoderMCPServerCleanupFinalizer}
	require.NoError(t, k8sClient.Update(ctx, obj))
	require.Equal(t, ptrTo(false), obj.Spec.Enabled, "enabled: false must not turn back into the default")
	require.NotNil(t, obj.Spec.Access, "access: {} must stay present, so the controller keeps owning the access list")
	require.Empty(t, obj.Spec.Access.Groups)

	obj.Status.ConfigID = "9b2a6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"
	obj.Status.Credentials = []coderv1alpha1.CoderMCPServerCredentialStatus{
		{Field: "header:X-Api-Key", SecretName: "tracker-mcp", SecretKey: "x-api-key"},
	}
	require.NoError(t, k8sClient.Status().Update(ctx, obj))

	obj.Finalizers = nil
	require.NoError(t, k8sClient.Update(ctx, obj))
	require.NoError(t, k8sClient.Delete(ctx, obj))
}
