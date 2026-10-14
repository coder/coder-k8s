package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
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
	"sigs.k8s.io/controller-runtime/pkg/log"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	poolMemberWorkspacePrefix = "kwp-"
	poolMemberPendingPoll     = 15 * time.Second
)

// CoderWorkspacePoolMemberReconciler makes the Coder workspace of one
// CoderWorkspacePoolMember (#153 plan 6.2). Nothing registers it yet. This
// version runs the checks before the create request. The create request,
// readiness, the watch rules, and cleanup follow before activation.
type CoderWorkspacePoolMemberReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  clock.PassiveClock

	rateLimited coderBackoff // Keyed by types.NamespacedName.
}

// Reconcile runs the first step of the member that applies, then returns.
func (r *CoderWorkspacePoolMemberReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil || r.Scheme == nil || r.Clock == nil {
		return ctrl.Result{}, errors.New("assertion failed: pool member reconciler needs a client, a scheme, and a clock")
	}
	// Only a wait after HTTP 429 keeps the backoff count.
	keepBackoff := false
	defer func() {
		if !keepBackoff {
			r.rateLimited.forget(req.NamespacedName)
		}
	}()
	member := &coderv1alpha1.CoderWorkspacePoolMember{}
	if err := r.Get(ctx, req.NamespacedName, member); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if _, err := assertPoolMember(member, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	}
	if !member.DeletionTimestamp.IsZero() {
		// The delete steps (plan 6.5) come later. Until then, a member whose
		// workspace can exist keeps its finalizer.
		if poolMemberMayHaveWorkspace(member) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.releaseFinalizer(ctx, member)
	}
	if member.Status.Phase == coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed {
		return ctrl.Result{}, nil // Final: the pool deletes failed members.
	}
	if !controllerutil.ContainsFinalizer(member, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer) {
		// No Coder call happens before the finalizer is stored.
		controllerutil.AddFinalizer(member, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer)
		if err := r.Update(ctx, member); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to pool member %s: %w", req.NamespacedName, err)
		}
		return ctrl.Result{}, nil
	}

	now := r.Clock.Now()
	before := member.Status.DeepCopy()
	deadline := member.CreationTimestamp.Add(time.Duration(member.Spec.ReadyTimeoutSeconds) * time.Second)
	var step *coderStep
	switch {
	case !now.Before(deadline):
		step = poolMemberDeadlineExceeded(member, member.Status.Reason, member.Status.Message)
	case poolMemberMayHaveWorkspace(member):
		// A create request may exist: never send it again. Confirming an
		// uncertain result and the readiness rules come later (plan 6.2
		// steps 7 and 8).
		step = coderWait(cmp.Or(member.Status.Reason, "ConfirmingCreate"), "%s", member.Status.Message)
	default:
		// The create request needs the client that checkCreate returns.
		// It comes in the next version of this controller.
		var err error
		_, step, err = r.checkCreate(ctx, member)
		if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
			return ctrl.Result{}, err
		}
		if now = r.Clock.Now(); step == nil && now.Before(deadline) {
			step = coderWait("CreatePending", "Every check passed. This controller version sends no create request.")
		}
		// Coder calls can take up to their timeout, so check the deadline again.
		if last := cmp.Or(step, &coderStep{reason: member.Status.Reason, message: member.Status.Message}); !last.failed && !now.Before(deadline) {
			step = poolMemberDeadlineExceeded(member, last.reason, last.message)
		}
	}
	applyPoolMemberStep(member, now, step)
	if err := r.writeStatus(ctx, member, before); err != nil {
		return ctrl.Result{}, err
	}
	if step.failed {
		return ctrl.Result{}, nil
	}
	keepBackoff = step.rateLimited
	return ctrl.Result{RequeueAfter: min(r.retryAfter(req.NamespacedName, member, step), deadline.Sub(now))}, nil
}

