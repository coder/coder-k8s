package coderbootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/codersdk"
)

// EnsureProvisionerKeyRequest describes how to create or look up a provisioner key in Coder.
type EnsureProvisionerKeyRequest struct {
	CoderURL         string
	SessionToken     string
	OrganizationName string
	KeyName          string
	Tags             map[string]string
}

// EnsureProvisionerKeyResponse contains provisioner key metadata.
type EnsureProvisionerKeyResponse struct {
	OrganizationID uuid.UUID
	KeyID          uuid.UUID
	KeyName        string
	// Key is the plaintext provisioner key. It is only non-empty when a key is created.
	Key string
}

// EnsureProvisionerKey creates a provisioner key if it does not already exist,
// otherwise it returns the existing key metadata.
func (c *SDKClient) EnsureProvisionerKey(ctx context.Context, req EnsureProvisionerKeyRequest) (EnsureProvisionerKeyResponse, error) {
	if err := validateProvisionerKeyInputs(req.CoderURL, req.SessionToken, req.KeyName); err != nil {
		return EnsureProvisionerKeyResponse{}, err
	}

	client, err := newAuthenticatedClient(req.CoderURL, req.SessionToken)
	if err != nil {
		return EnsureProvisionerKeyResponse{}, err
	}

	organizationName := req.OrganizationName
	if organizationName == "" {
		organizationName = codersdk.DefaultOrganization
	}

	organization, err := resolveOrganizationByName(ctx, client, organizationName)
	if err != nil {
		return EnsureProvisionerKeyResponse{}, err
	}

	existing, err := findOrganizationProvisionerKey(ctx, client, organization.ID, req.KeyName)
	if err != nil {
		return EnsureProvisionerKeyResponse{}, err
	}
	if existing != nil {
		if existing.ID == uuid.Nil {
			return EnsureProvisionerKeyResponse{}, xerrors.Errorf("assertion failed: provisioner key %q returned an empty ID", req.KeyName)
		}
		return EnsureProvisionerKeyResponse{
			OrganizationID: organization.ID,
			KeyID:          existing.ID,
			KeyName:        existing.Name,
		}, nil
	}

	created, err := withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (codersdk.CreateProvisionerKeyResponse, error) {
		return client.CreateProvisionerKey(requestCtx, organization.ID, codersdk.CreateProvisionerKeyRequest{
			Name: req.KeyName,
			Tags: req.Tags,
		})
	})
	if err != nil {
		return EnsureProvisionerKeyResponse{}, xerrors.Errorf("create provisioner key %q: %w", req.KeyName, err)
	}
	if created.Key == "" {
		return EnsureProvisionerKeyResponse{}, xerrors.Errorf("assertion failed: created provisioner key %q returned an empty key", req.KeyName)
	}

	createdMetadata, err := withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (*codersdk.ProvisionerKey, error) {
		return findOrganizationProvisionerKey(requestCtx, client, organization.ID, req.KeyName)
	})
	if err != nil {
		return EnsureProvisionerKeyResponse{}, xerrors.Errorf("query created provisioner key %q: %w", req.KeyName, err)
	}
	if createdMetadata == nil {
		return EnsureProvisionerKeyResponse{}, xerrors.Errorf("assertion failed: created provisioner key %q was not returned by list", req.KeyName)
	}
	if createdMetadata.ID == uuid.Nil {
		return EnsureProvisionerKeyResponse{}, xerrors.Errorf("assertion failed: created provisioner key %q returned an empty ID", req.KeyName)
	}

	return EnsureProvisionerKeyResponse{
		OrganizationID: organization.ID,
		KeyID:          createdMetadata.ID,
		KeyName:        createdMetadata.Name,
		Key:            created.Key,
	}, nil
}

