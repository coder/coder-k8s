package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

// Coder resolves aliases such as the "default" organization, the "me" user, and raw IDs on the
// server side, but the aggregated API must return every object under exactly the requested
// metadata.name. Request names therefore have to use the canonical organization and owner names.
// Name parsing stays syntactic; a segment is rejected only when authoritative resolution returns a
// differently named organization or user, and the rejection happens before any backend mutation.

// requireCanonicalTemplateName rejects a template request whose organization segment resolved to a
// differently named organization.
func requireCanonicalTemplateName(name, orgName, templateName string, org codersdk.Organization) error {
	if org.Name == "" {
		return fmt.Errorf("assertion failed: resolved organization for template %q must have a name", name)
	}
	if org.Name == orgName {
		return nil
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"template name %q: organization segment %q resolves to organization %q; use the canonical name %q",
		name, orgName, org.Name, coder.BuildTemplateName(org.Name, templateName),
	))
}

// requireCanonicalWorkspaceName rejects a workspace request whose organization or owner segment
// resolved to a differently named organization or user.
func requireCanonicalWorkspaceName(name, orgName, userName, workspaceName, canonicalOrg, canonicalUser string) error {
	if canonicalOrg == "" || canonicalUser == "" {
		return fmt.Errorf("assertion failed: resolved organization and owner for workspace %q must have names", name)
	}

	var aliases []string
	if canonicalOrg != orgName {
		aliases = append(aliases, fmt.Sprintf("organization segment %q resolves to organization %q", orgName, canonicalOrg))
	}
	if canonicalUser != userName {
		aliases = append(aliases, fmt.Sprintf("owner segment %q resolves to user %q", userName, canonicalUser))
	}
	if len(aliases) == 0 {
		return nil
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"workspace name %q: %s; use the canonical name %q",
		name, strings.Join(aliases, ", "), coder.BuildWorkspaceName(canonicalOrg, canonicalUser, workspaceName),
	))
}

// requireFetchedWorkspaceIdentity checks a workspace fetched by owner segment against the requested
// name. Membership in the requested organization is established first (by organization ID once the
// names differ), so a workspace from another organization stays an opaque NotFound that discloses
// nothing about it; only then are alias segments rejected with the canonical name.
func requireFetchedWorkspaceIdentity(
	ctx context.Context,
	sdk *codersdk.Client,
	name, orgName, userName, workspaceName string,
	workspace codersdk.Workspace,
) error {
	if sdk == nil {
		return fmt.Errorf("assertion failed: coder client must not be nil")
	}
	if workspace.OrganizationID == uuid.Nil {
		return fmt.Errorf("assertion failed: fetched workspace %q must carry an organization ID", name)
	}

	if workspace.OrganizationName != orgName {
		org, err := sdk.OrganizationByName(ctx, orgName)
		if err != nil {
			return coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
		}
		if org.ID != workspace.OrganizationID {
			return apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
		}
	}

	return requireCanonicalWorkspaceName(name, orgName, userName, workspaceName, workspace.OrganizationName, workspace.OwnerName)
}
