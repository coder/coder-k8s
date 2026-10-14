package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
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
	aiGatewayKeyReissue    aiGatewayKeyAction = "Reissue"    // The Secret lost the current key (D7).
)

// aiGatewayKeyDecision is the result of nextAIGatewayKeyAction.
type aiGatewayKeyDecision struct {
	action aiGatewayKeyAction
	// key is the held key for Adopt, and the orphan for Sweep and Settle.
	key    codersdk.AIGatewayKey
	serial int64 // The serial of key.
	// settleAt is when the sweep may delete the orphans, when there are
	// some.
	settleAt time.Time
	// healthy reports that the Secret holds the current key and Coder lists
	// it, so a sweep keeps Ready=True.
	healthy bool
}

// nextAIGatewayKeyAction is the pure decision of plan 4.2 step 7. own holds
// the listed keys whose name is prefix and a serial, and holds is the key
// that the Secret holds (uuid.Nil for none). K7 to K9 add rows.
func nextAIGatewayKeyAction(
	status *coderv1alpha1.CoderAIGatewayKeyStatus, prefix string, own []codersdk.AIGatewayKey, holds uuid.UUID, now time.Time,
) aiGatewayKeyDecision {
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
	healthy := current != nil && held != nil && held.ID.String() == current.ID

	var decision aiGatewayKeyDecision
	for _, k := range own {
		serial, ok := created(k)
		if !ok || (current != nil && k.ID.String() == current.ID) || aiGatewayKeyRetiring(status, k.ID) || (held != nil && k.ID == held.ID) {
			continue
		}
		// The marker write sets lastCreateAttemptTime together with the
		// first serial, so it is set whenever an orphan can exist.
		if status.LastCreateAttemptTime != nil {
			decision.settleAt = status.LastCreateAttemptTime.Add(aiGatewayKeySettleWindow)
		}
		decision.key, decision.serial = k, serial
		if !now.Before(decision.settleAt) {
			return aiGatewayKeyDecision{action: aiGatewayKeySweep, key: k, serial: serial, settleAt: decision.settleAt, healthy: healthy}
		}
		if current == nil {
			// Create gate: no create while an orphan of this object exists,
			// so lastCreateAttemptTime stops moving and the sweep runs.
			decision.action = aiGatewayKeySettle
			return decision
		}
		break
	}
	switch {
	case current == nil:
		decision.action = aiGatewayKeyIssue
	case !aiGatewayKeyListed(own, current.ID):
		decision.action = aiGatewayKeyKillSwitch
	case healthy:
		decision.action = aiGatewayKeyReady
	default:
		decision.action = aiGatewayKeyReissue
	}
	return decision
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
	return setAIGatewayKeyCurrent(key, decision.serial, decision.key.ID, decision.key.Name, now)
}

// sweepOrphan deletes one orphan (plan 4.5) after the I1 live read: a key
// that the Secret holds is never deleted. One DELETE per reconcile, then
// requeue at once.
func (r *CoderAIGatewayKeyReconciler) sweepOrphan(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, decision aiGatewayKeyDecision,
) *aiGatewayKeyStep {
	orphan := decision.key
	step := func(format string, args ...any) *aiGatewayKeyStep {
		s := aiGatewayKeyWait(time.Second, reasonDeletingOrphans, format, args...)
		s.ready, s.inProgress = decision.healthy, decision.healthy
		return s
	}
	secret, err := getOwnedSecret(ctx, r.APIReader, key, aiGatewayKeyKind, key.Spec.SecretName)
	if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
		return aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message)
	} else if err != nil {
		return step("The operator did not delete orphan %s: reading Secret %q failed (%s).", orphan.Name, key.Spec.SecretName, aiGatewayKeyAPIFailure(err))
	}
	if aiGatewayKeySecretHolds(secret, []codersdk.AIGatewayKey{orphan}) == orphan.ID {
		return step("Secret %q now holds key %s, so the operator does not delete it.", key.Spec.SecretName, orphan.Name)
	}
	if err := sdk.DeleteAIGatewayKey(ctx, orphan.ID); err != nil && coderStatus(err) != http.StatusNotFound {
		unavailable := coderStepUnavailable(fmt.Sprintf("delete orphan key %s", orphan.Name), err)
		s := aiGatewayKeyWait(aiGatewayKeyRetry, unavailable.reason, "%s", unavailable.message)
		s.rateLimited = unavailable.rateLimited
		return s
	}
	return step("Deleted orphan key %s.", orphan.Name)
}
