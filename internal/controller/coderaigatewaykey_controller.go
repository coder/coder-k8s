package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
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

// aiGatewayKeyPlaceholder has the format of a key, so the dry-run write sends
// the same shape of data as a real write. It is not a key.
var aiGatewayKeyPlaceholder = []byte(strings.Repeat("0", 43))

// Annotations on the output Secret (plan 5.2). They change with each key, so
// a reloader that watches the Secret sees every rotation.
const (
	aiGatewayKeyIDAnnotation   = "coder.com/ai-gateway-key-id"
	aiGatewayKeyNameAnnotation = "coder.com/ai-gateway-key-name"
)

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
	inProgress  bool // With ready: Reconciling=True too, for work that keeps the Secret valid.
	stalled     bool // With ready: Stalled=True too, for a failure beside a valid Secret that a person must fix.
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
	if key.Status.ObservedGeneration != key.Generation {
		// A spec change re-arms the issue step after the failure cap.
		key.Status.ConsecutiveIssueFailures = 0
	}
	step, err := r.observe(ctx, key, before)
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
func (r *CoderAIGatewayKeyReconciler) observe(
	ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, before *coderv1alpha1.CoderAIGatewayKeyStatus,
) (*aiGatewayKeyStep, error) {
	sdk, controlPlane, wait, err := controlPlaneCoderClient(ctx, r.Client, key.Namespace, key.Spec.ControlPlaneRef.Name)
	if err != nil {
		return nil, err
	}
	if wait != nil {
		return aiGatewayKeyWait(aiGatewayKeyRetry, wait.reason, "%s", wait.message), nil
	}
	// A stall found before the LIST sends nothing to Coder while no key of
	// this object can exist (nextSerial == 0, plan 4.8 rule 4). Otherwise it
	// runs in delete-only mode.
	var stall *aiGatewayKeyStep
	secret, err := getOwnedSecret(ctx, r.APIReader, key, aiGatewayKeyKind, key.Spec.SecretName)
	if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
		stall = aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message)
	} else if err != nil {
		return nil, err
	}
	if stall == nil {
		err = reservedSecretName(ctx, r.APIReader, key, key.Spec.SecretName, secret != nil)
		if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
			stall = aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message)
		} else if err != nil {
			return nil, err
		}
	}
	if stall == nil && (controlPlane.Spec.AIGateway == nil || !controlPlane.Spec.AIGateway.AllowKeys) {
		stall = aiGatewayKeyStalled("NotAllowed",
			"CoderControlPlane %s does not allow AI Gateway keys. Its admin can set spec.aiGateway.allowKeys to true.", controlPlane.Name)
	}
	if stall != nil && key.Status.NextSerial == 0 {
		return stall, nil
	}
	prefix, err := aiGatewayKeyNamePrefix(key.UID)
	if err != nil {
		return nil, err
	}
	listed, err := sdk.ListAIGatewayKeys(ctx)
	if err != nil && stall != nil {
		return stall, nil // The stall says more than a failed LIST. The sweep runs at the next resync.
	} else if err != nil {
		return aiGatewayKeyCoderFailure("list AI Gateway keys", err), nil
	}
	own := ownAIGatewayKeys(prefix, listed)
	// A retiring key that Coder no longer lists is gone: deleted by a
	// retire step whose answer was lost, or by someone else (plan 4.7).
	dropAIGatewayKeyRetiring(key, func(r coderv1alpha1.CoderAIGatewayKeyRetiring) bool { return !aiGatewayKeyListed(own, r.ID) })

	now := r.Clock.Now()
	if key.Status.LastCreateAttemptTime == nil && key.Status.NextSerial > 0 {
		// Only a status edit loses the time of the last create. The settle
		// window counts from now (fail closed), and status keeps that time,
		// so the window ends.
		stamp := metav1.NewTime(now)
		key.Status.LastCreateAttemptTime = &stamp
	}
	setAIGatewayKeyNextRotation(key) // An interval change applies at once (plan 3.4).
	if current := key.Status.CurrentKey; current != nil && aiGatewayKeyListed(own, current.ID) {
		// Coder lists the current key, so no kill switch holds. A stale
		// generation would re-arm a later kill switch at once.
		key.Status.KillSwitchGeneration = 0
	}
	if stall != nil {
		// The Secret is not this object's, but its live value still decides
		// I1 for the sweep (plan 4.8 rule 2).
		holds, err := r.liveSecretHolds(ctx, key, own)
		if err != nil {
			return nil, err
		}
		return r.deleteOnly(ctx, sdk, key, aiGatewayKeyOrphan(key, prefix, own, holds, now), stall, now), nil
	}

	decision := nextAIGatewayKeyAction(key, prefix, own, aiGatewayKeySecretHolds(secret, own), now)
	switch decision.action {
	case aiGatewayKeyAdopt:
		return adoptAIGatewayKey(key, own, decision, now)
	case aiGatewayKeyRecover:
		pending := key.Status.PendingKey
		key.Status.PendingKey = nil
		return aiGatewayKeyIssueFailed(key, fmt.Sprintf(
			"The create of key %s did not finish: the operator restarted, or a status write was lost. A committed key is an orphan.", pending.Name)), nil
	case aiGatewayKeySweep:
		if stall := aiGatewayKeyIssueBlocked(key); stall != nil && !decision.healthy {
			return r.deleteOnly(ctx, sdk, key, decision, stall, now), nil
		}
		return r.sweepOrphan(ctx, sdk, key, decision), nil
	case aiGatewayKeySettle:
		if stall := aiGatewayKeyIssueBlocked(key); stall != nil {
			return r.deleteOnly(ctx, sdk, key, decision, stall, now), nil
		}
		return aiGatewayKeyWait(max(decision.settleAt.Sub(now), time.Second), reasonDeletingOrphans,
			"Key %s of this object is an orphan. The operator creates no key until it deletes the orphan at %s, when no write of the last create attempt can still land.",
			decision.key.Name, decision.settleAt.UTC().Format(time.RFC3339)), nil
	case aiGatewayKeyIssue, aiGatewayKeyReissue, aiGatewayKeyRotate:
		serial := key.Status.NextSerial
		step, err := r.issueKey(ctx, sdk, key, before, prefix, decision.action)
		if err == nil && step.stalled && key.Status.NextSerial == serial {
			// The issue step stopped before the create, so the stall sweeps.
			step = r.deleteOnly(ctx, sdk, key, decision, step, now)
		}
		if err == nil && !step.ready && decision.action == aiGatewayKeyRotate {
			step = aiGatewayKeyKeepReady(key, step, true) // The Secret still holds the current key.
		}
		return step, err
	case aiGatewayKeyKillSwitch:
		if key.Status.KillSwitchGeneration == 0 {
			key.Status.KillSwitchGeneration = key.Generation
		}
		return r.deleteOnly(ctx, sdk, key, decision, aiGatewayKeyStalled(reasonKeyDeletedExternal,
			"Coder no longer lists the current key %s. Someone deleted it outside the operator, so the operator creates no new key until the spec changes.",
			key.Status.CurrentKey.Name), now), nil
	case aiGatewayKeyRearm:
		// Coder no longer lists the key, so there is nothing to retire. The
		// next reconcile issues a key behind the create gate.
		deleted := key.Status.CurrentKey.Name
		key.Status.CurrentKey, key.Status.NextRotationTime, key.Status.KillSwitchGeneration = nil, nil, 0
		return aiGatewayKeyWait(time.Second, reasonIssuingKey,
			"The spec changed after the kill switch, so the operator issues a new key in place of deleted key %s.", deleted), nil
	case aiGatewayKeyConflict:
		return r.deleteOnly(ctx, sdk, key, decision, aiGatewayKeyDriftBlocked(key, now), now), nil
	case aiGatewayKeyRetire:
		return r.retireKey(ctx, sdk, key, decision), nil
	case aiGatewayKeyReady:
		step := aiGatewayKeyReadyStep(key)
		if _, blocked := aiGatewayKeyRotationDue(key, now); blocked != nil {
			step = blocked
		} else if waiting := decision.retirement; waiting.reason != "" {
			// A replaced key waits: Ready stays True (plan 3.3).
			step.inProgress, step.reason, step.message = true, waiting.reason, waiting.message
		}
		step.requeue = aiGatewayKeyResyncAfter(key, now)
		for _, at := range []time.Time{decision.settleAt, decision.retirement.next} {
			if !at.IsZero() {
				// Come back to sweep the orphan or to retire the key.
				step.requeue = min(step.requeue, max(at.Sub(now), time.Second))
			}
		}
		return step, nil
	}
	return nil, fmt.Errorf("assertion failed: unknown AI Gateway key action %q", decision.action)
}

