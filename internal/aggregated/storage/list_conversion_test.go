package storage

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

func TestNamespaceForListConversionUsesResolvedNamespaceForAllNamespaces(t *testing.T) {
	t.Parallel()

	provider := &listConversionResolverProvider{defaultNamespace: "control-plane"}

	resolvedNamespace, err := namespaceForListConversion(context.Background(), "", provider)
	if err != nil {
		t.Fatalf("resolve namespace for all-namespaces list: %v", err)
	}
	if got, want := resolvedNamespace, "control-plane"; got != want {
		t.Fatalf("expected resolved namespace %q, got %q", want, got)
	}
	if got, want := provider.defaultNamespaceCalls, 1; got != want {
		t.Fatalf("expected DefaultNamespace to be called %d time, got %d", want, got)
	}
}

func TestNamespaceForListConversionRejectsProviderWithoutNamespaceResolver(t *testing.T) {
	t.Parallel()

	provider := &listConversionClientOnlyProvider{}

	_, err := namespaceForListConversion(context.Background(), "", provider)
	if err == nil {
		t.Fatal("expected error")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected ServiceUnavailable, got %v", err)
	}
}

func TestNamespaceForListConversionKeepsRequestNamespace(t *testing.T) {
	t.Parallel()

	provider := &listConversionResolverProvider{defaultNamespace: "control-plane"}

	resolvedNamespace, err := namespaceForListConversion(context.Background(), "request-namespace", provider)
	if err != nil {
		t.Fatalf("resolve namespace for namespaced list: %v", err)
	}
	if got, want := resolvedNamespace, "request-namespace"; got != want {
		t.Fatalf("expected request namespace %q, got %q", want, got)
	}
	if provider.defaultNamespaceCalls != 0 {
		t.Fatalf("expected DefaultNamespace not to be called for namespaced requests, got %d calls", provider.defaultNamespaceCalls)
	}
}

type listConversionClientOnlyProvider struct{}

var _ coder.ClientProvider = (*listConversionClientOnlyProvider)(nil)

func (*listConversionClientOnlyProvider) ClientForNamespace(
	_ context.Context,
	_ string,
) (*codersdk.Client, error) {
	return nil, nil
}

type listConversionResolverProvider struct {
	defaultNamespace      string
	defaultNamespaceCalls int
}

var (
	_ coder.ClientProvider    = (*listConversionResolverProvider)(nil)
	_ coder.NamespaceResolver = (*listConversionResolverProvider)(nil)
)

func (*listConversionResolverProvider) ClientForNamespace(
	_ context.Context,
	_ string,
) (*codersdk.Client, error) {
	return nil, nil
}

func (p *listConversionResolverProvider) DefaultNamespace(context.Context) (string, error) {
	p.defaultNamespaceCalls++
	return p.defaultNamespace, nil
}
