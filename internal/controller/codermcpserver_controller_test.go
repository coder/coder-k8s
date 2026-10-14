package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/controller"
	"github.com/coder/coder-k8s/internal/credentialsecret"
	"github.com/coder/coder-k8s/internal/credentialsecret/credentialsecrettest"
)

// Synthetic credential values. The leak scan looks for each of them.
const (
	testMCPKey          = "mcp-api-key-5c1e9b7a"       // #nosec G101 -- test fixture, not a credential.
	testMCPTenant       = "mcp-tenant-2d8f4e61"        // #nosec G101 -- test fixture, not a credential.
	testMCPClientSecret = "mcp-client-secret-9a3b7c0d" // #nosec G101 -- test fixture, not a credential.
	testMCPOrigin       = "https://mcp.example.com"
	testMCPAuthOrigin   = "https://auth.example.com"
)

var testMCPValues = []string{testMCPKey, testMCPTenant, testMCPClientSecret}

// mcpServerEnv is one namespace with a fake Coder that has the organization
// coder and, optionally, a ready control plane named coder that points at it
// and lets CoderMCPServer objects target that organization.
type mcpServerEnv struct {
	namespace    string
	fake         *fakeCoder
	orgID        uuid.UUID // The Coder organization coder.
	r            *controller.CoderMCPServerReconciler
	controlPlane *coderv1alpha1.CoderControlPlane
}

func newMCPServerEnv(ctx context.Context, t *testing.T, withControlPlane bool) mcpServerEnv {
	t.Helper()
	env := mcpServerEnv{namespace: createTestNamespace(ctx, t, "mcp"), fake: newFakeCoder(t)}
	env.orgID = env.fake.addOrganization("coder")
	env.r = &controller.CoderMCPServerReconciler{Client: k8sClient, Scheme: scheme}
	if withControlPlane {
		env.controlPlane = createTestControlPlane(ctx, t, env.namespace, "coder", env.fake.server.URL)
		env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}})
	}
	return env
}

func (env mcpServerEnv) setOptIn(ctx context.Context, t *testing.T, optIn *coderv1alpha1.MCPServersSpec) {
	t.Helper()
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(env.controlPlane), env.controlPlane))
	env.controlPlane.Spec.MCPServers = optIn
	require.NoError(t, k8sClient.Update(ctx, env.controlPlane))
}

// secret creates a Secret with data that opts in to origins.
func (env mcpServerEnv) secret(ctx context.Context, t *testing.T, name string, data map[string]string, origins ...string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{ObjectMeta: consentedObjectMeta(name, env.namespace, origins...), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return createSecret(ctx, t, s)
}

// keySecret creates the API key Secret of validMCPServerSpec.
func (env mcpServerEnv) keySecret(ctx context.Context, t *testing.T, value string) *corev1.Secret {
	t.Helper()
	return env.secret(ctx, t, "github-mcp", map[string]string{"token": value}, testMCPOrigin)
}

func (env mcpServerEnv) create(ctx context.Context, t *testing.T, mutate func(*coderv1alpha1.CoderMCPServerSpec)) *coderv1alpha1.CoderMCPServer {
	t.Helper()
	spec := validMCPServerSpec()
	if mutate != nil {
		mutate(&spec)
	}
	server := &coderv1alpha1.CoderMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: env.namespace}, Spec: spec}
	require.NoError(t, k8sClient.Create(ctx, server))
	t.Cleanup(func() {
		_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(server), server)
		server.Finalizers = nil
		_ = k8sClient.Update(context.Background(), server)
		_ = k8sClient.Delete(context.Background(), server)
	})
	return server
}

// reconcile runs one reconcile with captured logs, checks that neither the
// status, the annotations, the logs, nor the error holds a credential, and
// returns the stored object.
func (env mcpServerEnv) reconcile(ctx context.Context, t *testing.T, server *coderv1alpha1.CoderMCPServer) (*coderv1alpha1.CoderMCPServer, ctrl.Result) {
	t.Helper()
	logCtx, logs := capturingContext(ctx)
	result, err := env.r.Reconcile(logCtx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)})
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	got := &coderv1alpha1.CoderMCPServer{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), got))
	credentialsecrettest.RequireNoLeak(t, testMCPValues, map[string]any{"status": got.Status, "annotations": got.Annotations, "logs": logs.String(), "error": errText})
	require.NoError(t, err)
	return got, result
}

