package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
)

const testAIGatewayUID = types.UID("0f8e2a6c-1b3d-4e5f-9a7b-2c4d6e8f0a1b")

// Plan 4.2 steps 6 and 7 as a table: which listed keys belong to the object,
// what the Secret holds, and the action that follows.
func TestNextAIGatewayKeyAction(t *testing.T) {
	t.Parallel()
	prefix, err := aiGatewayKeyNamePrefix(testAIGatewayUID)
	require.NoError(t, err)
	require.Equal(t, "ck8s-0f8e2a6c1b3d4e5f9a7b2c4d6e8f0a1b-", prefix)
	for _, uid := range []types.UID{"", "0F8E2A6C-1B3D-4E5F-9A7B-2C4D6E8F0A1B", "0f8e2a6c-1b3d-4e5f-9a7b"} {
		_, err := aiGatewayKeyNamePrefix(uid)
		require.ErrorContains(t, err, "assertion failed", uid)
	}

	current := codersdk.AIGatewayKey{ID: uuid.New(), Name: prefix + "1", KeyPrefix: "AbCdEfGh012"}
	other := codersdk.AIGatewayKey{ID: uuid.New(), Name: prefix + "2", KeyPrefix: "ZyXwVuTs987"}
	listed := []codersdk.AIGatewayKey{
		current, other,
		// Never this object's: no serial, a non-digit serial, another UID,
		// Carol's manual key.
		{ID: uuid.New(), Name: prefix, KeyPrefix: "Aaaaaaaaaa1"},
		{ID: uuid.New(), Name: prefix + "1a", KeyPrefix: "Aaaaaaaaaa2"},
		// Serials that the controller never writes: leading zeros, a sign,
		// and a value past int64.
		{ID: uuid.New(), Name: prefix + "00", KeyPrefix: "Aaaaaaaaaa5"},
		{ID: uuid.New(), Name: prefix + "01", KeyPrefix: "Aaaaaaaaaa6"},
		{ID: uuid.New(), Name: prefix + "+1", KeyPrefix: "Aaaaaaaaaa7"},
		{ID: uuid.New(), Name: prefix + "99999999999999999999", KeyPrefix: "Aaaaaaaaaa8"},
		{ID: uuid.New(), Name: "ck8s-1f8e2a6c1b3d4e5f9a7b2c4d6e8f0a1b-1", KeyPrefix: "Aaaaaaaaaa3"},
		{ID: uuid.New(), Name: "carol", KeyPrefix: "Aaaaaaaaaa4"},
	}
	own := ownAIGatewayKeys(prefix, listed)
	require.Equal(t, []codersdk.AIGatewayKey{current, other}, own)

	secretWith := func(value string) *corev1.Secret {
		return &corev1.Secret{Data: map[string][]byte{coderv1alpha1.CoderAIGatewayKeySecretDataKey: []byte(value)}}
	}
	tail := strings.Repeat("x", 32)
	for name, tc := range map[string]struct {
		secret *corev1.Secret
		want   uuid.UUID
	}{
		"absent":               {secret: nil, want: uuid.Nil},
		"empty after claim":    {secret: &corev1.Secret{}, want: uuid.Nil},
		"current":              {secret: secretWith("AbCdEfGh012" + tail), want: current.ID},
		"other own key":        {secret: secretWith("ZyXwVuTs987" + tail), want: other.ID},
		"another object's key": {secret: secretWith("Aaaaaaaaaa3" + tail), want: uuid.Nil},
		"too short":            {secret: secretWith("AbCdEfGh012" + tail[1:]), want: uuid.Nil},
		"trailing newline":     {secret: secretWith("AbCdEfGh012" + tail + "\n"), want: uuid.Nil},
		"not alphanumeric":     {secret: secretWith("AbCdEfGh012" + tail[1:] + "-"), want: uuid.Nil},
	} {
		require.Equal(t, tc.want, aiGatewayKeySecretHolds(tc.secret, own), name)
	}

	currentStatus := &coderv1alpha1.CoderAIGatewayKeyCurrent{Serial: 1, ID: current.ID.String(), Name: current.Name}
	otherStatus := &coderv1alpha1.CoderAIGatewayKeyCurrent{Serial: 2, ID: other.ID.String(), Name: other.Name}
	now := time.Unix(1700000000, 0)
	recent, settled := &metav1.Time{Time: now.Add(-time.Minute)}, &metav1.Time{Time: now.Add(-aiGatewayKeySettleWindow)}
	retiring := func(n int) []coderv1alpha1.CoderAIGatewayKeyRetiring {
		var keys []coderv1alpha1.CoderAIGatewayKeyRetiring
		for i := range n {
			keys = append(keys, coderv1alpha1.CoderAIGatewayKeyRetiring{Serial: int64(10 + i), ID: uuid.NewString()})
		}
		return keys
	}
	for name, tc := range map[string]struct {
		status     coderv1alpha1.CoderAIGatewayKeyStatus
		rotation   coderv1alpha1.CoderAIGatewayKeyRotationSpec // IntervalSeconds 0 unless set.
		generation int64
		own        []codersdk.AIGatewayKey
		holds      uuid.UUID
		want       aiGatewayKeyAction
		key        uuid.UUID // The adopted or swept key.
		settleAt   *metav1.Time
	}{
		"new object": {want: aiGatewayKeyIssue},
		// Keys with serials this object never sent are not its keys (I6).
		"new object, keys with unused serials": {own: own, holds: other.ID, want: aiGatewayKeyIssue},
		"create in flight":                     {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 1, PendingKey: &coderv1alpha1.CoderAIGatewayKeyPending{}}, want: aiGatewayKeyRecover},
		"in flight during rotation": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, PendingKey: &coderv1alpha1.CoderAIGatewayKeyPending{Serial: 1}},
			own:    own, holds: current.ID, want: aiGatewayKeyRecover,
		},
		"lost status write after the first key": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, PendingKey: &coderv1alpha1.CoderAIGatewayKeyPending{Serial: 2}, LastCreateAttemptTime: recent},
			own:    own, holds: other.ID, want: aiGatewayKeyAdopt, key: other.ID,
		},
		"lost status write after a newer key": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: recent},
			own:    own, holds: other.ID, want: aiGatewayKeyAdopt, key: other.ID,
		},
		// A Secret restored with an older key is not taken back (4.4), and
		// the held key is never swept.
		"secret restored with an older key": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: otherStatus, LastCreateAttemptTime: settled},
			own:    own, holds: current.ID, want: aiGatewayKeyReissue, settleAt: nil,
		},
		"orphan inside the settle window": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, LastCreateAttemptTime: recent},
			own:    own, want: aiGatewayKeySettle, key: current.ID, settleAt: &metav1.Time{Time: recent.Add(aiGatewayKeySettleWindow)},
		},
		"orphan after the settle window": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, LastCreateAttemptTime: settled},
			own:    own, want: aiGatewayKeySweep, key: current.ID, settleAt: &metav1.Time{Time: now},
		},
		"steady state, orphan inside the settle window": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: recent},
			own:    own, holds: current.ID, want: aiGatewayKeyReady, key: other.ID, settleAt: &metav1.Time{Time: recent.Add(aiGatewayKeySettleWindow)},
		},
		"steady state, orphan after the settle window": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: settled},
			own:    own, holds: current.ID, want: aiGatewayKeySweep, key: other.ID, settleAt: &metav1.Time{Time: now},
		},
		"retiring key is not an orphan": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{
				NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: settled,
				RetiringKeys: []coderv1alpha1.CoderAIGatewayKeyRetiring{{Serial: 2, ID: other.ID.String(), Name: other.Name, ReplacedAt: metav1.NewTime(now)}},
			},
			own: own, holds: current.ID, want: aiGatewayKeyReady,
		},
		"steady state": {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own, holds: current.ID, want: aiGatewayKeyReady},
		// Plan 3.4: a scheduled rotation needs no retiring key, a requested
		// one fewer than 2. Both wait behind the create gate.
		"scheduled rotation due": {
			status:   coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: issuedAgo(currentStatus, 25*time.Hour)},
			rotation: coderv1alpha1.CoderAIGatewayKeyRotationSpec{IntervalSeconds: ptrToInt32(86400)}, own: own[:1], holds: current.ID, want: aiGatewayKeyRotate,
		},
		"scheduled rotation not due": {
			status:   coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: issuedAgo(currentStatus, 23*time.Hour)},
			rotation: coderv1alpha1.CoderAIGatewayKeyRotationSpec{IntervalSeconds: ptrToInt32(86400)}, own: own[:1], holds: current.ID, want: aiGatewayKeyReady,
		},
		"scheduled rotation while a key retires": {
			status:   coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: issuedAgo(currentStatus, 25*time.Hour), RetiringKeys: recentRetiring(other)},
			rotation: coderv1alpha1.CoderAIGatewayKeyRotationSpec{IntervalSeconds: ptrToInt32(86400)}, own: own, holds: current.ID, want: aiGatewayKeyReady,
		},
		"requested rotation while a key retires": {
			status:   coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: currentStatus, RetiringKeys: recentRetiring(other)},
			rotation: coderv1alpha1.CoderAIGatewayKeyRotationSpec{Trigger: "r1"}, own: own, holds: current.ID, want: aiGatewayKeyRotate,
		},
		"handled trigger": {
			status:   coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, LastHandledTrigger: "r1"},
			rotation: coderv1alpha1.CoderAIGatewayKeyRotationSpec{Trigger: "r1"}, own: own[:1], holds: current.ID, want: aiGatewayKeyReady,
		},
		"requested rotation behind the create gate": {
			status:   coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: recent},
			rotation: coderv1alpha1.CoderAIGatewayKeyRotationSpec{Trigger: "r1"}, own: own, holds: current.ID, want: aiGatewayKeyReady,
			key: other.ID, settleAt: &metav1.Time{Time: recent.Add(aiGatewayKeySettleWindow)},
		},
		"replaced key after its grace period": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{
				NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: settled,
				RetiringKeys: []coderv1alpha1.CoderAIGatewayKeyRetiring{{Serial: 2, ID: other.ID.String(), Name: other.Name, ReplacedAt: metav1.NewTime(now.Add(-time.Hour))}},
			},
			own: own, holds: current.ID, want: aiGatewayKeyRetire,
		},
		"current key deleted": {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own[1:], holds: uuid.Nil, want: aiGatewayKeyKillSwitch},
		// D6: only a spec change after the kill switch fired re-arms it. A
		// spec change before it fired re-arms nothing.
		"kill switch, same generation": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, KillSwitchGeneration: 1},
			own:    own[1:], generation: 1, want: aiGatewayKeyKillSwitch,
		},
		"kill switch, spec changed": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, KillSwitchGeneration: 1},
			own:    own[1:], generation: 2, want: aiGatewayKeyRearm,
		},
		"kill switch not fired, spec changed": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus},
			own:    own[1:], generation: 2, want: aiGatewayKeyKillSwitch,
		},
		// A settled orphan never comes before the kill switch or a re-issue.
		// The kill switch sweeps it in delete-only mode.
		"kill switch with a settled orphan": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: settled},
			own:    own[1:], want: aiGatewayKeyKillSwitch, key: other.ID, settleAt: &metav1.Time{Time: now},
		},
		"re-issue behind the create gate": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, CurrentKey: currentStatus, LastCreateAttemptTime: settled},
			own:    own, want: aiGatewayKeySweep, key: other.ID, settleAt: &metav1.Time{Time: now},
		},
		// Carry-forward from #292: a lost lastCreateAttemptTime settles from now.
		"orphan without a create time": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3},
			own:    own, want: aiGatewayKeySettle, key: current.ID, settleAt: &metav1.Time{Time: now.Add(aiGatewayKeySettleWindow)},
		},
		// D7: one drift re-issue per grace period, and room in retiringKeys.
		"secret emptied inside the grace period": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, LastDriftReissueTime: &metav1.Time{Time: now.Add(-59 * time.Minute)}},
			own:    own[:1], want: aiGatewayKeyConflict,
		},
		"secret emptied after the grace period": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, LastDriftReissueTime: &metav1.Time{Time: now.Add(-time.Hour)}},
			own:    own[:1], want: aiGatewayKeyReissue,
		},
		"secret emptied, retiring keys full": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, RetiringKeys: retiring(3)},
			own:    own[:1], want: aiGatewayKeyConflict,
		},
		"secret emptied, room for one more": {
			status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus, RetiringKeys: retiring(2)},
			own:    own[:1], want: aiGatewayKeyReissue,
		},
		"secret emptied":       {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own, holds: uuid.Nil, want: aiGatewayKeyReissue},
		"secret holds another": {status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 2, CurrentKey: currentStatus}, own: own, holds: other.ID, want: aiGatewayKeyReissue},
	} {
		key := &coderv1alpha1.CoderAIGatewayKey{Status: tc.status}
		key.Spec.Rotation = tc.rotation
		if key.Spec.Rotation.IntervalSeconds == nil {
			key.Spec.Rotation.IntervalSeconds = ptrToInt32(0)
		}
		key.Generation = tc.generation
		got := nextAIGatewayKeyAction(key, prefix, tc.own, tc.holds, now)
		require.Equal(t, tc.want, got.action, name)
		if tc.key != uuid.Nil {
			require.Equal(t, tc.key, got.key.ID, name)
		}
		if tc.settleAt != nil {
			require.Equal(t, tc.settleAt.Time, got.settleAt, name)
		} else {
			require.True(t, got.settleAt.IsZero(), name)
		}
	}
	require.Greater(t, aiGatewayKeySettleWindow, aiGatewayKeyMaxWriteAge+time.Minute,
		"the settle window must outlast the latest write start plus the API server's 60 s request timeout (plan R18)")
}

