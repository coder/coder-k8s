package controller

import (
	"math/rand/v2"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const (
	reasonRotationBlocked = "RotationBlocked"
	// aiGatewayKeyDefaultIntervalSeconds is the API default of
	// spec.rotation.intervalSeconds.
	aiGatewayKeyDefaultIntervalSeconds = 2592000
)

// aiGatewayKeyNextRotation returns when the scheduled rotation of the
// current key is due (plan 3.4): issuedAt plus the interval of the current
// spec, so an interval change applies at once. It returns nil when the
// interval is 0 or there is no current key.
func aiGatewayKeyNextRotation(key *coderv1alpha1.CoderAIGatewayKey) *time.Time {
	interval := ptr.Deref(key.Spec.Rotation.IntervalSeconds, aiGatewayKeyDefaultIntervalSeconds)
	if key.Status.CurrentKey == nil || interval == 0 {
		return nil
	}
	at := key.Status.CurrentKey.IssuedAt.Add(time.Duration(interval) * time.Second)
	return &at
}

// setAIGatewayKeyNextRotation shows the scheduled rotation in status.
func setAIGatewayKeyNextRotation(key *coderv1alpha1.CoderAIGatewayKey) {
	key.Status.NextRotationTime = nil
	if next := aiGatewayKeyNextRotation(key); next != nil {
		key.Status.NextRotationTime = &metav1.Time{Time: *next}
	}
}

// aiGatewayKeyRotationDue reports whether a rotation is due (plan 3.4). A
// requested rotation is a non-empty trigger that differs from the last
// handled one. A scheduled rotation starts only while no replaced key
// retires, and a requested one while fewer than 2 do. A due rotation that
// cannot start returns blocked, which names the retiring key that blocks it.
func aiGatewayKeyRotationDue(key *coderv1alpha1.CoderAIGatewayKey, now time.Time) (due bool, blocked *aiGatewayKeyStep) {
	requested := key.Spec.Rotation.Trigger != "" && key.Spec.Rotation.Trigger != key.Status.LastHandledTrigger
	next := aiGatewayKeyNextRotation(key)
	scheduled := next != nil && !now.Before(*next)
	limit := 1
	if requested {
		limit = 2
	}
	switch retiring := key.Status.RetiringKeys; {
	case !requested && !scheduled:
		return false, nil
	case len(retiring) < limit:
		return true, nil
	default:
		step := aiGatewayKeyReadyStep(key)
		step.inProgress, step.reason = true, reasonRotationBlocked
		step.message = "A rotation is due, but replaced key " + retiring[0].Name +
			" still waits for deletion. The rotation starts when the retiring keys leave room for one more."
		return false, step
	}
}

// aiGatewayKeyResyncAfter is the requeue of a Ready object: the resync with
// up to 10 % jitter, so objects do not list keys in step, and never later
// than the next rotation (plan 3.4 item 5). A rotation that is already due
// here is blocked, and only the retire step can unblock it, so a past due
// time does not shorten the resync.
func aiGatewayKeyResyncAfter(key *coderv1alpha1.CoderAIGatewayKey, now time.Time) time.Duration {
	requeue := aiGatewayKeyResync - rand.N(aiGatewayKeyResync/10) // #nosec G404 -- spreads requeues; not a secret.
	if next := aiGatewayKeyNextRotation(key); next != nil && next.After(now) {
		requeue = min(requeue, next.Sub(now))
	}
	return requeue
}
