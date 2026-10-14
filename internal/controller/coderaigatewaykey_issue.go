package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/credentialsecret"
)

// Bounds of one key issue (plan 4.3).
const (
	// aiGatewayKeyAttemptTimeout bounds the create, the answer check, the
	// Secret writes and the status write of one attempt.
	aiGatewayKeyAttemptTimeout = 90 * time.Second
	// aiGatewayKeyMaxWriteAge is the latest time after the marker's
	// timestamp (lastCreateAttemptTime) at which a Secret write may start.
	// With the API server's default 60 s request timeout (an assumption the
	// docs state, plan RK13), every write of an attempt has landed or failed
	// 3 minutes after that timestamp, inside the 5-minute settle window.
	aiGatewayKeyMaxWriteAge        = 2 * time.Minute
	aiGatewayKeySecretWriteTimeout = 10 * time.Second
	aiGatewayKeySecretWriteTries   = 3
	// aiGatewayKeyIssueFailureCap stops issuing after this many uncertain
	// create answers or failed Secret writes in a row, until the spec
	// changes. It bounds key churn when Coder or the Secret write keeps
	// failing.
	aiGatewayKeyIssueFailureCap = 10
	aiGatewayKeyUncertainBase   = 30 * time.Second
	aiGatewayKeyUncertainCap    = 10 * time.Minute

	reasonIssueFailing        = "IssueFailing"
	reasonCoderAnswerMismatch = "CoderAnswerMismatch"
	// Coder's 400 answers to a create (plan F5, F6).
	aiGatewayDuplicateName = "Key name must be unique."
	aiGatewayInvalidName   = "Invalid key name."
)

// issueKey runs the issue step of plan 4.3: claim and dry-run the Secret,
// write the marker, create the key, check the answer, write the key into the
// Secret, and record it in status. The key exists only in memory between the
// create answer and the Secret write, so these steps run in one reconcile.
// A replaced current key retires from the Secret write. drift reports a
// re-issue because the Secret lost the current key (decision D7).
func (r *CoderAIGatewayKeyReconciler) issueKey(
	ctx context.Context, sdk *codersdk.Client, key *coderv1alpha1.CoderAIGatewayKey, before *coderv1alpha1.CoderAIGatewayKeyStatus, prefix string, drift bool,
) (*aiGatewayKeyStep, error) {
	if key.Status.CurrentKey != nil && len(key.Status.RetiringKeys) >= aiGatewayKeyMaxRetiring {
		return nil, fmt.Errorf("assertion failed: AI Gateway key %s/%s has no room to retire its current key", key.Namespace, key.Name)
	}
	if step := aiGatewayKeyIssueBlocked(key); step != nil {
		return step, nil
	}
	labels, step, err := r.prepareSecret(ctx, key, prefix)
	if step != nil || err != nil {
		return step, err
	}

	// The marker and the serial go into one optimistic status update before
	// the create (plan I3). A conflict stops here, before any request.
	serial := key.Status.NextSerial
	name := prefix + strconv.FormatInt(serial, 10)
	// One instant stamps the marker (lastCreateAttemptTime, which starts the
	// settle window) and starts the write-age limit, so a slow marker write
	// shortens the time left for Secret writes, never the window. In
	// production the instant carries Go's monotonic reading.
	attemptStart := r.Clock.Now()
	now := metav1.NewTime(attemptStart)
	key.Status.PendingKey = &coderv1alpha1.CoderAIGatewayKeyPending{Serial: serial, Name: name, CreateAttemptTime: now}
	key.Status.NextSerial = serial + 1
	key.Status.LastCreateAttemptTime = &now
	r.applyStep(key, aiGatewayKeyWait(0, reasonIssuingKey, "Creating key %s.", name))
	if err := r.writeStatus(ctx, key, before); err != nil {
		return nil, err
	}
	*before = *key.Status.DeepCopy()

	attemptCtx, cancel := context.WithTimeout(ctx, aiGatewayKeyAttemptTimeout)
	defer cancel()
	created, err := sdk.CreateAIGatewayKey(attemptCtx, codersdk.CreateAIGatewayKeyRequest{Name: name})
	plaintext := credentialsecret.NewValue([]byte(created.Key))
	created.Key = ""
	if err != nil {
		key.Status.PendingKey = nil
		return aiGatewayKeyCreateFailure(key, name, err), nil
	}
	if mismatch := aiGatewayKeyAnswerMismatch(name, created, plaintext); mismatch != "" {
		// The controller deletes nothing here. A key with this object's name
		// pattern is an orphan for the sweep.
		key.Status.PendingKey = nil
		return aiGatewayKeyStalled(reasonCoderAnswerMismatch,
			"Coder answered the create of key %s with key ID %s, but %s. The operator creates no key until the spec changes.", name, created.ID, mismatch), nil
	}
	step, err = r.writeKeySecret(attemptCtx, key, labels, created, plaintext, attemptStart)
	if err != nil {
		return nil, err // An assertion failed. The marker stays.
	}
	if step != nil {
		// The plaintext is dropped when this reconcile returns. The Secret
		// still holds what it held, and the new key is an orphan.
		key.Status.PendingKey = nil
		return aiGatewayKeyIssueFailed(key, step.message), nil
	}

	key.Status.PendingKey = nil
	written := r.Clock.Now()
	if key.Status.CurrentKey != nil {
		retireAIGatewayKey(key, written)
	}
	if drift {
		key.Status.LastDriftReissueTime = &metav1.Time{Time: written}
	}
	step = setAIGatewayKeyCurrent(key, serial, created.ID, name, written, true)
	r.applyStep(key, step)
	if err := r.writeStatus(attemptCtx, key, before); err != nil {
		return nil, err
	}
	*before = *key.Status.DeepCopy()
	return step, nil
}

