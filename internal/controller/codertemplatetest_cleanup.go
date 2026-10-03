package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	// templateTestDeletionPolicyAnnotation is the #151 lifecycle annotation.
	// "retain" leaves the workspace in Coder; any other value means "delete".
	templateTestDeletionPolicyAnnotation = "coder.com/deletion-policy"
	templateTestUnavailablePoll          = 60 * time.Second
)

// templateTestCleanupDone reports whether the finalizer can go: no workspace
// of the test exists, the deletion policy is retain, or the control plane is
// gone (plan amendments A1 and A2).
func templateTestCleanupDone(tt *coderv1alpha1.CoderTemplateTest) bool {
	reason := templateTestDeletedReason(tt)
	return meta.IsStatusConditionTrue(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted) ||
		reason == "Retained" || reason == "ControlPlaneGone"
}

func templateTestDeletedReason(tt *coderv1alpha1.CoderTemplateTest) string {
	if c := meta.FindStatusCondition(tt.Status.Conditions, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted); c != nil {
		return c.Reason
	}
	return ""
}

// cleanup runs the cleanup cases (plan 2.6 with amendments A1 to A3) for a
// final test or a test being deleted. It changes only WorkspaceDeleted,
// never the phase, and releases the finalizer once cleanup is done.
func (r *CoderTemplateTestReconciler) cleanup(ctx context.Context, tt *coderv1alpha1.CoderTemplateTest) (ctrl.Result, error) {
	if !templateTestCleanupDone(tt) {
		before := tt.Status.DeepCopy()
		requeue, err := r.cleanupStep(ctx, tt)
		if err == nil {
			err = r.writeStatus(ctx, tt, before)
		}
		if err != nil || !templateTestCleanupDone(tt) {
			return ctrl.Result{RequeueAfter: requeue}, err
		}
	}
	return ctrl.Result{}, r.releaseFinalizer(ctx, tt)
}

func (r *CoderTemplateTestReconciler) cleanupStep(ctx context.Context, tt *coderv1alpha1.CoderTemplateTest) (time.Duration, error) {
	policy, note := tt.Annotations[templateTestDeletionPolicyAnnotation], ""
	if policy != "" && policy != "delete" && policy != "retain" {
		note = fmt.Sprintf(" The controller ignores %s value %q.", templateTestDeletionPolicyAnnotation, policy)
	}
	set := func(status metav1.ConditionStatus, reason, message string) {
		setTemplateTestCondition(tt, coderv1alpha1.CoderTemplateTestConditionWorkspaceDeleted, status, reason, message+note)
	}
	name := tt.Status.WorkspaceName
	switch {
	case policy == "retain":
		// No Coder call, so retain also works while Coder is unreachable.
		set(metav1.ConditionFalse, "Retained", fmt.Sprintf("%s is retain: the controller leaves workspace %s in Coder.", templateTestDeletionPolicyAnnotation, name))
		return 0, nil
	case !templateTestMayHaveWorkspace(tt):
		markTemplateTestNotCreated(tt)
		return 0, nil
	case templateTestDeletedReason(tt) == "OwnershipUnknown":
		// Someone else's workspace can hold the name. The controller never
		// reads or touches it again. Only retain releases the test (A3).
		return 0, nil
	}
	if gone, err := r.controlPlaneGone(ctx, tt); err != nil || gone {
		if gone {
			step := templateTestControlPlaneGone(tt)
			set(step.deleted.Status, step.deleted.Reason, step.deleted.Message)
		}
		return 0, err
	}

	sdk, _, step, err := r.coderClient(ctx, tt)
	if err != nil {
		return 0, err
	}
	if step == nil && tt.Status.WorkspaceID == "" {
		// A create request may exist: the same confirming reads as before
		// the deadline. They store the IDs when they adopt the workspace.
		step, err = r.confirmCreate(ctx, sdk, tt, r.Clock.Now())
		if step, err = waitOnWrongAnswer(ctx, step, err); err != nil {
			return 0, err
		}
		if tt.Status.WorkspaceID != "" {
			step = nil
		}
	}
	switch {
	case step == nil:
		// The delete steps come with the next part of plan PR 5.
		set(metav1.ConditionFalse, "CleanupPending", fmt.Sprintf("Workspace %s can exist. The delete steps are not enabled yet.", name))
		return 0, nil
	case step.deleted != nil:
		set(step.deleted.Status, step.deleted.Reason, step.deleted.Message)
		return 0, nil
	case step.reason == "ConfirmingCreate":
		set(metav1.ConditionUnknown, "CreateOutcomeUnknown", step.message)
		return templateTestRunningPoll, nil
	}
	// The control plane exists, but Coder or its token is not usable.
	set(metav1.ConditionFalse, "ControlPlaneUnavailable", step.message)
	return templateTestUnavailablePoll, nil
}

// controlPlaneGone reports whether the referenced control plane is NotFound
// or being deleted. Then nothing can clean up, so the finalizer goes (A1).
func (r *CoderTemplateTestReconciler) controlPlaneGone(ctx context.Context, tt *coderv1alpha1.CoderTemplateTest) (bool, error) {
	controlPlane := &coderv1alpha1.CoderControlPlane{}
	key := types.NamespacedName{Namespace: tt.Namespace, Name: tt.Spec.ControlPlaneRef.Name}
	if err := r.Get(ctx, key, controlPlane); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("get codercontrolplane %s: %w", key, err)
	}
	return !controlPlane.DeletionTimestamp.IsZero(), nil
}

func templateTestControlPlaneGone(tt *coderv1alpha1.CoderTemplateTest) *templateTestStep {
	step := templateTestFail("ControlPlaneGone", "CoderControlPlane %s is gone, so nothing can delete workspace %s. Look for it in Coder.",
		tt.Spec.ControlPlaneRef.Name, tt.Status.WorkspaceName)
	step.deleted = &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "ControlPlaneGone", Message: step.message}
	return step
}