// mcpRequests counts the fake's requests other than organization lookups.
func (env mcpServerEnv) mcpRequests() int {
	return env.fake.totalRequests() - env.fake.requestCount(routeOrganization)
}

// requireMCPServerStep checks the kstatus conditions of a wait, a stall, or
// the ready reason Applied, and returns the message.
func requireMCPServerStep(t *testing.T, server *coderv1alpha1.CoderMCPServer, stalled bool, reason string) string {
	t.Helper()
	want := map[string]metav1.ConditionStatus{"Ready": metav1.ConditionFalse, "Reconciling": metav1.ConditionTrue, "Stalled": metav1.ConditionFalse}
	switch {
	case reason == "Applied":
		require.False(t, stalled)
		want["Ready"], want["Reconciling"] = metav1.ConditionTrue, metav1.ConditionFalse
		require.Equal(t, server.Generation, server.Status.AppliedGeneration)
	case stalled:
		want["Reconciling"], want["Stalled"] = metav1.ConditionFalse, metav1.ConditionTrue
	}
	for conditionType, status := range want {
		condition := meta.FindStatusCondition(server.Status.Conditions, conditionType)
		require.NotNil(t, condition, conditionType)
		require.Equal(t, status, condition.Status, conditionType)
		require.Equal(t, reason, condition.Reason, conditionType)
		require.Equal(t, server.Generation, condition.ObservedGeneration)
	}
	require.Equal(t, server.Generation, server.Status.ObservedGeneration)
	return meta.FindStatusCondition(server.Status.Conditions, "Ready").Message
}

