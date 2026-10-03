package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

const (
	templateTestWorkspacePrefix  = "ktt-"
	templateTestUIDHexLength     = 28
	templateTestCoderTimeout     = 30 * time.Second
	templateTestPendingPoll      = 15 * time.Second
	templateTestRunningPoll      = 5 * time.Second
	templateTestDefaultTimeout   = int32(900)
	templateTestMaxMessageLength = 256
)

// +kubebuilder:rbac:groups=coder.com,resources=codertemplatetests,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=codertemplatetests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=codertemplatetests/finalizers,verbs=update

// CoderTemplateTestReconciler runs CoderTemplateTest objects (#152): it
// creates a throwaway workspace from a template version, waits until every
// agent is ready, records the result, and deletes the workspace.
type CoderTemplateTestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  clock.PassiveClock

	rateLimited sync.Map // types.NamespacedName -> consecutive HTTP 429 answers.
}

// templateTestControlPlaneRefIndex indexes tests by the control plane they use.
const templateTestControlPlaneRefIndex = ".spec.controlPlaneRef.name"

// SetupWithManager wires the reconciler into controller-runtime. Nothing
// calls it until activation (#152). A change of a control plane requeues the
// tests that use it, so cleanup that waits for Coder resumes at once.
func (r *CoderTemplateTestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil || r.Client == nil || r.Scheme == nil || r.Clock == nil {
		return fmt.Errorf("assertion failed: template test reconciler needs a manager, a client, a scheme, and a clock")
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &coderv1alpha1.CoderTemplateTest{}, templateTestControlPlaneRefIndex,
		func(obj client.Object) []string {
			if tt, ok := obj.(*coderv1alpha1.CoderTemplateTest); ok && tt.Spec.ControlPlaneRef.Name != "" {
				return []string{tt.Spec.ControlPlaneRef.Name}
			}
			return nil
		}); err != nil {
		return fmt.Errorf("index template tests by control plane: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderTemplateTest{}).
		Watches(&coderv1alpha1.CoderControlPlane{}, handler.EnqueueRequestsFromMapFunc(r.testsForControlPlane)).
		Named("codertemplatetest").
		Complete(r)
}

func (r *CoderTemplateTestReconciler) testsForControlPlane(ctx context.Context, obj client.Object) []reconcile.Request {
	var tests coderv1alpha1.CoderTemplateTestList
	if err := r.List(ctx, &tests, client.InNamespace(obj.GetNamespace()), client.MatchingFields{templateTestControlPlaneRefIndex: obj.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list template tests of a control plane", "controlPlane", obj.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tests.Items))
	for _, tt := range tests.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: tt.Namespace, Name: tt.Name}})
	}
	return requests
}

// templateTestStep is the outcome of a reconcile: a wait keeps the phase
// (Pending, or Running once a create request may exist), a failure makes the
// test Failed.
type templateTestStep struct {
	failed      bool
	reason      string
	message     string
	rateLimited bool // Coder answered 429: retry with backoff.
	succeeded   bool // The test passed and its workspace is deleted.
	// requeue overrides the default poll of a wait.
	requeue time.Duration
	// deleted overrides the WorkspaceDeleted condition of a failure.
	deleted *metav1.Condition
}

func templateTestWait(reason, format string, args ...any) *templateTestStep {
	return &templateTestStep{reason: reason, message: fmt.Sprintf(format, args...)}
}

func templateTestFail(reason, format string, args ...any) *templateTestStep {
	return &templateTestStep{failed: true, reason: reason, message: fmt.Sprintf(format, args...)}
}

