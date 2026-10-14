package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
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
// version runs the steps up to the create request: readiness, the watch
// rules, and cleanup follow before activation.
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
	workspaceName, err := assertPoolMember(member, req.NamespacedName)
	if err != nil {
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
	switch member.Status.Phase {
	case coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed:
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
	case member.Status.ReadyTime == nil && !now.Before(deadline):
		// The deadline bounds the time until ready, not the life of the workspace.
		step = poolMemberDeadlineExceeded(member, member.Status.Reason, member.Status.Message)
	case poolMemberMayHaveWorkspace(member):
		// A create request may exist: read Coder, never send it again.
		step, err = r.readWorkspace(ctx, member, before, workspaceName, now)
		if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
			return ctrl.Result{}, err
		}
		// Coder calls can take up to their timeout, so check the deadline
		// again. A workspace that became ready in time stays ready.
		if now = r.Clock.Now(); !step.failed && member.Status.ReadyTime == nil && !now.Before(deadline) {
			step = poolMemberDeadlineExceeded(member, step.reason, step.message)
		}
	default:
		var sdk *codersdk.Client
		sdk, step, err = r.checkCreate(ctx, member, workspaceName)
		if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
			return ctrl.Result{}, err
		}
		if now = r.Clock.Now(); step == nil && now.Before(deadline) {
			// Every check passed: write the marker, then create.
			step, err = r.createWorkspace(ctx, sdk, member, before, now, deadline)
			if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
				return ctrl.Result{}, err
			}
			now = r.Clock.Now()
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
	keepBackoff = step.rateLimited && !step.failed
	switch {
	case step.failed, member.Spec.ClaimRef != nil && member.Status.BindExtended,
		meta.IsStatusConditionTrue(member.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed):
		// The watch rules of a bound member (plan 6.2 step 11 rule 3) come
		// with claims. A used member waits for a person to delete it.
		return ctrl.Result{}, nil
	case member.Status.Phase == coderv1alpha1.CoderWorkspacePoolMemberPhaseReady:
		return ctrl.Result{RequeueAfter: r.retryAfter(req.NamespacedName, member, step)}, nil
	}
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
func (r *CoderWorkspacePoolMemberReconciler) checkCreate(
	ctx context.Context, member *coderv1alpha1.CoderWorkspacePoolMember, workspaceName string,
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
	if field := poolMemberInputMismatch(member, pool); field != "" {
		return nil, coderFail("InputsMismatch", "Member input %s differs from what CoderWorkspacePool %s builds.", field, spec.PoolName), nil
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
	ownerUser, step, err := checkCoderWorkspaceOwner(ctx, sdk, uuid.MustParse(spec.OwnerID))
	if step != nil || err != nil {
		return nil, step, err
	}
	// The pool resolved these IDs, and anyone who can write its status can
	// change them. So the member resolves the allowed template again and
	// builds only from it.
	template, step, err := resolveCoderTemplate(ctx, sdk, pool.Spec.Template, ownerUser)
	if step != nil || err != nil {
		return nil, step, err
	}
	if template.ID.String() != spec.TemplateID || template.OrganizationID.String() != spec.OrganizationID {
		return nil, coderFail("InputsMismatch", "Coder template %q is not the pinned template %s.", pool.Spec.Template, spec.TemplateID), nil
	}
	// The same for the version and the preset: the member resolves the
	// pool's selectors again and builds only what they select now.
	version, step, err := readCoderTemplateVersion(ctx, sdk, pool.Spec.Template, template, pool.Spec.Version.ID, pool.Spec.Version.Name)
	if step != nil || err != nil {
		return nil, step, err
	}
	if version.ID.String() != spec.TemplateVersionID {
		return nil, coderFail("InputsMismatch", "The pool selects version %s, not the pinned version %s.", version.ID, spec.TemplateVersionID), nil
	}
	if step := checkCoderTemplateVersionUsable(version); step != nil {
		return nil, step, nil
	}
	switch {
	case pool.Spec.PresetName == "" && spec.PresetID != "":
		return nil, coderFail("InputsMismatch", "The pool selects no preset, but the member pins preset %s.", spec.PresetID), nil
	case pool.Spec.PresetName != "":
		presets, err := sdk.TemplateVersionPresets(ctx, version.ID)
		if err != nil {
			return nil, coderStepUnavailable("get presets", err), nil
		}
		i := slices.IndexFunc(presets, func(p codersdk.Preset) bool { return p.Name == pool.Spec.PresetName })
		if i < 0 {
			return nil, coderWait("PresetNotFound", "Version %s has no preset %q.", version.Name, pool.Spec.PresetName), nil
		}
		if presets[i].ID.String() != spec.PresetID {
			return nil, coderFail("InputsMismatch", "Preset %q is %s, not the pinned preset %q.", pool.Spec.PresetName, presets[i].ID, spec.PresetID), nil
		}
	}
	member.Status.WorkspaceName = workspaceName
	_, err = sdk.WorkspaceByOwnerAndName(ctx, spec.OwnerID, workspaceName, codersdk.WorkspaceOptions{})
	switch {
	case err == nil:
		// Nothing of this member exists yet, so the workspace belongs to
		// someone else. The controller never touches it.
		return nil, coderFail("WorkspaceNameConflict", "Coder user %s already has a workspace named %s.", spec.OwnerID, workspaceName), nil
	case !isCoderNotFound(err):
		return nil, coderStepUnavailable("get workspace by name", err), nil
	}
	return sdk, nil, nil
}

// poolMemberInputMismatch names the first pinned input that differs from the
// pool, or returns "". A member can only build what its pool builds.
func poolMemberInputMismatch(member *coderv1alpha1.CoderWorkspacePoolMember, pool *coderv1alpha1.CoderWorkspacePool) string {
	spec, resolved := member.Spec, pool.Status.Resolved
	if resolved == nil {
		return "the pool's status.resolved, which is empty,"
	}
	params := make([]codersdk.WorkspaceBuildParameter, 0, len(spec.Parameters))
	for _, p := range spec.Parameters {
		params = append(params, codersdk.WorkspaceBuildParameter{Name: p.Name, Value: p.Value})
	}
	// The pool's spec can change before it resolves again, so the member
	// also compares with the pool's current parameters.
	poolParams := make([]codersdk.WorkspaceBuildParameter, 0, len(pool.Spec.Parameters))
	for _, p := range pool.Spec.Parameters {
		poolParams = append(poolParams, codersdk.WorkspaceBuildParameter{Name: p.Name, Value: p.Value})
	}
	for _, c := range []struct{ field, got, want string }{
		{"spec.controlPlaneRef", spec.ControlPlaneRef.Name, pool.Spec.ControlPlaneRef.Name},
		{"spec.ownerID", spec.OwnerID, pool.Spec.OwnerUserID},
		{"spec.ownerID", spec.OwnerID, resolved.OwnerID},
		{"spec.organizationID", spec.OrganizationID, resolved.OrganizationID},
		{"spec.templateID", spec.TemplateID, resolved.TemplateID},
		{"spec.templateVersionID", spec.TemplateVersionID, resolved.TemplateVersionID},
		{"spec.presetID", spec.PresetID, resolved.PresetID},
		{"spec.parametersHash", spec.ParametersHash, resolved.ParametersHash},
		{"spec.parameters", workspacePoolParametersHash(params), spec.ParametersHash},
		{"spec.parameters", workspacePoolParametersHash(poolParams), spec.ParametersHash},
		{"label " + coderv1alpha1.CoderWorkspacePoolSelectorLabel, member.Labels[coderv1alpha1.CoderWorkspacePoolSelectorLabel], string(pool.UID)},
	} {
		if c.got != c.want {
			return c.field
		}
	}
	return ""
}

// workspacePoolParametersHash identifies a set of rich parameter values,
// whatever their order: the hex SHA-256 of each name and value with its
// length, sorted by name. No parameters give the hash of empty input.
func workspacePoolParametersHash(params []codersdk.WorkspaceBuildParameter) string {
	sorted := slices.SortedFunc(slices.Values(params), func(a, b codersdk.WorkspaceBuildParameter) int { return strings.Compare(a.Name, b.Name) })
	h := sha256.New()
	for _, p := range sorted {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(p.Name), p.Name, len(p.Value), p.Value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// workspacePoolConsent checks the admin's consent in the control plane's
// spec.workspacePools: the owner is listed, with the template. A refusal is
// a wait, because the admin can still add them.
func workspacePoolConsent(controlPlane *coderv1alpha1.CoderControlPlane, ownerID, template string) *coderStep {
	var owners []coderv1alpha1.WorkspacePoolOwner
	if controlPlane.Spec.WorkspacePools != nil {
		owners = controlPlane.Spec.WorkspacePools.Owners
	}
	// Both CRDs require lower-case UUIDs. Comparing parsed UUIDs keeps the
	// match right even if one of the patterns changes.
	want, err := uuid.Parse(ownerID)
	i := slices.IndexFunc(owners, func(o coderv1alpha1.WorkspacePoolOwner) bool {
		got, gotErr := uuid.Parse(o.UserID)
		return err == nil && gotErr == nil && got == want
	})
	if i < 0 {
		return coderWait("OwnerNotAllowed", "CoderControlPlane %s does not list Coder user %s in spec.workspacePools.owners.", controlPlane.Name, ownerID)
	}
	// Coder matches organization and template names without case.
	if !slices.ContainsFunc(owners[i].Templates, func(t string) bool { return strings.EqualFold(t, template) }) {
		return coderWait("TemplateNotAllowed", "CoderControlPlane %s does not allow template %q for Coder user %s.", controlPlane.Name, template, ownerID)
	}
	return nil
}

// createWorkspace writes the create marker, then sends the only create
// request of this attempt, and sorts the answer by the CoderTemplateTest
// result table. A marker write conflict ends the reconcile before any request.
func (r *CoderWorkspacePoolMemberReconciler) createWorkspace(
	ctx context.Context, sdk *codersdk.Client, member *coderv1alpha1.CoderWorkspacePoolMember, before *coderv1alpha1.CoderWorkspacePoolMemberStatus, now, deadline time.Time,
) (*coderStep, error) {
	spec, name := member.Spec, member.Status.WorkspaceName
	member.Status.CreateAttemptTime = &metav1.Time{Time: now}
	applyPoolMemberStep(member, now, coderWait("CreatingWorkspace", "Creating workspace %s.", name))
	if err := r.writeStatus(ctx, member, before); err != nil {
		return nil, err
	}
	// The stored status now holds the marker. Compare later writes with it,
	// so clearing the marker is always written, even when the status then
	// equals the status before the marker.
	*before = *member.Status.DeepCopy()
	// The marker write can take a while. No request was sent yet, so a
	// member past its deadline clears the marker and fails without a workspace.
	if !r.Clock.Now().Before(deadline) {
		member.Status.CreateAttemptTime = nil
		return coderWait("CreatingWorkspace", "No create request was sent."), nil
	}
	params := make([]codersdk.WorkspaceBuildParameter, 0, len(spec.Parameters))
	for _, p := range spec.Parameters {
		params = append(params, codersdk.WorkspaceBuildParameter{Name: p.Name, Value: p.Value})
	}
	var presetID uuid.UUID
	if spec.PresetID != "" {
		presetID = uuid.MustParse(spec.PresetID) // assertPoolMember checked it.
	}
	// Only TemplateVersionID, never TemplateID, so Coder checks the archived flag.
	workspace, err := sdk.CreateUserWorkspace(ctx, spec.OwnerID, codersdk.CreateWorkspaceRequest{
		TemplateVersionID:       uuid.MustParse(spec.TemplateVersionID),
		TemplateVersionPresetID: presetID,
		Name:                    name,
		RichParameterValues:     params,
		AutomaticUpdates:        codersdk.AutomaticUpdatesNever,
	})
	status := coderStatus(err)
	switch {
	case err == nil:
		// Without a requested preset, Coder can pick one that matches the
		// parameters, so only a requested preset must come back.
		answeredPreset := spec.PresetID
		if spec.PresetID != "" {
			answeredPreset = ""
			if id := workspace.LatestBuild.TemplateVersionPresetID; id != nil {
				answeredPreset = id.String()
			}
		}
		if workspace.ID == uuid.Nil || workspace.LatestBuild.ID == uuid.Nil {
			return nil, &coderAnswerError{msg: "assertion failed: Coder answered start build without a workspace or build ID"}
		}
		if err := errors.Join(
			coderAnswerFor("workspace", name, workspace.Name),
			coderAnswerFor("start build workspace", workspace.ID.String(), workspace.LatestBuild.WorkspaceID.String()),
			coderAnswerFor("start build transition", string(codersdk.WorkspaceTransitionStart), string(workspace.LatestBuild.Transition)),
			coderAnswerFor("workspace owner", spec.OwnerID, workspace.OwnerID.String()),
			coderAnswerFor("start build version", spec.TemplateVersionID, workspace.LatestBuild.TemplateVersionID.String()),
			coderAnswerFor("start build preset", spec.PresetID, answeredPreset),
		); err != nil {
			return nil, err // The marker stays: only reads decide now.
		}
		member.Status.WorkspaceID, member.Status.StartBuildID = workspace.ID.String(), workspace.LatestBuild.ID.String()
		return coderWait("WaitingForBuild", "Workspace %s was created.", workspace.Name), nil
	case createHadNoEffect(err):
		member.Status.CreateAttemptTime = nil
		if status == http.StatusNotAcceptable {
			return coderWait("TemplateVersionImporting", "Version %s is still importing.", spec.TemplateVersionID), nil
		}
		step := coderWait("CreateRetrying", "The create request had no effect: %s", coderErrorSummary(err))
		step.rateLimited = status == http.StatusTooManyRequests
		return step, nil
	case status == http.StatusConflict:
		// The marker came right before this request, so someone else holds
		// the name. The controller never adopts that workspace.
		member.Status.CreateAttemptTime = nil
		return coderFail("WorkspaceNameConflict", "Workspace %s already exists: %s", name, coderErrorSummary(err)), nil
	case createRejected(status):
		member.Status.CreateAttemptTime = nil
		return coderFail("CreateRejected", "Coder rejected the create request: %s", coderErrorSummary(err)), nil
	default:
		return coderWait("ConfirmingCreate", "The create result is uncertain (%s). The controller reads Coder instead of sending it again.", coderErrorSummary(err)), nil
	}
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
	ready := metav1.ConditionFalse
	switch {
	case !step.failed && member.Status.ReadyTime != nil:
		member.Status.Phase, ready = coderv1alpha1.CoderWorkspacePoolMemberPhaseReady, metav1.ConditionTrue
		if step.reason == "AgentNotReady" {
			ready = metav1.ConditionFalse // Watch rule 3: the phase stays.
		}
	case !step.failed && poolMemberMayHaveWorkspace(member):
		member.Status.Phase = coderv1alpha1.CoderWorkspacePoolMemberPhaseCreating
	case !step.failed:
		member.Status.Phase = coderv1alpha1.CoderWorkspacePoolMemberPhasePending
	default:
		member.Status.Phase = coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed
		deleted := metav1.Condition{Status: metav1.ConditionTrue, Reason: "NotCreated", Message: "The controller never sent a workspace create request, or Coder refused it."}
		switch {
		case step.deleted != nil:
			deleted = *step.deleted
		case poolMemberMayHaveWorkspace(member):
			deleted = metav1.Condition{Status: metav1.ConditionFalse, Reason: "CleanupPending", Message: "The workspace can exist. The controller deletes it."}
		}
		setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionWorkspaceDeleted, deleted.Status, deleted.Reason, deleted.Message)
	}
	setPoolMemberCondition(member, now, coderv1alpha1.CoderWorkspacePoolMemberConditionReady, ready, step.reason, step.message)
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
// watch poll of a ready member, the build poll once a create request may
// exist, else the pending poll.
func (r *CoderWorkspacePoolMemberReconciler) retryAfter(key types.NamespacedName, member *coderv1alpha1.CoderWorkspacePoolMember, step *coderStep) time.Duration {
	switch {
	case step.rateLimited:
		return r.rateLimited.next(key)
	case member.Status.ReadyTime != nil:
		return poolMemberWatchPoll
	case poolMemberMayHaveWorkspace(member):
		return coderBuildPoll
	default:
		return poolMemberPendingPoll
	}
}