// Plan tests S1, S2, S10 and S12 (read side) for each credential field and
// destination: every failure stalls with zero MCP requests, and usable
// credentials reach the slug lookup.
func TestCoderMCPServerCredentialChecks(t *testing.T) {
	t.Parallel()
	apiKey := func(value string, origins ...string) func(context.Context, *testing.T, mcpServerEnv) {
		return func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.secret(ctx, t, "github-mcp", map[string]string{"token": value}, origins...)
		}
	}
	headers := func(tenantOrigins ...string) func(context.Context, *testing.T, mcpServerEnv) {
		return func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.secret(ctx, t, "tracker-mcp", map[string]string{"x-api-key": testMCPKey}, testMCPOrigin)
			env.secret(ctx, t, "tenant-mcp", map[string]string{"x-tenant": testMCPTenant}, tenantOrigins...)
		}
	}
	useHeaders := func(s *coderv1alpha1.CoderMCPServerSpec) {
		useCustomHeaders(s, "X-Api-Key", "X-Tenant")
		s.Auth.CustomHeaders[1].SecretRef.Name = "tenant-mcp"
	}
	oauth2 := func(value string, origins ...string) func(context.Context, *testing.T, mcpServerEnv) {
		return func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.secret(ctx, t, "tracker-oauth", map[string]string{"client-secret": value}, origins...)
		}
	}
	useOAuth2Revocation := func(revocation string) func(*coderv1alpha1.CoderMCPServerSpec) {
		return func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.RevocationURL = revocation })
		}
	}
	withURL := func(u string) func(*coderv1alpha1.CoderMCPServerSpec) {
		return func(s *coderv1alpha1.CoderMCPServerSpec) { s.URL = u }
	}
	modify := func(name string, edit func(*corev1.Secret)) func(context.Context, *testing.T, mcpServerEnv) {
		return func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			s := &corev1.Secret{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: name}, s))
			// Delete and create again, because a Secret's type is immutable.
			require.NoError(t, k8sClient.Delete(ctx, s))
			edit(s)
			s.ResourceVersion, s.UID = "", ""
			require.NoError(t, k8sClient.Create(ctx, s))
		}
	}
	notOpaque := func(s *corev1.Secret) { s.Type = "example.com/api-key" }
	ownedByCoder := func(s *corev1.Secret) {
		s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "coder.com/v1alpha1", Kind: "CoderControlPlane", Name: "coder", UID: "0b6f6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"}}
	}
	for _, tc := range []struct {
		name    string
		spec    func(*coderv1alpha1.CoderMCPServerSpec)
		secrets []func(context.Context, *testing.T, mcpServerEnv)
		reason  string // Empty: the credentials are usable and the slug lookup runs.
		message string // A part of the message.
	}{
		{name: "api key", secrets: setups(apiKey(testMCPKey, testMCPOrigin))},
		{name: "no credential", spec: func(s *coderv1alpha1.CoderMCPServerSpec) { s.Auth = coderv1alpha1.CoderMCPServerAuth{Type: "none"} }},
		{name: "uppercase host", spec: withURL("https://MCP.Example.com/github/mcp"), secrets: setups(apiKey(testMCPKey, testMCPOrigin))},
		{name: "explicit default port", spec: withURL("https://mcp.example.com:443/github/mcp"), secrets: setups(apiKey(testMCPKey, testMCPOrigin))},
		{name: "IPv6 host", spec: withURL("https://[FD00::1]:8443/mcp"), secrets: setups(apiKey(testMCPKey, "https://[fd00::1]:8443"))},
		{name: "IPv6 host, other port", spec: withURL("https://[fd00::1]/mcp"), secrets: setups(apiKey(testMCPKey, "https://[fd00::1]:8443")), reason: credentialsecret.ReasonDestinationNotAllowed},
		{name: "other port", spec: withURL("https://mcp.example.com:8443/mcp"), secrets: setups(apiKey(testMCPKey, testMCPOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed},
		{name: "other scheme", spec: withURL("http://mcp.example.com/mcp"), secrets: setups(apiKey(testMCPKey, testMCPOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed},
		{name: "subdomain", spec: withURL("https://eu.mcp.example.com/mcp"), secrets: setups(apiKey(testMCPKey, testMCPOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed},
		{name: "trailing dot", spec: withURL("https://mcp.example.com./mcp"), secrets: setups(apiKey(testMCPKey, testMCPOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed},
		{name: "user info in the destinations", secrets: setups(apiKey(testMCPKey, "https://user@mcp.example.com")), reason: credentialsecret.ReasonMalformed},
		{name: "missing Secret", reason: credentialsecret.ReasonSecretNotFound, message: "apiKey (to spec.url): "},
		{name: "missing key", secrets: setups(func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.secret(ctx, t, "github-mcp", map[string]string{"other": testMCPKey}, testMCPOrigin)
		}), reason: credentialsecret.ReasonKeyMissing},
		{name: "no label", secrets: setups(apiKey(testMCPKey, testMCPOrigin), modify("github-mcp", func(s *corev1.Secret) { s.Labels = nil })), reason: credentialsecret.ReasonNotOptedIn},
		{name: "no destinations", secrets: setups(apiKey(testMCPKey, testMCPOrigin), modify("github-mcp", func(s *corev1.Secret) { s.Annotations = nil })), reason: credentialsecret.ReasonNotOptedIn},
		{name: "api key, not Opaque", secrets: setups(apiKey(testMCPKey, testMCPOrigin), modify("github-mcp", notOpaque)), reason: credentialsecret.ReasonRefused},
		{name: "api key, owned by a coder.com object", secrets: setups(apiKey(testMCPKey, testMCPOrigin), modify("github-mcp", ownedByCoder)), reason: credentialsecret.ReasonRefused},
		{name: "empty value", secrets: setups(apiKey("", testMCPOrigin)), reason: credentialsecret.ReasonMalformed},
		{name: "padded value", secrets: setups(apiKey(testMCPKey+"\n", testMCPOrigin)), reason: credentialsecret.ReasonMalformed},
		{name: "oversized value", secrets: setups(apiKey(testMCPKey+strings.Repeat("k", credentialsecret.MaxValueBytes), testMCPOrigin)), reason: credentialsecret.ReasonMalformed},
		{name: "CR in the api key", secrets: setups(apiKey("Bearer "+testMCPKey+"\rX", testMCPOrigin)), reason: credentialsecret.ReasonMalformed, message: "not a valid HTTP header value"},
		{name: "LF in the api key", secrets: setups(apiKey(testMCPKey+"\nX: y", testMCPOrigin)), reason: credentialsecret.ReasonMalformed, message: "not a valid HTTP header value"},
		{name: "NUL in the api key", secrets: setups(apiKey(testMCPKey+"\x00x", testMCPOrigin)), reason: credentialsecret.ReasonMalformed, message: "not a valid HTTP header value"},
		{name: "non-ASCII UTF-8 in the api key", secrets: setups(apiKey(testMCPKey+"caf\xc3\xa9\tx", testMCPOrigin))},
		{name: "invalid UTF-8 in the api key", secrets: setups(apiKey(testMCPKey+"\xe9x", testMCPOrigin)), reason: credentialsecret.ReasonMalformed, message: "apiKey: the value of key token in Secret github-mcp is not valid UTF-8."},
		{name: "headers", spec: useHeaders, secrets: setups(headers(testMCPOrigin))},
		{name: "header, unlisted origin", spec: useHeaders, secrets: setups(headers(testMCPAuthOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed, message: "header:X-Tenant (to spec.url): "},
		{name: "header, not Opaque", spec: useHeaders, secrets: setups(headers(testMCPOrigin), modify("tenant-mcp", notOpaque)), reason: credentialsecret.ReasonRefused},
		{name: "header, CR in the value", spec: useHeaders, secrets: setups(func(ctx context.Context, t *testing.T, env mcpServerEnv) {
			env.secret(ctx, t, "tracker-mcp", map[string]string{"x-api-key": testMCPKey}, testMCPOrigin)
			env.secret(ctx, t, "tenant-mcp", map[string]string{"x-tenant": testMCPTenant + "\r\nX: y"}, testMCPOrigin)
		}), reason: credentialsecret.ReasonMalformed, message: "header:X-Tenant: "},
		{name: "oauth2", spec: useOAuth2Revocation(""), secrets: setups(oauth2(testMCPClientSecret, testMCPOrigin, testMCPAuthOrigin))},
		{name: "oauth2, CR in the client secret", spec: useOAuth2Revocation(""), secrets: setups(oauth2(testMCPClientSecret+"\rx", testMCPOrigin, testMCPAuthOrigin))},
		{name: "oauth2, invalid UTF-8 in the client secret", spec: useOAuth2Revocation(""), secrets: setups(oauth2(testMCPClientSecret+"\xffx", testMCPOrigin, testMCPAuthOrigin)), reason: credentialsecret.ReasonMalformed, message: "oauth2ClientSecret: "},
		{name: "oauth2, url not listed", spec: useOAuth2Revocation(""), secrets: setups(oauth2(testMCPClientSecret, testMCPAuthOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed, message: "oauth2ClientSecret (to spec.url): "},
		{name: "oauth2, auth origin not listed", spec: useOAuth2Revocation(""), secrets: setups(oauth2(testMCPClientSecret, testMCPOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed, message: "oauth2ClientSecret (to spec.auth.oauth2.authURL): "},
		{name: "oauth2, token origin not listed", spec: func(s *coderv1alpha1.CoderMCPServerSpec) {
			useOAuth2(s, func(o *coderv1alpha1.CoderMCPServerOAuth2Auth) { o.TokenURL = "https://token.example.net/t" })
		}, secrets: setups(oauth2(testMCPClientSecret, testMCPOrigin, testMCPAuthOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed, message: "(to spec.auth.oauth2.tokenURL): "},
		{name: "oauth2, revocation origin not listed", spec: useOAuth2Revocation("https://revoke.example.net/r"), secrets: setups(oauth2(testMCPClientSecret, testMCPOrigin, testMCPAuthOrigin)), reason: credentialsecret.ReasonDestinationNotAllowed, message: "(to spec.auth.oauth2.revocationURL): "},
		{name: "oauth2, revocation origin listed", spec: useOAuth2Revocation("https://revoke.example.net/r"), secrets: setups(oauth2(testMCPClientSecret, testMCPOrigin, testMCPAuthOrigin, "https://revoke.example.net"))},
		{name: "oauth2, not Opaque", spec: useOAuth2Revocation(""), secrets: setups(oauth2(testMCPClientSecret, testMCPOrigin, testMCPAuthOrigin), modify("tracker-oauth", notOpaque)), reason: credentialsecret.ReasonRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			for _, setup := range tc.secrets {
				setup(ctx, t, env)
			}
			got, result := env.reconcile(ctx, t, env.create(ctx, t, tc.spec))
			if tc.reason == "" {
				requireMCPServerStep(t, got, false, "Applied")
				require.Equal(t, 1, env.fake.requestCount(routeMCPServers), "usable credentials reach the slug lookup")
				require.Equal(t, 1, env.fake.requestCount(routeCreateMCPServer))
				require.Equal(t, 1, env.fake.requestCount(routeUpdateMCPServer))
				require.Equal(t, 4, env.mcpRequests(), "list, create, get and update")
				require.Equal(t, 10*time.Minute, result.RequeueAfter)
				return
			}
			require.Contains(t, requireMCPServerStep(t, got, true, tc.reason), tc.message)
			require.Zero(t, env.mcpRequests(), "a credential failure sends no MCP request")
			require.Zero(t, result.RequeueAfter, "the Secret watch requeues the object")
		})
	}
}

// setups lists the Secret setup steps of a test case.
func setups(items ...func(context.Context, *testing.T, mcpServerEnv)) []func(context.Context, *testing.T, mcpServerEnv) {
	return items
}

// Plan S1: a control plane's operator token, license or database Secret is
// never a credential, also when it opts in.
func TestCoderMCPServerRefusesControlPlaneSecrets(t *testing.T) {
	t.Parallel()
	for _, role := range append([]string{"operator token"}, controlPlaneSecretRoles...) {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			secretName, secretKey := "github-mcp", "token" // #nosec G101 -- a Secret name, not a credential.
			if role == "operator token" {
				secretName, secretKey = "coder-operator-token", coderv1alpha1.DefaultTokenSecretKey
				s := &corev1.Secret{}
				require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: env.namespace, Name: secretName}, s))
				s.Labels = map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue}
				s.Annotations = map[string]string{credentialsecret.DestinationsAnnotation: testMCPOrigin}
				require.NoError(t, k8sClient.Update(ctx, s))
			} else {
				env.keySecret(ctx, t, testMCPKey)
				createControlPlaneUsing(ctx, t, env.namespace, role, secretName)
			}
			got, _ := env.reconcile(ctx, t, env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
				s.Auth.APIKey.SecretRef = coderv1alpha1.SecretKeySelector{Name: secretName, Key: secretKey}
			}))
			requireMCPServerStep(t, got, true, credentialsecret.ReasonRefused)
			require.Zero(t, env.mcpRequests())
		})
	}
}

// A missing control plane sends nothing to Coder. Plan S17: an organization
// outside the opt-in, or force_on without allowForceOn, sends no MCP request.
func TestCoderMCPServerControlPlaneAndOptIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, false)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, nil)
	got, result := env.reconcile(ctx, t, server)
	require.Equal(t, "CoderControlPlane coder does not exist.", requireMCPServerStep(t, got, false, "ControlPlaneNotReady"))
	require.Equal(t, 30*time.Second, result.RequeueAfter)
	require.Zero(t, env.fake.totalRequests())

	env.controlPlane = createTestControlPlane(ctx, t, env.namespace, "coder", env.fake.server.URL)
	env.fake.addOrganization("other")
	for _, tc := range []struct {
		optIn   *coderv1alpha1.MCPServersSpec
		reason  string
		lookups int // Organization lookups of this reconcile.
	}{
		{optIn: nil, reason: "MCPServersNotAllowed", lookups: 1},
		{optIn: &coderv1alpha1.MCPServersSpec{Organizations: []string{"other", "missing"}}, reason: "MCPServersNotAllowed", lookups: 3},
		{optIn: &coderv1alpha1.MCPServersSpec{Organizations: []string{"CODER"}}, reason: "Applied", lookups: 1},
		{optIn: &coderv1alpha1.MCPServersSpec{Organizations: []string{"other", "coder"}}, reason: "Applied", lookups: 2},
	} {
		env.setOptIn(ctx, t, tc.optIn)
		orgLookups, mcp := env.fake.requestCount(routeOrganization), env.mcpRequests()
		got, _ = env.reconcile(ctx, t, server)
		requireMCPServerStep(t, got, tc.reason != "Applied", tc.reason)
		require.Equal(t, tc.lookups, env.fake.requestCount(routeOrganization)-orgLookups, tc.reason)
		if tc.reason != "Applied" {
			require.Equal(t, mcp, env.mcpRequests(), "only organization lookups happen")
		}
	}

	// A failed lookup of a listed name is a wait, not a refusal.
	env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"other", "coder"}})
	env.fake.failNext(routeOrganization, fakeFault{Rewrite: func(answer any) any { return answer }}) // The object's own organization.
	env.fake.failNext(routeOrganization, fakeFault{Status: 500})
	got, result = env.reconcile(ctx, t, server)
	require.Equal(t, "get organization other: Coder answered 500: fake fault 500", requireMCPServerStep(t, got, false, "CoderUnavailable"))
	require.Equal(t, 30*time.Second, result.RequeueAfter)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), server))
	server.Spec.Availability = "force_on"
	require.NoError(t, k8sClient.Update(ctx, server))
	mcp := env.mcpRequests()
	got, _ = env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, true, "ForceOnNotAllowed")
	require.Equal(t, mcp, env.mcpRequests())
	env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}, AllowForceOn: true})
	got, _ = env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, false, "Applied")
}

