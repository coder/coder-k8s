package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// aiGatewayKeySettleWindow is how long after the last create request the
// sweep waits before it deletes an orphan (plan 4.5). Every write of an
// attempt starts within aiGatewayKeyMaxWriteAge of the marker, and the API
// server ends a request within 60 s, so after 3 minutes no write of the
// attempt can still land. The rest is margin for clock skew between leaders.
const aiGatewayKeySettleWindow = 5 * time.Minute

const reasonDeletingOrphans = "DeletingOrphans"

// aiGatewayKeyAction is the one mutating step that a reconcile takes.
type aiGatewayKeyAction string

const (
	aiGatewayKeyReady      aiGatewayKeyAction = "Ready"      // Nothing to do.
	aiGatewayKeyAdopt      aiGatewayKeyAction = "Adopt"      // The Secret holds a newer key of this object (4.4).
	aiGatewayKeyRecover    aiGatewayKeyAction = "Recover"    // A marker from an attempt that did not finish (4.3).
	aiGatewayKeySweep      aiGatewayKeyAction = "Sweep"      // Delete one orphan (4.5).
	aiGatewayKeySettle     aiGatewayKeyAction = "Settle"     // An orphan waits for the settle window: no create (create gate).
	aiGatewayKeyIssue      aiGatewayKeyAction = "Issue"      // Issue the first key (4.3).
	aiGatewayKeyKillSwitch aiGatewayKeyAction = "KillSwitch" // Someone deleted the current key in Coder (4.7).
	aiGatewayKeyRearm      aiGatewayKeyAction = "Rearm"      // The spec changed after the kill switch: forget the deleted key.
	aiGatewayKeyReissue    aiGatewayKeyAction = "Reissue"    // The Secret lost the current key (D7).
)

// aiGatewayKeyDecision is the result of nextAIGatewayKeyAction.
type aiGatewayKeyDecision struct {
	action aiGatewayKeyAction
	// key is the held key for Adopt, and the first orphan otherwise.
	key    codersdk.AIGatewayKey
	serial int64 // The serial of key.
	// settleAt is when the sweep may delete the orphans. It is zero when
	// there is no orphan.
	settleAt time.Time
	// healthy reports that the Secret holds the current key and Coder lists
	// it, so a sweep keeps Ready=True.
	healthy bool
}

// settled reports that an orphan exists and its settle window has passed.
func (d aiGatewayKeyDecision) settled(now time.Time) bool {
	return !d.settleAt.IsZero() && !now.Before(d.settleAt)
}

// nextAIGatewayKeyAction is the pure decision of plan 4.2 step 7. own holds
// the listed keys whose name is prefix and a serial, and holds is the key
// that the Secret holds (uuid.Nil for none). The rows run in this order:
// recovery, then the first issue behind the create gate, then the kill
// switch, the steady state, and the drift re-issue. The sweep never comes
// before the kill switch or a re-issue. A stall still sweeps (delete-only
// mode, plan 4.8), and the observe step does that. K8 and K9 add rows.
func nextAIGatewayKeyAction(
	key *coderv1alpha1.CoderAIGatewayKey, prefix string, own []codersdk.AIGatewayKey, holds uuid.UUID, now time.Time,
) aiGatewayKeyDecision {
	status := &key.Status
	current := status.CurrentKey
	// Only serials below nextSerial can come from this object's create
	// requests (plan I3). A key with a higher serial is not this object's.
	created := func(k codersdk.AIGatewayKey) (int64, bool) {
		serial, ok := aiGatewayKeySerial(prefix, k.Name)
		return serial, ok && serial < status.NextSerial
	}
	var held *codersdk.AIGatewayKey
	for i := range own {
		if own[i].ID == holds && holds != uuid.Nil {
			held = &own[i]
		}
	}
	// Recovery 4.4: the Secret holds a key that this object created after
	// the current key, so a status write was lost after the Secret write.
	if held != nil && (current == nil || held.ID.String() != current.ID) {
		if serial, ok := created(*held); ok && (current == nil || serial > current.Serial) {
			return aiGatewayKeyDecision{action: aiGatewayKeyAdopt, key: *held, serial: serial}
		}
	}
	if status.PendingKey != nil {
		return aiGatewayKeyDecision{action: aiGatewayKeyRecover}
	}
	decision := aiGatewayKeyOrphan(key, prefix, own, holds, now)
	decision.healthy = current != nil && held != nil && held.ID.String() == current.ID

	switch {
	case current == nil && decision.settled(now):
		decision.action = aiGatewayKeySweep
	case current == nil && !decision.settleAt.IsZero():
		// Create gate: no create while an orphan of this object exists, so
		// lastCreateAttemptTime stops moving and the sweep runs.
		decision.action = aiGatewayKeySettle
	case current == nil:
		decision.action = aiGatewayKeyIssue
	case !aiGatewayKeyListed(own, current.ID) && aiGatewayKeyRearmed(key):
		decision.action = aiGatewayKeyRearm
	case !aiGatewayKeyListed(own, current.ID):
		decision.action = aiGatewayKeyKillSwitch
	case decision.healthy && decision.settled(now):
		decision.action = aiGatewayKeySweep
	case decision.healthy:
		decision.action = aiGatewayKeyReady
	default:
		decision.action = aiGatewayKeyReissue
	}
	return decision
}

