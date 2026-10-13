package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

// This file holds the Coder lookups that every controller which builds
// workspaces shares: the control plane check, the owner rules, and template
// and version resolution. CoderTemplateTest uses them today, and
// CoderWorkspacePool (#153) will too.

// coderStep is the outcome of a shared lookup that cannot go on: a wait,
// when the input can still appear, or a failure, when it never will. Each
// controller turns it into its own status.
type coderStep struct {
	failed      bool
	reason      string
	message     string
	rateLimited bool // Coder answered 429: retry with backoff.
}

func coderWait(reason, format string, args ...any) *coderStep {
	return &coderStep{reason: reason, message: fmt.Sprintf(format, args...)}
}

func coderFail(reason, format string, args ...any) *coderStep {
	return &coderStep{failed: true, reason: reason, message: fmt.Sprintf(format, args...)}
}

// coderStepUnavailable waits after a failed Coder call. The message holds the
// status and Coder's short message only, never details.
func coderStepUnavailable(action string, err error) *coderStep {
	step := coderWait("CoderUnavailable", "%s: %s", action, coderErrorSummary(err))
	step.rateLimited = coderStatus(err) == http.StatusTooManyRequests
	return step
}

// controlPlaneGone reports whether the CoderControlPlane name in namespace is
// NotFound or being deleted. Then nothing can clean up in Coder.
func controlPlaneGone(ctx context.Context, reader client.Reader, namespace, name string) (bool, error) {
	if reader == nil {
		return false, errors.New("assertion failed: control plane check needs a Kubernetes reader")
	}
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := reader.Get(ctx, key, controlPlane); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("get codercontrolplane %s: %w", key, err)
	}
	return !controlPlane.DeletionTimestamp.IsZero(), nil
}

// checkCoderWorkspaceOwner refuses owners whose token is worth more than a
// plain workspace user's: every start build hands the owner's token to
// Terraform.
func checkCoderWorkspaceOwner(ctx context.Context, sdk *codersdk.Client, ownerID uuid.UUID) (codersdk.User, *coderStep, error) {
	refuse := func(format string, args ...any) *coderStep {
		return coderWait("OwnerNotEligible", "Coder user %s: %s", ownerID, fmt.Sprintf(format, args...))
	}
	user, err := sdk.User(ctx, ownerID.String())
	if isCoderNotFound(err) {
		return user, refuse("the user does not exist."), nil
	} else if err != nil {
		return user, coderStepUnavailable("get owner", err), nil
	}
	if err := coderAnswerFor("user", ownerID.String(), user.ID.String()); err != nil {
		return user, nil, err
	}
	// Coder refuses a dormant owner's agents (401), so their workspaces could
	// only time out. Coder creates users through the API as dormant.
	if user.Status != codersdk.UserStatusActive {
		return user, refuse("status %q is not allowed. Only active users can own test workspaces: activate the user in Coder.", user.Status), nil
	}
	// Headless users created before service accounts still have login type none.
	if !user.IsServiceAccount && user.LoginType != codersdk.LoginTypePassword && user.LoginType != codersdk.LoginTypeNone { //nolint:staticcheck // See above.
		return user, refuse("login type %q belongs to a person. Use a service account or a password user.", user.LoginType), nil
	}
	for _, role := range user.Roles {
		if role.Name != codersdk.RoleMember {
			return user, refuse("site role %q is not allowed.", role.Name), nil
		}
	}
	// User.Roles holds site roles only, so read the roles in every
	// organization: an admin role anywhere makes the token privileged. Coder
	// also grants each organization's default member roles to every member.
	allowed := func(role string) bool {
		return role == codersdk.RoleOrganizationMember || role == codersdk.RoleOrganizationWorkspaceAccess
	}
	for _, orgID := range user.OrganizationIDs {
		org, err := sdk.Organization(ctx, orgID)
		if err != nil {
			return user, coderStepUnavailable("get owner organization", err), nil
		}
		if err := coderAnswerFor("organization", orgID.String(), org.ID.String()); err != nil {
			return user, nil, err
		}
		for _, role := range org.DefaultOrgMemberRoles {
			if !allowed(role) {
				return user, refuse("default member role %q in organization %s is not allowed.", role, orgID), nil
			}
		}
		member, err := sdk.OrganizationMember(ctx, orgID.String(), ownerID.String())
		if err != nil {
			return user, coderStepUnavailable("get owner membership", err), nil
		}
		if err := errors.Join(coderAnswerFor("member user", ownerID.String(), member.UserID.String()),
			coderAnswerFor("member organization", orgID.String(), member.OrganizationID.String())); err != nil {
			return user, nil, err
		}
		for _, role := range member.Roles {
			if !allowed(role.Name) {
				return user, refuse("role %q in organization %s is not allowed.", role.Name, orgID), nil
			}
		}
	}
	return user, nil, nil
}