// Reconcile runs the first step of the test that applies, then returns.
func (r *CoderTemplateTestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil || r.Scheme == nil || r.Clock == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: template test reconciler needs a client, a scheme, and a clock")
	}
	// Only a wait after HTTP 429 keeps the backoff count. Every other
	// outcome, including deletion and errors, forgets it.
	keepBackoff := false
	defer func() {
		if !keepBackoff {
			r.rateLimited.Delete(req.NamespacedName)
		}
	}()
	tt := &coderv1alpha1.CoderTemplateTest{}
	if err := r.Get(ctx, req.NamespacedName, tt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if tt.Name != req.Name || tt.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched template test %s/%s does not match request %s", tt.Namespace, tt.Name, req.NamespacedName)
	}
	workspaceName, err := templateTestWorkspaceName(tt.UID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tt.Status.WorkspaceName != "" && tt.Status.WorkspaceName != workspaceName {
		return ctrl.Result{}, fmt.Errorf("assertion failed: status.workspaceName %q is not %q", tt.Status.WorkspaceName, workspaceName)
	}
	// Deletion comes first: a test being deleted never starts new work.
	if !tt.DeletionTimestamp.IsZero() {
		return r.cleanup(ctx, tt)
	}
	final := isTemplateTestFinal(tt.Status.Phase)
	if (!final || !templateTestCleanupDone(tt)) && !controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer) {
		// No Coder call happens before the finalizer is stored.
		controllerutil.AddFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer)
		if err := r.Update(ctx, tt); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to template test %s/%s: %w", tt.Namespace, tt.Name, err)
		}
		return ctrl.Result{}, nil
	}

	now := r.Clock.Now()
	before := tt.Status.DeepCopy()
	switch {
	case tt.Status.Phase == "":
		tt.Status.Phase = coderv1alpha1.CoderTemplateTestPhasePending
		tt.Status.StartTime = &metav1.Time{Time: now}
		tt.Status.Reason, tt.Status.Message = "Initializing", "The controller started the test."
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionReconciling, metav1.ConditionTrue, tt.Status.Reason, tt.Status.Message)
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionReady, metav1.ConditionFalse, tt.Status.Reason, tt.Status.Message)
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionStalled, metav1.ConditionFalse, tt.Status.Reason, tt.Status.Message)
		return ctrl.Result{}, r.writeStatus(ctx, tt, before)
	case final:
		return r.cleanup(ctx, tt)
	}

	if tt.Status.StartTime == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: template test %s/%s is %s without status.startTime", tt.Namespace, tt.Name, tt.Status.Phase)
	}
	if templateTestMayHaveWorkspace(tt) {
		if gone, err := r.controlPlaneGone(ctx, tt); err != nil || gone {
			if err == nil {
				applyTemplateTestStep(tt, now, templateTestControlPlaneGone(tt))
				err = r.writeStatus(ctx, tt, before)
			}
			return ctrl.Result{}, err
		}
	}
	deadline := tt.Status.StartTime.Add(time.Duration(templateTestTimeoutSeconds(tt)) * time.Second)
	if !now.Before(deadline) {
		applyTemplateTestStep(tt, now, templateTestDeadlineExceeded(tt, tt.Status.Reason, tt.Status.Message))
		return ctrl.Result{}, r.writeStatus(ctx, tt, before)
	}

	sdk, controlPlane, step, err := r.coderClient(ctx, tt)
	if err == nil && step == nil {
		switch {
		case tt.Status.WorkspaceID != "":
			step, err = r.checkReadiness(ctx, sdk, tt, now)
		case tt.Status.CreateAttemptTime != nil:
			// A create request may exist: read Coder, never send it again.
			step, err = r.confirmCreate(ctx, sdk, tt, now)
		default:
			step, err = r.resolveInputs(ctx, sdk, controlPlane, tt, workspaceName)
		}
	}
	if step, err = waitOnWrongAnswer(ctx, step, err); err != nil {
		return ctrl.Result{}, err
	}
	// Coder calls can take up to their timeout, so check the deadline again.
	if now = r.Clock.Now(); !now.Before(deadline) && (step == nil || !step.failed) {
		last := cmp.Or(step, &templateTestStep{reason: tt.Status.Reason, message: tt.Status.Message})
		step = templateTestDeadlineExceeded(tt, last.reason, last.message)
	}
	if step == nil {
		// Every input is resolved and pinned: write the marker, then create.
		step, err = r.createWorkspace(ctx, sdk, tt, before, now, deadline)
		if step, err = waitOnWrongAnswer(ctx, step, err); err != nil {
			return ctrl.Result{}, err
		}
		// The create request can take up to its timeout.
		if now = r.Clock.Now(); !step.failed && !now.Before(deadline) {
			step = templateTestDeadlineExceeded(tt, step.reason, step.message)
		}
	}
	applyTemplateTestStep(tt, now, step)
	if err := r.writeStatus(ctx, tt, before); err != nil {
		return ctrl.Result{}, err
	}
	if step.failed {
		return ctrl.Result{}, nil
	}
	keepBackoff = step.rateLimited
	return ctrl.Result{RequeueAfter: min(r.retryAfter(req.NamespacedName, tt, step), deadline.Sub(now))}, nil
}

