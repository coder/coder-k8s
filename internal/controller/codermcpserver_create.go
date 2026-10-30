package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

const (
	// mcpServerConfirmPoll is the poll while a create result is uncertain.
	mcpServerConfirmPoll = 5 * time.Second
	// mcpServerConfirmingRejectedCreate is the wait reason after a create that
	// Coder rejected, while the read that confirms absence fails.
	mcpServerConfirmingRejectedCreate = "ConfirmingRejectedCreate"
)

// mcpServerNonce is the display name of a config that this object created,
// until the spec's display name replaces it. Coder stores no owner, so the
// nonce is the proof (plan 5.3).
func mcpServerNonce(server *coderv1alpha1.CoderMCPServer) string {
	return mcpServerNoncePrefix + string(server.UID)
}

// createOrRecord handles an object without status.configID, following the
// CoderAIProvider create (#253). It reads Coder first, so a create that
// committed earlier is recorded instead of sent again. Coder's unique slug
// index (plan F3) makes a second create of this slug answer 409, so a late
// commit never leaves two configs. It returns nil when status.configID is set.
func (r *CoderMCPServerReconciler) createOrRecord(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID,
	values map[string]credentialsecret.Value, before *coderv1alpha1.CoderMCPServerStatus,
) (*mcpServerStep, error) {
	stalled := meta.FindStatusCondition(server.Status.Conditions, coderv1alpha1.CoderMCPServerConditionStalled)
	if server.Status.CreateAttemptTime == nil && stalled != nil && stalled.Status == metav1.ConditionTrue &&
		stalled.Reason == "CoderRejected" && stalled.ObservedGeneration == server.Generation {
		// Coder rejected this spec. Only a spec change retries.
		return mcpServerStalled(0, stalled.Reason, "%s", stalled.Message), nil
	}
	ready := meta.FindStatusCondition(server.Status.Conditions, coderv1alpha1.CoderMCPServerConditionReady)
	rejected := server.Status.CreateAttemptTime != nil && ready != nil &&
		ready.Reason == mcpServerConfirmingRejectedCreate && ready.ObservedGeneration == server.Generation
	found, step := findMCPServerSlug(ctx, sdk, server, orgID)
	if step != nil {
		return keepRejectedMCPServerCreate(step, rejected), nil
	}
	if found != nil {
		if step := recordMCPServer(server, *found); step != nil {
			return step, nil
		}
		// Store the ID before the sync replaces the nonce (#353).
		return r.ensureMCPServerRecord(ctx, sdk, server)
	}
	if server.Status.CreateAttemptTime != nil {
		// A create of this object may have been sent. A missing config proves
		// that nothing committed only when the operator can see every config.
		if step := mcpServerRequireSiteOwner(ctx, sdk); step != nil {
			return keepRejectedMCPServerCreate(step, rejected), nil
		}
		if rejected {
			server.Status.CreateAttemptTime, server.Status.Credentials = nil, nil
			return mcpServerStalled(0, "CoderRejected", "Coder rejected the create request of MCP server config %s, and a read confirmed that nothing was created.", server.Spec.Slug), nil
		}
	}
	if step, err := r.createMCPServer(ctx, sdk, server, orgID, values, before); step != nil || err != nil {
		return step, err
	}
	return r.ensureMCPServerRecord(ctx, sdk, server)
}

// keepRejectedMCPServerCreate keeps the memory of a rejected create in the
// reason of a wait, so the create is not sent again once the wait ends.
func keepRejectedMCPServerCreate(step *mcpServerStep, rejected bool) *mcpServerStep {
	if rejected {
		step.reason = mcpServerConfirmingRejectedCreate
	}
	return step
}

// mcpServerRequireSiteOwner returns a wait unless the operator user is a site
// owner, who sees every config in the list.
func mcpServerRequireSiteOwner(ctx context.Context, sdk *codersdk.Client) *mcpServerStep {
	operator, err := sdk.User(ctx, codersdk.Me)
	if err != nil {
		return mcpServerWait(mcpServerRetry, "CoderUnavailable", "get operator user: %s", coderErrorSummary(err))
	}
	if !slices.ContainsFunc(operator.Roles, func(role codersdk.SlimRole) bool { return role.Name == codersdk.RoleOwner && role.OrganizationID == "" }) {
		return mcpServerWait(mcpServerResync, "ConfirmingCreate", "The operator user is not a site owner, so a missing MCP server config proves nothing.")
	}
	return nil
}

