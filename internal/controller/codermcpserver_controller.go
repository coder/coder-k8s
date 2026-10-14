package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

const (
	mcpServerControlPlaneRefIndex = ".spec.controlPlaneRef.name"
	// mcpServerResync re-reads Coder for objects that wait on Coder state,
	// for example a slug conflict that an admin can remove in the dashboard.
	mcpServerResync = 10 * time.Minute
	// mcpServerRetry is the retry for waits that no watch ends: a failed
	// Coder call, or an operator token Secret that is not readable yet.
	mcpServerRetry = 30 * time.Second
	// mcpServerSecretChangedRetry re-reads Secrets that changed between two
	// reads of one reconcile.
	mcpServerSecretChangedRetry = time.Second
	// mcpServerNoncePrefix starts the display name of a config that the
	// controller created, until the first apply sets the spec value.
	mcpServerNoncePrefix = "coder-k8s:"
)

// CoderMCPServerReconciler reconciles CoderMCPServer objects (#155). It is not
// registered yet. It checks the control plane, the organization and its
// opt-in, and the credential Secrets, and it reports a slug that a config of
// someone else holds. It writes nothing to Coder.
type CoderMCPServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// mcpServerStep is the outcome of one reconcile. A ready step reports that
// Coder matches the spec. A stalled step waits for a change of the spec or an
// input. Any other step waits for progress.
type mcpServerStep struct {
	ready   bool
	stalled bool
	reason  string
	message string
	requeue time.Duration
}

func mcpServerWait(requeue time.Duration, reason, format string, args ...any) *mcpServerStep {
	return &mcpServerStep{reason: reason, message: fmt.Sprintf(format, args...), requeue: requeue}
}

func mcpServerStalled(requeue time.Duration, reason, format string, args ...any) *mcpServerStep {
	return &mcpServerStep{stalled: true, reason: reason, message: fmt.Sprintf(format, args...), requeue: requeue}
}

// SetupWithManager wires the reconciler into controller-runtime. Nothing calls
// it until activation. The controller watches its own kind without a
// generation predicate, so an annotation change also triggers a reconcile.
// A change of the referenced control plane or of a credential Secret requeues
// the object.
func (r *CoderMCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil || r.Client == nil || r.Scheme == nil {
		return fmt.Errorf("assertion failed: MCP server reconciler needs a manager, a client, and a scheme")
	}
	indexes := map[string]func(*coderv1alpha1.CoderMCPServer) []string{
		mcpServerControlPlaneRefIndex:   func(s *coderv1alpha1.CoderMCPServer) []string { return []string{s.Spec.ControlPlaneRef.Name} },
		credentialSecretNamesFieldIndex: mcpServerSecretNames,
	}
	for name, values := range indexes {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &coderv1alpha1.CoderMCPServer{}, name, func(obj client.Object) []string {
			if s, ok := obj.(*coderv1alpha1.CoderMCPServer); ok {
				return values(s)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("index MCP servers by %s: %w", name, err)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderMCPServer{}).
		Watches(&coderv1alpha1.CoderControlPlane{}, handler.EnqueueRequestsFromMapFunc(r.serversForControlPlane)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return r.serversWith(ctx, obj.GetNamespace(), credentialSecretNamesFieldIndex, obj.GetName())
		}),
			builder.WithPredicates(credentialsecret.WatchPredicate())).
		Named("codermcpserver").
		Complete(r)
}

// serversForControlPlane maps a control plane to the objects that use it, and
// to the objects whose credential Secrets it uses for its own access, because
// credentialsecret.Read refuses those. On an update, controller-runtime maps
// the old and the new object, so a name that the control plane stops using
// re-queues its objects too.
func (r *CoderMCPServerReconciler) serversForControlPlane(ctx context.Context, obj client.Object) []reconcile.Request {
	requests := r.serversWith(ctx, obj.GetNamespace(), mcpServerControlPlaneRefIndex, obj.GetName())
	controlPlane, ok := obj.(*coderv1alpha1.CoderControlPlane)
	if !ok {
		return requests
	}
	for _, name := range credentialsecret.ControlPlaneSecretNames(controlPlane) {
		requests = append(requests, r.serversWith(ctx, obj.GetNamespace(), credentialSecretNamesFieldIndex, name)...)
	}
	return requests
}