// waitOnWrongAnswer turns a wrong Coder answer into a wait. A wrong answer
// means Coder, a proxy, or the SDK is broken. The controller logs it as an
// error and keeps the test waiting, so the test's deadline ends it instead of
// the work queue's backoff, which grows to about 17 minutes.
func waitOnWrongAnswer(ctx context.Context, step *templateTestStep, err error) (*templateTestStep, error) {
	var answerErr *coderAnswerError
	if !errors.As(err, &answerErr) {
		return step, err
	}
	log.FromContext(ctx).Error(err, "Coder answered something other than the controller asked for")
	return templateTestWait("CoderAnswerMismatch", "%v", answerErr), nil
}

// resolveInputs runs the Pending lookups and pins their results in status.
// A nil step means every input is resolved and the workspace name is free.
func (r *CoderTemplateTestReconciler) resolveInputs(
	ctx context.Context, sdk *codersdk.Client, controlPlane *coderv1alpha1.CoderControlPlane, tt *coderv1alpha1.CoderTemplateTest, workspaceName string,
) (*templateTestStep, error) {
	if controlPlane.Spec.TemplateTests == nil || controlPlane.Spec.TemplateTests.OwnerUserID == "" {
		return templateTestWait("OwnerNotConfigured", "Set spec.templateTests.ownerUserID on CoderControlPlane %s.", controlPlane.Name), nil
	}
	ownerID, err := uuid.Parse(controlPlane.Spec.TemplateTests.OwnerUserID)
	if err != nil {
		return templateTestWait("OwnerNotConfigured", "spec.templateTests.ownerUserID on CoderControlPlane %s: %v", controlPlane.Name, err), nil
	}
	owner, step, err := checkTemplateTestOwner(ctx, sdk, ownerID)
	if step != nil || err != nil {
		return step, err
	}

	orgName, templateName, err := coder.ParseTemplateName(tt.Spec.Template)
	if err != nil {
		return nil, fmt.Errorf("assertion failed: spec.template %q passed validation but does not parse: %w", tt.Spec.Template, err)
	}
	org, err := sdk.OrganizationByName(ctx, orgName)
	if isCoderNotFound(err) {
		return templateTestWait("TemplateNotFound", "Coder organization %q does not exist.", orgName), nil
	} else if err != nil {
		return coderUnavailable("get organization", err), nil
	}
	if err := coderAnswerFor("organization", strings.ToLower(orgName), strings.ToLower(org.Name)); err != nil {
		return nil, err
	}
	if !slices.Contains(owner.OrganizationIDs, org.ID) {
		return templateTestWait("OwnerNotEligible", "Coder user %s is not a member of organization %q.", ownerID, orgName), nil
	}
	template, err := sdk.TemplateByName(ctx, org.ID, templateName)
	if isCoderNotFound(err) {
		return templateTestWait("TemplateNotFound", "Coder template %q does not exist.", tt.Spec.Template), nil
	} else if err != nil {
		return coderUnavailable("get template", err), nil
	}
	if err := errors.Join(coderAnswerFor("template", strings.ToLower(templateName), strings.ToLower(template.Name)),
		coderAnswerFor("template organization", org.ID.String(), template.OrganizationID.String())); err != nil {
		return nil, err
	}
	if template.Deprecated {
		// Coder v2.37.2 refuses new workspaces for a deprecated template
		// (coderd/workspaces.go:958). The deprecation message is Coder
		// detail, so status leaves it out.
		return templateTestFail("TemplateDeprecated", "Coder template %q is deprecated and accepts no new workspaces.", tt.Spec.Template), nil
	}
	version, step, err := resolveTemplateTestVersion(ctx, sdk, tt, template)
	if step != nil || err != nil {
		return step, err
	}

	if version.ID.String() != tt.Status.TemplateVersionID {
		return nil, fmt.Errorf("assertion failed: resolved version %s is not the pinned version %s", version.ID, tt.Status.TemplateVersionID)
	}
	tt.Status.OrganizationID, tt.Status.TemplateID, tt.Status.OwnerID = org.ID.String(), template.ID.String(), ownerID.String()
	tt.Status.WorkspaceName = workspaceName

	_, err = sdk.WorkspaceByOwnerAndName(ctx, ownerID.String(), workspaceName, codersdk.WorkspaceOptions{})
	switch {
	case err == nil:
		// Nothing of this test exists yet, so the workspace belongs to
		// someone else. The controller never touches it.
		return templateTestFail("WorkspaceNameConflict", "Coder user %s already has a workspace named %s.", ownerID, workspaceName), nil
	case !isCoderNotFound(err):
		return coderUnavailable("get workspace by name", err), nil
	}
	return nil, nil
}