// recordMCPServer decides whether found, the config with the spec's slug, is
// the config that this object created. Only the nonce proves it. Anything
// else is a config that the controller never touches (no adoption). It
// returns nil when it recorded the ID.
func recordMCPServer(server *coderv1alpha1.CoderMCPServer, found codersdk.MCPServerConfig) *mcpServerStep {
	if found.DisplayName != mcpServerNonce(server) {
		// No create of this object can commit while another config holds the
		// slug, so nothing of this object can exist in Coder.
		server.Status.CreateAttemptTime, server.Status.Credentials = nil, nil
		return mcpServerStalled(mcpServerResync, "SlugConflict",
			"Coder organization %s already has an MCP server config with slug %s (ID %s). The controller does not change a config that it did not create.",
			server.Spec.Organization, server.Spec.Slug, found.ID)
	}
	server.Status.ConfigID, server.Status.CreateAttemptTime = found.ID.String(), nil
	return nil
}

// createMCPServer writes the cleanup record, then the create marker and the
// change tokens, then sends the only create request of this attempt. A failed
// record write or a conflict on the marker write ends the reconcile before
// any request. The config starts disabled with the nonce as its display name,
// so members never see it and chats never use it (plan F6) before the apply
// step.
func (r *CoderMCPServerReconciler) createMCPServer(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID,
	values map[string]credentialsecret.Value, before *coderv1alpha1.CoderMCPServerStatus,
) (*mcpServerStep, error) {
	now := time.Now()
	// Without the record, a config that a namespace deletion leaves behind
	// cannot be found again (#353 invariant 1).
	if step, err := r.writeMCPServerRecord(ctx, sdk, server, &now); step != nil || err != nil {
		return step, err
	}
	server.Status.CreateAttemptTime = &metav1.Time{Time: now}
	server.Status.Credentials = mcpServerCredentialStatus(server.Spec.Auth, values)
	applyMCPServerStep(server, mcpServerWait(0, "CreatingConfig", "Creating MCP server config %s in Coder.", server.Spec.Slug))
	if err := r.writeStatus(ctx, server, before); err != nil {
		return nil, err
	}
	*before = *server.Status.DeepCopy()

	req, err := mcpServerCreateRequest(server, values)
	if err != nil {
		return nil, err
	}
	created, err := sdk.CreateMCPServerConfig(ctx, orgID, req)
	status := coderStatus(err)
	switch {
	case err == nil:
		if created.ID == uuid.Nil || created.OrganizationID != orgID || created.Slug != server.Spec.Slug || created.DisplayName != req.DisplayName {
			// A wrong answer leaves the marker, so a read decides.
			return mcpServerWait(mcpServerConfirmPoll, "ConfirmingCreate",
				"The create answer does not match the request. The controller reads Coder instead of sending it again."), nil
		}
		server.Status.ConfigID, server.Status.CreateAttemptTime = created.ID.String(), nil
		var sent []string
		for _, c := range mcpServerCredentialRefs(server.Spec.Auth) {
			sent = append(sent, c.field)
		}
		r.rememberSentCredentials(server, sent, values)
		return nil, nil
	case status == 0 && createHadNoEffect(err):
		// A failed dial or TLS handshake never sent the request.
		server.Status.CreateAttemptTime, server.Status.Credentials = nil, nil
		return mcpServerWait(mcpServerRetry, "CreateRetrying", "The create request had no effect: %s", coderErrorSummary(err)), nil
	case status >= 400 && status < 500:
		return confirmRejectedMCPServerCreate(ctx, sdk, server, orgID, status, err), nil
	default:
		return mcpServerWait(mcpServerConfirmPoll, "ConfirmingCreate",
			"The create result is uncertain (%s). The controller reads Coder instead of sending it again.", coderErrorSummary(err)), nil
	}
}

