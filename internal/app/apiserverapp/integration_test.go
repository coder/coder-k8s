package apiserverapp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericoptions "k8s.io/apiserver/pkg/server/options"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

func TestIntegrationAggregatedAPIServerBootstrapAndList(t *testing.T) {
	t.Parallel()

	mockCoder := newIntegrationMockCoderServer("test-token")
	defer mockCoder.Close()

	mockCoderURLString := mockCoder.URL()
	mockCoderURL, err := url.Parse(mockCoderURLString)
	if err != nil {
		t.Fatalf("parse mock coder URL %q: %v", mockCoderURLString, err)
	}

	sdkClient := codersdk.New(mockCoderURL)
	if sdkClient == nil {
		t.Fatal("assertion failed: codersdk client must not be nil")
	}
	sdkClient.SetSessionToken("test-token")

	provider := &coder.StaticClientProvider{Client: sdkClient, Namespace: "test-ns"}
	if provider.Client == nil {
		t.Fatal("assertion failed: provider client must not be nil")
	}

	scheme := NewScheme()
	if scheme == nil {
		t.Fatal("assertion failed: scheme must not be nil")
	}
	codecs := serializer.NewCodecFactory(scheme)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("create aggregated API listener: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	secureServingOptions := genericoptions.NewSecureServingOptions()
	if secureServingOptions == nil {
		t.Fatal("assertion failed: secure serving options must not be nil")
	}
	secureServingOptions.Listener = listener
	secureServingOptions.BindPort = 0
	secureServingOptions.ServerCert.CertDirectory = ""
	secureServingOptions.ServerCert.PairName = ""

	recommendedConfig, err := NewRecommendedConfig(scheme, codecs, secureServingOptions)
	if err != nil {
		t.Fatalf("build recommended config: %v", err)
	}
	if recommendedConfig == nil {
		t.Fatal("assertion failed: recommended config must not be nil")
	}
	if recommendedConfig.LoopbackClientConfig == nil {
		t.Fatal("assertion failed: loopback client config must not be nil")
	}
	if recommendedConfig.LoopbackClientConfig.Host == "" {
		t.Fatal("assertion failed: loopback client host must not be empty")
	}

	server, err := NewGenericAPIServer(recommendedConfig)
	if err != nil {
		t.Fatalf("build generic API server: %v", err)
	}
	if server == nil {
		t.Fatal("assertion failed: generic API server must not be nil")
	}
	defer server.Destroy()

	apiGroupInfo, err := NewAPIGroupInfo(scheme, codecs, provider)
	if err != nil {
		t.Fatalf("build API group info: %v", err)
	}
	if apiGroupInfo == nil {
		t.Fatal("assertion failed: API group info must not be nil")
	}
	if err := InstallAPIGroup(server, apiGroupInfo); err != nil {
		t.Fatalf("install API group: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.PrepareRun().RunWithContext(ctx)
	}()
	defer func() {
		cancel()
		select {
		case runErr := <-errCh:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("aggregated API server exited with error: %v", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for aggregated API server to stop")
		}
	}()

	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			//nolint:gosec // Integration test uses ephemeral self-signed certs.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	baseURL := strings.TrimSuffix(recommendedConfig.LoopbackClientConfig.Host, "/")
	if baseURL == "" {
		t.Fatal("assertion failed: base URL must not be empty")
	}

	templateListURL := fmt.Sprintf(
		"%s/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/codertemplates",
		baseURL,
	)
	workspaceListURL := fmt.Sprintf(
		"%s/apis/aggregation.coder.com/v1alpha1/namespaces/test-ns/coderworkspaces",
		baseURL,
	)

	var templateList aggregationv1alpha1.CoderTemplateList
	mustGetJSONWithRetry(t, httpClient, errCh, templateListURL, &templateList)
	if len(templateList.Items) != 1 {
		t.Fatalf("expected 1 template, got %d", len(templateList.Items))
	}
	if got := templateList.Items[0].Name; got != "default.my-template" {
		t.Fatalf("expected template name default.my-template, got %q", got)
	}
	if got := templateList.Items[0].Namespace; got != "test-ns" {
		t.Fatalf("expected template namespace test-ns, got %q", got)
	}

	var workspaceList aggregationv1alpha1.CoderWorkspaceList
	mustGetJSONWithRetry(t, httpClient, errCh, workspaceListURL, &workspaceList)
	if len(workspaceList.Items) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(workspaceList.Items))
	}
	if got := workspaceList.Items[0].Name; got != "default.testuser.my-workspace" {
		t.Fatalf("expected workspace name default.testuser.my-workspace, got %q", got)
	}
	if got := workspaceList.Items[0].Namespace; got != "test-ns" {
		t.Fatalf("expected workspace namespace test-ns, got %q", got)
	}
}

func mustGetJSONWithRetry(t *testing.T, client *http.Client, errCh <-chan error, requestURL string, target any) {
	t.Helper()

	if client == nil {
		t.Fatal("assertion failed: HTTP client must not be nil")
	}
	if requestURL == "" {
		t.Fatal("assertion failed: request URL must not be empty")
	}
	if target == nil {
		t.Fatal("assertion failed: decode target must not be nil")
	}

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error

	for time.Now().Before(deadline) {
		select {
		case runErr := <-errCh:
			t.Fatalf("aggregated API server exited before request %q completed: %v", requestURL, runErr)
		default:
		}

		request, err := http.NewRequest(http.MethodGet, requestURL, nil)
		if err != nil {
			t.Fatalf("create request %q: %v", requestURL, err)
		}
		request.Header.Set("Accept", "application/json")

		response, err := client.Do(request)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}

		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil {
			t.Fatalf("read response body for %q: %v", requestURL, err)
		}
		if closeErr != nil {
			t.Fatalf("close response body for %q: %v", requestURL, closeErr)
		}

		if response.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, target); err != nil {
				t.Fatalf("decode response for %q: %v (body=%q)", requestURL, err, string(body))
			}
			return
		}

		lastErr = fmt.Errorf("unexpected status for %q: %d body=%s", requestURL, response.StatusCode, string(body))
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("request %q did not succeed before timeout: %v", requestURL, lastErr)
}