// Plan 4.2 step 4: every LIST failure class, with Coder's details left out.
func TestAIGatewayKeyListFailure(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status      int
		message     string
		reason      string
		stalled     bool
		rateLimited bool
	}{
		"no license":      {status: http.StatusForbidden, message: "AI Gateway is a Premium feature. Contact sales!", reason: "LicenseRequired", stalled: true},
		"not site owner":  {status: http.StatusForbidden, message: "Forbidden.", reason: "CoderRejected", stalled: true},
		"route missing":   {status: http.StatusNotFound, message: "Route not found.", reason: "NotSupported", stalled: true},
		"other not found": {status: http.StatusNotFound, message: "Resource not found.", reason: "CoderUnavailable"},
		"token rejected":  {status: http.StatusUnauthorized, message: "You must be logged in.", reason: "CoderUnavailable"},
		"rate limited":    {status: http.StatusTooManyRequests, message: "Rate limit.", reason: "CoderUnavailable", rateLimited: true},
		"server error":    {status: http.StatusBadGateway, message: "Bad gateway.", reason: "CoderUnavailable"},
	} {
		step := aiGatewayKeyCoderFailure("list AI Gateway keys", codersdk.NewError(tc.status, codersdk.Response{Message: tc.message, Detail: "detail-must-not-leak"}))
		require.Equal(t, tc.reason, step.reason, name)
		require.Equal(t, tc.stalled, step.stalled, name)
		require.Equal(t, tc.rateLimited, step.rateLimited, name)
		require.NotContains(t, step.message, "detail-must-not-leak", name)
		require.Contains(t, step.message, tc.message, name)
	}
	step := aiGatewayKeyCoderFailure("list AI Gateway keys", errors.New("dial tcp: connection refused"))
	require.Equal(t, "CoderUnavailable", step.reason)
	require.False(t, step.stalled)
}