// coderClient reads the referenced control plane and its operator token.
func (r *CoderTemplateTestReconciler) coderClient(ctx context.Context, tt *coderv1alpha1.CoderTemplateTest) (*codersdk.Client, *coderv1alpha1.CoderControlPlane, *templateTestStep, error) {
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	key := types.NamespacedName{Namespace: tt.Namespace, Name: tt.Spec.ControlPlaneRef.Name}
	if err := r.Get(ctx, key, controlPlane); apierrors.IsNotFound(err) {
		return nil, nil, templateTestWait("ControlPlaneNotReady", "CoderControlPlane %s does not exist.", key.Name), nil
	} else if err != nil {
		return nil, nil, nil, fmt.Errorf("get codercontrolplane %s: %w", key, err)
	}
	switch {
	case !controlPlane.DeletionTimestamp.IsZero():
		return nil, nil, templateTestWait("ControlPlaneNotReady", "CoderControlPlane %s is being deleted.", key.Name), nil
	case controlPlane.Status.URL == "":
		return nil, nil, templateTestWait("ControlPlaneNotReady", "CoderControlPlane %s has no status.url yet.", key.Name), nil
	case !controlPlane.Status.OperatorAccessReady || controlPlane.Status.OperatorTokenSecretRef == nil:
		return nil, nil, templateTestWait("OperatorAccessNotReady", "CoderControlPlane %s has no operator access yet.", key.Name), nil
	}
	ref := controlPlane.Status.OperatorTokenSecretRef
	secretKey := strings.TrimSpace(ref.Key)
	if secretKey == "" {
		secretKey = coderv1alpha1.DefaultTokenSecretKey
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: tt.Namespace, Name: strings.TrimSpace(ref.Name)}, secret); client.IgnoreNotFound(err) != nil {
		return nil, nil, nil, fmt.Errorf("read operator token of codercontrolplane %s: %w", key, err)
	}
	token := string(secret.Data[secretKey])
	if token == "" {
		return nil, nil, templateTestWait("OperatorAccessNotReady", "The operator token Secret of CoderControlPlane %s has no token yet.", key.Name), nil
	}
	coderURL, err := templateTestCoderURL(controlPlane)
	if err != nil {
		return nil, nil, templateTestWait("ControlPlaneNotReady", "CoderControlPlane %s has no usable Coder URL: %v", key.Name, err), nil
	}
	sdk, err := coder.NewSDKClient(coder.Config{CoderURL: coderURL, SessionToken: token, RequestTimeout: templateTestCoderTimeout})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create Coder client for codercontrolplane %s: %w", key, err)
	}
	return sdk, controlPlane, nil, nil
}