// assertPoolMember checks what the API and the controller guarantee, and
// returns the member's workspace name.
func assertPoolMember(member *coderv1alpha1.CoderWorkspacePoolMember, req types.NamespacedName) (string, error) {
	if member.Name != req.Name || member.Namespace != req.Namespace {
		return "", fmt.Errorf("assertion failed: fetched pool member %s/%s does not match request %s", member.Namespace, member.Name, req)
	}
	workspaceName, err := uidWorkspaceName(poolMemberWorkspacePrefix, member.UID)
	if err != nil {
		return "", err
	}
	if member.Status.WorkspaceName != "" && member.Status.WorkspaceName != workspaceName {
		return "", fmt.Errorf("assertion failed: status.workspaceName %q is not %q", member.Status.WorkspaceName, workspaceName)
	}
	spec := member.Spec
	for _, id := range []string{spec.OwnerID, spec.OrganizationID, spec.TemplateID, spec.TemplateVersionID, cmp.Or(spec.PresetID, uuid.Nil.String())} {
		if _, err := uuid.Parse(id); err != nil {
			return "", fmt.Errorf("assertion failed: pool member %s pins %q, which is not a UUID: %w", req, id, err)
		}
	}
	if spec.ReadyTimeoutSeconds <= 0 {
		return "", fmt.Errorf("assertion failed: pool member %s has readyTimeoutSeconds %d", req, spec.ReadyTimeoutSeconds)
	}
	return workspaceName, nil
}

// checkCreate runs the ownership gate and the consent check (plan 6.2 steps
// 4 and 5). A nil step means the member can create its workspace with sdk.
// The checks of the pinned template, version, preset, and workspace name
// come with the create request.
func (r *CoderWorkspacePoolMemberReconciler) checkCreate(
	ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember,
) (*codersdk.Client, *coderStep, error) {
	spec := member.Spec
	// Only a member that its pool controls can create. The member keeps no
	// marker here, so a failure proves that no workspace exists.
	owner := metav1.GetControllerOf(member)
	if owner == nil || owner.APIVersion != coderv1alpha1.GroupVersion.String() || owner.Kind != "CoderWorkspacePool" || owner.Name != spec.PoolName {
		return nil, coderFail("NotOwned", "CoderWorkspacePool %s does not control this member, so it creates no workspace.", spec.PoolName), nil
	}
	pool := &coderv1alpha1.CoderWorkspacePool{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: spec.PoolName}, pool); apierrors.IsNotFound(err) {
		return nil, coderWait("PoolNotFound", "CoderWorkspacePool %s does not exist.", spec.PoolName), nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("get coderworkspacepool %s/%s: %w", member.Namespace, spec.PoolName, err)
	}
	switch {
	case pool.UID != owner.UID:
		return nil, coderFail("NotOwned", "CoderWorkspacePool %s is not the pool that created this member.", spec.PoolName), nil
	case !pool.DeletionTimestamp.IsZero():
		return nil, coderWait("PoolDeleting", "CoderWorkspacePool %s is being deleted.", spec.PoolName), nil
	}

	sdk, controlPlane, wait, err := controlPlaneCoderClient(ctx, r.Client, member.Namespace, spec.ControlPlaneRef.Name)
	if wait != nil || err != nil {
		if wait != nil {
			return nil, coderWait(wait.reason, "%s", wait.message), nil
		}
		return nil, nil, err
	}
	if step := workspacePoolConsent(controlPlane, spec.OwnerID, pool.Spec.Template); step != nil {
		return nil, step, nil
	}
	_, step, err := checkCoderWorkspaceOwner(ctx, sdk, uuid.MustParse(spec.OwnerID))
	if step != nil || err != nil {
		return nil, step, err
	}
	return sdk, nil, nil
}

// workspacePoolConsent checks the admin's consent in the control plane's
// spec.workspacePools: the owner is listed, with the template. A refusal is
// a wait, because the admin can still add them.
func workspacePoolConsent(controlPlane *coderv1alpha1.CoderControlPlane, ownerID, template string) *coderStep {
	var owners []coderv1alpha1.WorkspacePoolOwner
	if controlPlane.Spec.WorkspacePools != nil {
		owners = controlPlane.Spec.WorkspacePools.Owners
	}
	i := slices.IndexFunc(owners, func(o coderv1alpha1.WorkspacePoolOwner) bool { return o.UserID == ownerID })
	if i < 0 {
		return coderWait("OwnerNotAllowed", "CoderControlPlane %s does not list Coder user %s in spec.workspacePools.owners.", controlPlane.Name, ownerID)
	}
	// Coder matches organization and template names without case.
	if !slices.ContainsFunc(owners[i].Templates, func(t string) bool { return strings.EqualFold(t, template) }) {
		return coderWait("TemplateNotAllowed", "CoderControlPlane %s does not allow template %q for Coder user %s.", controlPlane.Name, template, ownerID)
	}
	return nil
}