// Plan 4.3 outcome classes of a failed create, with Coder's details left out.
func TestAIGatewayKeyCreateFailure(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err         error
		reason      string
		stalled     bool
		rateLimited bool
		failures    int32
	}{
		"rate limited":   {err: codersdk.NewError(http.StatusTooManyRequests, codersdk.Response{Message: "Rate limit."}), reason: "CoderUnavailable", rateLimited: true},
		"token rejected": {err: codersdk.NewError(http.StatusUnauthorized, codersdk.Response{Message: "Unauthorized."}), reason: "CoderUnavailable"},
		"dial":           {err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}, reason: "CoderUnavailable"},
		"no license":     {err: codersdk.NewError(http.StatusForbidden, codersdk.Response{Message: "AI Gateway is a Premium feature. Contact sales!"}), reason: "LicenseRequired", stalled: true},
		"forbidden":      {err: codersdk.NewError(http.StatusForbidden, codersdk.Response{Message: "Forbidden."}), reason: "CoderRejected", stalled: true},
		"route missing":  {err: codersdk.NewError(http.StatusNotFound, codersdk.Response{Message: "Route not found."}), reason: "NotSupported", stalled: true},
		"duplicate name": {err: codersdk.NewError(http.StatusBadRequest, codersdk.Response{Message: "Key name must be unique."}), reason: "IssuingKey"},
		"invalid name":   {err: codersdk.NewError(http.StatusBadRequest, codersdk.Response{Message: "Invalid key name."}), reason: "CoderRejected", stalled: true},
		"406":            {err: codersdk.NewError(http.StatusNotAcceptable, codersdk.Response{Message: "Not acceptable."}), reason: "IssuingKey", failures: 1},
		"server error":   {err: codersdk.NewError(http.StatusBadGateway, codersdk.Response{Message: "Bad gateway."}), reason: "IssuingKey", failures: 1},
		"non-201 2xx":    {err: codersdk.NewError(http.StatusOK, codersdk.Response{Message: "unexpected status code 200"}), reason: "IssuingKey", failures: 1},
		"timeout":        {err: context.DeadlineExceeded, reason: "IssuingKey", failures: 1},
	} {
		key := &coderv1alpha1.CoderAIGatewayKey{}
		if sdkErr := (*codersdk.Error)(nil); errors.As(tc.err, &sdkErr) {
			sdkErr.Detail = "detail-must-not-leak"
		}
		step := aiGatewayKeyCreateFailure(key, "ck8s-x-0", tc.err)
		require.Equal(t, tc.reason, step.reason, name)
		require.Equal(t, tc.stalled, step.stalled, name)
		require.Equal(t, tc.rateLimited, step.rateLimited, name)
		require.Equal(t, tc.failures, key.Status.ConsecutiveIssueFailures, name)
		require.NotContains(t, step.message, "detail-must-not-leak", name)
	}

	// Uncertain answers back off from 30 s, doubling to 10 minutes, and the
	// tenth stalls.
	key := &coderv1alpha1.CoderAIGatewayKey{}
	var backoffs []time.Duration
	for range 9 {
		backoffs = append(backoffs, aiGatewayKeyIssueFailed(key, "x").requeue)
	}
	require.Equal(t, []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute,
	}, backoffs)
	require.True(t, aiGatewayKeyIssueFailed(key, "x").stalled)
}