// resolveCoderTemplate finds the template ref (<organization>.<template>) for
// owner, which must be a member of the template's organization.
func resolveCoderTemplate(ctx context.Context, sdk *codersdk.Client, ref string, owner codersdk.User) (codersdk.Template, *coderStep, error) {
	var template codersdk.Template
	orgName, templateName, err := coder.ParseTemplateName(ref)
	if err != nil {
		return template, nil, fmt.Errorf("assertion failed: spec.template %q passed validation but does not parse: %w", ref, err)
	}
	org, err := sdk.OrganizationByName(ctx, orgName)
	if isCoderNotFound(err) {
		return template, coderWait("TemplateNotFound", "Coder organization %q does not exist.", orgName), nil
	} else if err != nil {
		return template, coderStepUnavailable("get organization", err), nil
	}
	if err := coderAnswerFor("organization", strings.ToLower(orgName), strings.ToLower(org.Name)); err != nil {
		return template, nil, err
	}
	if !slices.Contains(owner.OrganizationIDs, org.ID) {
		return template, coderWait("OwnerNotEligible", "Coder user %s is not a member of organization %q.", owner.ID, orgName), nil
	}
	template, err = sdk.TemplateByName(ctx, org.ID, templateName)
	if isCoderNotFound(err) {
		return template, coderWait("TemplateNotFound", "Coder template %q does not exist.", ref), nil
	} else if err != nil {
		return template, coderStepUnavailable("get template", err), nil
	}
	if err := errors.Join(coderAnswerFor("template", strings.ToLower(templateName), strings.ToLower(template.Name)),
		coderAnswerFor("template organization", org.ID.String(), template.OrganizationID.String())); err != nil {
		return template, nil, err
	}
	if template.Deprecated {
		// Coder v2.37.2 refuses new workspaces for a deprecated template
		// (coderd/workspaces.go:958). The deprecation message is Coder
		// detail, so status leaves it out.
		return template, coderFail("TemplateDeprecated", "Coder template %q is deprecated and accepts no new workspaces.", ref), nil
	}
	return template, nil, nil
}

// readCoderTemplateVersion reads a version of template, ref's template: the
// version with ID id, else the one named name, else the active version. A
// returned version without a step belongs to the template, whatever its
// import status.
func readCoderTemplateVersion(
	ctx context.Context, sdk *codersdk.Client, ref string, template codersdk.Template, id, name string,
) (codersdk.TemplateVersion, *coderStep, error) {
	var version codersdk.TemplateVersion
	var err error
	var asked func() error // The rule for the answer: it names what was asked for.
	switch {
	case id != "":
		parsed, parseErr := uuid.Parse(id)
		if parseErr != nil {
			// The CRDs and the controllers store only UUIDs here.
			return version, nil, fmt.Errorf("assertion failed: version ID %q is not a UUID: %w", id, parseErr)
		}
		version, err = sdk.TemplateVersion(ctx, parsed)
		asked = func() error { return coderAnswerFor("version", parsed.String(), version.ID.String()) }
	case name != "":
		version, err = sdk.TemplateVersionByName(ctx, template.ID, name)
		asked = func() error { return coderAnswerFor("version", name, version.Name) }
	default:
		version, err = sdk.TemplateVersion(ctx, template.ActiveVersionID)
		asked = func() error { return coderAnswerFor("version", template.ActiveVersionID.String(), version.ID.String()) }
	}
	switch {
	case isCoderNotFound(err):
		return version, coderWait("TemplateVersionNotFound", "The version of template %q does not exist.", ref), nil
	case err != nil:
		return version, coderStepUnavailable("get template version", err), nil
	}
	if err := asked(); err != nil {
		return version, nil, err
	}
	if version.TemplateID == nil || *version.TemplateID != template.ID {
		return version, coderFail("TemplateVersionMismatch", "Version %s does not belong to template %q.", version.ID, ref), nil
	}
	return version, nil, nil
}

// checkCoderTemplateVersionUsable refuses an archived version and waits for
// an import. Nil means Coder can build workspaces from the version.
func checkCoderTemplateVersionUsable(version codersdk.TemplateVersion) *coderStep {
	if version.Archived {
		return coderFail("TemplateVersionArchived", "Version %s is archived.", version.Name)
	}
	switch version.Job.Status {
	case codersdk.ProvisionerJobSucceeded:
		return nil
	case codersdk.ProvisionerJobPending, codersdk.ProvisionerJobRunning:
		return coderWait("TemplateVersionImporting", "Version %s is still importing.", version.Name)
	default:
		return coderFail("TemplateVersionImportFailed", "The import of version %s ended %s.", version.Name, version.Job.Status)
	}
}
