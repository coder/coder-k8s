// Package coderbootstrap contains optional bootstrap integrations with the Coder API.
package coderbootstrap

import (
	"context"
	"errors"
	"net/http"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/codersdk"
)

const (
	coderSDKRequestTimeout = 30 * time.Second
)

type bypassRateLimitContextKey struct{}

func withRateLimitBypass(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, bypassRateLimitContextKey{}, true)
}

func shouldBypassRateLimit(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(bypassRateLimitContextKey{}).(bool)
	return enabled
}

func isRateLimitBypassRejected(err error) bool {
	var apiErr *codersdk.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode() == http.StatusPreconditionRequired
}

// IsRateLimitError reports whether err (or any wrapped cause) is a codersdk
// API error with HTTP 429 Too Many Requests.
func IsRateLimitError(err error) bool {
	var apiErr *codersdk.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode() == http.StatusTooManyRequests
}

func withOptionalRateLimitBypass[T any](ctx context.Context, operation func(context.Context) (T, error)) (T, error) {
	result, err := operation(withRateLimitBypass(ctx))
	if err == nil {
		return result, nil
	}
	if !isRateLimitBypassRejected(err) {
		return result, err
	}
	return operation(ctx)
}

type bypassRateLimitRoundTripper struct {
	base http.RoundTripper
}

func (rt bypassRateLimitRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, xerrors.New("assertion failed: request must not be nil")
	}
	if !shouldBypassRateLimit(req.Context()) {
		return rt.base.RoundTrip(req)
	}

	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	cloned.Header.Set(codersdk.BypassRatelimitHeader, "true")

	return rt.base.RoundTrip(cloned)
}

// RegisterWorkspaceProxyRequest describes how to register a workspace proxy in Coder.
type RegisterWorkspaceProxyRequest struct {
	CoderURL     string
	SessionToken string
	ProxyName    string
	DisplayName  string
	Icon         string
}

// RegisterWorkspaceProxyResponse contains the proxy identity and token returned by Coder.
type RegisterWorkspaceProxyResponse struct {
	ProxyName  string
	ProxyToken string
}

// Client provides optional bootstrap operations against the Coder API.
type Client interface {
	EnsureWorkspaceProxy(context.Context, RegisterWorkspaceProxyRequest) (RegisterWorkspaceProxyResponse, error)
	EnsureProvisionerKey(context.Context, EnsureProvisionerKeyRequest) (EnsureProvisionerKeyResponse, error)
	DeleteProvisionerKey(ctx context.Context, coderURL, sessionToken, orgName, keyName string) error
}

// SDKClient uses codersdk to perform bootstrap operations.
type SDKClient struct{}

// NewSDKClient returns a bootstrap client backed by codersdk.
func NewSDKClient() *SDKClient {
	return &SDKClient{}
}

// EnsureWorkspaceProxy creates or updates a workspace proxy and returns a token
// suitable for the workspace proxy process.
func (c *SDKClient) EnsureWorkspaceProxy(ctx context.Context, req RegisterWorkspaceProxyRequest) (RegisterWorkspaceProxyResponse, error) {
	if req.CoderURL == "" {
		return RegisterWorkspaceProxyResponse{}, xerrors.New("coder URL is required")
	}
	if req.SessionToken == "" {
		return RegisterWorkspaceProxyResponse{}, xerrors.New("session token is required")
	}
	if req.ProxyName == "" {
		return RegisterWorkspaceProxyResponse{}, xerrors.New("proxy name is required")
	}

	client, err := newAuthenticatedClient(req.CoderURL, req.SessionToken)
	if err != nil {
		return RegisterWorkspaceProxyResponse{}, err
	}

	existing, err := client.WorkspaceProxyByName(ctx, req.ProxyName)
	if err != nil {
		var apiErr *codersdk.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode() != http.StatusNotFound {
			return RegisterWorkspaceProxyResponse{}, xerrors.Errorf("query workspace proxy %q: %w", req.ProxyName, err)
		}

		created, createErr := withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (codersdk.UpdateWorkspaceProxyResponse, error) {
			return client.CreateWorkspaceProxy(requestCtx, codersdk.CreateWorkspaceProxyRequest{
				Name:        req.ProxyName,
				DisplayName: req.DisplayName,
				Icon:        req.Icon,
			})
		})
		if createErr != nil {
			return RegisterWorkspaceProxyResponse{}, xerrors.Errorf("create workspace proxy %q: %w", req.ProxyName, createErr)
		}
		return RegisterWorkspaceProxyResponse{
			ProxyName:  created.Proxy.Name,
			ProxyToken: created.ProxyToken,
		}, nil
	}

	displayName := req.DisplayName
	if displayName == "" {
		displayName = existing.DisplayName
	}
	icon := req.Icon
	if icon == "" {
		icon = existing.IconURL
	}

	updated, err := withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (codersdk.UpdateWorkspaceProxyResponse, error) {
		return client.PatchWorkspaceProxy(requestCtx, codersdk.PatchWorkspaceProxy{
			ID:              existing.ID,
			Name:            existing.Name,
			DisplayName:     displayName,
			Icon:            icon,
			RegenerateToken: true,
		})
	})
	if err != nil {
		return RegisterWorkspaceProxyResponse{}, xerrors.Errorf("update workspace proxy %q: %w", req.ProxyName, err)
	}

	return RegisterWorkspaceProxyResponse{
		ProxyName:  updated.Proxy.Name,
		ProxyToken: updated.ProxyToken,
	}, nil
}
