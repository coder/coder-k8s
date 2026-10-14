package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// poolResolvePoll is how often a pool reads Coder again while it waits for
// an input, and while it follows the template's active version.
const poolResolvePoll = 60 * time.Second

// CoderWorkspacePoolReconciler resolves the inputs of one CoderWorkspacePool
// and reports its members (#153 plan 6.3). Nothing registers it yet.
type CoderWorkspacePoolReconciler struct {
	client.Client
	// APIReader reads members without the cache, so a stale cache never
	// makes the pool count too few members.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Clock     clock.PassiveClock
}

// poolMemberCounts sorts the members of one pool.
type poolMemberCounts struct {
	unclaimed, ready, current, readyCurrent, outdated, bound, used int32
}

// Reconcile resolves the pool's inputs, counts its members, and writes the
// pool status.
func (r *CoderWorkspacePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil || r.APIReader == nil || r.Scheme == nil || r.Clock == nil {
		return ctrl.Result{}, errors.New("assertion failed: pool reconciler needs a client, an API reader, a scheme, and a clock")
	}
	pool := &coderv1alpha1.CoderWorkspacePool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pool.Name != req.Name || pool.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched pool %s/%s does not match request %s", pool.Namespace, pool.Name, req)
	}
	if pool.UID == "" || pool.Spec.Replicas == nil || pool.Spec.MaxInFlight == nil || pool.Spec.ReadyTimeoutSeconds == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: pool %s has no UID or no defaulted replicas, maxInFlight, or readyTimeoutSeconds", req)
	}
	if !pool.DeletionTimestamp.IsZero() {
		// Garbage collection deletes the unclaimed members. Their finalizers
		// run the last use check.
		return ctrl.Result{}, nil
	}

	before := pool.Status.DeepCopy()
	resolved, step, err := r.resolve(ctx, pool)
	if step, err = memberWaitOnWrongAnswer(ctx, step, err); err != nil {
		return ctrl.Result{}, err
	}
	if step == nil && !poolResolvedEqual(pool.Status.Resolved, resolved) {
		resolved.ResolvedTime = &metav1.Time{Time: r.Clock.Now()}
		pool.Status.Resolved = resolved
	}

	members := &coderv1alpha1.CoderWorkspacePoolMemberList{}
	if err := r.APIReader.List(ctx, members, client.InNamespace(pool.Namespace),
		client.MatchingLabels{coderv1alpha1.CoderWorkspacePoolSelectorLabel: string(pool.UID)}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list members of pool %s: %w", req, err)
	}
	counts := countPoolMembers(pool, members.Items)

	r.applyPoolStatus(pool, counts, step)
	if err := r.writePoolStatus(ctx, pool, before); err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case step != nil && step.failed:
		return ctrl.Result{}, nil // A person must change an input.
	case step != nil, pool.Spec.Version.Active != nil && *pool.Spec.Version.Active:
		return ctrl.Result{RequeueAfter: poolResolvePoll}, nil
	case !meta.IsStatusConditionTrue(pool.Status.Conditions, coderv1alpha1.CoderWorkspacePoolConditionReady):
		return ctrl.Result{RequeueAfter: poolMemberPendingPoll}, nil
	}
	return ctrl.Result{}, nil
}

// resolve reads the pool's inputs from Coder (plan 6.3 step 2) with the
// rules that the member checks again before it creates. A nil step means
// that new members can pin the returned values.
func (r *CoderWorkspacePoolReconciler) resolve(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool,
) (*coderv1alpha1.CoderWorkspacePoolResolved, *coderStep, error) {
	spec := pool.Spec
	sdk, controlPlane, wait, err := controlPlaneCoderClient(ctx, r.Client, pool.Namespace, spec.ControlPlaneRef.Name)
	if wait != nil || err != nil {
		if wait != nil {
			return nil, coderWait(wait.reason, "%s", wait.message), nil
		}
		return nil, nil, err
	}
	ownerID, err := uuid.Parse(spec.OwnerUserID)
	if err != nil {
		return nil, nil, fmt.Errorf("assertion failed: spec.ownerUserID %q passed validation but is not a UUID: %w", spec.OwnerUserID, err)
	}
	if step := workspacePoolConsent(controlPlane, spec.OwnerUserID, spec.Template); step != nil {
		return nil, step, nil
	}
	owner, step, err := checkCoderWorkspaceOwner(ctx, sdk, ownerID)
	if step != nil || err != nil {
		return nil, step, err
	}
	template, step, err := resolveCoderTemplate(ctx, sdk, spec.Template, owner)
	if step != nil || err != nil {
		return nil, step, err
	}
	version, step, err := readCoderTemplateVersion(ctx, sdk, spec.Template, template, spec.Version.ID, spec.Version.Name)
	if step != nil || err != nil {
		return nil, step, err
	}
	if step := checkCoderTemplateVersionUsable(version); step != nil {
		return nil, step, nil
	}
	var presetID string
	if spec.PresetName != "" {
		presets, err := sdk.TemplateVersionPresets(ctx, version.ID)
		if err != nil {
			return nil, coderStepUnavailable("get presets", err), nil
		}
		i := slices.IndexFunc(presets, func(p codersdk.Preset) bool { return p.Name == spec.PresetName })
		if i < 0 {
			return nil, coderWait("PresetNotFound", "Version %s has no preset %q.", version.Name, spec.PresetName), nil
		}
		if presets[i].ID == uuid.Nil {
			return nil, nil, &coderAnswerError{msg: "assertion failed: Coder answered a preset without an ID"}
		}
		presetID = presets[i].ID.String()
	}
	params := make([]codersdk.WorkspaceBuildParameter, 0, len(spec.Parameters))
	for _, p := range spec.Parameters {
		params = append(params, codersdk.WorkspaceBuildParameter{Name: p.Name, Value: p.Value})
	}
	return &coderv1alpha1.CoderWorkspacePoolResolved{
		OrganizationID: template.OrganizationID.String(), TemplateID: template.ID.String(),
		TemplateVersionID: version.ID.String(), TemplateVersionName: version.Name, PresetID: presetID,
		OwnerID: ownerID.String(), ParametersHash: workspacePoolParametersHash(params),
	}, nil, nil
}

