package coderbootstrap

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/codersdk"

	"github.com/coder/coder-k8s/internal/sameorigin"
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

// ProvisionerKeyByNameRequest identifies a provisioner key by its name.
type ProvisionerKeyByNameRequest struct {
	CoderURL         string
	SessionToken     string
	OrganizationName string
	KeyName          string
}

// ProvisionerKeyByName returns the provisioner key named req.KeyName in the
// organization req.OrganizationName. It returns nil when Coder answers that
// the organization or a key with that name does not exist.
func (c *SDKClient) ProvisionerKeyByName(ctx context.Context, req ProvisionerKeyByNameRequest) (*codersdk.ProvisionerKey, error) {
	if err := validateProvisionerKeyInputs(req.CoderURL, req.SessionToken, req.KeyName); err != nil {
		return nil, err
	}

	client, err := newAuthenticatedClient(req.CoderURL, req.SessionToken)
	if err != nil {
		return nil, err
	}

	organization, err := resolveOrganizationByName(ctx, client, cmp.Or(req.OrganizationName, codersdk.DefaultOrganization))
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (*codersdk.ProvisionerKey, error) {
		return findOrganizationProvisionerKey(requestCtx, client, organization.ID, req.KeyName)
	})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if key != nil && key.ID == uuid.Nil {
		return nil, xerrors.Errorf("assertion failed: provisioner key %q returned an empty ID", req.KeyName)
	}

	return key, nil
}

// DeleteProvisionerKeyByIDRequest identifies one provisioner key by the IDs
// that the caller recorded when it applied the key.
type DeleteProvisionerKeyByIDRequest struct {
	CoderURL       string
	SessionToken   string
	OrganizationID uuid.UUID
	KeyID          uuid.UUID
}

// DeleteProvisionerKeyByID deletes the provisioner key with req.KeyID in the
// organization req.OrganizationID. It does nothing when no key has that ID,
// so a key that someone else created with the same name stays. The Coder API
// deletes keys only by name, so the key is looked up by ID first. The lookup
// and the delete are two requests: a key deleted and created again with the
// same name between them is deleted too.
func (c *SDKClient) DeleteProvisionerKeyByID(ctx context.Context, req DeleteProvisionerKeyByIDRequest) error {
	if req.CoderURL == "" {
		return xerrors.New("coder URL is required")
	}
	if req.SessionToken == "" {
		return xerrors.New("session token is required")
	}
	if req.OrganizationID == uuid.Nil {
		return xerrors.New("organization ID is required")
	}
	if req.KeyID == uuid.Nil {
		return xerrors.New("provisioner key ID is required")
	}

	client, err := newAuthenticatedClient(req.CoderURL, req.SessionToken)
	if err != nil {
		return err
	}

	keys, err := withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) ([]codersdk.ProvisionerKey, error) {
		return client.ListProvisionerKeys(requestCtx, req.OrganizationID)
	})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return xerrors.Errorf("list provisioner keys for organization %q: %w", req.OrganizationID, err)
	}

	var match *codersdk.ProvisionerKey
	for i := range keys {
		if keys[i].ID == req.KeyID {
			match = &keys[i]
			break
		}
	}
	if match == nil {
		return nil
	}
	if match.Name == "" {
		return xerrors.Errorf("assertion failed: provisioner key %q has an empty name", req.KeyID)
	}

	_, err = withOptionalRateLimitBypass(ctx, func(requestCtx context.Context) (struct{}, error) {
		return struct{}{}, client.DeleteProvisionerKey(requestCtx, req.OrganizationID, match.Name)
	})
	if err != nil && !isNotFound(err) {
		return xerrors.Errorf("delete provisioner key %q: %w", match.Name, err)
	}

	return nil
}

// coderNotFoundMessage is the message of the 404 that Coder answers for a
// missing organization or provisioner key (coderd/httpapi/httpapi.go:149 and
// coderd/httpmw/provisionerkey.go:45-46 in v2.37.3). Another 404, for example
// from a proxy or for a wrong base URL, does not prove that the key is gone.
const coderNotFoundMessage = "Resource not found or you do not have access to this resource"

func isNotFound(err error) bool {
	var apiErr *codersdk.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode() == http.StatusNotFound && apiErr.Message == coderNotFoundMessage
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
	// net/http copies custom headers such as Coder-Session-Token to every
	// redirect target, so refuse redirects that leave the configured origin.
	client.HTTPClient.CheckRedirect = sameorigin.RedirectPolicy(coderAPIURL)
	// This client dials no websocket. codersdk websocket dials keep only the
	// Transport and skip CheckRedirect, so before adding one, also wrap the
	// Transport with sameorigin.RoundTripper (#299).

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
