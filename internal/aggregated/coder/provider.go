package coder

import (
	"context"
	"fmt"

	"github.com/coder/coder/v2/codersdk"
)

// ClientProvider resolves a Coder SDK client for a Kubernetes request namespace.
type ClientProvider interface {
	ClientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error)
}

// StaticClientProvider returns one static client for all namespaces.
type StaticClientProvider struct {
	Client *codersdk.Client
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
	if namespace == "" {
		return nil, fmt.Errorf("assertion failed: namespace must not be empty")
	}

	return p.Client, nil
}

// NewStaticClientProvider creates a StaticClientProvider from cfg.
func NewStaticClientProvider(cfg Config) (*StaticClientProvider, error) {
	client, err := NewSDKClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("new SDK client: %w", err)
	}

	provider := &StaticClientProvider{Client: client}
	if provider.Client == nil {
		return nil, fmt.Errorf("assertion failed: static client provider client is nil after successful construction")
	}

	return provider, nil
}