// DeleteProvisionerKey deletes a provisioner key by name.
// A missing key is treated as success for idempotency.
func (c *SDKClient) DeleteProvisionerKey(ctx context.Context, coderURL, sessionToken, orgName, keyName string) error {
	if err := validateProvisionerKeyInputs(coderURL, sessionToken, keyName); err != nil {
		return err
	}

	client, err := newAuthenticatedClient(coderURL, sessionToken)
	if err != nil {
		return err
	}

	organizationName := orgName
	if organizationName == "" {
		organizationName = codersdk.DefaultOrganization
	}

	organization, err := resolveOrganizationByName(ctx, client, organizationName)
	if err != nil {
		return err
	}

	_, err = withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (struct{}, error) {
		return struct{}{}, client.DeleteProvisionerKey(requestCtx, organization.ID, keyName)
	})
	if err == nil {
		return nil
	}

	var apiErr *codersdk.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode() == http.StatusNotFound {
		return nil
	}

	return xerrors.Errorf("delete provisioner key %q: %w", keyName, err)
}

// Entitlements returns deployment entitlements for the given coderd instance.
func (c *SDKClient) Entitlements(ctx context.Context, coderURL, sessionToken string) (codersdk.Entitlements, error) {
	if coderURL == "" {
		return codersdk.Entitlements{}, xerrors.New("coder URL is required")
	}
	if sessionToken == "" {
		return codersdk.Entitlements{}, xerrors.New("session token is required")
	}

	client, err := newAuthenticatedClient(coderURL, sessionToken)
	if err != nil {
		return codersdk.Entitlements{}, err
	}

	entitlements, err := withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (codersdk.Entitlements, error) {
		return client.Entitlements(requestCtx)
	})
	if err != nil {
		return codersdk.Entitlements{}, xerrors.Errorf("get entitlements: %w", err)
	}
	if entitlements.Features == nil {
		return codersdk.Entitlements{}, xerrors.New("assertion failed: entitlements.features is nil")
	}

	return entitlements, nil
}

func validateProvisionerKeyInputs(coderURL, sessionToken, keyName string) error {
	if coderURL == "" {
		return xerrors.New("coder URL is required")
	}
	if sessionToken == "" {
		return xerrors.New("session token is required")
	}
	if keyName == "" {
		return xerrors.New("provisioner key name is required")
	}

	return nil
}

func newAuthenticatedClient(coderURL, sessionToken string) (*codersdk.Client, error) {
	coderAPIURL, err := url.Parse(coderURL)
	if err != nil {
		return nil, xerrors.Errorf("parse coder URL: %w", err)
	}

	client := codersdk.New(coderAPIURL)
	if client == nil {
		return nil, xerrors.New("assertion failed: codersdk client is nil after successful construction")
	}
	client.SetSessionToken(sessionToken)
	if client.HTTPClient == nil {
		client.HTTPClient = &http.Client{}
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, xerrors.New("assertion failed: http.DefaultTransport is not *http.Transport")
	}
	// Use a dedicated transport to avoid sharing http.DefaultTransport's
	// connection pool across parallel test servers.
	client.HTTPClient.Transport = bypassRateLimitRoundTripper{base: defaultTransport.Clone()}
	client.HTTPClient.Timeout = coderSDKRequestTimeout

	return client, nil
}

func resolveOrganizationByName(ctx context.Context, client *codersdk.Client, organizationName string) (codersdk.Organization, error) {
	organization, err := client.OrganizationByName(ctx, organizationName)
	if err != nil {
		return codersdk.Organization{}, xerrors.Errorf("query organization %q: %w", organizationName, err)
	}
	if organization.ID == uuid.Nil {
		return codersdk.Organization{}, xerrors.Errorf("assertion failed: organization %q returned an empty ID", organizationName)
	}

	return organization, nil
}

func findOrganizationProvisionerKey(ctx context.Context, client *codersdk.Client, organizationID uuid.UUID, keyName string) (*codersdk.ProvisionerKey, error) {
	keys, err := client.ListProvisionerKeys(ctx, organizationID)
	if err != nil {
		return nil, xerrors.Errorf("list provisioner keys for organization %q: %w", organizationID, err)
	}

	var match *codersdk.ProvisionerKey
	for i := range keys {
		if keys[i].Name != keyName {
			continue
		}
		if match != nil {
			return nil, xerrors.Errorf("assertion failed: found multiple provisioner keys named %q in organization %q", keyName, organizationID)
		}
		match = &keys[i]
	}

	return match, nil
}
