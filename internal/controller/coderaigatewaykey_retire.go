package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"k8s.io/utils/ptr"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

// Retire step (plan 4.6).
const (
	reasonWaitingForGracePeriod = "WaitingForGracePeriod"
	reasonWaitingForConsumers   = "WaitingForConsumers"
	reasonRetiringKey           = "RetiringKey"
	// aiGatewayKeyDefaultConsumerWaitSeconds is the API default of
	// spec.rotation.consumerWaitSeconds.
	aiGatewayKeyDefaultConsumerWaitSeconds = 86400
	// aiGatewayKeyHeartbeatStale is the age after which a heartbeat proves
	// no live connection. Connections heartbeat every 60 s (plan F22). The
	// rest is margin for skew between the controller and the Coder database
	// clock (plan F9).
	aiGatewayKeyHeartbeatStale = 10 * time.Minute
	// aiGatewayKeyHeartbeatNewer is how much newer the current key's
	// heartbeat must be than a retiring key's to show that the consumers
	// switched. Both are database times.
	aiGatewayKeyHeartbeatNewer = 2 * time.Minute
)

// aiGatewayKeyRetirement is the state of the retiring keys at one reconcile.
type aiGatewayKeyRetirement struct {
	// due is a listed retiring key that the retire step may delete now.
	due *codersdk.AIGatewayKey
	// reason and message describe the first retiring key that still waits.
	// reason is "" when no key waits.
	reason, message string
	// next is when a waiting key can become due. It is zero when no key
	// waits.
	next time.Time
}

// aiGatewayKeyRetireState applies plan 4.6 conditions 1 and 2 to every
// listed retiring key. Condition 3, the I1 live read, runs right before the
// DELETE. The grace period and the consumer wait come from the current
// spec, so a change applies at once. The API keeps the grace period at 600 s
// or more.
func aiGatewayKeyRetireState(key *coderv1alpha1.CoderAIGatewayKey, own []codersdk.AIGatewayKey, now time.Time) aiGatewayKeyRetirement {
	grace := time.Duration(ptr.Deref(key.Spec.Rotation.GracePeriodSeconds, aiGatewayKeyDefaultGraceSeconds)) * time.Second
	wait := time.Duration(ptr.Deref(key.Spec.Rotation.ConsumerWaitSeconds, aiGatewayKeyDefaultConsumerWaitSeconds)) * time.Second
	var currentBeat *time.Time
	if current := key.Status.CurrentKey; current != nil {
		if i := slices.IndexFunc(own, func(k codersdk.AIGatewayKey) bool { return k.ID.String() == current.ID }); i >= 0 {
			currentBeat = own[i].LastHeartbeatAt
		}
	}
	var state aiGatewayKeyRetirement
	waits := func(reason string, at time.Time, format string, args ...any) {
		if state.reason == "" {
			state.reason, state.message = reason, fmt.Sprintf(format, args...)
		}
		if state.next.IsZero() || at.Before(state.next) {
			state.next = at
		}
	}
	for _, r := range key.Status.RetiringKeys {
		i := slices.IndexFunc(own, func(k codersdk.AIGatewayKey) bool { return k.ID.String() == r.ID })
		if i < 0 {
			continue // Gone in Coder. The observe step drops it from status.
		}
		graceEnd := r.ReplacedAt.Add(grace)
		if now.Before(graceEnd) {
			waits(reasonWaitingForGracePeriod, graceEnd, "Replaced key %s stays valid until %s, when its grace period ends.",
				r.Name, graceEnd.UTC().Format(time.RFC3339))
			continue
		}
		deadline := graceEnd.Add(wait)
		beat := own[i].LastHeartbeatAt
		live := beat != nil && now.Sub(*beat) <= aiGatewayKeyHeartbeatStale &&
			(currentBeat == nil || currentBeat.Before(beat.Add(aiGatewayKeyHeartbeatNewer)))
		if live && now.Before(deadline) {
			// Look again when the heartbeat goes stale or the wait ends.
			next := beat.Add(aiGatewayKeyHeartbeatStale + time.Second)
			if deadline.Before(next) {
				next = deadline
			}
			waits(reasonWaitingForConsumers, next,
				"Key %s still has live connections (last heartbeat %s). Restart the AI Gateway pods so that they read the new key. The operator deletes this key at %s at the latest.",
				r.Name, beat.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339))
			continue
		}
		if state.due == nil {
			state.due = &own[i]
		}
	}
	return state
}

// retireKey deletes the due retiring key after the I1 live read, and drops
// it from status when Coder no longer has it.
func (r *CoderAIGatewayKeyReconciler) retireKey(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, decision aiGatewayKeyDecision,
) *aiGatewayKeyStep {
	due := *decision.retirement.due
	step := r.deleteOwnKey(ctx, sdk, key, due, reasonRetiringKey, "replaced key")
	if step == nil {
		dropAIGatewayKeyRetiring(key, func(r coderv1alpha1.CoderAIGatewayKeyRetiring) bool { return r.ID == due.ID.String() })
		step = aiGatewayKeyWait(time.Second, reasonRetiringKey, "Deleted replaced key %s.", due.Name)
	}
	return aiGatewayKeyKeepReady(key, step, decision.healthy)
}

// dropAIGatewayKeyRetiring removes the retiring keys that drop reports.
func dropAIGatewayKeyRetiring(key *coderv1alpha1.CoderAIGatewayKey, drop func(coderv1alpha1.CoderAIGatewayKeyRetiring) bool) {
	key.Status.RetiringKeys = slices.DeleteFunc(slices.Clone(key.Status.RetiringKeys), drop)
	if len(key.Status.RetiringKeys) == 0 {
		key.Status.RetiringKeys = nil
	}
}
