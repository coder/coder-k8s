package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

// Coder resolves aliases such as the "default" organization, the "me" user, raw IDs, and
// alternate-cased template and workspace names on the server side, but the aggregated API must
// return every object under exactly the requested metadata.name. Request names therefore have to
// use the canonical organization, owner, template, and workspace names.
// Name parsing stays syntactic; a segment is rejected only when authoritative resolution returns a
// differently named organization or user, and the rejection happens before any backend mutation.

// requireCanonicalTemplateName rejects a template request whose organization segment resolved to a
// differently named organization. It runs before the template lookup, so the message corrects only
// the organization segment and says that the template segment has not been checked yet.
func requireCanonicalTemplateName(name, orgName, templateName string, org codersdk.Organization) error {
	if org.Name == "" {
		return fmt.Errorf("assertion failed: resolved organization for template %q must have a name", name)
	}
	if org.Name == orgName {
		return nil
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"template name %q: organization segment %q resolves to organization %q; use the canonical organization name, for example %q (the template segment is not checked until the organization segment is canonical)",
		name, orgName, org.Name, coder.BuildTemplateName(org.Name, templateName),
	))
}

// requireCanonicalTemplateLeaf rejects a fetched template whose own name differs from the requested
// final segment. Coder resolves template names case-insensitively, so an alternate-cased request
// finds the canonical template; it must not be returned or mutated under the requested name.
func requireCanonicalTemplateLeaf(name, orgName, templateName string, template codersdk.Template) error {
	if template.Name == "" {
		return fmt.Errorf("assertion failed: fetched template for %q must have a name", name)
	}
	if template.Name == templateName {
		return nil
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"template name %q: template segment %q resolves to template %q; use the canonical name %q",
		name, templateName, template.Name, coder.BuildTemplateName(orgName, template.Name),
	))
}

// requireCanonicalWorkspaceName rejects a workspace request whose organization or owner segment
// resolved to a differently named organization or user. hintWorkspace is the leaf placed in the
// canonical hint: the fetched workspace's own name when leafVerified is true, otherwise the requested
// leaf, in which case the message says the workspace segment has not been checked.
func requireCanonicalWorkspaceName(name, orgName, userName, hintWorkspace, canonicalOrg, canonicalUser string, leafVerified bool) error {
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

	canonical := coder.BuildWorkspaceName(canonicalOrg, canonicalUser, hintWorkspace)
	if leafVerified {
		return apierrors.NewBadRequest(fmt.Sprintf("workspace name %q: %s; use the canonical name %q", name, strings.Join(aliases, ", "), canonical))
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"workspace name %q: %s; use the canonical organization and owner names, for example %q (the workspace segment is not checked on create)",
		name, strings.Join(aliases, ", "), canonical,
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
			return mapMembershipVerificationError(err, name)
		}
		if org.ID != workspace.OrganizationID {
			return apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
		}
	}

	// Membership is settled above, so the fetched name may now shape the hint: every alias segment is
	// corrected at once and the advertised canonical name is accepted on retry.
	if workspace.Name == "" {
		return fmt.Errorf("assertion failed: fetched workspace for %q must have a name", name)
	}
	if err := requireCanonicalWorkspaceName(name, orgName, userName, workspace.Name, workspace.OrganizationName, workspace.OwnerName, true); err != nil {
		return err
	}

	// Coder resolves workspace names case-insensitively as well; the fetched workspace's own name
	// must equal the requested final segment.
	if workspace.Name != workspaceName {
		return apierrors.NewBadRequest(fmt.Sprintf(
			"workspace name %q: workspace segment %q resolves to workspace %q; use the canonical name %q",
			name, workspaceName, workspace.Name, coder.BuildWorkspaceName(workspace.OrganizationName, workspace.OwnerName, workspace.Name),
		))
	}

	return nil
}

// mapMembershipVerificationError maps an error from the requested-organization lookup that verifies
// membership for a workspace request. A denied lookup (403) becomes the same opaque NotFound as a
// genuine cross-organization mismatch: a workspace whose name is probed under an organization the
// caller cannot read must look the same whether it exists or not. Every other error (401, 404, 5xx)
// keeps its ordinary mapping; direct Create requests never use this mapping.
func mapMembershipVerificationError(err error, name string) error {
	if err == nil {
		return fmt.Errorf("assertion failed: membership verification error must not be nil")
	}

	var coderErr *codersdk.Error
	if errors.As(err, &coderErr) && coderErr.StatusCode() == http.StatusForbidden {
		return apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), name)
	}

	return coder.MapCoderError(err, aggregationv1alpha1.Resource("coderworkspaces"), name)
}