// setAIGatewayKeyCurrent records the key that the Secret now holds as the
// current key, as a successful issue or recovery 4.4 does, and returns the
// Ready step. applied reports that the key was issued for the object's
// current spec, so its trigger counts as handled and its generation as
// applied.
func setAIGatewayKeyCurrent(key *coderv1alpha1.CoderAIGatewayKey, serial int64, id uuid.UUID, name string, at time.Time, applied bool) *aiGatewayKeyStep {
	now := metav1.NewTime(at)
	key.Status.CurrentKey = &coderv1alpha1.CoderAIGatewayKeyCurrent{Serial: serial, ID: id.String(), Name: name, IssuedAt: now}
	key.Status.ConsecutiveIssueFailures = 0
	key.Status.KillSwitchGeneration = 0 // The kill switch was for the replaced key.
	if applied {
		key.Status.LastHandledTrigger = key.Spec.Rotation.Trigger
		key.Status.AppliedGeneration = key.Generation
	}
	key.Status.NextRotationTime = nil
	if interval := ptr.Deref(key.Spec.Rotation.IntervalSeconds, 0); interval > 0 {
		key.Status.NextRotationTime = &metav1.Time{Time: now.Add(time.Duration(interval) * time.Second)}
	}
	return aiGatewayKeyReadyStep(key)
}

func aiGatewayKeyReadyStep(key *coderv1alpha1.CoderAIGatewayKey) *aiGatewayKeyStep {
	return &aiGatewayKeyStep{ready: true, reason: "Ready", requeue: aiGatewayKeyResync, message: fmt.Sprintf(
		"Secret %q holds key %s, and Coder lists it.", key.Spec.SecretName, key.Status.CurrentKey.Name)}
}

// aiGatewayKeyIssueBlocked returns the stall that keeps the issue step from
// running until the spec changes: the failure cap, or a create answer that
// did not match the request in this generation.
func aiGatewayKeyIssueBlocked(key *coderv1alpha1.CoderAIGatewayKey) *aiGatewayKeyStep {
	if n := key.Status.ConsecutiveIssueFailures; n >= aiGatewayKeyIssueFailureCap {
		return aiGatewayKeyStalled(reasonIssueFailing,
			"%d key issues in a row had an uncertain Coder answer or a failed Secret write. The operator creates no key until the spec changes.", n)
	}
	stalled := meta.FindStatusCondition(key.Status.Conditions, coderv1alpha1.CoderAIGatewayKeyConditionStalled)
	if stalled != nil && stalled.Status == metav1.ConditionTrue && stalled.Reason == reasonCoderAnswerMismatch && stalled.ObservedGeneration == key.Generation {
		return aiGatewayKeyStalled(stalled.Reason, "%s", stalled.Message)
	}
	return nil
}