// checkTemplateTestOwner refuses owners whose token is worth more than a plain
// workspace user's: every start build hands the owner's token to Terraform.
func checkTemplateTestOwner(ctx context.Context, sdk *codersdk.Client, ownerID uuid.UUID) (codersdk.User, *templateTestStep, error) {
	refuse := func(format string, args ...any) *templateTestStep {
		return templateTestWait("OwnerNotEligible", "Coder user %s: %s", ownerID, fmt.Sprintf(format, args...))
	}
	user, err := sdk.User(ctx, ownerID.String())
	if isCoderNotFound(err) {
		return user, refuse("the user does not exist."), nil
	} else if err != nil {
		return user, coderUnavailable("get owner", err), nil
	}
	if err := coderAnswerFor("user", ownerID.String(), user.ID.String()); err != nil {
		return user, nil, err
	}
	// Coder refuses a dormant owner's agents (401), so their test could only
	// time out. Coder creates users through the API as dormant.
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
			return user, coderUnavailable("get owner organization", err), nil
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
			return user, coderUnavailable("get owner membership", err), nil
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

// resolveTemplateTestVersion returns the version under test. Once a version
// is pinned, the controller reads that version and never resolves again.
func resolveTemplateTestVersion(ctx context.Context, sdk *codersdk.Client, tt *coderv1alpha1.CoderTemplateTest, template codersdk.Template) (codersdk.TemplateVersion, *templateTestStep, error) {
	var version codersdk.TemplateVersion
	var err error
	var asked func() error // The rule for the answer: it names what was asked for.
	switch id := cmp.Or(tt.Status.TemplateVersionID, tt.Spec.Version.ID); {
	case id != "":
		parsed, parseErr := uuid.Parse(id)
		if parseErr != nil {
			// The CRD and this controller store only UUIDs here.
			return version, nil, fmt.Errorf("assertion failed: version ID %q is not a UUID: %w", id, parseErr)
		}
		version, err = sdk.TemplateVersion(ctx, parsed)
		asked = func() error { return coderAnswerFor("version", parsed.String(), version.ID.String()) }
	case tt.Spec.Version.Name != "":
		version, err = sdk.TemplateVersionByName(ctx, template.ID, tt.Spec.Version.Name)
		asked = func() error { return coderAnswerFor("version", tt.Spec.Version.Name, version.Name) }
	default:
		version, err = sdk.TemplateVersion(ctx, template.ActiveVersionID)
		asked = func() error { return coderAnswerFor("version", template.ActiveVersionID.String(), version.ID.String()) }
	}
	switch {
	case isCoderNotFound(err):
		return version, templateTestWait("TemplateVersionNotFound", "The version of template %q does not exist.", tt.Spec.Template), nil
	case err != nil:
		return version, coderUnavailable("get template version", err), nil
	}
	if err := asked(); err != nil {
		return version, nil, err
	}
	if version.TemplateID == nil || *version.TemplateID != template.ID {
		return version, templateTestFail("TemplateVersionMismatch", "Version %s does not belong to template %q.", version.ID, tt.Spec.Template), nil
	}
	// Pin the version as soon as Coder names it, even while it imports, so a
	// later promotion never changes the version under test.
	tt.Status.TemplateVersionID, tt.Status.TemplateVersionName = version.ID.String(), version.Name
	switch {
	case version.Archived:
		return version, templateTestFail("TemplateVersionArchived", "Version %s is archived.", version.Name), nil
	}
	switch version.Job.Status {
	case codersdk.ProvisionerJobSucceeded:
		return version, nil, nil
	case codersdk.ProvisionerJobPending, codersdk.ProvisionerJobRunning:
		return version, templateTestWait("TemplateVersionImporting", "Version %s is still importing.", version.Name), nil
	default:
		return version, templateTestFail("TemplateVersionImportFailed", "The import of version %s ended %s.", version.Name, version.Job.Status), nil
	}
}

func applyTemplateTestStep(tt *coderv1alpha1.CoderTemplateTest, now time.Time, step *templateTestStep) {
	tt.Status.Reason, tt.Status.Message = step.reason, step.message
	if step.succeeded {
		// Plan 2.3 step 11: Succeeded only after the delete build succeeded.
		tt.Status.Phase = coderv1alpha1.CoderTemplateTestPhaseSucceeded
		tt.Status.CompletionTime = &metav1.Time{Time: now}
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionReconciling, metav1.ConditionFalse, step.reason, step.message)
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionReady, metav1.ConditionTrue, step.reason, step.message)
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionStalled, metav1.ConditionFalse, step.reason, step.message)
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted, metav1.ConditionTrue, "Deleted", step.message)
		return
	}
	if !step.failed {
		tt.Status.Phase = coderv1alpha1.CoderTemplateTestPhasePending
		if templateTestMayHaveWorkspace(tt) {
			tt.Status.Phase = coderv1alpha1.CoderTemplateTestPhaseRunning
		}
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionReconciling, metav1.ConditionTrue, step.reason, step.message)
		return
	}
	tt.Status.Phase = coderv1alpha1.CoderTemplateTestPhaseFailed
	tt.Status.CompletionTime = &metav1.Time{Time: now}
	setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionReconciling, metav1.ConditionFalse, step.reason, step.message)
	setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionReady, metav1.ConditionFalse, step.reason, step.message)
	setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionStalled, metav1.ConditionTrue, step.reason, step.message)
	switch {
	case step.deleted != nil:
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted, step.deleted.Status, step.deleted.Reason, step.deleted.Message)
	case templateTestMayHaveWorkspace(tt):
		setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted, metav1.ConditionFalse,
			"CleanupPending", "The workspace can exist. The controller deletes it.")
	default:
		markTemplateTestNotCreated(tt, now)
	}
}

