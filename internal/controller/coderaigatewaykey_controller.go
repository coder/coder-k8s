package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

const (
	aiGatewayKeyKind = "CoderAIGatewayKey"
	// aiGatewayKeyResync re-reads Coder for a Ready or stalled object, so a
	// Coder-side change or a freed Secret name shows up without a watch.
	aiGatewayKeyResync = 10 * time.Minute
	// aiGatewayKeyRetry is the retry for waits that no watch ends: a failed
	// Coder call, or an operator token Secret that is not readable yet.
	aiGatewayKeyRetry = 30 * time.Second
	// aiGatewayKeyPrefixLength is the length of the visible key prefix that
	// Coder lists (plan F8).
	aiGatewayKeyPrefixLength = 11
	// aiGatewayKeyAppName is the app.kubernetes.io/name label of the output
	// Secret.
	aiGatewayKeyAppName = "coder-ai-gateway-key"
	// Coder's 403 answer without a Premium license or with the AI Gateway
	// turned off (plan F17), and its 404 answer for a route it does not have
	// (plan F11).
	aiGatewayPremiumSuffix   = "is a Premium feature. Contact sales!"
	aiGatewayRouteNotFound   = "Route not found."
	reasonIssuingKey         = "IssuingKey"
	reasonKeyDeletedExternal = "KeyDeletedExternally"
)

// aiGatewayKeyValue is the format of a Coder AI Gateway key (plan I1). A
// value in another format holds no key.
var aiGatewayKeyValue = regexp.MustCompile(`^[0-9A-Za-z]{43}$`)

// CoderAIGatewayKeyReconciler reconciles CoderAIGatewayKey objects (#156). It
// is not registered yet. This version runs the checks of the observe step
// (plan 4.2) and claims the output Secret. It creates and deletes no Coder
// key.
type CoderAIGatewayKeyReconciler struct {
	client.Client
	// APIReader reads the output Secret and the objects of the reserved-name
	// check live, not from the cache.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Clock     clock.PassiveClock

	rateLimited coderBackoff // Keyed by types.NamespacedName.
}

// aiGatewayKeyStep is the outcome of one reconcile. A ready step reports that
// the Secret holds the current key and Coder lists it. A stalled step waits
// for a change of the spec or an input. Any other step waits for progress.
type aiGatewayKeyStep struct {
	ready       bool
	stalled     bool
	reason      string
	message     string
	requeue     time.Duration
	rateLimited bool // Coder answered 429: retry with backoff.
}

func aiGatewayKeyWait(requeue time.Duration, reason, format string, args ...any) *aiGatewayKeyStep {
	return &aiGatewayKeyStep{reason: reason, message: fmt.Sprintf(format, args...), requeue: requeue}
}

func aiGatewayKeyStalled(reason, format string, args ...any) *aiGatewayKeyStep {
	return &aiGatewayKeyStep{stalled: true, reason: reason, message: fmt.Sprintf(format, args...), requeue: aiGatewayKeyResync}
}

// SetupWithManager wires the reconciler into controller-runtime. Nothing
// calls it until activation (K10). A change of the output Secret, or of any
// control plane in the namespace (any of them can reserve the Secret name),
// requeues the object at once.
func (r *CoderAIGatewayKeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil || r.Client == nil || r.APIReader == nil || r.Scheme == nil || r.Clock == nil {
		return errors.New("assertion failed: AI Gateway key reconciler needs a manager, a client, an API reader, a scheme, and a clock")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderAIGatewayKey{}).
		Owns(&corev1.Secret{}).
		Watches(&coderv1alpha1.CoderControlPlane{}, handler.EnqueueRequestsFromMapFunc(r.keysInNamespace)).
		Named("coderaigatewaykey").
		Complete(r)
}

func (r *CoderAIGatewayKeyReconciler) keysInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var keys coderv1alpha1.CoderAIGatewayKeyList
	if err := r.List(ctx, &keys, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list AI Gateway keys of a namespace", "namespace", obj.GetNamespace())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(keys.Items))
	for _, k := range keys.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: k.Namespace, Name: k.Name}})
	}
	return requests
}