// Plan 4.6 conditions 1 and 2 as a table, and (plan R9 at the decision) a
// key that the Secret holds is never an orphan, also in delete-only mode.
func TestAIGatewayKeyRetireState(t *testing.T) {
	t.Parallel()
	prefix, err := aiGatewayKeyNamePrefix(testAIGatewayUID)
	require.NoError(t, err)
	now := time.Unix(1700000000, 0)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	current := codersdk.AIGatewayKey{ID: uuid.New(), Name: prefix + "1"}
	retiring := codersdk.AIGatewayKey{ID: uuid.New(), Name: prefix + "0"}
	for name, tc := range map[string]struct {
		replaced     time.Duration // Before now.
		wait         int32
		beat, beatC  *time.Time // Heartbeats of the retiring and the current key.
		wantReason   string
		wantNext     time.Time
		wantDue      bool
		notListedYet bool
	}{
		"inside the grace period":    {replaced: 59 * time.Minute, wantReason: reasonWaitingForGracePeriod, wantNext: now.Add(time.Minute)},
		"grace over, no heartbeat":   {replaced: time.Hour, wantDue: true},
		"live connection":            {replaced: 2 * time.Hour, beat: ago(time.Minute), wantReason: reasonWaitingForConsumers, wantNext: now.Add(9*time.Minute + time.Second)},
		"live until the wait ends":   {replaced: 2 * time.Hour, wait: 3660, beat: ago(time.Minute), wantReason: reasonWaitingForConsumers, wantNext: now.Add(time.Minute)},
		"heartbeat stale":            {replaced: 2 * time.Hour, beat: ago(11 * time.Minute), wantDue: true},
		"current key heartbeats":     {replaced: 2 * time.Hour, beat: ago(3 * time.Minute), beatC: ago(time.Minute), wantDue: true},
		"current heartbeat too near": {replaced: 2 * time.Hour, beat: ago(2 * time.Minute), beatC: ago(time.Minute), wantReason: reasonWaitingForConsumers, wantNext: now.Add(8*time.Minute + time.Second)},
		"consumer wait over":         {replaced: 2 * time.Hour, wait: 3600, beat: ago(time.Minute), wantDue: true},
		"no consumer wait":           {replaced: time.Hour, wait: 0, beat: ago(time.Minute), wantDue: true},
		"gone in Coder":              {replaced: 2 * time.Hour, notListedYet: true},
	} {
		key := &coderv1alpha1.CoderAIGatewayKey{}
		key.Spec.Rotation.ConsumerWaitSeconds = ptrToInt32(86400)
		if tc.wait != 0 || name == "no consumer wait" {
			key.Spec.Rotation.ConsumerWaitSeconds = ptrToInt32(tc.wait)
		}
		key.Status.CurrentKey = &coderv1alpha1.CoderAIGatewayKeyCurrent{Serial: 1, ID: current.ID.String(), Name: current.Name}
		key.Status.RetiringKeys = []coderv1alpha1.CoderAIGatewayKeyRetiring{{Serial: 0, ID: retiring.ID.String(), Name: retiring.Name, ReplacedAt: metav1.NewTime(now.Add(-tc.replaced))}}
		r, c := retiring, current
		r.LastHeartbeatAt, c.LastHeartbeatAt = tc.beat, tc.beatC
		own := []codersdk.AIGatewayKey{c, r}
		if tc.notListedYet {
			own = own[:1]
		}
		got := aiGatewayKeyRetireState(key, own, uuid.Nil, now)
		require.Equal(t, tc.wantDue, got.due != nil, name)
		require.Equal(t, tc.wantReason, got.reason, name)
		require.Equal(t, tc.wantNext, got.next, name)
		if tc.wantDue {
			require.Equal(t, retiring.ID, got.due.ID, name)
			require.Nil(t, aiGatewayKeyRetireState(key, own, retiring.ID, now).due, name+": a held key is never due")
		}
	}

	orphan := codersdk.AIGatewayKey{ID: uuid.New(), Name: prefix + "2"}
	key := &coderv1alpha1.CoderAIGatewayKey{Status: coderv1alpha1.CoderAIGatewayKeyStatus{NextSerial: 3, LastCreateAttemptTime: &metav1.Time{Time: now.Add(-time.Hour)}}}
	require.Equal(t, orphan.ID, aiGatewayKeyOrphan(key, prefix, []codersdk.AIGatewayKey{orphan}, uuid.Nil, now).key.ID)
	require.True(t, aiGatewayKeyOrphan(key, prefix, []codersdk.AIGatewayKey{orphan}, orphan.ID, now).settleAt.IsZero(),
		"a key that a replaced Secret holds is not an orphan")
}

