// Package coderbootstrap contains optional bootstrap integrations with the Coder API.
package coderbootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/codersdk"
)

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
}

// SDKClient uses codersdk to register workspace proxies.
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

	coderURL, err := url.Parse(req.CoderURL)
	if err != nil {
		return RegisterWorkspaceProxyResponse{}, xerrors.Errorf("parse coder URL: %w", err)
	}

	client := codersdk.New(coderURL)
	client.SetSessionToken(req.SessionToken)

	existing, err := client.WorkspaceProxyByName(ctx, req.ProxyName)
	if err != nil {
		var apiErr *codersdk.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode() != http.StatusNotFound {
			return RegisterWorkspaceProxyResponse{}, xerrors.Errorf("query workspace proxy %q: %w", req.ProxyName, err)
		}

		created, createErr := client.CreateWorkspaceProxy(ctx, codersdk.CreateWorkspaceProxyRequest{
			Name:        req.ProxyName,
			DisplayName: req.DisplayName,
			Icon:        req.Icon,
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

	updated, err := client.PatchWorkspaceProxy(ctx, codersdk.PatchWorkspaceProxy{
		ID:              existing.ID,
		Name:            existing.Name,
		DisplayName:     displayName,
		Icon:            icon,
		RegenerateToken: true,
	})
	if err != nil {
		return RegisterWorkspaceProxyResponse{}, xerrors.Errorf("update workspace proxy %q: %w", req.ProxyName, err)
	}

	return RegisterWorkspaceProxyResponse{
		ProxyName:  updated.Proxy.Name,
		ProxyToken: updated.ProxyToken,
	}, nil
}