type integrationMockCoderServer struct {
	server *httptest.Server
}

func newIntegrationMockCoderServer(expectedSessionToken string) *integrationMockCoderServer {
	if expectedSessionToken == "" {
		panic("assertion failed: expected session token must not be empty")
	}

	organizationID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	templateID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	templateVersionID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	workspaceID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	workspaceBuildID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	now := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	organization := codersdk.Organization{
		MinimalOrganization: codersdk.MinimalOrganization{
			ID:   organizationID,
			Name: "default",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	template := codersdk.Template{
		ID:               templateID,
		Name:             "my-template",
		OrganizationName: "default",
		OrganizationID:   organizationID,
		ActiveVersionID:  templateVersionID,
		DisplayName:      "My Template",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	workspace := codersdk.Workspace{
		ID:               workspaceID,
		Name:             "my-workspace",
		OwnerName:        "testuser",
		OrganizationName: "default",
		OrganizationID:   organizationID,
		TemplateName:     "my-template",
		TemplateID:       templateID,
		CreatedAt:        now,
		UpdatedAt:        now,
		LastUsedAt:       now,
		LatestBuild: codersdk.WorkspaceBuild{
			ID:                workspaceBuildID,
			Transition:        codersdk.WorkspaceTransitionStart,
			Status:            codersdk.WorkspaceStatusRunning,
			TemplateVersionID: templateVersionID,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get(codersdk.SessionTokenHeader); token != expectedSessionToken {
			writeCoderError(w, http.StatusUnauthorized, fmt.Sprintf("unexpected session token %q", token))
			return
		}

		segments := splitPath(r.URL.Path)
		switch {
		case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "organizations") && len(segments) == 4:
			orgSegment := segments[3]
			if orgSegment != organization.Name && orgSegment != organization.ID.String() {
				writeCoderError(w, http.StatusNotFound, "organization not found")
				return
			}
			writeJSON(w, http.StatusOK, organization)
			return
		case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "templates") && len(segments) == 3:
			writeJSON(w, http.StatusOK, []codersdk.Template{template})
			return
		case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "organizations") && len(segments) == 6 && segments[4] == "templates":
			orgSegment := segments[3]
			templateSegment := segments[5]
			if orgSegment != organization.Name && orgSegment != organization.ID.String() {
				writeCoderError(w, http.StatusNotFound, "organization not found")
				return
			}
			if templateSegment != template.Name {
				writeCoderError(w, http.StatusNotFound, "template not found")
				return
			}
			writeJSON(w, http.StatusOK, template)
			return
		case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "workspaces") && len(segments) == 3:
			writeJSON(w, http.StatusOK, codersdk.WorkspacesResponse{Workspaces: []codersdk.Workspace{workspace}, Count: 1})
			return
		case r.Method == http.MethodGet && hasSegments(segments, "api", "v2", "users") && len(segments) == 6 && segments[4] == "workspace":
			ownerSegment := segments[3]
			workspaceSegment := segments[5]
			if ownerSegment != workspace.OwnerName || workspaceSegment != workspace.Name {
				writeCoderError(w, http.StatusNotFound, "workspace not found")
				return
			}
			writeJSON(w, http.StatusOK, workspace)
			return
		default:
			writeCoderError(w, http.StatusNotFound, fmt.Sprintf("unexpected route: %s %s", r.Method, r.URL.Path))
			return
		}
	}))

	return &integrationMockCoderServer{server: server}
}

func (s *integrationMockCoderServer) URL() string {
	if s == nil {
		panic("assertion failed: integration mock coder server must not be nil")
	}
	if s.server == nil {
		panic("assertion failed: integration mock coder server backing server must not be nil")
	}

	return s.server.URL
}

func (s *integrationMockCoderServer) Close() {
	if s == nil || s.server == nil {
		return
	}

	s.server.Close()
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "/")
}

func hasSegments(segments []string, expected ...string) bool {
	if len(segments) < len(expected) {
		return false
	}

	for i, segment := range expected {
		if segments[i] != segment {
			return false
		}
	}

	return true
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeCoderError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, codersdk.Response{Message: message})
}