// The organization ID is pinned at the first resolution. A later resolution
// to another ID stalls with no MCP request.
func TestCoderMCPServerOrganizationPinning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.keySecret(ctx, t, testMCPKey)
	server := env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) { s.Organization = "data" })
	env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"data"}})
	got, result := env.reconcile(ctx, t, server)
	require.Equal(t, "Coder organization data does not exist.", requireMCPServerStep(t, got, false, "OrganizationNotFound"))
	require.Equal(t, 10*time.Minute, result.RequeueAfter)
	require.Empty(t, got.Status.OrganizationID)

	first := env.fake.addOrganization("data")
	got, _ = env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, false, "Applied")
	require.Equal(t, first.String(), got.Status.OrganizationID)

	// The organization is deleted and another one takes its name.
	env.fake.mu.Lock()
	delete(env.fake.orgs, first)
	env.fake.mu.Unlock()
	second := env.fake.addOrganization("data")
	mcp := env.mcpRequests()
	got, _ = env.reconcile(ctx, t, server)
	message := requireMCPServerStep(t, got, true, "OrganizationChanged")
	require.Contains(t, message, second.String())
	require.Equal(t, first.String(), got.Status.OrganizationID, "the pinned ID stays")
	require.Equal(t, mcp, env.mcpRequests())
}

