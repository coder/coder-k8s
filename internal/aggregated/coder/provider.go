package coder

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/coder/coder/v2/codersdk"
)

// ClientProvider resolves a Coder SDK client for a Kubernetes request namespace.
type ClientProvider interface {
	ClientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error)
}

// StaticClientProvider returns one static client, optionally restricted to one namespace.
type StaticClientProvider struct {
	Client    *codersdk.Client
	Namespace string // If non-empty, only this namespace is allowed.
}

var _ ClientProvider = (*StaticClientProvider)(nil)

// ClientForNamespace returns the static client.
func (p *StaticClientProvider) ClientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error) {
	if p == nil {
		return nil, fmt.Errorf("assertion failed: static client provider must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if p.Client == nil {
		return nil, fmt.Errorf("assertion failed: static client provider client must not be nil")
	}
	if p.Namespace == "" {
		return nil, apierrors.NewServiceUnavailable(
			"static coder client provider is not namespace-pinned; configure --coder-namespace",
		)
	}
	if namespace == "" {
		namespace = p.Namespace
	}
	if namespace != p.Namespace {
		return nil, apierrors.NewBadRequest(
			fmt.Sprintf(
				"namespace %q is not served by this aggregated API server (configured for %q)",
				namespace,
				p.Namespace,
			),
		)
	}

	return p.Client, nil
}

// NewStaticClientProvider creates a StaticClientProvider from cfg and optional namespace restriction.
func NewStaticClientProvider(cfg Config, namespace string) (*StaticClientProvider, error) {
	client, err := NewSDKClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("new SDK client: %w", err)
	}

	provider := &StaticClientProvider{
		Client:    client,
		Namespace: namespace,
	}
	if provider.Client == nil {
		return nil, fmt.Errorf("assertion failed: static client provider client is nil after successful construction")
	}

	return provider, nil
}