// memberWaitOnWrongAnswer turns a wrong Coder answer into a wait, as
// waitOnWrongAnswer does for tests, so the member's deadline ends it.
func memberWaitOnWrongAnswer(ctx context.Context, step *coderStep, err error) (*coderStep, error) {
	var answerErr *coderAnswerError
	if !errors.As(err, &answerErr) {
		return step, err
	}
	log.FromContext(ctx).Error(err, "Coder answered something other than the controller asked for")
	return coderWait("CoderAnswerMismatch", "%v", answerErr), nil
}

func poolMemberDeadlineExceeded(member *coderv1alpha1.CoderWorkspacePoolMember, lastReason, lastMessage string) *coderStep {
	return coderFail("DeadlineExceeded", "The workspace was not ready within %ds. Last wait: %s: %s", member.Spec.ReadyTimeoutSeconds, lastReason, lastMessage)
}

// poolMemberMayHaveWorkspace reports whether a create request was possibly
// sent, so a workspace of this member can exist in Coder.
func poolMemberMayHaveWorkspace(member *coderv1alpha1.CoderWorkspacePoolMember) bool {
	return member.Status.CreateAttemptTime != nil || member.Status.WorkspaceID != ""
}

func applyPoolMemberStep(member *coderv1alpha1.CoderWorkspacePoolMember, now time.Time, step *coderStep) {
	member.Status.Reason, member.Status.Message = step.reason, step.message
	setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionReady, metav1.ConditionFalse, step.reason, step.message)
	switch {
	case !step.failed && poolMemberMayHaveWorkspace(member):
		member.Status.Phase = coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating
	case !step.failed:
		member.Status.Phase = coderv1alpha1.CoderWorkspacePoolMemberPhasePending
	case poolMemberMayHaveWorkspace(member):
		member.Status.Phase = coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed
		setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, metav1.ConditionFalse,
			"CleanupPending", "The workspace can exist. The controller deletes it.")
	default:
		member.Status.Phase = coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed
		setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, metav1.ConditionTrue,
			"NotCreated", "The controller never sent a workspace create request, or Coder refused it.")
	}
}

func setPoolMemberCondition(member *coderv1alpha1.CoderWorkspacePoolMember, now time.Time, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&member.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, ObservedGeneration: member.Generation,
		Reason: reason, Message: truncateTemplateTestMessage(message), LastTransitionTime: metav1.NewTime(now),
	})
}

// writeStatus writes changed status with optimistic concurrency, so a
// decision made on a stale object never persists.
func (r *CoderWorkspacePoolMemberReconciler) writeStatus(
	ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember, before *coderv1alpha1.CoderWorkspacePoolMemberStatus,
) error {
	member.Status.Message = truncateTemplateTestMessage(member.Status.Message)
	if equality.Semantic.DeepEqual(before, &member.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, member); err != nil {
		return fmt.Errorf("update status of pool member %s/%s: %w", member.Namespace, member.Name, err)
	}
	return nil
}

func (r *CoderWorkspacePoolMemberReconciler) releaseFinalizer(ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember) error {
	if !controllerutil.RemoveFinalizer(member, coderv1alpha1.CoderWorkspacePoolMemberCleanupFinalizer) {
		return nil
	}
	if err := r.Update(ctx, member); err != nil {
		return fmt.Errorf("remove finalizer from pool member %s/%s: %w", member.Namespace, member.Name, err)
	}
	return nil
}

// retryAfter returns the requeue delay of a wait: the HTTP 429 backoff, the
// build poll once a create request may exist, else the pending poll.
func (r *CoderWorkspacePoolMemberReconciler) retryAfter(key types.NamespacedName, member *coderv1alpha1.CoderWorkspacePoolMember, step *coderStep) time.Duration {
	switch {
	case step.rateLimited:
		return r.rateLimited.next(key)
	case poolMemberMayHaveWorkspace(member):
		return coderBuildPoll
	default:
		return poolMemberPendingPoll
	}
}