// aiGatewayKeyOrphan returns a decision with the first listed key of this
// object that is not current, not retiring and not held (the Secret holds
// holds), its serial, and the time at which the sweep may delete it. The
// decision has no key and a zero time when there is no orphan.
func aiGatewayKeyOrphan(key *coderv1alpha1.CoderAIGatewayKey, prefix string, own []codersdk.AIGatewayKey, holds uuid.UUID, now time.Time) aiGatewayKeyDecision {
	status := &key.Status
	for _, k := range own {
		serial, ok := aiGatewayKeySerial(prefix, k.Name)
		// Only serials below nextSerial can come from this object's create
		// requests (plan I3). A key with a higher serial is not this object's.
		if !ok || serial >= status.NextSerial || (status.CurrentKey != nil && k.ID.String() == status.CurrentKey.ID) ||
			aiGatewayKeyRetiring(status, k.ID) || (holds != uuid.Nil && k.ID == holds) {
			continue
		}
		// The marker write sets lastCreateAttemptTime together with the
		// first serial, so only a status edit can lose it. Without it, the
		// window counts from now (fail closed), and the observe step stores
		// that time so the window ends.
		start := now
		if status.LastCreateAttemptTime != nil {
			start = status.LastCreateAttemptTime.Time
		}
		return aiGatewayKeyDecision{key: k, serial: serial, settleAt: start.Add(aiGatewayKeySettleWindow)}
	}
	return aiGatewayKeyDecision{}
}

// aiGatewayKeyRearmed reports that the kill switch fired for an older
// generation, so a spec change re-arms the issue step (decision D6). When
// another step replaced the kill switch's condition, the switch fires again
// for the current generation: it fails closed.
func aiGatewayKeyRearmed(key *coderv1alpha1.CoderAIGatewayKey) bool {
	stalled := meta.FindStatusCondition(key.Status.Conditions, coderv1alpha1.CoderAIGatewayKeyConditionStalled)
	return stalled != nil && stalled.Status == metav1.ConditionTrue && stalled.Reason == reasonKeyDeletedExternal &&
		stalled.ObservedGeneration < key.Generation
}

func aiGatewayKeyRetiring(status *coderv1alpha1.CoderAIGatewayKeyStatus, id uuid.UUID) bool {
	for _, r := range status.RetiringKeys {
		if r.ID == id.String() {
			return true
		}
	}
	return false
}

// adoptAIGatewayKey runs recovery 4.4: the key that the Secret holds becomes
// current, with issuedAt now (later than the real time, so the old key's
// grace period only grows). The previous current key, when Coder still
// lists it, retires from now. No key is created. This is safe because the
// Secret proves that the controller held the plaintext.
func adoptAIGatewayKey(key *coderv1alpha1.CoderAIGatewayKey, own []codersdk.AIGatewayKey, decision aiGatewayKeyDecision, now time.Time) *aiGatewayKeyStep {
	if previous := key.Status.CurrentKey; previous != nil && aiGatewayKeyListed(own, previous.ID) {
		key.Status.RetiringKeys = append(key.Status.RetiringKeys, coderv1alpha1.CoderAIGatewayKeyRetiring{
			Serial: previous.Serial, ID: previous.ID, Name: previous.Name, ReplacedAt: metav1.NewTime(now),
		})
	}
	if pending := key.Status.PendingKey; pending != nil && pending.Serial == decision.serial {
		key.Status.PendingKey = nil
	}
	// Status does not record which spec the attempt ran for, and the spec
	// can change between the attempt and this recovery. So the recovery never
	// counts the current trigger as handled or the generation as applied: a
	// newer trigger still starts its rotation. At worst one extra rotation
	// runs, which is safe, while a consumed trigger would not be.
	return setAIGatewayKeyCurrent(key, decision.serial, decision.key.ID, decision.key.Name, now, false)
}