// serversWith returns requests for the objects in namespace whose index holds
// value.
func (r *CoderMCPServerReconciler) serversWith(ctx context.Context, namespace, index, value string) []reconcile.Request {
	var servers coderv1alpha1.CoderMCPServerList
	if err := r.List(ctx, &servers, client.InNamespace(namespace), client.MatchingFields{index: value}); err != nil {
		log.FromContext(ctx).Error(err, "list MCP servers", "index", index, "value", value)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(servers.Items))
	for _, s := range servers.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name}})
	}
	return requests
}

// Reconcile runs the checks in order and reports the first one that fails.
func (r *CoderMCPServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil || r.Scheme == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: MCP server reconciler needs a client and a scheme")
	}
	server := &coderv1alpha1.CoderMCPServer{}
	if err := r.Get(ctx, req.NamespacedName, server); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if server.Name != req.Name || server.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched MCP server %s/%s does not match %s", server.Namespace, server.Name, req.NamespacedName)
	}
	before := server.Status.DeepCopy()

	if !server.DeletionTimestamp.IsZero() {
		// The scope gate never blocks a deletion.
		if !controllerutil.ContainsFinalizer(server, coderv1alpha1.CoderMCPServerCleanupFinalizer) {
			return ctrl.Result{}, nil
		}
		if server.Status.ConfigID == "" && server.Status.CreateAttemptTime == nil {
			// Nothing of this object can exist in Coder.
			return ctrl.Result{}, r.patchFinalizer(ctx, server, false)
		}
		// Keep the finalizer: this controller version cannot prove what Coder
		// holds, so it neither deletes nor forgets the config.
		applyMCPServerStep(server, mcpServerWait(0, "DeletePending",
			"MCP server config %s can exist in Coder. This controller version keeps the finalizer and deletes nothing.", server.Spec.Slug))
		return ctrl.Result{}, r.writeStatus(ctx, server, before)
	}

	if err := r.patchFinalizer(ctx, server, true); err != nil {
		return ctrl.Result{}, err
	}
	step, err := r.check(ctx, server, before)
	if err != nil {
		return ctrl.Result{}, err
	}
	applyMCPServerStep(server, step)
	if err := r.writeStatus(ctx, server, before); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: step.requeue}, nil
}

// check gets Coder access, resolves and pins the organization, applies the
// control plane's opt-in, reads the credentials, and then creates the config
// or records the one that this object created. Every Coder call before the
// slug lookup is an organization lookup.
func (r *CoderMCPServerReconciler) check(
	ctx context.Context, server *coderv1alpha1.CoderMCPServer, before *coderv1alpha1.CoderMCPServerStatus,
) (*mcpServerStep, error) {
	sdk, controlPlane, wait, err := controlPlaneCoderClient(ctx, r.Client, server.Namespace, server.Spec.ControlPlaneRef.Name)
	if err != nil {
		return nil, err
	}
	if wait != nil {
		// The control plane watch ends most waits. A retry also covers an
		// operator token Secret that appears later.
		return mcpServerWait(mcpServerRetry, wait.reason, "%s", wait.message), nil
	}
	org, step, err := resolveMCPServerOrganization(ctx, sdk, server.Spec.Organization)
	if step != nil || err != nil {
		return step, err
	}
	if pinned := server.Status.OrganizationID; pinned != "" && pinned != org.ID.String() {
		return mcpServerStalled(mcpServerResync, "OrganizationChanged",
			"Coder organization %s now has ID %s, not the pinned ID %s. Re-create this object to use the new organization.", server.Spec.Organization, org.ID, pinned), nil
	}
	server.Status.OrganizationID = org.ID.String()
	if step, err := mcpServerScopeGate(ctx, sdk, controlPlane, server.Spec, org.ID); step != nil || err != nil {
		return step, err
	}
	values, step, err := readMCPServerCredentials(ctx, r.Client, server)
	if step != nil || err != nil {
		return step, err
	}
	if server.Status.ConfigID == "" {
		if step, err = r.createOrRecord(ctx, sdk, server, org.ID, values, before); err != nil {
			return nil, err
		}
		if step == nil {
			// Store the ID before anything replaces the nonce: after that,
			// only the stored ID proves that this object created the config.
			if err := r.writeStatus(ctx, server, before); err != nil {
				return nil, err
			}
			*before = *server.Status.DeepCopy()
		}
	}
	if step == nil {
		step = mcpServerWait(mcpServerResync, "Pending",
			"MCP server config %s exists in Coder, disabled. This controller version does not apply the spec yet.", server.Status.ConfigID)
	}
	// Coder messages can echo the request, so scrub the credentials from them.
	all := make([]credentialsecret.Value, 0, len(values))
	for _, v := range values {
		all = append(all, v)
	}
	step.message = credentialsecret.Redact(step.message, all...)
	return step, nil
}

