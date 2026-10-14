package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// aiGatewayKeySettleWindow is how long after the last create request the
// sweep waits before it deletes an orphan (plan 4.5). Every write of an
// attempt starts within aiGatewayKeyMaxWriteAge of the marker, and the API
// server ends a request within 60 s, so after 3 minutes no write of the
// attempt can still land. The rest is margin for clock skew between leaders.
const aiGatewayKeySettleWindow = 5 * time.Minute

const (
	reasonDeletingOrphans = "DeletingOrphans"
	reasonSecretConflict  = "SecretConflict"
	// aiGatewayKeyMaxRetiring is the API limit of status.retiringKeys.
	aiGatewayKeyMaxRetiring = 3
	// aiGatewayKeyDefaultGraceSeconds is the API default of
	// spec.rotation.gracePeriodSeconds.
	aiGatewayKeyDefaultGraceSeconds = 3600
)

// aiGatewayKeyAction is the one mutating step that a reconcile takes.
type aiGatewayKeyAction string

const (
	aiGatewayKeyReady      aiGatewayKeyAction = "Ready"      // Nothing to do.
	aiGatewayKeyAdopt      aiGatewayKeyAction = "Adopt"      // The Secret holds a newer key of this object (4.4).
	aiGatewayKeyRecover    aiGatewayKeyAction = "Recover"    // A marker from an attempt that did not finish (4.3).
	aiGatewayKeySweep      aiGatewayKeyAction = "Sweep"      // Delete one orphan (4.5).
	aiGatewayKeyRetire     aiGatewayKeyAction = "Retire"     // Delete one retiring key after its grace period (4.6).
	aiGatewayKeySettle     aiGatewayKeyAction = "Settle"     // An orphan waits for the settle window: no create (create gate).
	aiGatewayKeyIssue      aiGatewayKeyAction = "Issue"      // Issue the first key (4.3).
	aiGatewayKeyKillSwitch aiGatewayKeyAction = "KillSwitch" // Someone deleted the current key in Coder (4.7).
	aiGatewayKeyRearm      aiGatewayKeyAction = "Rearm"      // The spec changed after the kill switch: forget the deleted key.
	aiGatewayKeyReissue    aiGatewayKeyAction = "Reissue"    // The Secret lost the current key (D7).
	aiGatewayKeyRotate     aiGatewayKeyAction = "Rotate"     // A scheduled or requested rotation (3.4).
	aiGatewayKeyConflict   aiGatewayKeyAction = "Conflict"   // The Secret lost the key again too soon (D7).
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
	// retirement is the state of the retiring keys (plan 4.6).
	retirement aiGatewayKeyRetirement
}

// settled reports that an orphan exists and its settle window has passed.
func (d aiGatewayKeyDecision) settled(now time.Time) bool {
	return !d.settleAt.IsZero() && !now.Before(d.settleAt)
}

// nextAIGatewayKeyAction is the pure decision of plan 4.2 step 7. own holds
// the listed keys whose name is prefix and a serial, and holds is the key
// that the Secret holds (uuid.Nil for none). The rows run in this order:
// recovery, then the first issue behind the create gate, then the kill
// switch, the drift re-issue behind the create gate, and the steady state
// with its retire step, sweep and rotation. The sweep never comes before the kill
// switch. Every create waits behind the create gate, so at most one visible
// orphan exists at a time. A stall still retires keys and sweeps
// (delete-only mode, plan 4.8), and the observe step does that.
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
	case !decision.healthy && aiGatewayKeyDriftBlocked(key, now) != nil:
		decision.action = aiGatewayKeyConflict
	case !decision.healthy && decision.settled(now):
		decision.action = aiGatewayKeySweep
	case !decision.healthy && !decision.settleAt.IsZero():
		decision.action = aiGatewayKeySettle
	case !decision.healthy:
		decision.action = aiGatewayKeyReissue
	case decision.retirement.due != nil:
		decision.action = aiGatewayKeyRetire
	case decision.settled(now):
		decision.action = aiGatewayKeySweep
	default:
		// A rotation waits behind the create gate too: the Ready row requeues
		// at the end of the settle window.
		if due, _ := aiGatewayKeyRotationDue(key, now); due && decision.settleAt.IsZero() {
			decision.action = aiGatewayKeyRotate
		} else {
			decision.action = aiGatewayKeyReady
		}
	}
	return decision
}