// templateTestMayHaveWorkspace reports whether a create request was possibly
// sent, so a workspace of this test can exist in Coder.
func templateTestMayHaveWorkspace(tt *coderv1alpha1.CoderTemplateTest) bool {
	return tt.Status.CreateAttemptTime != nil || tt.Status.WorkspaceID != ""
}

// markTemplateTestNotCreated records that no create request was ever sent.
func markTemplateTestNotCreated(tt *coderv1alpha1.CoderTemplateTest, now time.Time) {
	setTemplateTestCondition(tt, now, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted, metav1.ConditionTrue,
		"NotCreated", "The controller never sent a workspace create request.")
}

// writeStatus writes changed status with optimistic concurrency, so a
// decision made on a stale object never persists.
func (r *CoderTemplateTestReconciler) writeStatus(ctx context.Context, tt *coderv1alpha1.CoderTemplateTest, before *coderv1alpha1.CoderTemplateTestStatus) error {
	tt.Status.ObservedGeneration = tt.Generation
	tt.Status.Message = truncateTemplateTestMessage(tt.Status.Message)
	if equality.Semantic.DeepEqual(before, &tt.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, tt); err != nil {
		return fmt.Errorf("update status of template test %s/%s: %w", tt.Namespace, tt.Name, err)
	}
	return nil
}

func (r *CoderTemplateTestReconciler) releaseFinalizer(ctx context.Context, tt *coderv1alpha1.CoderTemplateTest) error {
	if !controllerutil.RemoveFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer) {
		return nil
	}
	if err := r.Update(ctx, tt); err != nil {
		return fmt.Errorf("remove finalizer from template test %s/%s: %w", tt.Namespace, tt.Name, err)
	}
	return nil
}

// templateTestCoderURL returns the URL for Coder calls. With TLS, status.url
// is HTTPS on the service name, which certificates rarely cover, so the
// controller uses the internal HTTP URL like the control-plane controller.
func templateTestCoderURL(controlPlane *coderv1alpha1.CoderControlPlane) (*url.URL, error) {
	statusURL, err := url.Parse(controlPlane.Status.URL)
	if err != nil || statusURL.Scheme != "https" {
		return statusURL, err
	}
	internalURL := controlPlaneSDKURL(controlPlane)
	if internalURL == "" {
		return nil, errors.New("no internal HTTP URL")
	}
	return url.Parse(internalURL)
}