// resolveMCPServerOrganization looks up a Coder organization by name. Coder
// compares names without case.
func resolveMCPServerOrganization(ctx context.Context, sdk *codersdk.Client, name string) (codersdk.Organization, *mcpServerStep, error) {
	org, err := sdk.OrganizationByName(ctx, name)
	switch {
	case isCoderNotFound(err):
		return org, mcpServerWait(mcpServerResync, "OrganizationNotFound", "Coder organization %s does not exist.", name), nil
	case err != nil:
		return org, mcpServerWait(mcpServerRetry, "CoderUnavailable", "get organization %s: %s", name, coderErrorSummary(err)), nil
	}
	if err := coderAnswerFor("organization", strings.ToLower(name), strings.ToLower(org.Name)); err != nil {
		return org, mcpServerStalled(mcpServerResync, "CoderAnswerMismatch", "%s", err.Error()), nil
	}
	if org.ID == uuid.Nil {
		return org, nil, fmt.Errorf("assertion failed: Coder answered organization %s without an ID", name)
	}
	return org, nil, nil
}

// mcpServerScopeGate applies the control plane's opt-in (plan 4.4): the
// organization must resolve to the ID of a listed name, and force_on needs
// allowForceOn. A listed name that equals the organization name without case
// resolves to the same ID, so it needs no lookup.
func mcpServerScopeGate(
	ctx context.Context, sdk *codersdk.Client, controlPlane *coderv1alpha1.CoderControlPlane, spec coderv1alpha1.CoderMCPServerSpec, orgID uuid.UUID,
) (*mcpServerStep, error) {
	optIn := controlPlane.Spec.MCPServers
	if optIn == nil {
		return mcpServerStalled(0, "MCPServersNotAllowed", "CoderControlPlane %s does not set spec.mcpServers.", controlPlane.Name), nil
	}
	allowed := false
	for _, name := range optIn.Organizations {
		if strings.EqualFold(name, spec.Organization) {
			allowed = true
			break
		}
		listed, step, err := resolveMCPServerOrganization(ctx, sdk, name)
		if err != nil {
			return nil, err
		}
		if step != nil && step.reason == "OrganizationNotFound" {
			continue
		}
		if step != nil {
			return step, nil
		}
		if listed.ID == orgID {
			allowed = true
			break
		}
	}
	if !allowed {
		return mcpServerStalled(0, "MCPServersNotAllowed",
			"CoderControlPlane %s does not list Coder organization %s in spec.mcpServers.organizations.", controlPlane.Name, spec.Organization), nil
	}
	if spec.Availability == "force_on" && !optIn.AllowForceOn {
		return mcpServerStalled(0, "ForceOnNotAllowed", "CoderControlPlane %s does not set spec.mcpServers.allowForceOn.", controlPlane.Name), nil
	}
	return nil, nil
}

// mcpServerCredential is one credential field of the spec and the spec field
// of each URL that receives it (plan 5.2).
type mcpServerCredential struct {
	field        string // The status.credentials field name.
	ref          coderv1alpha1.SecretKeySelector
	destinations []string
	headerValue  bool // Coder sends the value as an HTTP header value.
}

func mcpServerCredentialRefs(auth coderv1alpha1.CoderMCPServerAuth) []mcpServerCredential {
	var out []mcpServerCredential
	if auth.APIKey != nil {
		out = append(out, mcpServerCredential{field: "apiKey", ref: auth.APIKey.SecretRef, destinations: []string{"spec.url"}, headerValue: true})
	}
	for _, h := range auth.CustomHeaders {
		out = append(out, mcpServerCredential{field: "header:" + h.Name, ref: h.SecretRef, destinations: []string{"spec.url"}, headerValue: true})
	}
	if o := auth.OAuth2; o != nil {
		destinations := []string{"spec.url", "spec.auth.oauth2.authURL", "spec.auth.oauth2.tokenURL"}
		if o.RevocationURL != "" {
			destinations = append(destinations, "spec.auth.oauth2.revocationURL")
		}
		out = append(out, mcpServerCredential{field: "oauth2ClientSecret", ref: o.ClientSecretRef, destinations: destinations})
	}
	return out
}