// A config that this object did not create is a conflict (plan S4): the
// lookup sends no write. Coder errors never put Detail or a credential into
// status or logs.
func TestCoderMCPServerSlugLookup(t *testing.T) {
	t.Parallel()
	create := func(slug, displayName string) func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer) {
		return func(ctx context.Context, t *testing.T, env mcpServerEnv, server *coderv1alpha1.CoderMCPServer) {
			req := apiKeyMCPRequest(slug)
			if displayName == "" {
				displayName = "coder-k8s:" + string(server.UID)
			}
			req.DisplayName = displayName
			_, err := env.fake.client(t, 2*time.Second).CreateMCPServerConfig(ctx, env.orgID, req)
			require.NoError(t, err)
		}
	}
	for _, tc := range []struct {
		name    string
		prepare func(context.Context, *testing.T, mcpServerEnv, *coderv1alpha1.CoderMCPServer)
		stalled bool
		reason  string
		message string
		requeue time.Duration
	}{
		{
			name: "slug taken", prepare: create("github", "GitHub"), stalled: true, reason: "SlugConflict", requeue: 10 * time.Minute,
			message: "Coder organization coder already has an MCP server config with slug github (ID ",
		},
		{
			name: "Coder error with a credential in its detail", reason: "CoderUnavailable", requeue: 30 * time.Second,
			message: "list MCP server configs: Coder answered 500: fake fault 500",
			prepare: func(_ context.Context, _ *testing.T, env mcpServerEnv, _ *coderv1alpha1.CoderMCPServer) {
				for range 2 { // One fault per reconcile.
					env.fake.failNext(routeMCPServers, fakeFault{Status: 500, Detail: "upstream rejected " + testMCPKey})
				}
			},
		},
		{name: "answer for another organization", prepare: func(ctx context.Context, t *testing.T, env mcpServerEnv, server *coderv1alpha1.CoderMCPServer) {
			create("github", "GitHub")(ctx, t, env, server)
			otherOrg := uuid.New()
			for range 2 { // One fault per reconcile.
				env.fake.failNext(routeMCPServers, fakeFault{Rewrite: func(answer any) any {
					list := answer.([]codersdk.MCPServerConfig)
					list[0].OrganizationID = otherOrg
					return list
				}})
			}
		}, stalled: true, reason: "CoderAnswerMismatch", requeue: 10 * time.Minute, message: "assertion failed: Coder answered MCP server config organization"},
		{name: "matching config without an ID", prepare: func(ctx context.Context, t *testing.T, env mcpServerEnv, server *coderv1alpha1.CoderMCPServer) {
			create("github", "")(ctx, t, env, server)
			for range 2 { // One fault per reconcile.
				env.fake.failNext(routeMCPServers, fakeFault{Rewrite: func(answer any) any {
					list := answer.([]codersdk.MCPServerConfig)
					list[0].ID = uuid.Nil
					return list
				}})
			}
		}, stalled: true, reason: "CoderAnswerMismatch", requeue: 10 * time.Minute, message: "assertion failed: Coder answered the MCP server config with slug github without an ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newMCPServerEnv(ctx, t, true)
			env.keySecret(ctx, t, testMCPKey)
			server := env.create(ctx, t, nil)
			tc.prepare(ctx, t, env, server)
			before := env.mcpRequests()
			got, result := env.reconcile(ctx, t, server)
			require.Contains(t, requireMCPServerStep(t, got, tc.stalled, tc.reason), tc.message)
			require.Equal(t, tc.requeue, result.RequeueAfter)
			require.Equal(t, 1, env.mcpRequests()-before, "one list and no write")

			// A reconcile without a change sends no write and leaves the stored
			// status unchanged (no condition time flaps).
			again, _ := env.reconcile(ctx, t, got)
			require.Equal(t, got.ResourceVersion, again.ResourceVersion)
			require.Equal(t, 2, env.mcpRequests()-before)
		})
	}
}