// Reconcile handles deletion first, stores the finalizer before any Coder
// call, then runs the observe step and at most one action.
func (r *CoderAIGatewayKeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil || r.APIReader == nil || r.Scheme == nil || r.Clock == nil {
		return ctrl.Result{}, errors.New("assertion failed: AI Gateway key reconciler needs a client, an API reader, a scheme, and a clock")
	}
	// Only a wait after HTTP 429 keeps the backoff count.
	keepBackoff := false
	defer func() {
		if !keepBackoff {
			r.rateLimited.forget(req.NamespacedName)
		}
	}()
	key := &coderv1alpha1.CoderAIGatewayKey{}
	if err := r.Get(ctx, req.NamespacedName, key); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if key.Name != req.Name || key.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched AI Gateway key %s/%s does not match %s", key.Namespace, key.Name, req.NamespacedName)
	}
	before := key.Status.DeepCopy()

	if !key.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(key, coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer) {
			return ctrl.Result{}, nil
		}
		if key.Status.NextSerial == 0 {
			// No create request was ever sent, so no key of this object can
			// exist in Coder (plan 3.5).
			return ctrl.Result{}, r.patchFinalizer(ctx, key, false)
		}
		// The deletion path (K9) is not in this version: keep the finalizer
		// and delete nothing.
		r.applyStep(key, aiGatewayKeyWait(0, "DeletePending",
			"Keys of this object can exist in Coder. This controller version keeps the finalizer and deletes nothing."))
		return ctrl.Result{}, r.writeStatus(ctx, key, before)
	}

	if err := r.patchFinalizer(ctx, key, true); err != nil {
		return ctrl.Result{}, err
	}
	step, err := r.observe(ctx, key)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.applyStep(key, step)
	if err := r.writeStatus(ctx, key, before); err != nil {
		return ctrl.Result{}, err
	}
	requeue := step.requeue
	if step.rateLimited {
		keepBackoff = true
		requeue = r.rateLimited.next(req.NamespacedName)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// observe runs the reads of plan 4.2 in order and returns the first step
// that applies. Every check before the LIST sends nothing to Coder.
func (r *CoderAIGatewayKeyReconciler) observe(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey) (*aiGatewayKeyStep, error) {
	sdk, controlPlane, wait, err := controlPlaneCoderClient(ctx, r.Client, key.Namespace, key.Spec.ControlPlaneRef.Name)
	if err != nil {
		return nil, err
	}
	if wait != nil {
		return aiGatewayKeyWait(aiGatewayKeyRetry, wait.reason, "%s", wait.message), nil
	}
	secret, err := getOwnedSecret(ctx, r.APIReader, key, aiGatewayKeyKind, key.Spec.SecretName)
	if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
		return aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message), nil
	} else if err != nil {
		return nil, err
	}
	err = reservedSecretName(ctx, r.APIReader, key, key.Spec.SecretName)
	if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
		return aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message), nil
	} else if err != nil {
		return nil, err
	}
	// With nextSerial > 0 the stalls above and this one continue in
	// delete-only mode (plan 4.8, K7). This version never creates a key, so
	// it stops here.
	if controlPlane.Spec.AIGateway == nil || !controlPlane.Spec.AIGateway.AllowKeys {
		return aiGatewayKeyStalled("NotAllowed",
			"CoderControlPlane %s does not allow AI Gateway keys. Its admin can set spec.aiGateway.allowKeys to true.", controlPlane.Name), nil
	}
	prefix, err := aiGatewayKeyNamePrefix(key.UID)
	if err != nil {
		return nil, err
	}
	listed, err := sdk.ListAIGatewayKeys(ctx)
	if err != nil {
		return aiGatewayKeyListFailure(err), nil
	}
	own := ownAIGatewayKeys(prefix, listed)

	switch action := nextAIGatewayKeyAction(&key.Status, own, aiGatewayKeySecretHolds(secret, own)); action {
	case aiGatewayKeyReady:
		return &aiGatewayKeyStep{ready: true, reason: "Ready", requeue: aiGatewayKeyResync, message: fmt.Sprintf(
			"Secret %q holds key %s, and Coder lists it.", key.Spec.SecretName, key.Status.CurrentKey.Name)}, nil
	case aiGatewayKeyIssue:
		return r.claimSecret(ctx, key)
	case aiGatewayKeyKillSwitch:
		return aiGatewayKeyStalled(reasonKeyDeletedExternal,
			"Coder no longer lists the current key %s. Someone deleted it outside the operator, so the operator creates no new key.", key.Status.CurrentKey.Name), nil
	default:
		// Recovery of a pending create (K6) and a drift re-issue (K7).
		return aiGatewayKeyWait(aiGatewayKeyResync, reasonIssuingKey, "This controller version does not run step %q yet.", action), nil
	}
}