// poolResolvedEqual compares two resolved inputs without their time.
func poolResolvedEqual(a, b *coderv1alpha1.CoderWorkspacePoolResolved) bool {
	if a == nil || b == nil {
		return a == b
	}
	a, b = a.DeepCopy(), b.DeepCopy()
	a.ResolvedTime, b.ResolvedTime = nil, nil
	return equality.Semantic.DeepEqual(a, b)
}

// countPoolMembers sorts the pool's members for the status counts. Only a
// member that the pool controls counts as unclaimed. Members that someone
// used, failed members, and deleting members never count, and a ready
// member counts as ready only while its Ready condition is True.
func countPoolMembers(pool *coderv1alpha1.CoderWorkspacePool, members []coderv1alpha1.CoderWorkspacePoolMember) poolMemberCounts {
	var c poolMemberCounts
	for i := range members {
		m := &members[i]
		used := meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionUsed)
		switch {
		case !m.DeletionTimestamp.IsZero():
			continue
		case m.Spec.ClaimRef != nil:
			c.bound++
			continue
		case used:
			c.used++
			continue
		}
		owner := metav1.GetControllerOf(m)
		if owner == nil || owner.UID != pool.UID || m.Status.Phase == coderv1alpha1.CoderWorkspacePoolMemberPhaseFailed {
			continue
		}
		c.unclaimed++
		ready := m.Status.Phase == coderv1alpha1.CoderWorkspacePoolMemberPhaseReady &&
			meta.IsStatusConditionTrue(m.Status.Conditions, coderv1alpha1.CoderWorkspacePoolMemberConditionReady)
		current := poolMemberInputMismatch(m, pool) == ""
		switch {
		case current && ready:
			c.ready++
			c.current++
			c.readyCurrent++
		case current:
			c.current++
		case ready:
			c.ready++
			c.outdated++
		default:
			c.outdated++
		}
	}
	return c
}

// applyPoolStatus writes the counts, the selector, the wait reason, and the
// conditions (plan 5.3 and 7.4).
func (r *CoderWorkspacePoolReconciler) applyPoolStatus(pool *coderv1alpha1.CoderWorkspacePool, c poolMemberCounts, step *coderStep) {
	st, replicas := &pool.Status, *pool.Spec.Replicas
	st.ObservedGeneration = pool.Generation
	st.Replicas, st.ReadyReplicas, st.UpdatedReplicas = c.unclaimed, c.ready, c.current
	st.BoundReplicas, st.UsedReplicas = c.bound, c.used
	// HPA refuses an empty selector, and the UID makes it unique per pool.
	st.Selector = labels.Set{coderv1alpha1.CoderWorkspacePoolSelectorLabel: string(pool.UID)}.String()
	ready := step == nil && c.readyCurrent >= replicas && c.outdated == 0
	switch {
	case step != nil:
	case ready:
		step = coderWait("Ready", "%d of %d workspaces are ready.", c.readyCurrent, replicas)
	default:
		step = coderWait("WaitingForMembers", "%d of %d current workspaces are ready, and %d outdated workspaces remain.", c.readyCurrent, replicas, c.outdated)
	}
	st.Reason, st.Message = step.reason, truncateTemplateTestMessage(step.message)
	now := metav1.NewTime(r.Clock.Now())
	set := func(conditionType string, ok bool) {
		status := metav1.ConditionFalse
		if ok {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: conditionType, Status: status, ObservedGeneration: pool.Generation,
			Reason: step.reason, Message: st.Message, LastTransitionTime: now,
		})
	}
	set(coderv1alpha1.CoderWorkspacePoolConditionReady, ready)
	set(coderv1alpha1.CoderWorkspacePoolConditionStalled, step.failed)
	set(coderv1alpha1.CoderWorkspacePoolConditionReconciling, !ready && !step.failed)
}

// writePoolStatus writes changed status with optimistic concurrency.
func (r *CoderWorkspacePoolReconciler) writePoolStatus(
	ctx context.Context, pool *coderv1alpha1.CoderWorkspacePool, before *coderv1alpha1.CoderWorkspacePoolStatus,
) error {
	if equality.Semantic.DeepEqual(before, &pool.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, pool); err != nil {
		return fmt.Errorf("update status of pool %s: %w", types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, err)
	}
	return nil
}