// changingSecretClient touches a Secret after the reconciler first reads it,
// so the next read of the same Secret sees a new resourceVersion.
type changingSecretClient struct {
	client.Client
	name    string
	touched bool
}

func (c *changingSecretClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if s, ok := obj.(*corev1.Secret); ok && key.Name == c.name && !c.touched {
		c.touched = true
		touched := s.DeepCopy()
		touched.Annotations["example.com/touched"] = "true"
		return c.Update(ctx, touched)
	}
	return nil
}

// Plan 5.2 rule 2: the reads of one Secret in one reconcile must see the same
// Secret state. Otherwise the reconcile sends nothing and reads again.
func TestCoderMCPServerSecretChangesDuringReconcile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	env.secret(ctx, t, "tracker-oauth", map[string]string{"client-secret": testMCPClientSecret}, testMCPOrigin, testMCPAuthOrigin)
	server := env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) { useOAuth2(s, nil) })
	env.r.Client = &changingSecretClient{Client: k8sClient, name: "tracker-oauth"}
	got, result := env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, false, "CredentialSecretChanged")
	require.Equal(t, time.Second, result.RequeueAfter)
	require.Zero(t, env.mcpRequests())

	got, _ = env.reconcile(ctx, t, server)
	requireMCPServerStep(t, got, false, "Applied")
}