// aiGatewayKeyAnswerMismatch checks a 201 answer against the request (the
// coderAnswerFor rule) and returns what is wrong, or "". The result never
// holds the key.
func aiGatewayKeyAnswerMismatch(name string, created codersdk.CreateAIGatewayKeyResponse, plaintext credentialsecret.Value) string {
	value := plaintext.Reveal()
	switch {
	case created.Name != name:
		return fmt.Sprintf("the answer names key %q", created.Name)
	case created.ID == uuid.Nil:
		return "the answer has no key ID"
	case !aiGatewayKeyValue.MatchString(value):
		return "the key in the answer is not 43 letters and digits"
	case created.KeyPrefix != value[:aiGatewayKeyPrefixLength]:
		return "the key prefix in the answer does not match the key"
	}
	return ""
}

// aiGatewayKeyCreateFailure classifies a failed create (plan 4.3 outcome
// classes). The marker is already cleared, and the serial stays used. The
// message holds the status and Coder's short message only: a non-201 2xx
// answer carries the key in its body, which only the error's detail holds.
func aiGatewayKeyCreateFailure(key *coderv1alpha1.CoderAIGatewayKey, name string, err error) *aiGatewayKeyStep {
	status, summary := coderStatus(err), coderErrorSummary(err)
	var message string
	if sdkErr := (*codersdk.Error)(nil); errors.As(err, &sdkErr) {
		message = sdkErr.Message
	}
	switch {
	case status == http.StatusForbidden && strings.HasSuffix(message, aiGatewayPremiumSuffix):
		return aiGatewayKeyStalled("LicenseRequired",
			"%s The deployment needs a Premium license, and the AI Gateway must be enabled (CODER_AI_GATEWAY_ENABLED).", summary)
	case status == http.StatusForbidden:
		return aiGatewayKeyStalled("CoderRejected", "%s The operator user must be a Coder site owner.", summary)
	case status == http.StatusNotFound && message == aiGatewayRouteNotFound:
		return aiGatewayKeyStalled("NotSupported", "%s This Coder deployment has no AI Gateway key API.", summary)
	case status == http.StatusBadRequest && message == aiGatewayDuplicateName:
		// Only an earlier create of this object can hold the name. The sweep
		// deletes that key, and the next create uses the next serial.
		return aiGatewayKeyWait(time.Second, reasonIssuingKey, "Coder already has a key named %s. The operator creates the next key with a new name.", name)
	case status == http.StatusBadRequest && message == aiGatewayInvalidName:
		return aiGatewayKeyStalled("CoderRejected", "assertion failed: Coder refused the key name %s as invalid: %s", name, summary)
	case aiGatewayKeyCreateHadNoEffect(err):
		unavailable := coderStepUnavailable("create AI Gateway key "+name, err)
		step := aiGatewayKeyWait(aiGatewayKeyRetry, unavailable.reason, "%s", unavailable.message)
		step.rateLimited = unavailable.rateLimited
		return step
	default:
		return aiGatewayKeyIssueFailed(key, fmt.Sprintf("The answer to the create of key %s is uncertain (%s). A committed key is an orphan.", name, summary))
	}
}

// aiGatewayKeyCreateHadNoEffect reports answers that prove Coder created
// nothing: the rate limiter and authentication answer before the handler,
// and a failed dial or TLS handshake sent no request. Coder's 406 means
// something else for workspaces, so it is not counted here.
func aiGatewayKeyCreateHadNoEffect(err error) bool {
	return coderStatus(err) != http.StatusNotAcceptable && createHadNoEffect(err)
}