// templateTestWorkspaceName derives the Coder workspace name from the object
// UID, so no other test can use it: "ktt-" and 28 hex characters (32 in all,
// Coder's limit).
func templateTestWorkspaceName(uid types.UID) (string, error) {
	hex := strings.ReplaceAll(string(uid), "-", "")
	if len(hex) < templateTestUIDHexLength || strings.Trim(hex, "0123456789abcdef") != "" {
		return "", fmt.Errorf("assertion failed: object UID %q needs at least %d lowercase hex characters", uid, templateTestUIDHexLength)
	}
	return templateTestWorkspacePrefix + hex[:templateTestUIDHexLength], nil
}

// coderAnswerFor is the rule for every Coder answer the reconciler trusts:
// the answer names the ID or name that the controller asked for. Anything else
// means Coder, a proxy, or the SDK is broken, so it is an assertion failure.
func coderAnswerFor(kind, asked, answered string) error {
	if asked == answered {
		return nil
	}
	return &coderAnswerError{msg: fmt.Sprintf("assertion failed: Coder answered %s %q when asked for %q", kind, answered, asked)}
}

type coderAnswerError struct{ msg string }

func (e *coderAnswerError) Error() string { return e.msg }

func templateTestDeadlineExceeded(tt *coderv1alpha1.CoderTemplateTest, lastReason, lastMessage string) *templateTestStep {
	return templateTestFail("DeadlineExceeded", "The test did not finish within %ds. Last wait: %s: %s",
		templateTestTimeoutSeconds(tt), lastReason, lastMessage)
}

func templateTestTimeoutSeconds(tt *coderv1alpha1.CoderTemplateTest) int32 {
	if tt.Spec.TimeoutSeconds == nil {
		return templateTestDefaultTimeout
	}
	return *tt.Spec.TimeoutSeconds
}

func isTemplateTestFinal(phase string) bool {
	return phase == coderv1alpha1.CoderTemplateTestPhaseSucceeded || phase == coderv1alpha1.CoderTemplateTestPhaseFailed
}

func isCoderNotFound(err error) bool {
	return coderStatus(err) == http.StatusNotFound
}

// coderUnavailable keeps the test waiting after a failed Coder call. The
// message holds the status and Coder's short message only, never details.
func coderUnavailable(action string, err error) *templateTestStep {
	step := templateTestWait("CoderUnavailable", "%s: %s", action, coderErrorSummary(err))
	step.rateLimited = coderStatus(err) == http.StatusTooManyRequests
	return step
}

// coderErrorSummary holds the status and Coder's short message only, never
// details, validation errors, or job logs.
func coderErrorSummary(err error) string {
	var sdkErr *codersdk.Error
	if errors.As(err, &sdkErr) {
		return fmt.Sprintf("Coder answered %d: %s", sdkErr.StatusCode(), sdkErr.Message)
	}
	return err.Error()
}

// coderStatus returns the HTTP status of a Coder answer, or 0 without one.
func coderStatus(err error) int {
	var sdkErr *codersdk.Error
	if errors.As(err, &sdkErr) {
		return sdkErr.StatusCode()
	}
	return 0
}

func truncateTemplateTestMessage(message string) string {
	if len(message) <= templateTestMaxMessageLength {
		return message
	}
	return strings.ToValidUTF8(message[:templateTestMaxMessageLength], "")
}

// setTemplateTestCondition sets a condition. A status change takes its
// transition time from now, the controller clock, which the TTL counts from.
func setTemplateTestCondition(
	tt *coderv1alpha1.CoderTemplateTest, now time.Time, conditionType string, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&tt.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, ObservedGeneration: tt.Generation,
		Reason: reason, Message: truncateTemplateTestMessage(message), LastTransitionTime: metav1.NewTime(now),
	})
}