// claimSecret is the first part of the issue step (plan 4.3): the output
// Secret exists and this object controls it before any key exists.
func (r *CoderAIGatewayKeyReconciler) claimSecret(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey) (*aiGatewayKeyStep, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":     aiGatewayKeyAppName,
		"app.kubernetes.io/instance": workspaceProxyInstanceLabelValue(key.Name),
	}
	_, err := claimOwnedSecret(ctx, r.Client, r.APIReader, r.Scheme, key, aiGatewayKeyKind, key.Spec.SecretName, labels)
	if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
		return aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message), nil
	} else if err != nil {
		return nil, err
	}
	return aiGatewayKeyWait(aiGatewayKeyResync, reasonIssuingKey,
		"Secret %q is claimed. This controller version creates no key yet.", key.Spec.SecretName), nil
}

// aiGatewayKeyListFailure classifies a failed LIST (plan 4.2 step 4). The
// message holds the status and Coder's short message only.
func aiGatewayKeyListFailure(err error) *aiGatewayKeyStep {
	var sdkErr *codersdk.Error
	if errors.As(err, &sdkErr) {
		switch status := sdkErr.StatusCode(); {
		case status == http.StatusForbidden && strings.HasSuffix(sdkErr.Message, aiGatewayPremiumSuffix):
			return aiGatewayKeyStalled("LicenseRequired",
				"%s The deployment needs a Premium license, and the AI Gateway must be enabled (CODER_AI_GATEWAY_ENABLED).", coderErrorSummary(err))
		case status == http.StatusForbidden:
			return aiGatewayKeyStalled("CoderRejected", "%s The operator user must be a Coder site owner.", coderErrorSummary(err))
		case status == http.StatusNotFound && sdkErr.Message == aiGatewayRouteNotFound:
			return aiGatewayKeyStalled("NotSupported", "%s This Coder deployment has no AI Gateway key API.", coderErrorSummary(err))
		}
	}
	unavailable := coderStepUnavailable("list AI Gateway keys", err)
	step := aiGatewayKeyWait(aiGatewayKeyRetry, unavailable.reason, "%s", unavailable.message)
	step.rateLimited = unavailable.rateLimited
	return step
}

// aiGatewayKeyNamePrefix returns the start of every Coder key name of the
// object with uid: "ck8s-", all 32 hex characters of the UID, and "-". The
// serial follows (plan D1).
func aiGatewayKeyNamePrefix(uid types.UID) (string, error) {
	hex := strings.ReplaceAll(string(uid), "-", "")
	if len(hex) != 32 || strings.Trim(hex, "0123456789abcdef") != "" {
		return "", fmt.Errorf("assertion failed: object UID %q needs 32 lowercase hex characters", uid)
	}
	return "ck8s-" + hex + "-", nil
}

// ownAIGatewayKeys returns the listed keys whose name is prefix and a serial
// (plan I6). The controller never acts on any other key.
func ownAIGatewayKeys(prefix string, keys []codersdk.AIGatewayKey) []codersdk.AIGatewayKey {
	var own []codersdk.AIGatewayKey
	for _, k := range keys {
		if serial, ok := strings.CutPrefix(k.Name, prefix); ok && serial != "" && strings.Trim(serial, "0123456789") == "" {
			own = append(own, k)
		}
	}
	return own
}

// aiGatewayKeySecretHolds returns the ID of the key in own whose prefix the
// Secret's value starts with, or uuid.Nil when the Secret is absent or holds
// no such key (plan 4.2 step 6). Prefixes are unique (plan F8), so a match
// proves which key the Secret holds. The value never leaves this function.
func aiGatewayKeySecretHolds(secret *corev1.Secret, own []codersdk.AIGatewayKey) uuid.UUID {
	if secret == nil {
		return uuid.Nil
	}
	value := secret.Data[coderv1alpha1.CoderAIGatewayKeySecretDataKey]
	if !aiGatewayKeyValue.Match(value) {
		return uuid.Nil
	}
	for _, k := range own {
		if k.KeyPrefix == string(value[:aiGatewayKeyPrefixLength]) {
			return k.ID
		}
	}
	return uuid.Nil
}