// mcpServerURLs parses every URL of the spec with the spec.baseURL rule of
// CoderAIProvider, so the checks hold without admission. The revocation URL
// must also be https.
func mcpServerURLs(spec coderv1alpha1.CoderMCPServerSpec) (map[string]*url.URL, *mcpServerStep) {
	raw := map[string]string{"spec.url": spec.URL}
	if o := spec.Auth.OAuth2; o != nil {
		raw["spec.auth.oauth2.authURL"], raw["spec.auth.oauth2.tokenURL"] = o.AuthURL, o.TokenURL
		if o.RevocationURL != "" {
			raw["spec.auth.oauth2.revocationURL"] = o.RevocationURL
		}
	}
	out := make(map[string]*url.URL, len(raw))
	for _, field := range []string{"spec.url", "spec.auth.oauth2.authURL", "spec.auth.oauth2.tokenURL", "spec.auth.oauth2.revocationURL"} {
		value, ok := raw[field]
		if !ok {
			continue
		}
		parsed, err := aiProviderBaseURL(value)
		if err == nil && field == "spec.auth.oauth2.revocationURL" && parsed.Scheme != "https" {
			err = errors.New("not https")
		}
		if err != nil {
			// A fixed message: the parse error repeats the URL, which can hold
			// user info.
			return nil, mcpServerStalled(0, credentialsecret.ReasonMalformed,
				"%s is not a URL that Coder accepts: it must be an http or https URL with a host, without user info, surrounding whitespace, or invalid percent escapes.", field)
		}
		out[field] = parsed
	}
	return out, nil
}

// readMCPServerCredentials reads every credential of the spec's auth type
// through the credential Secret contract, once per destination, and returns
// them by status.credentials field. All reads of one Secret must see the same
// Secret state, or the Secret changed during the reconcile (plan 5.2 rule 2).
func readMCPServerCredentials(
	ctx context.Context, reader client.Reader, server *coderv1alpha1.CoderMCPServer,
) (map[string]credentialsecret.Value, *mcpServerStep, error) {
	auth := server.Spec.Auth
	switch auth.Type {
	case "none", "api_key", "custom_headers", "oauth2":
	default:
		return nil, nil, fmt.Errorf("assertion failed: MCP server %s/%s has auth type %q", server.Namespace, server.Name, auth.Type)
	}
	if (auth.Type == "api_key") != (auth.APIKey != nil) || (auth.Type == "custom_headers") != (len(auth.CustomHeaders) > 0) || (auth.Type == "oauth2") != (auth.OAuth2 != nil) {
		return nil, nil, fmt.Errorf("assertion failed: MCP server %s/%s auth fields do not match auth type %q", server.Namespace, server.Name, auth.Type)
	}
	urls, step := mcpServerURLs(server.Spec)
	if step != nil {
		return nil, step, nil
	}
	values := map[string]credentialsecret.Value{}
	seen := map[string]credentialsecret.ChangeToken{}
	for _, c := range mcpServerCredentialRefs(auth) {
		for _, destination := range c.destinations {
			value, err := credentialsecret.Read(ctx, reader, server.Namespace, c.ref, urls[destination])
			if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
				return nil, mcpServerStalled(0, credErr.Reason, "%s (to %s): %s", c.field, destination, credErr.Message), nil
			} else if err != nil {
				return nil, nil, err
			}
			values[c.field] = value
			token := value.Token
			if first, ok := seen[c.ref.Name]; ok && (first.SecretUID != token.SecretUID || first.ResourceVersion != token.ResourceVersion) {
				return nil, mcpServerWait(mcpServerSecretChangedRetry, "CredentialSecretChanged",
					"Secret %s changed during the reconcile. The controller reads it again.", c.ref.Name), nil
			}
			seen[c.ref.Name] = token
			// Coder receives every credential inside a JSON string, and
			// encoding/json replaces invalid UTF-8 with U+FFFD, so Coder would
			// store another value.
			if !utf8.ValidString(value.Reveal()) {
				return nil, mcpServerStalled(0, credentialsecret.ReasonMalformed,
					"%s: the value of key %s in Secret %s is not valid UTF-8.", c.field, c.ref.Key, c.ref.Name), nil
			}
			if c.headerValue && !isHTTPFieldValue(value.Reveal()) {
				return nil, mcpServerStalled(0, credentialsecret.ReasonMalformed,
					"%s: the value of key %s in Secret %s is not a valid HTTP header value.", c.field, c.ref.Key, c.ref.Name), nil
			}
		}
	}
	return values, nil, nil
}