// prepareSecret runs the first two parts of the issue step (plan 4.3): the
// output Secret exists and this object controls it, and the API server
// accepts a write of a key into it, before any key exists. It returns the
// Secret's labels, or a step when the issue step cannot go on.
func (r *CoderAIGatewayKeyReconciler) prepareSecret(
	ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, prefix string,
) (map[string]string, *aiGatewayKeyStep, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":     aiGatewayKeyAppName,
		"app.kubernetes.io/instance": workspaceProxyInstanceLabelValue(key.Name),
	}
	secret, err := claimOwnedSecret(ctx, r.Client, r.APIReader, r.Scheme, key, aiGatewayKeyKind, key.Spec.SecretName, labels)
	if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
		return nil, aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message), nil
	} else if err != nil {
		return nil, nil, err
	}
	// The dry run sends what the real write will send, with a placeholder
	// in place of the key, so an admission webhook, a policy or a quota that
	// refuses it stops the issue step while no key exists. The object that a
	// dry run returns is not the stored Secret, so it is dropped.
	annotations := map[string]string{
		aiGatewayKeyIDAnnotation:   uuid.Nil.String(),
		aiGatewayKeyNameAnnotation: prefix + strconv.FormatInt(key.Status.NextSerial, 10),
	}
	_, err = writeOwnedSecret(ctx, r.Client, key, aiGatewayKeyKind, secret,
		map[string][]byte{coderv1alpha1.CoderAIGatewayKeySecretDataKey: aiGatewayKeyPlaceholder}, labels, annotations, client.DryRunAll)
	if err != nil {
		step, err := aiGatewayKeyWriteFailure(key.Spec.SecretName, err)
		return nil, step, err
	}
	return labels, nil, nil
}