// aiGatewayKeyOrphan returns a decision with the first listed key of this
// object that is not current, not retiring and not held (the Secret holds
// holds), its serial, and the time at which the sweep may delete it. The
// decision has no key and a zero time when there is no orphan. It also
// holds the state of the retiring keys.
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
		return aiGatewayKeyDecision{key: k, serial: serial, settleAt: start.Add(aiGatewayKeySettleWindow), retirement: aiGatewayKeyRetireState(key, own, holds, now)}
	}
	return aiGatewayKeyDecision{retirement: aiGatewayKeyRetireState(key, own, holds, now)}
}

// aiGatewayKeyRearmed reports that the kill switch fired in an older
// generation, so a spec change re-arms the issue step (decision D6). Status
// keeps that generation, so a transient failure between the spec change and
// the re-arm does not lose it. A kill switch that never fired (generation 0)
// fires first: a spec change before the switch fired re-arms nothing.
func aiGatewayKeyRearmed(key *coderv1alpha1.CoderAIGatewayKey) bool {
	fired := key.Status.KillSwitchGeneration
	return fired > 0 && fired < key.Generation
}

// aiGatewayKeyDriftBlocked returns the SecretConflict stall when the Secret
// lost the current key and a re-issue is not allowed (decision D7): one
// already ran inside the grace period, or retiringKeys has no room left.
func aiGatewayKeyDriftBlocked(key *coderv1alpha1.CoderAIGatewayKey, now time.Time) *aiGatewayKeyStep {
	grace := time.Duration(ptr.Deref(key.Spec.Rotation.GracePeriodSeconds, aiGatewayKeyDefaultGraceSeconds)) * time.Second
	if last := key.Status.LastDriftReissueTime; last != nil && now.Before(last.Add(grace)) {
		step := aiGatewayKeyStalled(reasonSecretConflict,
			"Secret %q lost key %s again within the grace period after the re-issue at %s. Another writer, such as a GitOps tool, likely manages the same Secret.",
			key.Spec.SecretName, key.Status.CurrentKey.Name, last.UTC().Format(time.RFC3339))
		step.requeue = min(step.requeue, max(last.Add(grace).Sub(now), time.Second))
		return step
	}
	if n := len(key.Status.RetiringKeys); n >= aiGatewayKeyMaxRetiring {
		return aiGatewayKeyStalled(reasonSecretConflict,
			"Secret %q lost key %s, and %d replaced keys still wait for deletion, so the operator issues no new key. Another writer likely manages the same Secret.",
			key.Spec.SecretName, key.Status.CurrentKey.Name, n)
	}
	return nil
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
func adoptAIGatewayKey(key *coderv1alpha1.CoderAIGatewayKey, own []codersdk.AIGatewayKey, decision aiGatewayKeyDecision, now time.Time) (*aiGatewayKeyStep, error) {
	if previous := key.Status.CurrentKey; previous != nil && aiGatewayKeyListed(own, previous.ID) {
		if err := retireAIGatewayKey(key, now); err != nil {
			return nil, err
		}
		// Status does not record whether the lost attempt was a drift
		// re-issue, so the recovery counts it as one (fail closed): another
		// loss of the Secret inside the grace period gives SecretConflict,
		// not a second re-issue at once.
		t := metav1.NewTime(now)
		key.Status.LastDriftReissueTime = &t
	}
	if pending := key.Status.PendingKey; pending != nil && pending.Serial == decision.serial {
		key.Status.PendingKey = nil
	}
	// Status does not record which spec the attempt ran for, and the spec
	// can change between the attempt and this recovery. So the recovery never
	// counts the current trigger as handled or the generation as applied: a
	// newer trigger still starts its rotation. At worst one extra rotation
	// runs, which is safe, while a consumed trigger would not be.
	return setAIGatewayKeyCurrent(key, decision.serial, decision.key.ID, decision.key.Name, now, false), nil
}

// retireAIGatewayKey moves the current key to retiringKeys, with its grace
// period counting from now. Every create first checks that the list has room
// (a drift re-issue below 3 entries, a requested rotation below 2, a
// scheduled rotation at 0), so recovery 4.4 of that create also fits. A full
// list here is a broken rule: the API's limit of 3 would refuse the status
// write.
func retireAIGatewayKey(key *coderv1alpha1.CoderAIGatewayKey, now time.Time) error {
	previous := key.Status.CurrentKey
	if n := len(key.Status.RetiringKeys); n >= aiGatewayKeyMaxRetiring {
		return fmt.Errorf("assertion failed: AI Gateway key %s/%s already has %d retiring keys, so it cannot retire key %s",
			key.Namespace, key.Name, n, previous.Name)
	}
	key.Status.RetiringKeys = append(key.Status.RetiringKeys, coderv1alpha1.CoderAIGatewayKeyRetiring{
		Serial: previous.Serial, ID: previous.ID, Name: previous.Name, ReplacedAt: metav1.NewTime(now),
	})
	return nil
}

// sweepOrphan deletes one orphan (plan 4.5). One DELETE per reconcile,
// then requeue at once.
func (r *CoderAIGatewayKeyReconciler) sweepOrphan(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, decision aiGatewayKeyDecision,
) *aiGatewayKeyStep {
	step := r.deleteOwnKey(ctx, sdk, key, decision.key, reasonDeletingOrphans, "orphan key")
	if step == nil {
		step = aiGatewayKeyWait(time.Second, reasonDeletingOrphans, "Deleted orphan key %s.", decision.key.Name)
	}
	return aiGatewayKeyKeepReady(key, step, decision.healthy)
}

// deleteOwnKey deletes key k of this object after the I1 live read: a key
// that the Secret holds is never deleted. It returns nil when Coder no
// longer has k (204 or 404), and otherwise the step that reports why not. A
// DELETE that Coder refuses with 403, or with 404 "Route not found.", needs
// a person, so it stalls the object. Other failures retry every 30 s with no
// deadline, as for a failed LIST. noun names k in messages.
func (r *CoderAIGatewayKeyReconciler) deleteOwnKey(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, k codersdk.AIGatewayKey, reason, noun string,
) *aiGatewayKeyStep {
	held, err := r.liveSecretHolds(ctx, key, []codersdk.AIGatewayKey{k})
	if err != nil {
		return aiGatewayKeyWait(time.Second, reason,
			"The operator did not delete %s %s: reading Secret %q failed (%s).", noun, k.Name, key.Spec.SecretName, aiGatewayKeyAPIFailure(err))
	}
	if held == k.ID {
		return aiGatewayKeyWait(time.Second, reason, "Secret %q now holds key %s, so the operator does not delete it.", key.Spec.SecretName, k.Name)
	}
	// Only Coder's own 404 proves that the key is gone. An HTML 404 of a proxy
	// or "Route not found." proves nothing.
	if err := sdk.DeleteAIGatewayKey(ctx, k.ID); err != nil && !isCoderResourceNotFound(err) {
		failure := aiGatewayKeyCoderFailure("delete "+noun+" "+k.Name, err)
		if failure.stalled {
			failure.message = fmt.Sprintf("The operator cannot delete %s %s. %s", noun, k.Name, failure.message)
		}
		return failure
	}
	return nil
}

// aiGatewayKeyKeepReady keeps Ready=True on a cleanup step while the Secret
// holds the current key (healthy). A stall stays reported with it, because
// a person must act, and its message says that the key still works.
func aiGatewayKeyKeepReady(key *coderv1alpha1.CoderAIGatewayKey, step *aiGatewayKeyStep, healthy bool) *aiGatewayKeyStep {
	if !healthy {
		return step
	}
	step.ready, step.inProgress = true, !step.stalled
	// A stall read back from its condition already has the note.
	if note := fmt.Sprintf("The key in Secret %q still works. ", key.Spec.SecretName); step.stalled && !strings.HasPrefix(step.message, note) {
		step.message = note + step.message
	}
	return step
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

// deleteOnly runs the retire step or the sweep inside a stall (plan 4.8):
// the stall stays the reported condition, so the cleanup never hides it, and
// no create runs. Callers pass only stalls.
func (r *CoderAIGatewayKeyReconciler) deleteOnly(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, decision aiGatewayKeyDecision, stall *aiGatewayKeyStep, now time.Time,
) *aiGatewayKeyStep {
	decision.healthy = false
	var cleanup *aiGatewayKeyStep
	switch {
	case decision.retirement.due != nil:
		cleanup = r.retireKey(ctx, sdk, key, decision)
	case decision.settled(now):
		cleanup = r.sweepOrphan(ctx, sdk, key, decision)
	default:
		for _, at := range []time.Time{decision.settleAt, decision.retirement.next} {
			if !at.IsZero() {
				stall.requeue = min(stall.requeue, max(at.Sub(now), time.Second))
			}
		}
		return stall
	}
	stall.message += " " + cleanup.message
	stall.requeue, stall.rateLimited = cleanup.requeue, cleanup.rateLimited
	return stall
}