// sweepOrphan deletes one orphan (plan 4.5) after the I1 live read: a key
// that the Secret holds is never deleted. One DELETE per reconcile, then
// requeue at once. A DELETE that Coder refuses with 403, or with 404 "Route
// not found.", needs a person, so it stalls the object. Other failures retry
// every 30 s with no deadline, as for a failed LIST.
func (r *CoderAIGatewayKeyReconciler) sweepOrphan(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, decision aiGatewayKeyDecision,
) *aiGatewayKeyStep {
	orphan := decision.key
	step := func(s *aiGatewayKeyStep) *aiGatewayKeyStep {
		// The Secret stays valid. A stall stays reported with it, because a
		// person must act on the orphan.
		if decision.healthy {
			s.ready, s.inProgress = true, !s.stalled
		}
		return s
	}
	held, err := r.liveSecretHolds(ctx, key, []codersdk.AIGatewayKey{orphan})
	if err != nil {
		return step(aiGatewayKeyWait(time.Second, reasonDeletingOrphans,
			"The operator did not delete orphan %s: reading Secret %q failed (%s).", orphan.Name, key.Spec.SecretName, aiGatewayKeyAPIFailure(err)))
	}
	if held == orphan.ID {
		return step(aiGatewayKeyWait(time.Second, reasonDeletingOrphans,
			"Secret %q now holds key %s, so the operator does not delete it.", key.Spec.SecretName, orphan.Name))
	}
	if err := sdk.DeleteAIGatewayKey(ctx, orphan.ID); (err != nil && coderStatus(err) != http.StatusNotFound) || coderRouteMissing(err) {
		failure := aiGatewayKeyCoderFailure("delete orphan key "+orphan.Name, err)
		if failure.stalled {
			failure.message = fmt.Sprintf("The operator cannot delete orphan key %s. %s", orphan.Name, failure.message)
		}
		return step(failure)
	}
	return step(aiGatewayKeyWait(time.Second, reasonDeletingOrphans, "Deleted orphan key %s.", orphan.Name))
}

// coderRouteMissing reports Coder's 404 for a route that it does not have
// (plan F11). Unlike the 404 for a missing key, it proves nothing was
// deleted.
func coderRouteMissing(err error) bool {
	var sdkErr *codersdk.Error
	return errors.As(err, &sdkErr) && sdkErr.StatusCode() == http.StatusNotFound && sdkErr.Message == aiGatewayRouteNotFound
}

// liveSecretHolds live-reads the output Secret and returns the key in own
// that it holds (plan I1), whoever owns the Secret: in delete-only mode a
// replaced Secret can still hold a key of this object (plan 4.8 rule 2). The
// value stays in memory and is only compared with key prefixes.
func (r *CoderAIGatewayKeyReconciler) liveSecretHolds(ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, own []codersdk.AIGatewayKey) (uuid.UUID, error) {
	secret := &corev1.Secret{}
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Spec.SecretName}, secret)
	if apierrors.IsNotFound(err) {
		return uuid.Nil, nil // No Secret holds any key (plan 4.8 rule 3).
	} else if err != nil {
		return uuid.Nil, err
	}
	return aiGatewayKeySecretHolds(secret, own), nil
}

// deleteOnly runs the sweep inside a stall (plan 4.8): the stall stays the
// reported condition, so the sweep never hides it, and no create runs.
// Callers pass only stalls.
func (r *CoderAIGatewayKeyReconciler) deleteOnly(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, decision aiGatewayKeyDecision, stall *aiGatewayKeyStep, now time.Time,
) *aiGatewayKeyStep {
	if decision.settleAt.IsZero() {
		return stall
	}
	if !decision.settled(now) {
		stall.requeue = min(stall.requeue, max(decision.settleAt.Sub(now), time.Second))
		return stall
	}
	decision.healthy = false
	swept := r.sweepOrphan(ctx, sdk, key, decision)
	stall.message += " " + swept.message
	stall.requeue, stall.rateLimited = swept.requeue, swept.rateLimited
	return stall
}