// aiGatewayKeyWriteFailure maps a failed write of the output Secret. The
// write can carry a key, so neither the step nor the returned error holds
// the API server's message, only its reason and code. Errors without an API
// status (assertion failures, a client-side timeout) hold no server text and
// pass through.
func aiGatewayKeyWriteFailure(name string, err error) (*aiGatewayKeyStep, error) {
	var apiStatus apierrors.APIStatus
	if !errors.As(err, &apiStatus) {
		return nil, err
	}
	reason, code := apiStatus.Status().Reason, apiStatus.Status().Code
	if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err) {
		return aiGatewayKeyStalled(reasonSecretWriteRejected,
			"The API server refused a write of Secret %q (%s, HTTP %d). Check the admission webhooks, policies and quotas for Secrets in this namespace.",
			name, reason, code), nil
	}
	return nil, fmt.Errorf("write output Secret %q: the API server answered %s (HTTP %d)", name, reason, code)
}

// aiGatewayKeyCoderFailure classifies a failed LIST (plan 4.2 step 4) or
// sweep DELETE. The message holds the status and Coder's short message only.
func aiGatewayKeyCoderFailure(action string, err error) *aiGatewayKeyStep {
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
	unavailable := coderStepUnavailable(action, err)
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
		if _, ok := aiGatewayKeySerial(prefix, k.Name); ok {
			own = append(own, k)
		}
	}
	return own
}

// aiGatewayKeySerial returns the serial of a key name that is prefix and a
// serial in the canonical form that the controller writes: decimal digits
// without leading zeros. "00" is not serial 0, so a key that someone else
// named that way is never this object's.
func aiGatewayKeySerial(prefix, name string) (int64, bool) {
	digits, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	serial, err := strconv.ParseInt(digits, 10, 64)
	return serial, err == nil && serial >= 0 && strconv.FormatInt(serial, 10) == digits
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
	var changed bool
	if add {
		changed = controllerutil.AddFinalizer(key, coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer)
	} else {
		changed = controllerutil.RemoveFinalizer(key, coderv1alpha1.CoderAIGatewayKeyCleanupFinalizer)
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
	case step.ready && step.stalled:
		ready, reconciling, stalled = metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionTrue
	case step.ready && step.inProgress:
		ready = metav1.ConditionTrue
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
