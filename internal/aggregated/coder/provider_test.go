package coder

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestStaticClientProviderClientForNamespace(t *testing.T) {
	t.Parallel()

	client, err := NewSDKClient(Config{
		CoderURL:     mustParseURL(t, "https://coder.example.com"),
		SessionToken: "session-token",
	})
	if err != nil {
		t.Fatalf("create SDK client: %v", err)
	}

	provider := &StaticClientProvider{Client: client, Namespace: "default"}
	resolvedClient, err := provider.ClientForNamespace(context.Background(), "default")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resolvedClient != client {
		t.Fatalf("expected provider to return static client %p, got %p", client, resolvedClient)
	}
}

func TestStaticClientProviderClientForNamespaceAssertions(t *testing.T) {
	t.Parallel()

	validClient, err := NewSDKClient(Config{
		CoderURL:     mustParseURL(t, "https://coder.example.com"),
		SessionToken: "session-token",
	})
	if err != nil {
		t.Fatalf("create SDK client: %v", err)
	}

	tests := []struct {
		name            string
		provider        *StaticClientProvider
		ctx             context.Context
		namespace       string
		wantErrContains string
	}{
		{
			name:            "rejects nil provider",
			provider:        nil,
			ctx:             context.Background(),
			namespace:       "default",
			wantErrContains: "assertion failed: static client provider must not be nil",
		},
		{
			name:            "rejects nil context",
			provider:        &StaticClientProvider{Client: validClient},
			ctx:             nil,
			namespace:       "default",
			wantErrContains: "assertion failed: context must not be nil",
		},
		{
			name:            "rejects nil client",
			provider:        &StaticClientProvider{},
			ctx:             context.Background(),
			namespace:       "default",
			wantErrContains: "assertion failed: static client provider client must not be nil",
		},
		{
			name:            "rejects unpinned provider",
			provider:        &StaticClientProvider{Client: validClient},
			ctx:             context.Background(),
			namespace:       "default",
			wantErrContains: "static coder client provider is not namespace-pinned; configure --coder-namespace",
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := testCase.provider.ClientForNamespace(testCase.ctx, testCase.namespace)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", testCase.wantErrContains)
			}
			if !strings.Contains(err.Error(), testCase.wantErrContains) {
				t.Fatalf("expected error containing %q, got %q", testCase.wantErrContains, err.Error())
			}
		})
	}
}

func TestStaticClientProviderClientForNamespaceNamespaceRestriction(t *testing.T) {
	t.Parallel()

	client, err := NewSDKClient(Config{
		CoderURL:     mustParseURL(t, "https://coder.example.com"),
		SessionToken: "session-token",
	})
	if err != nil {
		t.Fatalf("create SDK client: %v", err)
	}

	provider := &StaticClientProvider{
		Client:    client,
		Namespace: "control-plane",
	}

	resolvedClient, err := provider.ClientForNamespace(context.Background(), "control-plane")
	if err != nil {
		t.Fatalf("expected no error for matching namespace, got %v", err)
	}
	if resolvedClient != client {
		t.Fatalf("expected provider to return static client %p, got %p", client, resolvedClient)
	}

	_, err = provider.ClientForNamespace(context.Background(), "default")
	if err == nil {
		t.Fatal("expected namespace mismatch to fail")
	}
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("expected BadRequest for namespace mismatch, got %v", err)
	}
	wantErrContains := "namespace \"default\" is not served by this aggregated API server (configured for \"control-plane\")"
	if !strings.Contains(err.Error(), wantErrContains) {
		t.Fatalf("expected error containing %q, got %q", wantErrContains, err.Error())
	}
}

func TestStaticClientProviderClientForNamespaceAllowsClusterScopedListNamespace(t *testing.T) {
	t.Parallel()

	client, err := NewSDKClient(Config{
		CoderURL:     mustParseURL(t, "https://coder.example.com"),
		SessionToken: "session-token",
	})
	if err != nil {
		t.Fatalf("create SDK client: %v", err)
	}

	provider := &StaticClientProvider{
		Client:    client,
		Namespace: "control-plane",
	}

	resolvedClient, err := provider.ClientForNamespace(context.Background(), "")
	if err != nil {
		t.Fatalf("expected no error for empty namespace when provider is pinned, got %v", err)
	}
	if resolvedClient != client {
		t.Fatalf("expected provider to return static client %p, got %p", client, resolvedClient)
	}
}

func TestStaticClientProviderDefaultNamespace(t *testing.T) {
	t.Parallel()

	provider := &StaticClientProvider{Namespace: "control-plane"}
	resolvedNamespace, err := provider.DefaultNamespace(context.Background())
	if err != nil {
		t.Fatalf("resolve default namespace: %v", err)
	}
	if got, want := resolvedNamespace, "control-plane"; got != want {
		t.Fatalf("expected default namespace %q, got %q", want, got)
	}
}

func TestStaticClientProviderDefaultNamespaceAssertions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		provider        *StaticClientProvider
		wantErrContains string
		wantServiceDown bool
	}{
		{
			name:            "rejects nil provider",
			provider:        nil,
			wantErrContains: "assertion failed: static client provider must not be nil",
		},
		{
			name:            "rejects unpinned provider",
			provider:        &StaticClientProvider{},
			wantErrContains: "static provider has no default namespace",
			wantServiceDown: true,
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := testCase.provider.DefaultNamespace(context.Background())
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", testCase.wantErrContains)
			}
			if !strings.Contains(err.Error(), testCase.wantErrContains) {
				t.Fatalf("expected error containing %q, got %q", testCase.wantErrContains, err.Error())
			}
			if testCase.wantServiceDown && !apierrors.IsServiceUnavailable(err) {
				t.Fatalf("expected ServiceUnavailable, got %v", err)
			}
		})
	}
}

func TestNewStaticClientProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		cfg             Config
		namespace       string
		wantErrContains string
	}{
		{
			name: "success",
			cfg: Config{
				CoderURL:     mustParseURL(t, "https://coder.example.com"),
				SessionToken: "session-token",
			},
			namespace: "control-plane",
		},
		{
			name: "surfaces SDK config assertion",
			cfg: Config{
				SessionToken: "session-token",
			},
			namespace:       "control-plane",
			wantErrContains: "new SDK client: assertion failed: coder URL must not be nil",
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			provider, err := NewStaticClientProvider(testCase.cfg, testCase.namespace)
			if testCase.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", testCase.wantErrContains)
				}
				if !strings.Contains(err.Error(), testCase.wantErrContains) {
					t.Fatalf("expected error containing %q, got %q", testCase.wantErrContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if provider == nil {
				t.Fatal("expected non-nil provider")
			}
			if provider.Client == nil {
				t.Fatal("expected non-nil provider client")
			}
			if provider.Namespace != testCase.namespace {
				t.Fatalf("expected provider namespace %q, got %q", testCase.namespace, provider.Namespace)
			}
		})
	}
}