// Plan S15: access with no groups and enabled false survive the finalizer
// patch and the status write. Deletion releases the finalizer only when
// nothing of the object can exist in Coder.
func TestCoderMCPServerFinalizer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, false)
	server := env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
		s.Enabled = ptrTo(false)
		s.Access = &coderv1alpha1.CoderMCPServerAccess{Groups: []string{}}
	})
	got, _ := env.reconcile(ctx, t, server)
	require.Equal(t, []string{coderv1alpha1.CoderMCPServerCleanupFinalizer}, got.Finalizers)
	requireMCPServerStep(t, got, false, "ControlPlaneNotReady")
	require.Equal(t, ptrTo(false), got.Spec.Enabled)
	require.NotNil(t, got.Spec.Access, "access: {} means no group can read the config")
	require.Empty(t, got.Spec.Access.Groups)

	require.NoError(t, k8sClient.Delete(ctx, got))
	_, err := env.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(got), got)))

	// A config with a Coder ID keeps its finalizer: this version deletes nothing.
	kept := env.create(ctx, t, nil)
	kept, _ = env.reconcile(ctx, t, kept)
	kept.Status.ConfigID = "5d0c6c2e-3a4d-4c55-9d2f-3f1b1f0c9a11"
	require.NoError(t, k8sClient.Status().Update(ctx, kept))
	require.NoError(t, k8sClient.Delete(ctx, kept))
	kept, _ = env.reconcile(ctx, t, kept)
	require.Equal(t, []string{coderv1alpha1.CoderMCPServerCleanupFinalizer}, kept.Finalizers)
	requireMCPServerStep(t, kept, false, "DeletePending")
	require.Zero(t, env.fake.totalRequests())
}