// isHTTPFieldValue reports whether every byte of s may appear in an RFC 9110
// field value: HTAB, SP, visible ASCII, or obs-text. CR, LF and NUL may not.
func isHTTPFieldValue(s string) bool {
	for i := range len(s) {
		if b := s[i]; b != '\t' && b != ' ' && (b < 0x21 || b == 0x7F) {
			return false
		}
	}
	return true
}

// findMCPServerSlug lists the organization's configs, because Coder has no
// lookup by slug, and returns the one with the spec's slug, or nil. Slugs are
// unique per organization, with case.
func findMCPServerSlug(
	ctx context.Context, sdk *codersdk.Client, server *coderv1alpha1.CoderMCPServer, orgID uuid.UUID,
) (*codersdk.MCPServerConfig, *mcpServerStep) {
	configs, err := sdk.MCPServerConfigs(ctx, orgID)
	if err != nil {
		return nil, mcpServerWait(mcpServerRetry, "CoderUnavailable", "list MCP server configs: %s", coderErrorSummary(err))
	}
	for i, config := range configs {
		if err := coderAnswerFor("MCP server config organization", orgID.String(), config.OrganizationID.String()); err != nil {
			return nil, mcpServerStalled(mcpServerResync, "CoderAnswerMismatch", "%s", err.Error())
		}
		if config.Slug != server.Spec.Slug {
			continue
		}
		if config.ID == uuid.Nil {
			return nil, mcpServerStalled(mcpServerResync, "CoderAnswerMismatch", "assertion failed: Coder answered the MCP server config with slug %s without an ID", config.Slug)
		}
		return &configs[i], nil
	}
	return nil, nil
}

// patchFinalizer adds or removes the cleanup finalizer with a merge patch. The
// optimistic lock turns a concurrent finalizer change into a conflict, not a
// lost update.
func (r *CoderMCPServerReconciler) patchFinalizer(ctx context.Context, server *coderv1alpha1.CoderMCPServer, add bool) error {
	base := server.DeepCopy()
	changed := controllerutil.RemoveFinalizer(server, coderv1alpha1.CoderMCPServerCleanupFinalizer)
	if add {
		changed = controllerutil.AddFinalizer(server, coderv1alpha1.CoderMCPServerCleanupFinalizer)
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, server, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch finalizer of MCP server %s/%s: %w", server.Namespace, server.Name, err)
	}
	return nil
}

// applyMCPServerStep sets the kstatus conditions for step. Messages are cut to
// the CoderTemplateTest limit.
func applyMCPServerStep(server *coderv1alpha1.CoderMCPServer, step *mcpServerStep) {
	message := truncateTemplateTestMessage(step.message)
	ready, reconciling, stalled := metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionFalse
	switch {
	case step.ready:
		ready, reconciling = metav1.ConditionTrue, metav1.ConditionFalse
	case step.stalled:
		reconciling, stalled = metav1.ConditionFalse, metav1.ConditionTrue
	}
	for _, c := range []struct {
		conditionType string
		status        metav1.ConditionStatus
	}{
		{coderv1alpha1.CoderMCPServerConditionReady, ready},
		{coderv1alpha1.CoderMCPServerConditionReconciling, reconciling},
		{coderv1alpha1.CoderMCPServerConditionStalled, stalled},
	} {
		meta.SetStatusCondition(&server.Status.Conditions, metav1.Condition{
			Type: c.conditionType, Status: c.status, ObservedGeneration: server.Generation, Reason: step.reason, Message: message,
		})
	}
}

// writeStatus writes the status only when it changed.
func (r *CoderMCPServerReconciler) writeStatus(ctx context.Context, server *coderv1alpha1.CoderMCPServer, before *coderv1alpha1.CoderMCPServerStatus) error {
	server.Status.ObservedGeneration = server.Generation
	if equality.Semantic.DeepEqual(before, &server.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, server); err != nil {
		return fmt.Errorf("update status of MCP server %s/%s: %w", server.Namespace, server.Name, err)
	}
	return nil
}
