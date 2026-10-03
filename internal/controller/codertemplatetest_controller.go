package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
)

const (
	templateTestWorkspacePrefix  = "ktt-"
	templateTestUIDHexLength     = 28
	templateTestCoderTimeout     = 30 * time.Second
	templateTestPendingPoll      = 15 * time.Second
	templateTestDefaultTimeout   = int32(900)
	templateTestMaxMessageLength = 256
)

// CoderTemplateTestReconciler runs CoderTemplateTest objects. It is not
// registered with the manager yet: the API stays dormant until activation
// (#152). This version resolves the inputs of a test and never creates a
// workspace.
type CoderTemplateTestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  clock.PassiveClock
}

// templateTestStep is the outcome of a step that cannot continue: a wait
// keeps the test Pending, a failure makes it Failed.
type templateTestStep struct {
	failed  bool
	reason  string
	message string
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
	// Later changes add the create request. Until then no test can have a
	// workspace, so every cleanup below is the "never created" case.
	if tt.Status.CreateAttemptTime != nil || tt.Status.WorkspaceID != "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: template test %s/%s has a create marker, but this controller never creates workspaces", tt.Namespace, tt.Name)
	}

	if !tt.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.releaseFinalizer(ctx, tt)
	}
	final := isTemplateTestFinal(tt.Status.Phase)
	cleanedUp := final && meta.IsStatusConditionTrue(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted)
	if !cleanedUp && !controllerutil.ContainsFinalizer(tt, coderv1alpha1.CoderTemplateTestCleanupFinalizer) {
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
		setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionReconciling, metav1.ConditionTrue, tt.Status.Reason, tt.Status.Message)
		setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionReady, metav1.ConditionFalse, tt.Status.Reason, tt.Status.Message)
		setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionStalled, metav1.ConditionFalse, tt.Status.Reason, tt.Status.Message)
		return ctrl.Result{}, r.writeStatus(ctx, tt, before)
	case final:
		if !meta.IsStatusConditionTrue(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted) {
			markTemplateTestNotCreated(tt)
			return ctrl.Result{}, r.writeStatus(ctx, tt, before)
		}
		return ctrl.Result{}, r.releaseFinalizer(ctx, tt)
	}

	if tt.Status.StartTime == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: template test %s/%s is %s without status.startTime", tt.Namespace, tt.Name, tt.Status.Phase)
	}
	deadline := tt.Status.StartTime.Add(time.Duration(templateTestTimeoutSeconds(tt)) * time.Second)
	if !now.Before(deadline) {
		applyTemplateTestStep(tt, now, templateTestFail("DeadlineExceeded",
			"The test did not finish within %ds. Last wait: %s: %s", templateTestTimeoutSeconds(tt), tt.Status.Reason, tt.Status.Message))
		return ctrl.Result{}, r.writeStatus(ctx, tt, before)
	}

	step, err := r.resolveInputs(ctx, tt)
	if err != nil {
		return ctrl.Result{}, err
	}
	if step == nil {
		step = templateTestWait("ReadyToCreate", "Inputs are resolved. Creating workspaces is not enabled yet.")
	}
	applyTemplateTestStep(tt, now, step)
	if err := r.writeStatus(ctx, tt, before); err != nil {
		return ctrl.Result{}, err
	}
	if step.failed {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: min(templateTestPendingPoll, deadline.Sub(now))}, nil
}

// resolveInputs runs the Pending lookups. A nil step means every input is
// resolved. Later changes add owner eligibility and the template lookups.
func (r *CoderTemplateTestReconciler) resolveInputs(ctx context.Context, tt *coderv1alpha1.CoderTemplateTest) (*templateTestStep, error) {
	sdk, controlPlane, step, err := r.coderClient(ctx, tt)
	if step != nil || err != nil {
		return step, err
	}
	if controlPlane.Spec.TemplateTests == nil || controlPlane.Spec.TemplateTests.OwnerUserID == "" {
		return templateTestWait("OwnerNotConfigured", "Set spec.templateTests.ownerUserID on CoderControlPlane %s.", controlPlane.Name), nil
	}
	ownerID, err := uuid.Parse(controlPlane.Spec.TemplateTests.OwnerUserID)
	if err != nil {
		return templateTestWait("OwnerNotConfigured", "spec.templateTests.ownerUserID on CoderControlPlane %s: %v", controlPlane.Name, err), nil
	}
	if _, err := sdk.User(ctx, ownerID.String()); isCoderNotFound(err) {
		return templateTestWait("OwnerNotEligible", "Coder user %s does not exist.", ownerID), nil
	} else if err != nil {
		return coderUnavailable("get owner", err), nil
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
	coderURL, err := url.Parse(controlPlane.Status.URL)
	if err != nil {
		return nil, nil, templateTestWait("ControlPlaneNotReady", "CoderControlPlane %s has an invalid status.url: %v", key.Name, err), nil
	}
	sdk, err := coder.NewSDKClient(coder.Config{CoderURL: coderURL, SessionToken: token, RequestTimeout: templateTestCoderTimeout})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create Coder client for codercontrolplane %s: %w", key, err)
	}
	return sdk, controlPlane, nil, nil
}

func applyTemplateTestStep(tt *coderv1alpha1.CoderTemplateTest, now time.Time, step *templateTestStep) {
	tt.Status.Reason, tt.Status.Message = step.reason, step.message
	if !step.failed {
		tt.Status.Phase = coderv1alpha1.CoderTemplateTestPhasePending
		setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionReconciling, metav1.ConditionTrue, step.reason, step.message)
		return
	}
	tt.Status.Phase = coderv1alpha1.CoderTemplateTestPhaseFailed
	tt.Status.CompletionTime = &metav1.Time{Time: now}
	setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionReconciling, metav1.ConditionFalse, step.reason, step.message)
	setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionReady, metav1.ConditionFalse, step.reason, step.message)
	setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionStalled, metav1.ConditionTrue, step.reason, step.message)
	markTemplateTestNotCreated(tt)
}

// markTemplateTestNotCreated records that no create request was ever sent.
func markTemplateTestNotCreated(tt *coderv1alpha1.CoderTemplateTest) {
	setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted, metav1.ConditionTrue,
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
	var sdkErr *codersdk.Error
	return errors.As(err, &sdkErr) && sdkErr.StatusCode() == http.StatusNotFound
}

// coderUnavailable keeps the test waiting after a failed Coder call. The
// message holds the status and Coder's short message only, never details.
func coderUnavailable(action string, err error) *templateTestStep {
	var sdkErr *codersdk.Error
	if errors.As(err, &sdkErr) {
		return templateTestWait("CoderUnavailable", "%s: Coder answered %d: %s", action, sdkErr.StatusCode(), sdkErr.Message)
	}
	return templateTestWait("CoderUnavailable", "%s: %v", action, err)
}

func truncateTemplateTestMessage(message string) string {
	if len(message) <= templateTestMaxMessageLength {
		return message
	}
	return strings.ToValidUTF8(message[:templateTestMaxMessageLength], "")
}

func setTemplateTestCondition(tt *coderv1alpha1.CoderTemplateTest, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&tt.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, ObservedGeneration: tt.Generation,
		Reason: reason, Message: truncateTemplateTestMessage(message),
	})
}