// Plan S2 through the manager: a Secret update that removes the label, a
// Secret delete, and control plane changes each requeue the object at once.
func TestCoderMCPServerWatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMCPServerEnv(ctx, t, true)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{env.namespace: {}}},
		Controller: config.Controller{SkipNameValidation: ptrTo(true)},
	})
	require.NoError(t, err)
	require.NoError(t, (&controller.CoderMCPServerReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr))
	mgrCtx, cancel := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-mgrDone)
	})

	key := env.keySecret(ctx, t, testMCPKey)
	tenant := env.secret(ctx, t, "tenant-mcp", map[string]string{"x-tenant": testMCPTenant}, testMCPOrigin)
	server := env.create(ctx, t, func(s *coderv1alpha1.CoderMCPServerSpec) {
		useCustomHeaders(s, "X-Api-Key", "X-Tenant")
		s.Auth.CustomHeaders[0].SecretRef = coderv1alpha1.SecretKeySelector{Name: key.Name, Key: "token"}
		s.Auth.CustomHeaders[1].SecretRef = coderv1alpha1.SecretKeySelector{Name: tenant.Name, Key: "x-tenant"}
	})
	requireReason := func(reason string) {
		t.Helper()
		require.Eventually(t, func() bool {
			got := &coderv1alpha1.CoderMCPServer{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), got))
			condition := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			return condition != nil && condition.Reason == reason
		}, 30*time.Second, 100*time.Millisecond, reason)
	}
	requireReason("Applied")
	// A withdrawn consent disables the config in Coder (plan G8), so only a
	// spec edit applies it again.
	rearm := func(description string) {
		t.Helper()
		requireReason("DisabledExternally")
		got := &coderv1alpha1.CoderMCPServer{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(server), got))
		got.Spec.Description = description
		require.NoError(t, k8sClient.Update(ctx, got))
		requireReason("Applied")
	}

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(tenant), tenant))
	tenant.Labels = nil
	require.NoError(t, k8sClient.Update(ctx, tenant))
	requireReason(credentialsecret.ReasonNotOptedIn)
	tenant.Labels = map[string]string{credentialsecret.LabelKey: credentialsecret.LabelValue}
	require.NoError(t, k8sClient.Update(ctx, tenant))
	rearm("one")

	env.setOptIn(ctx, t, nil)
	requireReason("MCPServersNotAllowed")
	env.setOptIn(ctx, t, &coderv1alpha1.MCPServersSpec{Organizations: []string{"coder"}})
	requireReason("Applied")

	// Another control plane that uses a credential Secret for its own access
	// makes it unusable. Deleting that control plane makes it usable again.
	other := createControlPlaneUsing(ctx, t, env.namespace, "license", tenant.Name)
	requireReason(credentialsecret.ReasonRefused)
	require.NoError(t, k8sClient.Delete(ctx, &coderv1alpha1.CoderControlPlane{ObjectMeta: metav1.ObjectMeta{Name: other, Namespace: env.namespace}}))
	rearm("two")

	require.NoError(t, k8sClient.Delete(ctx, key))
	requireReason(credentialsecret.ReasonSecretNotFound)
}