// confirmRejectedMCPServerCreate reads Coder after a 4xx answer to a create.
// Coder sends its 4xx answers before the insert commits, but a proxy can put a
// 4xx in place of a committed answer, so only a read proves that nothing of
// this object exists. Until then the marker stays (#253 review round 3).
func confirmRejectedMCPServerCreate(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID, status int, createErr error,
) *mcpServerStep {
	retryable := mcpServerCreateRetryable(status)
	found, step := findMCPServerSlug(ctx, sdk, server, orgID)
	switch {
	case step != nil:
		return keepRejectedMCPServerCreate(step, !retryable)
	case found != nil:
		// Ours (the create committed) or someone else's (SlugConflict).
		return recordMCPServer(server, *found)
	}
	if step := mcpServerRequireSiteOwner(ctx, sdk); step != nil {
		return keepRejectedMCPServerCreate(step, !retryable)
	}
	server.Status.CreateAttemptTime, server.Status.Credentials = nil, nil
	if retryable {
		return mcpServerWait(mcpServerRetry, "CreateRetrying", "The create request had no effect: %s", coderErrorSummary(createErr))
	}
	return mcpServerStalled(0, "CoderRejected", "Coder rejected the create request: %s", coderErrorSummary(createErr))
}

// mcpServerCreateRetryable reports 4xx create answers that a retry can fix
// without a spec change: an operator token or role problem, a slug that was
// freed again, or a rate limit.
func mcpServerCreateRetryable(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return false
}

// mcpServerCredentialStatus returns the change token of every credential that
// the create request carries.
func mcpServerCredentialStatus(auth coderv1alpha1.CoderMCPServerAuth, values map[string]credentialsecret.Value) []coderv1alpha1.CoderMCPServerCredentialStatus {
	var out []coderv1alpha1.CoderMCPServerCredentialStatus
	for _, c := range mcpServerCredentialRefs(auth) {
		token := values[c.field].Token
		out = append(out, coderv1alpha1.CoderMCPServerCredentialStatus{
			Field: c.field, SecretName: token.SecretName, SecretKey: token.SecretKey, SecretUID: string(token.SecretUID), SecretResourceVersion: token.ResourceVersion,
		})
	}
	return out
}

// mcpServerCreateRequest builds the create request: every spec field, the
// credentials of the auth type, enabled false, and the nonce as the display
// name. Credentials are revealed only here, into the request body.
func mcpServerCreateRequest(server *coderv1alpha1.CoderMCPServer, values map[string]credentialsecret.Value) (codersdk.CreateMCPServerConfigRequest, error) {
	spec := server.Spec
	if spec.Transport == "" || spec.Availability == "" {
		return codersdk.CreateMCPServerConfigRequest{}, fmt.Errorf("assertion failed: MCP server %s/%s has no transport or availability after admission defaults", server.Namespace, server.Name)
	}
	req := codersdk.CreateMCPServerConfigRequest{
		DisplayName: mcpServerNonce(server), Slug: spec.Slug, Description: spec.Description, IconURL: spec.IconURL,
		Transport: spec.Transport, URL: spec.URL, AuthType: spec.Auth.Type, Availability: spec.Availability,
		Enabled: false, ModelIntent: spec.ModelIntent, AllowInPlanMode: spec.AllowInPlanMode, ForwardCoderHeaders: spec.ForwardCoderHeaders,
	}
	reveal := func(field string) (string, error) {
		v, ok := values[field]
		if !ok {
			return "", fmt.Errorf("assertion failed: MCP server %s/%s has no value for credential %s", server.Namespace, server.Name, field)
		}
		return v.Reveal(), nil
	}
	var err error
	switch auth := spec.Auth; auth.Type {
	case "api_key":
		req.APIKeyHeader = auth.APIKey.Header
		req.APIKeyValue, err = reveal("apiKey")
	case "custom_headers":
		req.CustomHeaders = make(map[string]string, len(auth.CustomHeaders))
		for _, h := range auth.CustomHeaders {
			if req.CustomHeaders[h.Name], err = reveal("header:" + h.Name); err != nil {
				break
			}
		}
	case "oauth2":
		o := auth.OAuth2
		req.OAuth2ClientID, req.OAuth2AuthURL, req.OAuth2TokenURL, req.OAuth2RevocationURL = o.ClientID, o.AuthURL, o.TokenURL, o.RevocationURL
		req.OAuth2Scopes = strings.Join(o.Scopes, " ")
		req.OAuth2ClientSecret, err = reveal("oauth2ClientSecret")
	}
	return req, err
}