// aiGatewayKeyIssueFailed counts an uncertain create answer or a failed
// Secret write, and backs off from 30 s, doubling to 10 minutes. The cap
// stalls the object.
func aiGatewayKeyIssueFailed(key *coderv1alpha1.CoderAIGatewayKey, message string) *aiGatewayKeyStep {
	key.Status.ConsecutiveIssueFailures++
	n := key.Status.ConsecutiveIssueFailures
	if n >= aiGatewayKeyIssueFailureCap {
		return aiGatewayKeyStalled(reasonIssueFailing, "%s %d key issues in a row failed. The operator creates no key until the spec changes.", message, n)
	}
	backoff := aiGatewayKeyUncertainCap
	if n < 6 { // 30 s * 2^5 passes the cap.
		backoff = min(aiGatewayKeyUncertainBase<<(n-1), aiGatewayKeyUncertainCap)
	}
	return aiGatewayKeyWait(backoff, reasonIssuingKey, "%s Failure %d of %d.", message, n, aiGatewayKeyIssueFailureCap)
}

// writeKeySecret writes the new key into the output Secret (plan 4.3 step 6):
// up to 3 tries within 10 s, each after a fresh live read. A read that finds
// the key already written resolves an uncertain earlier try. No try starts
// later than aiGatewayKeyMaxWriteAge after the marker's timestamp. It returns a
// step when the write failed, and an error only when an assertion failed.
// Neither holds the API server's message.
func (r *CoderAIGatewayKeyReconciler) writeKeySecret(
	ctx context.Context, key *coderv1alpha1.CoderAIGatewayKey, labels map[string]string,
	created codersdk.CreateAIGatewayKeyResponse, plaintext credentialsecret.Value, attemptStart time.Time,
) (*aiGatewayKeyStep, error) {
	ctx, cancel := context.WithTimeout(ctx, aiGatewayKeySecretWriteTimeout)
	defer cancel()
	name := key.Spec.SecretName
	want := []byte(plaintext.Reveal())
	data := map[string][]byte{coderv1alpha1.CoderAIGatewayKeySecretDataKey: want}
	annotations := map[string]string{aiGatewayKeyIDAnnotation: created.ID.String(), aiGatewayKeyNameAnnotation: created.Name}
	last := "no try ran"
	for try := 0; ; try++ {
		secret, err := getOwnedSecret(ctx, r.APIReader, key, aiGatewayKeyKind, name)
		if credErr := (*credentialsecret.Error)(nil); errors.As(err, &credErr) {
			return aiGatewayKeyStalled(credErr.Reason, "%s", credErr.Message), nil
		}
		switch {
		case err != nil:
			last = aiGatewayKeyAPIFailure(err)
		case secret == nil:
			return aiGatewayKeyWait(0, reasonIssuingKey, "Secret %q was deleted while the key was being written.", name), nil
		case bytes.Equal(secret.Data[coderv1alpha1.CoderAIGatewayKeySecretDataKey], want):
			return nil, nil // An earlier try landed.
		}
		if try == aiGatewayKeySecretWriteTries {
			return aiGatewayKeyWait(0, reasonIssuingKey, "Writing key %s into Secret %q failed %d times (last: %s).", created.Name, name, try, last), nil
		}
		if r.Clock.Since(attemptStart) >= aiGatewayKeyMaxWriteAge {
			return aiGatewayKeyWait(0, reasonIssuingKey,
				"The create of key %s took too long, so the operator did not write it into Secret %q.", created.Name, name), nil
		}
		if err != nil {
			continue
		}
		if _, err = writeOwnedSecret(ctx, r.Client, key, aiGatewayKeyKind, secret, data, labels, annotations); err == nil {
			return nil, nil
		}
		if strings.HasPrefix(err.Error(), "assertion failed:") {
			return nil, err
		}
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err) {
			return aiGatewayKeyWait(0, reasonSecretWriteRejected, "The API server refused to write key %s into Secret %q (%s).",
				created.Name, name, aiGatewayKeyAPIFailure(err)), nil
		}
		last = aiGatewayKeyAPIFailure(err)
	}
}

// aiGatewayKeyAPIFailure describes a failed Kubernetes request without the
// API server's message: the status reason and code, or the error text of a
// request that got no answer.
func aiGatewayKeyAPIFailure(err error) string {
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) {
		return fmt.Sprintf("%s, HTTP %d", apiStatus.Status().Reason, apiStatus.Status().Code)
	}
	return err.Error()
}