// aiGatewayKeyAction is the one mutating step that a reconcile takes.
type aiGatewayKeyAction string

const (
	aiGatewayKeyReady      aiGatewayKeyAction = "Ready"      // Nothing to do.
	aiGatewayKeyIssue      aiGatewayKeyAction = "Issue"      // Issue the first key (4.3).
	aiGatewayKeyRecover    aiGatewayKeyAction = "Recover"    // A create can be in flight (4.3, 4.4).
	aiGatewayKeyKillSwitch aiGatewayKeyAction = "KillSwitch" // Someone deleted the current key in Coder (4.7).
	aiGatewayKeyReissue    aiGatewayKeyAction = "Reissue"    // The Secret lost the current key (D7).
)

// nextAIGatewayKeyAction is the pure decision of plan 4.2 step 7. own holds
// the listed keys of this object, and holds is the key that the Secret holds
// (uuid.Nil for none). K6 to K9 add rows.
func nextAIGatewayKeyAction(status *coderv1alpha1.CoderAIGatewayKeyStatus, own []codersdk.AIGatewayKey, holds uuid.UUID) aiGatewayKeyAction {
	switch {
	case status.PendingKey != nil:
		return aiGatewayKeyRecover
	case status.CurrentKey == nil:
		return aiGatewayKeyIssue
	case !aiGatewayKeyListed(own, status.CurrentKey.ID):
		return aiGatewayKeyKillSwitch
	case holds != uuid.Nil && holds.String() == status.CurrentKey.ID:
		return aiGatewayKeyReady
	default:
		return aiGatewayKeyReissue
	}
}

func aiGatewayKeyListed(keys []codersdk.AIGatewayKey, id string) bool {
	for _, k := range keys {
		if k.ID.String() == id {
			return true
		}
	}
	return false
}

// patchFinalizer adds or removes the cleanup finalizer with a merge patch and
// an optimistic lock, so a concurrent change gives a conflict.
func (r *CoderAIGatewayKeyReconciler) patchFinalizer(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, add bool) error {
	base := key.DeepCopy()
	changed := controllerutil.RemoveFinalizer(key, coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer)
	if add {
		changed = controllerutil.AddFinalizer(key, coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer)
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, key, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch finalizer of AI Gateway key %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// applyStep sets the kstatus conditions for step. A status change takes its
// transition time from the controller clock. Messages are cut to 256 bytes.
func (r *CoderAIGatewayKeyReconciler) applyStep(key *coderv1alpha1.CoderAIGatewayKey, step *aiGatewayKeyStep) {
	ready, reconciling, stalled := metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionFalse
	switch {
	case step.ready:
		ready, reconciling = metav1.ConditionTrue, metav1.ConditionFalse
	case step.stalled:
		reconciling, stalled = metav1.ConditionFalse, metav1.ConditionTrue
	}
	now := metav1.NewTime(r.Clock.Now())
	for conditionType, status := range map[string]metav1.ConditionStatus{
		coderv1alpha1.CoderAIGatewayKeyConditionReady:       ready,
		coderv1alpha1.CoderAIGatewayKeyConditionReconciling: reconciling,
		coderv1alpha1.CoderAIGatewayKeyConditionStalled:     stalled,
	} {
		meta.SetStatusCondition(&key.Status.Conditions, metav1.Condition{
			Type: conditionType, Status: status, ObservedGeneration: key.Generation,
			Reason: step.reason, Message: truncateTemplateTestMessage(step.message), LastTransitionTime: now,
		})
	}
}

// writeStatus writes changed status with optimistic concurrency, so a
// decision made on a stale object never persists.
func (r *CoderAIGatewayKeyReconciler) writeStatus(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, before *coderv1alpha1.CoderAIGatewayKeyStatus) error {
	key.Status.ObservedGeneration = key.Generation
	if equality.Semantic.DeepEqual(before, &key.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, key); err != nil {
		return fmt.Errorf("update status of AI Gateway key %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}