func ptrToInt32(v int32) *int32 { return &v }

func issuedAgo(current *coderv1alpha1.CoderAIGatewayKeyCurrent, d time.Duration) *coderv1alpha1.CoderAIGatewayKeyCurrent {
	c := *current
	c.IssuedAt = metav1.NewTime(time.Unix(1700000000, 0).Add(-d))
	return &c
}

func recentRetiring(keys ...codersdk.AIGatewayKey) []coderv1alpha1.CoderAIGatewayKeyRetiring {
	var out []coderv1alpha1.CoderAIGatewayKeyRetiring
	for i, k := range keys {
		out = append(out, coderv1alpha1.CoderAIGatewayKeyRetiring{Serial: int64(10 + i), ID: k.ID.String(), Name: k.Name, ReplacedAt: metav1.NewTime(time.Unix(1700000000, 0))})
	}
	return out
}

// Plan 3.4 item 3: a third requested rotation while two keys retire is
// blocked, and the step names a key that blocks it and keeps Ready=True.
func TestAIGatewayKeyRotationBlocked(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0)
	key := &coderv1alpha1.CoderAIGatewayKey{}
	key.Spec.Rotation = coderv1alpha1.CoderAIGatewayKeyRotationSpec{IntervalSeconds: ptrToInt32(0), Trigger: "r3"}
	key.Status.CurrentKey = &coderv1alpha1.CoderAIGatewayKeyCurrent{Serial: 2, Name: "ck8s-x-2"}
	key.Status.LastHandledTrigger = "r2"
	key.Status.RetiringKeys = []coderv1alpha1.CoderAIGatewayKeyRetiring{{Serial: 0, Name: "ck8s-x-0"}, {Serial: 1, Name: "ck8s-x-1"}}
	due, blocked := aiGatewayKeyRotationDue(key, now)
	require.False(t, due)
	require.NotNil(t, blocked)
	require.True(t, blocked.ready && blocked.inProgress && !blocked.stalled)
	require.Equal(t, reasonRotationBlocked, blocked.reason)
	require.Contains(t, blocked.message, "ck8s-x-0")
	require.LessOrEqual(t, aiGatewayKeyResyncAfter(key, now), aiGatewayKeyResync)
	require.Greater(t, aiGatewayKeyResyncAfter(key, now), aiGatewayKeyResync*9/10)

	// An overdue scheduled rotation while a key retires is blocked too, and
	// its past due time does not make the resync a one-second poll.
	key.Spec.Rotation = coderv1alpha1.CoderAIGatewayKeyRotationSpec{IntervalSeconds: ptrToInt32(86400)}
	key.Status.CurrentKey.IssuedAt = metav1.NewTime(now.Add(-25 * time.Hour))
	key.Status.RetiringKeys = key.Status.RetiringKeys[:1]
	due, blocked = aiGatewayKeyRotationDue(key, now)
	require.False(t, due)
	require.NotNil(t, blocked)
	require.Greater(t, aiGatewayKeyResyncAfter(key, now), aiGatewayKeyResync*9/10)
	key.Status.CurrentKey.IssuedAt = metav1.NewTime(now.Add(-24*time.Hour + time.Minute))
	require.Equal(t, time.Minute, aiGatewayKeyResyncAfter(key, now), "a future rotation still bounds the resync")
}
